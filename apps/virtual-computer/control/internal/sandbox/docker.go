package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// DockerRuntime is the real runtime: one container per bot, its home in a named
// volume, driven through the docker CLI.
//
// It shells out to the docker binary rather than importing a Docker SDK, and that
// is the one dependency decision in this module: the control plane keeps a stdlib
// only go.mod, so the client it speaks to the daemon with is the CLI that is already
// on the host (docs/adr/0003-virtual-computer.md §3.2). Every argument is passed as
// argv, never through a shell, so a bot id or a typed string cannot become a command.
//
// What it deliberately does not do:
//
//   - It does not publish a port. The desktop is reached over the sandbox network by
//     the control plane, which proxies noVNC (docs/adr/0003 §3.6); `docker run` here
//     never gets a `-p`.
//   - It does not pass --privileged, --device, or a runtime flag: the isolation story
//     for the PoC is "hardened runc container" (docs/adr/0003 §3.3).
//   - It does not remove a volume. `Stop` is "the machine is off", not "the machine
//     is gone": the browser profile is the persistent half of the session.
type DockerRuntime struct {
	// bin is the docker client to run. It is a field so a test can point the runtime
	// at a fake script and never touch a real daemon.
	bin string
	// image is the sandbox image tag, e.g. vcomputer-sandbox:poc.
	image string
	// network is the sandbox network the container joins. The control plane must
	// share it: that network is how it reaches the noVNC port and the CDP endpoint,
	// and it is the reason no port is ever published.
	network string
	// novncPort and cdpPort are the ports the sandbox listens on inside its own
	// network. They are configuration, not constants: the control plane passes them
	// to each sandbox it starts.
	novncPort string
	cdpPort   string
	// geometry is the Xvfb screen size handed to the sandbox.
	geometry string
	// execTimeout bounds one Exec. A command that hangs must not hold a worker's
	// activity open forever.
	execTimeout time.Duration
	// waitTimeout and pollInterval bound how long Ensure waits for a starting
	// container to answer on its CDP endpoint.
	waitTimeout  time.Duration
	pollInterval time.Duration
	// http talks to the sandbox's CDP endpoint and never to anything else.
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
	// maxExecOutput caps what one command may return, so a runaway process cannot
	// fill the control plane's memory with its own output.
	maxExecOutput = 64 << 10
	// maxScreenshotOutput caps one capture. A 1280x800 PNG is well under a megabyte;
	// this only refuses something that is not a screenshot at all.
	maxScreenshotOutput = 16 << 20
	// defaultWaitTimeout is how long Ensure waits for a cold container to answer on
	// CDP: Chromium, Xvfb, and a window manager all have to come up inside it.
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

// WithSandboxPorts sets the ports the sandbox listens on inside its own network.
func WithSandboxPorts(novncPort, cdpPort string) DockerOption {
	return func(r *DockerRuntime) {
		r.novncPort = portOr(novncPort, DefaultNOVNCPort)
		r.cdpPort = portOr(cdpPort, DefaultCDPPort)
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
		bin:          envOr("VCOMPUTER_DOCKER_BIN", DefaultDockerBin),
		image:        envOr("VCOMPUTER_SANDBOX_IMAGE", DefaultImage),
		network:      envOr("VCOMPUTER_SANDBOX_NETWORK", DefaultNetwork),
		novncPort:    portOr(os.Getenv("VCOMPUTER_SANDBOX_NOVNC_PORT"), DefaultNOVNCPort),
		cdpPort:      portOr(os.Getenv("VCOMPUTER_SANDBOX_CDP_PORT"), DefaultCDPPort),
		geometry:     envOr("SCREEN_GEOMETRY", DefaultGeometry),
		execTimeout:  durationOr(os.Getenv("VCOMPUTER_EXEC_TIMEOUT"), DefaultExecTimeout),
		waitTimeout:  defaultWaitTimeout,
		pollInterval: defaultPollInterval,
		http:         &http.Client{Timeout: 10 * time.Second},
		clock:        time.Now,
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
	// possible without keeping the process alive (riset §2.3).
	if _, err := r.docker(ctx, "volume", "create", volumeName(id)); err != nil {
		return Session{}, fmt.Errorf("vcomputer: create the session volume: %w", err)
	}

	session, err := r.Status(ctx, id)
	switch {
	case errors.Is(err, ErrNoSession):
		if _, err := r.run(ctx, id); err != nil {
			return Session{}, err
		}
	case err != nil:
		return Session{}, err
	case session.State != StateRunning:
		// A container that exists and is stopped is a session that was stopped on
		// purpose: start it rather than creating a second one, so the volume keeps
		// being the same session.
		if _, err := r.docker(ctx, "start", containerName(id)); err != nil {
			return Session{}, fmt.Errorf("vcomputer: start the sandbox: %w", err)
		}
	}

	if err := r.waitForCDP(ctx, id); err != nil {
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

	out, err := r.docker(ctx, "inspect", "--format", inspectFormat, containerName(id))
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
		Volume:      volumeName(id),
		NoVNCURL:    NoVNCPath(id),
		CreatedAt:   created,
		LastUsedAt:  r.clock(),
	}
	if status == "running" {
		session.State = StateRunning
		// Chromium refuses a request whose Host header is neither an IP nor
		// localhost, so the endpoint the caller is handed is the container's address
		// on the sandbox network, never its name.
		if address, err := r.containerIP(ctx, id); err == nil && address != "" {
			session.CDPURL = sandboxURL(address, r.cdpPort)
		}
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

	if _, err := r.docker(ctx, "stop", containerName(id)); err != nil {
		if missingContainer(err) {
			return ErrNoSession
		}
		return fmt.Errorf("vcomputer: stop the sandbox: %w", err)
	}
	return nil
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
		containerName(id), "sh", "-c", "scrot -o -z /tmp/vc-shot.png && cat /tmp/vc-shot.png")
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
		containerName(id), "xdotool", "mousemove", strconv.Itoa(x), strconv.Itoa(y), "click", "1")
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
		containerName(id), "xdotool", "type", "--delay", "20", "--", text)
	return err
}

