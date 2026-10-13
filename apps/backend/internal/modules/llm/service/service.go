// Package service implements the LLM gateway use cases: which provider a
// workspace calls, in which order, what a call cost, and how much of the token
// allowance is left.
//
// Two rules run through the package:
//   - the secret never leaves this package in clear text: it is encrypted before
//     it reaches the store and decrypted only to build a provider;
//   - every call that reaches a provider is recorded in the usage ledger, so the
//     cost report and the quota check agree with the bill.
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
)

// NameMaxLength bounds the provider label. It is shown in the settings screen
// and in the fallback list, so it must stay short enough to render.
const NameMaxLength = 80

// MaxPriority bounds the fallback order. Anything above it is a typo rather than
// a deliberate position 9000 in the chain.
const MaxPriority = 1000

// DefaultUsageDays is the window the usage report covers when the caller does
// not choose one.
const DefaultUsageDays = 30

// MaxUsageDays bounds the report window, so one request cannot ask for years of
// aggregation.
const MaxUsageDays = 366

// ChainLimit is how many providers one request tries before it gives up.
const ChainLimit = 3

// Config holds the service settings.
type Config struct {
	// QuotaEnabled turns the pre-flight token check on. Local development runs
	// with it off, so a fresh workspace can call a model before any package
	// exists; staging and production run with it on.
	QuotaEnabled bool
	// ChainLimit caps the number of providers one request tries. Zero means
	// ChainLimit.
	ChainLimit int
}

// Deps are the service dependencies.
type Deps struct {
	Store   domain.Store
	Agents  domain.AgentStore
	Factory domain.Factory
	// Box encrypts the API keys at rest. Nil means this deployment cannot store
	// a secret, and Upsert then refuses to accept one instead of writing it in
	// clear text.
	Box domain.SecretBox
	// Recorder writes the usage ledger. Required: a gateway that cannot record
	// what it spent is not one this product can bill.
	Recorder domain.UsageRecorder
	// Reader reads the aggregated usage and the durable token total.
	Reader domain.UsageReader
	// Counter is the live token spend; Quota is the allowance. Both are required
	// only when QuotaEnabled is set.
	Counter domain.QuotaCounter
	Quota   domain.QuotaPolicy
	Costs   domain.CostTable
	Default domain.ProviderConfig
	// EmbeddingModel is the model the platform embeds with. It is a deployment
	// fact rather than a workspace setting: the vectors have to fit the schema,
	// and the workspace supplies only the key. Empty means a provider's own
	// configured model is used, which only works when that model embeds.
	EmbeddingModel string
	Settings       Config
}

// Service is the gateway implementation. It satisfies the domain's Gateway, the
// Resolver, and the QuotaChecker.
type Service struct {
	deps     Deps
	settings Config
	// clock is the time source, so the usage window and the quota period are
	// testable across a day and a month boundary.
	clock func() time.Time
}

// New builds the service.
func New(deps Deps) *Service {
	settings := deps.Settings
	if settings.ChainLimit <= 0 {
		settings.ChainLimit = ChainLimit
	}
	return &Service{deps: deps, settings: settings, clock: time.Now}
}

// Jakarta is where the customers are, so their day and month boundaries are the
// ones the usage report and the quota follow.
var Jakarta = func() *time.Location {
	location, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		// A container without the time zone database: UTC+7 has no daylight
		// saving, so the fixed zone is exact.
		return time.FixedZone("WIB", 7*60*60)
	}
	return location
}()

// SetClock replaces the time source, which the tests use to cross a day or a
// month boundary without waiting.
func (s *Service) SetClock(clock func() time.Time) { s.clock = clock }

func (s *Service) now() time.Time {
	if s == nil || s.clock == nil {
		return time.Now()
	}
	return s.clock()
}

// Providers lists the workspace configuration, redacted.
func (s *Service) Providers(ctx context.Context, scope domain.Scope) ([]domain.Redacted, error) {
	if err := requireWorkspace(scope); err != nil {
		return nil, err
	}

	stored, err := s.deps.Store.List(ctx, scope.WorkspaceID)
	if err != nil {
		return nil, err
	}

	providers := make([]domain.Redacted, 0, len(stored))
	for _, provider := range stored {
		providers = append(providers, provider.Redact())
	}
	return providers, nil
}

