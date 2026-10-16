package integration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	chathandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/handler"
	chatservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/service"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/packages/platform/database"
)

// flakyObjects is an object store whose Put can be made to fail.
type flakyObjects struct {
	attachObjects
	failPut bool
}

func (s *flakyObjects) Put(ctx context.Context, key, contentType string, content []byte) (chatdomain.StoredObject, error) {
	if s.failPut {
		return chatdomain.StoredObject{}, errors.New("storage is down")
	}
	return s.attachObjects.Put(ctx, key, contentType, content)
}

type hardeningRig struct {
	fixture *chatFixture
	service *chatservice.Service
	objects *flakyObjects
	app     *fiber.App
}

func newHardeningRig(t *testing.T) *hardeningRig {
	t.Helper()
	fixture := newChatFixture(t, appDatabase(t))
	objects := &flakyObjects{}
	service := chatservice.New(chatservice.Deps{
		Conversations: fixture.repo,
		Messages:      fixture.repo,
		Attachments:   fixture.repo,
		Events:        fixture.repo,
		Storage:       objects,
	})
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	group := app.Group("/api", func(c *fiber.Ctx) error {
		middleware.SetIdentity(c, fixture.userID, "owner@example.com")
		middleware.SetScope(c, database.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace})
		return c.Next()
	})
	chathandler.Routes(group, chathandler.New(service, zap.NewNop()))
	return &hardeningRig{fixture: fixture, service: service, objects: objects, app: app}
}

func (r *hardeningRig) upload(t *testing.T, conversationID uuid.UUID, messageID, filename, contentType string, content []byte) (int, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if messageID != "" {
		require.NoError(t, writer.WriteField("message_id", messageID))
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="file"; filename="`+filename+`"`)
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	require.NoError(t, err)
	_, _ = part.Write(content)
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(http.MethodPost, "/api/conversations/"+conversationID.String()+"/attachments", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := r.app.Test(req, -1)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func (r *hardeningRig) messageCount(t *testing.T, conversationID uuid.UUID) int {
	t.Helper()
	page, err := r.service.History(t.Context(), r.fixture.scope, conversationID, chatdomain.Cursor{})
	require.NoError(t, err)
	return len(page.Messages)
}

var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 32)...)
var exeBytes = append([]byte("MZ\x90\x00\x03\x00\x00\x00"), bytes.Repeat([]byte{0}, 64)...)

// Point 1: a failed store must not leave an empty message or publish one.
func TestHardeningFailedUploadLeavesNoMessage(t *testing.T) {
	rig := newHardeningRig(t)
	conversation := rig.fixture.directConversation(t)
	rig.objects.failPut = true

	status, body := rig.upload(t, conversation.ID, "", "a.png", "image/png", pngBytes)
	require.GreaterOrEqual(t, status, 400, body)
	require.Equal(t, 0, rig.messageCount(t, conversation.ID), "no empty carrier message may remain")

	events, err := rig.fixture.repo.SinceEvents(t.Context(), rig.fixture.workspace, 0, 500)
	require.NoError(t, err)
	for _, event := range events {
		require.NotEqual(t, chatdomain.EventMessageNew, event.Type, "no event for a message that failed")
	}
}

// Point 2: executables are refused by extension and by sniffed content.
func TestHardeningFileTypeAllowlist(t *testing.T) {
	rig := newHardeningRig(t)
	conversation := rig.fixture.directConversation(t)

	status, body := rig.upload(t, conversation.ID, "", "x.exe", "application/x-msdownload", exeBytes)
	require.True(t, status >= 400 && status < 500, "exe: %d %s", status, body)

	status, body = rig.upload(t, conversation.ID, "", "foto.png", "image/png", exeBytes)
	require.True(t, status >= 400 && status < 500, "exe disguised as png: %d %s", status, body)

	require.Equal(t, 0, rig.messageCount(t, conversation.ID), "a refused upload opens no message")

	status, body = rig.upload(t, conversation.ID, "", "foto.png", "image/png", pngBytes)
	require.Equal(t, fiber.StatusCreated, status, body)
	status, body = rig.upload(t, conversation.ID, "", "catatan.txt", "text/plain", []byte("halo"))
	require.Equal(t, fiber.StatusCreated, status, body)
}

// Point 3: retention removes the file from the message's read model and says so.
func TestHardeningPruneCleansTheMessage(t *testing.T) {
	rig := newHardeningRig(t)
	conversation := rig.fixture.directConversation(t)

	sent, err := rig.service.Send(t.Context(), rig.fixture.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID, Text: "lampiran berikut",
	})
	require.NoError(t, err)
	status, body := rig.upload(t, conversation.ID, sent.Message.ID.String(), "catatan.txt", "text/plain", []byte("halo"))
	require.Equal(t, fiber.StatusCreated, status, body)

	pruned, err := rig.service.PruneAttachments(t.Context(), rig.fixture.scope, time.Now().Add(time.Hour), 100)
	require.NoError(t, err)
	require.Equal(t, 1, pruned.Count)

	message, err := rig.fixture.repo.GetMessage(t.Context(), rig.fixture.scope, sent.Message.ID)
	require.NoError(t, err)
	require.Empty(t, message.Attachments, "messages.attachments must drop a pruned file")

	events := messageEventsFor(t, rig.fixture, sent.Message.ID)
	require.NotEmpty(t, events)
	require.Empty(t, events[len(events)-1].Attachments, "the stream must learn the file is gone")
}
