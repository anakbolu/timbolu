package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DockerRuntime is the real runtime: one container per bot, its home in a named
// volume, driven through the docker CLI.
//
// It shells out to the docker binary rather than importing a Docker SDK, and that
// is the one dependency decision in this module: the control plane keeps a
// stdlib-only go.mod, so the client it speaks to the daemon with is the CLI that is
// already on the host (docs/adr/0003-virtual-computer.md §3.2). Every argument is
// passed as argv, never through a shell, so a bot id or a typed string cannot become
// a command.
//
// What it deliberately does not do:
//
//   - It does not publish a port. The desktop is reached over the sandbox network by
//     the control plane, which proxies noVNC (docs/adr/0003 §3.6); `docker run` here
//     never gets a `-p`.
//   - It does not pass --privileged, --device, or a runtime flag: the isolation story
//     for the PoC is "hardened runc container" (docs/adr/0003 §3.3).
//   - It does not talk to Chromium directly. Every CDP request goes through the
//     sandbox's gate, which is the only listener on the container's network address,
//     and it carries the per-sandbox secret: a sandbox that answered an unauthenticated
//     CDP request would let anything else on the network drive the browser.
type DockerRuntime struct {
	// bin is the docker client to run. It is a field so a test can point the runtime
	// at a fake script and never touch a real daemon.
	bin string
	// image is the sandbox image tag, e.g. vcomputer-sandbox:poc.
	image string
	// network is the sandbox network the container joins. The control plane must
	// share it: that network is how it reaches the gate, and it is the reason no port
	// is ever published.
	network string
	// namePrefix scopes every name this runtime creates: the container is
	// <prefix>-<bot id> and the volume <prefix>-<bot id>-home. It is also the scope
	// Cleanup works in, so two deployments can share one docker host without
	// removing each other's sandboxes.
	namePrefix string
	// novncPort and gatePort are the ports the sandbox listens on inside its own
	// network. They are configuration, not constants: the control plane passes them
	// to each sandbox it starts, and gatePort is the one it dials. novncPort is the
	// port the gate forwards /novnc/ to, which is why the control plane never dials
	// it itself.
	novncPort string
	gatePort  string
	// geometry is the Xvfb screen size handed to the sandbox.
	geometry string
	// execTimeout bounds one Exec. A command that hangs must not hold a worker's
	// activity open forever.
	execTimeout time.Duration
	// navigateTimeout bounds one Navigate, verification included: opening a tab is
	// fast, and a page that has not finished loading by then is a page the caller
	// needs to hear about rather than wait on.
	navigateTimeout time.Duration
	// waitTimeout and pollInterval bound how long Ensure waits for a starting
	// container to answer through its gate.
	waitTimeout  time.Duration
	pollInterval time.Duration
	// http talks to the sandbox's gate and never to anything else.
	http  *http.Client
	clock func() time.Time
}

// Compile-time check that the real runtime is a runtime.
var _ Runtime = (*DockerRuntime)(nil)

// Defaults for everything a deployment does not configure. They are exported so the
// compose file, the e2e script, and the tests all name the same image, network, and
// ports instead of repeating the strings.
const (
	// DefaultImage is the sandbox image the compose file builds.
	DefaultImage = "vcomputer-sandbox:poc"
	// DefaultNetwork is the internal network the sandbox joins and the control plane
	// shares. It has no route out (docker-compose.poc.yml, `internal: true`).
	DefaultNetwork = "vcomputer-internal"
	// DefaultDockerBin is the client on PATH when VCOMPUTER_DOCKER_BIN is unset.
	DefaultDockerBin = "docker"
	// DefaultExecTimeout bounds one Exec when VCOMPUTER_EXEC_TIMEOUT is unset.
	DefaultExecTimeout = 30 * time.Second
	// DefaultGeometry is the display the sandbox image starts Xvfb with.
	DefaultGeometry = "1280x800x24"
	// sandboxDisplay is the X display inside the container, as baked into the image.
	sandboxDisplay = ":99"
	// gatePrefix is the path under which the sandbox's gate publishes Chromium's
	// DevTools endpoint. The gate is the only thing that may reach 9222, so this is
	// the only way in.
	gatePrefix = "/cdp"
	// DefaultNavigateTimeout bounds one Navigate, verification included, when
	// VCOMPUTER_NAVIGATE_TIMEOUT is unset.
	DefaultNavigateTimeout = 15 * time.Second
	// secretBytes is how much randomness a sandbox secret carries: 16 bytes is 32 hex
	// characters, which is far past guessing over a network that is already internal.
	secretBytes = 16
	// secretEnv is the environment variable a sandbox receives its secret in. It is
	// read back from the container rather than stored by the control plane, so this
	// name is the only place the two ends have to agree on.
	secretEnv = "SANDBOX_SECRET"
	// containerRunning is the state docker reports for a container that is up, and
	// the only one in which a sandbox has an address on its network and a gate that
	// is listening.
	containerRunning = "running"
	// maxExecOutput caps what one command may return, so a runaway process cannot
	// fill the control plane's memory with its own output.
	maxExecOutput = 64 << 10
	// maxScreenshotOutput caps one capture. A 1280x800 PNG is well under a megabyte;
	// this only refuses something that is not a screenshot at all.
	maxScreenshotOutput = 16 << 20
	// defaultWaitTimeout is how long Ensure waits for a cold container to answer
	// through its gate: Xvfb, the window manager, Chromium, and the gate itself all
	// have to come up inside it.
	defaultWaitTimeout = 45 * time.Second
	// defaultPollInterval is how often that wait asks again.
	defaultPollInterval = 500 * time.Millisecond
	// execKillGrace is how long Exec waits for the output pipes to close after its
	// deadline killed the process. A command that left a child behind keeps those pipes
	// open, and waiting for that child would make the deadline meaningless.
	execKillGrace = time.Second
)

// DockerOption configures a DockerRuntime. Options exist so the defaults above stay
// in one place and a test can replace exactly the ones it needs.
type DockerOption func(*DockerRuntime)

