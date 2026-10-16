// Package workflow holds the durable half of the virtual computer: one workflow that
// drives a bot's sandbox through every activity the module offers.
//
// It is a smoke run rather than a task. The agent task loop does not reach the sandbox
// yet (docs/adr/0003-virtual-computer.md), so what this workflow is for is proving the
// whole seam — agent worker → activities → control plane → sandbox — before anything
// depends on it. A deployment with no control plane configured fails it on the first
// step with ErrNotConfigured, which is the honest answer rather than a silent pass.
//
// The two sleeps are the point of a smoke run rather than padding: a browser that was
// just pointed at a page, and a window that was just typed into, are not ready in the
// same instant, so a capture taken immediately would be a capture of the previous
// screen and the run would prove nothing about either step.
//
// The run is also what ends the session it started. That is the one place this differs
// from a task, which deliberately leaves its session up so the next one reuses the
// browser profile: a smoke run leaves nothing behind to delete by hand, because a
// container left up per run is what made QA clean seven of them up manually.
package workflow

import (
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/vcomputer/domain"
	vcomputertemporal "github.com/robzlabz/employeebot/apps/backend/internal/modules/vcomputer/temporal"
)

const (
	// ensureSessionTimeout bounds the one call that may have to create the sandbox: the
	// control plane creates the volume, starts the container and waits for it to answer.
	// It is longer than every other step for that reason.
	ensureSessionTimeout = 90 * time.Second
	// stepTimeout bounds one of the remaining calls: navigate, screenshot, click, type, exec.
	stepTimeout = 30 * time.Second
	// maximumAttempts is how many times one step is attempted. Three is what makes the run
	// tolerate the transient half of the seam — a control plane restarting, a sandbox not yet
	// accepting connections — without turning a real failure into a long wait: the errors the
	// module classifies as permanent (a missing control plane, a refused token, a refused
	// request) come back three times and then fail the run, which is what a smoke run should
	// do.
	maximumAttempts = 3
	// navigateSettle is how long the browser is given to render what it was pointed at.
	navigateSettle = 3 * time.Second
	// inputSettle is how long the desktop is given to show what was clicked and typed.
	inputSettle = 1 * time.Second
	// smokeCommand is the command the run executes in the sandbox. It is fixed rather than
	// taken from the input because it is the check, not the payload: `echo` proves a shell ran
	// and `whoami` proves which user it ran as — the sandbox's whole point is that it is not
	// root, and a command that only printed something would not say.
	smokeCommand = "echo vcomputer-smoke && whoami"
)

// SmokeInput is one run: which bot's computer to drive, where to point its browser, and
// what to click and type once it is there.
//
// The fields are the tool calls a model would make, in the order a model would make them,
// which is why the coordinates are on the input rather than derived: reading a screenshot
// and deciding where to press is the model's job, and the smoke run stands in for it.
type SmokeInput struct {
	BotID string `json:"bot_id"`
	// URL is where Navigate points the browser. The control plane accepts http, https, data
	// and about, and refuses anything else, so the run does not validate it here.
	URL string `json:"url"`
	// ClickX and ClickY are the point to press once the page is up.
	ClickX int `json:"click_x"`
	ClickY int `json:"click_y"`
	// Text is what is typed into the focused window after the click.
	Text string `json:"text"`
	// KeepSession leaves the sandbox running when the run is over. The default — false
	// — is the one that costs nothing: a smoke run is a person watching it, and the
	// sandbox it drove is of no further use to anyone, so leaving it behind only means
	// an operator deleting `vcomputer-<bot>` and its volume by hand later. Setting this
	// is for the case where the desktop itself is what is being looked at afterwards.
	KeepSession bool `json:"keep_session"`
}

// SmokeResult is what the run observed.
//
// It carries the size of each capture rather than the capture itself. The PNG bytes do
// travel through Temporal inside ScreenshotResult — a few hundred kilobytes against a 2 MiB
// payload limit, which the activity package documents as a PoC shortcut — but a result that
// is read by a person, printed and compared run to run has no use for the bytes, and a
// workflow result is kept in history forever.
type SmokeResult struct {
	// SessionState is the state the control plane reported when the session was ensured:
	// "running" is what a working sandbox answers.
	SessionState string `json:"session_state"`
	// CDPURL is the sandbox's DevTools endpoint, `http://<sandbox-ip>:<port>`. It is carried
	// because it is the address the browser is actually driven through, and the one an
	// operator needs when a click lands nowhere.
	CDPURL string `json:"cdp_url"`
	// ScreenshotBeforeBytes is the size of the capture taken after the navigation.
	ScreenshotBeforeBytes int `json:"screenshot_before_bytes"`
	// ScreenshotAfterBytes is the size of the capture taken after the click and the text.
	ScreenshotAfterBytes int `json:"screenshot_after_bytes"`
	// ExecStdout is what the command printed.
	ExecStdout string `json:"exec_stdout"`
	// ExecStub says whether the runtime really ran the command. It is false with the docker
	// runtime and true with the memory one, which echoes the command instead: a run has to
	// read this field rather than the output, because a stub that answered an empty line looks
	// exactly like a command that printed nothing.
	ExecStub bool `json:"exec_stub"`
	// SessionStopped says whether the run ended the session it started. It is the
	// evidence for the cleanup rather than a report about the input: false means either
	// KeepSession asked for the session to be left up, or the run never got as far as
	// having one. A stop that failed fails the run, so a result is only ever returned
	// with the sandbox gone or deliberately kept.
	SessionStopped bool `json:"session_stopped"`
}

