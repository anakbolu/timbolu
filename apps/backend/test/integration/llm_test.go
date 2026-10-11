package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	agentdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/domain"
	agentrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/repository"
	authrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/repository"
	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	llmrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/repository"
	llmservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/service"
	workspacedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	workspacerepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/repository"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/crypto"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/quota"
)

// testEncryptionKey is a fixed 32-byte key, so the tests read back what they
// wrote without depending on a development .env.
const testEncryptionKey = "0123456789abcdef0123456789abcdef"

// stubProvider is one adapter whose answer a test scripts. The real adapters are
// covered by the contract suite and the integration tests cannot call a provider.
type stubProvider struct {
	model string
	usage llmdomain.Usage
	err   error
}

func (p *stubProvider) Name() string  { return llmdomain.AdapterOpenAI }
func (p *stubProvider) Model() string { return p.model }

func (p *stubProvider) Capabilities() llmdomain.Capabilities {
	return llmdomain.Capabilities{Tools: true, Vision: true, Streaming: true, MaxContextTokens: 128000}
}

func (p *stubProvider) Chat(context.Context, llmdomain.ChatRequest) (llmdomain.ChatResponse, error) {
	if p.err != nil {
		return llmdomain.ChatResponse{}, p.err
	}
	return llmdomain.ChatResponse{
		Text:         "siap",
		FinishReason: llmdomain.FinishStop,
		Usage:        p.usage,
		Model:        p.model,
	}, nil
}

func (p *stubProvider) Stream(context.Context, llmdomain.ChatRequest) (<-chan llmdomain.StreamEvent, error) {
	events := make(chan llmdomain.StreamEvent, 2)
	events <- llmdomain.StreamEvent{Type: llmdomain.EventUsage, Usage: &p.usage}
	events <- llmdomain.StreamEvent{Type: llmdomain.EventDone, FinishReason: llmdomain.FinishStop}
	close(events)
	return events, nil
}

// stubFactory hands the same scripted adapter to every configuration.
type stubFactory struct{ provider *stubProvider }

func (f stubFactory) Build(cfg llmdomain.ProviderConfig) (llmdomain.Provider, error) {
	if cfg.Adapter != llmdomain.AdapterOpenAI {
		return nil, errors.New("stubFactory: only the openai adapter is scripted")
	}
	f.provider.model = cfg.Model
	return f.provider, nil
}

// llmFixture is one onboarded workspace with the model gateway wired the way the
// container wires it: the real repository, the real encryption, and a scripted
// adapter.
type llmFixture struct {
	repo      *llmrepo.Repository
	agents    *llmrepo.AgentStore
	service   *llmservice.Service
	pool      *database.Pool
	workspace uuid.UUID
	userID    uuid.UUID
	box       *crypto.Box
	provider  *stubProvider
	agentIDs  []uuid.UUID
}

func newLLMFixture(t *testing.T, dsn string) *llmFixture {
	t.Helper()

	ctx := t.Context()
	pool := poolFor(t, dsn)

	box, err := crypto.NewFromString(testEncryptionKey)
	require.NoError(t, err)

	agents := agentrepo.New(pool)
	workspaces := workspacerepo.New(pool, agents)
	repo := llmrepo.New(pool)

	user, err := authrepo.New(pool.PgxPool()).CreateUser(ctx, uuid.NewString()+"@example.com", "$argon2id$hash")
	require.NoError(t, err)

	onboarded, err := workspaces.Onboard(ctx, user.ID, workspacedomain.OnboardRequest{
		Name:          "Toko Sinar",
		BusinessField: "Retail",
		Timezone:      "Asia/Jakarta",
	})
	require.NoError(t, err)

	// The onboarding step copies the six Bolu templates, so the workspace has
	// agents an override can point at.
	stored, err := agents.List(ctx, agentdomain.Scope{UserID: user.ID, WorkspaceID: onboarded.Workspace.ID})
	require.NoError(t, err)
	require.NotEmpty(t, stored)

	agentIDs := make([]uuid.UUID, 0, len(stored))
	for _, agent := range stored {
		agentIDs = append(agentIDs, agent.ID)
	}

	provider := &stubProvider{usage: llmdomain.Usage{InputTokens: 1200, OutputTokens: 300}}
	service := llmservice.New(llmservice.Deps{
		Store:    repo,
		Agents:   repo.NewAgentStore(box),
		Factory:  stubFactory{provider: provider},
		Box:      box,
		Recorder: repo,
		Reader:   repo,
		Settings: llmservice.Config{},
	})

	return &llmFixture{
		repo:      repo,
		agents:    repo.NewAgentStore(box),
		service:   service,
		pool:      pool,
		workspace: onboarded.Workspace.ID,
		userID:    user.ID,
		box:       box,
		provider:  provider,
		agentIDs:  agentIDs,
	}
}

