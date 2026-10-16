package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain/mocks"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
	"github.com/robzlabz/employeebot/packages/platform/database"
)

// identity stands in for what the container's middleware stores on the request.
type identity struct {
	userID      uuid.UUID
	workspaceID uuid.UUID
}

func newTestApp(t *testing.T, service chatdomain.Service, id identity) *fiber.App {
	t.Helper()

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			var fiberErr *fiber.Error
			code := fiber.StatusInternalServerError
			if errors.As(err, &fiberErr) {
				code = fiberErr.Code
			}
			return response.Error(c, code, err.Error(), "http_error", nil)
		},
	})

	handler := New(service, zap.NewNop())

	group := app.Group("/api", func(c *fiber.Ctx) error {
		middleware.SetIdentity(c, id.userID, "owner@example.com")
		middleware.SetScope(c, database.Scope{UserID: id.userID, WorkspaceID: id.workspaceID})
		return c.Next()
	})

	Routes(group, handler)
	ManagerRoutes(group, handler)
	ContentRoutes(app, handler)

	return app
}

func do(t *testing.T, app *fiber.App, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)

	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	recorder := httptest.NewRecorder()
	recorder.Code = resp.StatusCode
	for key, values := range resp.Header {
		for _, value := range values {
			recorder.Header().Add(key, value)
		}
	}
	if content, err := io.ReadAll(resp.Body); err == nil {
		recorder.Body.Write(content)
	}
	return recorder
}

func conversationFixture() chatdomain.Conversation {
	return chatdomain.Conversation{
		ID:             uuid.New(),
		WorkspaceID:    uuid.New(),
		Kind:           chatdomain.KindDirect,
		Title:          "",
		MessageCount:   2,
		LastActivityAt: time.Now(),
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
		Participants: []chatdomain.Participant{
			{ID: uuid.New(), AgentID: uuid.New(), DisplayName: "Oren", Role: "Penjualan"},
			{ID: uuid.New(), UserID: uuid.New(), DisplayName: "owner@example.com", Role: "member"},
		},
	}
}

func messageFixture(conversationID uuid.UUID) chatdomain.Message {
	return chatdomain.Message{
		ID:             uuid.New(),
		ConversationID: conversationID,
		AuthorUserID:   uuid.New(),
		Blocks: []chatdomain.Block{{
			Type: chatdomain.BlockText,
			Body: []byte(`{"type":"text","markdown":"Berapa pesanan hari ini?"}`),
		}},
		Status:    chatdomain.MessageComplete,
		CreatedAt: time.Now(),
	}
}

