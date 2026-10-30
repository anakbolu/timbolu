// Package repository implements the retention module's own storage: the queue of workspaces
// due a pass, and the record of what each pass did.
//
// Every statement here runs outside a tenant scope, and that is not an oversight — it is what
// these two tables are. The queue exists to tell the sweep which workspace to open a scope
// with, so a policy keyed on `app_current_workspace_id()` would make it match nothing; and the
// run log carries counts and a moment and no workspace, task or content at all. Everything the
// job then does to a task step or an attachment runs inside that workspace's own scope, in the
// module that owns the table.
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/retention/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/retention/repository/sqlcgen"
	"github.com/anakbolu/timbolu/packages/platform/database"
)

// Repository is the Postgres-backed implementation of domain.Repository.
type Repository struct {
	pool *database.Pool
}

// New builds the repository.
func New(pool *database.Pool) *Repository { return &Repository{pool: pool} }

// Compile-time check that the repository is the interface the module asked for.
var _ domain.Repository = (*Repository)(nil)

// DueTargets returns the workspaces whose next pass is due, oldest first.
func (r *Repository) DueTargets(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error) {
	if r == nil || r.pool == nil {
		return nil, fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}

	rows, err := sqlcgen.New(r.pool.PgxPool()).ListDueRetentionTargets(ctx,
		sqlcgen.ListDueRetentionTargetsParams{
			Now:        now,
			LimitCount: limitOr(limit),
		})
	if err != nil {
		return nil, fmt.Errorf("retention: list due targets: %w", err)
	}

	return rows, nil
}

// TouchTarget moves a workspace's next pass forward.
func (r *Repository) TouchTarget(ctx context.Context, scope domain.Scope, next time.Time) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}
	if scope.IsZero() {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	// It runs at the owner's level rather than in the workspace's scope, because the queue
	// row is what a scope is opened *from*: an update through the workspace policy would
	// need the scope this update exists to schedule. The statement names its workspace, so
	// it still cannot move a row it was not asked to.
	err := sqlcgen.New(r.pool.PgxPool()).TouchRetentionTarget(ctx, sqlcgen.TouchRetentionTargetParams{
		NextPruneAt: next,
		WorkspaceID: scope.WorkspaceID,
	})
	if err != nil {
		return fmt.Errorf("retention: touch target: %w", err)
	}

	return nil
}

// StartRun opens a run record and returns its id.
func (r *Repository) StartRun(ctx context.Context) (int64, error) {
	if r == nil || r.pool == nil {
		return 0, fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}

	id, err := sqlcgen.New(r.pool.PgxPool()).StartRetentionRun(ctx)
	if err != nil {
		return 0, fmt.Errorf("retention: start run: %w", err)
	}

	return id, nil
}

// FinishRun closes a run record with what it did.
//
// A run that failed is recorded too, with its reason and whatever it managed first — the row
// is what makes "retention stopped working three weeks ago" visible, and a failed pass that
// left no trace is the failure that goes unnoticed.
func (r *Repository) FinishRun(ctx context.Context, id int64, result domain.RunResult, failure error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}

	reason := ""
	if failure != nil {
		reason = failure.Error()
	}

	err := sqlcgen.New(r.pool.PgxPool()).FinishRetentionRun(ctx, sqlcgen.FinishRetentionRunParams{
		StepsPruned:       int64(result.StepsPruned),
		AttachmentsPruned: int64(result.Attachments),
		BytesFreed:        result.BytesFreed,
		Error:             reason,
		ID:                id,
	})
	if err != nil {
		return fmt.Errorf("retention: finish run: %w", err)
	}

	return nil
}

// Runs returns the newest runs, which is what a monitor reads.
func (r *Repository) Runs(ctx context.Context, limit int) ([]domain.Run, error) {
	if r == nil || r.pool == nil {
		return nil, fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}

	rows, err := sqlcgen.New(r.pool.PgxPool()).ListRetentionRuns(ctx, limitOr(limit))
	if err != nil {
		return nil, fmt.Errorf("retention: list runs: %w", err)
	}

	out := make([]domain.Run, 0, len(rows))
	for _, row := range rows {
		out = append(out, toRun(row))
	}

	return out, nil
}

