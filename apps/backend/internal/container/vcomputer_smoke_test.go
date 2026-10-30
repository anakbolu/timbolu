package container

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/vcomputer/domain"
	vcomputertemporal "github.com/anakbolu/timbolu/apps/backend/internal/modules/vcomputer/temporal"
	vcomputerworkflow "github.com/anakbolu/timbolu/apps/backend/internal/modules/vcomputer/workflow"
	"github.com/anakbolu/timbolu/packages/platform/config"
)

// TestVComputerSmokeWorkflowIsTheOneTheWorkerRegisters: the command starts the workflow by the
// value this seam hands it, and the SDK takes a workflow's name from the function itself.
//
// That is the failure a wrapper would cause and no other test would catch: a function that
// merely calls SmokeWorkflow compiles, runs, and registers under its own name, so the command
// would ask a deployed worker for a type it has never heard of. Comparing the function's own
// name to the registered one is what holds the two together.
func TestVComputerSmokeWorkflowIsTheOneTheWorkerRegisters(t *testing.T) {
	worker := &recordingWorker{}
	RegisterVComputerTasks(worker, &Container{VComputerTasks: vcomputertemporal.New(vcomputertemporal.Deps{})})
	require.Len(t, worker.workflows, 1)

	started := functionName(VComputerSmokeWorkflow())
	require.Equal(t, functionName(vcomputerworkflow.SmokeWorkflow), started)
	require.Equal(t, namesOf(worker.workflows), []string{started},
		"the command must start exactly the workflow the worker registered")
	require.True(t, strings.HasSuffix(started, ".SmokeWorkflow"),
		"the SDK names a workflow by the last segment of the function name, so %q is a type "+
			"no deployed worker answers", started)
}

// TestNewVComputerClientReportsAMissingControlPlane: the command's final capture has to fail
// with the missing configuration rather than reach a zero address or panic on a nil client.
//
// It is the same answer the activity set gives in the same state, which is what makes a smoke
// run's last step readable: "there is no control plane" is one sentence whichever step says it.
func TestNewVComputerClientReportsAMissingControlPlane(t *testing.T) {
	client := NewVComputerClient(config.VComputerConfig{})
	require.NotNil(t, client)

	_, err := client.Screenshot(context.Background(), "bot-1")
	require.ErrorIs(t, err, domain.ErrNotConfigured)
}
