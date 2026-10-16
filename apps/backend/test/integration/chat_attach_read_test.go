package integration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	chatservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/service"
	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/attachtext"
)

// unusedGateway answers the gateway questions the reply path never asks. It
// refuses rather than pretending to work, so a test that reaches one of them
// fails loudly instead of passing on a stub.
type unusedGateway struct{}

func (unusedGateway) Embed(context.Context, llmdomain.Scope, llmdomain.EmbedRequest) (llmdomain.EmbedResponse, error) {
	return llmdomain.EmbedResponse{}, errors.New("not used")
}
func (unusedGateway) Chat(context.Context, llmdomain.Scope, llmdomain.ChatRequest) (llmdomain.ChatResponse, error) {
	return llmdomain.ChatResponse{}, errors.New("not used")
}
func (unusedGateway) Providers(context.Context, llmdomain.Scope) ([]llmdomain.Redacted, error) {
	return nil, errors.New("not used")
}
func (unusedGateway) Upsert(context.Context, llmdomain.Scope, llmdomain.UpsertRequest) (llmdomain.Redacted, error) {
	return llmdomain.Redacted{}, errors.New("not used")
}
func (unusedGateway) Delete(context.Context, llmdomain.Scope, uuid.UUID) error {
	return errors.New("not used")
}
func (unusedGateway) Test(context.Context, llmdomain.Scope, uuid.UUID) (llmdomain.Capabilities, error) {
	return llmdomain.Capabilities{}, errors.New("not used")
}
func (unusedGateway) SetAgentModel(context.Context, llmdomain.Scope, llmdomain.AgentOverride) error {
	return errors.New("not used")
}
func (unusedGateway) AgentModel(context.Context, llmdomain.Scope) (llmdomain.AgentOverride, error) {
	return llmdomain.AgentOverride{}, errors.New("not used")
}
func (unusedGateway) Usage(context.Context, llmdomain.Scope, int) ([]llmdomain.UsageDaily, error) {
	return nil, errors.New("not used")
}

// scriptedStreamGateway answers one streamed round with a fixed sentence and
// records what the model was asked.
type scriptedStreamGateway struct {
	unusedGateway

	answer string

	mu       sync.Mutex
	requests []llmdomain.ChatRequest
}

func (g *scriptedStreamGateway) Stream(_ context.Context, _ llmdomain.Scope, req llmdomain.ChatRequest) (<-chan llmdomain.StreamEvent, error) {
	g.mu.Lock()
	g.requests = append(g.requests, req)
	g.mu.Unlock()

	events := make(chan llmdomain.StreamEvent, 2)
	events <- llmdomain.StreamEvent{Type: llmdomain.EventText, Text: g.answer}
	events <- llmdomain.StreamEvent{Type: llmdomain.EventDone, FinishReason: llmdomain.FinishStop}
	close(events)
	return events, nil
}

func (g *scriptedStreamGateway) recorded() []llmdomain.ChatRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]llmdomain.ChatRequest{}, g.requests...)
}

