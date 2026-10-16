// Package sandbox is the seam between the control plane and whatever actually
// holds a bot's computer.
//
// The PoC ships two implementations of one interface: InMemoryRuntime, which
// records what it was asked to do and answers a generated PNG, and DockerRuntime,
// which runs the real container through the docker CLI. The interface is the point
// of this package: it is what lets the control plane, the HTTP surface, and the
// tests exist independently of how a sandbox is hosted, and it is what a future
// gVisor/Kubernetes runtime (docs/riset-microvm.md §4.6, Opsi C) will implement
// without the API above it changing.
//
// Every method validates its own input before it reaches a container. The bot id
// becomes a container and a volume name, the URL becomes an argument to Chromium,
// and the typed text becomes an argv entry of `xdotool`: none of them is trusted,
// and none of them ever reaches a shell.
package sandbox

import (
	"context"
	"errors"
	"net/url"
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
	// ErrNoURL is a navigate with nothing to open.
	ErrNoURL = errors.New("vcomputer: a url is required")
	// ErrInvalidURL is a navigate whose scheme a browser should not be handed.
	// Only http, https, data, and about are accepted: file, javascript, and chrome
	// reach past the page the model was given.
	ErrInvalidURL = errors.New("vcomputer: a url must use the http, https, data, or about scheme")
)

// Session is what a caller gets back: enough to reach the desktop and to decide
// whether to keep it alive, and nothing about how it is hosted.
type Session struct {
	BotID string `json:"bot_id"`
	// State is one of the State* constants.
	State string `json:"state"`
	// Runtime names the implementation that owns the session: "memory" or "docker".
	Runtime string `json:"runtime"`
	// ContainerID is empty for the in-memory runtime and holds the real container
	// id for a running docker session. It is what a reaper and an operator need to
	// find the container.
	ContainerID string `json:"container_id,omitempty"`
	// NoVNCURL is where a person watches the desktop: a path on the control plane
	// that it proxies to the sandbox, so the sandbox network itself stays
	// unreachable (docs/adr/0003-virtual-computer.md §3.6).
	NoVNCURL string `json:"novnc_url,omitempty"`
	// CDPURL is Chromium's DevTools endpoint on the sandbox network, built from the
	// container's address: Chromium refuses a request whose Host header is neither
	// an IP nor localhost, so the address is what the URL carries.
	CDPURL string `json:"cdp_url,omitempty"`
	// Volume is the named volume that holds the persistent half of the session: the
	// browser profile and the bot's home. The container may die; this does not.
	Volume string `json:"volume,omitempty"`
	// CreatedAt is when the session was first ensured, and LastUsedAt is when it was
	// last touched. The difference is what an idle reaper will read.
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

// Action is one thing that was done to a desktop. The in-memory runtime records
// them instead of performing them, and the record is what makes the stub
// observable: a test can see that a click arrived.
type Action struct {
	// Kind is "screenshot", "click", "type", "navigate", or "exec".
	Kind string `json:"kind"`
	// Detail is the coordinate pair, the typed text, the URL, or the command. It is
	// a single field because the PoC only needs it for evidence, not for replay.
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
// actions stay separate here.
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
	// Navigate points the session's browser at a URL. Only http, https, data, and
	// about are accepted, and the runtime refuses the rest rather than asking the
	// browser to interpret them.
	Navigate(ctx context.Context, botID string, targetURL string) error
	// NoVNCTarget is the address inside the sandbox network that serves the
	// session's noVNC client, e.g. "http://172.20.0.4:6080". The control plane
	// proxies /v1/sessions/<bot>/novnc/ to it, which is why a browser only ever
	// needs to reach the control plane and never the sandbox network itself.
	NoVNCTarget(ctx context.Context, botID string) (string, error)
	// Exec runs a command inside the sandbox. It is the most dangerous of the
	// actions, which is why riset §5.1 labels it write_external and why nothing
	// above this package is wired to it yet.
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

// DefaultNOVNCPort and DefaultCDPPort are the ports a sandbox listens on when the
// deployment does not say otherwise. They are defaults for configuration
// (VCOMPUTER_SANDBOX_NOVNC_PORT, VCOMPUTER_SANDBOX_CDP_PORT), not values baked into
// a command line: the control plane passes the configured ports to every sandbox it
// starts, and the sandbox entrypoint reads them from its environment.
const (
	DefaultNOVNCPort = "6080"
	DefaultCDPPort   = "9223"
)

// NoVNCPath is where a person watches a session's desktop. It is a path on the
// control plane rather than an address on the sandbox network: the control plane
// proxies it, so a browser only ever reaches the control plane and the sandbox
// network stays internal. `path` is relative on purpose — noVNC resolves the
// websocket URL against the page it was loaded from.
//
// autoconnect must be the literal "true": the noVNC 1.3 shipped by Debian
// bookworm's `novnc` package only autoconnects for `autoconnect=true`, so any
// other spelling (`1`, `yes`, empty) leaves the browser on the Connect screen.
func NoVNCPath(botID string) string {
	return "/v1/sessions/" + botID + "/novnc/vnc.html?autoconnect=true&resize=scale&path=v1/sessions/" + botID + "/novnc/websockify"
}

// sandboxURL is an address on the sandbox network. Both the CDP endpoint and the
// noVNC target are built from the container's address rather than from its name,
// because Chromium refuses a request whose Host header is neither an IP nor
// localhost.
func sandboxURL(host, port string) string {
	return "http://" + host + ":" + port
}

// containerName and volumeName derive both names from the bot id. That is why the
// id is validated before it gets here: these strings end up in a `docker` command
// line.
func containerName(botID string) string { return "vcomputer-" + botID }
func volumeName(botID string) string    { return "vcomputer-" + botID + "-home" }

// ValidURL reports whether raw may be handed to the session's browser.
//
// The four accepted schemes are the ones that cannot reach past the page: http and
// https go out to the network the sandbox is allowed to reach, while data and about
// stay inside the browser. Everything else a browser understands — file,
// javascript, chrome, view-source — is refused, because a desktop that can open
// file:// is a desktop that can read the container's own files.
func ValidURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "data", "about":
		return true
	default:
		return false
	}
}

// cleanURL trims and validates a target, and is the single place the rule lives.
func cleanURL(raw string) (string, error) {
	target := strings.TrimSpace(raw)
	if target == "" {
		return "", ErrNoURL
	}
	if !ValidURL(target) {
		return "", ErrInvalidURL
	}
	return target, nil
}
