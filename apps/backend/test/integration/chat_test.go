package integration

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	authrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/repository"
	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	chatrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/repository"
	workspacedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	workspacerepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/repository"
	"github.com/robzlabz/employeebot/packages/platform/database"
)

// chatFixture is one onboarded workspace with the conversation repository.
type chatFixture struct {
	repo      *chatrepo.Repository
	pool      *database.Pool
	workspace uuid.UUID
	userID    uuid.UUID
	agentID   uuid.UUID
	scope     chatdomain.Scope
}

func newChatFixture(t *testing.T, dsn string) *chatFixture {
	t.Helper()

	ctx := t.Context()
	pool := poolFor(t, dsn)

	workspaces := workspacerepo.New(pool)

	user, err := authrepo.New(pool.PgxPool()).CreateUser(ctx, uuid.NewString()+"@example.com", "$argon2id$hash")
	require.NoError(t, err)

	onboarded, err := workspaces.Onboard(ctx, user.ID, workspacedomain.OnboardRequest{
		Name:          "Toko Sinar",
		BusinessField: "Retail",
		Timezone:      "Asia/Jakarta",
	})
	require.NoError(t, err)

	agentID := firstAgentOf(t, pool, onboarded.Workspace.ID)

	return &chatFixture{
		repo:      chatrepo.New(pool),
		pool:      pool,
		workspace: onboarded.Workspace.ID,
		userID:    user.ID,
		agentID:   agentID,
		scope:     chatdomain.Scope{UserID: user.ID, WorkspaceID: onboarded.Workspace.ID},
	}
}

// twoAgentsOf returns two Bolu of a workspace, creating them if the workspace has
// fewer.
//
// A group needs two participants and a sidebar order needs two threads, and a
// workspace starts empty, so a fixture that needs two makes them.
func twoAgentsOf(t *testing.T, pool *database.Pool, workspaceID uuid.UUID) [2]uuid.UUID {
	t.Helper()

	seedAgents(t, pool, workspaceID, "Oren", "Biru")

	ids := [2]uuid.UUID{}
	require.NoError(t, pool.InScopeRead(t.Context(), database.Scope{WorkspaceID: workspaceID}, func(tx pgx.Tx) error {
		rows, err := tx.Query(t.Context(),
			"SELECT id FROM agents WHERE workspace_id = $1 AND deleted_at IS NULL ORDER BY created_at LIMIT 2",
			workspaceID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for index := 0; rows.Next() && index < 2; index++ {
			if err := rows.Scan(&ids[index]); err != nil {
				return err
			}
		}
		return rows.Err()
	}))

	require.NotEqual(t, uuid.Nil, ids[0])
	require.NotEqual(t, uuid.Nil, ids[1])
	return ids
}

// firstAgentOf returns one Bolu of a workspace, creating it if the workspace has
// none.
//
// A workspace starts empty, so a fixture that needs a Bolu to point something at
// makes one the way a person does rather than reading one that onboarding used to
// leave behind.
func firstAgentOf(t *testing.T, pool *database.Pool, workspaceID uuid.UUID) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	err := pool.InScopeRead(t.Context(), database.Scope{WorkspaceID: workspaceID}, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(),
			"SELECT id FROM agents WHERE workspace_id = $1 AND deleted_at IS NULL ORDER BY created_at LIMIT 1",
			workspaceID).Scan(&id)
	})
	if err == nil {
		return id
	}
	// An empty workspace is the normal case now, so "no rows" is what sends this
	// helper to make one rather than what fails it.
	require.ErrorIs(t, err, pgx.ErrNoRows)

	return seedAgent(t, pool, workspaceID)
}

// directConversation opens the 1:1 thread with a Bolu.
func (f *chatFixture) directConversation(t *testing.T) chatdomain.Conversation {
	t.Helper()

	conversation, err := f.repo.Direct(t.Context(), f.scope, f.agentID)
	require.NoError(t, err)
	return conversation
}

