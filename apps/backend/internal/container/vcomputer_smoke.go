// The seam the smoke command reaches the virtual computer's module through.
//
// cmd/* may not import a module: tools/archcheck's cmd-through-container rule says a binary
// is a composition root, and composition happens in the container, so a second binary that
// assembled the module itself would be a second place the module's own shape is decided. The
// rule is why this file exists — what it holds is what cmd/vcomputer-smoke needs and the
// container did not yet expose: the workflow it starts, that workflow's input and result, the
// timeout rule a deployment applies, and a client to ask the control plane for one more
// capture after the run.
package container

import (
	temporalworkflow "go.temporal.io/sdk/workflow"

	vcomputerdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/vcomputer/domain"
	vcomputerservice "github.com/anakbolu/timbolu/apps/backend/internal/modules/vcomputer/service"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/vcomputer/workflow"
	"github.com/anakbolu/timbolu/packages/platform/config"
)

// VComputerSmokeInput is the input the smoke workflow takes, and VComputerSmokeResult is what
// it answered.
//
// They are aliases rather than copies, and that is the point: the command builds the input as
// a struct literal a person can read and decodes the result into a variable, while the worker
// registers the workflow that takes and returns these very types. A copy here would be a shape
// to keep in step with the workflow's for no gain, and one that drifts is a payload the worker
// cannot decode.
type (
	VComputerSmokeInput  = workflow.SmokeInput
	VComputerSmokeResult = workflow.SmokeResult
)

// VComputerSmokeWorkflow returns the workflow the smoke command starts.
//
// It is a function rather than an exported variable so nothing can reassign the workflow of
// every process that imports the container, and so the value it hands back is visibly the one
// RegisterVComputerTasks registers: the SDK takes a workflow's name from the function itself,
// so a worker and this command cannot disagree about it — the name stays "SmokeWorkflow",
// which is what a deployed worker already knows.
func VComputerSmokeWorkflow() func(temporalworkflow.Context, workflow.SmokeInput) (workflow.SmokeResult, error) {
	return workflow.SmokeWorkflow
}

// VComputerDefaultTimeout is the bound a control-plane call gets when the configuration sets no
// positive one.
//
// It is re-exported rather than restated, because the command reports it when
// VCOMPUTER_CONTROL_TIMEOUT is absent or will not parse: a copy of the number here would be a
// default that reads right and is not the one being enforced (issue #257).
const VComputerDefaultTimeout = vcomputerservice.DefaultTimeout

// NewVComputerClient builds the control-plane client the command uses for its final capture.
//
// It applies the timeout the activity set's own client applies, through the same rule, because
// the capture is a call to the same control plane as the run's activities: a client built with
// the configured value as given would be the one call of a smoke run without the bound a
// deployment has, and it is asked of a sandbox that may have just been started cold.
//
// An empty address is not refused here. The client answers ErrNotConfigured on every call,
// which is the honest report of a process pointed at nothing — and the command refuses the
// empty address before it gets this far, so this is about a caller other than it.
func NewVComputerClient(cfg config.VComputerConfig) vcomputerdomain.ComputerUseClient {
	return vcomputerservice.New(cfg.ControlURL, cfg.Token, vcomputerservice.EffectiveTimeout(cfg.Timeout))
}
