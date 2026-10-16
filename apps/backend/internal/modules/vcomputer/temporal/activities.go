// Package temporal holds the Temporal activities a worker will register to drive a
// bot's virtual computer.
//
// It is a thin layer for the same reason the other modules' activity packages are:
// the workflow package must stay free of the module's services so replay stays
// deterministic, and the client must stay free of the SDK so a unit test can call it
// without a Temporal server. An activity is where those two meet, and it does
// nothing but translate one request into one call.
//
// The activity set is registered by the agent worker:
// internal/container/vcomputer.go builds it and RegisterVComputerTasks installs it, together
// with the smoke workflow in internal/modules/vcomputer/workflow. It is registered even when
// no control plane is configured — every activity then answers ErrNotConfigured, which is
// what a deployment without a virtual computer looks like rather than a worker that has never
// heard of one.
//
// Nothing else calls these yet: the agent task loop and its tool catalog do not reach the
// sandbox (docs/adr/0003-virtual-computer.md), so no tool a model can call becomes a click or
// a command until that is wired, and neither does the approval gate that would have to stand
// in front of exec.
//
// The activities are named after the control plane's routes rather than after the
// tools a model sees. Tools carry labels and grants (docs/riset-microvm.md §5.1);
// these are the mechanics underneath them — Navigate is the one that points the
// browser somewhere, Click and Type are the input a page receives, Exec is the one
// that is not a browser at all — and one tool call may become more than one activity.
package temporal

import (
	"context"
	"errors"
	"time"

	// The SDK's own package is aliased because this package is called temporal too:
	// an unaliased import would read as if the module were calling itself.
	sdktemporal "go.temporal.io/sdk/temporal"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/vcomputer/domain"
)

// Deps are the activity dependencies. The client is the only required one.
type Deps struct {
	// Computer is the control plane. A nil client is a deployment without a virtual
	// computer, and every activity says so instead of panicking.
	Computer domain.ComputerUseClient
	// Logger is optional; the activities log the failures a caller cannot see from
	// the return value alone, such as a screenshot that came back empty.
	Logger *zap.Logger
}

// Activities is the set a worker registers.
type Activities struct {
	deps Deps
}

// New builds the activity set.
func New(deps Deps) *Activities {
	return &Activities{deps: deps}
}

// SessionRequest names the bot an activity acts on. Every request carries it: a
// computer belongs to one bot, and an activity that guessed which would be an
// activity that can drive the wrong desktop.
type SessionRequest struct {
	BotID string `json:"bot_id"`
}

