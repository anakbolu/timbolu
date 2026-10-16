package handler

import (
	"bytes"
	"errors"
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

// streamIdentity is what the container's stream middleware stores on a request
// that authenticated from the query string.
type streamIdentity struct {
	userID      uuid.UUID
	workspaceID uuid.UUID
}

func newStreamApp(t *testing.T, service chatdomain.Service, id streamIdentity) *fiber.App {
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

	StreamRoutes(group, handler)
	return app
}

// TestSSEStreamsTheBacklogAndTheLiveEvents is the E5.7 gate on the wire format:
// each frame carries its event id, which is what a reconnect resumes from.
func TestSSEStreamsTheBacklogAndTheLiveEvents(t *testing.T) {
	service := mocks.NewService(t)
	id := streamIdentity{userID: uuid.New(), workspaceID: uuid.New()}
	conversationID := uuid.New()

	events := make(chan chatdomain.Event, 4)
	events <- chatdomain.Event{
		ID:             7,
		WorkspaceID:    id.workspaceID,
		Type:           chatdomain.EventMessageNew,
		ConversationID: conversationID,
		Payload:        []byte(`{"message_id":"a"}`),
		CreatedAt:      time.Now(),
	}
	events <- chatdomain.Event{
		ID:             8,
		WorkspaceID:    id.workspaceID,
		Type:           chatdomain.EventAgentState,
		ConversationID: conversationID,
		Payload:        []byte(`{"agent_id":"b","state":"working"}`),
		CreatedAt:      time.Now(),
	}
	close(events)

	// The resume point arrives in the header the browser sends on its own.
	service.EXPECT().Stream(mock.Anything, mock.Anything, int64(6)).Return(events, nil).Once()

	app := newStreamApp(t, service, id)

	req := httptest.NewRequest(fiber.MethodGet, "/api/events/stream", nil)
	req.Header.Set("Last-Event-ID", "6")
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	require.Contains(t, resp.Header.Get("Cache-Control"), "no-cache")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	text := string(body)

	require.Contains(t, text, "id: 7\n")
	require.Contains(t, text, "event: message.new\n")
	require.Contains(t, text, `"message_id":"a"`)
	require.Contains(t, text, "id: 8\n")
	require.Contains(t, text, "event: agent.state\n")
	require.Contains(t, text, `"state":"working"`)

	// The frames are separated by a blank line, which is what ends an event.
	require.Contains(t, text, "\n\n")
}

// TestSSEResumesFromTheQueryParameter covers the client that reconnects
// explicitly rather than through the browser's own header.
func TestSSEResumesFromTheQueryParameter(t *testing.T) {
	service := mocks.NewService(t)
	id := streamIdentity{userID: uuid.New(), workspaceID: uuid.New()}

	events := make(chan chatdomain.Event)
	close(events)

	service.EXPECT().Stream(mock.Anything, mock.Anything, int64(42)).Return(events, nil).Once()

	app := newStreamApp(t, service, id)
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/api/events/stream?last_event_id=42", nil), -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusOK, resp.StatusCode)
}

// TestSSEIgnoresAnUnreadableResumePoint: a malformed id starts from the
// beginning rather than failing the connection.
func TestSSEIgnoresAnUnreadableResumePoint(t *testing.T) {
	service := mocks.NewService(t)
	id := streamIdentity{userID: uuid.New(), workspaceID: uuid.New()}

	events := make(chan chatdomain.Event)
	close(events)

	service.EXPECT().Stream(mock.Anything, mock.Anything, int64(0)).Return(events, nil).Once()

	app := newStreamApp(t, service, id)
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/api/events/stream?last_event_id=bukan-angka", nil), -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusOK, resp.StatusCode)
}

// TestSocketRefusesAPlainRequest keeps the WebSocket endpoint honest about what
// it speaks, instead of answering with an empty 200.
func TestSocketRefusesAPlainRequest(t *testing.T) {
	service := mocks.NewService(t)

	app := newStreamApp(t, service, streamIdentity{userID: uuid.New(), workspaceID: uuid.New()})
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/api/events/socket", nil), -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusUpgradeRequired, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "upgrade_required")
	require.Contains(t, string(body), "/events/stream", "the refusal names the fallback")
}

// TestStreamFailureIsReported keeps a broken stream a visible error.
func TestStreamFailureIsReported(t *testing.T) {
	service := mocks.NewService(t)
	id := streamIdentity{userID: uuid.New(), workspaceID: uuid.New()}

	service.EXPECT().Stream(mock.Anything, mock.Anything, mock.Anything).
		Return(nil, errors.New("the live stream is not configured")).Once()

	app := newStreamApp(t, service, id)
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/api/events/stream", nil), -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
}

// TestUploadStoresAFile covers the multipart path: the file is read, bounded, and
// handed to the service, and the response carries a URL a client can follow.
func TestUploadStoresAFile(t *testing.T) {
	service := mocks.NewService(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}
	conversationID := uuid.New()
	messageID := uuid.New()
	attachment := chatdomain.Attachment{
		ID:          uuid.New(),
		MessageID:   messageID,
		Filename:    "daftar harga.pdf",
		ContentType: "application/pdf",
		ByteSize:    5,
	}

	service.EXPECT().Attach(mock.Anything, mock.Anything, mock.MatchedBy(func(req chatdomain.AttachRequest) bool {
		return req.MessageID == messageID && req.Filename == "daftar harga.pdf"
	}), []byte("harga")).Return(attachment, nil).Once()

	app := newTestApp(t, service, id)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "daftar harga.pdf")
	require.NoError(t, err)
	_, err = part.Write([]byte("harga"))
	require.NoError(t, err)
	require.NoError(t, writer.WriteField("message_id", messageID.String()))
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(fiber.MethodPost,
		"/api/conversations/"+conversationID.String()+"/attachments", &body)
	req.Header.Set(fiber.HeaderContentType, writer.FormDataContentType())

	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusCreated, resp.StatusCode)

	content, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(content), `"filename":"daftar harga.pdf"`)
	require.Contains(t, string(content), `"url":"/api/attachments/`+attachment.ID.String()+`"`)
	require.Contains(t, string(content), `"byte_size":5`)
}