// inScope runs one query inside the fixture's tenant scope, which is how the
// application reads: Row Level Security filters it even when the query carries
// no workspace filter of its own.
func (f *llmFixture) inScope(ctx context.Context, scan func(pgx.Tx) error) error {
	return f.pool.InScopeRead(ctx, database.Scope{WorkspaceID: f.workspace}, scan)
}

// providerKeyCiphertext reads the stored ciphertext, so a test can prove the
// plaintext is not in the column.
func (f *llmFixture) providerKeyCiphertext(t *testing.T, id uuid.UUID) []byte {
	t.Helper()

	var stored []byte
	require.NoError(t, f.inScope(t.Context(), func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), "SELECT api_key_encrypted FROM llm_providers WHERE id = $1", id).Scan(&stored)
	}))
	return stored
}

// agentModelJSON reads the raw override document of one Bolu.
func (f *llmFixture) agentModelJSON(t *testing.T, agentID uuid.UUID) string {
	t.Helper()

	var raw string
	require.NoError(t, f.inScope(t.Context(), func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), "SELECT default_model::text FROM agents WHERE id = $1", agentID).Scan(&raw)
	}))
	return raw
}

// ledgerTotals counts the usage rows of the workspace and sums their tokens.
func (f *llmFixture) ledgerTotals(t *testing.T) (int, int) {
	t.Helper()

	var rows, tokens int
	require.NoError(t, f.inScope(t.Context(), func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(),
			"SELECT count(*), COALESCE(sum(input_tokens + output_tokens), 0) FROM usage_ledger WHERE workspace_id = $1",
			f.workspace).Scan(&rows, &tokens)
	}))
	return rows, tokens
}

func (f *llmFixture) scope() llmdomain.Scope {
	return llmdomain.Scope{UserID: f.userID, WorkspaceID: f.workspace}
}

// createProvider stores one configuration and returns its redacted view.
func (f *llmFixture) createProvider(t *testing.T, name, model, key string, mutate ...func(*llmdomain.ProviderConfig)) llmdomain.Redacted {
	t.Helper()

	cfg := llmdomain.ProviderConfig{
		Name:          name,
		Adapter:       llmdomain.AdapterOpenAI,
		BaseURL:       "https://api.example.test",
		Model:         model,
		APIKey:        key,
		Priority:      10,
		ContextTokens: 128000,
		Enabled:       true,
	}
	for _, change := range mutate {
		change(&cfg)
	}

	created, err := f.service.Upsert(t.Context(), f.scope(), llmdomain.UpsertRequest{ProviderConfig: cfg})
	require.NoError(t, err)
	return created
}

// updateProvider edits an existing configuration. An empty key means "keep the
// stored secret", and an update that reuses a name must target the same row,
// because two providers cannot share a label.
func (f *llmFixture) updateProvider(t *testing.T, id uuid.UUID, mutate ...func(*llmdomain.ProviderConfig)) llmdomain.Redacted {
	t.Helper()

	cfg := llmdomain.ProviderConfig{
		ID:      id,
		Name:    "Utama",
		Adapter: llmdomain.AdapterOpenAI,
		Model:   "gpt-4o",
		Enabled: true,
	}
	for _, change := range mutate {
		change(&cfg)
	}

	updated, err := f.service.Upsert(t.Context(), f.scope(), llmdomain.UpsertRequest{ProviderConfig: cfg})
	require.NoError(t, err)
	return updated
}