// Upsert stores one provider configuration. The first provider of a workspace
// becomes its default, so a workspace is never left without one.
func (s *Service) Upsert(ctx context.Context, scope domain.Scope, req domain.UpsertRequest) (domain.Redacted, error) {
	if err := requireWorkspace(scope); err != nil {
		return domain.Redacted{}, err
	}

	cfg := req.ProviderConfig
	cfg.WorkspaceID = scope.WorkspaceID
	cfg.Name = strings.TrimSpace(cfg.Name)
	cfg.Adapter = strings.TrimSpace(cfg.Adapter)
	cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
	cfg.Model = strings.TrimSpace(cfg.Model)

	if err := s.validate(cfg); err != nil {
		return domain.Redacted{}, err
	}

	stored, err := s.buildStored(cfg, req.ClearKey)
	if err != nil {
		return domain.Redacted{}, err
	}

	var saved domain.StoredProvider
	if cfg.ID == uuid.Nil {
		// The first provider of a workspace is its default: without one the
		// fallback chain would have no head.
		if !stored.IsDefault {
			existing, err := s.deps.Store.List(ctx, scope.WorkspaceID)
			if err != nil {
				return domain.Redacted{}, err
			}
			stored.IsDefault = len(existing) == 0
		}
		saved, err = s.deps.Store.Create(ctx, stored)
	} else {
		saved, err = s.deps.Store.Update(ctx, stored)
	}
	if err != nil {
		return domain.Redacted{}, err
	}

	return saved.Redact(), nil
}

// Delete removes one configuration.
func (s *Service) Delete(ctx context.Context, scope domain.Scope, id uuid.UUID) error {
	if err := requireWorkspace(scope); err != nil {
		return err
	}
	if id == uuid.Nil {
		return fmt.Errorf("%w: provider id is required", domain.ErrInvalidRequest)
	}

	return s.deps.Store.Delete(ctx, scope.WorkspaceID, id)
}

// Test calls the provider once and reports what it can do, which is how the
// settings screen proves that a key works before a Bolu depends on it.
func (s *Service) Test(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Capabilities, error) {
	if err := requireWorkspace(scope); err != nil {
		return domain.Capabilities{}, err
	}

	stored, err := s.deps.Store.Get(ctx, scope.WorkspaceID, id)
	if err != nil {
		return domain.Capabilities{}, err
	}

	cfg, err := s.open(stored)
	if err != nil {
		return domain.Capabilities{}, err
	}

	provider, err := s.deps.Factory.Build(cfg)
	if err != nil {
		return domain.Capabilities{}, err
	}

	// The prompt is the smallest one that proves the round trip: one short
	// answer, no tools. The call is billed by the provider, so it is recorded
	// like any other.
	response, err := provider.Chat(ctx, domain.ChatRequest{
		Messages:  []domain.Message{{Role: domain.RoleUser, Text: "Balas satu kata: siap"}},
		MaxTokens: 16,
		Metadata: domain.RequestMetadata{
			WorkspaceID: scope.WorkspaceID,
			Purpose:     domain.PurposeRouting,
		},
	})
	if err != nil {
		return domain.Capabilities{}, err
	}

	if err := s.record(ctx, scope, cfg, response, domain.RequestMetadata{Purpose: domain.PurposeRouting}); err != nil {
		return domain.Capabilities{}, err
	}

	return provider.Capabilities(), nil
}

// AgentModel reads the model override of one Bolu.
func (s *Service) AgentModel(ctx context.Context, scope domain.Scope) (domain.AgentOverride, error) {
	if err := requireAgent(scope); err != nil {
		return domain.AgentOverride{}, err
	}

	return s.deps.Agents.AgentModel(ctx, scope.WorkspaceID, scope.AgentID)
}

// SetAgentModel stores the model override of one Bolu. The provider it points at
// must exist in the same workspace, so a stale id cannot silently fall back.
func (s *Service) SetAgentModel(ctx context.Context, scope domain.Scope, override domain.AgentOverride) error {
	if err := requireAgent(scope); err != nil {
		return err
	}

	override.Adapter = strings.TrimSpace(override.Adapter)
	override.BaseURL = strings.TrimSpace(override.BaseURL)
	override.Model = strings.TrimSpace(override.Model)

	if override.Adapter != "" {
		// Validate the pair the same way a stored provider is validated, so an
		// unknown adapter is rejected here rather than at the first task.
		if _, err := s.deps.Factory.Build(domain.ProviderConfig{
			Adapter: override.Adapter,
			BaseURL: override.BaseURL,
			Model:   override.Model,
			APIKey:  override.APIKey,
		}); err != nil {
			return err
		}
	}
	if override.ProviderID != uuid.Nil {
		if _, err := s.deps.Store.Get(ctx, scope.WorkspaceID, override.ProviderID); err != nil {
			return err
		}
	}

	return s.deps.Agents.SetAgentModel(ctx, scope.WorkspaceID, scope.AgentID, override)
}