// WithDockerBin sets the docker client to run.
func WithDockerBin(bin string) DockerOption {
	return func(r *DockerRuntime) {
		if strings.TrimSpace(bin) != "" {
			r.bin = strings.TrimSpace(bin)
		}
	}
}

// WithSandboxImage sets the image a session's container is created from.
func WithSandboxImage(image string) DockerOption {
	return func(r *DockerRuntime) {
		if strings.TrimSpace(image) != "" {
			r.image = strings.TrimSpace(image)
		}
	}
}

// WithSandboxNetwork sets the network the sandbox joins and the control plane shares.
func WithSandboxNetwork(network string) DockerOption {
	return func(r *DockerRuntime) {
		if strings.TrimSpace(network) != "" {
			r.network = strings.TrimSpace(network)
		}
	}
}

// WithSandboxPorts sets the ports the sandbox listens on inside its own network:
// the noVNC port the gate forwards to, and the gate's own port.
func WithSandboxPorts(novncPort, gatePort string) DockerOption {
	return func(r *DockerRuntime) {
		r.novncPort = portOr(novncPort, DefaultNOVNCPort)
		r.gatePort = portOr(gatePort, DefaultGatePort)
	}
}

// WithNamePrefix sets what this runtime's containers and volumes are named after,
// and therefore which objects Cleanup considers its own.
func WithNamePrefix(prefix string) DockerOption {
	return func(r *DockerRuntime) {
		if trimmed := strings.TrimSpace(prefix); trimmed != "" {
			r.namePrefix = trimmed
		}
	}
}

// WithScreenGeometry sets the display geometry handed to a sandbox.
func WithScreenGeometry(geometry string) DockerOption {
	return func(r *DockerRuntime) {
		if strings.TrimSpace(geometry) != "" {
			r.geometry = strings.TrimSpace(geometry)
		}
	}
}

// WithExecTimeout bounds one Exec.
func WithExecTimeout(timeout time.Duration) DockerOption {
	return func(r *DockerRuntime) {
		if timeout > 0 {
			r.execTimeout = timeout
		}
	}
}

// WithNavigateTimeout bounds one Navigate. It exists so a test does not have to wait
// the full default for a page that never finishes loading.
func WithNavigateTimeout(timeout time.Duration) DockerOption {
	return func(r *DockerRuntime) {
		if timeout > 0 {
			r.navigateTimeout = timeout
		}
	}
}

// WithWaitTimeout bounds how long Ensure waits for a starting container. It exists so
// a test does not have to wait the full default for a sandbox that never answers.
func WithWaitTimeout(timeout time.Duration) DockerOption {
	return func(r *DockerRuntime) {
		if timeout > 0 {
			r.waitTimeout = timeout
		}
	}
}

// NewDocker builds the runtime from the environment, then applies the options. The
// environment is how the compose file configures it; the options are how a test does.
func NewDocker(options ...DockerOption) *DockerRuntime {
	runtime := &DockerRuntime{
		bin:             envOr("VCOMPUTER_DOCKER_BIN", DefaultDockerBin),
		image:           envOr("VCOMPUTER_SANDBOX_IMAGE", DefaultImage),
		network:         envOr("VCOMPUTER_SANDBOX_NETWORK", DefaultNetwork),
		namePrefix:      envOr("VCOMPUTER_SANDBOX_NAME_PREFIX", DefaultNamePrefix),
		novncPort:       portOr(os.Getenv("VCOMPUTER_SANDBOX_NOVNC_PORT"), DefaultNOVNCPort),
		gatePort:        portOr(os.Getenv("VCOMPUTER_SANDBOX_GATE_PORT"), DefaultGatePort),
		geometry:        envOr("SCREEN_GEOMETRY", DefaultGeometry),
		execTimeout:     durationOr(os.Getenv("VCOMPUTER_EXEC_TIMEOUT"), DefaultExecTimeout),
		navigateTimeout: durationOr(os.Getenv("VCOMPUTER_NAVIGATE_TIMEOUT"), DefaultNavigateTimeout),
		waitTimeout:     defaultWaitTimeout,
		pollInterval:    defaultPollInterval,
		http:            &http.Client{Timeout: 10 * time.Second},
		clock:           time.Now,
	}
	for _, option := range options {
		option(runtime)
	}
	return runtime
}

// Ensure creates the container if it is not there, starts it if it is stopped, and
// waits until its browser answers. It is idempotent: a second call finds the same
// container, and the volume is created only once.
func (r *DockerRuntime) Ensure(ctx context.Context, botID string) (Session, error) {
	id, err := cleanBotID(botID)
	if err != nil {
		return Session{}, err
	}

	// The volume holds /home/bolu: the Chromium profile and the bot's files. It is
	// created once and reused, which is what makes "log in once, continue tomorrow"
	// possible without keeping the process alive (riset §2.3). The label is what
	// Cleanup finds it by later: a name pattern is a convention, a label is a record,
	// and the difference is what keeps a cleanup from removing a stranger's volume.
	if _, err := r.docker(ctx, "volume", "create",
		"--label", botLabel+"="+id, volumeName(r.namePrefix, id)); err != nil {
		return Session{}, fmt.Errorf("vcomputer: create the session volume: %w", err)
	}

	session, err := r.Status(ctx, id)
	switch {
	case errors.Is(err, ErrNoSession):
		// The secret is generated here and nowhere else: it is a credential for one
		// sandbox, it lives in that container's environment for as long as the
		// container does, and whoever needs it gets it back through GateTarget rather
		// than from a store the control plane would have to keep.
		secret, err := newSecret()
		if err != nil {
			return Session{}, err
		}
		if _, err := r.run(ctx, id, secret); err != nil {
			return Session{}, err
		}
	case err != nil:
		return Session{}, err
	case session.State != StateRunning:
		// A container that exists and is stopped is a session that was stopped on
		// purpose: start it rather than creating a second one, so the volume keeps
		// being the same session.
		if _, err := r.docker(ctx, "start", containerName(r.namePrefix, id)); err != nil {
			return Session{}, fmt.Errorf("vcomputer: start the sandbox: %w", err)
		}
	}

	if err := r.waitForGate(ctx, id); err != nil {
		return Session{}, err
	}
	return r.Status(ctx, id)
}