func TestListConversations(t *testing.T) {
	service := mocks.NewService(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}

	service.EXPECT().Conversations(mock.Anything, mock.Anything).Return([]chatdomain.Conversation{conversationFixture()}, nil).Once()

	app := newTestApp(t, service, id)
	resp := do(t, app, fiber.MethodGet, "/api/conversations", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"kind":"direct"`)
	require.Contains(t, resp.Body.String(), `"name":"Oren"`)
	require.Contains(t, resp.Body.String(), `"is_agent":true`)
}

func TestDirectConversation(t *testing.T) {
	service := mocks.NewService(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}
	agentID := uuid.New()

	service.EXPECT().Direct(mock.Anything, mock.Anything, agentID).Return(conversationFixture(), nil).Once()

	app := newTestApp(t, service, id)
	resp := do(t, app, fiber.MethodPost, "/api/conversations/direct", `{"agent_id":"`+agentID.String()+`"}`)

	require.Equal(t, fiber.StatusOK, resp.Code)

	// A malformed id is refused before the service is reached.
	bad := do(t, app, fiber.MethodPost, "/api/conversations/direct", `{"agent_id":"bukan-uuid"}`)
	require.Equal(t, fiber.StatusBadRequest, bad.Code)
	require.Contains(t, bad.Body.String(), "invalid agent id")
}

func TestCreateGroupValidatesItsBody(t *testing.T) {
	service := mocks.NewService(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}

	app := newTestApp(t, service, id)
	resp := do(t, app, fiber.MethodPost, "/api/conversations/groups", `{"title":"Grup","agent_ids":["bukan-uuid"]}`)

	require.Equal(t, fiber.StatusBadRequest, resp.Code)
	require.Contains(t, resp.Body.String(), "invalid agent id")
}

// TestSendDefaultsToReplying: a message is answered unless the client says
// otherwise, which is the behaviour the chat depends on.
func TestSendDefaultsToReplying(t *testing.T) {
	service := mocks.NewService(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}
	conversation := conversationFixture()
	message := messageFixture(conversation.ID)
	replyID := uuid.New()
	responder := chatdomain.AgentRef{ID: uuid.New(), Name: "Oren", Role: "Penjualan"}

	service.EXPECT().Send(mock.Anything, mock.Anything, mock.MatchedBy(func(req chatdomain.SendRequest) bool {
		return req.ConversationID == conversation.ID && req.Reply && req.Text == "Berapa pesanan?"
	})).Return(chatdomain.SendResult{
		Message:        message,
		ReplyMessageID: replyID,
		Responder:      &responder,
		Routed:         true,
	}, nil).Once()

	app := newTestApp(t, service, id)
	resp := do(t, app, fiber.MethodPost, "/api/conversations/"+conversation.ID.String()+"/messages",
		`{"text":"Berapa pesanan?"}`)

	require.Equal(t, fiber.StatusCreated, resp.Code)
	require.Contains(t, resp.Body.String(), `"reply_message_id":"`+replyID.String()+`"`)
	require.Contains(t, resp.Body.String(), `"name":"Oren"`)
	require.Contains(t, resp.Body.String(), `"routed":true`)
	require.Contains(t, resp.Body.String(), `"markdown":"Berapa pesanan hari ini?"`)
}

func TestSendCanSkipTheReply(t *testing.T) {
	service := mocks.NewService(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}
	conversation := conversationFixture()

	service.EXPECT().Send(mock.Anything, mock.Anything, mock.MatchedBy(func(req chatdomain.SendRequest) bool {
		return !req.Reply
	})).Return(chatdomain.SendResult{Message: messageFixture(conversation.ID)}, nil).Once()

	app := newTestApp(t, service, id)
	resp := do(t, app, fiber.MethodPost, "/api/conversations/"+conversation.ID.String()+"/messages",
		`{"text":"Catatan","reply":false}`)

	require.Equal(t, fiber.StatusCreated, resp.Code)
	require.NotContains(t, resp.Body.String(), "reply_message_id")
}

// TestARefusedFileTypeIs415: the file is real and the request is well formed, so the status
// says what is wrong — the media type — rather than blaming the request.
func TestARefusedFileTypeIs415(t *testing.T) {
	service := mocks.NewService(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}
	conversationID := uuid.New()

	service.EXPECT().Attach(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(chatdomain.Attachment{}, fmt.Errorf("%w: .exe is an executable", chatdomain.ErrFileTypeNotAllowed)).Once()

	app := newTestApp(t, service, id)
	resp := upload(t, app, conversationID, "", "a.exe", []byte("MZ"))

	require.Equal(t, fiber.StatusUnsupportedMediaType, resp.Code)
	require.Contains(t, resp.Body.String(), "file_type_not_allowed")
}

func upload(t *testing.T, app *fiber.App, conversationID uuid.UUID, messageID, filename string, content []byte) *httptest.ResponseRecorder {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = part.Write(content)
	require.NoError(t, err)
	if messageID != "" {
		require.NoError(t, writer.WriteField("message_id", messageID))
	}
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(fiber.MethodPost,
		"/api/conversations/"+conversationID.String()+"/attachments", &body)
	req.Header.Set(fiber.HeaderContentType, writer.FormDataContentType())

	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	recorder := httptest.NewRecorder()
	recorder.Code = resp.StatusCode
	if content, err := io.ReadAll(resp.Body); err == nil {
		recorder.Body.Write(content)
	}
	return recorder
}

// TestHistoryCarriesAnOpaqueCursor keeps a client from building one by hand.
func TestHistoryCarriesAnOpaqueCursor(t *testing.T) {
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}
	service := mocks.NewService(t)
	conversation := conversationFixture()
	boundary := messageFixture(conversation.ID)

	var captured chatdomain.Cursor
	service.EXPECT().History(mock.Anything, mock.Anything, conversation.ID, mock.Anything).
		RunAndReturn(func(_ context.Context, _ chatdomain.Scope, _ uuid.UUID, cursor chatdomain.Cursor) (chatdomain.Page, error) {
			captured = cursor
			return chatdomain.Page{
				Messages:                 []chatdomain.Message{boundary},
				NextBeforeCreatedAtNanos: boundary.CreatedAt.UnixNano(),
				NextBeforeID:             boundary.ID,
				HasMore:                  true,
			}, nil
		}).Once()

	app := newTestApp(t, service, id)
	resp := do(t, app, fiber.MethodGet, "/api/conversations/"+conversation.ID.String()+"/messages?limit=25", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Equal(t, 25, captured.Limit)
	require.Contains(t, resp.Body.String(), `"has_more":true`)

	// The cursor the response carries is what the next request sends back.
	cursor := encodeCursor(boundary.CreatedAt.UnixNano(), boundary.ID)
	require.Contains(t, resp.Body.String(), cursor)

	nanos, parsedID, ok := decodeCursor(cursor)
	require.True(t, ok)
	require.Equal(t, boundary.CreatedAt.UnixNano(), nanos)
	require.Equal(t, boundary.ID, parsedID)

	// A hand-made cursor is refused rather than passed to the database.
	bad := do(t, app, fiber.MethodGet, "/api/conversations/"+conversation.ID.String()+"/messages?cursor=bukan-cursor", "")
	require.Equal(t, fiber.StatusBadRequest, bad.Code)
	require.Contains(t, bad.Body.String(), "invalid cursor")
}

func TestEventsReplayPassesTheCursorThrough(t *testing.T) {
	service := mocks.NewService(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}

	var (
		afterID int64
		limit   int
	)
	service.EXPECT().Events(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _ chatdomain.Scope, after int64, size int) ([]chatdomain.Event, error) {
			afterID, limit = after, size
			return []chatdomain.Event{{
				ID:          42,
				WorkspaceID: id.workspaceID,
				Type:        chatdomain.EventMessageNew,
				Payload:     []byte(`{"message_id":"x"}`),
				CreatedAt:   time.Now(),
			}}, nil
		}).Once()

	app := newTestApp(t, service, id)
	resp := do(t, app, fiber.MethodGet, "/api/events?after_id=7&limit=50", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Equal(t, int64(7), afterID)
	require.Equal(t, 50, limit)
	require.Contains(t, resp.Body.String(), `"id":42`)
	require.Contains(t, resp.Body.String(), `"type":"message.new"`)
}

// TestContentPolicyNamesTheFramingApplication is the gate on the directive that
// makes the block visible at all.
//
// The document is served from a different host than the application by design, so
// `frame-ancestors 'self'` would refuse every frame and the block would never
// appear. The application's own origin has to be named instead.
func TestContentPolicyNamesTheFramingApplication(t *testing.T) {
	service := mocks.NewService(t)
	reference := chatdomain.EncodeContentRef(chatdomain.ContentRefPrefix + "/workspaces/a/objects/b/index.html")

	service.EXPECT().Content(mock.Anything, reference).Return([]byte("<html></html>"), nil).Once()

	handler := New(service, zap.NewNop()).WithContent(ContentConfig{
		FrameAncestors: []string{"https://app.bolu.id"},
	})

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	ContentRoutes(app, handler)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/content/"+reference, nil), -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	policy := resp.Header.Get("Content-Security-Policy")
	require.Contains(t, policy, "frame-ancestors https://app.bolu.id")
	require.NotContains(t, policy, "frame-ancestors 'self'", "a self-only policy cannot frame a cross-origin document")
	require.Contains(t, policy, "connect-src 'none'")
}

// TestContentIsServedUnderAStrictPolicy is the E5.6 gate on the response: the
// policy is what makes agent-written script safe, not the sandbox attribute
// alone.
func TestContentIsServedUnderAStrictPolicy(t *testing.T) {
	service := mocks.NewService(t)
	reference := chatdomain.EncodeContentRef(chatdomain.ContentRefPrefix + "/workspaces/a/objects/b/index.html")

	service.EXPECT().Content(mock.Anything, reference).Return([]byte("<html><script>document.cookie</script></html>"), nil).Once()

	app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New()})
	resp := do(t, app, fiber.MethodGet, "/content/"+reference, "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Equal(t, "text/html; charset=utf-8", resp.Header().Get("Content-Type"))
	require.Equal(t, "nosniff", resp.Header().Get("X-Content-Type-Options"))
	require.Equal(t, "no-referrer", resp.Header().Get("Referrer-Policy"))

	policy := resp.Header().Get("Content-Security-Policy")
	require.Contains(t, policy, "connect-src 'none'", "a script in the document must not reach the network")
	require.Contains(t, policy, "default-src 'none'")
	require.Contains(t, policy, "form-action 'none'")
	require.Contains(t, policy, "base-uri 'none'")
	require.Contains(t, policy, "frame-ancestors 'self'", "the default allows only this origin")
}

func TestDownloadSendsTheFileInline(t *testing.T) {
	service := mocks.NewService(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}
	attachment := chatdomain.Attachment{
		ID:          uuid.New(),
		Filename:    "daftar harga.pdf",
		ContentType: "application/pdf",
		ByteSize:    5,
	}

	service.EXPECT().AttachmentContent(mock.Anything, mock.Anything, attachment.ID).
		Return(attachment, []byte("harga"), nil).Once()

	app := newTestApp(t, service, id)
	resp := do(t, app, fiber.MethodGet, "/api/attachments/"+attachment.ID.String(), "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Equal(t, "harga", resp.Body.String())
	require.Equal(t, "application/pdf", resp.Header().Get("Content-Type"))
	require.Contains(t, resp.Header().Get("Content-Disposition"), "daftar harga.pdf")
	require.Equal(t, "nosniff", resp.Header().Get("X-Content-Type-Options"))
}

// TestFailuresMapToStatuses is the error contract the frontend reads.
func TestFailuresMapToStatuses(t *testing.T) {
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}

	tests := map[string]struct {
		err    error
		status int
		code   string
	}{
		"missing conversation": {err: chatdomain.ErrConversationNotFound, status: fiber.StatusNotFound, code: "conversation_not_found"},
		"missing message":      {err: chatdomain.ErrMessageNotFound, status: fiber.StatusNotFound, code: "message_not_found"},
		"missing attachment":   {err: chatdomain.ErrAttachmentNotFound, status: fiber.StatusNotFound, code: "attachment_not_found"},
		"not in the group":     {err: chatdomain.ErrAgentNotInConversation, status: fiber.StatusBadRequest, code: "agent_not_in_conversation"},
		"nobody can answer":    {err: chatdomain.ErrNoResponder, status: fiber.StatusConflict, code: "no_responder"},
		"invalid block":        {err: chatdomain.ErrInvalidBlock, status: fiber.StatusBadRequest, code: "invalid_block"},
		"block too large":      {err: chatdomain.ErrBlockTooLarge, status: fiber.StatusRequestEntityTooLarge, code: "block_too_large"},
		"invalid input":        {err: chatdomain.ErrInvalidInput, status: fiber.StatusBadRequest, code: "invalid_input"},
		"no storage":           {err: chatdomain.ErrStorageUnavailable, status: fiber.StatusServiceUnavailable, code: "storage_not_configured"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			service := mocks.NewService(t)
			service.EXPECT().Send(mock.Anything, mock.Anything, mock.Anything).
				Return(chatdomain.SendResult{}, test.err).Once()

			app := newTestApp(t, service, id)
			resp := do(t, app, fiber.MethodPost, "/api/conversations/"+uuid.NewString()+"/messages", `{"text":"halo"}`)

			require.Equal(t, test.status, resp.Code)
			require.Contains(t, resp.Body.String(), test.code)
		})
	}
}

// TestTheBlockDocumentIsWhatAClientReceives keeps the stored form and the
// delivered form the same: a client stores what it renders.
func TestTheBlockDocumentIsWhatAClientReceives(t *testing.T) {
	service := mocks.NewService(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}
	conversation := conversationFixture()

	message := messageFixture(conversation.ID)
	message.Blocks = []chatdomain.Block{
		{Type: chatdomain.BlockText, Body: []byte(`{"type":"text","markdown":"Rekap"}`)},
		{Type: chatdomain.BlockChart, Body: []byte(`{"type":"chart","spec":{"kind":"pie","slices":[{"name":"Kaos","value":10}]}}`)},
	}

	service.EXPECT().Send(mock.Anything, mock.Anything, mock.Anything).
		Return(chatdomain.SendResult{Message: message}, nil).Once()

	app := newTestApp(t, service, id)
	resp := do(t, app, fiber.MethodPost, "/api/conversations/"+conversation.ID.String()+"/messages", `{"text":"Rekap"}`)

	require.Equal(t, fiber.StatusCreated, resp.Code)
	require.Contains(t, resp.Body.String(), `"type":"chart"`)
	require.Contains(t, resp.Body.String(), `"slices"`)
	require.NotContains(t, resp.Body.String(), `"Body"`, "the wrapper never reaches a client")
}
