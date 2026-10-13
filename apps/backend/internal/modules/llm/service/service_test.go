package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain/mocks"
)

// fakeBox is a reversible stand-in for the real AES-GCM box: the tests care
// about which key reaches the store, not about the cipher.
type fakeBox struct{ calls int }

func (b *fakeBox) Encrypt(plaintext string) ([]byte, error) {
	b.calls++
	return []byte("sealed:" + plaintext), nil
}

func (b *fakeBox) Decrypt(ciphertext []byte) (string, error) {
	const prefix = "sealed:"
	if len(ciphertext) < len(prefix) || string(ciphertext[:len(prefix)]) != prefix {
		return "", errors.New("crypto: ciphertext is invalid")
	}
	return string(ciphertext[len(prefix):]), nil
}

// fakeProvider is one adapter whose answers a test scripts. The real adapters
// are covered by the contract suite; what matters here is how the gateway drives
// them.
type fakeProvider struct {
	name     string
	model    string
	caps     domain.Capabilities
	chatFn   func(domain.ChatRequest) (domain.ChatResponse, error)
	streamFn func(domain.ChatRequest) (<-chan domain.StreamEvent, error)
	embedFn  func(domain.EmbedRequest) (domain.EmbedResponse, error)
}

func (p *fakeProvider) Name() string { return p.name }

func (p *fakeProvider) Model() string { return p.model }

func (p *fakeProvider) Capabilities() domain.Capabilities { return p.caps }

func (p *fakeProvider) Chat(_ context.Context, req domain.ChatRequest) (domain.ChatResponse, error) {
	if p.chatFn == nil {
		return domain.ChatResponse{}, errors.New("fake provider: no chat scripted")
	}
	return p.chatFn(req)
}

func (p *fakeProvider) Stream(_ context.Context, req domain.ChatRequest) (<-chan domain.StreamEvent, error) {
	if p.streamFn == nil {
		return nil, errors.New("fake provider: no stream scripted")
	}
	return p.streamFn(req)
}

func (p *fakeProvider) Embed(_ context.Context, req domain.EmbedRequest) (domain.EmbedResponse, error) {
	if p.embedFn == nil {
		// A provider without the capability answers the way a real one does, so a
		// test that forgets to script it sees the honest refusal.
		return domain.EmbedResponse{}, fmt.Errorf("%w: fake provider has no embedding route", domain.ErrUnsupported)
	}
	return p.embedFn(req)
}

// fakeFactory behaves like the platform registry: it accepts the two known
// adapters, refuses anything else, and hands back what a test registered for the
// model.
type fakeFactory struct {
	providers map[string]*fakeProvider
	built     []domain.ProviderConfig
}

func (f *fakeFactory) Build(cfg domain.ProviderConfig) (domain.Provider, error) {
	if cfg.Adapter != domain.AdapterOpenAI && cfg.Adapter != domain.AdapterAnthropic {
		return nil, fmt.Errorf("%w: unknown adapter %q", domain.ErrInvalidRequest, cfg.Adapter)
	}
	f.built = append(f.built, cfg)

	if provider, ok := f.providers[cfg.Model]; ok {
		return provider, nil
	}
	return &fakeProvider{
		name:  cfg.Adapter,
		model: cfg.Model,
		caps:  domain.Capabilities{Tools: true, Vision: true, Streaming: true, MaxContextTokens: cfg.ContextTokens},
	}, nil
}

// models lists the models the factory was asked for, in order, which is how a
// test asserts the fallback sequence.
func (f *fakeFactory) models() []string {
	models := make([]string, 0, len(f.built))
	for _, cfg := range f.built {
		models = append(models, cfg.Model)
	}
	return models
}

type harness struct {
	deps     Deps
	store    *mocks.Store
	agents   *mocks.AgentStore
	factory  *fakeFactory
	recorder *mocks.UsageRecorder
	reader   *mocks.UsageReader
	counter  *mocks.QuotaCounter
	policy   *mocks.QuotaPolicy
	service  *Service
	box      *fakeBox
}

func newHarness(t *testing.T, mutate ...func(*Deps)) *harness {
	t.Helper()

	box := &fakeBox{}
	factory := &fakeFactory{providers: map[string]*fakeProvider{}}
	deps := Deps{
		Store:    mocks.NewStore(t),
		Agents:   mocks.NewAgentStore(t),
		Factory:  factory,
		Box:      box,
		Recorder: mocks.NewUsageRecorder(t),
		Reader:   mocks.NewUsageReader(t),
	}
	for _, change := range mutate {
		change(&deps)
	}

	return &harness{
		deps:     deps,
		store:    deps.Store.(*mocks.Store),
		agents:   deps.Agents.(*mocks.AgentStore),
		factory:  factory,
		recorder: deps.Recorder.(*mocks.UsageRecorder),
		reader:   deps.Reader.(*mocks.UsageReader),
		service:  New(deps),
		box:      box,
	}
}

