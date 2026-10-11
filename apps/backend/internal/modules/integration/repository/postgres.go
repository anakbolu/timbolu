// Package repository implements the integration data access with pgx and the
// sqlc-generated queries.
//
// Every statement runs inside the caller's tenant scope, so Row Level Security
// filters it even when a statement forgets its workspace filter. The token
// columns are read by one statement that exists to open them, so listing the
// workspace's connections cannot carry a ciphertext into a response by accident.
package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/repository/sqlcgen"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
)

// Repository is the Postgres-backed implementation of the integration ports.
type Repository struct {
	pool *database.Pool
}

// New builds the repository.
func New(pool *database.Pool) *Repository {
	return &Repository{pool: pool}
}

// List returns the workspace's connections.
func (r *Repository) List(ctx context.Context, scope domain.Scope) ([]domain.Integration, error) {
	var out []domain.Integration

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListIntegrations(ctx, scope.WorkspaceID)
		if err != nil {
			return fmt.Errorf("integration: list: %w", err)
		}
		out = make([]domain.Integration, 0, len(rows))
		for _, row := range rows {
			out = append(out, toIntegration(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Get returns one connection.
func (r *Repository) Get(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Integration, error) {
	var out domain.Integration

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetIntegration(ctx, sqlcgen.GetIntegrationParams{
			WorkspaceID: scope.WorkspaceID,
			ID:          id,
		})
		if err != nil {
			return mapNotFound(err, "get integration")
		}
		out = toIntegration(row)
		return nil
	})
	if err != nil {
		return domain.Integration{}, err
	}
	return out, nil
}

// Upsert stores a connection, keyed by app and account label.
func (r *Repository) Upsert(ctx context.Context, scope domain.Scope, input domain.ConnectInput, sealed domain.StoredToken) (domain.Integration, error) {
	var out domain.Integration

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.UpsertIntegration(ctx, sqlcgen.UpsertIntegrationParams{
			WorkspaceID:           scope.WorkspaceID,
			App:                   input.App,
			AccountLabel:          input.AccountLabel,
			Scopes:                scopesOr(input.Scopes),
			AccessTokenEncrypted:  sealed.AccessTokenEncrypted,
			RefreshTokenEncrypted: sealed.RefreshTokenEncrypted,
			TokenExpiresAt:        sealed.ExpiresAt,
		})
		if err != nil {
			return translateWriteError(err, "upsert")
		}
		out = toIntegration(row)
		return nil
	})
	if err != nil {
		return domain.Integration{}, err
	}
	return out, nil
}

// Sealed returns the stored token of a connection, still sealed.
func (r *Repository) Sealed(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.StoredToken, error) {
	var out domain.StoredToken

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetIntegrationToken(ctx, sqlcgen.GetIntegrationTokenParams{
			WorkspaceID: scope.WorkspaceID,
			ID:          id,
		})
		if err != nil {
			return mapNotFound(err, "get integration token")
		}
		out = domain.StoredToken{
			AccessTokenEncrypted:  row.AccessTokenEncrypted,
			RefreshTokenEncrypted: row.RefreshTokenEncrypted,
			ExpiresAt:             row.TokenExpiresAt,
			Scopes:                row.Scopes,
		}
		return nil
	})
	if err != nil {
		return domain.StoredToken{}, err
	}
	return out, nil
}

// SaveRefreshed replaces the sealed token after a refresh.
func (r *Repository) SaveRefreshed(ctx context.Context, scope domain.Scope, id uuid.UUID, sealed domain.StoredToken) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		_, err := queries.SaveRefreshedToken(ctx, sqlcgen.SaveRefreshedTokenParams{
			WorkspaceID:           scope.WorkspaceID,
			ID:                    id,
			AccessTokenEncrypted:  sealed.AccessTokenEncrypted,
			RefreshTokenEncrypted: sealed.RefreshTokenEncrypted,
			TokenExpiresAt:        sealed.ExpiresAt,
		})
		if err != nil {
			return mapNotFound(err, "save refreshed token")
		}
		return nil
	})
}

