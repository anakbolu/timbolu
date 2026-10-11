// Package repository implements the task runtime data access with pgx and the
// sqlc-generated queries.
//
// Every statement runs inside the caller's tenant scope, so Row Level Security
// filters it even when a query forgets its workspace filter. The writes are
// guarded by status in the statement itself: a retried activity cannot overwrite
// a result the user has already seen.
package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/repository/sqlcgen"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
)

// Repository is the Postgres-backed implementation of the task ports.
type Repository struct {
	pool *database.Pool
}

// New builds the repository.
func New(pool *database.Pool) *Repository {
	return &Repository{pool: pool}
}

// Create stores a queued task.
func (r *Repository) Create(ctx context.Context, scope domain.Scope, task domain.NewTask) (domain.Task, error) {
	var created domain.Task

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.CreateTask(ctx, sqlcgen.CreateTaskParams{
			WorkspaceID:    scope.WorkspaceID,
			AgentID:        task.AgentID,
			ParentTaskID:   optional(task.ParentTaskID),
			ConversationID: optional(task.ConversationID),
			ReplyMessageID: optional(task.ReplyMessageID),
			Trigger:        task.Trigger,
			Title:          task.Title,
			Depth:          int32(task.Depth),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: the Bolu, the conversation, the message, or the parent task is not in this workspace",
					domain.ErrInvalidInput)
			}
			return translateWriteError(err, "create task")
		}

		created = toTask(row)
		return nil
	})
	if err != nil {
		return domain.Task{}, err
	}

	return created, nil
}

// Get returns one task.
func (r *Repository) Get(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Task, error) {
	var task domain.Task

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetTask(ctx, sqlcgen.GetTaskParams{ID: id, WorkspaceID: scope.WorkspaceID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrTaskNotFound
			}
			return fmt.Errorf("task: get: %w", err)
		}

		task = toTask(row)
		return nil
	})
	if err != nil {
		return domain.Task{}, err
	}

	return task, nil
}

// List returns the history, newest first.
func (r *Repository) List(ctx context.Context, scope domain.Scope, filter domain.Filter) ([]domain.Task, error) {
	limit := filter.Limit
	switch {
	case limit <= 0:
		limit = domain.DefaultHistoryLimit
	case limit > domain.MaxHistoryLimit:
		limit = domain.MaxHistoryLimit
	}

	var tasks []domain.Task

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListTasks(ctx, sqlcgen.ListTasksParams{
			WorkspaceID:  scope.WorkspaceID,
			Limit:        int32(limit),
			AgentID:      optional(filter.AgentID),
			ParentTaskID: optional(filter.ParentTaskID),
			Status:       optionalString(filter.Status),
			Live:         filter.Live,
		})
		if err != nil {
			return fmt.Errorf("task: list: %w", err)
		}

		tasks = make([]domain.Task, 0, len(rows))
		for _, row := range rows {
			tasks = append(tasks, toTask(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return tasks, nil
}

// Children returns the tasks this one handed work to.
func (r *Repository) Children(ctx context.Context, scope domain.Scope, taskID uuid.UUID) ([]domain.Task, error) {
	var tasks []domain.Task

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListChildTasks(ctx, sqlcgen.ListChildTasksParams{
			WorkspaceID:  scope.WorkspaceID,
			ParentTaskID: optional(taskID),
		})
		if err != nil {
			return fmt.Errorf("task: list children: %w", err)
		}

		tasks = make([]domain.Task, 0, len(rows))
		for _, row := range rows {
			tasks = append(tasks, toTask(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return tasks, nil
}

// Start records the workflow that owns the task and marks it running.
func (r *Repository) Start(ctx context.Context, scope domain.Scope, id uuid.UUID, workflowID, runID string) (domain.Task, error) {
	var task domain.Task

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.StartTask(ctx, sqlcgen.StartTaskParams{
			ID:            id,
			WorkspaceID:   scope.WorkspaceID,
			WorkflowID:    workflowID,
			TemporalRunID: runID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrTaskNotFound
			}
			return fmt.Errorf("task: start: %w", err)
		}

		task = toTask(row)
		return nil
	})
	if err != nil {
		return domain.Task{}, err
	}

	return task, nil
}

// Progress records a heartbeat with the running totals.
func (r *Repository) Progress(ctx context.Context, scope domain.Scope, progress domain.Progress) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		_, err := queries.ProgressTask(ctx, sqlcgen.ProgressTaskParams{
			ID:           progress.TaskID,
			WorkspaceID:  scope.WorkspaceID,
			InputTokens:  progress.InputTokens,
			OutputTokens: progress.OutputTokens,
			CostMicros:   progress.CostMicros,
			StepCount:    int32(progress.StepCount),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// The task finished while this heartbeat was in flight, which is
				// not a failure: there is nothing left to report on.
				return domain.ErrTaskNotFound
			}
			return fmt.Errorf("task: progress: %w", err)
		}
		return nil
	})
}

// Park records that the task is waiting for a human decision.
func (r *Repository) Park(ctx context.Context, scope domain.Scope, id uuid.UUID, reason string, draftID uuid.UUID) (domain.Task, error) {
	var task domain.Task

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.ParkTask(ctx, sqlcgen.ParkTaskParams{
			ID:             id,
			WorkspaceID:    scope.WorkspaceID,
			WaitingReason:  reason,
			WaitingDraftID: optional(draftID),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrTaskNotFound
			}
			return fmt.Errorf("task: park: %w", err)
		}

		task = toTask(row)
		return nil
	})
	if err != nil {
		return domain.Task{}, err
	}

	return task, nil
}