// Status reads the container's state. A bot with no container is ErrNoSession rather
// than a session in state "stopped": this API reports what the node actually holds,
// not what a record somewhere says it should.
func (r *DockerRuntime) Status(ctx context.Context, botID string) (Session, error) {
	id, err := cleanBotID(botID)
	if err != nil {
		return Session{}, err
	}

	out, err := r.docker(ctx, "inspect", "--format", inspectFormat, containerName(r.namePrefix, id))
	if err != nil {
		if missingContainer(err) {
			return Session{}, ErrNoSession
		}
		return Session{}, fmt.Errorf("vcomputer: inspect the sandbox: %w", err)
	}

	containerID, status, created := parseInspect(string(out))
	session := Session{
		BotID:       id,
		State:       StateStopped,
		Runtime:     "docker",
		ContainerID: containerID,
		Volume:      volumeName(r.namePrefix, id),
		NoVNCURL:    NoVNCPath(id),
		CreatedAt:   created,
		LastUsedAt:  r.clock(),
	}
	if status == containerRunning {
		session.State = StateRunning
		// CDPURL is a path on this control plane rather than the container's
		// address: the control plane holds the sandbox secret, so a caller that
		// drives the browser through it needs the control token and nothing else.
		// It is answered only for a running container, because a stopped session has
		// no browser to drive and a path that cannot work would mislead a caller.
		session.CDPURL = CDPPath(id)
	}
	return session, nil
}

// Stop stops the container and keeps the volume, so the next Ensure reuses the
// browser profile rather than starting a login from scratch.
func (r *DockerRuntime) Stop(ctx context.Context, botID string) error {
	id, err := cleanBotID(botID)
	if err != nil {
		return err
	}

	// Stop is honest about what it stops: a bot with no container, and a container
	// that is already down, are both ErrNoSession. That is what makes a second DELETE
	// answer 404 instead of reporting a stop that did not happen.
	session, err := r.Status(ctx, id)
	if err != nil {
		return err
	}
	if session.State != StateRunning {
		return ErrNoSession
	}

	if _, err := r.docker(ctx, "stop", containerName(r.namePrefix, id)); err != nil {
		if missingContainer(err) {
			return ErrNoSession
		}
		return fmt.Errorf("vcomputer: stop the sandbox: %w", err)
	}
	return nil
}

// Purge removes the session entirely: the container whatever state it is in, and
// the volume that holds the browser profile.
//
// The distinction from Stop is the point. Stop is "the machine is off" and keeps
// the profile so the next Ensure continues where the bot left off; Purge is "the
// machine is gone", which is what a delete request and a bot that was removed from
// the product both need. `-f` is deliberate: a session whose container is running,
// wedged, or already dead is still a session the caller asked to remove, and asking
// them to stop it first would be asking them to know the container's state.
//
// A bot with neither container nor volume is ErrNoSession, so a purge that retried
// answers 404 rather than reporting a removal that did not happen.
func (r *DockerRuntime) Purge(ctx context.Context, botID string) error {
	id, err := cleanBotID(botID)
	if err != nil {
		return err
	}

	removed := false
	if _, err := r.docker(ctx, "rm", "-f", containerName(r.namePrefix, id)); err != nil {
		if !missingContainer(err) {
			return fmt.Errorf("vcomputer: remove the sandbox: %w", err)
		}
	} else {
		removed = true
	}

	if _, err := r.docker(ctx, "volume", "rm", volumeName(r.namePrefix, id)); err != nil {
		if !missingVolume(err) {
			return fmt.Errorf("vcomputer: remove the session volume: %w", err)
		}
	} else {
		removed = true
	}

	if !removed {
		return ErrNoSession
	}
	return nil
}

// Cleanup removes what a crashed control plane left behind: every sandbox this
// runtime owns that is not running, and — when asked — their home volumes.
//
// It is scoped by the label and by the name prefix, so a deployment never removes
// another deployment's sandboxes on a shared host, and it never touches a running
// container: a running sandbox belongs to a session somebody is using, and a
// "cleanup" that killed it would be the opposite of what the word means.
//
// It answers counts of what it actually removed plus one line per object that was
// selected and could not be removed — a volume still mounted by a running container
// is the realistic case — because "nothing was removed" and "nothing could be
// removed" are different answers to a caller that just asked for a cleanup.
func (r *DockerRuntime) Cleanup(ctx context.Context, volumes bool) (CleanupResult, error) {
	return r.remove(ctx, false, volumes)
}

// Reap removes everything this runtime owns, running sandboxes included, and always
// their volumes. It is what the control plane calls on its way out.
//
// It exists because a sandbox is not part of the compose stack: the control plane
// creates it, so `docker compose down` cannot remove it, and a running container keeps
// both its volume and the internal network in use — which is what turns a clean
// teardown into leftovers an operator has to find by hand
// (docs/adr/0003-virtual-computer.md §3.9). The scoping is the same as Cleanup's, so
// this removes one deployment's sandboxes and never a second deployment's.
//
// `docker rm -f` is what removes a running container, and it is deliberate: the
// shutdown path has already decided that this control plane's sessions are over. The
// volumes are removed after the containers for the same reason: a volume mounted by a
// container that still exists cannot be removed.
func (r *DockerRuntime) Reap(ctx context.Context) (CleanupResult, error) {
	return r.remove(ctx, true, true)
}