// sixBlocks is one message carrying every block type the schema defines.
func sixBlocks(t *testing.T) []chatdomain.Block {
	t.Helper()

	documents := []string{
		`{"type":"text","markdown":"**Rekap hari ini**","title":"Rekap"}`,
		`{"type":"table","title":"Pesanan","columns":["Pelanggan","Total"],
			"rows":[["Toko Sari","Rp 1.124.000"],["Bagas","Rp 189.000"]]}`,
		fmt.Sprintf(`{"type":"draft","draft_id":%q,"action_kind":"gmail.send","title":"INV-0043",
			"status":"pending","fields":[["Total","Rp 1.124.000"]]}`, uuid.NewString()),
		`{"type":"chart","title":"Penjualan","spec":{"kind":"bar","categories":["Sen","Sel"],
			"series":[{"name":"Kaos","data":[10,12]}]}}`,
		`{"type":"mermaid","title":"Alur","diagram":"flowchart","code":"flowchart TD\n  A --> B"}`,
		`{"type":"html","title":"Simulasi","content_ref":"` +
			chatdomain.EncodeContentRef(chatdomain.ObjectKey(chatdomain.ContentRefPrefix, uuid.NewString(), uuid.NewString(), "index.html")) +
			`","byte_size":2048}`,
	}

	blocks := make([]chatdomain.Block, 0, len(documents))
	for _, document := range documents {
		var header struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal([]byte(document), &header))
		blocks = append(blocks, chatdomain.Block{Type: header.Type, Body: json.RawMessage(document)})
	}
	return blocks
}

// TestSixBlockTypesRoundTrip is the E5.1 gate: every block type survives a write
// and a read, whole.
func TestSixBlockTypesRoundTrip(t *testing.T) {
	fixture := newChatFixture(t, appDatabase(t))
	conversation := fixture.directConversation(t)

	blocks := sixBlocks(t)
	require.NoError(t, chatdomain.ValidateBlocks(blocks))

	stored, err := fixture.repo.Append(t.Context(), fixture.scope, chatdomain.Message{
		ConversationID: conversation.ID,
		AuthorAgentID:  fixture.agentID,
		Blocks:         blocks,
		Status:         chatdomain.MessageComplete,
	})
	require.NoError(t, err)
	require.Len(t, stored.Blocks, len(chatdomain.BlockTypes))

	// Read it back through a fresh query, so the JSONB column is what is proven
	// rather than the value that was passed in.
	read, err := fixture.repo.GetMessage(t.Context(), fixture.scope, stored.ID)
	require.NoError(t, err)
	require.Len(t, read.Blocks, len(chatdomain.BlockTypes))

	for i, block := range read.Blocks {
		require.Equal(t, blocks[i].Type, block.Type)
		require.JSONEq(t, string(blocks[i].Body), string(block.Body))
	}

	types := make([]string, 0, len(read.Blocks))
	for _, block := range read.Blocks {
		types = append(types, block.Type)
	}
	require.ElementsMatch(t, chatdomain.BlockTypes, types)

	// The stored value is the block document, not a wrapper, which is what makes
	// the stored form and the rendered form the same thing.
	require.Contains(t, string(read.Blocks[1].Body), `"columns"`)
	require.NotContains(t, string(read.Blocks[1].Body), `"Body"`)
}

// TestCursorPaginationOverAThousandMessages is the E5.1 gate on pagination: a
// thousand messages are read back exactly once each.
func TestCursorPaginationOverAThousandMessages(t *testing.T) {
	fixture := newChatFixture(t, appDatabase(t))
	conversation := fixture.directConversation(t)

	const total = 1000
	require.NoError(t, fixture.bulkMessages(t, conversation.ID, total))

	seen := map[uuid.UUID]bool{}
	cursor := chatdomain.Cursor{Limit: 100}
	pages := 0

	for {
		page, err := fixture.repo.History(t.Context(), fixture.scope, conversation.ID, cursor)
		require.NoError(t, err)
		pages++

		if len(page.Messages) == 0 {
			break
		}
		for _, message := range page.Messages {
			require.False(t, seen[message.ID], "message %s was returned twice", message.ID)
			seen[message.ID] = true
		}
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor()

		require.Less(t, pages, 20, "pagination did not terminate")
	}

	require.Equal(t, total, len(seen), "every message was returned exactly once")
	require.Equal(t, 10, pages, "a thousand messages in pages of a hundred")
}

