package workflow

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/vcomputer/domain"
	vcomputertemporal "github.com/robzlabz/employeebot/apps/backend/internal/modules/vcomputer/temporal"
)

// smokeHarness is one test environment with every activity the smoke run calls scripted.
//
// The expectations are attached to the method values themselves — on a nil receiver, the
// same way the workflow calls them — so the names the run schedules are the names this
// harness mocks, by construction rather than by a string that could drift.
type smokeHarness struct {
	env *testsuite.TestWorkflowEnvironment

	// order is the activities that ran, in the order they ran.
	order []string

	// What each activity answers, and what it was handed.
	session     domain.Session
	sessionErr  error
	navigated   string
	navigateErr error
	// before and after are the PNG bytes of the two captures, in call order.
	before  []byte
	after   []byte
	click   vcomputertemporal.ClickRequest
	text    vcomputertemporal.TypeRequest
	exec    vcomputertemporal.ExecRequest
	execOut domain.ExecResult
	// stopped is the bot of every StopSession the run scheduled, and stopErr is what it
	// answers. The bot id is recorded rather than a count because the cleanup is the one
	// call whose whole point is naming the sandbox to take down.
	stopped []string
	stopErr error
}

func newSmokeHarness(t *testing.T) *smokeHarness {
	t.Helper()

	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(SmokeWorkflow)

	var activities *vcomputertemporal.Activities
	h := &smokeHarness{env: env}

	env.OnActivity(activities.EnsureSession, mock.Anything, mock.Anything).Return(
		func(context.Context, vcomputertemporal.SessionRequest) (domain.Session, error) {
			h.order = append(h.order, "EnsureSession")
			return h.session, h.sessionErr
		})

	env.OnActivity(activities.Navigate, mock.Anything, mock.Anything).Return(
		func(_ context.Context, request vcomputertemporal.NavigateRequest) error {
			h.order = append(h.order, "Navigate")
			h.navigated = request.URL
			return h.navigateErr
		})

	// The two captures are told apart by their call order rather than by a count: the run
	// takes one after the navigation and one after the click and the text, and a harness that
	// answered the same bytes to both could not tell the two apart either.
	env.OnActivity(activities.Screenshot, mock.Anything, mock.Anything).Return(
		func(context.Context, vcomputertemporal.SessionRequest) (vcomputertemporal.ScreenshotResult, error) {
			capture := h.before
			if len(h.order) > 0 && h.order[len(h.order)-1] == "Type" {
				capture = h.after
			}
			h.order = append(h.order, "Screenshot")
			return vcomputertemporal.ScreenshotResult{PNG: capture, Bytes: len(capture)}, nil
		})

	env.OnActivity(activities.Click, mock.Anything, mock.Anything).Return(
		func(_ context.Context, request vcomputertemporal.ClickRequest) error {
			h.order = append(h.order, "Click")
			h.click = request
			return nil
		})

	env.OnActivity(activities.Type, mock.Anything, mock.Anything).Return(
		func(_ context.Context, request vcomputertemporal.TypeRequest) error {
			h.order = append(h.order, "Type")
			h.text = request
			return nil
		})

	env.OnActivity(activities.Exec, mock.Anything, mock.Anything).Return(
		func(_ context.Context, request vcomputertemporal.ExecRequest) (domain.ExecResult, error) {
			h.order = append(h.order, "Exec")
			h.exec = request
			return h.execOut, nil
		})

	env.OnActivity(activities.StopSession, mock.Anything, mock.Anything).Return(
		func(_ context.Context, request vcomputertemporal.SessionRequest) error {
			h.order = append(h.order, "StopSession")
			h.stopped = append(h.stopped, request.BotID)
			return h.stopErr
		})

	return h
}

func smokeInput() SmokeInput {
	return SmokeInput{
		BotID:  "bot-42",
		URL:    "http://vcomputer-fixture:8080/",
		ClickX: 120,
		ClickY: 240,
		Text:   "halo dari smoke",
	}
}

