package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/apps/backend/internal/container"
	chatdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/chat/domain"
	taskdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/task/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/temporal"
	"github.com/anakbolu/timbolu/packages/platform/config"
)

// This file covers the seam issue #235 was about: the task runtime is assembled
// before the chat module, and the file reader the activities use lives in the
// chat module. A reader captured at assembly time is therefore nil in every
// process — API and worker alike — and every attachment is named instead of
// read.
//
// A fake reader cannot catch that, which is why this test builds the real
// container and registers the activities the way cmd/agent-worker does: the
// assembly order is the thing under test.

// recordingModel is a provider-compatible endpoint that records every prompt it
// is asked and answers with one fixed sentence. The task reaches it through the
// real gateway, the real adapter, and the real provider configuration.
type recordingModel struct {
	server *httptest.Server

	mu      sync.Mutex
	prompts []string
}

func newRecordingModel(t *testing.T, answer string) *recordingModel {
	t.Helper()

	model := &recordingModel{}
	model.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read the model request: %v", err)
			return
		}

		if r.URL.Path != "/v1/chat/completions" {
			// The embeddings route is not served. Memory is a degraded read on
			// purpose, so the task answers without it rather than not at all.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"no such route"}}`))
			return
		}

		model.mu.Lock()
		model.prompts = append(model.prompts, string(body))
		model.mu.Unlock()

		var request struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode the model request: %v", err)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "chatcmpl-attach",
			"object": "chat.completion",
			"model":  request.Model,
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": answer},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 4, "total_tokens": 16},
		})
	}))
	t.Cleanup(model.server.Close)

	return model
}

func (m *recordingModel) recorded() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]string{}, m.prompts...)
}

// TestATasksAttachmentContentReachesTheModel is issue #235's gate: a file
// uploaded through the chat module is read by the worker's own activity set and
// its content reaches the model.
func TestATasksAttachmentContentReachesTheModel(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := appDatabase(t)
	temporalHost := startTemporal(t, ctx)
	fixture := newChatFixture(t, dsn)

	model := newRecordingModel(t, "Stok kaos merah tinggal 7 pcs.")

	cfg := &config.Config{
		Application: config.AppConfig{Environment: "test", FrontendURL: "https://app.example.com"},
		Http:        config.HttpConfig{Address: ":0", ApiPrefix: "/api"},
		Database:    config.DatabaseConfig{URL: dsn, MaxOpenConns: 5, MaxIdleConns: 1},
		JWT:         config.JWTConfig{Secret: "integration-test-secret", ExpireHours: 1},
		Auth: config.AuthConfig{
			AccessTokenMinutes: 15, RefreshHours: 24, VerificationHours: 24,
			ResetMinutes: 60, StateMinutes: 10, LoginThrottleCapacity: 100, LoginThrottleRefill: 100,
		},
		Mail:    config.MailConfig{Driver: "log"},
		Logging: config.LoggingConfig{Level: "error", Format: "json"},
		Temporal: config.TemporalConfig{
			HostPort:       temporalHost,
			Namespace:      "default",
			TaskQueueAgent: "bolu-agent-attach-test",
		},
		// The attachments are written where the container can read them back,
		// which is what makes the upload and the read the same object storage.
		Storage: config.StorageConfig{Driver: "local", LocalRoot: t.TempDir()},
		Llm: config.LlmConfig{
			SecretEncryptionKey: testEncryptionKey,
			Default: config.DefaultProviderConfig{
				Adapter:       "openai",
				BaseURL:       model.server.URL,
				Model:         "echo-vision",
				APIKey:        "sk-integration",
				ContextTokens: 8000,
			},
		},
	}

	app, err := container.New(ctx, cfg, container.WithoutHTTP(), container.WithLogger(zap.NewNop()))
	require.NoError(t, err)
	t.Cleanup(app.Close)

	require.NotNil(t, app.Tasks, "the worker's activity set must be built")
	require.NotNil(t, app.Services.Chat, "the chat module owns the files a task reads")

	// The worker, registered the way cmd/agent-worker registers it.
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- container.RunWorker(workerCtx, app, container.WorkerConfig{
			Name:          "integration-attach-worker",
			TaskQueue:     func(c *temporal.Client) string { return c.TaskQueueAgent() },
			Register:      container.RegisterAgentTasks,
			NeedsDatabase: true,
		})
	}()
	t.Cleanup(func() {
		stopWorker()
		select {
		case err := <-workerDone:
			require.NoError(t, err)
		case <-time.After(30 * time.Second):
			t.Error("the worker did not stop")
		}
	})

	conversation := fixture.directConversation(t)

	// The file is uploaded the way the product uploads one: through the chat
	// service, into the object storage the container configured.
	carrier, err := app.Services.Chat.Send(ctx, fixture.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "Tolong cek lampiran ini",
	})
	require.NoError(t, err)

	_, err = app.Services.Chat.Attach(ctx, fixture.scope, chatdomain.AttachRequest{
		MessageID: carrier.Message.ID,
		Filename:  "stok.txt",
	}, []byte("Stok kaos merah: 7 pcs"))
	require.NoError(t, err)

	// The question opens the task, which the worker runs.
	sent, err := app.Services.Chat.Send(ctx, fixture.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "Berapa stok kaos merah?",
		Reply:          true,
	})
	require.NoError(t, err)

	placeholder, err := fixture.repo.GetMessage(ctx, fixture.scope, sent.ReplyMessageIDs[0])
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, placeholder.TaskID, "the message must be handed to the durable runtime")

	taskScope := taskdomain.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace}
	require.NoError(t, waitFor(t, 2*time.Minute, func() bool {
		task, err := app.Services.Task.Get(t.Context(), taskScope, placeholder.TaskID)
		if err != nil {
			return false
		}
		return task.Status == taskdomain.StatusSucceeded || task.Status == taskdomain.StatusFailed
	}), "the task must finish")

	task, err := app.Services.Task.Get(ctx, taskScope, placeholder.TaskID)
	require.NoError(t, err)
	require.Equal(t, taskdomain.StatusSucceeded, task.Status, "the task's own summary: %s", task.Summary)

	prompts := model.recorded()
	require.NotEmpty(t, prompts, "the task must have called the model")

	asked := strings.Join(prompts, "\n")
	require.Contains(t, asked, "stok.txt", "the prompt names the file")
	require.Contains(t, asked, "Stok kaos merah: 7 pcs", "the file's content is what the model reads")
}
