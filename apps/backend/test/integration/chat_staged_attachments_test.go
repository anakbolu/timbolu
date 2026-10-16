package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	chathandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/handler"
	chatservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/service"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/packages/platform/database"
)

// The staged-attachment flow is the fix for the order the UI used to send a file in: the message
// went first with `reply: true`, the reply task started, and only then was the file uploaded
// against the message that came back — so the model never saw the file on the turn it was sent
// with. What these tests hold is that the two halves now arrive as one committed fact: the file is
// uploaded first, the message names it, and the binding and the message are stored in the same
// transaction, before anything reads the message.
//
// The ordering is the part a test can pass by accident, so it is not asserted by counting calls:
// the task starter below reads the attachment row from Postgres at the moment it is called, which
// is what a worker would have seen, and the assertions are made about that read.

// stagedRig is one workspace with the chat service over the real repository, an object store the
// test can inspect, and a task runtime that looks at the database while it runs.
type stagedRig struct {
	fixture *chatFixture
	service *chatservice.Service
	objects *attachObjects
	starter *stagedStarter
	app     *fiber.App
}

func newStagedRig(t *testing.T) *stagedRig {
	t.Helper()

	fixture := newChatFixture(t, appDatabase(t))
	objects := &attachObjects{}
	starter := &stagedStarter{pool: fixture.pool, workspace: fixture.workspace}

	service := chatservice.New(chatservice.Deps{
		Conversations: fixture.repo,
		Messages:      fixture.repo,
		Attachments:   fixture.repo,
		Events:        fixture.repo,
		Storage:       objects,
		Agents:        stagedAgents{},
		Tasks:         starter,
	})

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	group := app.Group("/api", func(c *fiber.Ctx) error {
		middleware.SetIdentity(c, fixture.userID, "owner@example.com")
		middleware.SetScope(c, database.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace})
		return c.Next()
	})
	chathandler.Routes(group, chathandler.New(service, zap.NewNop()))

	return &stagedRig{fixture: fixture, service: service, objects: objects, starter: starter, app: app}
}

// stagedAgents answers the two questions the reply path asks about a Bolu: that it is in the
// thread, and that it is awake. Nothing about it is under test here, so a stub is the honest
// stand-in — the subject is what the database said when the task was opened.
type stagedAgents struct{}

func (stagedAgents) Agent(_ context.Context, _ chatdomain.Scope, id uuid.UUID) (chatdomain.AgentRef, error) {
	return stagedAgents{}.ref(id), nil
}

func (stagedAgents) Agents(_ context.Context, _ chatdomain.Scope, ids []uuid.UUID) ([]chatdomain.AgentRef, error) {
	refs := make([]chatdomain.AgentRef, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, stagedAgents{}.ref(id))
	}
	return refs, nil
}

func (stagedAgents) Active(chatdomain.AgentRef) bool { return true }

func (stagedAgents) ref(id uuid.UUID) chatdomain.AgentRef {
	return chatdomain.AgentRef{ID: id, Name: "Oren", Role: "Penjualan", Stored: "active"}
}

// stagedStarter is the durable runtime, and it is where the ordering is proved.
//
// Start reads the attachment row from Postgres before it returns, and records what it read along
// with the message it was asked to answer. A starter that only counted its calls would pass on
// the flow this change replaces, because that flow also called it — it just called it too early,
// which is exactly what the read makes visible.
type stagedStarter struct {
	pool      *database.Pool
	workspace uuid.UUID

	mu      sync.Mutex
	watched uuid.UUID
	looks   []stagedLook
}

// stagedLook is what one Start saw.
type stagedLook struct {
	// Placeholder is the message the task was asked to answer, which is the agent's empty
	// bubble rather than the message the user wrote.
	Placeholder uuid.UUID
	// Status and MessageID are the attachment row as it stood when Start ran.
	Status    string
	MessageID uuid.UUID
	Err       error
}

// watch names the upload whose binding Start is to report on.
func (s *stagedStarter) watch(id uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.watched = id
}

