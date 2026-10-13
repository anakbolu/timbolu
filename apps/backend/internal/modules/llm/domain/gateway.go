package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Adapter names, used in configuration and in the usage ledger.
const (
	AdapterOpenAI    = "openai"
	AdapterAnthropic = "anthropic"
)

// KnownAdapters lists the adapters this gateway understands. The settings screen
// offers exactly these, and the migration's check constraint matches, so the
// three cannot drift apart.
var KnownAdapters = []string{AdapterOpenAI, AdapterAnthropic}

// ProviderConfig is one configured route to a model. It is what a workspace
// stores, and what an agent may override.
type ProviderConfig struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	// Name is the label the user sees, e.g. "OpenRouter (utama)".
	Name string
	// Adapter selects the wire format: openai or anthropic.
	Adapter string
	// BaseURL is the endpoint root; the adapter appends its path.
	BaseURL string
	// Model is the model name the provider expects.
	Model string
	// APIKey is the decrypted secret. It never leaves the backend: handlers and
	// logs only ever see whether one is set.
	APIKey string
	// Priority orders the fallback chain: lower goes first.
	Priority int
	// MaxTokens caps the answer length; zero means the adapter default.
	MaxTokens int
	// IsDefault marks the provider a workspace uses when an agent has no
	// override and no priority order is configured.
	IsDefault bool
	// ContextTokens is the model's context window, used for the pre-flight check.
	ContextTokens int
	Enabled       bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Redacted renders the configuration for an API response: the key is replaced
// by whether one is stored.
type Redacted struct {
	ID            uuid.UUID `json:"id"`
	Name          string    `json:"name"`
	Adapter       string    `json:"adapter"`
	BaseURL       string    `json:"base_url"`
	Model         string    `json:"model"`
	Priority      int       `json:"priority"`
	MaxTokens     int       `json:"max_tokens"`
	IsDefault     bool      `json:"is_default"`
	ContextTokens int       `json:"context_tokens"`
	Enabled       bool      `json:"enabled"`
	HasAPIKey     bool      `json:"has_api_key"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Redact hides the secret.
func (c ProviderConfig) Redact() Redacted {
	return Redacted{
		ID:            c.ID,
		Name:          c.Name,
		Adapter:       c.Adapter,
		BaseURL:       c.BaseURL,
		Model:         c.Model,
		Priority:      c.Priority,
		MaxTokens:     c.MaxTokens,
		IsDefault:     c.IsDefault,
		ContextTokens: c.ContextTokens,
		Enabled:       c.Enabled,
		HasAPIKey:     c.APIKey != "",
		CreatedAt:     c.CreatedAt,
		UpdatedAt:     c.UpdatedAt,
	}
}

// AgentOverride is the per-Bolu part of a configuration: which model it uses and
// which provider it prefers. It mirrors agents.default_model.
type AgentOverride struct {
	// ProviderID pins the workspace provider this Bolu must use.
	ProviderID uuid.UUID `json:"provider_id,omitempty"`
	// Adapter and BaseURL describe a provider that exists only for this Bolu.
	Adapter string `json:"adapter,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
	Model   string `json:"model,omitempty"`
	// APIKey is a per-agent key, stored encrypted in the same column.
	APIKey string `json:"api_key,omitempty"`
	// MaxTokens caps this Bolu's answers.
	MaxTokens int `json:"max_tokens,omitempty"`
}

// IsZero reports whether the override says nothing, which means "use the
// workspace configuration".
func (o AgentOverride) IsZero() bool {
	return o.ProviderID == uuid.Nil && o.Adapter == "" && o.Model == ""
}

// Factory builds a ready-to-call adapter for one configuration. The container
// supplies it, so this module never learns which adapters exist and adding a
// provider stays a container change.
type Factory interface {
	Build(cfg ProviderConfig) (Provider, error)
}

// Resolver turns a workspace and an agent into the ordered list of providers to
// try: the primary first, then the fallbacks.
type Resolver interface {
	// Resolve returns at least one provider, or ErrNoProvider.
	Resolve(ctx context.Context, scope Scope) ([]ProviderConfig, error)
	// Provider builds a ready-to-call adapter for one configuration.
	Provider(cfg ProviderConfig) (Provider, error)
	// Default is the platform-level configuration, used when a workspace has
	// none. It may be zero, in which case a workspace without providers cannot
	// call a model.
	Default() ProviderConfig
}

// Scope is the tenant identity a resolution runs as.
type Scope struct {
	UserID      uuid.UUID
	WorkspaceID uuid.UUID
	// AgentID narrows the resolution to one Bolu's override.
	AgentID uuid.UUID
}