// Usage returns the aggregated spend of the workspace, most recent day first.
func (s *Service) Usage(ctx context.Context, scope domain.Scope, days int) ([]domain.UsageDaily, error) {
	if err := requireWorkspace(scope); err != nil {
		return nil, err
	}

	if days <= 0 {
		days = DefaultUsageDays
	}
	if days > MaxUsageDays {
		days = MaxUsageDays
	}

	// The report is read in Jakarta days, so "today" matches the day the customer
	// is living in rather than the day the server is living in.
	today := s.now().In(Jakarta)
	since := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, Jakarta).
		AddDate(0, 0, -(days - 1))

	return s.deps.Reader.Daily(ctx, scope.WorkspaceID, since)
}

func (s *Service) validate(cfg domain.ProviderConfig) error {
	if cfg.Model == "" {
		return fmt.Errorf("%w: model is required", domain.ErrInvalidRequest)
	}
	if cfg.Name == "" {
		return fmt.Errorf("%w: name is required", domain.ErrInvalidRequest)
	}
	if len([]rune(cfg.Name)) > NameMaxLength {
		return fmt.Errorf("%w: name is longer than %d characters", domain.ErrInvalidRequest, NameMaxLength)
	}
	if cfg.Priority < 0 || cfg.Priority > MaxPriority {
		return fmt.Errorf("%w: priority must be between 0 and %d", domain.ErrInvalidRequest, MaxPriority)
	}
	if cfg.MaxTokens < 0 || cfg.ContextTokens < 0 {
		return fmt.Errorf("%w: token limits cannot be negative", domain.ErrInvalidRequest)
	}
	if cfg.APIKey != "" && s.deps.Box == nil {
		// Refusing is the only safe answer: storing the key in clear text is
		// worse than asking the operator to configure a key.
		return fmt.Errorf("%w: no encryption key is configured, so an API key cannot be stored", domain.ErrInvalidRequest)
	}

	// Building the adapter is the check that the adapter name and the model are
	// ones this deployment can serve. It costs a struct, no network call.
	if _, err := s.deps.Factory.Build(cfg); err != nil {
		return err
	}
	return nil
}

// buildStored encrypts the key and shapes the row to write.
//
// The three states of the key travel through EncryptedKey: nil keeps the stored
// secret, an empty slice clears it, and a sealed value replaces it.
func (s *Service) buildStored(cfg domain.ProviderConfig, clearKey bool) (domain.StoredProvider, error) {
	stored := domain.StoredProvider{ProviderConfig: cfg}

	switch {
	case clearKey:
		stored.EncryptedKey = []byte{}
	case cfg.APIKey != "":
		sealed, err := s.deps.Box.Encrypt(cfg.APIKey)
		if err != nil {
			return domain.StoredProvider{}, fmt.Errorf("llm: encrypt api key: %w", err)
		}
		stored.EncryptedKey = sealed
	}

	// The plaintext key is never part of the row.
	stored.APIKey = ""
	return stored, nil
}

// open decrypts a stored provider into a usable configuration.
func (s *Service) open(stored domain.StoredProvider) (domain.ProviderConfig, error) {
	cfg := stored.ProviderConfig
	if len(stored.EncryptedKey) == 0 {
		return cfg, nil
	}
	if s.deps.Box == nil {
		return domain.ProviderConfig{}, fmt.Errorf("%w: no encryption key is configured", domain.ErrInvalidRequest)
	}

	key, err := s.deps.Box.Decrypt(stored.EncryptedKey)
	if err != nil {
		return domain.ProviderConfig{}, fmt.Errorf("llm: decrypt api key of %q: %w", stored.Name, err)
	}
	cfg.APIKey = key
	return cfg, nil
}

func requireWorkspace(scope domain.Scope) error {
	if scope.WorkspaceID == uuid.Nil {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidRequest)
	}
	return nil
}

func requireAgent(scope domain.Scope) error {
	if err := requireWorkspace(scope); err != nil {
		return err
	}
	if scope.AgentID == uuid.Nil {
		return fmt.Errorf("%w: agent is required", domain.ErrInvalidRequest)
	}
	return nil
}

// tryNext reports whether the next provider in the chain is worth trying. A
// malformed request or a missing configuration fails everywhere; a rate limit,
// an outage, or a capability the model lacks may not.
func tryNext(err error) bool {
	return domain.Retryable(err) ||
		errors.Is(err, domain.ErrUnsupported) ||
		errors.Is(err, domain.ErrContextTooLong)
}

// compile-time checks that the service satisfies every port it is wired to.
var (
	_ domain.Gateway      = (*Service)(nil)
	_ domain.Resolver     = (*Service)(nil)
	_ domain.QuotaChecker = (*Service)(nil)
)
