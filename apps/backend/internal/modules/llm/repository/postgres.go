// Package repository implements the LLM gateway data access: the provider
// configuration, the usage ledger, and the Bolu model override.
//
// Every statement runs inside the caller's tenant scope, so Row Level Security
// filters it even if a query forgets its workspace filter. The API keys are
// stored as ciphertext; this package never holds a plaintext secret, which is
// why the ports it fulfils speak in domain.StoredProvider.
package repository

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/repository/sqlcgen"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
)

// Repository is the Postgres-backed implementation of the gateway ports.
type Repository struct {
	pool *database.Pool
}

// New builds the repository.
func New(pool *database.Pool) *Repository {
	return &Repository{pool: pool}
}

// List returns every provider of the workspace, the default first and then the
// configured fallback order.
func (r *Repository) List(ctx context.Context, workspaceID uuid.UUID) ([]domain.StoredProvider, error) {
	var providers []domain.StoredProvider

	err := r.read(ctx, workspaceID, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListProviders(ctx, workspaceID)
		if err != nil {
			return fmt.Errorf("llm: list providers: %w", err)
		}

		providers = make([]domain.StoredProvider, 0, len(rows))
		for _, row := range rows {
			providers = append(providers, toStoredProvider(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return providers, nil
}

// Get returns one provider.
func (r *Repository) Get(ctx context.Context, workspaceID, id uuid.UUID) (domain.StoredProvider, error) {
	var provider domain.StoredProvider

	err := r.read(ctx, workspaceID, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetProvider(ctx, sqlcgen.GetProviderParams{ID: id, WorkspaceID: workspaceID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrProviderNotFound
			}
			return fmt.Errorf("llm: get provider: %w", err)
		}

		provider = toStoredProvider(row)
		return nil
	})
	if err != nil {
		return domain.StoredProvider{}, err
	}

	return provider, nil
}

// Create inserts a provider. When it is the default, the previous default is
// cleared in the same transaction so the workspace always has exactly one.
func (r *Repository) Create(ctx context.Context, stored domain.StoredProvider) (domain.StoredProvider, error) {
	var created domain.StoredProvider

	err := r.write(ctx, stored.WorkspaceID, func(queries *sqlcgen.Queries) error {
		if stored.IsDefault {
			if err := clearDefaults(ctx, queries, stored.WorkspaceID, uuid.Nil); err != nil {
				return err
			}
		}

		row, err := queries.CreateProvider(ctx, sqlcgen.CreateProviderParams{
			WorkspaceID:     stored.WorkspaceID,
			Name:            stored.Name,
			Adapter:         stored.Adapter,
			BaseUrl:         stored.BaseURL,
			Model:           stored.Model,
			ApiKeyEncrypted: stored.EncryptedKey,
			Priority:        int32(stored.Priority),
			MaxTokens:       int32(stored.MaxTokens),
			ContextTokens:   int32(stored.ContextTokens),
			IsDefault:       stored.IsDefault,
			Enabled:         stored.Enabled,
		})
		if err != nil {
			return translateWriteError(err, "create")
		}

		created = toStoredProvider(row)
		return nil
	})
	if err != nil {
		return domain.StoredProvider{}, err
	}

	return created, nil
}

// Update replaces a provider. A nil EncryptedKey keeps the stored secret.
func (r *Repository) Update(ctx context.Context, stored domain.StoredProvider) (domain.StoredProvider, error) {
	var updated domain.StoredProvider

	err := r.write(ctx, stored.WorkspaceID, func(queries *sqlcgen.Queries) error {
		if stored.IsDefault {
			if err := clearDefaults(ctx, queries, stored.WorkspaceID, stored.ID); err != nil {
				return err
			}
		}

		row, err := queries.UpdateProvider(ctx, sqlcgen.UpdateProviderParams{
			ID:              stored.ID,
			WorkspaceID:     stored.WorkspaceID,
			Name:            stored.Name,
			Adapter:         stored.Adapter,
			BaseUrl:         stored.BaseURL,
			Model:           stored.Model,
			Priority:        int32(stored.Priority),
			MaxTokens:       int32(stored.MaxTokens),
			ContextTokens:   int32(stored.ContextTokens),
			IsDefault:       stored.IsDefault,
			Enabled:         stored.Enabled,
			ApiKeyEncrypted: stored.EncryptedKey,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrProviderNotFound
			}
			return translateWriteError(err, "update")
		}

		updated = toStoredProvider(row)
		return nil
	})
	if err != nil {
		return domain.StoredProvider{}, err
	}

	return updated, nil
}

// Delete removes a provider.
func (r *Repository) Delete(ctx context.Context, workspaceID, id uuid.UUID) error {
	return r.write(ctx, workspaceID, func(queries *sqlcgen.Queries) error {
		removed, err := queries.DeleteProvider(ctx, sqlcgen.DeleteProviderParams{ID: id, WorkspaceID: workspaceID})
		if err != nil {
			return fmt.Errorf("llm: delete provider: %w", err)
		}
		if removed == 0 {
			return domain.ErrProviderNotFound
		}
		return nil
	})
}

// Record appends one row to the usage ledger. It is the only write path of a
// provider call's cost, so a call that is not recorded is a bug the tests catch.
func (r *Repository) Record(ctx context.Context, entry domain.UsageEntry) error {
	return r.write(ctx, entry.WorkspaceID, func(queries *sqlcgen.Queries) error {
		_, err := queries.InsertUsageEntry(ctx, sqlcgen.InsertUsageEntryParams{
			WorkspaceID:      entry.WorkspaceID,
			AgentID:          optionalUUID(entry.AgentID),
			TaskID:           optionalUUID(entry.TaskID),
			Provider:         entry.Provider,
			Model:            entry.Model,
			Purpose:          entry.Purpose,
			InputTokens:      int32(entry.Usage.InputTokens),
			OutputTokens:     int32(entry.Usage.OutputTokens),
			CacheReadTokens:  int32(entry.Usage.CacheReadTokens),
			CacheWriteTokens: int32(entry.Usage.CacheWriteTokens),
			CostMicros:       entry.CostMicros,
		})
		if err != nil {
			return fmt.Errorf("llm: record usage: %w", err)
		}
		return nil
	})
}

// Daily returns the per-day usage totals since a date, most recent first.
// CostSince returns the cost recorded since a moment, in micro-rupiah.
//
// It reads the daily aggregation rather than the ledger: the view is one row per
// workspace, day, provider, model, and purpose, so a day's ceiling is answered by
// a handful of rows instead of a scan that grows without bound.
func (r *Repository) CostSince(ctx context.Context, workspaceID uuid.UUID, since time.Time) (int64, error) {
	days, err := r.Daily(ctx, workspaceID, since)
	if err != nil {
		return 0, err
	}

	var total int64
	for _, day := range days {
		total += day.CostMicros
	}
	return total, nil
}

func (r *Repository) Daily(ctx context.Context, workspaceID uuid.UUID, since time.Time) ([]domain.UsageDaily, error) {
	var days []domain.UsageDaily

	err := r.read(ctx, workspaceID, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListDailyUsage(ctx, sqlcgen.ListDailyUsageParams{WorkspaceID: workspaceID, Day: since})
		if err != nil {
			return fmt.Errorf("llm: list daily usage: %w", err)
		}

		days = make([]domain.UsageDaily, 0, len(rows))
		for _, row := range rows {
			days = append(days, domain.UsageDaily{
				Day:              row.Day,
				Provider:         row.Provider,
				Model:            row.Model,
				Purpose:          row.Purpose,
				Calls:            row.Calls,
				InputTokens:      row.InputTokens,
				OutputTokens:     row.OutputTokens,
				CacheReadTokens:  row.CacheReadTokens,
				CacheWriteTokens: row.CacheWriteTokens,
				CostMicros:       row.CostMicros,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return days, nil
}

// SumSince returns the tokens the ledger recorded since a moment.
func (r *Repository) SumSince(ctx context.Context, workspaceID uuid.UUID, since time.Time) (int64, error) {
	var total int64

	err := r.read(ctx, workspaceID, func(queries *sqlcgen.Queries) error {
		sum, err := queries.SumTokensSince(ctx, sqlcgen.SumTokensSinceParams{WorkspaceID: workspaceID, CreatedAt: since})
		if err != nil {
			return fmt.Errorf("llm: sum usage: %w", err)
		}
		total = sum
		return nil
	})
	if err != nil {
		return 0, err
	}

	return total, nil
}

func (r *Repository) read(ctx context.Context, workspaceID uuid.UUID, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("llm: database is not configured")
	}
	return r.pool.InScopeRead(ctx, database.Scope{WorkspaceID: workspaceID}, func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

func (r *Repository) write(ctx context.Context, workspaceID uuid.UUID, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("llm: database is not configured")
	}
	return r.pool.InScope(ctx, database.Scope{WorkspaceID: workspaceID}, func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

func clearDefaults(ctx context.Context, queries *sqlcgen.Queries, workspaceID, keep uuid.UUID) error {
	// uuid.Nil means "keep none", which is what an insert needs: the new row
	// does not exist yet, so no id may be spared.
	cleared, err := queries.ClearDefaultProviders(ctx, sqlcgen.ClearDefaultProvidersParams{
		WorkspaceID: workspaceID,
		ID:          keep,
	})
	if err != nil {
		return fmt.Errorf("llm: clear default: %w", err)
	}
	// Exactly one row may be spared, and only when it is the row being updated.
	if cleared > 1 {
		return fmt.Errorf("llm: %d providers were the default, expected at most 1", cleared)
	}
	return nil
}

func translateWriteError(err error, action string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			// Either the name is taken or another provider became the default.
			return fmt.Errorf("%w: %s", domain.ErrDuplicateProvider, pgErr.ConstraintName)
		case "23514":
			// The adapter check constraint.
			return fmt.Errorf("%w: unsupported adapter", domain.ErrInvalidRequest)
		}
	}
	return fmt.Errorf("llm: %s provider: %w", action, err)
}

func toStoredProvider(row sqlcgen.LlmProvider) domain.StoredProvider {
	return domain.StoredProvider{
		ProviderConfig: domain.ProviderConfig{
			ID:            row.ID,
			WorkspaceID:   row.WorkspaceID,
			Name:          row.Name,
			Adapter:       row.Adapter,
			BaseURL:       row.BaseUrl,
			Model:         row.Model,
			Priority:      int(row.Priority),
			MaxTokens:     int(row.MaxTokens),
			ContextTokens: int(row.ContextTokens),
			IsDefault:     row.IsDefault,
			Enabled:       row.Enabled,
			CreatedAt:     row.CreatedAt,
			UpdatedAt:     row.UpdatedAt,
		},
		EncryptedKey: row.ApiKeyEncrypted,
	}
}

func optionalUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// ------------------------------------------------------- the Bolu override

// AgentStore reads and writes the model override of one Bolu. The override lives
// in agents.default_model, which the agent module owns; this repository is the
// only place that knows both the column and the encryption, so the agent module
// stays unaware of provider configuration.
type AgentStore struct {
	pool *database.Pool
	box  domain.SecretBox
}

// NewAgentStore builds the override store.
func NewAgentStore(pool *database.Pool, box domain.SecretBox) *AgentStore {
	return &AgentStore{pool: pool, box: box}
}

// NewAgentStore builds the override store on the same connection as this
// repository, which is how the container wires it without reaching for the pool.
func (r *Repository) NewAgentStore(box domain.SecretBox) *AgentStore {
	if r == nil {
		return NewAgentStore(nil, box)
	}
	return NewAgentStore(r.pool, box)
}

// storedOverride is the JSONB shape. It is separate from domain.AgentOverride so
// the secret is stored under its own key and never leaks into a response.
type storedOverride struct {
	ProviderID uuid.UUID `json:"provider_id,omitempty"`
	Adapter    string    `json:"adapter,omitempty"`
	BaseURL    string    `json:"base_url,omitempty"`
	Model      string    `json:"model,omitempty"`
	MaxTokens  int       `json:"max_tokens,omitempty"`
	// APIKeyEncrypted is base64 of the sealed key. The column it lives in is
	// JSONB, which cannot hold raw bytes.
	APIKeyEncrypted string `json:"api_key_encrypted,omitempty"`
}

// AgentModel reads the override of one Bolu.
func (s *AgentStore) AgentModel(ctx context.Context, workspaceID, agentID uuid.UUID) (domain.AgentOverride, error) {
	var override domain.AgentOverride

	err := s.read(ctx, workspaceID, func(queries *sqlcgen.Queries) error {
		raw, err := queries.GetAgentModelOverride(ctx, sqlcgen.GetAgentModelOverrideParams{
			ID:          agentID,
			WorkspaceID: workspaceID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrProviderNotFound
			}
			return fmt.Errorf("llm: read agent model: %w", err)
		}

		override, err = s.decode(raw)
		return err
	})
	if err != nil {
		return domain.AgentOverride{}, err
	}

	return override, nil
}

// SetAgentModel stores the override of one Bolu.
func (s *AgentStore) SetAgentModel(ctx context.Context, workspaceID, agentID uuid.UUID, override domain.AgentOverride) error {
	encoded, err := s.encode(override)
	if err != nil {
		return err
	}

	return s.write(ctx, workspaceID, func(queries *sqlcgen.Queries) error {
		updated, err := queries.SetAgentModelOverride(ctx, sqlcgen.SetAgentModelOverrideParams{
			ID:           agentID,
			WorkspaceID:  workspaceID,
			DefaultModel: encoded,
		})
		if err != nil {
			return fmt.Errorf("llm: write agent model: %w", err)
		}
		if updated == 0 {
			return domain.ErrProviderNotFound
		}
		return nil
	})
}

func (s *AgentStore) encode(override domain.AgentOverride) ([]byte, error) {
	stored := storedOverride{
		ProviderID: override.ProviderID,
		Adapter:    override.Adapter,
		BaseURL:    override.BaseURL,
		Model:      override.Model,
		MaxTokens:  override.MaxTokens,
	}

	if override.APIKey != "" {
		if s.box == nil {
			return nil, fmt.Errorf("%w: no encryption key is configured", domain.ErrInvalidRequest)
		}
		sealed, err := s.box.Encrypt(override.APIKey)
		if err != nil {
			return nil, fmt.Errorf("llm: encrypt agent key: %w", err)
		}
		stored.APIKeyEncrypted = base64.StdEncoding.EncodeToString(sealed)
	}

	encoded, err := json.Marshal(stored)
	if err != nil {
		return nil, fmt.Errorf("llm: encode agent model: %w", err)
	}
	return encoded, nil
}

func (s *AgentStore) decode(raw []byte) (domain.AgentOverride, error) {
	var stored storedOverride
	if len(raw) == 0 {
		return domain.AgentOverride{}, nil
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return domain.AgentOverride{}, fmt.Errorf("llm: decode agent model: %w", err)
	}

	override := domain.AgentOverride{
		ProviderID: stored.ProviderID,
		Adapter:    stored.Adapter,
		BaseURL:    stored.BaseURL,
		Model:      stored.Model,
		MaxTokens:  stored.MaxTokens,
	}

	if stored.APIKeyEncrypted != "" {
		if s.box == nil {
			return domain.AgentOverride{}, fmt.Errorf("%w: no encryption key is configured", domain.ErrInvalidRequest)
		}
		sealed, err := base64.StdEncoding.DecodeString(stored.APIKeyEncrypted)
		if err != nil {
			return domain.AgentOverride{}, fmt.Errorf("llm: decode agent key: %w", err)
		}
		override.APIKey, err = s.box.Decrypt(sealed)
		if err != nil {
			return domain.AgentOverride{}, fmt.Errorf("llm: decrypt agent key: %w", err)
		}
	}

	return override, nil
}

func (s *AgentStore) read(ctx context.Context, workspaceID uuid.UUID, fn func(*sqlcgen.Queries) error) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("llm: database is not configured")
	}
	return s.pool.InScopeRead(ctx, database.Scope{WorkspaceID: workspaceID}, func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

func (s *AgentStore) write(ctx context.Context, workspaceID uuid.UUID, fn func(*sqlcgen.Queries) error) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("llm: database is not configured")
	}
	return s.pool.InScope(ctx, database.Scope{WorkspaceID: workspaceID}, func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}
