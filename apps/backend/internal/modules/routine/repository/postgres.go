// Package repository implements the routine data access with pgx and the
// sqlc-generated queries.
//
// Every statement runs inside the caller's tenant scope, so Row Level Security
// filters it even when a query forgets its workspace filter.
package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/repository/sqlcgen"
	"github.com/robzlabz/employeebot/packages/platform/database"
)

// Repository is the Postgres-backed implementation of the routine ports.
type Repository struct {
	pool *database.Pool
}

// New builds the repository.
func New(pool *database.Pool) *Repository {
	return &Repository{pool: pool}
}

// listLimit is the ceiling of a list read, so one request cannot pull every
// routine and every run of a workspace.
const listLimit = 200

// Create stores one routine.
func (r *Repository) Create(ctx context.Context, scope domain.Scope, routine domain.Routine) (domain.Routine, error) {
	var stored domain.Routine

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.CreateRoutine(ctx, sqlcgen.CreateRoutineParams{
			WorkspaceID:   scope.WorkspaceID,
			AgentID:       routine.AgentID,
			Name:          routine.Name,
			Kind:          routine.Kind,
			Minute:        int32(routine.Minute),
			HourStart:     hour(routine.HourStart),
			HourEnd:       hour(routine.HourEnd),
			DayOfWeek:     optionalDay(routine.DayOfWeek),
			TriggerSource: routine.TriggerSource,
			Instruction:   routine.Instruction,
			Enabled:       routine.Enabled,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// The insert is a SELECT, so a Bolu outside this workspace comes
				// back as no rows rather than as a constraint violation.
				return fmt.Errorf("%w: the bolu is not in this workspace", domain.ErrInvalidInput)
			}
			return translate(err, "create routine")
		}

		stored = fromCreate(row)
		return nil
	})
	if err != nil {
		return domain.Routine{}, err
	}

	return stored, nil
}

// Get returns one routine.
func (r *Repository) Get(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Routine, error) {
	var routine domain.Routine

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetRoutine(ctx, sqlcgen.GetRoutineParams{ID: id, WorkspaceID: scope.WorkspaceID})
		if err != nil {
			return translate(err, "get routine")
		}

		routine = fromGet(row)
		return nil
	})
	if err != nil {
		return domain.Routine{}, err
	}

	return routine, nil
}