// Resume clears the waiting state when the decision arrives.
func (r *Repository) Resume(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Task, error) {
	var task domain.Task

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.ResumeTask(ctx, sqlcgen.ResumeTaskParams{ID: id, WorkspaceID: scope.WorkspaceID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrTaskNotFound
			}
			return fmt.Errorf("task: resume: %w", err)
		}

		task = toTask(row)
		return nil
	})
	if err != nil {
		return domain.Task{}, err
	}

	return task, nil
}

// Finish records the terminal state.
func (r *Repository) Finish(ctx context.Context, scope domain.Scope, finish domain.Finish) (domain.Task, error) {
	var task domain.Task

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.FinishTask(ctx, sqlcgen.FinishTaskParams{
			ID:            finish.TaskID,
			WorkspaceID:   scope.WorkspaceID,
			Status:        finish.Status,
			Summary:       finish.Summary,
			StoppedReason: finish.StoppedReason,
			InputTokens:   finish.InputTokens,
			OutputTokens:  finish.OutputTokens,
			CostMicros:    finish.CostMicros,
			StepCount:     int32(finish.StepCount),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// A task that already finished stays finished. Reporting not
				// found is honest: there was nothing to write.
				return domain.ErrTaskNotFound
			}
			return fmt.Errorf("task: finish: %w", err)
		}

		task = toTask(row)
		return nil
	})
	if err != nil {
		return domain.Task{}, err
	}

	return task, nil
}

// Cancel stops a task that has not finished.
func (r *Repository) Cancel(ctx context.Context, scope domain.Scope, id uuid.UUID, reason string) (domain.Task, error) {
	var task domain.Task

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.CancelTask(ctx, sqlcgen.CancelTaskParams{
			ID:            id,
			WorkspaceID:   scope.WorkspaceID,
			StoppedReason: reason,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrTaskNotFound
			}
			return fmt.Errorf("task: cancel: %w", err)
		}

		task = toTask(row)
		return nil
	})
	if err != nil {
		return domain.Task{}, err
	}

	return task, nil
}

// CountLiveTasksForAgent reports how many tasks a Bolu has in flight, which is
// what the registry reads before deleting one.
func (r *Repository) CountLiveTasksForAgent(ctx context.Context, scope domain.Scope, agentID uuid.UUID) (int, error) {
	var count int64

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		value, err := queries.CountLiveTasksForAgent(ctx, sqlcgen.CountLiveTasksForAgentParams{
			WorkspaceID: scope.WorkspaceID,
			AgentID:     agentID,
		})
		if err != nil {
			return fmt.Errorf("task: count live: %w", err)
		}
		count = value
		return nil
	})
	if err != nil {
		return 0, err
	}

	return int(count), nil
}

