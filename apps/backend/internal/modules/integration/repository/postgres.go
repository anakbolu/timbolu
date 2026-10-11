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
	"encoding/json"
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

// Enqueue stores a verified delivery.
//
// The insert conflicts on the delivery rather than failing, so a provider's retry
// returns ErrDeliveryExists while the endpoint still answers 200: the provider
// did its job, and telling it the event is a duplicate would make it retry
// forever.
func (r *Repository) Enqueue(ctx context.Context, input domain.EnqueueInput) (domain.QueuedEvent, error) {
	var out domain.QueuedEvent

	err := r.write(ctx, domain.Scope{WorkspaceID: input.WorkspaceID}, func(queries *sqlcgen.Queries) error {
		row, err := queries.EnqueueWebhookEvent(ctx, sqlcgen.EnqueueWebhookEventParams{
			WorkspaceID:   input.WorkspaceID,
			IntegrationID: nullableID(input.IntegrationID),
			App:           input.App,
			DeliveryID:    input.DeliveryID,
			Event:         input.Event,
			Payload:       payloadOr(input.Payload),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrDeliveryExists
			}
			return translateWriteError(err, "enqueue webhook event")
		}
		out = toQueuedEvent(row)
		return nil
	})
	if err != nil {
		return domain.QueuedEvent{}, err
	}
	return out, nil
}

// Pending returns the oldest unprocessed deliveries of a workspace.
func (r *Repository) Pending(ctx context.Context, scope domain.Scope, limit int) ([]domain.QueuedEvent, error) {
	if limit <= 0 {
		limit = 50
	}

	var out []domain.QueuedEvent

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.PendingWebhookEvents(ctx, sqlcgen.PendingWebhookEventsParams{
			WorkspaceID: scope.WorkspaceID,
			Limit:       int32(limit),
		})
		if err != nil {
			return fmt.Errorf("integration: pending webhook events: %w", err)
		}
		out = make([]domain.QueuedEvent, 0, len(rows))
		for _, row := range rows {
			out = append(out, toQueuedEvent(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Processed marks a delivery as acted on.
func (r *Repository) Processed(ctx context.Context, scope domain.Scope, id uuid.UUID) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		affected, err := queries.MarkWebhookEventProcessed(ctx, sqlcgen.MarkWebhookEventProcessedParams{
			WorkspaceID: scope.WorkspaceID,
			ID:          id,
		})
		if err != nil {
			return translateWriteError(err, "mark webhook event processed")
		}
		if affected == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
}

// Failed records an attempt that did not work.
func (r *Repository) Failed(ctx context.Context, scope domain.Scope, id uuid.UUID, reason string) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		affected, err := queries.FailWebhookEvent(ctx, sqlcgen.FailWebhookEventParams{
			WorkspaceID: scope.WorkspaceID,
			ID:          id,
			LastError:   reason,
		})
		if err != nil {
			return translateWriteError(err, "fail webhook event")
		}
		if affected == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
}

// ListMCPServers returns the servers a workspace added.
func (r *Repository) ListMCPServers(ctx context.Context, scope domain.Scope) ([]domain.MCPServer, error) {
	var out []domain.MCPServer

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListMCPServers(ctx, scope.WorkspaceID)
		if err != nil {
			return fmt.Errorf("integration: list MCP servers: %w", err)
		}
		out = make([]domain.MCPServer, 0, len(rows))
		for _, row := range rows {
			out = append(out, toMCPServer(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetMCPServer returns one server.
func (r *Repository) GetMCPServer(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.MCPServer, error) {
	var out domain.MCPServer

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetMCPServer(ctx, sqlcgen.GetMCPServerParams{
			WorkspaceID: scope.WorkspaceID,
			ID:          id,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrMCPServerNotFound
			}
			return fmt.Errorf("integration: get MCP server: %w", err)
		}
		out = toMCPServer(row)
		return nil
	})
	if err != nil {
		return domain.MCPServer{}, err
	}
	return out, nil
}

// UpsertMCPServer stores a server, keyed by app.
func (r *Repository) UpsertMCPServer(ctx context.Context, scope domain.Scope, input domain.MCPServerInput, sealedCredential []byte) (domain.MCPServer, error) {
	var out domain.MCPServer

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.UpsertMCPServer(ctx, sqlcgen.UpsertMCPServerParams{
			WorkspaceID:         scope.WorkspaceID,
			App:                 input.App,
			Label:               input.Label,
			Endpoint:            input.Endpoint,
			CredentialEncrypted: sealedCredential,
			Enabled:             input.Enabled,
		})
		if err != nil {
			return translateWriteError(err, "upsert MCP server")
		}
		out = toMCPServer(row)
		return nil
	})
	if err != nil {
		return domain.MCPServer{}, err
	}
	return out, nil
}

// SaveMCPTools records the tool list a server reported.
func (r *Repository) SaveMCPTools(ctx context.Context, scope domain.Scope, id uuid.UUID, tools []domain.MCPTool) error {
	encoded, err := json.Marshal(tools)
	if err != nil {
		return fmt.Errorf("integration: encode MCP tools: %w", err)
	}

	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		_, err := queries.SaveMCPTools(ctx, sqlcgen.SaveMCPToolsParams{
			WorkspaceID: scope.WorkspaceID,
			ID:          id,
			Tools:       encoded,
			ToolCount:   int32(len(tools)),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrMCPServerNotFound
			}
			return translateWriteError(err, "save MCP tools")
		}
		return nil
	})
}