// ClickRequest is one press. The coordinates come from a model reading a
// screenshot, so they are what the model believes it saw — the control plane is what
// decides whether they land on anything.
type ClickRequest struct {
	BotID string `json:"bot_id"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
}

// TypeRequest is one string sent to the focused window.
type TypeRequest struct {
	BotID string `json:"bot_id"`
	Text  string `json:"text"`
}

// NavigateRequest is one URL. The URL is the only payload in this package that a
// model writes in full, which is why the control plane — not this layer — is what
// decides whether its scheme is one the browser may be pointed at.
type NavigateRequest struct {
	BotID string `json:"bot_id"`
	URL   string `json:"url"`
}

// ExecRequest is one command. It is the request that must never reach this activity
// without an approval behind it: riset §5.1 labels `computer.exec` write_external,
// because a shell command has no machine-readable shape to gate on.
type ExecRequest struct {
	BotID   string `json:"bot_id"`
	Command string `json:"command"`
}

// ScreenshotResult carries a capture back through the workflow.
//
// TODO: a screenshot is up to a few hundred kilobytes and Temporal's default payload
// limit is 2 MiB, so this is fine for a PoC and wrong for a loop that takes one
// capture per step. The version that gets wired should write the PNG to the object
// storage in Jakarta and return the key, which is the claim-check rule ADR 0001
// already applies to everything else a workflow touches.
type ScreenshotResult struct {
	PNG        []byte    `json:"png"`
	Bytes      int       `json:"bytes"`
	CapturedAt time.Time `json:"captured_at"`
}

// EnsureSession makes sure the bot has a computer, and is safe to call before every
// task: the control plane creates the session once and returns it afterwards.
//
// It is the first activity of any task that needs a desktop, because every other
// activity fails with ErrNoSession when it is skipped.
func (a *Activities) EnsureSession(ctx context.Context, request SessionRequest) (domain.Session, error) {
	return a.client().EnsureSession(ctx, request.BotID)
}

// StopSession ends a session and keeps its volume.
//
// It exists so a reaper or an explicit "matikan komputer" action has somewhere to
// call. It is deliberately not called at the end of a task: a session that ends with
// the task that used it would lose the browser profile it was kept for, which is the
// opposite of what the persistent volume is for.
func (a *Activities) StopSession(ctx context.Context, request SessionRequest) error {
	return a.client().StopSession(ctx, request.BotID)
}

// Screenshot is the desktop as it is now. The workflow decides what to do with the
// bytes; this activity only fetches them.
func (a *Activities) Screenshot(ctx context.Context, request SessionRequest) (ScreenshotResult, error) {
	png, err := a.client().Screenshot(ctx, request.BotID)
	if err != nil {
		return ScreenshotResult{}, err
	}
	if len(png) == 0 {
		// An empty capture is not a failure of the sandbox, but it is useless to the
		// model, and returning it silently would turn into "the screen is blank" in a
		// task summary rather than into a bug.
		if a.deps.Logger != nil {
			a.deps.Logger.Warn("vcomputer: the control plane answered an empty screenshot",
				zap.String("bot_id", request.BotID))
		}
	}
	return ScreenshotResult{PNG: png, Bytes: len(png), CapturedAt: time.Now()}, nil
}

// Click presses a point on the desktop.
func (a *Activities) Click(ctx context.Context, request ClickRequest) error {
	return a.client().Click(ctx, request.BotID, request.X, request.Y)
}

// Type sends text to the focused window.
func (a *Activities) Type(ctx context.Context, request TypeRequest) error {
	return a.client().Type(ctx, request.BotID, request.Text)
}

// Navigate points the browser at a page.
//
// It is the activity a model reaches for first, and the one whose URL decides whether
// the task is reading a page or writing to a third party's
// (docs/riset-microvm.md §5.1): the control plane refuses a scheme it does not accept,
// and the approval gate is what decided the call was allowed at all.
//
// A navigation the control plane carried out and that did not load is turned into a
// non-retryable error here, and that is the one place in this package that classifies
// anything. The default retry policy would otherwise spend the activity's whole budget
// on the same URL: the page is missing, or the sandbox has no route to it, and neither
// answer changes by asking again — while the cost of asking is a fresh browser
// navigation each time. The errors that do deserve a retry are the ones where the
// control plane itself did not answer (ErrUnavailable), and those are left alone.
func (a *Activities) Navigate(ctx context.Context, request NavigateRequest) error {
	err := a.client().Navigate(ctx, request.BotID, request.URL)
	if errors.Is(err, domain.ErrNavigationFailed) {
		return sdktemporal.NewNonRetryableApplicationError(err.Error(), navigationFailedType, err)
	}
	return err
}

// navigationFailedType is the error type the failed navigation crosses the activity
// boundary as. It is a short name rather than the sentence because a Temporal error
// type is what a workflow branches on, and the sentence is already in the message.
const navigationFailedType = "NavigationFailed"

// Exec runs one command inside the sandbox and answers what it printed.
//
// Whether it really ran is in the answer's `stub` field, and a caller has to read that
// field rather than the exit code: the docker runtime executes for real (`stub: false`),
// while the memory runtime the control plane runs in tests echoes the command
// (`stub: true`) and answers exit 0 with no output. The exit code alone cannot tell
// those apart, and a stub that looked like a successful command would reach a task
// summary as "nothing to report" instead of as a bug.
func (a *Activities) Exec(ctx context.Context, request ExecRequest) (domain.ExecResult, error) {
	return a.client().Exec(ctx, request.BotID, request.Command)
}

// client is the single place the "no control plane" case is turned into an error, so
// every activity answers the same way when a deployment forgot to configure one.
func (a *Activities) client() domain.ComputerUseClient {
	if a == nil || a.deps.Computer == nil {
		return unconfigured{}
	}
	return a.deps.Computer
}

// unconfigured is the client a deployment without a control plane gets. It exists so
// the check is one branch in one place rather than a nil test in every activity, and
// so the error names the missing configuration rather than the call that hit it.
type unconfigured struct{}

func (unconfigured) EnsureSession(context.Context, string) (domain.Session, error) {
	return domain.Session{}, domain.ErrNotConfigured
}

func (unconfigured) StopSession(context.Context, string) error { return domain.ErrNotConfigured }

func (unconfigured) Screenshot(context.Context, string) ([]byte, error) {
	return nil, domain.ErrNotConfigured
}

func (unconfigured) Click(context.Context, string, int, int) error { return domain.ErrNotConfigured }

func (unconfigured) Type(context.Context, string, string) error { return domain.ErrNotConfigured }

func (unconfigured) Navigate(context.Context, string, string) error { return domain.ErrNotConfigured }

func (unconfigured) Exec(context.Context, string, string) (domain.ExecResult, error) {
	return domain.ExecResult{}, domain.ErrNotConfigured
}
