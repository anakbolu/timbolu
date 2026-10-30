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

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	chatdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/chat/domain"
	chathandler "github.com/anakbolu/timbolu/apps/backend/internal/modules/chat/handler"
	chatservice "github.com/anakbolu/timbolu/apps/backend/internal/modules/chat/service"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/middleware"
	"github.com/anakbolu/timbolu/packages/platform/database"
)

// attachObjects is an in-memory object store: the test is about Postgres rows.
type attachObjects struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (s *attachObjects) Put(_ context.Context, key, contentType string, content []byte) (chatdomain.StoredObject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.objects == nil {
		s.objects = map[string][]byte{}
	}
	s.objects[key] = content
	return chatdomain.StoredObject{Key: key, ContentType: contentType, ByteSize: int64(len(content)), ChecksumSHA256: "sum"}, nil
}
func (s *attachObjects) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objects[key], nil
}
func (s *attachObjects) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	return nil
}
func (s *attachObjects) List(_ context.Context, prefix string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	return keys, nil
}
func (s *attachObjects) Driver() string { return "memory" }

func newAttachService(fixture *chatFixture) *chatservice.Service {
	return chatservice.New(chatservice.Deps{
		Conversations: fixture.repo,
		Messages:      fixture.repo,
		Attachments:   fixture.repo,
		Events:        fixture.repo,
		Storage:       &attachObjects{},
	})
}

// pngOf and pdfOf build content a sniffer reads as those types, because an upload is
// checked against its bytes as well as its name.
func pngOf(text string) []byte { return append([]byte("\x89PNG\r\n\x1a\n"), text...) }
func pdfOf(text string) []byte { return append([]byte("%PDF-1.4\n"), text...) }

// attachedEvent finds the newest message event for a message that carries
// attachments, decoded the way the SSE stream sends it.
func messageEventsFor(t *testing.T, fixture *chatFixture, messageID uuid.UUID) []chatdomain.MessagePayload {
	t.Helper()
	events, err := fixture.repo.SinceEvents(t.Context(), fixture.workspace, 0, 500)
	require.NoError(t, err)
	var out []chatdomain.MessagePayload
	for _, event := range events {
		if event.Type != chatdomain.EventMessageNew && event.Type != chatdomain.EventMessageUpdated {
			continue
		}
		var payload chatdomain.MessagePayload
		require.NoError(t, json.Unmarshal(event.Payload, &payload))
		if payload.MessageID == messageID.String() {
			out = append(out, payload)
		}
	}
	return out
}

// TestAttachWithoutMessageShowsTheFile reproduces QA PR #182 Bug 1: the message
// opened for an upload must carry the file in history and on the event stream.
func TestAttachWithoutMessageShowsTheFile(t *testing.T) {
	fixture := newChatFixture(t, appDatabase(t))
	service := newAttachService(fixture)
	conversation := fixture.directConversation(t)

	// What handler.Attach does without message_id.
	opened, err := service.Send(t.Context(), fixture.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID, Text: "", ForAttachment: true,
	})
	require.NoError(t, err)
	_, err = service.Attach(t.Context(), fixture.scope, chatdomain.AttachRequest{
		MessageID: opened.Message.ID, Filename: "a.png",
	}, pngOf("png-bytes"))
	require.NoError(t, err)

	page, err := service.History(t.Context(), fixture.scope, conversation.ID, chatdomain.Cursor{})
	require.NoError(t, err)
	require.Len(t, page.Messages, 1)
	wire := chatdomain.WireMessage(page.Messages[0])
	require.Len(t, wire.Attachments, 1, "history: the opened message must carry the upload")
	require.Equal(t, "a.png", wire.Attachments[0].Filename)

	events := messageEventsFor(t, fixture, opened.Message.ID)
	require.NotEmpty(t, events)
	last := events[len(events)-1]
	require.Len(t, last.Attachments, 1, "event stream: the last message event must carry the upload")
	require.Equal(t, "a.png", last.Attachments[0].Filename)
}

// TestAttachWithMessageShowsTheFile covers the path with message_id, which had
// the same gap on main.
func TestAttachWithMessageShowsTheFile(t *testing.T) {
	fixture := newChatFixture(t, appDatabase(t))
	service := newAttachService(fixture)
	conversation := fixture.directConversation(t)

	sent, err := service.Send(t.Context(), fixture.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID, Text: "lampiran berikut",
	})
	require.NoError(t, err)
	for _, name := range []string{"satu.pdf", "dua.pdf"} {
		_, err = service.Attach(t.Context(), fixture.scope, chatdomain.AttachRequest{
			MessageID: sent.Message.ID, Filename: name,
		}, pdfOf(name))
		require.NoError(t, err)
	}

	message, err := fixture.repo.GetMessage(t.Context(), fixture.scope, sent.Message.ID)
	require.NoError(t, err)
	require.Len(t, message.Attachments, 2, "messages.attachments must list both files")

	events := messageEventsFor(t, fixture, sent.Message.ID)
	require.NotEmpty(t, events)
	require.Len(t, events[len(events)-1].Attachments, 2)
}