// createTask inserts the task a usage row may point at, which the runtime owns
// and this epic only references.
func (f *llmFixture) createTask(t *testing.T, agentID uuid.UUID) uuid.UUID {
	t.Helper()

	var taskID uuid.UUID
	require.NoError(t, f.pool.InScope(t.Context(), database.Scope{WorkspaceID: f.workspace}, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(),
			`INSERT INTO tasks (workspace_id, agent_id, trigger, title)
			 VALUES ($1, $2, 'chat', 'Catat pesanan')
			 RETURNING id`,
			f.workspace, agentID).Scan(&taskID)
	}))
	return taskID
}

// TestProviderConfigurationRoundTrip is the E4.6 gate: a provider survives a
// write and a read, with its key sealed in the database.
func TestProviderConfigurationRoundTrip(t *testing.T) {
	fixture := newLLMFixture(t, appDatabase(t))
	ctx := t.Context()

	created := fixture.createProvider(t, "Utama", "gpt-4o-mini", "sk-live-secret")
	require.True(t, created.IsDefault, "the first provider becomes the default")
	require.True(t, created.HasAPIKey)

	// The secret must not be readable in the row itself. The read runs in the
	// tenant scope, the same way the application reads it, so Row Level Security
	// applies to the assertion too.
	stored := fixture.providerKeyCiphertext(t, created.ID)
	require.NotEmpty(t, stored)
	require.NotContains(t, string(stored), "sk-live-secret")

	providers, err := fixture.service.Providers(ctx, fixture.scope())
	require.NoError(t, err)
	require.Len(t, providers, 1)
	require.Equal(t, "Utama", providers[0].Name)

	// The call path decrypts the key, which is what proves the round trip.
	chain, err := fixture.service.Resolve(ctx, fixture.scope())
	require.NoError(t, err)
	require.Len(t, chain, 1)
	require.Equal(t, "sk-live-secret", chain[0].APIKey)

	// An edit that does not send a key keeps the stored one.
	updated := fixture.updateProvider(t, created.ID)
	require.True(t, updated.HasAPIKey, "an edit without a key must keep the stored secret")

	// And clearing it is an explicit request.
	cleared, err := fixture.service.Upsert(ctx, fixture.scope(), llmdomain.UpsertRequest{
		ProviderConfig: llmdomain.ProviderConfig{
			ID:      updated.ID,
			Name:    "Utama",
			Adapter: llmdomain.AdapterOpenAI,
			Model:   "gpt-4o",
			Enabled: true,
		},
		ClearKey: true,
	})
	require.NoError(t, err)
	require.False(t, cleared.HasAPIKey)

	require.NoError(t, fixture.service.Delete(ctx, fixture.scope(), cleared.ID))
	_, err = fixture.repo.Get(ctx, fixture.workspace, cleared.ID)
	require.ErrorIs(t, err, llmdomain.ErrProviderNotFound)
}

// TestProvidersAreIsolatedByWorkspace is the tenant gate: two workspaces of the
// same deployment never see each other's providers.
func TestProvidersAreIsolatedByWorkspace(t *testing.T) {
	dsn := appDatabase(t)
	fixture := newLLMFixture(t, dsn)
	other := newLLMFixture(t, dsn)
	ctx := t.Context()

	created := fixture.createProvider(t, "Utama", "gpt-4o-mini", "sk-live-secret")

	providers, err := other.service.Providers(ctx, other.scope())
	require.NoError(t, err)
	require.Empty(t, providers, "another workspace must see nothing")

	_, err = other.repo.Get(ctx, other.workspace, created.ID)
	require.ErrorIs(t, err, llmdomain.ErrProviderNotFound)

	_, err = other.service.Resolve(ctx, other.scope())
	require.ErrorIs(t, err, llmdomain.ErrNoProvider)
}

