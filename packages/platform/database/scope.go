package database

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Scope is the identity a transaction runs as. Row Level Security reads both
// settings: app.user_id is the authenticated user, app.workspace_id is the
// active tenant. At least one of them is required, so no query ever runs
// unscoped by accident.
type Scope struct {
	UserID      uuid.UUID
	WorkspaceID uuid.UUID
}

// IsZero reports whether the scope carries no identity at all.
func (s Scope) IsZero() bool {
	return s.UserID == uuid.Nil && s.WorkspaceID == uuid.Nil
}

// InScope runs fn inside a transaction whose connection carries the scope
// settings. Every statement in fn is filtered by Row Level Security, so a query
// that forgets `WHERE workspace_id = ...` still cannot reach another tenant.
func (p *Pool) InScope(ctx context.Context, scope Scope, fn func(tx pgx.Tx) error) error {
	return p.inScope(ctx, scope, pgx.TxOptions{}, fn)
}

// InScopeRead is InScope for a read-only transaction.
func (p *Pool) InScopeRead(ctx context.Context, scope Scope, fn func(tx pgx.Tx) error) error {
	return p.inScope(ctx, scope, pgx.TxOptions{AccessMode: pgx.ReadOnly}, fn)
}

func (p *Pool) inScope(ctx context.Context, scope Scope, options pgx.TxOptions, fn func(tx pgx.Tx) error) error {
	if scope.IsZero() {
		return fmt.Errorf("scope requires a user id or a workspace id")
	}
	if p == nil || p.pool == nil {
		return fmt.Errorf("database pool is not configured")
	}

	tx, err := p.pool.BeginTx(ctx, options)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if scope.UserID != uuid.Nil {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.user_id', $1, true)", scope.UserID.String()); err != nil {
			return fmt.Errorf("set user context: %w", err)
		}
	}
	if scope.WorkspaceID != uuid.Nil {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.workspace_id', $1, true)", scope.WorkspaceID.String()); err != nil {
			return fmt.Errorf("set tenant context: %w", err)
		}
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// SetWorkspace activates a tenant inside an already open transaction. Onboarding
// needs it: the workspace row is created first (which only requires an
// authenticated user), and only then can its teams and owner membership be
// written, because those policies require the active workspace.
func SetWorkspace(ctx context.Context, tx pgx.Tx, workspaceID uuid.UUID) error {
	if workspaceID == uuid.Nil {
		return fmt.Errorf("workspace id is required")
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.workspace_id', $1, true)", workspaceID.String()); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}
	return nil
}
