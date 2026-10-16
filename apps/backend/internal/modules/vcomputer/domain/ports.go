// Package domain defines the vcomputer module's contract: the shape of a bot's
// virtual computer as the backend sees it, and the one port the rest of the
// backend may use to reach it.
//
// The module is a PoC and it is deliberately incomplete in one specific way: it
// declares what the backend needs and provides an implementation, but nothing
// registers it (docs/adr/0003-virtual-computer.md §3.2). That is what keeps the
// package importable and unused — `api` and `agent-worker` behave exactly as they
// did before it existed.
//
// The port is narrow on purpose. A single "run anything on the computer" method
// would erase the granularity the approval gates depend on
// (docs/riset-microvm.md §5.1): browsing is `read`, clicking and typing are
// `write_external`, and exec is the most dangerous of the four. Each stays its own
// method here so a caller can label them separately when they are wired.
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
// caller has to decide what to do with each: a missing session is created, an
// unimplemented runtime is a configuration mistake, and a refused command is a
// policy decision.
var (
	// ErrNotConfigured is a call with no control plane address. A deployment that
	// has not set one has no virtual computer, and it should learn that from the
	// first call rather than from a nil dereference.
	ErrNotConfigured = errors.New("vcomputer: the control plane address is not configured")
	// ErrNoSession is an action on a bot whose session does not exist.
	ErrNoSession = errors.New("vcomputer: no session for that bot")
	// ErrInvalidInput is a request the control plane refused: no bot id, an id that
	// cannot name a container, or an empty command.
	ErrInvalidInput = errors.New("vcomputer: the control plane refused the request")
	// ErrNotImplemented is the control plane answering with its skeleton runtime.
	// It is distinct from a failure because it is the expected answer while the PoC
	// runs with `-runtime docker`.
	ErrNotImplemented = errors.New("vcomputer: the sandbox runtime is a skeleton")
	// ErrUnavailable is the control plane being unreachable or failing. It is worth
	// retrying, unlike the four above.
	ErrUnavailable = errors.New("vcomputer: the control plane is unavailable")
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
	// NoVNCURL is where a person watches the desktop. It is an internal address and
	// must never be handed to a browser outside the sandbox network.
	NoVNCURL string `json:"novnc_url,omitempty"`
	// CDPURL is Chromium's DevTools endpoint inside the sandbox. It is prepared in
	// the image and unused by the PoC; it is the better route for typing into a page
	// than synthesized keystrokes.
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

// ExecResult is the answer to one command. Stub is true while the control plane's
// runtime refuses to run anything, which is the PoC's default: it echoes the
// command so a caller can see what would have run.
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
	// Exec runs a command inside the sandbox. It is the most dangerous call in the
	// module: a shell can do anything, and no per-command label exists, so riset §5.1
	// treats every command as write_external until an allowlist is designed.
	Exec(ctx context.Context, botID string, command string) (ExecResult, error)
}