// seen returns the reads Start made.
func (s *stagedStarter) seen() []stagedLook {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]stagedLook(nil), s.looks...)
}

func (s *stagedStarter) Start(ctx context.Context, _ chatdomain.Scope, req chatdomain.StartRequest) (uuid.UUID, error) {
	look := stagedLook{Placeholder: req.MessageID}

	s.mu.Lock()
	watched := s.watched
	s.mu.Unlock()

	if watched != uuid.Nil {
		var messageID *uuid.UUID
		var status string
		err := s.pool.InScopeRead(ctx,
			database.Scope{WorkspaceID: s.workspace}, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx,
					"SELECT message_id, status FROM message_attachments WHERE id = $1 AND workspace_id = $2",
					watched, s.workspace).Scan(&messageID, &status)
			})
		if err != nil {
			look.Err = err
		} else {
			look.Status = status
			if messageID != nil {
				look.MessageID = *messageID
			}
		}
	}

	s.mu.Lock()
	s.looks = append(s.looks, look)
	s.mu.Unlock()

	return uuid.New(), nil
}

// stagedEnvelope is the response envelope every endpoint here answers in: one of the two fields is
// set, and the payload is decoded by the endpoint's own shape.
type stagedEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

// uploadStaged posts one file the way the new flow does: before the message, against the thread it
// belongs to. The answer is the staged shape — `attachment_id`, `name`, `mime`, `size`, `url` — and
// it is decoded as that shape, so a field that drifted would fail here rather than at a client.
func (r *stagedRig) uploadStaged(t *testing.T, conversationID uuid.UUID, filename string, content []byte) (int, chatdomain.StagedAttachmentPayload, string) {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="file"; filename="`+filename+`"`)
	header.Set("Content-Type", "application/pdf")
	part, err := writer.CreatePart(header)
	require.NoError(t, err)
	_, err = part.Write(content)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(http.MethodPost,
		"/api/conversations/"+conversationID.String()+"/attachments?staged=true", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := r.app.Test(req, -1)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var envelope stagedEnvelope
	require.NoError(t, json.Unmarshal(raw, &envelope), string(raw))

	var payload chatdomain.StagedAttachmentPayload
	if len(envelope.Data) > 0 {
		require.NoError(t, json.Unmarshal(envelope.Data, &payload), string(raw))
	}
	return resp.StatusCode, payload, envelope.Error.Code
}

// send posts one message the way the new flow does: the words and the ids of the files already
// uploaded.
func (r *stagedRig) send(t *testing.T, conversationID uuid.UUID, body string) (int, chatdomain.MessagePayload, string, string) {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost,
		"/api/conversations/"+conversationID.String()+"/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.app.Test(req, -1)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var envelope stagedEnvelope
	require.NoError(t, json.Unmarshal(raw, &envelope), string(raw))

	var payload struct {
		Message        chatdomain.MessagePayload `json:"message"`
		ReplyMessageID string                    `json:"reply_message_id"`
	}
	if len(envelope.Data) > 0 {
		require.NoError(t, json.Unmarshal(envelope.Data, &payload), string(raw))
	}
	return resp.StatusCode, payload.Message, payload.ReplyMessageID, envelope.Error.Code
}

// attachmentRow is the stored shape of one upload, read through the workspace's own scope.
type attachmentRow struct {
	Status         string
	MessageID      *uuid.UUID
	ConversationID *uuid.UUID
	UserID         *uuid.UUID
	StagedAt       *time.Time
}

func (r *stagedRig) attachmentRow(t *testing.T, id uuid.UUID) attachmentRow {
	t.Helper()

	var row attachmentRow
	require.NoError(t, r.fixture.pool.InScopeRead(t.Context(),
		database.Scope{UserID: r.fixture.userID, WorkspaceID: r.fixture.workspace}, func(tx pgx.Tx) error {
			return tx.QueryRow(t.Context(),
				`SELECT status, message_id, conversation_id, user_id, staged_at
				 FROM message_attachments WHERE id = $1 AND workspace_id = $2`,
				id, r.fixture.workspace).
				Scan(&row.Status, &row.MessageID, &row.ConversationID, &row.UserID, &row.StagedAt)
		}))

	return row
}

// messageCount is what a client would see in the thread.
func (r *stagedRig) messageCount(t *testing.T, conversationID uuid.UUID) int {
	t.Helper()

	page, err := r.service.History(t.Context(), r.fixture.scope, conversationID, chatdomain.Cursor{})
	require.NoError(t, err)

	return len(page.Messages)
}

// seenEvent is one message event of a thread, decoded the way the stream sends it.
type seenEvent struct {
	Type    string
	Payload chatdomain.MessagePayload
}

func (r *stagedRig) messageEvents(t *testing.T, conversationID uuid.UUID) []seenEvent {
	t.Helper()

	events, err := r.fixture.repo.SinceEvents(t.Context(), r.fixture.workspace, 0, 500)
	require.NoError(t, err)

	out := make([]seenEvent, 0, len(events))
	for _, event := range events {
		if event.Type != chatdomain.EventMessageNew && event.Type != chatdomain.EventMessageUpdated {
			continue
		}

		var payload chatdomain.MessagePayload
		require.NoError(t, json.Unmarshal(event.Payload, &payload))
		if payload.ConversationID != conversationID.String() {
			continue
		}
		out = append(out, seenEvent{Type: string(event.Type), Payload: payload})
	}

	return out
}

// ofMessage narrows a thread's events to the ones about one message.
func ofMessage(events []seenEvent, messageID string) []seenEvent {
	out := make([]seenEvent, 0, len(events))
	for _, event := range events {
		if event.Payload.MessageID == messageID {
			out = append(out, event)
		}
	}
	return out
}

// idsOf renders attachment ids for a request body.
func idsOf(ids ...string) string {
	quoted := make([]string, 0, len(ids))
	for _, id := range ids {
		quoted = append(quoted, `"`+id+`"`)
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

// TestAStagedUploadIsAFileWithNoMessageAndNoEvent is the first half of the flow on its own: the
// file is stored, and nothing else happened. No message was opened for it — the carrier message
// the old path needed is exactly what made the reply start too early — and no client was told
// anything, because a file in no message's read model is not something a message stream can carry.
func TestAStagedUploadIsAFileWithNoMessageAndNoEvent(t *testing.T) {
	rig := newStagedRig(t)
	conversation := rig.fixture.directConversation(t)

	content := pdfOf("isi laporan")
	status, uploaded, code := rig.uploadStaged(t, conversation.ID, "laporan.pdf", content)
	require.Equal(t, fiber.StatusCreated, status, code)
	require.NotEmpty(t, uploaded.AttachmentID)
	require.Equal(t, "laporan.pdf", uploaded.Name)
	require.Equal(t, "application/pdf", uploaded.Mime)
	require.Equal(t, int64(len(content)), uploaded.Size)
	require.Equal(t, "/api/attachments/"+uploaded.AttachmentID, uploaded.URL, "the wire shape a client follows")

	id := uuid.MustParse(uploaded.AttachmentID)
	row := rig.attachmentRow(t, id)
	require.Equal(t, chatdomain.AttachmentStaged, row.Status)
	require.Nil(t, row.MessageID, "a staged upload belongs to no message")
	require.NotNil(t, row.ConversationID)
	require.Equal(t, conversation.ID, *row.ConversationID, "the thread a bind will check it against")
	require.NotNil(t, row.UserID)
	require.Equal(t, rig.fixture.userID, *row.UserID, "the person a bind will check it against")
	require.NotNil(t, row.StagedAt, "the age the staged sweep reads")

	require.Zero(t, rig.messageCount(t, conversation.ID), "an upload opens no message")
	require.Empty(t, rig.messageEvents(t, conversation.ID), "and publishes nothing")
}

// TestTheTaskStartsAfterTheFilesAreBound is the whole point of the change, and it is asserted from
// inside the task runtime: at the moment Start runs, the attachment is already bound to the
// message the user sent. The model that a worker would hand this message to therefore reads the
// file on the turn it was sent with, which is what the old order could not do.
func TestTheTaskStartsAfterTheFilesAreBound(t *testing.T) {
	rig := newStagedRig(t)
	conversation := rig.fixture.directConversation(t)

	_, uploaded, code := rig.uploadStaged(t, conversation.ID, "nota.pdf", pdfOf("nota bulan ini"))
	require.NotEmpty(t, uploaded.AttachmentID, code)
	attachmentID := uuid.MustParse(uploaded.AttachmentID)

	rig.starter.watch(attachmentID)

	status, sent, replyID, code := rig.send(t, conversation.ID,
		`{"text":"ini notanya","attachment_ids":`+idsOf(uploaded.AttachmentID)+`}`)
	require.Equal(t, fiber.StatusCreated, status, code)
	require.Len(t, sent.Attachments, 1, "the answer already carries the file")
	require.Equal(t, uploaded.AttachmentID, sent.Attachments[0].ID)
	require.NotEmpty(t, replyID, "the message asks for an answer")

	looks := rig.starter.seen()
	require.Len(t, looks, 1, "the reply is dispatched once")
	look := looks[0]
	require.NoError(t, look.Err, "the task runtime must be able to read the attachment")
	require.Equal(t, chatdomain.AttachmentBound, look.Status,
		"the file must already be bound when the task starts")
	require.Equal(t, sent.MessageID, look.MessageID.String(),
		"and bound to the message the user sent, not to the agent's placeholder")
	require.NotEqual(t, replyID, look.MessageID.String())

	row := rig.attachmentRow(t, attachmentID)
	require.Equal(t, chatdomain.AttachmentBound, row.Status)
	require.NotNil(t, row.MessageID)
	require.Equal(t, sent.MessageID, row.MessageID.String())

	events := ofMessage(rig.messageEvents(t, conversation.ID), sent.MessageID)
	require.Len(t, events, 1, "exactly one message.new for the message the user sent")
	require.Equal(t, chatdomain.EventMessageNew, events[0].Type)
	require.Len(t, events[0].Payload.Attachments, 1, "carrying the file, so no client has to fetch it")
	require.Equal(t, uploaded.AttachmentID, events[0].Payload.Attachments[0].ID)
}

// TestAFileFromAnotherThreadCannotBeBound: an attachment id is a client's to guess, and the file
// it names may belong to a thread the sender has no business writing in. The bind is checked
// against the conversation it was uploaded to, so the send is refused — and refused as "not
// found", because whether somebody else's upload exists is not a client's business.
func TestAFileFromAnotherThreadCannotBeBound(t *testing.T) {
	rig := newStagedRig(t)
	first := rig.fixture.directConversation(t)

	agents := twoAgentsOf(t, rig.fixture.pool, rig.fixture.workspace)
	second, err := rig.fixture.repo.Direct(t.Context(), rig.fixture.scope, agents[1])
	require.NoError(t, err)
	require.NotEqual(t, first.ID, second.ID)

	_, uploaded, code := rig.uploadStaged(t, first.ID, "laporan.pdf", pdfOf("isi laporan"))
	require.NotEmpty(t, uploaded.AttachmentID, code)

	status, _, _, errCode := rig.send(t, second.ID,
		`{"text":"ini filenya","reply":false,"attachment_ids":`+idsOf(uploaded.AttachmentID)+`}`)
	require.Equal(t, fiber.StatusNotFound, status)
	require.Equal(t, "attachment_not_found", errCode)

	require.Zero(t, rig.messageCount(t, second.ID), "a refused send stores no message")
	row := rig.attachmentRow(t, uuid.MustParse(uploaded.AttachmentID))
	require.Equal(t, chatdomain.AttachmentStaged, row.Status, "and leaves the upload as it was")
	require.Nil(t, row.MessageID)
}

// TestAFileFromAnotherPersonCannotBeBound: the second half of the ownership check. The thread is
// the same one, so only the uploader differs, and the answer is still "not found" — a file that
// arrived from somebody else's session is not a file this sender may name.
func TestAFileFromAnotherPersonCannotBeBound(t *testing.T) {
	rig := newStagedRig(t)
	conversation := rig.fixture.directConversation(t)

	_, uploaded, code := rig.uploadStaged(t, conversation.ID, "laporan.pdf", pdfOf("isi laporan"))
	require.NotEmpty(t, uploaded.AttachmentID, code)

	// Somebody else in the workspace, which is the only difference from the case above.
	other, err := createVerifiedUser(t, rig.fixture.pool)
	require.NoError(t, err)

	stranger := chatdomain.Scope{UserID: other, WorkspaceID: rig.fixture.workspace}
	_, err = rig.service.Send(t.Context(), stranger, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "ini filenya",
		AttachmentIDs:  []uuid.UUID{uuid.MustParse(uploaded.AttachmentID)},
	})
	require.ErrorIs(t, err, chatdomain.ErrAttachmentNotFound)

	require.Zero(t, rig.messageCount(t, conversation.ID), "a refused send stores no message")
	row := rig.attachmentRow(t, uuid.MustParse(uploaded.AttachmentID))
	require.Equal(t, chatdomain.AttachmentStaged, row.Status)
	require.Nil(t, row.MessageID)
}

// TestAFileAlreadyBoundCannotBeSentTwice: a staged upload is a single-use claim. Sending the same
// id twice must not put one file on two messages, and the second send is refused as a conflict —
// the request is well formed and the file is real, and what is wrong is the state it is in.
func TestAFileAlreadyBoundCannotBeSentTwice(t *testing.T) {
	rig := newStagedRig(t)
	conversation := rig.fixture.directConversation(t)

	_, uploaded, code := rig.uploadStaged(t, conversation.ID, "laporan.pdf", pdfOf("isi laporan"))
	require.NotEmpty(t, uploaded.AttachmentID, code)

	status, sent, _, errCode := rig.send(t, conversation.ID,
		`{"text":"pertama","reply":false,"attachment_ids":`+idsOf(uploaded.AttachmentID)+`}`)
	require.Equal(t, fiber.StatusCreated, status, errCode)
	require.Len(t, sent.Attachments, 1)

	status, _, _, errCode = rig.send(t, conversation.ID,
		`{"text":"kedua","reply":false,"attachment_ids":`+idsOf(uploaded.AttachmentID)+`}`)
	require.Equal(t, fiber.StatusConflict, status)
	require.Equal(t, "attachment_already_bound", errCode)

	require.Equal(t, 1, rig.messageCount(t, conversation.ID), "the second send stored nothing")

	// The file is still on the message it was sent with, and only on that one.
	message, err := rig.fixture.repo.GetMessage(t.Context(), rig.fixture.scope, uuid.MustParse(sent.MessageID))
	require.NoError(t, err)
	require.Len(t, message.Attachments, 1)
}

// TestAMixedSendBindsNothing: one bad id takes the whole send down, including the good id that was
// named beside it. Anything else would leave a client with a message that silently lost a file —
// or, worse, with the file bound to a message that was never stored.
func TestAMixedSendBindsNothing(t *testing.T) {
	rig := newStagedRig(t)
	conversation := rig.fixture.directConversation(t)

	_, bound, code := rig.uploadStaged(t, conversation.ID, "sudah-terkirim.pdf", pdfOf("sudah terkirim"))
	require.NotEmpty(t, bound.AttachmentID, code)
	status, _, _, errCode := rig.send(t, conversation.ID,
		`{"text":"pertama","reply":false,"attachment_ids":`+idsOf(bound.AttachmentID)+`}`)
	require.Equal(t, fiber.StatusCreated, status, errCode)

	_, waiting, code := rig.uploadStaged(t, conversation.ID, "menunggu.pdf", pdfOf("masih menunggu"))
	require.NotEmpty(t, waiting.AttachmentID, code)

	// A file that is already bound, named first, and a good one beside it.
	status, _, _, errCode = rig.send(t, conversation.ID,
		`{"text":"campur","reply":false,"attachment_ids":`+idsOf(bound.AttachmentID, waiting.AttachmentID)+`}`)
	require.Equal(t, fiber.StatusConflict, status)
	require.Equal(t, "attachment_already_bound", errCode)

	require.Equal(t, 1, rig.messageCount(t, conversation.ID), "the refused send stored nothing")
	require.Equal(t, chatdomain.AttachmentStaged, rig.attachmentRow(t, uuid.MustParse(waiting.AttachmentID)).Status,
		"the good file must not have been bound by a send that failed")

	// A file that does not exist, named first, and the same good one beside it.
	missing := uuid.NewString()
	status, _, _, errCode = rig.send(t, conversation.ID,
		`{"text":"campur","reply":false,"attachment_ids":`+idsOf(missing, waiting.AttachmentID)+`}`)
	require.Equal(t, fiber.StatusNotFound, status)
	require.Equal(t, "attachment_not_found", errCode)

	require.Equal(t, 1, rig.messageCount(t, conversation.ID))
	require.Equal(t, chatdomain.AttachmentStaged, rig.attachmentRow(t, uuid.MustParse(waiting.AttachmentID)).Status)

	// And a malformed id is its own answer, so a screen can point at the field rather than at
	// the send.
	status, _, _, errCode = rig.send(t, conversation.ID,
		`{"text":"campur","reply":false,"attachment_ids":["bukan-uuid"]}`)
	require.Equal(t, fiber.StatusBadRequest, status)
	require.Equal(t, "invalid_attachment_id", errCode)
	require.Equal(t, 1, rig.messageCount(t, conversation.ID))

	// The good file is still sendable, which is what "nothing was bound" means in practice.
	status, sent, _, errCode := rig.send(t, conversation.ID,
		`{"text":"akhirnya","reply":false,"attachment_ids":`+idsOf(waiting.AttachmentID)+`}`)
	require.Equal(t, fiber.StatusCreated, status, errCode)
	require.Len(t, sent.Attachments, 1)
}

// TestAMessageMayBeOnlyFiles: a file is a message. The old flow needed an empty carrier message
// for this, which is what started the reply before the file was linked; naming the file in the
// send is the same intent expressed in one request, and an empty send with no file is still
// refused.
func TestAMessageMayBeOnlyFiles(t *testing.T) {
	rig := newStagedRig(t)
	conversation := rig.fixture.directConversation(t)

	_, uploaded, code := rig.uploadStaged(t, conversation.ID, "bukti.png", pngOf("bukti transfer"))
	require.NotEmpty(t, uploaded.AttachmentID, code)

	status, sent, _, errCode := rig.send(t, conversation.ID,
		`{"text":"","reply":false,"attachment_ids":`+idsOf(uploaded.AttachmentID)+`}`)
	require.Equal(t, fiber.StatusCreated, status, errCode)
	require.Len(t, sent.Attachments, 1)
	require.Empty(t, sent.Blocks, "a message with no words has no blocks")

	status, _, _, errCode = rig.send(t, conversation.ID, `{"text":"","reply":false}`)
	require.Equal(t, fiber.StatusBadRequest, status)
	require.Equal(t, "invalid_input", errCode)
	require.Equal(t, 1, rig.messageCount(t, conversation.ID))
}

// TestTheStagedSweepTakesTheFilesNobodySent is the retention half: an upload that was never named
// in a message has no message to be kept with, so it goes on its own clock — bytes and row
// together — while a file that was sent stays with the message that carries it, and one that is
// still waiting stays for its client.
//
// The uploads here are made through the service rather than over HTTP because the subject is the
// sweep; the object store is the deployment's own local driver, so "the bytes are gone" is a fact
// about storage rather than about a fake.
func TestTheStagedSweepTakesTheFilesNobodySent(t *testing.T) {
	fixture := newRetentionFixture(t, retentionDatabase(t))
	ctx := t.Context()

	scope := chatdomain.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace}

	conversation, err := fixture.app.Repositories.Chat.Direct(ctx, scope, fixture.agentID)
	require.NoError(t, err)

	abandoned, err := fixture.app.Services.Chat.Attach(ctx, scope, chatdomain.AttachRequest{
		ConversationID: conversation.ID,
		Filename:       "belum-terkirim.pdf",
		Staged:         true,
	}, pdfOf("diunggah lalu tidak pernah dikirim"))
	require.NoError(t, err)

	fresh, err := fixture.app.Services.Chat.Attach(ctx, scope, chatdomain.AttachRequest{
		ConversationID: conversation.ID,
		Filename:       "baru-saja.pdf",
		Staged:         true,
	}, pdfOf("baru saja diunggah"))
	require.NoError(t, err)

	message := fixture.message(t)
	sent, err := fixture.app.Services.Chat.Attach(ctx, scope, chatdomain.AttachRequest{
		MessageID: message.ID,
		Filename:  "nota-terkirim.pdf",
	}, pdfOf("dikirim bersama pesannya"))
	require.NoError(t, err)

	// Both files are aged past the twenty-four hour staged window, and only one of them is
	// staged. The bound one is well inside the ninety-day attachment policy, so if it goes,
	// the staged sweep took something it had no business taking.
	fixture.ageStagedAttachment(t, abandoned.ID, 48*time.Hour)
	fixture.ageAttachment(t, sent.ID, 48*time.Hour)

	// The events before the pass, so that what the pass published can be told apart from what
	// the uploads above did.
	before, err := fixture.app.Repositories.Chat.SinceEvents(ctx, fixture.workspace, 0, 500)
	require.NoError(t, err)

	result, err := fixture.app.Retention.Run(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, result.Attachments, 1)
	require.GreaterOrEqual(t, result.BytesFreed, int64(len(pdfOf("diunggah lalu tidak pernah dikirim"))))

	_, _, err = fixture.app.Services.Chat.AttachmentContent(ctx, scope, abandoned.ID)
	require.Error(t, err, "the row and the bytes go together")

	_, waiting, err := fixture.app.Services.Chat.AttachmentContent(ctx, scope, fresh.ID)
	require.NoError(t, err)
	require.Equal(t, pdfOf("baru saja diunggah"), waiting, "a file still inside its window stays")

	_, carried, err := fixture.app.Services.Chat.AttachmentContent(ctx, scope, sent.ID)
	require.NoError(t, err)
	require.Equal(t, pdfOf("dikirim bersama pesannya"), carried,
		"the staged clock is not the attachment policy")

	// A staged upload is in no message's read model, so its removal is not something a client
	// could be told about: the pass publishes nothing at all.
	after, err := fixture.app.Repositories.Chat.SinceEvents(ctx, fixture.workspace, 0, 500)
	require.NoError(t, err)
	require.Len(t, after, len(before), "a staged file has no read model to update")
}

// ageStagedAttachment moves an upload's staging moment back, which is the clock the staged sweep
// reads — a separate column from created_at, because a staged file's age is how long it has waited
// for a message rather than how long the deployment has kept it.
func (f *retentionFixture) ageStagedAttachment(t *testing.T, id uuid.UUID, age time.Duration) {
	t.Helper()

	require.NoError(t, f.pool.InScope(t.Context(),
		database.Scope{UserID: f.userID, WorkspaceID: f.workspace}, func(tx pgx.Tx) error {
			_, err := tx.Exec(t.Context(),
				"UPDATE message_attachments SET staged_at = now() - $1::interval WHERE id = $2 AND workspace_id = $3",
				age.String(), id, f.workspace)

			return err
		}))
}