// remove is the one implementation of "throw away what this runtime owns". The two
// public entry points differ in exactly two booleans, and keeping them as booleans here
// is what makes that difference visible rather than duplicated.
func (r *DockerRuntime) remove(ctx context.Context, running, volumes bool) (CleanupResult, error) {
	var result CleanupResult

	containers, err := r.list(ctx, []string{"ps"}, "-a", "--format", "{{.Names}}|{{.State}}")
	if err != nil {
		return CleanupResult{}, err
	}
	for _, line := range containers {
		name, state, found := strings.Cut(line, "|")
		if !found || (strings.TrimSpace(state) == containerRunning && !running) {
			continue
		}
		// -f is only meaningful for a running container and harmless for the rest: a
		// stopped one is removed either way, and asking for the force is what keeps
		// the two branches from needing different commands.
		if _, err := r.docker(ctx, "rm", "-f", strings.TrimSpace(name)); err != nil {
			result.Failed = append(result.Failed, failureLine(err))
			continue
		}
		result.RemovedContainers++
	}

	if !volumes {
		return result, nil
	}

	names, err := r.list(ctx, []string{"volume", "ls"}, "--format", "{{.Name}}")
	if err != nil {
		return CleanupResult{}, err
	}
	for _, name := range names {
		if _, err := r.docker(ctx, "volume", "rm", strings.TrimSpace(name)); err != nil {
			result.Failed = append(result.Failed, failureLine(err))
			continue
		}
		result.RemovedVolumes++
	}
	return result, nil
}

// list runs a listing command scoped to what this runtime owns: the label every
// sandbox carries, and the name prefix that keeps two deployments on one host apart.
// `docker` treats `name=` as a regular expression for containers and for volumes
// alike, which is why the prefix is anchored.
//
// The command arrives as its words rather than as one string because the scoping
// flags belong between the command and the listing's own arguments, and the command
// is not always one word: `ps --filter ... -a` and `volume ls --filter ...` are the
// two shapes this runtime asks for, and `volume --filter ... ls` is not a command
// docker has — it answers "unknown flag: --filter".
func (r *DockerRuntime) list(ctx context.Context, command []string, args ...string) ([]string, error) {
	scoped := make([]string, 0, len(command)+len(args)+4)
	scoped = append(scoped, command...)
	scoped = append(scoped,
		"--filter", "label="+botLabel,
		"--filter", "name=^"+regexp.QuoteMeta(r.namePrefix)+"-",
	)
	scoped = append(scoped, args...)

	out, err := r.docker(ctx, scoped...)
	if err != nil {
		return nil, fmt.Errorf("vcomputer: list the sandboxes: %w", err)
	}
	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines, nil
}

// Screenshot captures the Xvfb display inside the container: scrot writes a PNG, and
// the same exec prints it back over stdout.
//
// The exec is against the sandbox container only — the control plane never runs a
// command on its own host — and the bytes are checked for a PNG signature, because a
// runtime that answered something else would be a runtime that hands a caller a lie.
func (r *DockerRuntime) Screenshot(ctx context.Context, botID string) ([]byte, error) {
	id, err := cleanBotID(botID)
	if err != nil {
		return nil, err
	}

	output, err := r.dockerExec(ctx, maxScreenshotOutput, "-e", "DISPLAY="+sandboxDisplay,
		containerName(r.namePrefix, id), "sh", "-c", "scrot -o -z /tmp/vc-shot.png && cat /tmp/vc-shot.png")
	if err != nil {
		return nil, err
	}
	if len(output) == 0 {
		return nil, errors.New("vcomputer: the sandbox answered an empty screenshot")
	}
	if !bytes.HasPrefix(output, pngSignature) {
		return nil, errors.New("vcomputer: the sandbox did not answer a PNG")
	}
	return output, nil
}

// Click presses a point on the desktop.
func (r *DockerRuntime) Click(ctx context.Context, botID string, x, y int) error {
	id, err := cleanBotID(botID)
	if err != nil {
		return err
	}

	_, err = r.dockerExec(ctx, maxExecOutput, "-e", "DISPLAY="+sandboxDisplay,
		containerName(r.namePrefix, id), "xdotool", "mousemove", strconv.Itoa(x), strconv.Itoa(y), "click", "1")
	return err
}

// Type sends text to the focused window.
//
// The text is an argv entry, never part of a shell command: `xdotool type -- <text>`
// takes it literally, and `--` is what keeps a string that starts with a dash from
// being read as a flag.
func (r *DockerRuntime) Type(ctx context.Context, botID string, text string) error {
	id, err := cleanBotID(botID)
	if err != nil {
		return err
	}

	_, err = r.dockerExec(ctx, maxExecOutput, "-e", "DISPLAY="+sandboxDisplay,
		containerName(r.namePrefix, id), "xdotool", "type", "--delay", "20", "--", text)
	return err
}

// Navigate points the session's browser at a URL, leaves exactly one tab open, and
// then checks that the page actually loaded.
//
// The check is the part that matters. Chromium's HTTP endpoint answers "opened" as
// soon as it has a target, which is true even when there is nothing at the address: a
// sandbox has no route out (docker-compose.poc.yml, `internal: true`), so every
// external URL opens Chromium's error page and would otherwise be reported as a
// successful navigation. Verification drives the DevTools protocol over the target's
// websocket, through the gate, until the document reports that it finished loading.
//
// Every failure after the URL has been validated is a *NavigationError, which the
// HTTP surface answers 502 for: the request was well formed and the browser could not
// carry it out, which is not the caller's mistake. A bot with no session stays
// ErrNoSession, because that one is.
func (r *DockerRuntime) Navigate(ctx context.Context, botID string, targetURL string) (Navigation, error) {
	id, err := cleanBotID(botID)
	if err != nil {
		return Navigation{}, err
	}
	target, err := cleanURL(targetURL)
	if err != nil {
		return Navigation{}, err
	}

	gate, err := r.GateTarget(ctx, id)
	if err != nil {
		return Navigation{}, err
	}
	base := gate.URL + gatePrefix

	opened, err := r.openTarget(ctx, base, gate.Secret, target)
	if err != nil {
		return Navigation{}, navigationFailure(target, "", err)
	}
	if err := r.closeOtherPages(ctx, base, gate.Secret, opened.ID); err != nil {
		return Navigation{}, navigationFailure(target, "", err)
	}
	return r.verifyLoad(ctx, target, gate, opened)
}

