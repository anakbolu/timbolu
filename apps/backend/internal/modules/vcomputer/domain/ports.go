// Package domain defines the vcomputer module's contract: the shape of a bot's
// virtual computer as the backend sees it, and the one port the rest of the
// backend may use to reach it.
//
// The module is a PoC and it is deliberately incomplete in one specific way: it
// declares what the backend needs and provides an implementation, and the agent worker
// registers the activities (internal/container/vcomputer.go) — but nothing in a task reaches
// them yet. The agent task loop and its tool catalog are not wired to this port, so no tool a
// model can call becomes a click or a command, and the approval gate that would stand in front
// of exec has nothing to gate. The smoke workflow is what exercises the seam in the meantime.
//
// The port is narrow on purpose. A single "run anything on the computer" method
// would erase the granularity the approval gates depend on
// (docs/riset-microvm.md §5.1): browsing is `read` — Navigate is the call that
// points the browser at a page — clicking and typing are `write_external`, and exec
// is the most dangerous of the four. Each stays its own method here so a caller can
// label them separately when they are wired.
package domain

import (
	"context"
	"errors"
	"time"
)

// Session states, mirroring what the control plane reports.
const (
	StateStopped = "stopped"
	StateRunning = "running"
	StateError   = "error"
)

// Errors this module returns. They are sentinels rather than strings because a
// caller has to decide what to do with each: a missing session is created, a refused
// token is a configuration mistake, and a request the control plane will not take is
// a policy decision.
var (
	// ErrNotConfigured is a call this deployment cannot make: either no control plane
	// address was configured, or the control plane refused the bearer token with a
	// 401 because VCOMPUTER_CONTROL_TOKEN is missing or does not match the one the
	// control plane was started with. Both are deployment mistakes, and neither
	// becomes true by asking again.
	ErrNotConfigured = errors.New("vcomputer: the control plane is not configured")
	// ErrNoSession is an action on a bot whose session does not exist.
	ErrNoSession = errors.New("vcomputer: no session for that bot")
	// ErrInvalidInput is a request the control plane refused: no bot id, an id that
	// cannot name a container, a URL whose scheme is not one of the four it accepts,
	// or an empty command.
	ErrInvalidInput = errors.New("vcomputer: the control plane refused the request")
	// ErrUnavailable is the control plane being unreachable or failing in a way that
	// may pass. It is worth retrying, unlike the three above.
	ErrUnavailable = errors.New("vcomputer: the control plane is unavailable")
	// ErrNavigationFailed is a navigation the control plane carried out and that
	// still did not put the page in front of the caller: the browser landed on its
	// own error page, the document never finished loading inside the control
	// plane's navigate timeout, or the page answered 4xx/5xx. It is not retryable
	// for the same reason a refused request is not: the same URL produces the same
	// answer, so a retry spends the budget reaching it again — and a page that is
	// temporarily down is better handled by a caller that decides to try later than
	// by a retry policy that cannot tell the two apart.
	ErrNavigationFailed = errors.New("vcomputer: the navigation failed")
)

// Session is one bot's virtual computer, as much of it as the backend needs: where
// it is, whether it is up, and what holds its persistent half.
type Session struct {
	BotID string `json:"bot_id"`
	// State is one of the State* constants.
	State string `json:"state"`
	// Runtime names the implementation on the control plane's side: "memory" for the
	// PoC stub, "docker" for the real one.
	Runtime string `json:"runtime"`
	// ContainerID is what an operator or a reaper needs to find the container.
	ContainerID string `json:"container_id,omitempty"`
	// NoVNCURL is where a person watches the desktop. It is a path on the control
	// plane (`/v1/sessions/<bot>/novnc/...`), not a sandbox address: the control
	// plane reverse-proxies it to the sandbox's noVNC port, which listens on the
	// sandbox's own loopback and is exposed to anything outside it only by the gate
	// on the sandbox's network address, which demands that sandbox's secret.
	NoVNCURL string `json:"novnc_url,omitempty"`
	// CDPURL is where this session's DevTools endpoint is reachable: a path on the
	// control plane (`/v1/sessions/<bot>/cdp`), not a sandbox address. Chromium
	// listens on the sandbox's loopback only, and the one listener the sandbox has
	// on its network address is vcomputer-gate, which forwards `/cdp/*` there after
	// checking a per-sandbox secret. So the control plane is the only thing that can
	// dial CDP, and a caller holding just the control token goes through the same
	// path (`/v1/sessions/<bot>/cdp[/<rest>]`, any method — the DevTools HTTP API is
	// not method-uniform); the control plane rewrites `webSocketDebuggerUrl` in
	// `/json/list` answers to that prefix for exactly this reason.
	CDPURL string `json:"cdp_url,omitempty"`
	// Volume is the named volume holding the browser profile and the bot's home. The
	// container may die; the session does not, which is what makes "log in once,
	// continue tomorrow" possible (docs/riset-microvm.md §2.3).
	Volume string `json:"volume,omitempty"`
	// CreatedAt is when the session was first ensured; LastUsedAt is what an idle
	// reaper will read.
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

// ExecResult is the answer to one command. Stub says whether the runtime actually
// ran it: false with the docker runtime, true with the memory runtime, which echoes
// the command instead of executing it. A caller has to read that field rather than
// the exit code, because a stub that answered 0 with no output is indistinguishable
// from a command that ran and printed nothing.
type ExecResult struct {
	Command  string `json:"command"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Stub     bool   `json:"stub"`
}

// ComputerUseClient is the backend's port onto the control plane.
//
// It is an interface rather than a concrete client so the Temporal activities can
// be tested with a fake and so a future implementation (a Kubernetes Pod instead of
// a Docker container, riset §4.6 Opsi C) can replace it without touching them.
type ComputerUseClient interface {
	// EnsureSession creates the session if it does not exist and returns it either
	// way. It is idempotent, so a worker may call it before every task.
	EnsureSession(ctx context.Context, botID string) (Session, error)
	// StopSession ends the session and keeps its volume. The next EnsureSession reuses
	// the browser profile rather than starting a login from scratch.
	StopSession(ctx context.Context, botID string) error
	// Screenshot is the current desktop as PNG bytes.
	Screenshot(ctx context.Context, botID string) ([]byte, error)
	// Click presses a point on the desktop. It is a write_external action: it can send
	// data to a third-party page, so it goes through the approval gate when wired.
	Click(ctx context.Context, botID string, x, y int) error
	// Type sends text to the focused window. Also write_external, for the same reason.
	Type(ctx context.Context, botID string, text string) error
	// Navigate points the browser at a page, and is the one call that decides what
	// the desktop is looking at: a `read` action while the page is the caller's own
	// and write_external as soon as it is a third party's, which is why the control
	// plane — not this layer — is what validates the scheme.
	Navigate(ctx context.Context, botID string, url string) error
	// Exec runs a command inside the sandbox. It is the most dangerous call in the
	// module: a shell can do anything, and no per-command label exists, so riset §5.1
	// treats every command as write_external until an allowlist is designed.
	Exec(ctx context.Context, botID string, command string) (ExecResult, error)
}