// SmokeWorkflow drives one bot's virtual computer through every activity the module offers,
// in the order a task would use them, and reports what each one answered.
//
// It ends the session it started unless KeepSession says otherwise, and it does so on
// every path: the cleanup is deferred and scheduled on a disconnected context, so a run
// that failed halfway still takes its sandbox with it rather than leaving it for an
// operator. A stop that failed is a failure of the run — the leftover container is the
// complaint this answers — and it is reported alongside whatever failed first. That is
// also the end-to-end proof the StopSession activity was missing: registered since the
// activity set was, and never once scheduled by anything.
//
// The activities are called by method value on a nil receiver, which is the SDK's own
// pattern: the name comes from the method, so the set this workflow calls and the set the
// worker registers cannot disagree about it. Nothing of the receiver is dereferenced — a
// method value carries the receiver without touching it.
func SmokeWorkflow(ctx workflow.Context, in SmokeInput) (result SmokeResult, err error) {
	var activities *vcomputertemporal.Activities

	// Both results are named rather than a literal built at the end, because the cleanup
	// below is what records that it ran and what fails the run when it did not: a deferred
	// function can only reach the values the caller receives when the function has named
	// returns, and a mutation of a local would be left behind by the return statement that
	// copied it.

	ensureCtx := workflow.WithActivityOptions(ctx, activityOptions(ensureSessionTimeout))
	stepCtx := workflow.WithActivityOptions(ctx, activityOptions(stepTimeout))

	var session domain.Session
	if err := workflow.ExecuteActivity(ensureCtx, activities.EnsureSession,
		vcomputertemporal.SessionRequest{BotID: in.BotID}).Get(ensureCtx, &session); err != nil {
		return result, err
	}

	// The run owns the sandbox it just made, so it is the run that ends it — on every
	// path, including the one where a later step failed. Leaving it up is what left
	// `vcomputer-<bot>` containers and their volumes behind for an operator to delete by
	// hand, and a smoke run's leftovers are of no use to anyone afterwards. The
	// disconnection is what makes the cleanup survive the case that needs it most: a
	// workflow that is failing or being cancelled would otherwise have its stop
	// scheduled into a context that is already done.
	defer func() {
		if in.KeepSession {
			return
		}
		cleanupCtx, cancel := workflow.NewDisconnectedContext(stepCtx)
		defer cancel()
		stopErr := workflow.ExecuteActivity(cleanupCtx, activities.StopSession,
			vcomputertemporal.SessionRequest{BotID: in.BotID}).Get(cleanupCtx, nil)
		if stopErr != nil {
			// A stop that failed is a container still running, so it is the run's own
			// failure rather than a note on a success. It is joined rather than
			// substituted when a step failed first: the two are different answers, and
			// the leftover sandbox is the one an operator has to act on.
			err = errors.Join(err, fmt.Errorf("stop the session for %s: %w", in.BotID, stopErr))
			return
		}
		result.SessionStopped = true
	}()

	if err := workflow.ExecuteActivity(stepCtx, activities.Navigate,
		vcomputertemporal.NavigateRequest{BotID: in.BotID, URL: in.URL}).Get(stepCtx, nil); err != nil {
		return result, err
	}

	workflow.Sleep(ctx, navigateSettle)

	var before vcomputertemporal.ScreenshotResult
	if err := workflow.ExecuteActivity(stepCtx, activities.Screenshot,
		vcomputertemporal.SessionRequest{BotID: in.BotID}).Get(stepCtx, &before); err != nil {
		return result, err
	}

	if err := workflow.ExecuteActivity(stepCtx, activities.Click,
		vcomputertemporal.ClickRequest{BotID: in.BotID, X: in.ClickX, Y: in.ClickY}).Get(stepCtx, nil); err != nil {
		return result, err
	}

	if err := workflow.ExecuteActivity(stepCtx, activities.Type,
		vcomputertemporal.TypeRequest{BotID: in.BotID, Text: in.Text}).Get(stepCtx, nil); err != nil {
		return result, err
	}

	workflow.Sleep(ctx, inputSettle)

	var after vcomputertemporal.ScreenshotResult
	if err := workflow.ExecuteActivity(stepCtx, activities.Screenshot,
		vcomputertemporal.SessionRequest{BotID: in.BotID}).Get(stepCtx, &after); err != nil {
		return result, err
	}

	var executed domain.ExecResult
	if err := workflow.ExecuteActivity(stepCtx, activities.Exec,
		vcomputertemporal.ExecRequest{BotID: in.BotID, Command: smokeCommand}).Get(stepCtx, &executed); err != nil {
		return result, err
	}

	result.SessionState = session.State
	result.CDPURL = session.CDPURL
	result.ScreenshotBeforeBytes = before.Bytes
	result.ScreenshotAfterBytes = after.Bytes
	result.ExecStdout = executed.Stdout
	result.ExecStub = executed.Stub
	return result, nil
}

// activityOptions is what one step runs under: a bound on the call and a bound on the
// attempts. There is no heartbeat here and no retry backoff worth configuring — every step
// of a smoke run is short and the run is a person waiting for it — so the two values that
// matter are the timeout and the ceiling on attempts.
func activityOptions(timeout time.Duration) workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: timeout,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: maximumAttempts,
		},
	}
}