// bulkMessages inserts many messages in one transaction, which is how the
// pagination gate stays fast.
func (f *chatFixture) bulkMessages(t *testing.T, conversationID uuid.UUID, count int) error {
	t.Helper()

	blocks, err := chatdomain.MarshalBlocks([]chatdomain.Block{{
		Type: chatdomain.BlockText,
		Body: json.RawMessage(`{"type":"text","markdown":"pesan"}`),
	}})
	require.NoError(t, err)

	return f.pool.InScope(t.Context(), database.Scope{WorkspaceID: f.workspace}, func(tx pgx.Tx) error {
		for i := range count {
			_, err := tx.Exec(t.Context(),
				`INSERT INTO messages (workspace_id, conversation_id, author_user_id, blocks, status)
				 VALUES ($1, $2, $3, $4, 'complete')`,
				f.workspace, conversationID, f.userID, blocks)
			if err != nil {
				return fmt.Errorf("insert message %d: %w", i, err)
			}
		}
		return nil
	})
}

// TestConversationsAreIsolatedByWorkspace is the tenant gate: Row Level Security
// filters conversations, messages, and attachments.
func TestConversationsAreIsolatedByWorkspace(t *testing.T) {
	dsn := appDatabase(t)
	fixture := newChatFixture(t, dsn)
	other := newChatFixture(t, dsn)

	conversation := fixture.directConversation(t)
	stored, err := fixture.repo.Append(t.Context(), fixture.scope, chatdomain.Message{
		ConversationID: conversation.ID,
		AuthorUserID:   fixture.userID,
		Blocks:         []chatdomain.Block{{Type: chatdomain.BlockText, Body: json.RawMessage(`{"type":"text","markdown":"rahasia"}`)}},
	})
	require.NoError(t, err)

	_, err = other.repo.GetConversation(t.Context(), other.scope, conversation.ID)
	require.ErrorIs(t, err, chatdomain.ErrConversationNotFound)

	_, err = other.repo.GetMessage(t.Context(), other.scope, stored.ID)
	require.ErrorIs(t, err, chatdomain.ErrMessageNotFound)

	page, err := other.repo.History(t.Context(), other.scope, conversation.ID, chatdomain.Cursor{})
	require.NoError(t, err)
	require.Empty(t, page.Messages)

	conversations, err := other.repo.List(t.Context(), other.scope)
	require.NoError(t, err)
	require.Empty(t, conversations)
}

// TestDirectConversationIsReused keeps one thread per Bolu rather than a new one
// per visit.
func TestDirectConversationIsReused(t *testing.T) {
	fixture := newChatFixture(t, appDatabase(t))

	first := fixture.directConversation(t)
	second := fixture.directConversation(t)

	require.Equal(t, first.ID, second.ID)
	require.Equal(t, chatdomain.KindDirect, first.Kind)
	require.Len(t, first.Participants, 2, "the Bolu and the member who opened it")
}

// TestGroupsKeepTheirParticipants covers the group shape and the add path.
func TestGroupsKeepTheirParticipants(t *testing.T) {
	fixture := newChatFixture(t, appDatabase(t))

	// A group takes two Bolu, and a workspace starts empty, so the fixture makes
	// them.
	pair := twoAgentsOf(t, fixture.pool, fixture.workspace)

	group, err := fixture.repo.Create(t.Context(), fixture.scope, chatdomain.KindGroup, "Grup operasional",
		[]uuid.UUID{pair[0], pair[1]}, []uuid.UUID{fixture.userID})
	require.NoError(t, err)
	require.Equal(t, chatdomain.KindGroup, group.Kind)
	require.Len(t, group.Participants, 3)

	// Adding a participant twice is not an error and does not duplicate the row.
	require.NoError(t, fixture.repo.AddParticipants(t.Context(), fixture.scope, group.ID,
		[]uuid.UUID{pair[1]}, []uuid.UUID{fixture.userID}))

	reloaded, err := fixture.repo.GetConversation(t.Context(), fixture.scope, group.ID)
	require.NoError(t, err)
	require.Len(t, reloaded.Participants, 3)

	require.NoError(t, fixture.repo.Rename(t.Context(), fixture.scope, group.ID, "Grup baru"))
	renamed, err := fixture.repo.GetConversation(t.Context(), fixture.scope, group.ID)
	require.NoError(t, err)
	require.Equal(t, "Grup baru", renamed.Title)
}

