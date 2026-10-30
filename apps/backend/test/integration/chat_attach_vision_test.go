package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	chatdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/chat/domain"
	chatservice "github.com/anakbolu/timbolu/apps/backend/internal/modules/chat/service"
	llmdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
	llmrepo "github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/repository"
	llmservice "github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/service"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/llm"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/llm/pricing"
	"github.com/anakbolu/timbolu/packages/platform/crypto"
)

// This file covers issue #237 end to end: a picture meets a model that cannot read
// it, and the person still gets an answer. Everything between the reply and the
// model is real — the gateway, the adapter, the provider row in Postgres — and only
// the model itself is a stub, because that is what a deployment points at.
//
// The case here is the one no configuration can cover: the provider answers 400 to
// a prompt carrying a picture. That 400 is a capability the chain lacks, not a
// malformed request, and reading it as the latter is what makes every retry carry
// the same picture and the reply end as a failure. The configured case — a model the
// deployment already knows is text-only — is in chat_attach_textonly_test.go.

// textOnlyModel is a provider-compatible endpoint that records every prompt it is
// asked and streams one fixed sentence. With refuseImages it answers a prompt that
// carries a picture the way a text-only model behind an OpenAI-compatible route does.
type textOnlyModel struct {
	server *httptest.Server

	mu      sync.Mutex
	prompts []string

	answer       string
	refuseImages bool
}