// navigationFailure wraps a browser-side failure as the answer a caller can act on:
// the URL it asked for, wherever the browser got to, and why.
func navigationFailure(target, finalURL string, err error) *NavigationError {
	return &NavigationError{URL: target, FinalURL: finalURL, Reason: err.Error()}
}

// loadProbe is the expression the verification evaluates. It answers one JSON string
// with the four things the answer needs — whether the document finished loading,
// where the browser actually is, what the page calls itself, and what the server
// answered for the document — so the four describe one instant rather than four
// slightly different ones.
//
// The status comes from the navigation timing entry rather than from a Network event,
// and that is the cheaper half of the same fact: this poll already runs on the target's
// connection, `responseStatus` is the status of the response that actually committed —
// after any redirect, which is the response a caller means by "the page" — and a
// browser that does not report one answers 0, "not known", rather than a number that
// would be a lie. A document that never committed has no entry, and that case is
// already a failure: chrome-error:// for a navigation the browser could not make, and
// the timeout below for one that never finished.
const loadProbe = `JSON.stringify({state: document.readyState, href: location.href, title: document.title, status: (performance.getEntriesByType("navigation")[0] || {}).responseStatus || 0})`

// blankPage is what a freshly created target reports before its navigation commits.
const blankPage = "about:blank"

// loadPollInterval is how often the verification asks again. It is short because a
// navigation that works usually completes in well under a second, and the whole loop
// is bounded by navigateTimeout either way.
const loadPollInterval = 100 * time.Millisecond

// loadState is what one evaluation of loadProbe answers.
type loadState struct {
	State string `json:"state"`
	Href  string `json:"href"`
	Title string `json:"title"`
	// Status is the main document's HTTP status, 0 when the browser reported none.
	Status int `json:"status"`
}

// verifyLoad drives the target's DevTools websocket until the page it opened reports
// that it finished loading, and answers what the browser ended up showing.
//
// Three outcomes are failures rather than results, and they are the reason this
// exists: a document that finishes on chrome-error:// is Chromium's own error page —
// the sandbox reached nothing — a document that never finishes inside navigateTimeout
// is a page the caller must not be told loaded, and a document that finished on a
// 4xx/5xx response is an error page the server served, which the browser reports as a
// perfectly loaded page and a caller cannot tell from a real one.
func (r *DockerRuntime) verifyLoad(ctx context.Context, target string, gate SandboxTarget, opened cdpTarget) (Navigation, error) {
	ctx, cancel := context.WithTimeout(ctx, r.navigateTimeout)
	defer cancel()

	address, err := gateWebSocketURL(gate, opened)
	if err != nil {
		return Navigation{}, navigationFailure(target, "", err)
	}
	conn, err := dialWS(ctx, address, gate.Secret)
	if err != nil {
		return Navigation{}, navigationFailure(target, "", err)
	}
	defer func() { _ = conn.Close() }()

	// Page.enable is what makes the browser report load events, and it is also the
	// cheapest proof that the target accepts commands at all.
	if _, err := conn.call(1, "Page.enable", nil); err != nil {
		return Navigation{}, navigationFailure(target, "", err)
	}

	var (
		lastHref string
		lastErr  error
	)
	for id := 2; ; id++ {
		state, err := conn.evaluate(id)
		switch {
		case err != nil:
			// A browser replacing its context mid-navigation refuses the evaluation,
			// which is not a failure: the next round asks again. The error is kept so
			// that a navigation that never succeeds can say why.
			lastErr = err
		case strings.HasPrefix(state.Href, "chrome-error://"):
			return Navigation{}, &NavigationError{
				URL:      target,
				FinalURL: state.Href,
				Reason:   "the browser could not load the page",
			}
		default:
			lastErr = nil
			if state.Href != "" {
				lastHref = state.Href
			}
			// A target that has not committed a navigation yet reports about:blank,
			// which is complete the moment it exists. Waiting for a real address is
			// what keeps "opened" from being mistaken for "loaded" — except when
			// about:blank is the address that was asked for.
			if state.State == "complete" && (state.Href != blankPage || target == blankPage) {
				// A document that finished on 4xx or 5xx is a page the server refused
				// to serve, and reporting it as a navigation would be the same lie as
				// reporting chrome-error:// as one: the caller asked for a page and
				// got an error, which is exactly the case a 200-with-an-error-page
				// used to hide. The status travels with the failure so the caller can
				// tell a missing page (404) from a broken one (500).
				if state.Status >= 400 {
					return Navigation{}, &NavigationError{
						URL:        target,
						FinalURL:   state.Href,
						HTTPStatus: state.Status,
						Reason:     fmt.Sprintf("the page answered HTTP %d", state.Status),
					}
				}
				return Navigation{FinalURL: state.Href, Title: state.Title, HTTPStatus: state.Status}, nil
			}
		}

		if ctx.Err() != nil {
			reason := fmt.Sprintf("the page did not finish loading within %s", r.navigateTimeout)
			if lastErr != nil {
				reason += ": " + lastErr.Error()
			}
			return Navigation{}, &NavigationError{URL: target, FinalURL: lastHref, Reason: reason}
		}
		select {
		case <-ctx.Done():
		case <-time.After(loadPollInterval):
		}
	}
}

// evaluate runs the load probe over an attached connection. returnByValue is what
// makes the answer the string the expression produced rather than a handle to an
// object the control plane would have to ask about again.
func (c *wsConn) evaluate(id int) (loadState, error) {
	result, err := c.call(id, "Runtime.evaluate", map[string]any{
		"expression":    loadProbe,
		"returnByValue": true,
	})
	if err != nil {
		return loadState{}, err
	}

	var answer struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(result, &answer); err != nil {
		return loadState{}, fmt.Errorf("vcomputer: read the load probe: %w", err)
	}
	var state loadState
	if err := json.Unmarshal([]byte(answer.Result.Value), &state); err != nil {
		return loadState{}, fmt.Errorf("vcomputer: read the load probe: %w", err)
	}
	return state, nil
}

