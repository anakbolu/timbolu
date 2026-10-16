package sandbox

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The docker runtime is tested against a fake docker client and a fake Chromium
// endpoint, both real: the client is a shell script that keeps the state a daemon
// would keep and writes down every call it received, and the endpoint is an HTTP
// server. Nothing here needs a daemon, and the recorded argv is what lets the test
// assert the exact command line the runtime builds — the flags that isolate a
// sandbox, and the absence of a published port.

// fakeDocker is the stand-in for the docker client: a script plus the state a daemon
// would hold for one container.
type fakeDocker struct {
	// bin is the script's path, and what the runtime is pointed at.
	bin string
	// logPath holds one line per call: the argv, tab separated.
	logPath string
	// statePath holds the container's state: absent, running, or exited.
	statePath string
	// shotPath is the PNG the fake `scrot` hands back.
	shotPath string
}

// newFakeDocker writes the script and the screenshot it serves.
func newFakeDocker(t *testing.T) *fakeDocker {
	t.Helper()

	dir := t.TempDir()
	fake := &fakeDocker{
		bin:       filepath.Join(dir, "docker"),
		logPath:   filepath.Join(dir, "calls.log"),
		statePath: filepath.Join(dir, "state"),
		shotPath:  filepath.Join(dir, "shot.png"),
	}
	writeTestPNG(t, fake.shotPath)

	script := strings.NewReplacer(
		"@LOG@", fake.logPath,
		"@STATE@", fake.statePath,
		"@SHOT@", fake.shotPath,
	).Replace(fakeDockerScript)

	if err := os.WriteFile(fake.bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake docker client: %v", err)
	}
	return fake
}

// fakeDockerScript is the client. `$1` is the verb, and the state file is what
// `inspect` reads, so the lifecycle behaves the way a daemon's would.
const fakeDockerScript = `#!/bin/sh
log_call() {
	{ printf '%s' "$1"; shift; for a in "$@"; do printf '\t%s' "$a"; done; printf '\n'; } >> "@LOG@"
}
log_call "$@"

state() {
	if [ -f "@STATE@" ]; then cat "@STATE@"; else echo absent; fi
}

case "$1" in
volume)
	exit 0
	;;
run)
	echo running > "@STATE@"
	echo container-id-123
	exit 0
	;;
start)
	echo running > "@STATE@"
	echo vcomputer-bot-a
	exit 0
	;;
stop)
	echo exited > "@STATE@"
	echo vcomputer-bot-a
	exit 0
	;;
inspect)
	if [ "$(state)" = absent ]; then
		echo "Error: No such object: vcomputer-bot-a" >&2
		exit 1
	fi
	case "$*" in
	*IPAddress*)
		echo "127.0.0.1"
		;;
	*)
		printf 'container-id-123|%s|2026-10-16T09:00:00Z\n' "$(state)"
		;;
	esac
	exit 0
	;;
exec)
	if [ "$(state)" = absent ]; then
		echo "Error: No such container: vcomputer-bot-a" >&2
		exit 1
	fi
	for a in "$@"; do
		case "$a" in
		*scrot*)
			cat "@SHOT@"
			exit 0
			;;
		*hello-from-sandbox*)
			echo "hello-from-sandbox"
			echo "bolu"
			exit 0
			;;
		*exit-3*)
			echo "boom"
			echo "bad" >&2
			exit 3
			;;
		*sleep*)
			sleep 5
			exit 0
			;;
		esac
	done
	exit 0
	;;
esac

echo "the fake docker client does not know $1" >&2
exit 2
`

// calls reads every call the fake client received, each as its argv.
func (f *fakeDocker) calls(t *testing.T) [][]string {
	t.Helper()

	payload, err := os.ReadFile(f.logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read the fake docker log: %v", err)
	}

	var calls [][]string
	for _, line := range strings.Split(strings.TrimRight(string(payload), "\n"), "\n") {
		if line == "" {
			continue
		}
		calls = append(calls, strings.Split(line, "\t"))
	}
	return calls
}