// SetStatus records a status a person may have to act on.
func (r *Repository) SetStatus(ctx context.Context, scope domain.Scope, id uuid.UUID, status, reason string) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		_, err := queries.SetIntegrationStatus(ctx, sqlcgen.SetIntegrationStatusParams{
			WorkspaceID:  scope.WorkspaceID,
			ID:           id,
			Status:       status,
			StatusReason: reason,
		})
		if err != nil {
			return mapNotFound(err, "set integration status")
		}
		return nil
	})
}

// Disconnect clears the stored token and marks the row disconnected.
func (r *Repository) Disconnect(ctx context.Context, scope domain.Scope, id uuid.UUID) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		_, err := queries.DisconnectIntegration(ctx, sqlcgen.DisconnectIntegrationParams{
			WorkspaceID: scope.WorkspaceID,
			ID:          id,
		})
		if err != nil {
			return mapNotFound(err, "disconnect integration")
		}
		return nil
	})
}

// Delete removes the row.
func (r *Repository) Delete(ctx context.Context, scope domain.Scope, id uuid.UUID) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		affected, err := queries.DeleteIntegration(ctx, sqlcgen.DeleteIntegrationParams{
			WorkspaceID: scope.WorkspaceID,
			ID:          id,
		})
		if err != nil {
			return fmt.Errorf("integration: delete: %w", err)
		}
		if affected == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
}

// DataKey returns the workspace's wrapped data key.
func (r *Repository) DataKey(ctx context.Context, scope domain.Scope) (domain.WrappedKey, error) {
	var out domain.WrappedKey

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetWorkspaceKey(ctx, scope.WorkspaceID)
		if err != nil {
			return mapNotFound(err, "get workspace key")
		}
		out = domain.WrappedKey{KeyID: row.KmsKeyID, Bytes: row.WrappedDataKey}
		return nil
	})
	if err != nil {
		return domain.WrappedKey{}, err
	}
	return out, nil
}

// SaveDataKey stores the wrapped data key, replacing an existing wrap.
func (r *Repository) SaveDataKey(ctx context.Context, scope domain.Scope, wrapped domain.WrappedKey) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		_, err := queries.UpsertWorkspaceKey(ctx, sqlcgen.UpsertWorkspaceKeyParams{
			WorkspaceID:    scope.WorkspaceID,
			WrappedDataKey: wrapped.Bytes,
			KmsKeyID:       wrapped.KeyID,
		})
		if err != nil {
			return translateWriteError(err, "save workspace key")
		}
		return nil
	})
}

// SaveState records an OAuth state as usable until it expires.
func (r *Repository) SaveState(ctx context.Context, state domain.OAuthState) error {
	return r.write(ctx, domain.Scope{UserID: state.UserID, WorkspaceID: state.WorkspaceID}, func(queries *sqlcgen.Queries) error {
		if err := queries.SaveOAuthState(ctx, sqlcgen.SaveOAuthStateParams{
			NonceHash:   hashNonce(state.Nonce),
			WorkspaceID: state.WorkspaceID,
			UserID:      state.UserID,
			App:         state.App,
			ReturnTo:    state.ReturnTo,
			ExpiresAt:   state.ExpiresAt,
		}); err != nil {
			return translateWriteError(err, "save oauth state")
		}
		return nil
	})
}

// ConsumeState spends a state's nonce.
//
// The single statement is the guarantee: the second of two callbacks carrying the
// same state finds no row, because the first set consumed_at.
func (r *Repository) ConsumeState(ctx context.Context, workspaceID uuid.UUID, nonce string, now time.Time) error {
	return r.write(ctx, domain.Scope{WorkspaceID: workspaceID}, func(queries *sqlcgen.Queries) error {
		_, err := queries.ConsumeOAuthState(ctx, sqlcgen.ConsumeOAuthStateParams{
			NonceHash:   hashNonce(nonce),
			WorkspaceID: workspaceID,
			ExpiresAt:   now,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrInvalidState
			}
			return fmt.Errorf("integration: consume oauth state: %w", err)
		}
		return nil
	})
}

// hashNonce renders the stored form of a state's nonce.
//
// The value handed to the provider is never stored, so a leaked backup cannot be
// used to finish somebody's flow. The nonce is random and high-entropy, so a
// plain hash is enough here: there is nothing to guess.
func hashNonce(nonce string) string {
	sum := sha256.Sum256([]byte(nonce))
	return hex.EncodeToString(sum[:])
}