// GateTarget is the address of the session's gate and the secret it wants.
//
// The secret is read back out of the container's own environment rather than kept in
// the control plane: it is a credential for one sandbox, it was generated when that
// sandbox was created, and the container is where it lives. That also means the
// control plane holds no per-session secret in memory across restarts, and a
// container started by another instance of it is reached the same way.
func (r *DockerRuntime) GateTarget(ctx context.Context, botID string) (SandboxTarget, error) {
	id, err := cleanBotID(botID)
	if err != nil {
		return SandboxTarget{}, err
	}

	state, address, err := r.containerAddress(ctx, id)
	if err != nil {
		return SandboxTarget{}, err
	}
	if state != containerRunning {
		// A container that exists and is not running is a session that was stopped,
		// and it is not the same answer as a bot with no session at all: the caller
		// asked about something that is there, so this is a runtime failure (502)
		// rather than a 404. Its gate has no address on the network either way.
		return SandboxTarget{}, fmt.Errorf("vcomputer: sandbox %s is %s, so its gate is not listening", id, state)
	}

	secret, err := r.containerSecret(ctx, id)
	if err != nil {
		return SandboxTarget{}, err
	}
	return SandboxTarget{URL: sandboxURL(address, r.gatePort), Secret: secret}, nil
}

// Exec runs a command inside the sandbox and answers what it printed.
//
// This is the call riset §5.1 labels write_external, and nothing above this package
// is wired to it yet: a shell command has no machine-readable shape to gate on, so the
// gate has to exist before a model can reach it. What is real here is the mechanics —
// real stdout, real stderr, the real exit code — and Stub is false because the command
// really ran.
func (r *DockerRuntime) Exec(ctx context.Context, botID string, command string) (ExecResult, error) {
	id, err := cleanBotID(botID)
	if err != nil {
		return ExecResult{}, err
	}
	if strings.TrimSpace(command) == "" {
		return ExecResult{}, ErrNoCommand
	}

	execCtx, cancel := context.WithTimeout(ctx, r.execTimeout)
	defer cancel()

	stdout := &cappedBuffer{limit: maxExecOutput}
	stderr := &cappedBuffer{limit: maxExecOutput}

	cmd := exec.CommandContext(execCtx, r.bin, "exec", "-u", "bolu", containerName(r.namePrefix, id), "sh", "-c", command)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// A command that spawns a background process leaves that process holding the pipes
	// this command writes to, and the read side would wait for it: without WaitDelay the
	// deadline above bounds the process and not the call. The grace is short because the
	// process has already been killed; only the pipes are still open.
	cmd.WaitDelay = execKillGrace

	result := ExecResult{Command: command}
	err = cmd.Run()

	if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		// The deadline is checked before the exit code: a killed process reports an
		// exit too, and reporting it would look like a command that ran to completion.
		return ExecResult{}, fmt.Errorf("vcomputer: exec timed out after %s", r.execTimeout)
	}

	if err != nil {
		// A container that is not there is a missing session, not a command that ran and
		// failed. This is the rule dockerExec already applies, so an unknown session
		// answers the same way whichever route reached it.
		if failure := (&dockerError{args: []string{"exec"}, stderr: stderr.String(), err: err}); failure.missing() {
			return ExecResult{}, ErrNoSession
		}
	}

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		result.ExitCode = 0
	case errors.As(err, &exitErr):
		result.ExitCode = exitErr.ExitCode()
	default:
		// The command never started: there is no exit code to report, and answering one
		// anyway would look like a command that ran.
		return ExecResult{}, fmt.Errorf("vcomputer: exec the sandbox: %w", err)
	}

	result.Stdout = stdout.String()
	result.Stderr = stderr.String()
	if stdout.dropped || stderr.dropped {
		result.Stderr += fmt.Sprintf("\n[output truncated at %d bytes]\n", maxExecOutput)
	}
	return result, nil
}

// run creates the container. The absence of `-p` is the point: the desktop is reached
// over the sandbox network by the control plane, never from the public internet.
//
// The secret goes in as an environment variable and nowhere else — not as an argv
// entry, which `ps` would show to anything on the host — and it is what makes the
// sandbox's gate refuse every request that does not come from the control plane that
// started it.
func (r *DockerRuntime) run(ctx context.Context, botID, secret string) ([]byte, error) {
	args := []string{
		"run", "-d",
		"--name", containerName(r.namePrefix, botID),
		"--label", botLabel + "=" + botID,
		"--network", r.network,
		"--volume", volumeName(r.namePrefix, botID) + ":/home/bolu",
		"--env", "NOVNC_PORT=" + r.novncPort,
		"--env", "GATE_PORT=" + r.gatePort,
		"--env", "SANDBOX_SECRET=" + secret,
		"--env", "SCREEN_GEOMETRY=" + r.geometry,
		"--security-opt", "no-new-privileges:true",
		"--cap-drop", "ALL",
		"--memory", "2g",
		"--pids-limit", "512",
		r.image,
	}

	out, err := r.docker(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("vcomputer: start the sandbox: %w", err)
	}
	return out, nil
}

// waitForGate polls the sandbox's gate until it answers a DevTools request that
// carries the secret. A container that is up is not yet a browser that is up: Xvfb,
// the window manager, Chromium, and the gate itself all start inside it, and a caller
// that screenshots too early gets a black screen.
//
// The probe is the CDP endpoint rather than the gate's own health route on purpose:
// it is the request the control plane is about to make for real, so it answers "the
// thing I am waiting for works" instead of "a process is listening".
func (r *DockerRuntime) waitForGate(ctx context.Context, botID string) error {
	deadline := time.Now().Add(r.waitTimeout)
	var lastErr error

	for {
		gate, err := r.GateTarget(ctx, botID)
		if err != nil {
			lastErr = err
		} else if err := r.probe(ctx, gate.URL+gatePrefix+"/json/version", gate.Secret); err != nil {
			lastErr = err
		} else {
			return nil
		}

		if !time.Now().Before(deadline) {
			return fmt.Errorf("vcomputer: sandbox %s did not answer on CDP within %s: %v",
				botID, r.waitTimeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.pollInterval):
		}
	}
}