// forget drops the log, so a test can assert only on what happened after a point.
func (f *fakeDocker) forget(t *testing.T) {
	t.Helper()
	if err := os.Remove(f.logPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("clear the fake docker log: %v", err)
	}
}

// cdpRequest is one request the fake browser endpoint received, split the way the
// runtime builds it.
type cdpRequest struct {
	method string
	path   string
	query  string
}

// fakeCDP is the stand-in for Chromium's DevTools HTTP endpoint.
type fakeCDP struct {
	server *httptest.Server
	port   string

	mu       sync.Mutex
	requests []cdpRequest
}

// newFakeCDP serves the endpoints the runtime uses.
func newFakeCDP(t *testing.T) *fakeCDP {
	t.Helper()

	cdp := &fakeCDP{}
	cdp.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdp.mu.Lock()
		cdp.requests = append(cdp.requests, cdpRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery})
		cdp.mu.Unlock()

		switch {
		case r.URL.Path == "/json/version":
			_, _ = w.Write([]byte(`{"Browser":"Chrome/140.0.0.0"}`))
		case r.URL.Path == "/json/new":
			_, _ = w.Write([]byte(`{"id":"page-new","type":"page"}`))
		case r.URL.Path == "/json/list":
			_, _ = w.Write([]byte(`[
				{"id":"page-old","type":"page"},
				{"id":"page-new","type":"page"},
				{"id":"browser","type":"browser"}
			]`))
		case strings.HasPrefix(r.URL.Path, "/json/close/"):
			_, _ = w.Write([]byte("Target is closing"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(cdp.server.Close)

	parsed, err := url.Parse(cdp.server.URL)
	if err != nil {
		t.Fatalf("parse the fake cdp url: %v", err)
	}
	cdp.port = parsed.Port()
	return cdp
}

// seen returns the requests the fake endpoint received.
func (c *fakeCDP) seen() []cdpRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]cdpRequest(nil), c.requests...)
}

// dockerFixture is the runtime under test plus the two fakes behind it.
type dockerFixture struct {
	runtime *DockerRuntime
	docker  *fakeDocker
	cdp     *fakeCDP
}

// newDockerFixture builds a runtime pointed at the fakes. The sandbox's CDP port is
// the fake endpoint's port, so the URLs the runtime builds actually resolve.
func newDockerFixture(t *testing.T, options ...DockerOption) *dockerFixture {
	t.Helper()

	fake := newFakeDocker(t)
	cdp := newFakeCDP(t)

	base := []DockerOption{
		WithDockerBin(fake.bin),
		WithSandboxImage("vcomputer-sandbox:poc"),
		WithSandboxNetwork("vcomputer-internal"),
		WithSandboxPorts("6080", cdp.port),
		WithWaitTimeout(10 * time.Second),
	}
	return &dockerFixture{
		runtime: NewDocker(append(base, options...)...),
		docker:  fake,
		cdp:     cdp,
	}
}