// Append writes one step, assigning its sequence number in the statement.
func (r *Repository) Append(ctx context.Context, scope domain.Scope, step domain.Step) (domain.Step, error) {
	var stored domain.Step

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.AppendTaskStep(ctx, sqlcgen.AppendTaskStepParams{
			WorkspaceID:  scope.WorkspaceID,
			TaskID:       step.TaskID,
			Kind:         step.Kind,
			ToolName:     step.ToolName,
			ToolLabel:    step.ToolLabel,
			Input:        payloadOr(step.Input),
			Output:       payloadOr(step.Output),
			InputTokens:  int32(step.InputTokens),
			OutputTokens: int32(step.OutputTokens),
		})
		if err != nil {
			return translateWriteError(err, "append step")
		}

		stored = toStep(row)
		return nil
	})
	if err != nil {
		return domain.Step{}, err
	}

	return stored, nil
}

// ListSteps returns a task's steps in order.
func (r *Repository) ListSteps(ctx context.Context, scope domain.Scope, taskID uuid.UUID) ([]domain.Step, error) {
	var steps []domain.Step

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListTaskSteps(ctx, sqlcgen.ListTaskStepsParams{
			WorkspaceID: scope.WorkspaceID,
			TaskID:      taskID,
		})
		if err != nil {
			return fmt.Errorf("task: list steps: %w", err)
		}

		steps = make([]domain.Step, 0, len(rows))
		for _, row := range rows {
			steps = append(steps, toStep(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return steps, nil
}

func (r *Repository) read(ctx context.Context, scope domain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("task: database is not configured")
	}
	return r.pool.InScopeRead(ctx, database.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}, func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

func (r *Repository) write(ctx context.Context, scope domain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("task: database is not configured")
	}
	return r.pool.InScope(ctx, database.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}, func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

// ------------------------------------------------------------- conversions

func toTask(row sqlcgen.Task) domain.Task {
	return domain.Task{
		ID:             row.ID,
		WorkspaceID:    row.WorkspaceID,
		AgentID:        row.AgentID,
		ParentTaskID:   deref(row.ParentTaskID),
		ConversationID: deref(row.ConversationID),
		Trigger:        row.Trigger,
		Title:          row.Title,
		Status:         row.Status,
		ReplyMessageID: deref(row.ReplyMessageID),
		WorkflowID:     row.WorkflowID,
		TemporalRunID:  row.TemporalRunID,
		Depth:          int(row.Depth),
		Summary:        row.Summary,
		StoppedReason:  row.StoppedReason,
		WaitingReason:  row.WaitingReason,
		WaitingDraftID: deref(row.WaitingDraftID),
		StepCount:      int(row.StepCount),
		InputTokens:    row.InputTokens,
		OutputTokens:   row.OutputTokens,
		CostMicros:     row.CostMicros,
		HeartbeatAt:    row.HeartbeatAt,
		StartedAt:      row.StartedAt,
		FinishedAt:     row.FinishedAt,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
	}
}

func toStep(row sqlcgen.TaskStep) domain.Step {
	return domain.Step{
		ID:           row.ID,
		TaskID:       row.TaskID,
		Seq:          int(row.Seq),
		Kind:         row.Kind,
		ToolName:     row.ToolName,
		ToolLabel:    row.ToolLabel,
		Input:        row.Input,
		Output:       row.Output,
		InputTokens:  int(row.InputTokens),
		OutputTokens: int(row.OutputTokens),
		Pruned:       row.PrunedAt != nil,
		CreatedAt:    row.CreatedAt,
	}
}

func optional(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func deref(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

func payloadOr(payload []byte) []byte {
	if len(payload) == 0 {
		return []byte("{}")
	}
	return payload
}

// translateWriteError turns the constraint violations the runtime can actually
// hit into the errors the service reports, and leaves the rest wrapped.
func translateWriteError(err error, action string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23503":
			// A foreign key failure means the Bolu, the conversation, or the
			// parent task is not in this workspace, which RLS also refuses.
			return fmt.Errorf("%w: %s", domain.ErrInvalidInput, "referenced row is not in this workspace")
		case "23514":
			// The status, trigger, kind, or depth check constraint.
			return fmt.Errorf("%w: %s", domain.ErrInvalidInput, "value is outside the allowed set")
		}
	}
	return fmt.Errorf("task: %s: %w", action, err)
}
