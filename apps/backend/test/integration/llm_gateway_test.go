package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	authrepo "github.com/anakbolu/timbolu/apps/backend/internal/modules/auth/repository"
	llmdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
	llmrepo "github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/repository"
	llmservice "github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/service"
	workspacedomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/workspace/domain"
	workspacerepo "github.com/anakbolu/timbolu/apps/backend/internal/modules/workspace/repository"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/llm"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/llm/pricing"
	"github.com/anakbolu/timbolu/packages/platform/crypto"
	"github.com/anakbolu/timbolu/packages/platform/database"
)

// gatewayFixture is one onboarded workspace whose gateway uses the real
// adapters, the real encryption, and the real repository — only the provider
// endpoints are stubs, so the fallback and the recording are exercised end to
// end.
type gatewayFixture struct {
	service   *llmservice.Service
	repo      *llmrepo.Repository
	pool      *database.Pool
	workspace uuid.UUID
	userID    uuid.UUID
}

func newGatewayFixture(t *testing.T, dsn string) *gatewayFixture {
	t.Helper()

	ctx := t.Context()
	pool := poolFor(t, dsn)

	box, err := crypto.NewFromString(testEncryptionKey)
	require.NoError(t, err)

	workspaces := workspacerepo.New(pool)
	repo := llmrepo.New(pool)

	user, err := authrepo.New(pool.PgxPool()).CreateUser(ctx, uuid.NewString()+"@example.com", "$argon2id$hash")
	require.NoError(t, err)

	onboarded, err := workspaces.Onboard(ctx, user.ID, workspacedomain.OnboardRequest{
		Name:          "Toko Sinar",
		BusinessField: "Retail",
		Timezone:      "Asia/Jakarta",
	})
	require.NoError(t, err)

	return &gatewayFixture{
		service: llmservice.New(llmservice.Deps{
			Store:    repo,
			Agents:   repo.NewAgentStore(box),
			Factory:  llm.Factory{},
			Box:      box,
			Recorder: repo,
			Reader:   repo,
			Costs:    pricing.New(pricing.Default(), pricing.DefaultFallback),
		}),
		repo:      repo,
		pool:      pool,
		workspace: onboarded.Workspace.ID,
		userID:    user.ID,
	}
}

func (f *gatewayFixture) scope() llmdomain.Scope {
	return llmdomain.Scope{UserID: f.userID, WorkspaceID: f.workspace}
}

// addProvider stores one configuration pointing at a stub server.
func (f *gatewayFixture) addProvider(t *testing.T, name, model, baseURL string, mutate ...func(*llmdomain.ProviderConfig)) llmdomain.Redacted {
	t.Helper()

	cfg := llmdomain.ProviderConfig{
		Name:          name,
		Adapter:       llmdomain.AdapterOpenAI,
		BaseURL:       baseURL,
		Model:         model,
		APIKey:        "sk-" + name,
		Priority:      10,
		ContextTokens: 8000,
		Enabled:       true,
	}
	for _, change := range mutate {
		change(&cfg)
	}

	created, err := f.service.Upsert(t.Context(), f.scope(), llmdomain.UpsertRequest{ProviderConfig: cfg})
	require.NoError(t, err)
	return created
}

// ledgerRows returns the recorded calls, oldest first.
func (f *gatewayFixture) ledgerRows(t *testing.T) []llmdomain.UsageEntry {
	t.Helper()

	rows := []llmdomain.UsageEntry{}
	require.NoError(t, f.pool.InScopeRead(t.Context(), database.Scope{WorkspaceID: f.workspace}, func(tx pgx.Tx) error {
		cursor, err := tx.Query(t.Context(),
			`SELECT provider, model, purpose, input_tokens, output_tokens, cost_micros
			 FROM usage_ledger WHERE workspace_id = $1 ORDER BY id`, f.workspace)
		if err != nil {
			return err
		}
		defer cursor.Close()

		for cursor.Next() {
			var entry llmdomain.UsageEntry
			if err := cursor.Scan(&entry.Provider, &entry.Model, &entry.Purpose,
				&entry.Usage.InputTokens, &entry.Usage.OutputTokens, &entry.CostMicros); err != nil {
				return err
			}
			rows = append(rows, entry)
		}
		return cursor.Err()
	}))

	return rows
}

// completionServer answers one OpenAI-compatible completion with fixed token
// counts, which is what makes the recorded cost predictable.
func completionServer(t *testing.T, text string, input, output int) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		require.NotEmpty(t, r.Header.Get("Authorization"), "the adapter must send the stored key")

		// A real provider echoes the model it served, which is what the adapter
		// reports back and the ledger records.
		var request struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"model":   request.Model,
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": input, "completion_tokens": output, "total_tokens": input + output},
		})
	}))
	t.Cleanup(server.Close)

	return server
}

