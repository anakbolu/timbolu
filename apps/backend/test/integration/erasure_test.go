package integration

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/container"
	approvaldomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/approval/domain"
	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	memorydomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/memory/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/retention/domain"
	taskdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	"github.com/robzlabz/employeebot/packages/platform/database"
)

// EPIC 13's E13.6 gate: **a request to erase a workspace removes all of it**, verified by asking the
// database afterwards rather than by trusting the call's own report.
//
// The verification the issue asks for is exactly this: run the flow on a test workspace and check
// that no rows, embeddings, or attachments remain. So the test fills a workspace with one of
// everything — a conversation with a message and a file, a task with its steps, a memory with its
// vector, a draft with its audit lines — then erases it and counts what is left. Every assertion is
// against a query, because "the rows are gone" is a fact about the database and not about the
// service's return value.

// erasureFixture is a workspace with data in every module that holds any.
type erasureFixture struct {
	app       *container.Container
	workspace uuid.UUID
	userID    uuid.UUID
	agentID   uuid.UUID
}

func newErasureFixture(t *testing.T, dsn string) *erasureFixture {
	t.Helper()

	ctx := t.Context()
	pool := poolFor(t, dsn)

	user, err := createVerifiedUser(t, pool)
	require.NoError(t, err)

	workspace, err := onboardWorkspace(t, pool, user)
	require.NoError(t, err)

	cfg := retentionConfig(t, dsn)

	app, err := container.New(ctx, cfg, container.WithLogger(zap.NewNop()), container.WithoutHTTP())
	require.NoError(t, err)
	t.Cleanup(app.Close)

	require.NotNil(t, app.Retention, "the erasure path runs through the retention module")

	return &erasureFixture{
		app:       app,
		workspace: workspace,
		userID:    user,
		agentID:   firstAgentOf(t, pool, workspace),
	}
}

// fill puts one row in every table that holds a workspace's data, and returns the object keys it
// wrote.
//
// It goes through the modules' own services and repositories rather than raw inserts, so what the
// test erases is what the product creates — a fixture that wrote rows by hand could easily write a
// shape the erasure path never sees.
func (f *erasureFixture) fill(t *testing.T) []string {
	t.Helper()

	ctx := t.Context()
	chatScope := chatdomain.Scope{UserID: f.userID, WorkspaceID: f.workspace}
	taskScope := taskdomain.Scope{UserID: f.userID, WorkspaceID: f.workspace}

	// A conversation, a message, and a file: the attachment lives in object storage with a row
	// pointing at it.
	conversation, err := f.app.Repositories.Chat.Direct(ctx, chatScope, f.agentID)
	require.NoError(t, err)

	message, err := f.app.Repositories.Chat.Append(ctx, chatScope, chatdomain.Message{
		ConversationID: conversation.ID,
		AuthorUserID:   f.userID,
		Status:         chatdomain.MessageComplete,
	})
	require.NoError(t, err)

	attachment, err := f.app.Services.Chat.Attach(ctx, chatScope, chatdomain.AttachRequest{
		MessageID: message.ID,
		Filename:  "nota.pdf",
	}, pdfOf("isi nota pelanggan"))
	require.NoError(t, err)

	// A task with recorded steps, which is the module's own data.
	task, err := f.app.Services.Task.Dispatch(ctx, taskScope, taskdomain.DispatchRequest{
		AgentID: f.agentID,
		Trigger: taskdomain.TriggerChat,
		Title:   "Rekap pesanan",
		Prompt:  "Rekap pesanan hari ini",
	})
	require.NoError(t, err)

	_, err = f.app.Repositories.Task.Append(ctx, taskScope, taskdomain.Step{
		TaskID: task.ID,
		Kind:   taskdomain.StepThink,
		Input:  []byte(`{"prompt":"Rekap pesanan hari ini"}`),
		Output: []byte(`{"text":"Baik."}`),
	})
	require.NoError(t, err)

	// An audit line, which is the data that cannot be written over or removed from anywhere but
	// an erasure — so a workspace whose trail is empty is a workspace whose erasure proves nothing
	// about the guard the trail carries. Writing it here is what makes the assertion below reach the
	// `app.audit_erase` flag: without it the delete cascades into an empty table, the trigger never
	// fires, and a missing flag goes unnoticed.
	require.NoError(t, f.app.Repositories.Approval.RecordAudit(ctx,
		approvaldomain.Scope{UserID: f.userID, WorkspaceID: f.workspace},
		approvaldomain.AuditEntry{
			Action:       "draft.created",
			ObjectType:   "draft",
			ObjectID:     uuid.NewString(),
			ActorAgentID: f.agentID,
			Metadata:     map[string]any{"action_kind": "gmail.send"},
		}))

	// A memory with its vector, which is the data UU PDP is really about: a derived
	// representation of what a customer said. It is written through the module's own repository
	// with a vector built here, because the service embeds its input and this deployment has no
	// provider configured — what the erasure removes is a row of the shape the product writes,
	// which is what matters.
	_, err = f.app.Repositories.Memory.CreateMemory(ctx,
		memorydomain.Scope{UserID: f.userID, WorkspaceID: f.workspace},
		memorydomain.NewMemory{
			AgentID:   f.agentID,
			Content:   "Ongkir Bandung 15 ribu",
			Kind:      memorydomain.KindFact,
			Embedding: vector(0.25),
			CreatedBy: "user",
		})
	require.NoError(t, err)

	return []string{attachment.StorageKey}
}