func newTextOnlyModel(t *testing.T, answer string, refuseImages bool) *textOnlyModel {
	t.Helper()

	model := &textOnlyModel{answer: answer, refuseImages: refuseImages}
	model.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read the model request: %v", err)
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			// Memory embeds through the same key; a deployment without an
			// embedding route degrades instead of failing.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"no such route"}}`))
			return
		}

		model.mu.Lock()
		model.prompts = append(model.prompts, string(body))
		model.mu.Unlock()

		if model.refuseImages && strings.Contains(string(body), "image_url") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"This model does not support image input","type":"invalid_request_error"}}`))
			return
		}

		var request struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode the model request: %v", err)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		chunk := func(payload map[string]any) {
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Errorf("encode a chunk: %v", err)
				return
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
		}
		chunk(map[string]any{
			"id": "chatcmpl-textonly", "object": "chat.completion.chunk", "model": request.Model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": model.answer}}},
		})
		chunk(map[string]any{
			"id": "chatcmpl-textonly", "object": "chat.completion.chunk", "model": request.Model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 12, "completion_tokens": 4, "total_tokens": 16},
		})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(model.server.Close)

	return model
}

// recorded returns every prompt the model was asked, oldest first.
func (m *textOnlyModel) recorded() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]string{}, m.prompts...)
}

// withPictures counts the prompts that carried a picture, which is what a
// deployment pays for when the capability is guessed wrong.
func (m *textOnlyModel) withPictures() int {
	count := 0
	for _, prompt := range m.recorded() {
		if strings.Contains(prompt, "image_url") {
			count++
		}
	}

	return count
}

// visionRig is one workspace whose chat surface talks to a real gateway, a real
// adapter, and a real provider row: the only stub is the model endpoint itself.
type visionRig struct {
	fixture      *chatFixture
	objects      *attachObjects
	service      *chatservice.Service
	gateway      *llmservice.Service
	model        *textOnlyModel
	conversation chatdomain.Conversation
}

// newVisionRigWith wires the two halves of the feature — the chat surface and the
// model gateway — inside one workspace, with the given adapter factory.
func newVisionRigWith(t *testing.T, model *textOnlyModel, factory llm.Factory) *visionRig {
	t.Helper()

	fixture := newChatFixture(t, appDatabase(t))
	objects := &attachObjects{}
	service := chatservice.New(chatservice.Deps{
		Conversations: fixture.repo,
		Messages:      fixture.repo,
		Attachments:   fixture.repo,
		Events:        fixture.repo,
		Storage:       objects,
	})

	box, err := crypto.NewFromString(testEncryptionKey)
	require.NoError(t, err)

	repo := llmrepo.New(fixture.pool)
	gateway := llmservice.New(llmservice.Deps{
		Store:    repo,
		Agents:   repo.NewAgentStore(box),
		Factory:  factory,
		Box:      box,
		Recorder: repo,
		Reader:   repo,
		Costs:    pricing.New(pricing.Default(), pricing.DefaultFallback),
	})

	_, err = gateway.Upsert(t.Context(),
		llmdomain.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace},
		llmdomain.UpsertRequest{ProviderConfig: llmdomain.ProviderConfig{
			Name:          "lokal",
			Adapter:       llmdomain.AdapterOpenAI,
			BaseURL:       model.server.URL,
			Model:         "echo-text",
			APIKey:        "sk-lokal",
			Priority:      10,
			ContextTokens: 8000,
			Enabled:       true,
		}})
	require.NoError(t, err)

	return &visionRig{
		fixture:      fixture,
		objects:      objects,
		service:      service,
		gateway:      gateway,
		model:        model,
		conversation: fixture.directConversation(t),
	}
}

// pngUpload is a picture, which is what the whole question is about.
func pngUpload() []byte { return append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...) }

// askWithAPicture sends one message carrying a picture and returns the answer and
// the notices the person saw.
func (r *visionRig) askWithAPicture(t *testing.T) (string, []string) {
	t.Helper()

	sent, err := r.service.Send(t.Context(), r.fixture.scope, chatdomain.SendRequest{
		ConversationID: r.conversation.ID, Text: "Stok di foto ini tinggal berapa?",
	})
	require.NoError(t, err)

	_, err = r.service.Attach(t.Context(), r.fixture.scope, chatdomain.AttachRequest{
		MessageID: sent.Message.ID, Filename: "foto.png",
	}, pngUpload())
	require.NoError(t, err)

	// The message as Postgres holds it: the row points at the object, and the
	// responder is handed only the row.
	message, err := r.fixture.repo.GetMessage(t.Context(), r.fixture.scope, sent.Message.ID)
	require.NoError(t, err)
	require.Len(t, message.Attachments, 1)

	responder := chatservice.NewModelResponder(chatservice.ResponderDeps{
		Gateway: r.gateway, Storage: r.objects, Logger: zap.NewNop(),
	})

	chunks, err := responder.Reply(t.Context(), r.fixture.scope, chatdomain.ReplyRequest{
		Conversation: r.conversation,
		Agent:        chatdomain.AgentRef{ID: r.fixture.agentID, Name: "Oren"},
		Message:      message,
		History:      []chatdomain.Message{message},
		RepairBudget: 1,
	})
	require.NoError(t, err)

	var (
		answer  strings.Builder
		notices []string
	)
	for chunk := range chunks {
		require.NotEqual(t, chatdomain.ChunkError, chunk.Kind,
			"the reply must survive a model that cannot read the picture: %s", chunk.Err)
		switch chunk.Kind {
		case chatdomain.ChunkText:
			answer.WriteString(chunk.Text)
		case chatdomain.ChunkNotice:
			notices = append(notices, chunk.Text)
		}
	}

	return answer.String(), notices
}

// TestAProviderThatRefusesThePictureStillAnswers is issue #237 through the real
// adapter: a model the deployment did not list answers 400 to a prompt carrying a
// picture. The reply must be tried once more without the picture and finish, rather
// than failing every attempt with the same picture on it.
func TestAProviderThatRefusesThePictureStillAnswers(t *testing.T) {
	model := newTextOnlyModel(t, "Fotonya tidak bisa kubaca, tapi stoknya 7 pcs.", true)
	rig := newVisionRigWith(t, model, llm.Factory{})

	answer, notices := rig.askWithAPicture(t)

	require.Contains(t, answer, "7 pcs")
	require.Contains(t, strings.Join(notices, " "), "tidak bisa membaca gambar",
		"the person is told the picture was not read")

	prompts := model.recorded()
	require.Len(t, prompts, 2, "one refused attempt, one without the picture")
	require.Contains(t, prompts[0], "image_url", "the first attempt is what the provider refused")
	require.NotContains(t, prompts[1], "image_url")
	require.Contains(t, prompts[1], "tidak bisa dibaca")
}