// containerAddress reads the container's state and its address on the sandbox
// network in one inspect. The network is named explicitly: a container may sit on
// more than one, and the address that matters is the one the control plane shares.
//
// The state comes back with the address because the two answer the same question —
// whether there is a gate to talk to — and reading them separately would leave a
// window in which the container is stopped between the two calls. A missing
// container is ErrNoSession; a container that exists and is not running is not,
// because the caller asked about a session that is there.
func (r *DockerRuntime) containerAddress(ctx context.Context, botID string) (state, address string, err error) {
	format := fmt.Sprintf(`{{.State.Status}}|{{(index .NetworkSettings.Networks %q).IPAddress}}`, r.network)

	out, err := r.docker(ctx, "inspect", "--format", format, containerName(r.namePrefix, botID))
	if err != nil {
		if missingContainer(err) {
			return "", "", ErrNoSession
		}
		return "", "", fmt.Errorf("vcomputer: inspect the sandbox network: %w", err)
	}

	fields := strings.Split(strings.TrimSpace(string(out)), "|")
	if len(fields) > 0 {
		state = strings.TrimSpace(fields[0])
	}
	if len(fields) > 1 {
		address = strings.TrimSpace(fields[1])
	}
	return state, address, nil
}

// containerSecret reads the sandbox secret back out of the container's environment.
//
// It is recovered rather than remembered so that the control plane holds no
// per-session credential across a restart: the container carries the secret it was
// created with, and any instance of the control plane that can inspect the container
// can reach its gate. `{{json .Config.Env}}` is used because it is one JSON array of
// "KEY=value" strings, which parses without depending on the daemon's output
// formatting the way a template with delimiters would.
func (r *DockerRuntime) containerSecret(ctx context.Context, botID string) (string, error) {
	out, err := r.docker(ctx, "inspect", "--format", `{{json .Config.Env}}`, containerName(r.namePrefix, botID))
	if err != nil {
		if missingContainer(err) {
			return "", ErrNoSession
		}
		return "", fmt.Errorf("vcomputer: read the sandbox secret: %w", err)
	}

	var environment []string
	if err := json.Unmarshal(out, &environment); err != nil {
		return "", fmt.Errorf("vcomputer: read the sandbox environment: %w", err)
	}
	for _, entry := range environment {
		if value, found := strings.CutPrefix(entry, secretEnv+"="); found {
			return value, nil
		}
	}
	// A container without a secret is a container no gate will talk to, and it is a
	// container this control plane did not create.
	return "", fmt.Errorf("vcomputer: sandbox %s carries no %s", botID, secretEnv)
}

// newSecret generates a sandbox secret: 16 bytes of crypto/rand as hex, which is 32
// characters. It is a credential for one container on a network that is already
// internal, so 128 bits of randomness is far past what a guess could reach.
func newSecret() (string, error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("vcomputer: generate a sandbox secret: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// openTarget opens one tab and answers the target Chromium created.
//
// The URL is percent-encoded the way a browser's encodeURIComponent would be: Chromium
// unescapes the query back into the address to open, and a raw space or `#` would break
// the request line before Chromium ever saw it.
//
// The whole target is answered, not just its id, because the verification needs the
// websocket address Chromium publishes for it.
func (r *DockerRuntime) openTarget(ctx context.Context, base, secret, target string) (cdpTarget, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/json/new?"+escapeTarget(target), nil)
	if err != nil {
		return cdpTarget{}, fmt.Errorf("vcomputer: build the navigation request: %w", err)
	}
	request.Header.Set(SecretHeader, secret)

	response, err := r.http.Do(request)
	if err != nil {
		return cdpTarget{}, fmt.Errorf("vcomputer: navigate: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return cdpTarget{}, fmt.Errorf("vcomputer: read the navigation answer: %w", err)
	}
	if response.StatusCode >= 400 {
		return cdpTarget{}, fmt.Errorf("vcomputer: navigate: chromium answered %s: %s",
			response.Status, strings.TrimSpace(string(payload)))
	}

	var opened cdpTarget
	if err := json.Unmarshal(payload, &opened); err != nil {
		return cdpTarget{}, fmt.Errorf("vcomputer: read the navigation answer: %w", err)
	}
	return opened, nil
}

// escapeTarget percent-encodes a URL for Chromium's /json/new query, keeping only the
// unreserved characters. A space becomes %20 rather than a plus: this query is not a
// form, and Chromium would open a URL containing a literal plus.
func escapeTarget(target string) string {
	return strings.ReplaceAll(url.QueryEscape(target), "+", "%20")
}

// closeOtherPages leaves one tab behind, so a session that navigates ten times holds
// one page rather than ten.
func (r *DockerRuntime) closeOtherPages(ctx context.Context, base, secret, keep string) error {
	targets, err := r.listTargets(ctx, base, secret)
	if err != nil {
		return err
	}

	for _, target := range targets {
		if target.Type != "page" || target.ID == keep {
			continue
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/json/close/"+target.ID, nil)
		if err != nil {
			return fmt.Errorf("vcomputer: build the close request: %w", err)
		}
		request.Header.Set(SecretHeader, secret)

		response, err := r.http.Do(request)
		if err != nil {
			return fmt.Errorf("vcomputer: close the previous tab: %w", err)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
		_ = response.Body.Close()
	}
	return nil
}

// cdpTarget is one entry of Chromium's target list, as much of it as this runtime
// reads: what it is, what it is called, and where its debugger is.
type cdpTarget struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	WSURL string `json:"webSocketDebuggerUrl"`
}

// listTargets reads the open targets.
func (r *DockerRuntime) listTargets(ctx context.Context, base, secret string) ([]cdpTarget, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/json/list", nil)
	if err != nil {
		return nil, fmt.Errorf("vcomputer: build the target list request: %w", err)
	}
	request.Header.Set(SecretHeader, secret)

	response, err := r.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("vcomputer: list the browser targets: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("vcomputer: read the target list: %w", err)
	}
	if response.StatusCode >= 400 {
		return nil, fmt.Errorf("vcomputer: list the browser targets: chromium answered %s", response.Status)
	}

	var targets []cdpTarget
	if err := json.Unmarshal(payload, &targets); err != nil {
		return nil, fmt.Errorf("vcomputer: read the target list: %w", err)
	}
	return targets, nil
}

// probe asks one CDP endpoint through a sandbox's gate whether it is up.
func (r *DockerRuntime) probe(ctx context.Context, url, secret string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set(SecretHeader, secret)

	response, err := r.http.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))

	if response.StatusCode >= 400 {
		return fmt.Errorf("cdp answered %s", response.Status)
	}
	return nil
}

// docker runs one docker command and answers its stdout.
func (r *DockerRuntime) docker(ctx context.Context, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, r.bin, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, &dockerError{args: args, stderr: stderr.String(), err: err}
	}
	return stdout.Bytes(), nil
}

// dockerExec runs `docker exec` against one session and answers its stdout, with the
// output capped and a missing container reported as ErrNoSession rather than as a
// runtime failure.
func (r *DockerRuntime) dockerExec(ctx context.Context, limit int, args ...string) ([]byte, error) {
	stdout := &cappedBuffer{limit: limit}
	stderr := &cappedBuffer{limit: maxExecOutput}

	full := append([]string{"exec", "-u", "bolu"}, args...)
	cmd := exec.CommandContext(ctx, r.bin, full...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		failure := &dockerError{args: full, stderr: stderr.String(), err: err}
		if failure.missing() {
			return nil, ErrNoSession
		}
		return nil, fmt.Errorf("vcomputer: exec in the sandbox: %w", failure)
	}
	return stdout.Bytes(), nil
}

// inspectFormat is what Status reads in one call: the container id, its state, and
// when it was created. The fields are separated by a pipe because neither a hex id
// nor an RFC 3339 timestamp can contain one.
const inspectFormat = "{{.Id}}|{{.State.Status}}|{{.Created}}"

// parseInspect splits that answer. A field the daemon did not fill in is left empty
// rather than guessed at.
func parseInspect(out string) (containerID, status string, created time.Time) {
	fields := strings.Split(strings.TrimSpace(out), "|")
	if len(fields) > 0 {
		containerID = strings.TrimSpace(fields[0])
	}
	if len(fields) > 1 {
		status = strings.TrimSpace(fields[1])
	}
	if len(fields) > 2 {
		if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(fields[2])); err == nil {
			created = parsed
		}
	}
	return containerID, status, created
}

