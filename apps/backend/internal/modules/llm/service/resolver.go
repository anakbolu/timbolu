package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
)

// Resolve returns the ordered list of providers to try, which is the fallback
// chain: what the Bolu asked for, then the workspace configuration, then the
// platform default.
//
// The order is data: `is_default` and `priority` come from the settings screen,
// and the Bolu override only ever moves one provider to the front or changes the
// model. Nothing about the chain is decided here.
func (s *Service) Resolve(ctx context.Context, scope domain.Scope) ([]domain.ProviderConfig, error) {
	if err := requireWorkspace(scope); err != nil {
		return nil, err
	}

	stored, err := s.deps.Store.List(ctx, scope.WorkspaceID)
	if err != nil {
		return nil, err
	}

	workspace := make([]domain.ProviderConfig, 0, len(stored))
	byID := make(map[uuid.UUID]domain.ProviderConfig, len(stored))
	for _, provider := range stored {
		if !provider.Enabled {
			continue
		}

		cfg, err := s.open(provider)
		if err != nil {
			return nil, err
		}
		workspace = append(workspace, cfg)
		byID[cfg.ID] = cfg
	}

	var override domain.AgentOverride
	if scope.AgentID != uuid.Nil {
		override, err = s.deps.Agents.AgentModel(ctx, scope.WorkspaceID, scope.AgentID)
		if err != nil {
			return nil, err
		}
	}

	chain := s.chain(scope.WorkspaceID, workspace, byID, override)
	if len(chain) == 0 {
		return nil, fmt.Errorf("%w: atur penyedia di Pengaturan → Model", domain.ErrNoProvider)
	}
	if len(chain) > s.settings.ChainLimit {
		// A long tail costs a round trip each before the caller gets an answer;
		// three providers is already generous for a provider outage.
		chain = chain[:s.settings.ChainLimit]
	}

	return chain, nil
}

// Default is the platform-level configuration, used when a workspace configured
// nothing of its own.
func (s *Service) Default() domain.ProviderConfig { return s.deps.Default }

// Provider builds a ready-to-call adapter for one configuration. It is the same
// factory the request path uses, so a caller that resolves a chain can build the
// entry it wants without knowing which adapters exist.
func (s *Service) Provider(cfg domain.ProviderConfig) (domain.Provider, error) {
	return s.deps.Factory.Build(cfg)
}

// chain applies the Bolu override to the workspace chain.
func (s *Service) chain(workspaceID uuid.UUID, workspace []domain.ProviderConfig, byID map[uuid.UUID]domain.ProviderConfig, override domain.AgentOverride) []domain.ProviderConfig {
	base := s.withPlatformDefault(workspace)

	switch {
	case override.Adapter != "" && override.Model != "":
		// A provider that exists only for this Bolu: the workspace chain stays
		// behind it as the fallback.
		return append([]domain.ProviderConfig{s.standalone(workspaceID, override)}, base...)

	case override.ProviderID != uuid.Nil:
		if pinned, ok := byID[override.ProviderID]; ok {
			if override.MaxTokens > 0 {
				pinned.MaxTokens = override.MaxTokens
			}
			return append([]domain.ProviderConfig{pinned}, withoutID(base, pinned.ID)...)
		}
		// A pinned provider that was deleted or disabled is not an error the
		// task should die of: the chain falls back to the workspace order, and
		// the settings screen shows the stale pick.
		return base

	case override.Model != "" && len(base) > 0:
		// Only the model differs: same endpoint, cheaper or larger model.
		head := base[0]
		head.Model = override.Model
		if override.MaxTokens > 0 {
			head.MaxTokens = override.MaxTokens
		}
		return append([]domain.ProviderConfig{head}, base[1:]...)
	}

	return base
}

// standalone turns a Bolu's own configuration into a provider. The key and the
// context window come from the override when it names them, and from the
// platform default when it shares the adapter.
func (s *Service) standalone(workspaceID uuid.UUID, override domain.AgentOverride) domain.ProviderConfig {
	cfg := domain.ProviderConfig{
		WorkspaceID: workspaceID,
		Name:        "Bolu",
		Adapter:     override.Adapter,
		BaseURL:     override.BaseURL,
		Model:       override.Model,
		APIKey:      override.APIKey,
		MaxTokens:   override.MaxTokens,
		Enabled:     true,
	}

	if s.deps.Default.Adapter == override.Adapter {
		if cfg.APIKey == "" {
			cfg.APIKey = s.deps.Default.APIKey
		}
		if cfg.BaseURL == "" {
			cfg.BaseURL = s.deps.Default.BaseURL
		}
		if cfg.MaxTokens == 0 {
			cfg.MaxTokens = s.deps.Default.MaxTokens
		}
		cfg.ContextTokens = s.deps.Default.ContextTokens
	}

	return cfg
}

// withPlatformDefault appends the platform-level provider, which is what a
// workspace that configured nothing of its own calls.
//
// It is deliberately not appended behind a workspace's own providers: a
// workspace that chose its providers must not quietly spend the platform's key
// as a last resort.
func (s *Service) withPlatformDefault(workspace []domain.ProviderConfig) []domain.ProviderConfig {
	if len(workspace) > 0 {
		return workspace
	}

	def := s.deps.Default
	if def.Adapter == "" || def.Model == "" {
		return workspace
	}
	return []domain.ProviderConfig{def}
}

func withoutID(providers []domain.ProviderConfig, id uuid.UUID) []domain.ProviderConfig {
	remaining := make([]domain.ProviderConfig, 0, len(providers))
	for _, cfg := range providers {
		if cfg.ID == id {
			continue
		}
		remaining = append(remaining, cfg)
	}
	return remaining
}