// provider registers a scripted provider for a model and returns it.
func (h *harness) provider(model string, mutate ...func(*fakeProvider)) *fakeProvider {
	provider := &fakeProvider{
		name:  domain.AdapterOpenAI,
		model: model,
		caps:  domain.Capabilities{Tools: true, Vision: true, Streaming: true, MaxContextTokens: 128000},
	}
	for _, change := range mutate {
		change(provider)
	}
	h.factory.providers[model] = provider
	return provider
}

// withQuota wires the live counter and the allowance.
func (h *harness) withQuota(t *testing.T, allowance int64) {
	t.Helper()

	quotaPolicy := mocks.NewQuotaPolicy(t)
	quotaPolicy.EXPECT().Allowance(mock.Anything, mock.Anything).Return(allowance, nil)
	h.policy = quotaPolicy
	h.service.deps.Quota = quotaPolicy
	h.service.settings.QuotaEnabled = true
}

// withCounter adds the live token counter, which the quota check reads first.
func (h *harness) withCounter(t *testing.T) *mocks.QuotaCounter {
	t.Helper()

	counter := mocks.NewQuotaCounter(t)
	h.counter = counter
	h.service.deps.Counter = counter
	return counter
}

func workspaceScope() domain.Scope {
	return domain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}
}

// storedProvider builds a row as the store returns it: the key is ciphertext and
// the plaintext field is empty.
func storedProvider(mutate ...func(*domain.StoredProvider)) domain.StoredProvider {
	provider := domain.StoredProvider{
		ProviderConfig: domain.ProviderConfig{
			ID:            uuid.New(),
			WorkspaceID:   uuid.New(),
			Name:          "Utama",
			Adapter:       domain.AdapterOpenAI,
			BaseURL:       "https://api.example.test",
			Model:         "gpt-4o-mini",
			Priority:      10,
			ContextTokens: 128000,
			Enabled:       true,
			CreatedAt:     time.Now(),
			UpdatedAt:     time.Now(),
		},
		EncryptedKey: []byte("sealed:sk-live-1"),
	}
	for _, change := range mutate {
		change(&provider)
	}
	return provider
}

// TestProvidersRedactsTheSecret is the rule the settings screen depends on: it
// learns whether a key is stored, never what it is.
func TestProvidersRedactsTheSecret(t *testing.T) {
	h := newHarness(t)
	scope := workspaceScope()
	provider := storedProvider(func(p *domain.StoredProvider) { p.WorkspaceID = scope.WorkspaceID })

	h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return([]domain.StoredProvider{provider}, nil)

	providers, err := h.service.Providers(context.Background(), scope)
	require.NoError(t, err)
	require.Len(t, providers, 1)
	require.True(t, providers[0].HasAPIKey)
	require.Equal(t, "Utama", providers[0].Name)

	encoded, err := json.Marshal(providers[0])
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "sk-live-1", "the response must not carry the secret")
}

// TestUpsertEncryptsTheKeyBeforeStoring is the invariant the migration's bytea
// column exists for.
func TestUpsertEncryptsTheKeyBeforeStoring(t *testing.T) {
	h := newHarness(t)
	scope := workspaceScope()

	h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return(nil, nil)
	h.store.EXPECT().Create(mock.Anything, mock.MatchedBy(func(stored domain.StoredProvider) bool {
		return string(stored.EncryptedKey) == "sealed:sk-new" &&
			stored.APIKey == "" &&
			stored.Name == "Utama" &&
			stored.IsDefault
	})).Return(storedProvider(func(p *domain.StoredProvider) { p.IsDefault = true }), nil)

	created, err := h.service.Upsert(context.Background(), scope, domain.UpsertRequest{
		ProviderConfig: domain.ProviderConfig{
			Name:    "Utama",
			Adapter: domain.AdapterOpenAI,
			Model:   "gpt-4o-mini",
			APIKey:  "sk-new",
			Enabled: true,
		},
	})
	require.NoError(t, err)
	require.True(t, created.IsDefault, "the first provider becomes the default")
	require.Equal(t, 1, h.box.calls)
}