// TestASecondDefaultReplacesTheFirst keeps the single-default invariant, which a
// workspace's fallback chain depends on.
func TestASecondDefaultReplacesTheFirst(t *testing.T) {
	fixture := newLLMFixture(t, appDatabase(t))
	ctx := t.Context()

	first := fixture.createProvider(t, "Satu", "gpt-4o-mini", "sk-1")
	second := fixture.createProvider(t, "Dua", "gpt-4o", "sk-2", func(cfg *llmdomain.ProviderConfig) {
		cfg.IsDefault = true
		cfg.Priority = 5
	})

	providers, err := fixture.service.Providers(ctx, fixture.scope())
	require.NoError(t, err)
	require.Len(t, providers, 2)
	require.Equal(t, second.ID, providers[0].ID, "the new default leads the chain")
	require.True(t, providers[0].IsDefault)
	require.Equal(t, first.ID, providers[1].ID)
	require.False(t, providers[1].IsDefault)
}

// TestAgentModelOverrideIsSealed is the per-Bolu gate: the override lives in
// agents.default_model, and a key stored there is ciphertext too.
func TestAgentModelOverrideIsSealed(t *testing.T) {
	fixture := newLLMFixture(t, appDatabase(t))
	ctx := t.Context()
	agentID := fixture.agentIDs[0]

	scope := fixture.scope()
	scope.AgentID = agentID

	require.NoError(t, fixture.service.SetAgentModel(ctx, scope, llmdomain.AgentOverride{
		Adapter:   llmdomain.AdapterOpenAI,
		Model:     "gpt-4o-mini",
		APIKey:    "sk-agent-secret",
		MaxTokens: 2048,
	}))

	raw := fixture.agentModelJSON(t, agentID)
	require.NotContains(t, raw, "sk-agent-secret", "the override key must be sealed in the column")

	override, err := fixture.service.AgentModel(ctx, scope)
	require.NoError(t, err)
	require.Equal(t, "gpt-4o-mini", override.Model)
	require.Equal(t, "sk-agent-secret", override.APIKey)
	require.Equal(t, 2048, override.MaxTokens)

	// The Bolu's own provider leads the chain, ahead of the workspace.
	fixture.createProvider(t, "Utama", "gpt-4o", "sk-live-1")
	chain, err := fixture.service.Resolve(ctx, scope)
	require.NoError(t, err)
	require.Len(t, chain, 2)
	require.Equal(t, "gpt-4o-mini", chain[0].Model)
	require.Equal(t, "sk-agent-secret", chain[0].APIKey)
}

// TestEveryCallIsRecordedExactlyOnce is the E4.7 gate: one provider call means
// one ledger row, and the daily aggregation sees it.
func TestEveryCallIsRecordedExactlyOnce(t *testing.T) {
	fixture := newLLMFixture(t, appDatabase(t))
	ctx := t.Context()

	fixture.createProvider(t, "Utama", "gpt-4o-mini", "sk-live-1")

	taskID := fixture.createTask(t, fixture.agentIDs[0])
	for range 3 {
		response, err := fixture.service.Chat(ctx, fixture.scope(), llmdomain.ChatRequest{
			Messages: []llmdomain.Message{{Role: llmdomain.RoleUser, Text: "Catat pesanan"}},
			Metadata: llmdomain.RequestMetadata{
				WorkspaceID: fixture.workspace,
				TaskID:      taskID,
				Purpose:     llmdomain.PurposeAgent,
			},
		})
		require.NoError(t, err)
		require.Equal(t, "siap", response.Text)
	}

	rows, tokens := fixture.ledgerTotals(t)
	require.Equal(t, 3, rows, "exactly one row per call")
	require.Equal(t, 3*1500, tokens)

	usage, err := fixture.service.Usage(ctx, fixture.scope(), 1)
	require.NoError(t, err)
	require.Len(t, usage, 1)
	require.Equal(t, int64(3), usage[0].Calls)
	require.Equal(t, int64(3*1500), usage[0].Total())
	require.Equal(t, "gpt-4o-mini", usage[0].Model)

	sum, err := fixture.repo.SumSince(ctx, fixture.workspace, llmservice.PeriodStart(time.Now()))
	require.NoError(t, err)
	require.Equal(t, int64(3*1500), sum)
}