// Grants lists the grants of one Bolu.
func (r *Repository) Grants(ctx context.Context, scope domain.Scope, agentID uuid.UUID) ([]domain.Grant, error) {
	var out []domain.Grant

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListGrants(ctx, sqlcgen.ListGrantsParams{
			WorkspaceID: scope.WorkspaceID,
			AgentID:     agentID,
		})
		if err != nil {
			return fmt.Errorf("integration: list grants: %w", err)
		}
		out = make([]domain.Grant, 0, len(rows))
		for _, row := range rows {
			out = append(out, domain.Grant{
				AgentID:       row.AgentID,
				IntegrationID: row.IntegrationID,
				Permission:    row.Permission,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetGrant adds or updates one Bolu's permission on one integration.
func (r *Repository) SetGrant(ctx context.Context, scope domain.Scope, agentID, integrationID uuid.UUID, permission string) (domain.Grant, error) {
	var out domain.Grant

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.UpsertGrant(ctx, sqlcgen.UpsertGrantParams{
			WorkspaceID:   scope.WorkspaceID,
			AgentID:       agentID,
			IntegrationID: integrationID,
			Permission:    permission,
		})
		if err != nil {
			return translateWriteError(err, "set grant")
		}
		out = domain.Grant{
			AgentID:       row.AgentID,
			IntegrationID: row.IntegrationID,
			Permission:    row.Permission,
		}
		return nil
	})
	if err != nil {
		return domain.Grant{}, err
	}
	return out, nil
}

// DeleteGrant removes one grant.
func (r *Repository) DeleteGrant(ctx context.Context, scope domain.Scope, agentID, integrationID uuid.UUID) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		affected, err := queries.DeleteGrant(ctx, sqlcgen.DeleteGrantParams{
			WorkspaceID:   scope.WorkspaceID,
			AgentID:       agentID,
			IntegrationID: integrationID,
		})
		if err != nil {
			return fmt.Errorf("integration: delete grant: %w", err)
		}
		if affected == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
}

// ------------------------------------------------------------------ helpers

func (r *Repository) read(ctx context.Context, scope domain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("integration: database is not configured")
	}
	return r.pool.InScopeRead(ctx, database.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}, func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

func (r *Repository) write(ctx context.Context, scope domain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("integration: database is not configured")
	}
	return r.pool.InScope(ctx, database.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}, func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

// toIntegration renders a stored row without its sealed columns.
func toIntegration(row sqlcgen.Integration) domain.Integration {
	return domain.Integration{
		ID:              row.ID,
		App:             row.App,
		AccountLabel:    row.AccountLabel,
		Status:          row.Status,
		StatusReason:    row.StatusReason,
		Scopes:          row.Scopes,
		ExpiresAt:       row.TokenExpiresAt,
		LastRefreshedAt: row.LastRefreshedAt,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

// scopesOr keeps an empty list as an empty array rather than nil, so the column
// reads back as `{}` and a client never has to handle null.
func scopesOr(scopes []string) []string {
	if scopes == nil {
		return []string{}
	}
	return scopes
}

// mapNotFound turns a missing row into the module's own sentinel. RLS makes
// another workspace's row invisible, so a row that is not there and a row that
// belongs to somebody else are the same answer.
func mapNotFound(err error, operation string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	return fmt.Errorf("integration: %s: %w", operation, err)
}

// translateWriteError turns the schema's own refusals into the module's
// sentinels, so a caller sees what it can act on rather than a driver error.
func translateWriteError(err error, action string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23503":
			// A foreign key failure means the row referenced is not in this
			// workspace, which RLS refuses too. Reporting it as invalid input
			// keeps the tenant boundary invisible.
			return fmt.Errorf("%w: a referenced row is not in this workspace", domain.ErrInvalidInput)
		case "23514":
			// A check constraint: an unknown status or an unknown permission.
			return fmt.Errorf("%w: a value is outside the allowed set", domain.ErrInvalidInput)
		}
	}
	return fmt.Errorf("integration: %s: %w", action, err)
}