// brokenServer fails every call the way an outage does, which is the failure the
// fallback exists for.
func brokenServer(t *testing.T, status int) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream is down","type":"server_error"}}`))
	}))
	t.Cleanup(server.Close)

	return server
}

// TestFallbackMovesToTheNextProvider is the E4.6 gate: a provider that is down
// is skipped, the next one answers, and the ledger names the one that did.
func TestFallbackMovesToTheNextProvider(t *testing.T) {
	fixture := newGatewayFixture(t, appDatabase(t))
	ctx := t.Context()

	down := brokenServer(t, http.StatusServiceUnavailable)
	healthy := completionServer(t, "siap", 42, 7)

	fixture.addProvider(t, "Mati", "gpt-4o-mini", down.URL, func(cfg *llmdomain.ProviderConfig) {
		cfg.Priority = 10
	})
	fixture.addProvider(t, "Hidup", "gpt-4o", healthy.URL, func(cfg *llmdomain.ProviderConfig) {
		cfg.Priority = 20
	})

	response, err := fixture.service.Chat(ctx, fixture.scope(), llmdomain.ChatRequest{
		Messages: []llmdomain.Message{{Role: llmdomain.RoleUser, Text: "Catat pesanan baru"}},
		Metadata: llmdomain.RequestMetadata{WorkspaceID: fixture.workspace, Purpose: llmdomain.PurposeAgent},
	})
	require.NoError(t, err)
	require.Equal(t, "siap", response.Text)
	require.Equal(t, "gpt-4o", response.Model, "the second provider answered")

	rows := fixture.ledgerRows(t)
	require.Len(t, rows, 1, "only the provider that answered is recorded")
	require.Equal(t, "gpt-4o", rows[0].Model)
	require.Equal(t, llmdomain.AdapterOpenAI, rows[0].Provider)
	require.Equal(t, 42, rows[0].Usage.InputTokens)
	require.Equal(t, 7, rows[0].Usage.OutputTokens)
	require.Positive(t, rows[0].CostMicros, "a known model must not cost zero")
}

// TestCachedTokensAreRecorded: a provider that reports cached tokens must have
// them in the ledger, because they are billed differently from fresh input.
func TestCachedTokensAreRecorded(t *testing.T) {
	fixture := newGatewayFixture(t, appDatabase(t))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-cache",
			"object":  "chat.completion",
			"model":   "gpt-4o-mini",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "siap"}, "finish_reason": "stop"}},
			"usage": map[string]any{
				"prompt_tokens":         100,
				"completion_tokens":     10,
				"total_tokens":          110,
				"prompt_tokens_details": map[string]any{"cached_tokens": 64},
			},
		})
	}))
	t.Cleanup(server.Close)

	fixture.addProvider(t, "Cache", "gpt-4o-mini", server.URL)

	_, err := fixture.service.Chat(t.Context(), fixture.scope(), llmdomain.ChatRequest{
		Messages: []llmdomain.Message{{Role: llmdomain.RoleUser, Text: "Catat pesanan"}},
		Metadata: llmdomain.RequestMetadata{WorkspaceID: fixture.workspace},
	})
	require.NoError(t, err)

	var cached int
	require.NoError(t, fixture.pool.InScopeRead(t.Context(), database.Scope{WorkspaceID: fixture.workspace}, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(),
			"SELECT cache_read_tokens FROM usage_ledger WHERE workspace_id = $1", fixture.workspace).Scan(&cached)
	}))
	require.Equal(t, 64, cached, "the cached tokens must survive into the ledger")
}

// TestFallbackRecordsOneRowPerCall keeps the ledger honest when the first
// provider fails: the failed attempt is not a billable call.
func TestFallbackRecordsOneRowPerCall(t *testing.T) {
	fixture := newGatewayFixture(t, appDatabase(t))
	ctx := t.Context()

	down := brokenServer(t, http.StatusTooManyRequests)
	healthy := completionServer(t, "ok", 10, 5)

	fixture.addProvider(t, "Kena limit", "gpt-4o-mini", down.URL)
	fixture.addProvider(t, "Cadangan", "gpt-4o-mini", healthy.URL, func(cfg *llmdomain.ProviderConfig) {
		cfg.Priority = 20
	})

	for range 2 {
		_, err := fixture.service.Chat(ctx, fixture.scope(), llmdomain.ChatRequest{
			Messages: []llmdomain.Message{{Role: llmdomain.RoleUser, Text: "Catat pesanan"}},
			Metadata: llmdomain.RequestMetadata{WorkspaceID: fixture.workspace},
		})
		require.NoError(t, err)
	}

	rows := fixture.ledgerRows(t)
	require.Len(t, rows, 2, "two calls, two rows: the rejected attempt is not billed")
	for _, row := range rows {
		require.Equal(t, 10, row.Usage.InputTokens)
	}
}

