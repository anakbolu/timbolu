// Package activity holds the side-effecting half of a routine run: the one method the
// worker registers, which turns a tick into a recorded run.
//
// It exists as a thin layer rather than as a Temporal-aware service for the same
// reason the memory module's does: the workflow package must stay free of the
// module's services so a replay stays deterministic, and the service must stay free
// of the SDK so a unit test can call it without a server.
package activity

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/routine/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/routine/workflow"
)

// Deps are the activity dependencies.
type Deps struct {
	Routines domain.Service
	Logger   *zap.Logger
}

// Activities is the set the worker registers.
type Activities struct {
	deps Deps
}

// New builds the activity set.
func New(deps Deps) *Activities {
	return &Activities{deps: deps}
}

// RunRoutine performs one tick of one routine.
//
// It returns a failure only when nothing could be recorded at all: a tick whose task
// could not be opened has already written a `failed` run with the reason, so reporting
// an error as well would make Temporal retry a tick whose outcome is on the record.
func (a *Activities) RunRoutine(ctx context.Context, input domain.RunInput) (workflow.RunResult, error) {
	if a.deps.Routines == nil {
		return workflow.RunResult{}, errors.New("routine: the service is not configured")
	}

	workspaceID, err := parseID(input.WorkspaceID, "workspace")
	if err != nil {
		return workflow.RunResult{}, err
	}
	routineID, err := parseID(input.RoutineID, "routine")
	if err != nil {
		return workflow.RunResult{}, err
	}

	run, err := a.deps.Routines.RunOnce(ctx, domain.Scope{WorkspaceID: workspaceID}, routineID)
	if err != nil {
		// A routine that was deleted between the tick and this call, or one that was
		// paused, is a normal race rather than a failure: the schedule is removed with
		// the routine, but a tick already in flight still arrives.
		if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrInvalidInput) {
			if a.deps.Logger != nil {
				a.deps.Logger.Info("a routine tick found nothing to run",
					zap.String("routine_id", routineID.String()), zap.Error(err))
			}
			return workflow.RunResult{}, nil
		}
		return workflow.RunResult{}, err
	}

	if a.deps.Logger != nil {
		a.deps.Logger.Info("routine run recorded",
			zap.String("routine_id", routineID.String()),
			zap.String("run_id", run.ID.String()),
			zap.String("status", run.Status),
		)
	}

	return workflow.RunResult{
		RunID:   run.ID.String(),
		Status:  run.Status,
		Summary: run.Summary,
	}, nil
}

// parseID reads an id that travelled through the workflow as a string.
func parseID(value, field string) (uuid.UUID, error) {
	if value == "" {
		return uuid.Nil, fmt.Errorf("%w: %s id is required", domain.ErrInvalidInput, field)
	}

	id, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: %s id is not valid", domain.ErrInvalidInput, field)
	}
	return id, nil
}