// List returns the routines the screen shows.
func (r *Repository) List(ctx context.Context, scope domain.Scope, filter domain.Filter) ([]domain.Routine, error) {
	var routines []domain.Routine

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListRoutines(ctx, sqlcgen.ListRoutinesParams{
			WorkspaceID: scope.WorkspaceID,
			AgentID:     optionalID(filter.AgentID),
			Enabled:     filter.Enabled,
			Limit:       listLimit,
		})
		if err != nil {
			return translate(err, "list routines")
		}

		routines = make([]domain.Routine, 0, len(rows))
		for _, row := range rows {
			routines = append(routines, fromList(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return routines, nil
}

// ListByTrigger returns the enabled routines watching one event.
func (r *Repository) ListByTrigger(ctx context.Context, scope domain.Scope, triggerSource string) ([]domain.Routine, error) {
	var routines []domain.Routine

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListRoutinesByTrigger(ctx, sqlcgen.ListRoutinesByTriggerParams{
			WorkspaceID:   scope.WorkspaceID,
			TriggerSource: triggerSource,
		})
		if err != nil {
			return translate(err, "list routines by trigger")
		}

		routines = make([]domain.Routine, 0, len(rows))
		for _, row := range rows {
			routines = append(routines, fromTrigger(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return routines, nil
}

// ListEnabled returns every enabled routine of the workspace.
func (r *Repository) ListEnabled(ctx context.Context, scope domain.Scope) ([]domain.Routine, error) {
	var routines []domain.Routine

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListEnabledRoutines(ctx, scope.WorkspaceID)
		if err != nil {
			return translate(err, "list enabled routines")
		}

		routines = make([]domain.Routine, 0, len(rows))
		for _, row := range rows {
			routines = append(routines, fromEnabled(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return routines, nil
}

// Update stores a change to one routine.
func (r *Repository) Update(ctx context.Context, scope domain.Scope, routine domain.Routine) (domain.Routine, error) {
	var stored domain.Routine

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.UpdateRoutine(ctx, sqlcgen.UpdateRoutineParams{
			ID:            routine.ID,
			WorkspaceID:   scope.WorkspaceID,
			AgentID:       routine.AgentID,
			Name:          routine.Name,
			Kind:          routine.Kind,
			Minute:        int32(routine.Minute),
			HourStart:     hour(routine.HourStart),
			HourEnd:       hour(routine.HourEnd),
			DayOfWeek:     optionalDay(routine.DayOfWeek),
			TriggerSource: routine.TriggerSource,
			Instruction:   routine.Instruction,
			Enabled:       routine.Enabled,
			PausedReason:  routine.PausedReason,
		})
		if err != nil {
			return translate(err, "update routine")
		}

		stored = fromUpdate(row)
		return nil
	})
	if err != nil {
		return domain.Routine{}, err
	}

	return stored, nil
}

// SetScheduleID records the engine's id for one routine.
func (r *Repository) SetScheduleID(ctx context.Context, scope domain.Scope, id uuid.UUID, scheduleID string) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		return translate(queries.SetRoutineScheduleID(ctx, sqlcgen.SetRoutineScheduleIDParams{
			ID:          id,
			WorkspaceID: scope.WorkspaceID,
			ScheduleID:  scheduleID,
		}), "set schedule id")
	})
}

// Delete removes one routine, keeping its runs.
func (r *Repository) Delete(ctx context.Context, scope domain.Scope, id uuid.UUID) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.DeleteRoutine(ctx, sqlcgen.DeleteRoutineParams{ID: id, WorkspaceID: scope.WorkspaceID})
		if err != nil {
			return translate(err, "delete routine")
		}
		if rows == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
}

// StartRun opens one run.
func (r *Repository) StartRun(ctx context.Context, scope domain.Scope, run domain.Run) (domain.Run, error) {
	var stored domain.Run

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.StartRoutineRun(ctx, sqlcgen.StartRoutineRunParams{
			WorkspaceID: scope.WorkspaceID,
			RoutineID:   optionalID(run.RoutineID),
			RoutineName: run.RoutineName,
			Status:      run.Status,
		})
		if err != nil {
			return translate(err, "start run")
		}

		stored = fromStartRun(row)
		return nil
	})
	if err != nil {
		return domain.Run{}, err
	}

	return stored, nil
}

// FinishRun records how a run ended, once.
//
// The update only matches a run that has not finished, so a retried activity does
// not overwrite the first outcome. When nothing matched, the row is read back and
// returned: "this run already has an outcome" is the answer the caller asked for,
// and it is the same answer the first attempt gave.
func (r *Repository) FinishRun(ctx context.Context, scope domain.Scope, finish domain.FinishRun) (domain.Run, error) {
	var stored domain.Run

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.FinishRoutineRun(ctx, sqlcgen.FinishRoutineRunParams{
			ID:          finish.RunID,
			WorkspaceID: scope.WorkspaceID,
			Status:      finish.Status,
			Summary:     finish.Summary,
			TaskID:      optionalID(finish.TaskID),
			FinishedAt:  new(finish.FinishedAt),
		})
		if err == nil {
			stored = fromFinishRun(row)
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return translate(err, "finish run")
		}

		existing, readErr := queries.GetRoutineRun(ctx, sqlcgen.GetRoutineRunParams{
			ID:          finish.RunID,
			WorkspaceID: scope.WorkspaceID,
		})
		if readErr != nil {
			return translate(readErr, "finish run")
		}

		stored = fromGetRun(existing)
		return nil
	})
	if err != nil {
		return domain.Run{}, err
	}

	return stored, nil
}