// countWorkspaceRows counts every row the schema says belongs to a workspace.
//
// It reads `information_schema` rather than listing the tables, for the same reason the erasure's own
// count does: a list written here would be a second list to keep — and a table the test forgot to
// check is a table whose leftovers the test would report as "nothing remains".
//
// `data_erasures` is deliberately excluded, and the exclusion is the point rather than a
// convenience: it is the one table that holds a workspace id *without* a reference, because the
// record has to outlive the data it describes. Counting it here would make this assertion fail on
// the evidence that the erasure worked.
func (f *erasureFixture) countWorkspaceRows(t *testing.T) int64 {
	t.Helper()

	var total int64
	require.NoError(t, f.app.DB.InScopeRead(t.Context(),
		database.Scope{UserID: f.userID, WorkspaceID: f.workspace}, func(tx pgx.Tx) error {
			rows, err := tx.Query(t.Context(), `
				SELECT format('SELECT %L AS table_name, count(*) AS rows FROM %I.%I WHERE workspace_id = %L',
				              c.table_name, c.table_schema, c.table_name, $1::text)
				FROM information_schema.columns c
				JOIN information_schema.tables t
				  ON t.table_schema = c.table_schema AND t.table_name = c.table_name
				WHERE c.column_name = 'workspace_id'
				  AND c.table_schema = current_schema()
				  AND t.table_type = 'BASE TABLE'
				  AND t.table_name <> 'data_erasures'`, f.workspace.String())
			if err != nil {
				return err
			}

			queries := []string{}
			for rows.Next() {
				var query string
				if err := rows.Scan(&query); err != nil {
					return err
				}
				queries = append(queries, query)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			rows.Close()

			for _, query := range queries {
				var name string
				var count int64
				if err := tx.QueryRow(t.Context(), query).Scan(&name, &count); err != nil {
					return err
				}
				total += count
				if count > 0 {
					t.Logf("workspace %s still has %d rows in %s", f.workspace, count, name)
				}
			}

			return nil
		}))

	return total
}

// TestErasingAWorkspaceLeavesNothingBehind is the issue's own verification, and it checks the three
// places data can hide: the tenant's rows, the object storage, and the row that proves it happened.
func TestErasingAWorkspaceLeavesNothingBehind(t *testing.T) {
	fixture := newErasureFixture(t, retentionDatabase(t))
	ctx := t.Context()

	keys := fixture.fill(t)
	require.NotEmpty(t, keys)

	before := fixture.countWorkspaceRows(t)
	require.Greater(t, before, int64(0), "the fixture wrote something to erase")

	// The attachment's bytes are really there before the erasure: without this the assertion that
	// they are gone afterwards would pass for a fixture that never stored anything.
	for _, key := range keys {
		id := attachmentIDFor(t, fixture, key)
		_, _, err := fixture.app.Services.Chat.AttachmentContent(ctx,
			chatdomain.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace}, id)
		require.NoError(t, err, "the file is readable before the erasure")
	}

	result, err := fixture.app.Retention.Erase(ctx, domain.ErasureRequest{
		WorkspaceID: fixture.workspace,
		RequestedBy: fixture.userID,
	})
	require.NoError(t, err)
	require.Greater(t, result.Objects, int64(0), "the bytes in the bucket were removed")
	require.Greater(t, result.Rows, int64(0), "the rows were counted and removed")

	// 1. No rows anywhere.
	require.Zero(t, fixture.countWorkspaceRows(t), "every row that named the workspace is gone")

	// 2. No objects under either of the module's prefixes.
	for _, prefix := range chatdomain.ObjectPrefixes {
		remaining, err := fixture.app.Services.Chat.EraseObjects(ctx, fixture.workspace)
		require.NoError(t, err)
		require.Zero(t, remaining, "nothing is left under %s", prefix)
	}

	// 3. The workspace itself is gone.
	var workspaceExists bool
	require.NoError(t, fixture.app.DB.InScopeRead(ctx,
		database.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace}, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM workspaces WHERE id = $1)",
				fixture.workspace).Scan(&workspaceExists)
		}))
	require.False(t, workspaceExists, "the tenant row is gone, not flagged")

	// 4. And the record survives it — as values, because the rows it names are gone.
	erasure, err := fixture.app.Retention.FindErasure(ctx, fixture.workspace)
	require.NoError(t, err)
	require.True(t, erasure.Done(), "the erasure completed")
	require.Equal(t, "Toko Sinar", erasure.WorkspaceName, "the name outlived the row that held it")
	require.Equal(t, fixture.userID, erasure.RequestedBy)
	require.Empty(t, erasure.Error)
	require.Equal(t, result.Objects, erasure.Objects)
}

