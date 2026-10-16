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
	"fmt"
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
	// CDPURL is where a caller drives the session's browser: a path on the control
	// plane, not an address on the sandbox network. The control plane proxies it to
	// the sandbox's gate, which is the only thing inside a sandbox that listens on
	// the network address and the only thing that asks for the sandbox secret, so a
	// worker needs the control token and nothing else.
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

// Navigation is what the browser ended up showing after a Navigate. It is a result
// rather than a bare error because "the page loaded" is not the whole answer: a
// caller that asked for a URL and got a redirect, or a page with no title, needs to
// see that.
type Navigation struct {
	// FinalURL is where the browser landed, which differs from the URL the caller
	// asked for after a redirect. A navigation that ends on chrome-error:// never
	// reaches the caller as a Navigation: it is a NavigationError.
	FinalURL string `json:"final_url"`
	// Title is the loaded document's title, empty when the page has none.
	Title string `json:"title"`
}

// NavigationError is a navigation that reached the browser but not the page: the
// browser refused to open the URL, the document never finished loading inside the
// timeout, or it finished on chrome-error:// — which is what Chromium shows when
// there was nothing at the address, and what a sandbox with no egress produces for
// every address outside its own network.
//
// It carries both URLs and a reason so the caller can tell "the page is broken"
// from "the sandbox could not reach it", and the control plane answers 502 with
// them rather than the 200 a plain error would produce.
type NavigationError struct {
	// URL is what the caller asked for.
	URL string
	// FinalURL is where the browser stopped, empty when it never reported one.
	FinalURL string
	// Reason is the short human-readable cause.
	Reason string
}

func (e *NavigationError) Error() string {
	return fmt.Sprintf("vcomputer: navigation to %s failed: %s (final url %q)", e.URL, e.Reason, e.FinalURL)
}

// SandboxTarget is how the control plane reaches a session's sandbox: the address
// of the gate on the sandbox network, and the secret that gate requires.
//
// The two travel together because they are only ever useful together, and because
// the alternative — a second interface method that inspects the container again —
// would double the docker calls on the noVNC path, where one page load becomes
// dozens of proxied requests.
//
// The secret is a credential: it belongs in a request header and nowhere else, so
// it has no JSON tag and must never be logged.
type SandboxTarget struct {
	// URL is the gate's base address, e.g. "http://172.20.0.4:8443". Both the CDP
	// route and the noVNC route are paths under it.
	URL string
	// Secret is the value the gate wants in X-Sandbox-Secret.
	Secret string
}

// CleanupResult is what a Cleanup removed, and what it could not.
type CleanupResult struct {
	// RemovedContainers and RemovedVolumes are the counts that actually happened,
	// not the counts that were attempted.
	RemovedContainers int `json:"removed_containers"`
	RemovedVolumes    int `json:"removed_volumes"`
	// Failed holds one line per object that was selected and could not be removed,
	// e.g. a volume still mounted by a running container. It is empty — and left
	// out of the JSON — when everything that was selected is gone.
	Failed []string `json:"failed,omitempty"`
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
	// Purge removes the session entirely: the container whatever state it is in,
	// and the volume that holds the browser profile. It is what makes a bot
	// forgettable, and it is the difference between Stop ("the machine is off")
	// and Purge ("the machine is gone").
	Purge(ctx context.Context, botID string) error
	// Cleanup removes every sandbox this runtime owns that is not running, which is
	// what a crashed control plane leaves behind. With volumes it also removes
	// their home volumes, and it never touches a running container: a running
	// sandbox belongs to a session somebody is using.
	Cleanup(ctx context.Context, volumes bool) (CleanupResult, error)
	// Screenshot is the current desktop as PNG bytes.
	Screenshot(ctx context.Context, botID string) ([]byte, error)
	// Click presses a point on the desktop.
	Click(ctx context.Context, botID string, x, y int) error
	// Type sends text to the focused window.
	Type(ctx context.Context, botID string, text string) error
	// Navigate points the session's browser at a URL and answers what the browser
	// ended up showing. Only http, https, data, and about are accepted, and the
	// runtime refuses the rest rather than asking the browser to interpret them.
	// A page that does not load is a *NavigationError, not a success: the caller
	// asked for a page, and answering "navigated" would be a lie.
	Navigate(ctx context.Context, botID string, targetURL string) (Navigation, error)
	// GateTarget is where the session's sandbox gate listens, and the secret it
	// wants. The control plane proxies /v1/sessions/<bot>/novnc/ and
	// /v1/sessions/<bot>/cdp/ to it, which is why a caller only ever needs to
	// reach the control plane: the gate is the one thing inside a sandbox that
	// listens on the sandbox network, and it is what asks for the secret.
	GateTarget(ctx context.Context, botID string) (SandboxTarget, error)
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

// The ports and names a sandbox gets when the deployment configures nothing. They
// are defaults for configuration (VCOMPUTER_SANDBOX_NOVNC_PORT,
// VCOMPUTER_SANDBOX_GATE_PORT, VCOMPUTER_SANDBOX_NAME_PREFIX), not values baked
// into a command line: the control plane passes them to every sandbox it starts,
// and the sandbox entrypoint reads its own from the environment.
const (
	// DefaultNOVNCPort is the port noVNC's websockify listens on inside a sandbox.
	// It is loopback only: the gate is what publishes noVNC to the sandbox network,
	// so the control plane never dials this port itself.
	DefaultNOVNCPort = "6080"
	// DefaultGatePort is the port the sandbox's gate listens on. It is the only
	// listener on a sandbox's network address, and it is what the control plane
	// dials for both CDP and noVNC.
	DefaultGatePort = "8443"
	// DefaultNamePrefix is what a sandbox's container and volume are named after:
	// the container is <prefix>-<bot id> and the volume <prefix>-<bot id>-home. It
	// is also the scope a cleanup works in, so two stacks can share one host
	// without removing each other's sandboxes.
	DefaultNamePrefix = "vcomputer"
	// botLabel is the label every sandbox container and volume carries. A label
	// rather than a name pattern is what lets the control plane find the sandboxes
	// it owns: a name is a convention, a label is a record.
	botLabel = "vcomputer.bot"
)

// SecretHeader is the header a sandbox's gate wants the sandbox secret in.
//
// It is exported because the control plane's HTTP surface has to attach it: a
// caller holds the control token, the gate wants the sandbox secret, and the
// control plane is the only thing that holds both. The secret is per sandbox and
// generated when its container is created, so it is a credential and never a
// configuration value: it travels in this header and in the container's
// environment, and nowhere else.
const SecretHeader = "X-Sandbox-Secret"

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

// CDPPath is where a caller drives the session's browser, and it is a path on the
// control plane for the same reason NoVNCPath is: the control plane proxies it to
// the sandbox's gate with the sandbox secret attached, so a worker holding only
// the control token can use the DevTools HTTP API — and its websocket, which is
// what /json/list answers a webSocketDebuggerUrl for.
func CDPPath(botID string) string {
	return "/v1/sessions/" + botID + "/cdp"
}

// sandboxURL is an address on the sandbox network, built from the container's
// address rather than from its name: a container name resolves over the network,
// but the address the control plane inspected is the one it wants to dial.
func sandboxURL(host, port string) string {
	return "http://" + host + ":" + port
}

// containerName and volumeName derive both names from the prefix and the bot id.
// That is why the id is validated before it gets here: these strings end up in a
// `docker` command line.
func containerName(prefix, botID string) string { return prefix + "-" + botID }
func volumeName(prefix, botID string) string    { return prefix + "-" + botID + "-home" }

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