// IsZero reports whether the scope carries no identity.
func (s Scope) IsZero() bool { return s.WorkspaceID == uuid.Nil }

// Embedder is the embedding half of the gateway.
//
// It is declared on its own so a caller that only embeds — the memory and
// knowledge pipeline — does not depend on chat, provider configuration, and the
// usage report to send a string and get a vector back.
type Embedder interface {
	// Embed turns text into vectors, walking the fallback chain the same way a
	// completion does.
	Embed(ctx context.Context, scope Scope, req EmbedRequest) (EmbedResponse, error)
}

// Gateway is what the agent worker calls.
type Gateway interface {
	// Embedder is part of the gateway: the same chain, the same ledger.
	Embedder
	// Chat runs one completion, falling back when a provider fails in a way that
	// another provider might survive.
	Chat(ctx context.Context, scope Scope, req ChatRequest) (ChatResponse, error)
	// Stream runs one completion and reports progress, with the same fallback
	// rule applied before the first byte arrives.
	Stream(ctx context.Context, scope Scope, req ChatRequest) (<-chan StreamEvent, error)
	// Providers lists the workspace configuration, redacted.
	Providers(ctx context.Context, scope Scope) ([]Redacted, error)
	// Upsert stores one provider configuration.
	Upsert(ctx context.Context, scope Scope, req UpsertRequest) (Redacted, error)
	// Delete removes one configuration.
	Delete(ctx context.Context, scope Scope, id uuid.UUID) error
	// Test calls the provider with a one-line prompt and reports its
	// capabilities, which is what the settings screen uses to prove a key works.
	Test(ctx context.Context, scope Scope, id uuid.UUID) (Capabilities, error)
	// SetAgentModel stores a Bolu's override.
	SetAgentModel(ctx context.Context, scope Scope, override AgentOverride) error
	// AgentModel reads a Bolu's override.
	AgentModel(ctx context.Context, scope Scope) (AgentOverride, error)
	// Usage returns the aggregated spend of the workspace, which the model
	// screen shows next to the quota.
	Usage(ctx context.Context, scope Scope, days int) ([]UsageDaily, error)
}

// StoredProvider is a configuration as it lives in the database: the key is
// ciphertext, which only the service can open.
type StoredProvider struct {
	ProviderConfig
	// EncryptedKey is the sealed API key. Nil after a read means no key is
	// stored; nil on an update means "keep the stored key", so an edit that only
	// renames a provider cannot wipe the secret.
	EncryptedKey []byte
}

// Redact renders a stored configuration for an API response. The stored row does
// not carry the plaintext key, so the presence of the ciphertext is what decides
// HasAPIKey.
func (s StoredProvider) Redact() Redacted {
	redacted := s.ProviderConfig.Redact()
	redacted.HasAPIKey = len(s.EncryptedKey) > 0
	return redacted
}

// UpsertRequest is one provider write.
type UpsertRequest struct {
	ProviderConfig
	// ClearKey removes the stored API key. An empty APIKey means "keep the
	// stored secret", because the client never receives it and therefore cannot
	// send it back.
	ClearKey bool
}

// Store persists provider configurations. It never sees a plaintext key: the
// service encrypts before writing and decrypts after reading, so the secret
// exists in clear only inside the process that uses it.
type Store interface {
	List(ctx context.Context, workspaceID uuid.UUID) ([]StoredProvider, error)
	Get(ctx context.Context, workspaceID, id uuid.UUID) (StoredProvider, error)
	// Create inserts a new configuration. A default clears the previous default
	// in the same transaction, so the single-default invariant holds.
	Create(ctx context.Context, stored StoredProvider) (StoredProvider, error)
	// Update replaces an existing configuration.
	Update(ctx context.Context, stored StoredProvider) (StoredProvider, error)
	Delete(ctx context.Context, workspaceID, id uuid.UUID) error
}

// AgentStore reads and writes one Bolu's model override.
type AgentStore interface {
	AgentModel(ctx context.Context, workspaceID, agentID uuid.UUID) (AgentOverride, error)
	SetAgentModel(ctx context.Context, workspaceID, agentID uuid.UUID, override AgentOverride) error
}

// SecretBox encrypts and decrypts API keys at rest. EPIC 8 (#66) replaces the
// implementation with per-workspace envelope encryption under KMS; the contract
// stays the same.
type SecretBox interface {
	Encrypt(plaintext string) ([]byte, error)
	Decrypt(ciphertext []byte) (string, error)
}

// CostTable turns token counts into a cost. Prices are per million tokens, in
// micro-rupiah, so a cheap call is still a whole number.
type CostTable interface {
	Cost(model string, usage Usage) int64
}
