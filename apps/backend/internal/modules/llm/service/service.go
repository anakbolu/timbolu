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
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
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
	// Auditor records model configuration changes in the workspace's trail. Optional:
	// unwired means the change happens and leaves no line, which is what a deployment
	// without the approval module runs with.
	Auditor domain.Auditor
	// Logger reports a trail that could not be written. Optional.
	Logger *zap.Logger
	// Tracer starts the spans of a model call. Optional: without it the calls run
	// untraced, which is what a deployment with telemetry off does.
	Tracer trace.Tracer
	// Metrics reports what a call was and how long it took. Optional: without it the calls
	// happen and nothing is charted.
	Metrics  domain.Metrics
	Settings Config
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

	redacted := saved.Redact()
	action := "provider.created"
	if cfg.ID != uuid.Nil {
		action = "provider.updated"
	}
	// The line is built from the redacted shape, so the key cannot reach it even by
	// accident: what is recorded is what a screen shows.
	s.audit(ctx, scope, domain.AuditEntry{
		Action:      action,
		ObjectType:  "provider",
		ObjectID:    redacted.ID.String(),
		ActorUserID: scope.UserID,
		Metadata: map[string]any{
			"name":        redacted.Name,
			"adapter":     redacted.Adapter,
			"model":       redacted.Model,
			"has_api_key": redacted.HasAPIKey,
			"is_default":  redacted.IsDefault,
		},
	})

	return redacted, nil
}

// audit records one entry. A trail that could not be written is reported rather than
// failing the change: the configuration already changed, and refusing it afterwards would
// leave the workspace with a provider nobody can see.
func (s *Service) audit(ctx context.Context, scope domain.Scope, entry domain.AuditEntry) {
	if s.deps.Auditor == nil {
		return
	}
	if err := s.deps.Auditor.Record(ctx, scope, entry); err != nil {
		s.deps.Logger.Warn("could not record a model configuration audit entry",
			zap.String("action", entry.Action), zap.Error(err))
	}
}

// Delete removes one configuration.
func (s *Service) Delete(ctx context.Context, scope domain.Scope, id uuid.UUID) error {
	if err := requireWorkspace(scope); err != nil {
		return err
	}
	if id == uuid.Nil {
		return fmt.Errorf("%w: provider id is required", domain.ErrInvalidRequest)
	}

	// The name is read before the row goes, because after it the trail can only say that a
	// provider was removed — not which one. Read only when something will record it, so a
	// deployment with no trail does not pay a query for a line nobody writes.
	name := ""
	if s.deps.Auditor != nil {
		if providers, err := s.deps.Store.List(ctx, scope.WorkspaceID); err == nil {
			for _, provider := range providers {
				if provider.ID == id {
					name = provider.Name
					break
				}
			}
		}
	}

	if err := s.deps.Store.Delete(ctx, scope.WorkspaceID, id); err != nil {
		return err
	}

	s.audit(ctx, scope, domain.AuditEntry{
		Action:      "provider.deleted",
		ObjectType:  "provider",
		ObjectID:    id.String(),
		ActorUserID: scope.UserID,
		Metadata:    map[string]any{"name": name},
	})

	return nil
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

	if err := s.deps.Agents.SetAgentModel(ctx, scope.WorkspaceID, scope.AgentID, override); err != nil {
		return err
	}

	// Which model a Bolu thinks with is a cost decision, so the change is recorded. The
	// entry names the shape of the override rather than a key: a per-Bolu override may
	// carry its own endpoint and key, and neither belongs in a trail.
	s.audit(ctx, scope, domain.AuditEntry{
		Action:      "agent_model.changed",
		ObjectType:  "agent",
		ObjectID:    scope.AgentID.String(),
		ActorUserID: scope.UserID,
		Metadata: map[string]any{
			"adapter":     override.Adapter,
			"model":       override.Model,
			"provider_id": idOrEmpty(override.ProviderID),
			"has_own_key": len(override.APIKey) > 0,
			"max_tokens":  override.MaxTokens,
		},
	})

	return nil
}

// idOrEmpty renders an optional id, because a metadata field that carried the zero uuid
// would name a provider that is not there.
func idOrEmpty(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
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

// Report returns what the workspace spent over a window, read three ways.
//
// The window is expressed in days and resolved in Jakarta, exactly like the daily report, so the two
// screens cannot disagree about what "the last seven days" means. What differs is the granularity:
// the daily rows are whole days and this is the interval itself, which is what makes a total here
// comparable to a period's allowance rather than to a day's.
func (s *Service) Report(ctx context.Context, scope domain.Scope, days, limit int) (domain.UsageReport, error) {
	if err := requireWorkspace(scope); err != nil {
		return domain.UsageReport{}, err
	}

	if days <= 0 {
		days = DefaultUsageDays
	}
	if days > MaxUsageDays {
		days = MaxUsageDays
	}

	today := s.now().In(Jakarta)
	since := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, Jakarta).
		AddDate(0, 0, -(days - 1))

	report, err := s.deps.Reader.Report(ctx, scope.WorkspaceID, since, limit)
	if err != nil {
		return domain.UsageReport{}, err
	}

	// The moment is set here rather than in the repository: the window is this module's answer to
	// "the last seven days", and a storage layer that recomputed it could answer a different one.
	report.PeriodStartedAt = since

	return report, nil
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