// TestQuotaCheckReadsTheLedgerWhenThereIsNoRedis proves the durable fallback: the
// check is still correct without the live counter.
func TestQuotaCheckReadsTheLedgerWhenThereIsNoRedis(t *testing.T) {
	fixture := newLLMFixture(t, appDatabase(t))
	ctx := t.Context()

	fixture.createProvider(t, "Utama", "gpt-4o-mini", "sk-live-1")

	_, err := fixture.service.Chat(ctx, fixture.scope(), llmdomain.ChatRequest{
		Messages: []llmdomain.Message{{Role: llmdomain.RoleUser, Text: "Catat pesanan"}},
		Metadata: llmdomain.RequestMetadata{WorkspaceID: fixture.workspace},
	})
	require.NoError(t, err)

	// The stub factory is swapped for an adapter that would fail the request, so
	// a call that gets through is proof the check let it, not that it answered.
	fixture.provider.err = errors.New("must not be called")

	blocked := llmservice.New(llmservice.Deps{
		Store:    fixture.repo,
		Agents:   fixture.agents,
		Factory:  stubFactory{provider: fixture.provider},
		Box:      fixture.box,
		Recorder: fixture.repo,
		Reader:   fixture.repo,
		Quota:    fixedQuota{tokens: 1500},
		Settings: llmservice.Config{QuotaEnabled: true},
	})

	_, err = blocked.Chat(ctx, fixture.scope(), llmdomain.ChatRequest{
		Messages: []llmdomain.Message{{Role: llmdomain.RoleUser, Text: "Catat pesanan"}},
		Metadata: llmdomain.RequestMetadata{WorkspaceID: fixture.workspace},
	})
	require.ErrorIs(t, err, llmdomain.ErrQuotaExceeded)
	require.NotContains(t, err.Error(), "must not be called", "the provider was never reached")
}

// TestQuotaCounterFollowsTheJakartaMonth: the live counter rolls over when the
// customer's month does.
func TestQuotaCounterFollowsTheJakartaMonth(t *testing.T) {
	counter := quota.New(newRedisRawClient(t))

	workspace := uuid.New()
	// 23:59 on 31 October in Jakarta, which is 16:59 UTC.
	current := time.Date(2026, 10, 31, 16, 59, 0, 0, time.UTC)
	counter.SetClock(func() time.Time { return current })

	require.NoError(t, counter.Add(t.Context(), workspace, 500))

	spent, err := counter.Spent(t.Context(), workspace)
	require.NoError(t, err)
	require.Equal(t, int64(500), spent)

	// Two minutes later the month has rolled over in Jakarta.
	counter.SetClock(func() time.Time { return current.Add(2 * time.Minute) })

	spent, err = counter.Spent(t.Context(), workspace)
	require.NoError(t, err)
	require.Zero(t, spent, "the new period starts empty")

	require.NoError(t, counter.Reset(t.Context(), workspace))
}

// newRedisRawClient is the raw Redis client the quota counter takes, reusing the
// container the rate limit tests start.
func newRedisRawClient(t *testing.T) *redis.Client {
	t.Helper()

	options, err := redis.ParseURL(redisURL(t))
	require.NoError(t, err)

	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })

	return client
}

// fixedQuota is the allowance policy before subscriptions exist.
type fixedQuota struct {
	tokens    int64
	dailyCost int64
}

func (f fixedQuota) Allowance(context.Context, uuid.UUID) (int64, error) { return f.tokens, nil }

func (f fixedQuota) DailyCostAllowance(context.Context, uuid.UUID) (int64, error) {
	return f.dailyCost, nil
}
