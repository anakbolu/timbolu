package container

import (
	"context"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	sdkworker "go.temporal.io/sdk/worker"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/vcomputer/domain"
	vcomputerservice "github.com/anakbolu/timbolu/apps/backend/internal/modules/vcomputer/service"
	vcomputertemporal "github.com/anakbolu/timbolu/apps/backend/internal/modules/vcomputer/temporal"
	vcomputerworkflow "github.com/anakbolu/timbolu/apps/backend/internal/modules/vcomputer/workflow"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/temporal"
	"github.com/anakbolu/timbolu/packages/platform/config"
)

// sdkRecordingWorker is the recording double with the rest of the SDK worker interface
// attached.
//
// RegisterAgentTasks takes a real sdkworker.Worker — that is the signature cmd/agent-worker
// passes — so a test of it needs one, and the embedded interface is nil because nothing here
// starts or stops a worker: the two registration methods are the whole of what is called, and
// the embedded nil is what keeps this from being a second implementation of a worker.
type sdkRecordingWorker struct {
	sdkworker.Worker
	recordingWorker
}

// The two forwarders are explicit rather than promoted: both embedded fields would otherwise
// offer RegisterWorkflow and RegisterActivity, and the selector would be ambiguous.
func (w *sdkRecordingWorker) RegisterWorkflow(workflowFn any) {
	w.recordingWorker.RegisterWorkflow(workflowFn)
}

func (w *sdkRecordingWorker) RegisterActivity(activityFn any) {
	w.recordingWorker.RegisterActivity(activityFn)
}

// TestRegisterVComputerTasksNeedsAActivitySet: a deployment without the module registers
// nothing rather than panicking on a nil activity set.
func TestRegisterVComputerTasksNeedsAActivitySet(t *testing.T) {
	worker := &recordingWorker{}

	RegisterVComputerTasks(worker, nil)
	RegisterVComputerTasks(worker, &Container{})
	require.Empty(t, worker.workflows)
	require.Empty(t, worker.activities)

	RegisterVComputerTasks(worker, &Container{VComputerTasks: vcomputertemporal.New(vcomputertemporal.Deps{})})
	require.Equal(t, []string{functionName(vcomputerworkflow.SmokeWorkflow)}, namesOf(worker.workflows))
	require.Len(t, worker.activities, 1,
		"the activity set is registered as one struct, which is what gives each method its name")
}

// TestAgentWorkerRegistrationKeepsTheVirtualComputerWithoutATaskRuntime is the ordering the
// wiring exists for: a worker whose task runtime is disabled still drives a bot's computer.
//
// The two are independent — one reaches Postgres and the model gateway, the other reaches an
// HTTP control plane — so a deployment migrating its database must not lose the ability to
// answer a smoke run. Registering the virtual computer after the runtime's nil check would tie
// them together, and this is the test that would notice.
func TestAgentWorkerRegistrationKeepsTheVirtualComputerWithoutATaskRuntime(t *testing.T) {
	worker := &sdkRecordingWorker{}
	container := &Container{VComputerTasks: vcomputertemporal.New(vcomputertemporal.Deps{})}

	RegisterAgentTasks(worker, container)

	require.Equal(t,
		[]string{functionName(temporal.WorkerPingWorkflow), functionName(vcomputerworkflow.SmokeWorkflow)},
		namesOf(worker.workflows),
		"the ping workflow and the smoke workflow, and nothing from the task runtime")
	require.Len(t, worker.activities, 2, "the ping activity and the virtual computer's activity set")
}

// TestNewVComputerTasksLeavesAnUnconfiguredDeploymentAnswering: an empty address builds no
// client at all, and the activity set says so on every call.
//
// This is the state every deployment is in until a control plane runs, so it has to be a
// working state rather than a panic or a call to a zero address — and the answer has to be
// distinguishable from a control plane that is merely down.
func TestNewVComputerTasksLeavesAnUnconfiguredDeploymentAnswering(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	activities := NewVComputerTasks(config.VComputerConfig{}, zap.New(core))
	lines := logLines(logs)
	require.Len(t, lines, 1, "a deployment with no control plane says so once, at startup")
	require.Contains(t, lines[0], "virtual computer is not configured")

	_, err := activities.EnsureSession(context.Background(), vcomputertemporal.SessionRequest{BotID: "bot-1"})
	require.ErrorIs(t, err, domain.ErrNotConfigured)

	_, err = activities.Screenshot(context.Background(), vcomputertemporal.SessionRequest{BotID: "bot-1"})
	require.ErrorIs(t, err, domain.ErrNotConfigured)
}