// TestUpsertRefusesAKeyWithoutAnEncryptionKey: storing a secret in clear text is
// worse than refusing the write.
func TestUpsertRefusesAKeyWithoutAnEncryptionKey(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.Box = nil })
	scope := workspaceScope()

	_, err := h.service.Upsert(context.Background(), scope, domain.UpsertRequest{
		ProviderConfig: domain.ProviderConfig{
			Name:    "Utama",
			Adapter: domain.AdapterOpenAI,
			Model:   "gpt-4o-mini",
			APIKey:  "sk-new",
			Enabled: true,
		},
	})
	require.ErrorIs(t, err, domain.ErrInvalidRequest)
	require.ErrorContains(t, err, "no encryption key is configured")
}

// TestUpsertKeepsTheStoredKeyWhenNoneIsSent: the screen never receives the
// secret, so an edit must not wipe it.
func TestUpsertKeepsTheStoredKeyWhenNoneIsSent(t *testing.T) {
	h := newHarness(t)
	scope := workspaceScope()
	existing := storedProvider(func(p *domain.StoredProvider) { p.WorkspaceID = scope.WorkspaceID })

	h.store.EXPECT().Update(mock.Anything, mock.MatchedBy(func(stored domain.StoredProvider) bool {
		return stored.EncryptedKey == nil
	})).Return(existing, nil)

	_, err := h.service.Upsert(context.Background(), scope, domain.UpsertRequest{
		ProviderConfig: domain.ProviderConfig{
			ID:      existing.ID,
			Name:    "Utama (baru)",
			Adapter: domain.AdapterOpenAI,
			Model:   "gpt-4o-mini",
			Enabled: true,
		},
	})
	require.NoError(t, err)
	require.Zero(t, h.box.calls, "no key was sent, so nothing was sealed")
}

// TestUpsertClearKeyRemovesTheSecret: the operator asked for the key to go.
func TestUpsertClearKeyRemovesTheSecret(t *testing.T) {
	h := newHarness(t)
	scope := workspaceScope()
	existing := storedProvider(func(p *domain.StoredProvider) { p.WorkspaceID = scope.WorkspaceID })

	h.store.EXPECT().Update(mock.Anything, mock.MatchedBy(func(stored domain.StoredProvider) bool {
		return stored.EncryptedKey != nil && len(stored.EncryptedKey) == 0
	})).Return(existing, nil)

	_, err := h.service.Upsert(context.Background(), scope, domain.UpsertRequest{
		ProviderConfig: domain.ProviderConfig{
			ID:      existing.ID,
			Name:    "Utama",
			Adapter: domain.AdapterOpenAI,
			Model:   "gpt-4o-mini",
			Enabled: true,
		},
		ClearKey: true,
	})
	require.NoError(t, err)
	require.Zero(t, h.box.calls)
}

func TestUpsertValidatesTheRequest(t *testing.T) {
	h := newHarness(t)
	scope := workspaceScope()

	tests := map[string]domain.ProviderConfig{
		"missing model":   {Name: "Utama", Adapter: domain.AdapterOpenAI},
		"missing name":    {Adapter: domain.AdapterOpenAI, Model: "gpt-4o-mini"},
		"unknown adapter": {Name: "Utama", Adapter: "mystery", Model: "x"},
		"priority too high": {
			Name: "Utama", Adapter: domain.AdapterOpenAI, Model: "gpt-4o-mini", Priority: MaxPriority + 1,
		},
		"negative tokens": {
			Name: "Utama", Adapter: domain.AdapterOpenAI, Model: "gpt-4o-mini", MaxTokens: -1,
		},
		"long name": {
			Name: string(make([]rune, NameMaxLength+1)), Adapter: domain.AdapterOpenAI, Model: "gpt-4o-mini",
		},
	}

	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := h.service.Upsert(context.Background(), scope, domain.UpsertRequest{ProviderConfig: cfg})
			require.ErrorIs(t, err, domain.ErrInvalidRequest)
		})
	}
}