// TestDeletingOneFileKeepsTheOthers: the row and the message's read model go together, and
// the read model is rebuilt from the rows that are still there, so a message carrying two
// files must keep the one that was not deleted.
func TestDeletingOneFileKeepsTheOthers(t *testing.T) {
	fixture := newChatFixture(t, appDatabase(t))
	service := newAttachService(fixture)
	conversation := fixture.directConversation(t)

	first, err := service.Attach(t.Context(), fixture.scope, chatdomain.AttachRequest{
		ConversationID: conversation.ID, Filename: "satu.png",
	}, pngOf("satu"))
	require.NoError(t, err)
	second, err := service.Attach(t.Context(), fixture.scope, chatdomain.AttachRequest{
		MessageID: first.MessageID, Filename: "dua.png",
	}, pngOf("dua"))
	require.NoError(t, err)

	message, err := fixture.repo.Delete(t.Context(), fixture.scope, first.ID)
	require.NoError(t, err)
	require.Len(t, message.Attachments, 1)
	require.Equal(t, second.ID, message.Attachments[0].ID)

	stored, err := fixture.repo.GetMessage(t.Context(), fixture.scope, first.MessageID)
	require.NoError(t, err)
	require.Len(t, stored.Attachments, 1, "the file that was not deleted stays on the message")
	require.Equal(t, second.ID, stored.Attachments[0].ID)

	// The sweep retries after a partial failure, so deleting a row that is already gone
	// converges: no message to publish, and no error to report as work not done.
	again, err := fixture.repo.Delete(t.Context(), fixture.scope, first.ID)
	require.NoError(t, err)
	require.Equal(t, uuid.Nil, again.ID)
}

// attachApp serves the chat routes over the fixture's identity, which is what a multipart
// upload needs: the type check runs at the HTTP boundary, and so does the part header a
// client attaches to its file.
func attachApp(t *testing.T, fixture *chatFixture, service *chatservice.Service) *fiber.App {
	t.Helper()

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	group := app.Group("/api", func(c *fiber.Ctx) error {
		middleware.SetIdentity(c, fixture.userID, "owner@example.com")
		middleware.SetScope(c, database.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace})
		return c.Next()
	})
	chathandler.Routes(group, chathandler.New(service, zap.NewNop()))
	return app
}

// uploadClaimingType posts one file whose part carries a Content-Type of its own, which is
// the claim a client is free to make.
func uploadClaimingType(t *testing.T, app *fiber.App, conversationID uuid.UUID, filename, claimedType string, content []byte) (int, chatdomain.AttachmentPayload) {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="file"; filename="`+filename+`"`)
	header.Set("Content-Type", claimedType)
	part, err := writer.CreatePart(header)
	require.NoError(t, err)
	_, err = part.Write(content)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(http.MethodPost,
		"/api/conversations/"+conversationID.String()+"/attachments", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var envelope struct {
		Data chatdomain.AttachmentPayload `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &envelope))
	return resp.StatusCode, envelope.Data
}

// TestTheTypeAClientClaimsIsNotTheTypeStored: the download endpoint serves the type on the
// row, so a part header must not reach it. Both uploads here pass the allowlist either way;
// what they prove is which half of the request decides, and the client is told the same
// thing that was written.
func TestTheTypeAClientClaimsIsNotTheTypeStored(t *testing.T) {
	fixture := newChatFixture(t, appDatabase(t))
	service := newAttachService(fixture)
	app := attachApp(t, fixture, service)
	conversation := fixture.directConversation(t)

	status, stored := uploadClaimingType(t, app, conversation.ID, "catatan.txt", "image/png", []byte("halo"))
	require.Equal(t, fiber.StatusCreated, status)
	require.Equal(t, "text/plain", stored.ContentType, "the extension decides, not the claim")

	status, stored = uploadClaimingType(t, app, conversation.ID, "foto.png", "text/plain", pngOf("foto"))
	require.Equal(t, fiber.StatusCreated, status)
	require.Equal(t, "image/png", stored.ContentType)

	row, err := service.Attachment(t.Context(), fixture.scope, uuid.MustParse(stored.ID))
	require.NoError(t, err)
	require.Equal(t, "image/png", row.ContentType, "what a download later serves is what was written")
}