// dockerError is one failed docker command: the argv, what the daemon said, and the
// underlying failure. It exists so "there is no such container" can be told apart from
// "the daemon is not there", which are a 404 and a 500.
type dockerError struct {
	args   []string
	stderr string
	err    error
}

func (e *dockerError) Error() string {
	message := strings.TrimSpace(e.stderr)
	if message == "" {
		message = e.err.Error()
	}
	return fmt.Sprintf("%s: %s", e.args[0], message)
}

func (e *dockerError) Unwrap() error { return e.err }

// missing reports whether the daemon answered "that container does not exist".
func (e *dockerError) missing() bool {
	message := strings.ToLower(e.stderr)
	return strings.Contains(message, "no such object") ||
		strings.Contains(message, "no such container")
}

// missingVolume reports whether the daemon answered "that volume does not exist".
// Docker words this differently from a missing container, and both have to be told
// apart from a daemon that is simply not there.
func missingVolume(err error) bool {
	var failure *dockerError
	if !errors.As(err, &failure) {
		return false
	}
	return strings.Contains(strings.ToLower(failure.stderr), "no such volume")
}

// failureLine reduces a docker failure to the one line a cleanup report can carry:
// what the daemon said, which is the part a person can act on.
func failureLine(err error) string {
	var failure *dockerError
	if errors.As(err, &failure) {
		if message := strings.TrimSpace(failure.stderr); message != "" {
			return message
		}
	}
	return err.Error()
}

// cappedBuffer collects at most limit bytes and remembers whether it had to drop any.
type cappedBuffer struct {
	buf     bytes.Buffer
	limit   int
	dropped bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if remaining := c.limit - c.buf.Len(); remaining > 0 {
		if len(p) <= remaining {
			c.buf.Write(p)
		} else {
			c.buf.Write(p[:remaining])
			c.dropped = true
		}
	} else if len(p) > 0 {
		c.dropped = true
	}
	// The full length is always reported: a short write would turn a truncation into
	// an I/O error inside the command being run.
	return len(p), nil
}

func (c *cappedBuffer) Bytes() []byte  { return c.buf.Bytes() }
func (c *cappedBuffer) String() string { return c.buf.String() }

// missingContainer reports whether an error came from a container that does not
// exist, which is a 404 for the caller rather than a runtime failure.
func missingContainer(err error) bool {
	var failure *dockerError
	return errors.As(err, &failure) && failure.missing()
}

// pngSignature is the first four bytes of every PNG, and the check that keeps a
// runtime from handing a caller an error message and calling it a screenshot.
var pngSignature = []byte{0x89, 'P', 'N', 'G'}

// envOr reads a variable, falling back to a default when it is unset or blank.
func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// portOr accepts a port number and falls back to a default for anything else. A port
// becomes part of a URL and of a container's environment, so a value that is not a
// number is a configuration mistake rather than something to pass on.
func portOr(value, fallback string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fallback
	}
	if _, err := strconv.Atoi(trimmed); err != nil {
		return fallback
	}
	return trimmed
}

// durationOr reads a Go duration such as "30s".
func durationOr(value string, fallback time.Duration) time.Duration {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(trimmed)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