// TestUploadWithoutAMessageOpensOne: an upload may precede the send, so the
// server opens the message the file belongs to.
func TestUploadWithoutAMessageOpensOne(t *testing.T) {
	service := mocks.NewService(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}
	conversationID := uuid.New()
	attachment := chatdomain.Attachment{ID: uuid.New(), MessageID: uuid.New(), Filename: "a.txt"}

	service.EXPECT().Attach(mock.Anything, mock.Anything, mock.MatchedBy(func(req chatdomain.AttachRequest) bool {
		return req.ConversationID == conversationID && req.MessageID == uuid.Nil
	}), mock.Anything).Return(attachment, nil).Once()

	app := newTestApp(t, service, id)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "a.txt")
	require.NoError(t, err)
	_, err = part.Write([]byte("a"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(fiber.MethodPost,
		"/api/conversations/"+conversationID.String()+"/attachments", &body)
	req.Header.Set(fiber.HeaderContentType, writer.FormDataContentType())

	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusCreated, resp.StatusCode)
}

// TestUploadRequiresAFile keeps the attachment endpoint from accepting an empty
// multipart body.
func TestUploadRequiresAFile(t *testing.T) {
	service := mocks.NewService(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}

	app := newTestApp(t, service, id)

	req := httptest.NewRequest(fiber.MethodPost, "/api/conversations/"+uuid.NewString()+"/attachments",
		strings.NewReader("--boundary--"))
	req.Header.Set(fiber.HeaderContentType, "multipart/form-data; boundary=boundary")

	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

// TestAddParticipantsValidatesTheBody covers the group management endpoint.
func TestAddParticipantsValidatesTheBody(t *testing.T) {
	service := mocks.NewService(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}
	conversation := conversationFixture()
	conversation.Kind = chatdomain.KindGroup
	conversation.Title = "Grup operasional"
	added := uuid.New()

	service.EXPECT().AddParticipants(mock.Anything, mock.Anything, conversation.ID,
		mock.MatchedBy(func(req chatdomain.AddParticipantsRequest) bool {
			return len(req.AgentIDs) == 1 && req.AgentIDs[0] == added
		})).Return(conversation, nil).Once()

	app := newTestApp(t, service, id)
	resp := do(t, app, fiber.MethodPost, "/api/conversations/"+conversation.ID.String()+"/participants",
		`{"agent_ids":["`+added.String()+`"]}`)

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), "Grup operasional")

	bad := do(t, app, fiber.MethodPost, "/api/conversations/"+conversation.ID.String()+"/participants",
		`{"agent_ids":["bukan-uuid"]}`)
	require.Equal(t, fiber.StatusBadRequest, bad.Code)
}

// TestGetConversation covers the single read.
func TestGetConversation(t *testing.T) {
	service := mocks.NewService(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}
	conversation := conversationFixture()

	service.EXPECT().Conversation(mock.Anything, mock.Anything, conversation.ID).
		Return(conversation, nil).Once()

	app := newTestApp(t, service, id)
	resp := do(t, app, fiber.MethodGet, "/api/conversations/"+conversation.ID.String(), "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"kind":"direct"`)

	bad := do(t, app, fiber.MethodGet, "/api/conversations/bukan-uuid", "")
	require.Equal(t, fiber.StatusBadRequest, bad.Code)
}
