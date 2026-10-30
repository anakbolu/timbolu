package workflow

import (
	"context"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/routine/domain"
)

const (
	routineID   = "11111111-1111-1111-1111-111111111111"
	workspaceID = "33333333-3333-3333-3333-333333333333"
)

// TestTheWorkflowNameIsTheFunctionName is the contract no compiler checks: the worker
// registers a workflow by the name of the function it is handed, while the scheduler
// starts one by this string. When they drift apart the run finds no worker and retries
// until it times out, which looks like a slow routine rather than like a bug.
func TestTheWorkflowNameIsTheFunctionName(t *testing.T) {
	require.Equal(t, "RoutineRunWorkflow", WorkflowName)

	full := runtime.FuncForPC(reflect.ValueOf(RoutineRunWorkflow).Pointer()).Name()
	require.Equal(t, WorkflowName, full[strings.LastIndex(full, ".")+1:])
}

// TestTheActivityNameIsTheMethodName is the same guard for the one activity the
// workflow calls.
func TestTheActivityNameIsTheMethodName(t *testing.T) {
	require.Equal(t, "RunRoutine", ActivityRun)
}

// stub carries the one method the workflow schedules, under the name it calls it by.
type stub struct{}

func (stub) RunRoutine(context.Context, domain.RunInput) (RunResult, error) {
	return RunResult{}, nil
}

// TestTheWorkflowPerformsOneTick proves the run reaches the activity with the ids it
// was started with, and that what the activity reports is what the workflow returns.
func TestTheWorkflowPerformsOneTick(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(RoutineRunWorkflow)
	env.RegisterActivityWithOptions(stub{}.RunRoutine, activity.RegisterOptions{Name: ActivityRun})

	var passed domain.RunInput
	env.OnActivity(ActivityRun, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input domain.RunInput) (RunResult, error) {
			passed = input
			return RunResult{RunID: "a-run", Status: domain.RunRunning, Summary: "dijalankan"}, nil
		}).Once()

	env.ExecuteWorkflow(RoutineRunWorkflow, domain.RunInput{RoutineID: routineID, WorkspaceID: workspaceID})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result RunResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, "a-run", result.RunID)
	require.Equal(t, domain.RunRunning, result.Status)

	require.Equal(t, routineID, passed.RoutineID, "the tick is performed for the routine it was started for")
	require.Equal(t, workspaceID, passed.WorkspaceID, "and inside the workspace that owns it")
}

// TestATickThatCouldNotDispatchReturnsTheError: the activity records a failed run when
// it cannot dispatch, so a returned error means nothing could be recorded at all — and
// that is worth surfacing rather than swallowing.
func TestATickThatCouldNotDispatchReturnsTheError(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(RoutineRunWorkflow)
	env.RegisterActivityWithOptions(stub{}.RunRoutine, activity.RegisterOptions{Name: ActivityRun})

	env.OnActivity(ActivityRun, mock.Anything, mock.Anything).Return(
		func(context.Context, domain.RunInput) (RunResult, error) {
			return RunResult{}, context.DeadlineExceeded
		}).Once()

	env.ExecuteWorkflow(RoutineRunWorkflow, domain.RunInput{RoutineID: routineID, WorkspaceID: workspaceID})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
}

// TestTheTickIsNotRetried is why a routine cannot double-execute on a retry: one
// tick is one run, so the activity is attempted once. A retry would open a second run
// for a single slot of the calendar, which is the duplicate the epic's first gate
// forbids.
func TestTheTickIsNotRetried(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(RoutineRunWorkflow)
	env.RegisterActivityWithOptions(stub{}.RunRoutine, activity.RegisterOptions{Name: ActivityRun})

	attempts := 0
	env.OnActivity(ActivityRun, mock.Anything, mock.Anything).Return(
		func(context.Context, domain.RunInput) (RunResult, error) {
			attempts++
			return RunResult{}, context.DeadlineExceeded
		})

	env.ExecuteWorkflow(RoutineRunWorkflow, domain.RunInput{RoutineID: routineID, WorkspaceID: workspaceID})
	require.Error(t, env.GetWorkflowError())

	require.Equal(t, 1, attempts, "one tick is one attempt, so a retry cannot open a second run")
}