// TestDockerEnsureCreatesAHardenedContainerAndWaitsForTheBrowser covers the whole
// start path: the volume, the container, the isolation flags, and the wait until the
// browser inside it answers.
func TestDockerEnsureCreatesAHardenedContainerAndWaitsForTheBrowser(t *testing.T) {
	fixture := newDockerFixture(t)

	session, err := fixture.runtime.Ensure(t.Context(), "bot-a")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}

	if session.State != StateRunning || session.Runtime != "docker" {
		t.Fatalf("session = %+v", session)
	}
	if session.ContainerID != "container-id-123" {
		t.Fatalf("container id = %q", session.ContainerID)
	}
	if session.Volume != "vcomputer-bot-a-home" {
		t.Fatalf("volume = %q", session.Volume)
	}
	// CDP is reached by address, never by name: Chromium refuses a request whose Host
	// header is neither an IP nor localhost.
	if want := "http://127.0.0.1:" + fixture.cdp.port; session.CDPURL != want {
		t.Fatalf("cdp url = %q, want %q", session.CDPURL, want)
	}
	if want := NoVNCPath("bot-a"); session.NoVNCURL != want {
		t.Fatalf("novnc url = %q, want %q", session.NoVNCURL, want)
	}

	calls := fixture.docker.calls(t)
	volume := findCall(t, calls, "volume", "create")
	if want := []string{"volume", "create", "vcomputer-bot-a-home"}; !equalArgv(volume, want) {
		t.Fatalf("volume call = %v, want %v", volume, want)
	}

	run := findCall(t, calls, "run")
	if run == nil {
		t.Fatalf("no run call: %v", calls)
	}
	// The isolation flags are this runtime's contract, so each one is named.
	for _, required := range [][2]string{
		{"--name", "vcomputer-bot-a"},
		{"--label", "vcomputer.bot=bot-a"},
		{"--network", "vcomputer-internal"},
		{"--volume", "vcomputer-bot-a-home:/home/bolu"},
		{"--env", "NOVNC_PORT=6080"},
		{"--env", "CDP_PORT=" + fixture.cdp.port},
		{"--security-opt", "no-new-privileges:true"},
		{"--cap-drop", "ALL"},
		{"--memory", "2g"},
		{"--pids-limit", "512"},
	} {
		if !containsPair(run, required[0], required[1]) {
			t.Fatalf("run call %v is missing %v", run, required)
		}
	}
	if last := run[len(run)-1]; last != "vcomputer-sandbox:poc" {
		t.Fatalf("run call %v does not end with the image", run)
	}

	// No published port anywhere: the desktop is reached over the sandbox network by
	// the control plane, never from outside.
	for _, call := range calls {
		for _, arg := range call {
			switch arg {
			case "-p", "--publish", "-P", "--expose":
				t.Fatalf("call %v publishes a port", call)
			}
		}
	}

	// The wait is real: the browser endpoint was asked whether it is up.
	if seen := fixture.cdp.seen(); len(seen) == 0 || seen[0] != (cdpRequest{method: http.MethodGet, path: "/json/version"}) {
		t.Fatalf("cdp requests = %v, want a readiness probe first", seen)
	}

	// A second ensure is the same session: no second container, and no start.
	fixture.docker.forget(t)
	again, err := fixture.runtime.Ensure(t.Context(), "bot-a")
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if again.ContainerID != session.ContainerID {
		t.Fatalf("second ensure answered container %q, want %q", again.ContainerID, session.ContainerID)
	}
	for _, call := range fixture.docker.calls(t) {
		if call[0] == "run" || call[0] == "start" {
			t.Fatalf("second ensure touched the container: %v", call)
		}
	}
}

// TestDockerEnsureStartsAStoppedContainerInsteadOfReplacingIt is the promise the named
// volume exists for: the machine goes off, the browser profile stays.
func TestDockerEnsureStartsAStoppedContainerInsteadOfReplacingIt(t *testing.T) {
	fixture := newDockerFixture(t)

	if _, err := fixture.runtime.Ensure(t.Context(), "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := fixture.runtime.Stop(t.Context(), "bot-a"); err != nil {
		t.Fatalf("stop: %v", err)
	}

	stopped, err := fixture.runtime.Status(t.Context(), "bot-a")
	if err != nil {
		t.Fatalf("status after stop: %v", err)
	}
	if stopped.State != StateStopped {
		t.Fatalf("state after stop = %q, want %q", stopped.State, StateStopped)
	}

	fixture.docker.forget(t)
	restarted, err := fixture.runtime.Ensure(t.Context(), "bot-a")
	if err != nil {
		t.Fatalf("ensure after stop: %v", err)
	}
	if restarted.State != StateRunning {
		t.Fatalf("state after restart = %q", restarted.State)
	}

	calls := fixture.docker.calls(t)
	if findCall(t, calls, "start") == nil {
		t.Fatalf("no start call: %v", calls)
	}
	if findCall(t, calls, "run") != nil {
		t.Fatalf("a stopped session was replaced instead of restarted: %v", calls)
	}
}