// TestResolveOrdersTheChain is the fallback order the task asks for: the Bolu
// override, then the workspace default, then the priority order, then the
// platform provider.
func TestResolveOrdersTheChain(t *testing.T) {
	scope := workspaceScope()
	agentID := uuid.New()

	secondary := storedProvider(func(p *domain.StoredProvider) {
		p.WorkspaceID = scope.WorkspaceID
		p.ID = uuid.New()
		p.Name = "Cadangan"
		p.Model = "claude-3-5-haiku"
		p.Adapter = domain.AdapterAnthropic
		p.Priority = 20
		p.IsDefault = false
		p.EncryptedKey = []byte("sealed:sk-2")
	})
	primary := storedProvider(func(p *domain.StoredProvider) {
		p.WorkspaceID = scope.WorkspaceID
		p.Name = "Utama"
		p.Priority = 50
		p.IsDefault = true
	})
	disabled := storedProvider(func(p *domain.StoredProvider) {
		p.WorkspaceID = scope.WorkspaceID
		p.ID = uuid.New()
		p.Name = "Mati"
		p.Enabled = false
	})

	platform := domain.ProviderConfig{
		Name:    "platform",
		Adapter: domain.AdapterOpenAI,
		BaseURL: "https://platform.example.test",
		Model:   "gpt-4o-mini",
		APIKey:  "sk-platform",
		Enabled: true,
	}

	t.Run("workspace order", func(t *testing.T) {
		h := newHarness(t, func(d *Deps) { d.Default = platform })
		h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return([]domain.StoredProvider{primary, secondary, disabled}, nil)

		chain, err := h.service.Resolve(context.Background(), scope)
		require.NoError(t, err)
		require.Len(t, chain, 2, "the disabled provider is skipped, and the platform provider stays unused")
		require.Equal(t, "Utama", chain[0].Name)
		require.Equal(t, "Cadangan", chain[1].Name)
		require.Equal(t, "sk-live-1", chain[0].APIKey, "the stored key is decrypted for the call")
	})

	t.Run("the platform provider answers a workspace that configured nothing", func(t *testing.T) {
		h := newHarness(t, func(d *Deps) { d.Default = platform })
		h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return(nil, nil)

		chain, err := h.service.Resolve(context.Background(), scope)
		require.NoError(t, err)
		require.Len(t, chain, 1)
		require.Equal(t, "platform", chain[0].Name)
		require.Equal(t, "sk-platform", chain[0].APIKey)
	})

	t.Run("pinned provider goes first", func(t *testing.T) {
		h := newHarness(t)
		h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return([]domain.StoredProvider{primary, secondary}, nil)
		h.agents.EXPECT().AgentModel(mock.Anything, scope.WorkspaceID, agentID).
			Return(domain.AgentOverride{ProviderID: secondary.ID}, nil)

		chain, err := h.service.Resolve(context.Background(), domain.Scope{
			UserID: scope.UserID, WorkspaceID: scope.WorkspaceID, AgentID: agentID,
		})
		require.NoError(t, err)
		require.Len(t, chain, 2)
		require.Equal(t, secondary.ID, chain[0].ID)
		require.Equal(t, primary.ID, chain[1].ID, "the rest of the chain stays behind it")
	})

	t.Run("model only override replaces the head", func(t *testing.T) {
		h := newHarness(t)
		h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return([]domain.StoredProvider{primary, secondary}, nil)
		h.agents.EXPECT().AgentModel(mock.Anything, scope.WorkspaceID, agentID).
			Return(domain.AgentOverride{Model: "gpt-4o"}, nil)

		chain, err := h.service.Resolve(context.Background(), domain.Scope{
			UserID: scope.UserID, WorkspaceID: scope.WorkspaceID, AgentID: agentID,
		})
		require.NoError(t, err)
		require.Equal(t, "gpt-4o", chain[0].Model)
		require.Equal(t, domain.AdapterOpenAI, chain[0].Adapter)
		require.Equal(t, "Cadangan", chain[1].Name)
	})

	t.Run("a Bolu may bring its own provider", func(t *testing.T) {
		h := newHarness(t, func(d *Deps) {
			d.Default = domain.ProviderConfig{
				Adapter:       domain.AdapterAnthropic,
				BaseURL:       "https://platform.example.test",
				Model:         "claude-3-5-haiku",
				APIKey:        "sk-platform",
				ContextTokens: 200000,
				Enabled:       true,
			}
		})
		h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return([]domain.StoredProvider{primary}, nil)
		h.agents.EXPECT().AgentModel(mock.Anything, scope.WorkspaceID, agentID).
			Return(domain.AgentOverride{Adapter: domain.AdapterAnthropic, Model: "claude-sonnet-4"}, nil)

		chain, err := h.service.Resolve(context.Background(), domain.Scope{
			UserID: scope.UserID, WorkspaceID: scope.WorkspaceID, AgentID: agentID,
		})
		require.NoError(t, err)
		require.Len(t, chain, 2, "the Bolu's own provider, then the workspace chain")
		require.Equal(t, "claude-sonnet-4", chain[0].Model)
		require.Equal(t, "sk-platform", chain[0].APIKey, "the platform key of the same adapter is reused")
		require.Equal(t, 200000, chain[0].ContextTokens)
		require.Equal(t, "Utama", chain[1].Name)
	})

	t.Run("a stale pin falls back to the workspace order", func(t *testing.T) {
		h := newHarness(t)
		h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return([]domain.StoredProvider{primary}, nil)
		h.agents.EXPECT().AgentModel(mock.Anything, scope.WorkspaceID, agentID).
			Return(domain.AgentOverride{ProviderID: uuid.New()}, nil)

		chain, err := h.service.Resolve(context.Background(), domain.Scope{
			UserID: scope.UserID, WorkspaceID: scope.WorkspaceID, AgentID: agentID,
		})
		require.NoError(t, err)
		require.Len(t, chain, 1)
		require.Equal(t, primary.ID, chain[0].ID)
	})

	t.Run("nothing configured is an error the UI can act on", func(t *testing.T) {
		h := newHarness(t)
		h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return(nil, nil)

		_, err := h.service.Resolve(context.Background(), scope)
		require.ErrorIs(t, err, domain.ErrNoProvider)
	})

	t.Run("a workspace keeps its own providers to itself", func(t *testing.T) {
		h := newHarness(t, func(d *Deps) { d.Default = platform })
		h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return([]domain.StoredProvider{primary}, nil)

		chain, err := h.service.Resolve(context.Background(), scope)
		require.NoError(t, err)
		require.Len(t, chain, 1, "the platform key is not spent behind a configured workspace")
		require.Equal(t, primary.ID, chain[0].ID)
	})
}

