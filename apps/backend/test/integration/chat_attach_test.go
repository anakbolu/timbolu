package integration

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	chatservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/service"
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
		MessageID: opened.Message.ID, Filename: "a.png", ContentType: "image/png",
	}, []byte("png-bytes"))
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
			MessageID: sent.Message.ID, Filename: name, ContentType: "application/pdf",
		}, []byte(name))
		require.NoError(t, err)
	}

	message, err := fixture.repo.GetMessage(t.Context(), fixture.scope, sent.Message.ID)
	require.NoError(t, err)
	require.Len(t, message.Attachments, 2, "messages.attachments must list both files")

	events := messageEventsFor(t, fixture, sent.Message.ID)
	require.NotEmpty(t, events)
	require.Len(t, events[len(events)-1].Attachments, 2)
}