// TestDockerStopIsHonestAboutWhatItStops is what makes a second DELETE answer 404: a
// session that is not running is not a session that can be stopped.
func TestDockerStopIsHonestAboutWhatItStops(t *testing.T) {
	fixture := newDockerFixture(t)

	if err := fixture.runtime.Stop(t.Context(), "bot-a"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("stop without a container: err = %v, want ErrNoSession", err)
	}
	if _, err := fixture.runtime.Ensure(t.Context(), "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := fixture.runtime.Stop(t.Context(), "bot-a"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := fixture.runtime.Stop(t.Context(), "bot-a"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("second stop: err = %v, want ErrNoSession", err)
	}
}

// TestDockerScreenshotReturnsThePNGTheContainerProduced is the difference between this
// runtime and the stub: the bytes come from the sandbox's own display.
func TestDockerScreenshotReturnsThePNGTheContainerProduced(t *testing.T) {
	fixture := newDockerFixture(t)

	if _, err := fixture.runtime.Ensure(t.Context(), "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	payload, err := fixture.runtime.Screenshot(t.Context(), "bot-a")
	if err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	image, err := png.Decode(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("screenshot is not a decodable PNG: %v", err)
	}
	// The capture is the fake container's own image, not something the runtime made up.
	if bounds := image.Bounds(); bounds.Dx() != 4 || bounds.Dy() != 3 {
		t.Fatalf("screenshot is %dx%d, want the 4x3 capture the container produced", bounds.Dx(), bounds.Dy())
	}

	call := findCall(t, fixture.docker.calls(t), "exec")
	if call == nil {
		t.Fatal("no exec call for the screenshot")
	}
	joined := strings.Join(call, " ")
	if !strings.Contains(joined, "scrot") || !strings.Contains(joined, "DISPLAY=:99") {
		t.Fatalf("screenshot call = %v, want scrot against the sandbox display", call)
	}
}

// TestDockerClickAndTypePassTheirArgumentsLiterally covers the two actions that reach
// the X server: the coordinates, and text that must never be read as shell syntax.
func TestDockerClickAndTypePassTheirArgumentsLiterally(t *testing.T) {
	fixture := newDockerFixture(t)

	if _, err := fixture.runtime.Ensure(t.Context(), "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	fixture.docker.forget(t)

	if err := fixture.runtime.Click(t.Context(), "bot-a", 640, 400); err != nil {
		t.Fatalf("click: %v", err)
	}
	want := []string{"exec", "-u", "bolu", "-e", "DISPLAY=:99", "vcomputer-bot-a", "xdotool", "mousemove", "640", "400", "click", "1"}
	if got := findCall(t, fixture.docker.calls(t), "exec"); !equalArgv(got, want) {
		t.Fatalf("click call = %v, want %v", got, want)
	}

	// Text with spaces, a leading dash, and shell metacharacters: it arrives as one
	// argv entry, so `xdotool` types it literally and no shell of ours sees it.
	text := `-x; rm -rf /home/bolu $(whoami) "halo"`
	fixture.docker.forget(t)
	if err := fixture.runtime.Type(t.Context(), "bot-a", text); err != nil {
		t.Fatalf("type: %v", err)
	}
	want = []string{"exec", "-u", "bolu", "-e", "DISPLAY=:99", "vcomputer-bot-a", "xdotool", "type", "--delay", "20", "--", text}
	if got := findCall(t, fixture.docker.calls(t), "exec"); !equalArgv(got, want) {
		t.Fatalf("type call = %v, want %v", got, want)
	}
}

// TestDockerExecReportsTheRealResult is what the stub cannot do: a real exit code, real
// streams, and Stub false.
func TestDockerExecReportsTheRealResult(t *testing.T) {
	fixture := newDockerFixture(t)

	if _, err := fixture.runtime.Ensure(t.Context(), "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	result, err := fixture.runtime.Exec(t.Context(), "bot-a", "echo hello-from-sandbox && whoami")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if result.Stub {
		t.Fatal("Stub = true: the docker runtime really runs the command")
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d", result.ExitCode)
	}
	if !strings.Contains(result.Stdout, "hello-from-sandbox") || !strings.Contains(result.Stdout, "bolu") {
		t.Fatalf("stdout = %q", result.Stdout)
	}

	// A command that fails is a result, not an error: the caller asked for its exit code.
	failed, err := fixture.runtime.Exec(t.Context(), "bot-a", "exit-3")
	if err != nil {
		t.Fatalf("failing exec: %v", err)
	}
	if failed.ExitCode != 3 || !strings.Contains(failed.Stderr, "bad") {
		t.Fatalf("failed = %+v", failed)
	}

	if _, err := fixture.runtime.Exec(t.Context(), "bot-a", "   "); !errors.Is(err, ErrNoCommand) {
		t.Fatalf("blank command: err = %v, want ErrNoCommand", err)
	}
}

// TestDockerExecStopsAtItsDeadline keeps one hanging command from holding a worker's
// activity open forever.
func TestDockerExecStopsAtItsDeadline(t *testing.T) {
	fixture := newDockerFixture(t, WithExecTimeout(200*time.Millisecond))

	if _, err := fixture.runtime.Ensure(t.Context(), "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	started := time.Now()
	if _, err := fixture.runtime.Exec(t.Context(), "bot-a", "sleep 5"); err == nil {
		t.Fatal("exec of a hanging command answered without an error")
	} else if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("exec took %s, want it cut off at the deadline", elapsed)
	}
}

// TestDockerNavigateLeavesOneTabBehind covers the browser action: a target opened for
// the URL, and every other page target closed.
func TestDockerNavigateLeavesOneTabBehind(t *testing.T) {
	fixture := newDockerFixture(t)

	if _, err := fixture.runtime.Ensure(t.Context(), "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	// A URL with a space and a query, which is what a search or a fixture form produces.
	target := "http://vcomputer-fixture:8080/?q=halo timbolu&page=1"
	if err := fixture.runtime.Navigate(t.Context(), "bot-a", target); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// Everything after the readiness probe the start made: the open, the list, and the
	// close of the tab the session had before.
	seen := fixture.cdp.seen()[1:]
	want := []cdpRequest{
		{method: http.MethodPut, path: "/json/new"},
		{method: http.MethodGet, path: "/json/list"},
		{method: http.MethodGet, path: "/json/close/page-old"},
	}
	if len(seen) != len(want) {
		t.Fatalf("cdp requests = %v, want %v", seen, want)
	}
	for i, request := range want {
		if got := seen[i]; got.method != request.method || got.path != request.path {
			t.Fatalf("request %d = %v, want %v", i, got, request)
		}
	}

	// Chromium unescapes the query back into the address to open, so the URL the caller
	// gave is the URL the browser is handed — space and all.
	decoded, err := url.QueryUnescape(seen[0].query)
	if err != nil {
		t.Fatalf("the navigation query is not decodable: %v", err)
	}
	if decoded != target {
		t.Fatalf("chromium was asked to open %q, want %q", decoded, target)
	}
	for _, request := range seen {
		if request.path == "/json/close/page-new" || request.path == "/json/close/browser" {
			t.Fatalf("the wrong target was closed: %v", seen)
		}
	}

	// A URL the browser should not be handed is refused before any request is made.
	before := len(fixture.cdp.seen())
	if err := fixture.runtime.Navigate(t.Context(), "bot-a", "file:///home/bolu/.bash_history"); !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("err = %v, want ErrInvalidURL", err)
	}
	if after := len(fixture.cdp.seen()); after != before {
		t.Fatalf("a refused navigation reached the browser: %v", fixture.cdp.seen()[before:])
	}
}

// TestDockerNoVNCTargetIsTheContainerAddress covers what the control plane proxies to.
func TestDockerNoVNCTargetIsTheContainerAddress(t *testing.T) {
	fixture := newDockerFixture(t)

	if _, err := fixture.runtime.NoVNCTarget(t.Context(), "bot-a"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("target without a container: err = %v, want ErrNoSession", err)
	}
	if _, err := fixture.runtime.Ensure(t.Context(), "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	target, err := fixture.runtime.NoVNCTarget(t.Context(), "bot-a")
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	if target != "http://127.0.0.1:6080" {
		t.Fatalf("target = %q", target)
	}
}

// TestDockerActionsOnAMissingContainerAreNoSession keeps a 404 honest: every action
// reports that there is no session rather than a runtime failure.
func TestDockerActionsOnAMissingContainerAreNoSession(t *testing.T) {
	fixture := newDockerFixture(t)
	ctx := t.Context()

	if _, err := fixture.runtime.Status(ctx, "bot-a"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("status: err = %v, want ErrNoSession", err)
	}
	if _, err := fixture.runtime.Screenshot(ctx, "bot-a"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("screenshot: err = %v, want ErrNoSession", err)
	}
	if err := fixture.runtime.Click(ctx, "bot-a", 1, 2); !errors.Is(err, ErrNoSession) {
		t.Fatalf("click: err = %v, want ErrNoSession", err)
	}
	if err := fixture.runtime.Type(ctx, "bot-a", "halo"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("type: err = %v, want ErrNoSession", err)
	}
	if err := fixture.runtime.Navigate(ctx, "bot-a", "https://example.com"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("navigate: err = %v, want ErrNoSession", err)
	}
	if _, err := fixture.runtime.Exec(ctx, "bot-a", "ls"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("exec: err = %v, want ErrNoSession", err)
	}
}

// TestDockerEnsureReportsAnUnreachableBrowser is the honest failure for a container
// whose browser never came up: the session is not handed to the caller.
func TestDockerEnsureReportsAnUnreachableBrowser(t *testing.T) {
	fixture := newDockerFixture(t, WithWaitTimeout(300*time.Millisecond))

	// The container starts, but nothing answers on its CDP port.
	fixture.cdp.server.Close()

	_, err := fixture.runtime.Ensure(t.Context(), "bot-a")
	if err == nil {
		t.Fatal("ensure answered a session for a sandbox whose browser never came up")
	}
	if !strings.Contains(err.Error(), "did not answer on CDP") {
		t.Fatalf("err = %v, want the wait to have timed out", err)
	}
}

// TestDockerConfigurationComesFromTheEnvironment covers the deployment seam: the
// compose file sets these, and the runtime must not hardcode what the spec requires it
// to configure.
func TestDockerConfigurationComesFromTheEnvironment(t *testing.T) {
	t.Setenv("VCOMPUTER_DOCKER_BIN", "/usr/local/bin/podman-docker")
	t.Setenv("VCOMPUTER_SANDBOX_IMAGE", "registry.example/vcomputer-sandbox:1")
	t.Setenv("VCOMPUTER_SANDBOX_NETWORK", "vcomputer-other")
	t.Setenv("VCOMPUTER_SANDBOX_NOVNC_PORT", "7080")
	t.Setenv("VCOMPUTER_SANDBOX_CDP_PORT", "9224")
	t.Setenv("VCOMPUTER_EXEC_TIMEOUT", "5s")
	t.Setenv("SCREEN_GEOMETRY", "1920x1080x24")

	runtime := NewDocker()
	if runtime.bin != "/usr/local/bin/podman-docker" {
		t.Fatalf("bin = %q", runtime.bin)
	}
	if runtime.image != "registry.example/vcomputer-sandbox:1" {
		t.Fatalf("image = %q", runtime.image)
	}
	if runtime.network != "vcomputer-other" {
		t.Fatalf("network = %q", runtime.network)
	}
	if runtime.novncPort != "7080" || runtime.cdpPort != "9224" {
		t.Fatalf("ports = %q/%q", runtime.novncPort, runtime.cdpPort)
	}
	if runtime.execTimeout != 5*time.Second {
		t.Fatalf("exec timeout = %s", runtime.execTimeout)
	}
	if runtime.geometry != "1920x1080x24" {
		t.Fatalf("geometry = %q", runtime.geometry)
	}
}

// TestDockerDefaultsAreTheOnesTheStackUses keeps the two in step: the image the compose
// file builds and the network it declares are the ones the runtime reaches for.
func TestDockerDefaultsAreTheOnesTheStackUses(t *testing.T) {
	for _, key := range []string{
		"VCOMPUTER_DOCKER_BIN", "VCOMPUTER_SANDBOX_IMAGE", "VCOMPUTER_SANDBOX_NETWORK",
		"VCOMPUTER_SANDBOX_NOVNC_PORT", "VCOMPUTER_SANDBOX_CDP_PORT", "VCOMPUTER_EXEC_TIMEOUT",
		"SCREEN_GEOMETRY",
	} {
		t.Setenv(key, "")
	}

	runtime := NewDocker()
	if runtime.bin != DefaultDockerBin {
		t.Fatalf("bin = %q, want %q", runtime.bin, DefaultDockerBin)
	}
	if runtime.image != DefaultImage {
		t.Fatalf("image = %q, want %q", runtime.image, DefaultImage)
	}
	if runtime.network != DefaultNetwork {
		t.Fatalf("network = %q, want %q", runtime.network, DefaultNetwork)
	}
	if runtime.novncPort != DefaultNOVNCPort || runtime.cdpPort != DefaultCDPPort {
		t.Fatalf("ports = %q/%q, want %q/%q", runtime.novncPort, runtime.cdpPort, DefaultNOVNCPort, DefaultCDPPort)
	}
	if runtime.execTimeout != DefaultExecTimeout {
		t.Fatalf("exec timeout = %s, want %s", runtime.execTimeout, DefaultExecTimeout)
	}
}

// TestDockerPortConfigurationRefusesWhatIsNotAPort keeps a typo from becoming a URL
// that cannot work.
func TestDockerPortConfigurationRefusesWhatIsNotAPort(t *testing.T) {
	t.Setenv("VCOMPUTER_SANDBOX_NOVNC_PORT", "not-a-port")
	t.Setenv("VCOMPUTER_SANDBOX_CDP_PORT", "0x17")

	runtime := NewDocker()
	if runtime.novncPort != DefaultNOVNCPort || runtime.cdpPort != DefaultCDPPort {
		t.Fatalf("ports = %q/%q, want the defaults", runtime.novncPort, runtime.cdpPort)
	}
}

// TestMissingContainerIsRecognised covers the classification a 404 depends on: the
// daemon's "no such object" is a missing session, and anything else is a real failure.
func TestMissingContainerIsRecognised(t *testing.T) {
	missing := &dockerError{args: []string{"inspect"}, stderr: "Error: No such object: vcomputer-bot-a", err: errors.New("exit status 1")}
	if !missingContainer(missing) {
		t.Fatal("a missing container was not recognised")
	}
	refused := &dockerError{args: []string{"inspect"}, stderr: "Cannot connect to the Docker daemon at unix:///var/run/docker.sock", err: errors.New("exit status 1")}
	if missingContainer(refused) {
		t.Fatal("an unreachable daemon was reported as a missing container")
	}
}

// writeTestPNG writes a small real PNG for the fake `scrot` to answer with.
func writeTestPNG(t *testing.T, path string) {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	for x := range 4 {
		for y := range 3 {
			img.Set(x, y, color.RGBA{R: uint8(x * 60), G: uint8(y * 80), B: 0x40, A: 0xff})
		}
	}

	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatalf("encode the test png: %v", err)
	}
	if err := os.WriteFile(path, buffer.Bytes(), 0o644); err != nil {
		t.Fatalf("write the test png: %v", err)
	}
}

// findCall returns the first call whose argv starts with the given words.
func findCall(t *testing.T, calls [][]string, prefix ...string) []string {
	t.Helper()
	for _, call := range calls {
		if len(call) >= len(prefix) && equalArgv(call[:len(prefix)], prefix) {
			return call
		}
	}
	return nil
}

// equalArgv compares two argument vectors.
func equalArgv(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// containsPair reports whether an argv holds a flag immediately followed by its value.
func containsPair(argv []string, flag, value string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == value {
			return true
		}
	}
	return false
}