// TestAttachmentContentReachesTheModel is issue #187 end to end: a file is
// uploaded through the service, its row and its bytes land in Postgres and object
// storage, and the prompt the model receives carries the file's content rather
// than only its name.
func TestAttachmentContentReachesTheModel(t *testing.T) {
	fixture := newChatFixture(t, appDatabase(t))
	storage := &attachObjects{}
	service := chatservice.New(chatservice.Deps{
		Conversations: fixture.repo,
		Messages:      fixture.repo,
		Attachments:   fixture.repo,
		Events:        fixture.repo,
		Storage:       storage,
	})

	conversation := fixture.directConversation(t)
	sent, err := service.Send(t.Context(), fixture.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID, Text: "Tolong cek lampiran ini",
	})
	require.NoError(t, err)

	_, err = service.Attach(t.Context(), fixture.scope, chatdomain.AttachRequest{
		MessageID: sent.Message.ID, Filename: "stok.txt",
	}, []byte("Stok kaos merah: 7 pcs"))
	require.NoError(t, err)

	// The message as Postgres holds it: the row points at the object, and the
	// responder is handed only the row.
	message, err := fixture.repo.GetMessage(t.Context(), fixture.scope, sent.Message.ID)
	require.NoError(t, err)
	require.Len(t, message.Attachments, 1)
	require.Equal(t, "stok.txt", message.Attachments[0].Filename)

	gateway := &scriptedStreamGateway{answer: "Stok kaos merah tinggal 7 pcs."}
	responder := chatservice.NewModelResponder(chatservice.ResponderDeps{
		Gateway: gateway,
		Storage: storage,
		Logger:  zap.NewNop(),
	})

	chunks, err := responder.Reply(t.Context(), fixture.scope, chatdomain.ReplyRequest{
		Conversation: conversation,
		Agent:        chatdomain.AgentRef{ID: fixture.agentID, Name: "Oren"},
		Message:      message,
		History:      []chatdomain.Message{message},
		RepairBudget: 1,
	})
	require.NoError(t, err)

	var answer strings.Builder
	for chunk := range chunks {
		require.NotEqual(t, chatdomain.ChunkError, chunk.Kind, "the answer must not fail: %s", chunk.Err)
		answer.WriteString(chunk.Text)
	}
	require.Contains(t, answer.String(), "7 pcs")

	requests := gateway.recorded()
	require.Len(t, requests, 1)
	require.NotEmpty(t, requests[0].Messages)

	last := requests[0].Messages[len(requests[0].Messages)-1]
	require.Contains(t, last.Text, "Tolong cek lampiran ini")
	require.Contains(t, last.Text, "[Lampiran: stok.txt (text/plain, 22 B)]",
		"the prompt names the file the way a model can cite it")
	require.Contains(t, last.Text, "Stok kaos merah: 7 pcs",
		"the file's content is what the model reads")
}

// TestAnOversizedAttachmentIsNamedNotRead is the documented limit, end to end: a
// file above the configured bound is described in the prompt and its object is
// never fetched.
func TestAnOversizedAttachmentIsNamedNotRead(t *testing.T) {
	fixture := newChatFixture(t, appDatabase(t))
	storage := &attachObjects{}
	service := chatservice.New(chatservice.Deps{
		Conversations: fixture.repo,
		Messages:      fixture.repo,
		Attachments:   fixture.repo,
		Events:        fixture.repo,
		Storage:       storage,
	})

	conversation := fixture.directConversation(t)
	sent, err := service.Send(t.Context(), fixture.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID, Text: "cek ini",
	})
	require.NoError(t, err)

	_, err = service.Attach(t.Context(), fixture.scope, chatdomain.AttachRequest{
		MessageID: sent.Message.ID, Filename: "besar.txt",
	}, []byte(strings.Repeat("a", 2048)))
	require.NoError(t, err)

	message, err := fixture.repo.GetMessage(t.Context(), fixture.scope, sent.Message.ID)
	require.NoError(t, err)

	gateway := &scriptedStreamGateway{answer: "berkasnya terlalu besar"}
	responder := chatservice.NewModelResponder(chatservice.ResponderDeps{
		Gateway: gateway,
		Storage: storage,
		// Smaller than the upload, so the file is named rather than read.
		Limits: attachtext.Limits{MaxFileBytes: 1024},
		Logger: zap.NewNop(),
	})

	chunks, err := responder.Reply(t.Context(), fixture.scope, chatdomain.ReplyRequest{
		Conversation: conversation,
		Agent:        chatdomain.AgentRef{ID: fixture.agentID, Name: "Oren"},
		Message:      message,
		History:      []chatdomain.Message{message},
		RepairBudget: 1,
	})
	require.NoError(t, err)
	for range chunks {
	}

	requests := gateway.recorded()
	require.Len(t, requests, 1)
	last := requests[0].Messages[len(requests[0].Messages)-1]
	require.Contains(t, last.Text, "besar.txt")
	require.NotContains(t, last.Text, strings.Repeat("a", 100),
		"the bytes of an oversized file never reach the prompt")
}