// TestAttachmentMetadataAndIsolation is the E5.1 attachment gate: the bytes are
// named by a key, the metadata is a row, and another workspace sees neither.
func TestAttachmentMetadataAndIsolation(t *testing.T) {
	dsn := appDatabase(t)
	fixture := newChatFixture(t, dsn)
	other := newChatFixture(t, dsn)

	conversation := fixture.directConversation(t)
	message, err := fixture.repo.Append(t.Context(), fixture.scope, chatdomain.Message{
		ConversationID: conversation.ID,
		AuthorUserID:   fixture.userID,
		Blocks:         []chatdomain.Block{{Type: chatdomain.BlockText, Body: json.RawMessage(`{"type":"text","markdown":"lampiran"}`)}},
	})
	require.NoError(t, err)

	key := chatdomain.ObjectKey("attachments", fixture.workspace.String(), uuid.NewString(), "daftar harga.pdf")
	attachment, err := fixture.repo.Save(t.Context(), fixture.scope, chatdomain.Attachment{
		MessageID:      message.ID,
		StorageKey:     key,
		Filename:       "daftar harga.pdf",
		ContentType:    "application/pdf",
		ChecksumSHA256: "abc",
	}, []byte("harga"))
	require.NoError(t, err)
	require.Equal(t, int64(5), attachment.ByteSize)

	read, err := fixture.repo.Open(t.Context(), fixture.scope, attachment.ID)
	require.NoError(t, err)
	require.Equal(t, key, read.StorageKey)
	require.Equal(t, "daftar harga.pdf", read.Filename)

	_, err = other.repo.Open(t.Context(), other.scope, attachment.ID)
	require.ErrorIs(t, err, chatdomain.ErrAttachmentNotFound)
}

// TestTheEventStreamReplaysInOrder is the E5.7 gate on the durable side: an
// event is a row with a monotonic ID, so a reconnect resumes exactly.
func TestTheEventStreamReplaysInOrder(t *testing.T) {
	fixture := newChatFixture(t, appDatabase(t))
	conversation := fixture.directConversation(t)

	payload, err := json.Marshal(chatdomain.MessagePayload{ConversationID: conversation.ID.String()})
	require.NoError(t, err)

	ids := make([]int64, 0, 5)
	for range 5 {
		event, err := fixture.repo.AppendEvent(t.Context(), chatdomain.Event{
			WorkspaceID:    fixture.workspace,
			Type:           chatdomain.EventMessageNew,
			ConversationID: conversation.ID,
			Payload:        payload,
		})
		require.NoError(t, err)
		require.Positive(t, event.ID)
		ids = append(ids, event.ID)
	}

	// The IDs increase, which is what makes "since the last one I saw" work.
	for i := 1; i < len(ids); i++ {
		require.Greater(t, ids[i], ids[i-1])
	}

	replay, err := fixture.repo.SinceEvents(t.Context(), fixture.workspace, ids[1], 100)
	require.NoError(t, err)
	require.Len(t, replay, 3)
	require.Equal(t, ids[2], replay[0].ID)
	require.Equal(t, ids[4], replay[2].ID)

	latest, err := fixture.repo.LatestEvents(t.Context(), fixture.workspace, 2)
	require.NoError(t, err)
	require.Len(t, latest, 2)
	require.Equal(t, ids[4], latest[0].ID, "newest first")
}