// TestResolveCapsTheChain: an outage must not turn into an unbounded chain of
// round trips.
func TestResolveCapsTheChain(t *testing.T) {
	scope := workspaceScope()
	providers := make([]domain.StoredProvider, 0, 5)
	for i := range 5 {
		providers = append(providers, storedProvider(func(p *domain.StoredProvider) {
			p.WorkspaceID = scope.WorkspaceID
			p.ID = uuid.New()
			p.Name = string(rune('a' + i))
			p.Priority = i
		}))
	}

	h := newHarness(t, func(d *Deps) { d.Settings.ChainLimit = 2 })
	h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return(providers, nil)

	chain, err := h.service.Resolve(context.Background(), scope)
	require.NoError(t, err)
	require.Len(t, chain, 2)
}

// TestAgentOverrideRoundTrip: the override is stored through the agent store,
// which is the only place that knows the encryption.
func TestSetAgentModelValidatesThePin(t *testing.T) {
	h := newHarness(t)
	scope := workspaceScope()
	scope.AgentID = uuid.New()

	h.store.EXPECT().Get(mock.Anything, scope.WorkspaceID, mock.Anything).Return(domain.StoredProvider{}, domain.ErrProviderNotFound)

	err := h.service.SetAgentModel(context.Background(), scope, domain.AgentOverride{ProviderID: uuid.New()})
	require.ErrorIs(t, err, domain.ErrProviderNotFound)
}

func TestAgentModelRequiresAnAgent(t *testing.T) {
	h := newHarness(t)
	scope := workspaceScope()

	_, err := h.service.AgentModel(context.Background(), scope)
	require.ErrorIs(t, err, domain.ErrInvalidRequest)
	require.ErrorContains(t, err, "agent is required")
}

// TestUsageWindow covers the report window, including its bound.
func TestUsageWindow(t *testing.T) {
	h := newHarness(t)
	scope := workspaceScope()
	fixed := time.Date(2026, 10, 10, 15, 4, 5, 0, Jakarta)
	h.service.SetClock(func() time.Time { return fixed })

	var since time.Time
	h.reader.EXPECT().Daily(mock.Anything, scope.WorkspaceID, mock.Anything).
		RunAndReturn(func(_ context.Context, _ uuid.UUID, from time.Time) ([]domain.UsageDaily, error) {
			since = from
			return nil, nil
		})

	_, err := h.service.Usage(context.Background(), scope, 0)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 9, 11, 0, 0, 0, 0, Jakarta), since, "30 days ending today")

	_, err = h.service.Usage(context.Background(), scope, 1)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 10, 10, 0, 0, 0, 0, Jakarta), since)

	_, err = h.service.Usage(context.Background(), scope, 5000)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 10, 10, 0, 0, 0, 0, Jakarta).AddDate(0, 0, -(MaxUsageDays-1)), since)
}
