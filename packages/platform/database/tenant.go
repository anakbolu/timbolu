package database

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// WorkspaceID is the tenant identifier carried through request contexts.
type WorkspaceID = uuid.UUID

// WithWorkspace runs fn inside a transaction whose connection carries the
// tenant setting. Row Level Security then filters every statement, so a query
// that forgets `WHERE workspace_id = ...` still cannot read another tenant.
func (p *Pool) WithWorkspace(ctx context.Context, workspaceID WorkspaceID, fn func(tx pgx.Tx) error) error {
	if workspaceID == uuid.Nil {
		return fmt.Errorf("workspace id is required")
	}
	if p == nil || p.pool == nil {
		return fmt.Errorf("database pool is not configured")
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.workspace_id', $1, true)", workspaceID.String()); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// WithWorkspaceRead runs fn inside a read-only tenant transaction. The read
// path is separate so a read can never accidentally write.
func (p *Pool) WithWorkspaceRead(ctx context.Context, workspaceID WorkspaceID, fn func(tx pgx.Tx) error) error {
	if workspaceID == uuid.Nil {
		return fmt.Errorf("workspace id is required")
	}
	if p == nil || p.pool == nil {
		return fmt.Errorf("database pool is not configured")
	}

	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("begin read transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.workspace_id', $1, true)", workspaceID.String()); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit read transaction: %w", err)
	}
	return nil
}