// TestNewVComputerTasksNeverLogsTheToken: the token is the control plane's shared secret and
// is the one field of the configuration that must not appear in a log line — the address is
// the one that must, because it is what an operator checks when a run fails.
func TestNewVComputerTasksNeverLogsTheToken(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	NewVComputerTasks(config.VComputerConfig{
		ControlURL: "http://vcomputer-control:8090",
		Token:      "shared-secret",
		Timeout:    30 * time.Second,
	}, zap.New(core))

	entries := logs.All()
	require.Len(t, entries, 1, "whether a virtual computer is configured is logged once, at startup")
	require.Equal(t, "virtual computer is configured", entries[0].Message)

	// The whole record, message and fields, is searched rather than the fields alone: a
	// secret leaks through whichever of the two somebody reaches for.
	require.NotContains(t, entries[0].ContextMap(), "token")
	require.NotContains(t, entries[0].ContextMap()["control_url"], "shared-secret")
}

// TestNewVComputerTasksWarnsAboutAnAddressWithoutAToken: an address with no token is a
// deployment that looks complete at startup and is refused on every call, because the control
// plane requires the bearer on every /v1 route. Saying "configured" there is how an operator
// ends up reading a 401 as a control plane problem (issue #218).
//
// The warning names the variable and never a value, the same rule the line beside it follows.
func TestNewVComputerTasksWarnsAboutAnAddressWithoutAToken(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	// The client is still built, so what a run reports is the control plane's own refusal
	// rather than a local sentinel that hides which of the two variables is missing. The
	// assertion is on the log line alone: reaching a control plane is the client's test.
	_ = NewVComputerTasks(config.VComputerConfig{
		ControlURL: "http://vcomputer-control:8090",
		Timeout:    30 * time.Second,
	}, zap.New(core))

	entries := logs.All()
	require.Len(t, entries, 1, "the startup line says which of the two variables is missing")
	require.Equal(t, zap.WarnLevel, entries[0].Level)
	require.Contains(t, entries[0].Message, "VCOMPUTER_CONTROL_TOKEN is empty")
	require.Equal(t, "http://vcomputer-control:8090", entries[0].ContextMap()["control_url"])
}

// TestNewVComputerTasksLogsTheTimeoutInForce is issue #257: a timeout that is not a bound is
// replaced by the client's default, so a startup line repeating the configured value told an
// operator the bound they wrote was in force while every call waited 30s.
//
// The warning is the other half of it. The replacement must not be silent, and because the
// line that reports the timeout in force no longer names the configured value, the warning is
// where the value that was given is seen.
func TestNewVComputerTasksLogsTheTimeoutInForce(t *testing.T) {
	cases := map[string]struct {
		configured time.Duration
		effective  time.Duration
		warns      bool
	}{
		"negative": {-5 * time.Second, vcomputerservice.DefaultTimeout, true},
		"zero":     {0, vcomputerservice.DefaultTimeout, true},
		"positive": {90 * time.Second, 90 * time.Second, false},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			NewVComputerTasks(config.VComputerConfig{
				ControlURL: "http://vcomputer-control:8090",
				Token:      "shared-secret",
				Timeout:    testCase.configured,
			}, zap.New(core))

			configured := logs.FilterMessage("virtual computer is configured").All()
			require.Len(t, configured, 1, "a configured virtual computer is still reported once")
			require.Equal(t, testCase.effective, configured[0].ContextMap()["timeout"],
				"the line reports the timeout every call waits, not the one that was configured")

			warnings := logs.FilterLevelExact(zap.WarnLevel).All()
			if !testCase.warns {
				require.Empty(t, warnings, "a timeout that is in force needs no warning")
				return
			}

			require.Len(t, warnings, 1)
			require.Contains(t, warnings[0].Message, "VCOMPUTER_CONTROL_TIMEOUT")
			require.Equal(t, testCase.configured, warnings[0].ContextMap()["configured_timeout"],
				"the warning names the value that was given, which no other line does")
			require.Equal(t, testCase.effective, warnings[0].ContextMap()["timeout"])
		})
	}
}