// TestSmokeWorkflowDrivesEveryStep is what the run is for: every activity the module offers
// reaches the sandbox, in the order a task would use them — the session first, then the page,
// then a capture, the input, the second capture, the command, and last the stop that takes the
// sandbox back down — and what they answered is what the result reports.
func TestSmokeWorkflowDrivesEveryStep(t *testing.T) {
	h := newSmokeHarness(t)
	h.session = domain.Session{State: domain.StateRunning, CDPURL: "http://10.0.0.2:9223"}
	h.before = bytes.Repeat([]byte{1}, 120)
	h.after = bytes.Repeat([]byte{2}, 300)
	h.execOut = domain.ExecResult{Stdout: "vcomputer-smoke\nbolu\n", Stub: true}

	h.env.ExecuteWorkflow(SmokeWorkflow, smokeInput())

	require.True(t, h.env.IsWorkflowCompleted())
	require.NoError(t, h.env.GetWorkflowError())

	var result SmokeResult
	require.NoError(t, h.env.GetWorkflowResult(&result))
	require.Equal(t, domain.StateRunning, result.SessionState)
	require.Equal(t, "http://10.0.0.2:9223", result.CDPURL)
	require.Equal(t, "vcomputer-smoke\nbolu\n", result.ExecStdout)
	require.True(t, result.ExecStub)

	require.Equal(t,
		[]string{"EnsureSession", "Navigate", "Screenshot", "Click", "Type", "Screenshot", "Exec", "StopSession"},
		h.order,
		"every step runs once, in the order a task would use them, and the sandbox is stopped last")

	// The cleanup is what stops a smoke run from leaving a container behind, so the bot it
	// names is asserted rather than the call alone: a stop of some other sandbox is not a
	// cleanup.
	require.Equal(t, []string{"bot-42"}, h.stopped)
	require.True(t, result.SessionStopped)

	// The captures are reported as sizes, and the sizes are the ones the control plane
	// answered: the bytes themselves stay in the activity's own result.
	require.Equal(t, 120, result.ScreenshotBeforeBytes)
	require.Equal(t, 300, result.ScreenshotAfterBytes)

	// Each step carries the input the run was started with, so the coordinates and the text
	// reach the desktop rather than only the workflow's own record of them.
	require.Equal(t, "http://vcomputer-fixture:8080/", h.navigated)
	require.Equal(t, 120, h.click.X)
	require.Equal(t, 240, h.click.Y)
	require.Equal(t, "halo dari smoke", h.text.Text)
	require.Equal(t, "bot-42", h.click.BotID)

	// The command is the run's own check, not the input's text: a run that executed what it
	// was told to type would prove nothing about the sandbox's shell.
	require.Equal(t, smokeCommand, h.exec.Command)
}

// TestSmokeWorkflowStopsWhenNavigateFails: the first step that fails ends the run. A
// navigation the control plane refused means the page is not where the run believes it is, so
// clicking and typing afterwards would press coordinates on whatever was on screen before —
// and a smoke run that reported success from that would be worse than no run at all.
//
// The assertion is on the message rather than on the sentinel, and that is not laziness: an
// activity's error crosses a boundary that keeps only its type and its text, so the identity of
// the value the control plane's classification returned does not survive it — neither in this
// environment nor against a real server. What a caller can rely on is the text, which is why
// the module's sentinels are sentences rather than codes.
func TestSmokeWorkflowStopsWhenNavigateFails(t *testing.T) {
	h := newSmokeHarness(t)
	h.session = domain.Session{State: domain.StateRunning}
	h.navigateErr = domain.ErrInvalidInput

	h.env.ExecuteWorkflow(SmokeWorkflow, smokeInput())

	require.True(t, h.env.IsWorkflowCompleted())
	require.ErrorContains(t, h.env.GetWorkflowError(), domain.ErrInvalidInput.Error())

	require.Equal(t, []string{"EnsureSession", "Navigate", "Navigate", "Navigate", "StopSession"}, h.order,
		"the refused navigation is attempted the configured three times and then fails the run")

	// The sandbox the run made is still taken down. This is the case the cleanup exists for:
	// a run that failed halfway would otherwise leave a container and its volume running with
	// nothing left to stop them.
	require.Equal(t, []string{"bot-42"}, h.stopped)
}

