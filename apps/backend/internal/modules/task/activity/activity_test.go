package activity

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/workflow"
)

// TestEveryActivityNameHasAMethod is the contract between the workflow and the
// worker.
//
// The workflow schedules an activity by name; the worker registers a method by
// its Go name. Nothing in the compiler connects the two, and a mismatch does not
// fail loudly: the activity task is scheduled, no worker can run it, and the task
// stalls until its timeout. That is exactly the failure this test prevents.
func TestEveryActivityNameHasAMethod(t *testing.T) {
	names := []string{
		workflow.ActivityLoadContext,
		workflow.ActivityCallModel,
		workflow.ActivityRunTool,
		workflow.ActivityRequestDraft,
		workflow.ActivityRecordStep,
		workflow.ActivityBeat,
		workflow.ActivityComplete,
		workflow.ActivityPrepareHandoff,
		workflow.ActivityMarkHandoffStarted,
		workflow.ActivityResume,
	}

	methods := map[string]bool{}
	activities := reflect.TypeOf(&Activities{})
	for index := range activities.NumMethod() {
		methods[activities.Method(index).Name] = true
	}

	for _, name := range names {
		require.True(t, methods[name],
			"the workflow schedules %q but no activity method of that name is registered on the worker", name)
	}
}

// TestRegisteredActivityCountMatchesTheWorkflow guards the other direction: an
// activity that no workflow calls is dead weight, and one the workflow calls but
// the worker does not register is a stall.
func TestRegisteredActivityCountMatchesTheWorkflow(t *testing.T) {
	activities := reflect.TypeOf(&Activities{})
	require.Equal(t, 10, activities.NumMethod(),
		"a new activity method must be registered in the workflow's name list too")
}