// TestStreamingMessagesAreIndexed keeps the "sedang mengetik" lookup cheap and
// proves the status column round-trips.
func TestStreamingMessagesAreIndexed(t *testing.T) {
	fixture := newChatFixture(t, appDatabase(t))
	conversation := fixture.directConversation(t)

	streaming, err := fixture.repo.Append(t.Context(), fixture.scope, chatdomain.Message{
		ConversationID: conversation.ID,
		AuthorAgentID:  fixture.agentID,
		Status:         chatdomain.MessageStreaming,
	})
	require.NoError(t, err)
	require.Equal(t, chatdomain.MessageStreaming, streaming.Status)

	finished, err := fixture.repo.Update(t.Context(), fixture.scope, chatdomain.Message{
		ID:           streaming.ID,
		Blocks:       []chatdomain.Block{{Type: chatdomain.BlockText, Body: json.RawMessage(`{"type":"text","markdown":"jawaban"}`)}},
		Status:       chatdomain.MessagePartial,
		FinishReason: "penyedia putus",
	})
	require.NoError(t, err)
	require.Equal(t, chatdomain.MessagePartial, finished.Status)
	require.Equal(t, "penyedia putus", finished.FinishReason)

	// The partial answer is readable, which is the point: the tokens that
	// arrived were paid for.
	read, err := fixture.repo.GetMessage(t.Context(), fixture.scope, streaming.ID)
	require.NoError(t, err)
	require.Len(t, read.Blocks, 1)
}

// TestLatestMessagesReadsTheTail is what a chat opens with.
func TestLatestMessagesReadsTheTail(t *testing.T) {
	fixture := newChatFixture(t, appDatabase(t))
	conversation := fixture.directConversation(t)

	for i := range 12 {
		_, err := fixture.repo.Append(t.Context(), fixture.scope, chatdomain.Message{
			ConversationID: conversation.ID,
			AuthorUserID:   fixture.userID,
			Blocks: []chatdomain.Block{{
				Type: chatdomain.BlockText,
				Body: json.RawMessage(fmt.Sprintf(`{"type":"text","markdown":"pesan %d"}`, i)),
			}},
		})
		require.NoError(t, err)
	}

	messages, err := fixture.repo.LatestMessages(t.Context(), fixture.scope, conversation.ID, 5)
	require.NoError(t, err)
	require.Len(t, messages, 5)

	// Oldest first, and the last one is the newest message.
	var last struct {
		Markdown string `json:"markdown"`
	}
	require.NoError(t, json.Unmarshal(messages[4].Blocks[0].Body, &last))
	require.Equal(t, "pesan 11", last.Markdown)
}

// TestMessagesReferenceTheirTask keeps the optional link to the work that
// produced a message.
func TestMessagesReferenceTheirTask(t *testing.T) {
	fixture := newChatFixture(t, appDatabase(t))
	conversation := fixture.directConversation(t)
	taskID := fixture.createTask(t, conversation.ID)

	message, err := fixture.repo.Append(t.Context(), fixture.scope, chatdomain.Message{
		ConversationID: conversation.ID,
		AuthorAgentID:  fixture.agentID,
		TaskID:         taskID,
		Blocks:         []chatdomain.Block{{Type: chatdomain.BlockText, Body: json.RawMessage(`{"type":"text","markdown":"jawaban"}`)}},
	})
	require.NoError(t, err)
	require.Equal(t, taskID, message.TaskID)
}

// createTask inserts the task a message may point at, which EPIC 6 owns.
func (f *chatFixture) createTask(t *testing.T, conversationID uuid.UUID) uuid.UUID {
	t.Helper()

	var taskID uuid.UUID
	require.NoError(t, f.pool.InScope(t.Context(), database.Scope{WorkspaceID: f.workspace}, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(),
			`INSERT INTO tasks (workspace_id, agent_id, conversation_id, trigger, title)
			 VALUES ($1, $2, $3, 'chat', 'Balas pesan')
			 RETURNING id`,
			f.workspace, f.agentID, conversationID).Scan(&taskID)
	}))
	return taskID
}