// TestAMalformedRequestDoesNotSpendTheChain: a request every provider would
// reject must fail once instead of trying them all.
func TestAMalformedRequestDoesNotSpendTheChain(t *testing.T) {
	fixture := newGatewayFixture(t, appDatabase(t))
	ctx := t.Context()

	var secondCalls int
	first := brokenServer(t, http.StatusBadRequest)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondCalls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(second.Close)

	fixture.addProvider(t, "Pertama", "gpt-4o-mini", first.URL)
	fixture.addProvider(t, "Kedua", "gpt-4o-mini", second.URL, func(cfg *llmdomain.ProviderConfig) {
		cfg.Priority = 20
	})

	_, err := fixture.service.Chat(ctx, fixture.scope(), llmdomain.ChatRequest{
		Messages: []llmdomain.Message{{Role: llmdomain.RoleUser, Text: "apa saja"}},
		Metadata: llmdomain.RequestMetadata{WorkspaceID: fixture.workspace},
	})
	require.ErrorIs(t, err, llmdomain.ErrInvalidRequest)
	require.Zero(t, secondCalls, "the second provider was never called")
	require.Empty(t, fixture.ledgerRows(t), "a failed request is not billed")
}

// TestTheBoluOverrideLeadsTheChain wires the per-Bolu override through the real
// resolver: the Bolu's own endpoint is tried before the workspace chain.
func TestTheBoluOverrideLeadsTheChain(t *testing.T) {
	fixture := newGatewayFixture(t, appDatabase(t))
	ctx := t.Context()

	// The workspace provider is down; the Bolu's own provider answers.
	down := brokenServer(t, http.StatusServiceUnavailable)
	healthy := completionServer(t, "dari bolu", 20, 4)

	fixture.addProvider(t, "Workspace", "gpt-4o-mini", down.URL)

	agentID := fixture.firstAgentID(t)
	scope := fixture.scope()
	scope.AgentID = agentID

	require.NoError(t, fixture.service.SetAgentModel(ctx, scope, llmdomain.AgentOverride{
		Adapter: llmdomain.AdapterOpenAI,
		BaseURL: healthy.URL,
		Model:   "gpt-4o",
		APIKey:  "sk-bolu",
	}))

	response, err := fixture.service.Chat(ctx, scope, llmdomain.ChatRequest{
		Messages: []llmdomain.Message{{Role: llmdomain.RoleUser, Text: "Catat pesanan"}},
		Metadata: llmdomain.RequestMetadata{WorkspaceID: fixture.workspace, AgentID: agentID},
	})
	require.NoError(t, err)
	require.Equal(t, "dari bolu", response.Text)

	rows := fixture.ledgerRows(t)
	require.Len(t, rows, 1)
	require.Equal(t, "gpt-4o", rows[0].Model)
}

// firstAgentID returns one Bolu of the fixture workspace, creating it when the
// workspace is empty, which is how a workspace starts.
func (f *gatewayFixture) firstAgentID(t *testing.T) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	err := f.pool.InScopeRead(t.Context(), database.Scope{WorkspaceID: f.workspace}, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(),
			"SELECT id FROM agents WHERE workspace_id = $1 AND deleted_at IS NULL ORDER BY created_at LIMIT 1",
			f.workspace).Scan(&id)
	})
	if err == nil {
		return id
	}
	require.ErrorIs(t, err, pgx.ErrNoRows)

	return seedAgent(t, f.pool, f.workspace)
}

// TestNoProviderIsAnActionableError: a workspace with nothing configured gets a
// message the screen can act on, not a provider failure.
func TestNoProviderIsAnActionableError(t *testing.T) {
	fixture := newGatewayFixture(t, appDatabase(t))

	_, err := fixture.service.Chat(t.Context(), fixture.scope(), llmdomain.ChatRequest{
		Messages: []llmdomain.Message{{Role: llmdomain.RoleUser, Text: "halo"}},
		Metadata: llmdomain.RequestMetadata{WorkspaceID: fixture.workspace},
	})
	require.ErrorIs(t, err, llmdomain.ErrNoProvider)
	require.Contains(t, fmt.Sprint(err), "Pengaturan")
}