// LatestRun returns the newest run of one routine.
func (r *Repository) LatestRun(ctx context.Context, scope domain.Scope, routineID uuid.UUID) (domain.Run, error) {
	var run domain.Run

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.LatestRoutineRun(ctx, sqlcgen.LatestRoutineRunParams{
			WorkspaceID: scope.WorkspaceID,
			RoutineID:   &routineID,
		})
		if err != nil {
			return translate(err, "latest run")
		}

		run = fromLatestRun(row)
		return nil
	})
	if err != nil {
		return domain.Run{}, err
	}

	return run, nil
}

// AttachTask records the task a run opened.
func (r *Repository) AttachTask(ctx context.Context, scope domain.Scope, runID, taskID uuid.UUID) (domain.Run, error) {
	var stored domain.Run

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.AttachRoutineRunTask(ctx, sqlcgen.AttachRoutineRunTaskParams{
			ID:          runID,
			WorkspaceID: scope.WorkspaceID,
			TaskID:      optionalID(taskID),
		})
		if err != nil {
			return translate(err, "attach task")
		}

		stored = fromAttachTask(row)
		return nil
	})
	if err != nil {
		return domain.Run{}, err
	}

	return stored, nil
}

// RunByTask returns the run a task belongs to.
func (r *Repository) RunByTask(ctx context.Context, scope domain.Scope, taskID uuid.UUID) (domain.Run, error) {
	var run domain.Run

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetRoutineRunByTask(ctx, sqlcgen.GetRoutineRunByTaskParams{
			WorkspaceID: scope.WorkspaceID,
			TaskID:      &taskID,
		})
		if err != nil {
			return translate(err, "run by task")
		}

		run = fromRunByTask(row)
		return nil
	})
	if err != nil {
		return domain.Run{}, err
	}

	return run, nil
}

// ListRuns returns the run history.
func (r *Repository) ListRuns(ctx context.Context, scope domain.Scope, filter domain.RunFilter) ([]domain.Run, error) {
	var runs []domain.Run

	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > listLimit {
		limit = listLimit
	}

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListRoutineRuns(ctx, sqlcgen.ListRoutineRunsParams{
			WorkspaceID: scope.WorkspaceID,
			RoutineID:   optionalID(filter.RoutineID),
			AgentID:     optionalID(filter.AgentID),
			Status:      optionalString(filter.Status),
			Limit:       int32(limit),
		})
		if err != nil {
			return translate(err, "list runs")
		}

		runs = make([]domain.Run, 0, len(rows))
		for _, row := range rows {
			runs = append(runs, fromListRun(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return runs, nil
}

// Agents returns the Bolu of the workspace with the name and the rest switch.
func (r *Repository) Agents(ctx context.Context, scope domain.Scope) ([]domain.AgentInfo, error) {
	var agents []domain.AgentInfo

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListRoutineAgents(ctx, scope.WorkspaceID)
		if err != nil {
			return translate(err, "list agents")
		}

		agents = make([]domain.AgentInfo, 0, len(rows))
		for _, row := range rows {
			agents = append(agents, domain.AgentInfo{ID: row.ID, Name: row.Name, Resting: row.Resting})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return agents, nil
}

// ----------------------------------------------------------------- helpers

func (r *Repository) read(ctx context.Context, scope domain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("routine: database is not configured")
	}
	return r.pool.InScopeRead(ctx, database.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}, func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

func (r *Repository) write(ctx context.Context, scope domain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("routine: database is not configured")
	}
	return r.pool.InScope(ctx, database.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}, func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

func translate(err error, operation string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", domain.ErrNotFound, operation)
	}
	return fmt.Errorf("routine: %s: %w", operation, err)
}

// optionalID writes an id that may be absent. A zero uuid in the database would be
// a reference to a row nobody has, so absent is nil.
func optionalID(id uuid.UUID) *uuid.UUID {
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

// hour writes a wall-clock hour, validating nothing here because the service
// already refused anything outside 0-23. It is not optional: the column is
// nullable only because the bootstrap migration allowed it, and a routine that did
// not use an hour would be a cadence the form cannot produce.
func hour(value int) *int32 {
	return new(int32(value))
}

// optionalDay writes a weekday, where zero genuinely means "this cadence does not
// use one".
func optionalDay(day int) *int32 {
	if day == 0 {
		return nil
	}
	return new(int32(day))
}