// Navigate points the session's browser at a URL and leaves exactly one tab open.
//
// It uses Chromium's HTTP endpoint rather than the DevTools websocket: `PUT /json/new`
// opens a target and answers it as JSON, and closing every other page target is what
// keeps a session from accumulating a tab per navigation.
func (r *DockerRuntime) Navigate(ctx context.Context, botID string, targetURL string) error {
	id, err := cleanBotID(botID)
	if err != nil {
		return err
	}
	target, err := cleanURL(targetURL)
	if err != nil {
		return err
	}

	base, err := r.cdpBase(ctx, id)
	if err != nil {
		return err
	}

	opened, err := r.openTarget(ctx, base, target)
	if err != nil {
		return err
	}
	return r.closeOtherPages(ctx, base, opened)
}

// NoVNCTarget is the address the control plane's noVNC proxy forwards to.
func (r *DockerRuntime) NoVNCTarget(ctx context.Context, botID string) (string, error) {
	id, err := cleanBotID(botID)
	if err != nil {
		return "", err
	}

	address, err := r.containerIP(ctx, id)
	if err != nil {
		return "", err
	}
	if address == "" {
		return "", ErrNoSession
	}
	return sandboxURL(address, r.novncPort), nil
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

	cmd := exec.CommandContext(execCtx, r.bin, "exec", "-u", "bolu", containerName(id), "sh", "-c", command)
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
func (r *DockerRuntime) run(ctx context.Context, botID string) ([]byte, error) {
	args := []string{
		"run", "-d",
		"--name", containerName(botID),
		"--label", "vcomputer.bot=" + botID,
		"--network", r.network,
		"--volume", volumeName(botID) + ":/home/bolu",
		"--env", "NOVNC_PORT=" + r.novncPort,
		"--env", "CDP_PORT=" + r.cdpPort,
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

// waitForCDP polls the container's DevTools endpoint until it answers. A container
// that is up is not yet a browser that is up: Xvfb, the window manager, and Chromium
// all start inside it, and a caller that screenshots too early gets a black screen.
func (r *DockerRuntime) waitForCDP(ctx context.Context, botID string) error {
	deadline := time.Now().Add(r.waitTimeout)
	var lastErr error

	for {
		address, err := r.containerIP(ctx, botID)
		if err != nil {
			lastErr = err
		} else if address != "" {
			if err := r.probe(ctx, sandboxURL(address, r.cdpPort)+"/json/version"); err != nil {
				lastErr = err
			} else {
				return nil
			}
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

// containerIP is the container's address on the sandbox network. The network is named
// explicitly: a container may sit on more than one, and the address that matters is
// the one the control plane shares.
func (r *DockerRuntime) containerIP(ctx context.Context, botID string) (string, error) {
	format := fmt.Sprintf(`{{(index .NetworkSettings.Networks %q).IPAddress}}`, r.network)

	out, err := r.docker(ctx, "inspect", "--format", format, containerName(botID))
	if err != nil {
		if missingContainer(err) {
			return "", ErrNoSession
		}
		return "", fmt.Errorf("vcomputer: inspect the sandbox network: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// cdpBase is the CDP endpoint of a running session.
func (r *DockerRuntime) cdpBase(ctx context.Context, botID string) (string, error) {
	address, err := r.containerIP(ctx, botID)
	if err != nil {
		return "", err
	}
	if address == "" {
		return "", ErrNoSession
	}
	return sandboxURL(address, r.cdpPort), nil
}

// openTarget opens one tab and answers its target id.
//
// The URL is percent-encoded the way a browser's encodeURIComponent would be: Chromium
// unescapes the query back into the address to open, and a raw space or `#` would break
// the request line before Chromium ever saw it.
func (r *DockerRuntime) openTarget(ctx context.Context, base, target string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/json/new?"+escapeTarget(target), nil)
	if err != nil {
		return "", fmt.Errorf("vcomputer: build the navigation request: %w", err)
	}

	response, err := r.http.Do(request)
	if err != nil {
		return "", fmt.Errorf("vcomputer: navigate: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("vcomputer: read the navigation answer: %w", err)
	}
	if response.StatusCode >= 400 {
		return "", fmt.Errorf("vcomputer: navigate: chromium answered %s: %s",
			response.Status, strings.TrimSpace(string(payload)))
	}

	var opened struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(payload, &opened); err != nil {
		return "", fmt.Errorf("vcomputer: read the navigation answer: %w", err)
	}
	return opened.ID, nil
}

// escapeTarget percent-encodes a URL for Chromium's /json/new query, keeping only the
// unreserved characters. A space becomes %20 rather than a plus: this query is not a
// form, and Chromium would open a URL containing a literal plus.
func escapeTarget(target string) string {
	return strings.ReplaceAll(url.QueryEscape(target), "+", "%20")
}

// closeOtherPages leaves one tab behind, so a session that navigates ten times holds
// one page rather than ten.
func (r *DockerRuntime) closeOtherPages(ctx context.Context, base, keep string) error {
	targets, err := r.listTargets(ctx, base)
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
		response, err := r.http.Do(request)
		if err != nil {
			return fmt.Errorf("vcomputer: close the previous tab: %w", err)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
		_ = response.Body.Close()
	}
	return nil
}

// target is one entry of Chromium's target list, as much of it as this runtime reads.
type target struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// listTargets reads the open targets.
func (r *DockerRuntime) listTargets(ctx context.Context, base string) ([]target, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/json/list", nil)
	if err != nil {
		return nil, fmt.Errorf("vcomputer: build the target list request: %w", err)
	}

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

	var targets []target
	if err := json.Unmarshal(payload, &targets); err != nil {
		return nil, fmt.Errorf("vcomputer: read the target list: %w", err)
	}
	return targets, nil
}

// probe asks one CDP endpoint whether it is up.
func (r *DockerRuntime) probe(ctx context.Context, url string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

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