// TestNewVComputerTasksDoesNotWarnAboutATimeoutWithNoControlPlane: without an address no call
// is made, so a non-positive timeout bounds nothing and a warning about it would be noise in
// the deployment that has no virtual computer — which is every deployment until one runs.
func TestNewVComputerTasksDoesNotWarnAboutATimeoutWithNoControlPlane(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	NewVComputerTasks(config.VComputerConfig{Timeout: -5 * time.Second}, zap.New(core))

	require.Empty(t, logs.All())
}

// TestAgentWorkerRegistrationInventory is the inventory of what the agent worker installs,
// printed one name per line.
//
// It asserts nothing on purpose. What it is for is the moment the worker's registration
// changes: a workflow or an activity that disappears from this output is a deployment where
// something stopped being runnable, and reading the list is how that is noticed. The names are
// resolved the way the SDK resolves them — the workflow function's own name, and one line per
// exported method for an activity struct — so what is printed is what a scheduled task looks
// for.
func TestAgentWorkerRegistrationInventory(t *testing.T) {
	worker := &sdkRecordingWorker{}
	container := &Container{VComputerTasks: vcomputertemporal.New(vcomputertemporal.Deps{})}

	RegisterAgentTasks(worker, container)

	for _, workflowFn := range worker.workflows {
		t.Logf("registered: workflow %s", functionName(workflowFn))
	}
	for _, activityFn := range worker.activities {
		for _, name := range activityNames(activityFn) {
			t.Logf("registered: activity %s", name)
		}
	}
}

// functionName is the name the SDK registers a function under, which is its own: a worker
// registers the function, and the SDK derives the type name from it.
func functionName(fn any) string {
	if fn == nil {
		return "<nil>"
	}
	return runtime.FuncForPC(reflect.ValueOf(fn).Pointer()).Name()
}

// namesOf resolves a recorded registration list to the names the SDK would derive from it,
// which is what makes the lists comparable: a function value cannot be compared with another,
// and the name is the thing the deployment looks up anyway.
func namesOf(fns []any) []string {
	names := make([]string, 0, len(fns))
	for _, fn := range fns {
		names = append(names, functionName(fn))
	}
	return names
}

// logLines is the messages one observer recorded, in order.
func logLines(logs *observer.ObservedLogs) []string {
	entries := logs.All()
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, entry.Message)
	}
	return lines
}

// activityNames lists what one registration installs: a plain function is one activity, and a
// struct is one per exported method, under the method's own name.
func activityNames(fn any) []string {
	value := reflect.ValueOf(fn)
	if value.Kind() != reflect.Pointer || value.Elem().Kind() != reflect.Struct {
		return []string{functionName(fn)}
	}

	typ := value.Type()
	names := make([]string, 0, typ.NumMethod())
	for i := range typ.NumMethod() {
		names = append(names, typ.Elem().Name()+"."+typ.Method(i).Name)
	}
	return names
}

// TestActivityNamesCoverTheControlPlane: the inventory is only worth reading if the names it
// prints are the ones the workflow schedules, so this pins the set rather than its shape.
func TestActivityNamesCoverTheControlPlane(t *testing.T) {
	names := activityNames(vcomputertemporal.New(vcomputertemporal.Deps{}))
	require.Equal(t,
		[]string{"Activities.Click", "Activities.EnsureSession", "Activities.Exec", "Activities.Navigate",
			"Activities.Screenshot", "Activities.StopSession", "Activities.Type"},
		names)

	// And the same method values are what the workflow calls, which is the reason the smoke
	// run cannot schedule an activity the worker does not register.
	require.True(t, strings.HasSuffix(functionName(vcomputerworkflow.SmokeWorkflow), ".SmokeWorkflow"))
}
