// Package sandbox is the seam between the control plane and whatever actually
// holds a bot's computer.
//
// The PoC ships two implementations of one interface: InMemoryRuntime, which
// records what it was asked to do and answers a generated PNG, and DockerRuntime,
// which is a skeleton with the exact commands written down where the code will go.
// The interface is the point of this package: it is what lets the control plane,
// the HTTP surface, and the tests exist before a single container is created, and
// it is what a future gVisor/Kubernetes runtime (docs/riset-microvm.md §4.6, Opsi C)
// will implement without the API above it changing.
//
// Nothing here talks to Docker yet, and nothing here executes a command. That is
// deliberate: the PoC's job is to show the shape of the session lifecycle, not to
// run untrusted input.
package sandbox

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Session states. They are the states the control plane reports and the states a
// reaper will later act on: `stopped` is a session that exists in the record but
// holds no container, `running` is one that does.
const (
	StateStopped  = "stopped"
	StateStarting = "starting"
	StateRunning  = "running"
	StateError    = "error"
)

// Errors this package returns. The control plane maps them to HTTP status codes,
// so they are sentinels rather than formatted strings.
var (
	// ErrNoBotID is a request that named no bot. A session is per bot, so there is
	// nothing to look up without one.
	ErrNoBotID = errors.New("vcomputer: a bot id is required")
	// ErrInvalidBotID is an id that cannot name a container. The id becomes part of a
	// `docker create` command line, so it is checked rather than trusted.
	ErrInvalidBotID = errors.New("vcomputer: a bot id may hold only letters, digits, dot, dash, and underscore")
	// ErrNoCommand is an exec with nothing to run.
	ErrNoCommand = errors.New("vcomputer: a command is required")
	// ErrNoSession is an action on a bot that has no session. The caller is expected
	// to ensure the session first, which is what makes "ensure" idempotent and
	// "stop" honest.
	ErrNoSession = errors.New("vcomputer: no session for that bot")
	// ErrNotImplemented is the DockerRuntime answering. It exists so the skeleton is
	// honest at runtime instead of pretending a container was created.
	ErrNotImplemented = errors.New("vcomputer: the docker runtime is a skeleton; the command it will run is written in the comment")
)

// Session is what a caller gets back: enough to reach the desktop and to decide
// whether to keep it alive, and nothing about how it is hosted.
type Session struct {
	BotID string `json:"bot_id"`
	// State is one of the State* constants.
	State string `json:"state"`
	// Runtime names the implementation that owns the session: "memory" or "docker".
	Runtime string `json:"runtime"`
	// ContainerID is empty for the in-memory runtime and for a session that is only
	// recorded. It is what a reaper and an operator need to find the container.
	ContainerID string `json:"container_id,omitempty"`
	// NoVNCURL is where a person watches the desktop. It is an internal address: the
	// control API is never published (docs/adr/0003-virtual-computer.md §3.6).
	NoVNCURL string `json:"novnc_url,omitempty"`
	// CDPURL is Chromium's DevTools endpoint. It is prepared in the image and unused
	// by the PoC.
	CDPURL string `json:"cdp_url,omitempty"`
	// Volume is the named volume that holds the persistent half of the session: the
	// browser profile and the bot's home. The container may die; this does not.
	Volume string `json:"volume,omitempty"`
	// CreatedAt is when the session was first ensured, and LastUsedAt is when it was
	// last touched. The difference is what an idle reaper will read.
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

// Action is one thing that was done to a desktop. The PoC records them instead of
// performing them, and the record is what makes the stub observable: a test and the
// demo page can both see that a click arrived.
type Action struct {
	// Kind is "screenshot", "click", "type", or "exec".
	Kind string `json:"kind"`
	// Detail is the coordinate pair, the typed text, or the command. It is a single
	// field because the PoC only needs it for evidence, not for replay.
	Detail string    `json:"detail"`
	At     time.Time `json:"at"`
}

// ExecResult is the answer to one command. Stub is true while the runtime refuses
// to run anything: the PoC echoes the command so a caller can see what *would* have
// run, and never touches the host.
type ExecResult struct {
	Command  string `json:"command"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Stub     bool   `json:"stub"`
}

// Runtime is the whole surface the control plane has on a sandbox: the lifecycle,
// the desktop actions a model can ask for, and nothing else.
//
// It is deliberately narrow. A single "run anything" method would erase the
// granularity the approval gates depend on (docs/riset-microvm.md §5), so the
// actions stay separate here even while the PoC stubs all of them.
type Runtime interface {
	// Ensure creates the session if it does not exist and returns it either way. It
	// is idempotent: calling it twice must not start two containers.
	Ensure(ctx context.Context, botID string) (Session, error)
	// Status reports a session, and ErrNoSession when there is none.
	Status(ctx context.Context, botID string) (Session, error)
	// Stop ends the session and keeps the volume. The next Ensure reuses it.
	Stop(ctx context.Context, botID string) error
	// Screenshot is the current desktop as PNG bytes.
	Screenshot(ctx context.Context, botID string) ([]byte, error)
	// Click presses a point on the desktop.
	Click(ctx context.Context, botID string, x, y int) error
	// Type sends text to the focused window.
	Type(ctx context.Context, botID string, text string) error
	// Exec runs a command inside the sandbox. It is the most dangerous of the four,
	// which is why riset §5.1 labels it write_external and why the PoC does not
	// implement it.
	Exec(ctx context.Context, botID string, command string) (ExecResult, error)
}

// ValidBotID reports whether an id may name a container and a volume. The rule is
// deliberately tight — letters, digits, dot, dash, underscore, no leading dash or
// dot, at most 64 characters — because the id is interpolated into
// `vcomputer-<id>` and handed to the Docker CLI.
func ValidBotID(botID string) bool {
	if botID == "" || len(botID) > 64 || botID[0] == '-' || botID[0] == '.' {
		return false
	}
	for _, r := range botID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// cleanBotID trims and validates an id, and is the single place the rule lives.
func cleanBotID(botID string) (string, error) {
	id := strings.TrimSpace(botID)
	if id == "" {
		return "", ErrNoBotID
	}
	if !ValidBotID(id) {
		return "", ErrInvalidBotID
	}
	return id, nil
}