// attachmentIDFor reads the attachment id back from a storage key, which is how the test names the
// file it stored without keeping a second reference to it.
func attachmentIDFor(t *testing.T, f *erasureFixture, key string) uuid.UUID {
	t.Helper()

	attachments := []chatdomain.Attachment{}
	require.NoError(t, f.app.DB.InScopeRead(t.Context(),
		database.Scope{UserID: f.userID, WorkspaceID: f.workspace}, func(tx pgx.Tx) error {
			rows, err := tx.Query(t.Context(),
				"SELECT id FROM message_attachments WHERE workspace_id = $1 AND storage_key = $2",
				f.workspace, key)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id uuid.UUID
				if err := rows.Scan(&id); err != nil {
					return err
				}
				attachments = append(attachments, chatdomain.Attachment{ID: id})
			}

			return rows.Err()
		}))

	require.Len(t, attachments, 1, "the fixture stored exactly one attachment under %s", key)

	return attachments[0].ID
}

// TestAnErasureOfAWorkspaceThatIsAlreadyGoneIsRefused: the name read fails, so the request is
// answered rather than reported as a success that removed nothing.
func TestAnErasureOfAWorkspaceThatIsAlreadyGoneIsRefused(t *testing.T) {
	fixture := newErasureFixture(t, retentionDatabase(t))
	ctx := t.Context()

	fixture.fill(t)

	request := domain.ErasureRequest{WorkspaceID: fixture.workspace, RequestedBy: fixture.userID}

	_, err := fixture.app.Retention.Erase(ctx, request)
	require.NoError(t, err)

	_, err = fixture.app.Retention.Erase(ctx, request)
	require.Error(t, err, "a second request for the same workspace is refused rather than reported as done")
}

// TestTheErasureRecordIsReadableAfterTheDataIsGone is the half of UU PDP that is easy to forget: the
// right to be answered. The lookup reads the record by the id the workspace had.
func TestTheErasureRecordIsReadableAfterTheDataIsGone(t *testing.T) {
	fixture := newErasureFixture(t, retentionDatabase(t))
	ctx := t.Context()

	fixture.fill(t)

	_, err := fixture.app.Retention.Erase(ctx, domain.ErasureRequest{
		WorkspaceID: fixture.workspace,
		RequestedBy: fixture.userID,
	})
	require.NoError(t, err)

	// A workspace that was never erased answers not-found rather than an empty record.
	other := newErasureFixture(t, retentionDatabase(t))

	_, err = fixture.app.Retention.FindErasure(ctx, other.workspace)
	require.Error(t, err, "a workspace that was not erased has no record")

	records, err := fixture.app.Retention.Erasures(ctx, 10)
	require.NoError(t, err)
	require.NotEmpty(t, records, "the record is in the list an auditor reads")
}