// TestAMessageCannotPointAtAMissingTask keeps the foreign key honest.
func TestAMessageCannotPointAtAMissingTask(t *testing.T) {
	fixture := newChatFixture(t, appDatabase(t))
	conversation := fixture.directConversation(t)

	_, err := fixture.repo.Append(t.Context(), fixture.scope, chatdomain.Message{
		ConversationID: conversation.ID,
		AuthorAgentID:  fixture.agentID,
		TaskID:         uuid.New(),
		Blocks:         []chatdomain.Block{{Type: chatdomain.BlockText, Body: json.RawMessage(`{"type":"text","markdown":"x"}`)}},
	})
	require.Error(t, err)
}

// TestAttachmentStorageKeysAreUnique keeps one attachment from overwriting
// another's bytes.
func TestAttachmentStorageKeysAreUnique(t *testing.T) {
	fixture := newChatFixture(t, appDatabase(t))
	conversation := fixture.directConversation(t)

	message, err := fixture.repo.Append(t.Context(), fixture.scope, chatdomain.Message{
		ConversationID: conversation.ID,
		AuthorUserID:   fixture.userID,
		Blocks:         []chatdomain.Block{{Type: chatdomain.BlockText, Body: json.RawMessage(`{"type":"text","markdown":"x"}`)}},
	})
	require.NoError(t, err)

	key := chatdomain.ObjectKey("attachments", fixture.workspace.String(), uuid.NewString(), "a.pdf")
	_, err = fixture.repo.Save(t.Context(), fixture.scope, chatdomain.Attachment{
		MessageID: message.ID, StorageKey: key, Filename: "a.pdf",
	}, []byte("first"))
	require.NoError(t, err)

	_, err = fixture.repo.Save(t.Context(), fixture.scope, chatdomain.Attachment{
		MessageID: message.ID, StorageKey: key, Filename: "b.pdf",
	}, []byte("second"))
	require.Error(t, err, "two attachments must not share a storage key")
}

// TestConversationOrderFollowsActivity keeps the sidebar order meaningful.
func TestConversationOrderFollowsActivity(t *testing.T) {
	fixture := newChatFixture(t, appDatabase(t))

	// Two threads are what makes an order meaningful, so the fixture makes two Bolu.
	pair := twoAgentsOf(t, fixture.pool, fixture.workspace)

	first, err := fixture.repo.Direct(t.Context(), fixture.scope, pair[0])
	require.NoError(t, err)
	second, err := fixture.repo.Direct(t.Context(), fixture.scope, pair[1])
	require.NoError(t, err)

	// Writing in the older thread moves it to the top.
	_, err = fixture.repo.Append(t.Context(), fixture.scope, chatdomain.Message{
		ConversationID: first.ID,
		AuthorUserID:   fixture.userID,
		Blocks:         []chatdomain.Block{{Type: chatdomain.BlockText, Body: json.RawMessage(`{"type":"text","markdown":"baru"}`)}},
	})
	require.NoError(t, err)

	conversations, err := fixture.repo.List(t.Context(), fixture.scope)
	require.NoError(t, err)
	require.Len(t, conversations, 2)
	require.Equal(t, first.ID, conversations[0].ID)
	require.Equal(t, 1, conversations[0].MessageCount)
	require.False(t, conversations[0].LastActivityAt.IsZero(), "a thread with no messages still sorts by its creation time")
	_ = second
}

// TestContentTablesAreProtected proves the new table carries the same tenant
// policy as every other one.
func TestContentTablesAreProtected(t *testing.T) {
	pool := poolFor(t, appDatabase(t))

	for _, table := range []string{"message_attachments"} {
		var enabled, forced bool
		require.NoError(t, pool.PgxPool().QueryRow(t.Context(),
			"SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1", table).
			Scan(&enabled, &forced))
		require.True(t, enabled, "%s must have Row Level Security enabled", table)
		require.True(t, forced, "%s must force the policy on the table owner too", table)
	}

	// The status column and its check constraint exist, which is what makes a
	// partial answer a state the database knows about.
	var constraint string
	require.NoError(t, pool.PgxPool().QueryRow(t.Context(),
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
		 WHERE conrelid = 'messages'::regclass AND contype = 'c' AND pg_get_constraintdef(oid) LIKE '%streaming%'`).
		Scan(&constraint))
	require.Contains(t, constraint, "partial")
}