// MCPServerTools returns the tool list a server last reported.
func (r *Repository) MCPServerTools(ctx context.Context, scope domain.Scope, id uuid.UUID) ([]domain.MCPTool, error) {
	var out []domain.MCPTool

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetMCPServer(ctx, sqlcgen.GetMCPServerParams{
			WorkspaceID: scope.WorkspaceID,
			ID:          id,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrMCPServerNotFound
			}
			return fmt.Errorf("integration: read MCP tools: %w", err)
		}

		if len(row.Tools) == 0 {
			return nil
		}
		if err := json.Unmarshal(row.Tools, &out); err != nil {
			// A stored list this build cannot read is reported as no tools rather
			// than as an error: the next refresh replaces it, and failing a task
			// because a cache is unreadable would be the wrong trade.
			out = nil
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetMCPServerError records why a server could not be reached.
func (r *Repository) SetMCPServerError(ctx context.Context, scope domain.Scope, id uuid.UUID, reason string) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		_, err := queries.SetMCPServerError(ctx, sqlcgen.SetMCPServerErrorParams{
			WorkspaceID: scope.WorkspaceID,
			ID:          id,
			LastError:   reason,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrMCPServerNotFound
			}
			return translateWriteError(err, "set MCP server error")
		}
		return nil
	})
}

// DeleteMCPServer removes one.
func (r *Repository) DeleteMCPServer(ctx context.Context, scope domain.Scope, id uuid.UUID) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		affected, err := queries.DeleteMCPServer(ctx, sqlcgen.DeleteMCPServerParams{
			WorkspaceID: scope.WorkspaceID,
			ID:          id,
		})
		if err != nil {
			return translateWriteError(err, "delete MCP server")
		}
		if affected == 0 {
			return domain.ErrMCPServerNotFound
		}
		return nil
	})
}

// MCPServerCredential returns the sealed credential of a server.
func (r *Repository) MCPServerCredential(ctx context.Context, scope domain.Scope, id uuid.UUID) ([]byte, error) {
	var sealed []byte

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		value, err := queries.GetMCPServerCredential(ctx, sqlcgen.GetMCPServerCredentialParams{
			WorkspaceID: scope.WorkspaceID,
			ID:          id,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrMCPServerNotFound
			}
			return fmt.Errorf("integration: read MCP credential: %w", err)
		}
		sealed = value
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sealed, nil
}

// toMCPServer renders a stored server.
func toMCPServer(row sqlcgen.McpServer) domain.MCPServer {
	return domain.MCPServer{
		ID:        row.ID,
		App:       row.App,
		Label:     row.Label,
		Endpoint:  row.Endpoint,
		Enabled:   row.Enabled,
		ToolCount: int(row.ToolCount),
		LastError: row.LastError,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
}

// toQueuedEvent renders a stored delivery.
func toQueuedEvent(row sqlcgen.WebhookEvent) domain.QueuedEvent {
	out := domain.QueuedEvent{
		ID:          row.ID,
		WorkspaceID: row.WorkspaceID,
		App:         row.App,
		DeliveryID:  row.DeliveryID,
		Event:       row.Event,
		Payload:     row.Payload,
		ReceivedAt:  row.ReceivedAt,
		Attempts:    int(row.Attempts),
	}
	if row.IntegrationID != nil {
		out.IntegrationID = *row.IntegrationID
	}
	return out
}

// payloadOr keeps an absent body as an empty object rather than null, so a
// consumer never has to handle two shapes of nothing.
func payloadOr(payload []byte) []byte {
	if len(payload) == 0 {
		return []byte(`{}`)
	}
	return payload
}

// nullableID renders a uuid the column allows to be absent.
func nullableID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
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