// TestSmokeWorkflowKeepsTheSessionWhenAsked is the opt-out: when the input says so, the run
// leaves the desktop up and says in its result that it did. That is for looking at the screen
// after the run — the reason the command takes a final capture — and the result has to be
// honest about it, because "no container left behind" is what every other run promises.
func TestSmokeWorkflowKeepsTheSessionWhenAsked(t *testing.T) {
	h := newSmokeHarness(t)
	h.session = domain.Session{State: domain.StateRunning}

	in := smokeInput()
	in.KeepSession = true
	h.env.ExecuteWorkflow(SmokeWorkflow, in)

	require.True(t, h.env.IsWorkflowCompleted())
	require.NoError(t, h.env.GetWorkflowError())

	var result SmokeResult
	require.NoError(t, h.env.GetWorkflowResult(&result))
	require.False(t, result.SessionStopped)
	require.Empty(t, h.stopped, "KeepSession means the sandbox is deliberately left running")
	require.Equal(t,
		[]string{"EnsureSession", "Navigate", "Screenshot", "Click", "Type", "Screenshot", "Exec"},
		h.order,
		"every step but the cleanup runs")
}

// TestSmokeWorkflowFailsWhenTheCleanupFails: a session that would not stop is a leftover
// container, which is the whole complaint this cleanup answers. The run reports it as a
// failure rather than as a success with a note, because a run that said it was done while a
// sandbox was still up is exactly the state nobody would look at again.
func TestSmokeWorkflowFailsWhenTheCleanupFails(t *testing.T) {
	h := newSmokeHarness(t)
	h.session = domain.Session{State: domain.StateRunning}
	h.stopErr = domain.ErrUnavailable

	h.env.ExecuteWorkflow(SmokeWorkflow, smokeInput())

	require.True(t, h.env.IsWorkflowCompleted())
	require.ErrorContains(t, h.env.GetWorkflowError(), domain.ErrUnavailable.Error())
	// The cleanup runs under the same retry policy as every other step, so an unavailable
	// control plane is tried the configured three times before the run gives up.
	require.Equal(t,
		[]string{"EnsureSession", "Navigate", "Screenshot", "Click", "Type", "Screenshot", "Exec",
			"StopSession", "StopSession", "StopSession"},
		h.order,
		"the steps ran and the run failed on the cleanup rather than on a step")
}

// TestSmokeWorkflowReportsBothFailures: a step that failed and a sandbox that would not stop
// are different answers, and the second one is the one an operator has to act on — the run
// failed *and* left a container behind. So the run says both rather than dropping the cleanup
// into the step's error, which is what a substitution would do.
func TestSmokeWorkflowReportsBothFailures(t *testing.T) {
	h := newSmokeHarness(t)
	h.session = domain.Session{State: domain.StateRunning}
	h.navigateErr = domain.ErrInvalidInput
	h.stopErr = domain.ErrUnavailable

	h.env.ExecuteWorkflow(SmokeWorkflow, smokeInput())

	require.True(t, h.env.IsWorkflowCompleted())
	err := h.env.GetWorkflowError()
	require.ErrorContains(t, err, domain.ErrInvalidInput.Error())
	require.ErrorContains(t, err, domain.ErrUnavailable.Error())
	require.Equal(t, []string{"bot-42", "bot-42", "bot-42"}, h.stopped,
		"the cleanup is retried like every other step, and every attempt names the sandbox it failed to stop")
}