// ---------------------------------------------------------------- the erasure evidence

// Start opens an erasure record and returns its id.
func (r *Repository) Start(ctx context.Context, request domain.ErasureRequest, workspaceName, requesterEmail string) (int64, error) {
	if r == nil || r.pool == nil {
		return 0, fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}

	// Unscoped like the run log and for the same reason: the row records that a tenant was
	// removed, so it cannot be written inside that tenant's scope — and it carries an id, a name,
	// a moment, and counts rather than anything the customer said.
	id, err := sqlcgen.New(r.pool.PgxPool()).StartErasure(ctx, sqlcgen.StartErasureParams{
		WorkspaceID:      request.WorkspaceID,
		WorkspaceName:    workspaceName,
		RequestedBy:      request.RequestedBy,
		RequestedByEmail: requesterEmail,
	})
	if err != nil {
		return 0, fmt.Errorf("retention: start erasure: %w", err)
	}

	return id, nil
}

// Finish closes an erasure record with what was removed, or with why it stopped.
func (r *Repository) Finish(ctx context.Context, id int64, result domain.ErasureResult, failure error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}

	reason := ""
	if failure != nil {
		reason = failure.Error()
	}

	err := sqlcgen.New(r.pool.PgxPool()).FinishErasure(ctx, sqlcgen.FinishErasureParams{
		ObjectsDeleted: result.Objects,
		RowsDeleted:    result.Rows,
		Error:          reason,
		ID:             id,
	})
	if err != nil {
		return fmt.Errorf("retention: finish erasure: %w", err)
	}

	return nil
}

// List returns the newest erasures, which is what an auditor reads.
func (r *Repository) List(ctx context.Context, limit int) ([]domain.Erasure, error) {
	if r == nil || r.pool == nil {
		return nil, fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}

	rows, err := sqlcgen.New(r.pool.PgxPool()).ListErasures(ctx, limitOr(limit))
	if err != nil {
		return nil, fmt.Errorf("retention: list erasures: %w", err)
	}

	out := make([]domain.Erasure, 0, len(rows))
	for _, row := range rows {
		out = append(out, toErasure(row))
	}

	return out, nil
}

// Find returns one workspace's completed erasure.
func (r *Repository) Find(ctx context.Context, workspaceID uuid.UUID) (domain.Erasure, error) {
	if r == nil || r.pool == nil {
		return domain.Erasure{}, fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}

	row, err := sqlcgen.New(r.pool.PgxPool()).FindErasure(ctx, workspaceID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Erasure{}, fmt.Errorf("%w: %s", domain.ErrErasureNotFound, workspaceID)
		}

		return domain.Erasure{}, fmt.Errorf("retention: find erasure: %w", err)
	}

	return toErasure(row), nil
}

// toErasure maps a row, turning the absent completion back into a zero moment.
func toErasure(row sqlcgen.DataErasure) domain.Erasure {
	erasure := domain.Erasure{
		ID:              row.ID,
		WorkspaceID:     row.WorkspaceID,
		WorkspaceName:   row.WorkspaceName,
		RequestedBy:     row.RequestedBy,
		RequestedByMail: row.RequestedByEmail,
		RequestedAt:     row.RequestedAt,
		Objects:         row.ObjectsDeleted,
		Rows:            row.RowsDeleted,
		Error:           row.Error,
	}
	if row.CompletedAt != nil {
		erasure.CompletedAt = *row.CompletedAt
	}

	return erasure
}

// toRun maps a row, turning the absent finish time back into a zero moment.
func toRun(row sqlcgen.RetentionRun) domain.Run {
	run := domain.Run{
		ID:          row.ID,
		StartedAt:   row.StartedAt,
		StepsPruned: int(row.StepsPruned),
		Attachments: int(row.AttachmentsPruned),
		BytesFreed:  row.BytesFreed,
		Error:       row.Error,
	}
	if row.FinishedAt != nil {
		run.FinishedAt = *row.FinishedAt
	}

	return run
}

// limitOr takes the deployment's bound or the module's default.
func limitOr(limit int) int32 {
	if limit <= 0 {
		return domain.DefaultLimit
	}

	return int32(limit)
}
