// Package workflow holds the durable half of a routine run: one workflow per tick,
// started by the routine's schedule or by an event.
//
// It is deliberately thin. The schedule decides *when* a routine runs and Temporal
// owns that; this workflow decides *what happens* on one tick, and it does so by
// calling exactly one activity. Everything that touches a database, a queue, or a
// clock lives in that activity, because a workflow replayed from its history cannot
// do any of those.
package workflow

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/domain"
)

// WorkflowName is what the worker registers and what the scheduler starts.
//
// It is the Go function's own name, deliberately: the worker registers a workflow by
// the name of the function it is handed, while the scheduler starts one by this
// string. TestTheWorkflowNameIsTheFunctionName keeps them equal — two names that
// agree only by accident is how a scheduled run finds no worker and retries until it
// times out, which looks like a slow routine rather than like a bug.
const WorkflowName = "RoutineRunWorkflow"

// Activity names, referenced by string so the worker registers them without the
// workflow sharing a function value with it.
const (
	// ActivityRun is the whole of one tick: open the run, decide whether there is
	// work, and dispatch it.
	ActivityRun = "RunRoutine"
)

// runTimeout bounds one tick. A routine's work becomes a task that runs out of
// process, so this only covers opening the run and starting that task.
const runTimeout = 2 * time.Minute

// RunResult is what one tick did.
type RunResult struct {
	// RunID is the run that was recorded.
	RunID string
	// Status is the run's status after this tick: running when a task was opened,
	// skipped when there was nothing to do, failed when the task could not be.
	Status string
	// Summary explains a skip or a failure in the user's words.
	Summary string
}

// RoutineRunWorkflow performs one tick of one routine.
//
// It returns the tick's outcome rather than retrying forever: a tick that could not
// dispatch its task has recorded a failed run, and the *next* tick is the schedule's
// job. That is what "a failure does not stop the following run" means in practice —
// the failure is reported, and the calendar keeps its promises.
func RoutineRunWorkflow(ctx workflow.Context, input domain.RunInput) (RunResult, error) {
	runCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: runTimeout,
		// One attempt. The activity records a failed run when it cannot dispatch, so
		// retrying it would open a second run for one tick — which is exactly the
		// double execution the epic's first gate forbids.
		RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1},
	})

	var result RunResult
	if err := workflow.ExecuteActivity(runCtx, ActivityRun, input).Get(runCtx, &result); err != nil {
		return RunResult{}, err
	}

	return result, nil
}
