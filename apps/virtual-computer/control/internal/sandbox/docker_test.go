package sandbox

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
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

// The docker runtime is tested against a fake docker client and a fake sandbox gate,
// both real: the client is a shell script that keeps the state a daemon would keep and
// writes down every call it received, and the gate is an HTTP server that also speaks
// the DevTools websocket. Nothing here needs a daemon, and the recorded argv is what
// lets the test assert the exact command line the runtime builds — the flags that
// isolate a sandbox, the secret it hands over, and the absence of a published port.
//
// The fake gate is what makes the browser tests meaningful. Chromium publishes its
// debugger on its own loopback address, which the control plane cannot dial and must
// not be able to; the fake answers the same paths under the gate's address instead, so
// a navigation that works is one that went through the gate.

// fakeDocker is the stand-in for the docker client: a script plus the state a daemon
// would hold for the containers and volumes the tests put there.
type fakeDocker struct {
	// bin is the script's path, and what the runtime is pointed at.
	bin string
	// logPath holds one line per call: the argv, tab separated.
	logPath string
	// stateDir holds one file per container, named after the container. The file is
	// the record `inspect` cuts fields out of.
	stateDir string
	// volumesPath holds one volume name per line, which is what `volume ls` answers.
	volumesPath string
	// refusePath holds the objects whose removal the fake daemon refuses, which is
	// how a cleanup that cannot remove something is exercised.
	refusePath string
	// shotPath is the PNG the fake `scrot` hands back.
	shotPath string
}

// newFakeDocker writes the script and the screenshot it serves.
func newFakeDocker(t *testing.T) *fakeDocker {
	t.Helper()

	dir := t.TempDir()
	fake := &fakeDocker{
		bin:         filepath.Join(dir, "docker"),
		logPath:     filepath.Join(dir, "calls.log"),
		stateDir:    filepath.Join(dir, "containers"),
		volumesPath: filepath.Join(dir, "volumes"),
		refusePath:  filepath.Join(dir, "refuse"),
		shotPath:    filepath.Join(dir, "shot.png"),
	}
	if err := os.MkdirAll(fake.stateDir, 0o755); err != nil {
		t.Fatalf("create the fake docker state: %v", err)
	}
	writeTestPNG(t, fake.shotPath)

	script := strings.NewReplacer(
		"@LOG@", fake.logPath,
		"@STATE@", fake.stateDir,
		"@VOLUMES@", fake.volumesPath,
		"@REFUSE@", fake.refusePath,
		"@SHOT@", fake.shotPath,
	).Replace(fakeDockerScript)

	if err := os.WriteFile(fake.bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake docker client: %v", err)
	}
	return fake
}

// fakeDockerScript is the client. `$1` is the verb, and the state directory is what
// `inspect`, `ps`, and the lifecycle read, so the fake behaves the way a daemon's would.
//
// What it deliberately does not do is re-implement `docker`'s filter matching: the
// runtime asks a listing for the label and the name prefix it owns, and the test
// asserts that argv. The fake answers what a daemon that had applied those filters
// would have answered.
//
// A container record is `id|state|created|ip|secret`, which is what the three inspect
// formats the runtime asks for are cut out of. The secret is what the container was
// started with, and it is what `inspect --format '{{json .Config.Env}}'` reports back:
// the control plane recovers it from there rather than remembering it.
const fakeDockerScript = `#!/bin/sh
log_call() {
	{ printf '%s' "$1"; shift; for a in "$@"; do printf '\t%s' "$a"; done; printf '\n'; } >> "@LOG@"
}
log_call "$@"

record() {
	if [ -f "@STATE@/$1" ]; then cat "@STATE@/$1"; else echo absent; fi
}

field() {
	printf '%s' "$1" | cut -d'|' -f "$2"
}

refuse() {
	[ -f "@REFUSE@" ] && grep -qx "$1" "@REFUSE@"
}

# positional prints the arguments that are not flags and not the values of the flags
# this runtime uses, one per line. A listing carries the runtime's own --filter pairs
# between the verb and the rest of its arguments, so the verb is the first word left
# once the flags are taken out.
positional() {
	shift
	skip=no
	for a in "$@"; do
		if [ "$skip" = yes ]; then skip=no; continue; fi
		case "$a" in
		--filter|--format|--label|--name|--volume|--network|--env|--security-opt|--cap-drop|--memory|--pids-limit)
			skip=yes
			continue
			;;
		-*)
			continue
			;;
		esac
		printf '%s\n' "$a"
	done
}

# last_arg prints the last argument, which is where the object a removal names sits: a
# removal has no flags before its name in this runtime's usage.
last_arg() {
	value=""
	for a in "$@"; do value="$a"; done
	printf '%s' "$value"
}

set_state() {
	sed "s/^\([^|]*\)|[^|]*|/\1|$2|/" "@STATE@/$1" > "@STATE@/$1.tmp"
	mv "@STATE@/$1.tmp" "@STATE@/$1"
}

case "$1" in
volume)
	verb=$(positional "$@" | sed -n 1p)
	name=$(positional "$@" | sed -n 2p)
	case "$verb" in
	create)
		grep -qx "$name" "@VOLUMES@" 2>/dev/null || echo "$name" >> "@VOLUMES@"
		echo "$name"
		exit 0
		;;
	rm)
		if refuse "$name"; then
			echo "Error response from daemon: remove $name: volume is in use" >&2
			exit 1
		fi
		if grep -qx "$name" "@VOLUMES@" 2>/dev/null; then
			grep -vx "$name" "@VOLUMES@" > "@VOLUMES@.tmp"
			mv "@VOLUMES@.tmp" "@VOLUMES@"
			echo "$name"
			exit 0
		fi
		echo "Error: No such volume: $name" >&2
		exit 1
		;;
	ls)
		if [ -f "@VOLUMES@" ]; then cat "@VOLUMES@"; fi
		exit 0
		;;
	esac
	echo "the fake docker client does not know volume $verb" >&2
	exit 2
	;;
run)
	name=""
	secret=""
	previous=""
	for a in "$@"; do
		if [ "$previous" = "--name" ]; then name="$a"; fi
		case "$a" in
		SANDBOX_SECRET=*) secret="${a#SANDBOX_SECRET=}" ;;
		esac
		previous="$a"
	done
	printf 'container-id-123|running|2026-10-16T09:00:00Z|127.0.0.1|%s\n' "$secret" > "@STATE@/$name"
	echo container-id-123
	exit 0
	;;
start)
	name="$2"
	if [ "$(record "$name")" = absent ]; then
		echo "Error: No such container: $name" >&2
		exit 1
	fi
	set_state "$name" running
	echo "$name"
	exit 0
	;;
stop)
	name="$2"
	if [ "$(record "$name")" = absent ]; then
		echo "Error: No such container: $name" >&2
		exit 1
	fi
	set_state "$name" exited
	echo "$name"
	exit 0
	;;
inspect)
	format="$3"
	name="$4"
	if [ "$(record "$name")" = absent ]; then
		echo "Error: No such object: $name" >&2
		exit 1
	fi
	case "$format" in
	*IPAddress*)
		printf '%s|%s\n' "$(field "$(record "$name")" 2)" "$(field "$(record "$name")" 4)"
		;;
	*Config.Env*)
		printf '["PATH=/usr/local/bin","SANDBOX_SECRET=%s","SCREEN_GEOMETRY=1280x800x24"]\n' "$(field "$(record "$name")" 5)"
		;;
	*)
		printf '%s|%s|%s\n' "$(field "$(record "$name")" 1)" "$(field "$(record "$name")" 2)" "$(field "$(record "$name")" 3)"
		;;
	esac
	exit 0
	;;
ps)
	for file in "@STATE@"/*; do
		if [ -f "$file" ]; then
			printf '%s|%s\n' "${file##*/}" "$(field "$(cat "$file")" 2)"
		fi
	done
	exit 0
	;;
rm)
	name=$(last_arg "$@")
	force=no
	if [ "$2" = "-f" ]; then force=yes; fi
	if refuse "$name"; then
		echo "Error response from daemon: cannot remove container \"$name\": the container is wedged" >&2
		exit 1
	fi
	if [ "$(record "$name")" = absent ]; then
		echo "Error: No such container: $name" >&2
		exit 1
	fi
	if [ "$force" = no ] && [ "$(field "$(record "$name")" 2)" = running ]; then
		echo "Error response from daemon: You cannot remove a running container $name. Stop the container before attempting removal or force remove" >&2
		exit 1
	fi
	rm -f "@STATE@/$name"
	echo "$name"
	exit 0
	;;
exec)
	name=""
	for a in "$@"; do
		if [ -f "@STATE@/$a" ]; then name="$a"; fi
	done
	if [ "$(record "$name")" = absent ]; then
		echo "Error: No such container: $name" >&2
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

// container puts a container in the fake daemon's state, the way a `docker run` or an
// earlier control plane would have left it.
func (f *fakeDocker) container(t *testing.T, name, state string) {
	t.Helper()

	record := strings.Join([]string{
		"container-id-" + name,
		state,
		"2026-10-16T09:00:00Z",
		"127.0.0.1",
		strings.Repeat("a", 32),
	}, "|")
	if err := os.WriteFile(filepath.Join(f.stateDir, name), []byte(record+"\n"), 0o644); err != nil {
		t.Fatalf("record the fake container %s: %v", name, err)
	}
}

// volume puts a volume in the fake daemon's state.
func (f *fakeDocker) volume(t *testing.T, name string) {
	t.Helper()

	file, err := os.OpenFile(f.volumesPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("record the fake volume %s: %v", name, err)
	}
	defer func() { _ = file.Close() }()
	if _, err := fmt.Fprintln(file, name); err != nil {
		t.Fatalf("record the fake volume %s: %v", name, err)
	}
}

// refuseRemoval makes the fake daemon refuse to remove these objects, which is what a
// volume still mounted by a running container or a wedged container looks like.
func (f *fakeDocker) refuseRemoval(t *testing.T, names ...string) {
	t.Helper()

	if err := os.WriteFile(f.refusePath, []byte(strings.Join(names, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("record the refused removals: %v", err)
	}
}

// stateOf answers a container's state in the fake daemon, or "" when it holds no such
// container.
func (f *fakeDocker) stateOf(t *testing.T, name string) string {
	t.Helper()

	record, err := os.ReadFile(filepath.Join(f.stateDir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read the fake container %s: %v", name, err)
	}
	fields := strings.Split(strings.TrimSpace(string(record)), "|")
	if len(fields) < 2 {
		t.Fatalf("the fake container %s holds %q", name, record)
	}
	return fields[1]
}

// volumes answers the volumes the fake daemon still holds, in the order it holds them.
func (f *fakeDocker) volumes(t *testing.T) []string {
	t.Helper()

	payload, err := os.ReadFile(f.volumesPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read the fake volumes: %v", err)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimRight(string(payload), "\n"), "\n") {
		if line != "" {
			names = append(names, line)
		}
	}
	return names
}

// cdpRequest is one request the fake gate received, split the way the runtime builds
// it: what it was, where it went, and the secret it carried.
type cdpRequest struct {
	method string
	path   string
	query  string
	secret string
}

// loadScript is what the fake page reports when the runtime evaluates the load probe.
// A test scripts these, which is what lets it say "this page loaded", "this page landed
// on the browser's own error page", or "this page never finishes".
type loadScript struct {
	state string
	href  string
	title string
}

// probe is the JSON string a DevTools answer carries for this script. The runtime
// parses it as the document's readyState, its address, and its title.
func (s loadScript) probe() string {
	return fmt.Sprintf(`{"state":%q,"href":%q,"title":%q}`, s.state, s.href, s.title)
}

// The frame opcodes the DevTools connection uses, and the constant RFC 6455 appends to
// the handshake key.
const (
	wsText  = 0x1
	wsClose = 0x8
	wsPing  = 0x9
	wsPong  = 0xa
	wsGUID  = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

// fakeCDP is the stand-in for a sandbox's gate: the only listener on the sandbox
// network, which is why it, and not Chromium, is what the runtime talks to.
//
// It is a real HTTP server and a real websocket server, and the two halves matter
// separately. The HTTP half is Chromium's DevTools endpoint as the gate publishes it,
// under /cdp. The websocket half is the target's debugger: Chromium answers a
// webSocketDebuggerUrl pointing at its own loopback address, which the control plane
// cannot dial, so the runtime has to rewrite the address to the gate's and keep the
// path. The fake answers on that rewritten path, and only on that one.
type fakeCDP struct {
	server *httptest.Server
	port   string

	mu        sync.Mutex
	requests  []cdpRequest
	devtools  []string
	targets   []cdpTarget
	closed    []string
	loads     []loadScript
	evaluated int
	nextID    int
	conns     []net.Conn
}

// newFakeCDP serves the gate endpoints the runtime uses.
func newFakeCDP(t *testing.T) *fakeCDP {
	t.Helper()

	gate := &fakeCDP{
		// A session that already has a tab open, and the browser target that is not a
		// page: navigating must close the first and never the second.
		targets: []cdpTarget{
			{ID: "page-old", Type: "page", WSURL: "ws://127.0.0.1:9222/devtools/page/page-old"},
			{ID: "browser", Type: "browser", WSURL: "ws://127.0.0.1:9222/devtools/browser/browser-1"},
		},
		loads: []loadScript{{state: "complete", href: "http://vcomputer-fixture:8080/", title: "vcomputer"}},
	}
	gate.server = httptest.NewServer(http.HandlerFunc(gate.serve))
	t.Cleanup(gate.server.Close)
	// The websockets are closed before the server is: a handler still blocked in a read
	// would otherwise hold the server's shutdown open.
	t.Cleanup(gate.closeConnections)

	parsed, err := url.Parse(gate.server.URL)
	if err != nil {
		t.Fatalf("parse the fake gate url: %v", err)
	}
	gate.port = parsed.Port()
	return gate
}

// serve is the gate: it authenticates the request, then routes it to the endpoint the
// path names.
func (c *fakeCDP) serve(w http.ResponseWriter, r *http.Request) {
	secret := r.Header.Get(SecretHeader)
	c.record(cdpRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, secret: secret})

	// The gate is the only thing listening on a sandbox's network address, and the
	// secret is the only thing that says a request came from the control plane that
	// started the sandbox: an unauthenticated request must not reach Chromium.
	if secret == "" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("the sandbox secret is missing\n"))
		return
	}

	switch {
	case strings.HasPrefix(r.URL.Path, "/cdp/devtools/"):
		c.serveDevTools(w, r)
	case r.URL.Path == "/cdp/json/version":
		c.answerJSON(w, map[string]string{
			"Browser":              "Chrome/140.0.0.0",
			"webSocketDebuggerUrl": "ws://127.0.0.1:9222/devtools/browser/browser-1",
		})
	case r.URL.Path == "/cdp/json/new":
		c.openTarget(w)
	case r.URL.Path == "/cdp/json/list":
		c.answerJSON(w, c.targetList())
	case strings.HasPrefix(r.URL.Path, "/cdp/json/close/"):
		c.closeTarget(w, strings.TrimPrefix(r.URL.Path, "/cdp/json/close/"))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// openTarget answers /json/new with the target Chromium would have created, including
// the loopback debugger address the control plane must not dial: the path it carries is
// what the runtime keeps when it rewrites the address to the gate's.
func (c *fakeCDP) openTarget(w http.ResponseWriter) {
	c.mu.Lock()
	c.nextID++
	id := fmt.Sprintf("page-%d", c.nextID)
	opened := cdpTarget{ID: id, Type: "page", WSURL: "ws://127.0.0.1:9222/devtools/page/" + id}
	c.targets = append(c.targets, opened)
	c.mu.Unlock()

	c.answerJSON(w, opened)
}

// closeTarget answers /json/close/<id> and forgets the target, so what the fake holds
// afterwards is what the session really has open.
func (c *fakeCDP) closeTarget(w http.ResponseWriter, id string) {
	c.mu.Lock()
	remaining := make([]cdpTarget, 0, len(c.targets))
	for _, target := range c.targets {
		if target.ID == id {
			c.closed = append(c.closed, id)
			continue
		}
		remaining = append(remaining, target)
	}
	c.targets = remaining
	c.mu.Unlock()

	_, _ = w.Write([]byte("Target is closing"))
}

// answerJSON writes one JSON body.
func (c *fakeCDP) answerJSON(w http.ResponseWriter, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// serveDevTools completes the RFC 6455 handshake and answers the DevTools commands the
// runtime sends over the connection. A server writes unmasked frames; the client's
// frames are masked, and the mask is removed here.
func (c *fakeCDP) serveDevTools(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	conn, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	c.track(conn)

	accept := sha1.Sum([]byte(key + wsGUID))
	_, _ = fmt.Fprintf(buffered,
		"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
		base64.StdEncoding.EncodeToString(accept[:]))
	if err := buffered.Flush(); err != nil {
		return
	}

	for {
		opcode, payload, err := readClientFrame(buffered)
		if err != nil {
			return
		}
		switch opcode {
		case wsText:
			if err := c.answerCommand(buffered, payload); err != nil {
				return
			}
		case wsPing:
			if err := writeServerFrame(buffered, wsPong, payload); err != nil {
				return
			}
		case wsClose:
			return
		}
	}
}

// answerCommand answers one DevTools command. The load probe is answered from the
// script the test set, so what the runtime is told about the page is the test's choice.
func (c *fakeCDP) answerCommand(buffered *bufio.ReadWriter, payload []byte) error {
	var command struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal(payload, &command); err != nil {
		// A message the fake cannot read is one it cannot correlate, which is what the
		// runtime's own client does with a message it cannot read: skip it.
		return nil
	}
	c.recordCommand(command.Method)

	result := any(map[string]any{})
	if command.Method == "Runtime.evaluate" {
		result = map[string]any{
			"result": map[string]any{"type": "string", "value": c.nextLoad().probe()},
		}
	}
	message, err := json.Marshal(map[string]any{"id": command.ID, "result": result})
	if err != nil {
		return err
	}
	return writeServerFrame(buffered, wsText, message)
}

// readClientFrame reads one frame the client sent. A client must mask its frames,
// which is the one rule that differs from the client in ws.go: the payload is unmasked
// here, and an unmasked frame is a protocol violation the server has to refuse.
func readClientFrame(buffered *bufio.ReadWriter) (opcode byte, payload []byte, err error) {
	var header [2]byte
	if _, err := io.ReadFull(buffered, header[:]); err != nil {
		return 0, nil, err
	}
	opcode = header[0] & 0x0f
	masked := header[1]&0x80 != 0

	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		var extended [2]byte
		if _, err := io.ReadFull(buffered, extended[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(extended[:]))
	case 127:
		var extended [8]byte
		if _, err := io.ReadFull(buffered, extended[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(extended[:])
	}
	if !masked {
		return 0, nil, errors.New("the client sent an unmasked frame")
	}

	var mask [4]byte
	if _, err := io.ReadFull(buffered, mask[:]); err != nil {
		return 0, nil, err
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(buffered, payload); err != nil {
		return 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return opcode, payload, nil
}

// writeServerFrame sends one unmasked frame, which is what a server is required to
// send: masking is one-sided, and a client closes the connection over a masked frame
// from a server.
func writeServerFrame(buffered *bufio.ReadWriter, opcode byte, payload []byte) error {
	frame := []byte{0x80 | opcode}
	switch size := len(payload); {
	case size < 126:
		frame = append(frame, byte(size))
	case size <= 0xffff:
		frame = append(frame, 126, byte(size>>8), byte(size))
	default:
		frame = append(frame, 127)
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(size))
		frame = append(frame, length[:]...)
	}
	frame = append(frame, payload...)
	if _, err := buffered.Write(frame); err != nil {
		return err
	}
	return buffered.Flush()
}

// setLoads scripts what the page answers, one entry per evaluation. The last entry
// answers every further evaluation, so a test can say "this page never finishes".
func (c *fakeCDP) setLoads(loads ...loadScript) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loads = loads
	c.evaluated = 0
}

// nextLoad answers the script for one evaluation.
func (c *fakeCDP) nextLoad() loadScript {
	c.mu.Lock()
	defer c.mu.Unlock()

	state := c.loads[c.evaluated]
	if c.evaluated < len(c.loads)-1 {
		c.evaluated++
	}
	return state
}

func (c *fakeCDP) record(request cdpRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, request)
}

func (c *fakeCDP) recordCommand(method string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.devtools = append(c.devtools, method)
}

func (c *fakeCDP) track(conn net.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conns = append(c.conns, conn)
}

// closeConnections ends every debugger connection still open, so the server can shut
// down even if a handler is blocked in a read.
func (c *fakeCDP) closeConnections() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, conn := range c.conns {
		_ = conn.Close()
	}
	c.conns = nil
}

// seen returns the requests the fake gate received, in order.
func (c *fakeCDP) seen() []cdpRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]cdpRequest(nil), c.requests...)
}

// onlySecret returns the secret every request carried, and fails the test when the
// gate saw one without a secret or saw more than one value: the runtime holds exactly
// one secret for one sandbox, and it attaches it to every request.
//
// Call it before asking the gate anything without the secret on purpose.
func (c *fakeCDP) onlySecret(t *testing.T) string {
	t.Helper()

	secret := ""
	for _, request := range c.seen() {
		if request.secret == "" {
			t.Fatalf("the gate received %s %s without a secret", request.method, request.path)
		}
		if secret == "" {
			secret = request.secret
		} else if secret != request.secret {
			t.Fatalf("the gate received two secrets: %q and %q", secret, request.secret)
		}
	}
	if secret == "" {
		t.Fatal("the gate received no request at all")
	}
	return secret
}

// commands returns the DevTools commands the fake page received, in order.
func (c *fakeCDP) commands() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.devtools...)
}

// websocketPaths returns the debugger paths the runtime dialed, which is where the
// rewrite shows: they are the gate's paths, never the loopback address Chromium
// published.
func (c *fakeCDP) websocketPaths() []string {
	var paths []string
	for _, request := range c.seen() {
		if strings.HasPrefix(request.path, "/cdp/devtools/") {
			paths = append(paths, request.path)
		}
	}
	return paths
}

// pages returns the page targets the fake browser still holds: one entry per tab.
func (c *fakeCDP) pages() []string {
	var ids []string
	for _, target := range c.targetList() {
		if target.Type == "page" {
			ids = append(ids, target.ID)
		}
	}
	return ids
}

// closedTargets returns the target ids the runtime closed, in order.
func (c *fakeCDP) closedTargets() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.closed...)
}

// targetList returns what /json/list would answer.
func (c *fakeCDP) targetList() []cdpTarget {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]cdpTarget(nil), c.targets...)
}

// dockerFixture is the runtime under test plus the two fakes behind it.
type dockerFixture struct {
	runtime *DockerRuntime
	docker  *fakeDocker
	cdp     *fakeCDP
}

// newDockerFixture builds a runtime pointed at the fakes. The sandbox's gate port is
// the fake gate's port, and the fake container's address is the loopback one the gate
// listens on, so every request the runtime makes lands in the fake's log.
func newDockerFixture(t *testing.T, options ...DockerOption) *dockerFixture {
	t.Helper()

	fake := newFakeDocker(t)
	gate := newFakeCDP(t)

	base := []DockerOption{
		WithDockerBin(fake.bin),
		WithSandboxImage("vcomputer-sandbox:poc"),
		WithSandboxNetwork("vcomputer-internal"),
		WithSandboxPorts(DefaultNOVNCPort, gate.port),
		WithWaitTimeout(10 * time.Second),
	}
	return &dockerFixture{
		runtime: NewDocker(append(base, options...)...),
		docker:  fake,
		cdp:     gate,
	}
}

// TestDockerEnsureCreatesAHardenedContainerAndWaitsForTheBrowser covers the whole
// start path: the volume, the container, the isolation flags, the secret the sandbox's
// gate will ask for, and the wait until the browser inside it answers.
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
	// CDP is a path on the control plane, not an address on the sandbox network: the
	// control plane holds the sandbox secret, so a caller drives the browser through it
	// with the control token and nothing else.
	if want := CDPPath("bot-a"); session.CDPURL != want {
		t.Fatalf("cdp url = %q, want %q", session.CDPURL, want)
	}
	if want := NoVNCPath("bot-a"); session.NoVNCURL != want {
		t.Fatalf("novnc url = %q, want %q", session.NoVNCURL, want)
	}

	calls := fixture.docker.calls(t)
	volume := findCall(t, calls, "volume", "create")
	// The label is what a cleanup finds the volume by later: a name pattern is a
	// convention, a label is a record.
	if want := []string{"volume", "create", "--label", "vcomputer.bot=bot-a", "vcomputer-bot-a-home"}; !equalArgv(volume, want) {
		t.Fatalf("volume call = %v, want %v", volume, want)
	}

	run := findCall(t, calls, "run")
	if run == nil {
		t.Fatalf("no run call: %v", calls)
	}
	// The isolation flags and the configuration the sandbox reads out of its own
	// environment are this runtime's contract, so each one is named.
	for _, required := range [][2]string{
		{"--name", "vcomputer-bot-a"},
		{"--label", "vcomputer.bot=bot-a"},
		{"--network", "vcomputer-internal"},
		{"--volume", "vcomputer-bot-a-home:/home/bolu"},
		{"--env", "NOVNC_PORT=" + DefaultNOVNCPort},
		{"--env", "GATE_PORT=" + fixture.cdp.port},
		{"--env", "SCREEN_GEOMETRY=1280x800x24"},
		{"--security-opt", "no-new-privileges:true"},
		{"--cap-drop", "ALL"},
		{"--memory", "2g"},
		{"--pids-limit", "512"},
	} {
		if !containsPair(run, required[0], required[1]) {
			t.Fatalf("run call %v is missing %v", run, required)
		}
	}
	// The secret is the credential for this one sandbox: 16 bytes of randomness, which
	// is 32 hex characters, and it travels in the environment rather than in the argv,
	// where `ps` would show it to anything on the host.
	if secret := envValue(run, secretEnv); !isSecret(secret) {
		t.Fatalf("the sandbox was started with secret %q, want 32 hex characters", secret)
	}
	if last := run[len(run)-1]; last != "vcomputer-sandbox:poc" {
		t.Fatalf("run call %v does not end with the image", run)
	}

	// No published port, and no CDP port either: the gate is the only listener on the
	// sandbox's address, and the control plane reaches it over the sandbox network.
	for _, call := range calls {
		for _, arg := range call {
			switch arg {
			case "-p", "--publish", "-P", "--expose":
				t.Fatalf("call %v publishes a port", call)
			}
			if strings.Contains(arg, "CDP_PORT") {
				t.Fatalf("call %v still configures a CDP port", call)
			}
		}
	}

	// The wait is real and it goes through the gate: the first thing the runtime asks
	// is Chromium's version, on the gate's address, with the sandbox secret attached.
	seen := fixture.cdp.seen()
	if len(seen) == 0 || seen[0].method != http.MethodGet || seen[0].path != "/cdp/json/version" {
		t.Fatalf("gate requests = %v, want a readiness probe first", seen)
	}
	if attached := fixture.cdp.onlySecret(t); attached != envValue(run, secretEnv) {
		t.Fatalf("the readiness probe carried secret %q, want the one the sandbox was started with", attached)
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
	// A stopped session has no browser to drive, so it answers no path to one: a path
	// that cannot work would mislead the caller.
	if stopped.CDPURL != "" {
		t.Fatalf("cdp url of a stopped session = %q, want it empty", stopped.CDPURL)
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

// TestDockerNavigateAnswersWhatThePageLoaded covers what makes Navigate more than a
// request: the page is driven over the DevTools websocket through the gate, and what it
// reports is what the caller is told. The mismatch is the interesting case — the caller
// asked for one URL and the page settled on another — because the answer has to be the
// page's own address rather than an echo of the request.
func TestDockerNavigateAnswersWhatThePageLoaded(t *testing.T) {
	fixture := newDockerFixture(t)

	if _, err := fixture.runtime.Ensure(t.Context(), "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	requested := "http://vcomputer-fixture:8080/?q=halo timbolu&page=1"
	landed := "http://vcomputer-fixture:8080/landing?from=redirect&q=halo%20timbolu"
	// The first evaluation is what a target reports before its navigation commits, and
	// the second is the page that finished: the runtime has to keep asking rather than
	// take the first answer.
	fixture.cdp.setLoads(
		loadScript{state: "loading", href: blankPage},
		loadScript{state: "complete", href: landed, title: "Landing page"},
	)

	navigation, err := fixture.runtime.Navigate(t.Context(), "bot-a", requested)
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if navigation.FinalURL != landed {
		t.Fatalf("final url = %q, want the page's own address %q", navigation.FinalURL, landed)
	}
	if navigation.Title != "Landing page" {
		t.Fatalf("title = %q", navigation.Title)
	}

	// The verification is a real DevTools conversation: Page.enable first, then the load
	// probe, repeated until the page reports that it finished.
	commands := fixture.cdp.commands()
	if len(commands) < 3 || commands[0] != "Page.enable" || commands[1] != "Runtime.evaluate" || commands[2] != "Runtime.evaluate" {
		t.Fatalf("devtools commands = %v, want Page.enable and then the load probe", commands)
	}

	// And it happened over the gate: the debugger address Chromium published is its own
	// loopback one, and the runtime kept the target's path while replacing the address.
	pages := fixture.cdp.pages()
	if len(pages) != 1 {
		t.Fatalf("tabs = %v, want the one the navigation opened", pages)
	}
	paths := fixture.cdp.websocketPaths()
	if len(paths) != 1 || paths[0] != "/cdp/devtools/page/"+pages[0] {
		t.Fatalf("debugger paths = %v, want the target's path on the gate", paths)
	}

	// The URL the caller gave is the URL the browser was handed, space and all: the gate
	// unescapes the query back into the address to open.
	opened := findRequest(t, fixture.cdp.seen(), "/cdp/json/new")
	decoded, err := url.QueryUnescape(opened.query)
	if err != nil {
		t.Fatalf("the navigation query is not decodable: %v", err)
	}
	if decoded != requested {
		t.Fatalf("the browser was asked to open %q, want %q", decoded, requested)
	}

	if secret := fixture.cdp.onlySecret(t); !isSecret(secret) {
		t.Fatalf("the gate was asked with secret %q, want the sandbox's own", secret)
	}
}

// TestDockerNavigateReportsTheBrowsersOwnErrorPage is the failure a sandbox with no
// route out produces for every external address: Chromium's HTTP endpoint answers
// "opened", and the document that finished is the browser's error page. Reporting that
// as a navigation would be a lie, so it is a *NavigationError carrying where the
// browser actually landed.
func TestDockerNavigateReportsTheBrowsersOwnErrorPage(t *testing.T) {
	fixture := newDockerFixture(t)

	if _, err := fixture.runtime.Ensure(t.Context(), "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	requested := "https://example.com/"
	fixture.cdp.setLoads(loadScript{
		state: "complete",
		href:  "chrome-error://chromewebdata/",
		title: "example.com",
	})

	_, err := fixture.runtime.Navigate(t.Context(), "bot-a", requested)
	var failure *NavigationError
	if !errors.As(err, &failure) {
		t.Fatalf("err = %v, want a *NavigationError", err)
	}
	if failure.URL != requested {
		t.Fatalf("failed url = %q, want the one that was asked for %q", failure.URL, requested)
	}
	if failure.FinalURL != "chrome-error://chromewebdata/" {
		t.Fatalf("final url = %q, want the browser's error page", failure.FinalURL)
	}
	if failure.Reason == "" {
		t.Fatal("the failure carries no reason")
	}
}

// TestDockerNavigateGivesUpOnAPageThatNeverFinishes keeps a navigation that will never
// succeed from holding the caller: a page that has not reported "complete" inside the
// navigate timeout is a page nobody may be told loaded.
func TestDockerNavigateGivesUpOnAPageThatNeverFinishes(t *testing.T) {
	fixture := newDockerFixture(t, WithNavigateTimeout(300*time.Millisecond))

	if _, err := fixture.runtime.Ensure(t.Context(), "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	requested := "http://vcomputer-fixture:8080/slow"
	fixture.cdp.setLoads(loadScript{state: "loading", href: requested, title: "slow"})

	started := time.Now()
	_, err := fixture.runtime.Navigate(t.Context(), "bot-a", requested)
	var failure *NavigationError
	if !errors.As(err, &failure) {
		t.Fatalf("err = %v, want a *NavigationError", err)
	}
	if failure.FinalURL != requested {
		t.Fatalf("final url = %q, want where the page got to %q", failure.FinalURL, requested)
	}
	if !strings.Contains(failure.Reason, "did not finish loading") {
		t.Fatalf("reason = %q, want the timeout named", failure.Reason)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("navigate took %s, want it cut off at the navigate timeout", elapsed)
	}
}

// TestDockerNavigateLeavesOneTabBehind covers the browser action: a target opened for
// the URL, every other page target closed, and the one the session now has left open.
func TestDockerNavigateLeavesOneTabBehind(t *testing.T) {
	fixture := newDockerFixture(t)

	if _, err := fixture.runtime.Ensure(t.Context(), "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	// A URL with a space and a query, which is what a search or a fixture form produces.
	target := "http://vcomputer-fixture:8080/?q=halo timbolu&page=1"
	if _, err := fixture.runtime.Navigate(t.Context(), "bot-a", target); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// Everything after the readiness probe the start made: the open, the list, the close
	// of the tab the session had before, and the debugger connection to the new one.
	seen := fixture.cdp.seen()[1:]
	want := []cdpRequest{
		{method: http.MethodPut, path: "/cdp/json/new"},
		{method: http.MethodGet, path: "/cdp/json/list"},
		{method: http.MethodGet, path: "/cdp/json/close/page-old"},
	}
	if len(seen) != len(want)+1 {
		t.Fatalf("gate requests = %v, want the open, the list, the close, and the debugger", seen)
	}
	for i, request := range want {
		if got := seen[i]; got.method != request.method || got.path != request.path {
			t.Fatalf("request %d = %v, want %v", i, got, request)
		}
	}
	if path := seen[len(want)].path; !strings.HasPrefix(path, "/cdp/devtools/") {
		t.Fatalf("the debugger was reached at %q, want a path on the gate", path)
	}

	// What the session is left holding is one tab, and the browser target is not a tab:
	// closing it would end the session's browser rather than its page.
	if pages := fixture.cdp.pages(); len(pages) != 1 {
		t.Fatalf("tabs after the navigation = %v, want one", pages)
	}
	if closed := fixture.cdp.closedTargets(); !equalArgv(closed, []string{"page-old"}) {
		t.Fatalf("closed targets = %v, want the session's previous tab only", closed)
	}
	if secret := fixture.cdp.onlySecret(t); !isSecret(secret) {
		t.Fatalf("the gate was asked with secret %q, want the sandbox's own", secret)
	}

	// A URL the browser should not be handed is refused before any request is made.
	before := len(fixture.cdp.seen())
	if _, err := fixture.runtime.Navigate(t.Context(), "bot-a", "file:///home/bolu/.bash_history"); !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("err = %v, want ErrInvalidURL", err)
	}
	if after := len(fixture.cdp.seen()); after != before {
		t.Fatalf("a refused navigation reached the gate: %v", fixture.cdp.seen()[before:])
	}
}

// TestDockerGateTargetCarriesTheSecretTheContainerHolds covers what the control plane
// proxies to. The secret is generated when the sandbox is created, it lives in that
// container's environment, and it is recovered from there rather than remembered: the
// control plane holds no per-session credential across a restart.
func TestDockerGateTargetCarriesTheSecretTheContainerHolds(t *testing.T) {
	fixture := newDockerFixture(t)

	// A container that is not there is not a gate: a bot with no session answers 404
	// rather than an address that cannot work.
	if _, err := fixture.runtime.GateTarget(t.Context(), "bot-a"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("target without a container: err = %v, want ErrNoSession", err)
	}
	if _, err := fixture.runtime.Ensure(t.Context(), "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	target, err := fixture.runtime.GateTarget(t.Context(), "bot-a")
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	if want := "http://127.0.0.1:" + fixture.cdp.port; target.URL != want {
		t.Fatalf("target url = %q, want the container's address on the gate port %q", target.URL, want)
	}
	if !isSecret(target.Secret) {
		t.Fatalf("secret = %q, want 32 hex characters", target.Secret)
	}

	// The secret the container was started with is the one it reports, and the one the
	// readiness probe carried: one credential, recovered from the container itself.
	run := findCall(t, fixture.docker.calls(t), "run")
	if started := envValue(run, secretEnv); started != target.Secret {
		t.Fatalf("the container was started with secret %q and reports %q", started, target.Secret)
	}
	if attached := fixture.cdp.onlySecret(t); attached != target.Secret {
		t.Fatalf("the gate was asked with secret %q, want %q", attached, target.Secret)
	}

	// The gate is what asks for it: the same endpoint without the secret is refused, and
	// with it, answered.
	if response := gateGet(t, target.URL+"/cdp/json/version", ""); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a request without the secret answered %s, want 401", response.Status)
	}
	if response := gateGet(t, target.URL+"/cdp/json/version", target.Secret); response.StatusCode != http.StatusOK {
		t.Fatalf("a request with the secret answered %s, want 200", response.Status)
	}
}

// TestDockerPurgeRemovesTheContainerAndTheVolume is the difference between Stop and
// Purge: the machine is gone, and so is the profile it left behind.
func TestDockerPurgeRemovesTheContainerAndTheVolume(t *testing.T) {
	fixture := newDockerFixture(t)

	if _, err := fixture.runtime.Ensure(t.Context(), "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	// The volume Ensure created is one the fake daemon now holds, so a purge has one to
	// remove; a purge that removed nothing would be a purge of nothing.
	if volumes := fixture.docker.volumes(t); len(volumes) != 1 {
		t.Fatalf("volumes after ensure = %v, want the session's one", volumes)
	}
	fixture.docker.forget(t)

	if err := fixture.runtime.Purge(t.Context(), "bot-a"); err != nil {
		t.Fatalf("purge: %v", err)
	}

	calls := fixture.docker.calls(t)
	if got := findCall(t, calls, "rm"); !equalArgv(got, []string{"rm", "-f", "vcomputer-bot-a"}) {
		t.Fatalf("container removal = %v, want a forced removal of the session", got)
	}
	if got := findCall(t, calls, "volume", "rm"); !equalArgv(got, []string{"volume", "rm", "vcomputer-bot-a-home"}) {
		t.Fatalf("volume removal = %v, want the session's volume", got)
	}

	if state := fixture.docker.stateOf(t, "vcomputer-bot-a"); state != "" {
		t.Fatalf("the container survived the purge: %q", state)
	}
	if volumes := fixture.docker.volumes(t); len(volumes) != 0 {
		t.Fatalf("volumes after the purge = %v, want none", volumes)
	}
	if _, err := fixture.runtime.Status(t.Context(), "bot-a"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("status after the purge: err = %v, want ErrNoSession", err)
	}
}

// TestDockerPurgeWithoutASessionIsNoSession is what makes a retried delete answer 404:
// a bot with neither container nor volume is not a purge that removed something.
func TestDockerPurgeWithoutASessionIsNoSession(t *testing.T) {
	fixture := newDockerFixture(t)

	if err := fixture.runtime.Purge(t.Context(), "bot-a"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("purge: err = %v, want ErrNoSession", err)
	}

	// The fake answered the two failures a purge has to tell apart from a real one, so
	// the answer above came from the daemon's own wording rather than from a guess.
	calls := fixture.docker.calls(t)
	if findCall(t, calls, "rm") == nil || findCall(t, calls, "volume", "rm") == nil {
		t.Fatalf("a purge of a missing session did not ask: %v", calls)
	}
}

// TestDockerCleanupRemovesStoppedContainersAndNeverARunningOne is the promise the
// scoping exists for: a cleanup removes what a crashed control plane left behind, and a
// running sandbox — a session somebody is using — is not that.
func TestDockerCleanupRemovesStoppedContainersAndNeverARunningOne(t *testing.T) {
	fixture := newDockerFixture(t)
	fixture.docker.container(t, "vcomputer-bot-running", "running")
	fixture.docker.container(t, "vcomputer-bot-stopped", "exited")
	fixture.docker.container(t, "vcomputer-bot-wedged", "exited")
	fixture.docker.refuseRemoval(t, "vcomputer-bot-wedged")

	result, err := fixture.runtime.Cleanup(t.Context(), false)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if result.RemovedContainers != 1 {
		t.Fatalf("removed %d containers, want the one stopped one: %+v", result.RemovedContainers, result)
	}
	// "Nothing was removed" and "nothing could be removed" are different answers to a
	// caller that just asked for a cleanup, so the one the daemon refused is reported.
	if len(result.Failed) != 1 || !strings.Contains(result.Failed[0], "wedged") {
		t.Fatalf("failed = %v, want the one line the daemon answered for the wedged container", result.Failed)
	}

	if state := fixture.docker.stateOf(t, "vcomputer-bot-running"); state != "running" {
		t.Fatalf("the running container is %q, want it untouched", state)
	}
	if state := fixture.docker.stateOf(t, "vcomputer-bot-stopped"); state != "" {
		t.Fatalf("the stopped container is still there: %q", state)
	}

	// The listing is scoped to what this runtime owns — the label every sandbox carries,
	// and the name prefix that keeps two deployments on one host apart — and a cleanup
	// that was not asked for volumes never touches one.
	listing := findCall(t, fixture.docker.calls(t), "ps")
	for _, filter := range [][2]string{{"--filter", "label=vcomputer.bot"}, {"--filter", "name=^vcomputer-"}} {
		if !containsPair(listing, filter[0], filter[1]) {
			t.Fatalf("listing %v is missing %v", listing, filter)
		}
	}
	for _, call := range fixture.docker.calls(t) {
		if call[0] == "volume" {
			t.Fatalf("a cleanup without volumes touched one: %v", call)
		}
		for _, arg := range call {
			if arg == "vcomputer-bot-running" {
				t.Fatalf("the running container was named: %v", call)
			}
		}
	}
}

// TestDockerCleanupRemovesVolumesAndReportsWhatItCouldNot covers the other half of a
// cleanup, and the honest answer for a volume it cannot remove: a volume still mounted
// by a running container is the realistic case.
func TestDockerCleanupRemovesVolumesAndReportsWhatItCouldNot(t *testing.T) {
	fixture := newDockerFixture(t)
	fixture.docker.container(t, "vcomputer-bot-running", "running")
	fixture.docker.volume(t, "vcomputer-bot-running-home")
	fixture.docker.volume(t, "vcomputer-bot-gone-home")
	fixture.docker.refuseRemoval(t, "vcomputer-bot-running-home")

	result, err := fixture.runtime.Cleanup(t.Context(), true)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if result.RemovedContainers != 0 {
		t.Fatalf("removed %d containers, want none: the only one is running", result.RemovedContainers)
	}
	if result.RemovedVolumes != 1 {
		t.Fatalf("removed %d volumes, want the one nothing is holding: %+v", result.RemovedVolumes, result)
	}
	if len(result.Failed) != 1 || !strings.Contains(result.Failed[0], "in use") {
		t.Fatalf("failed = %v, want the one line the daemon answered for the volume in use", result.Failed)
	}

	if volumes := fixture.docker.volumes(t); !equalArgv(volumes, []string{"vcomputer-bot-running-home"}) {
		t.Fatalf("volumes = %v, want the one that could not be removed", volumes)
	}

	listing := findCall(t, fixture.docker.calls(t), "volume", "ls")
	for _, filter := range [][2]string{{"--filter", "label=vcomputer.bot"}, {"--filter", "name=^vcomputer-"}} {
		if !containsPair(listing, filter[0], filter[1]) {
			t.Fatalf("listing %v is missing %v", listing, filter)
		}
	}
}

// TestDockerListingsPutTheirFiltersAfterTheVerb is the argv contract with the docker
// client. The scoping flags go between the command and the listing's own arguments,
// and the command is not always one word: `docker ps --filter ... -a` and
// `docker volume ls --filter ...` are the two shapes this runtime asks for, while
// `docker volume --filter ... ls` is not a command docker has — it answers
// "unknown flag: --filter", which is the 500 a cleanup used to answer with.
func TestDockerListingsPutTheirFiltersAfterTheVerb(t *testing.T) {
	fixture := newDockerFixture(t)
	fixture.docker.container(t, "vcomputer-bot-a", "exited")
	fixture.docker.volume(t, "vcomputer-bot-a-home")

	if _, err := fixture.runtime.Cleanup(t.Context(), true); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	calls := fixture.docker.calls(t)
	if got := findCall(t, calls, "ps"); !equalArgv(got, []string{
		"ps",
		"--filter", "label=vcomputer.bot",
		"--filter", "name=^vcomputer-",
		"-a", "--format", "{{.Names}}|{{.State}}",
	}) {
		t.Fatalf("container listing = %v", got)
	}
	if got := findCall(t, calls, "volume", "ls"); !equalArgv(got, []string{
		"volume", "ls",
		"--filter", "label=vcomputer.bot",
		"--filter", "name=^vcomputer-",
		"--format", "{{.Name}}",
	}) {
		t.Fatalf("volume listing = %v", got)
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
	if _, err := fixture.runtime.Navigate(ctx, "bot-a", "https://example.com"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("navigate: err = %v, want ErrNoSession", err)
	}
	if _, err := fixture.runtime.GateTarget(ctx, "bot-a"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("gate target: err = %v, want ErrNoSession", err)
	}
	if err := fixture.runtime.Purge(ctx, "bot-a"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("purge: err = %v, want ErrNoSession", err)
	}
	if _, err := fixture.runtime.Exec(ctx, "bot-a", "ls"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("exec: err = %v, want ErrNoSession", err)
	}
}

// TestDockerEnsureReportsAnUnreachableBrowser is the honest failure for a container
// whose gate never came up: the session is not handed to the caller.
func TestDockerEnsureReportsAnUnreachableBrowser(t *testing.T) {
	fixture := newDockerFixture(t, WithWaitTimeout(300*time.Millisecond))

	// The container starts, but nothing answers on its gate.
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
	t.Setenv("VCOMPUTER_SANDBOX_NAME_PREFIX", "vcomputer-other-stack")
	t.Setenv("VCOMPUTER_SANDBOX_NOVNC_PORT", "7080")
	t.Setenv("VCOMPUTER_SANDBOX_GATE_PORT", "9443")
	t.Setenv("VCOMPUTER_EXEC_TIMEOUT", "5s")
	t.Setenv("VCOMPUTER_NAVIGATE_TIMEOUT", "7s")
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
	if runtime.namePrefix != "vcomputer-other-stack" {
		t.Fatalf("name prefix = %q", runtime.namePrefix)
	}
	if runtime.novncPort != "7080" || runtime.gatePort != "9443" {
		t.Fatalf("ports = %q/%q", runtime.novncPort, runtime.gatePort)
	}
	if runtime.execTimeout != 5*time.Second {
		t.Fatalf("exec timeout = %s", runtime.execTimeout)
	}
	if runtime.navigateTimeout != 7*time.Second {
		t.Fatalf("navigate timeout = %s", runtime.navigateTimeout)
	}
	if runtime.geometry != "1920x1080x24" {
		t.Fatalf("geometry = %q", runtime.geometry)
	}
}

// TestDockerDefaultsAreTheOnesTheStackUses keeps the two in step: the image the compose
// file builds, the network it declares, and the ports the sandbox's entrypoint reads are
// the ones the runtime reaches for.
func TestDockerDefaultsAreTheOnesTheStackUses(t *testing.T) {
	for _, key := range []string{
		"VCOMPUTER_DOCKER_BIN", "VCOMPUTER_SANDBOX_IMAGE", "VCOMPUTER_SANDBOX_NETWORK",
		"VCOMPUTER_SANDBOX_NAME_PREFIX", "VCOMPUTER_SANDBOX_NOVNC_PORT",
		"VCOMPUTER_SANDBOX_GATE_PORT", "VCOMPUTER_EXEC_TIMEOUT", "VCOMPUTER_NAVIGATE_TIMEOUT",
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
	if runtime.namePrefix != DefaultNamePrefix {
		t.Fatalf("name prefix = %q, want %q", runtime.namePrefix, DefaultNamePrefix)
	}
	if runtime.novncPort != DefaultNOVNCPort || runtime.gatePort != DefaultGatePort {
		t.Fatalf("ports = %q/%q, want %q/%q", runtime.novncPort, runtime.gatePort, DefaultNOVNCPort, DefaultGatePort)
	}
	if runtime.execTimeout != DefaultExecTimeout {
		t.Fatalf("exec timeout = %s, want %s", runtime.execTimeout, DefaultExecTimeout)
	}
	if runtime.navigateTimeout != DefaultNavigateTimeout {
		t.Fatalf("navigate timeout = %s, want %s", runtime.navigateTimeout, DefaultNavigateTimeout)
	}
}

// TestDockerPortConfigurationRefusesWhatIsNotAPort keeps a typo from becoming a URL
// that cannot work.
func TestDockerPortConfigurationRefusesWhatIsNotAPort(t *testing.T) {
	t.Setenv("VCOMPUTER_SANDBOX_NOVNC_PORT", "not-a-port")
	t.Setenv("VCOMPUTER_SANDBOX_GATE_PORT", "0x17")

	runtime := NewDocker()
	if runtime.novncPort != DefaultNOVNCPort || runtime.gatePort != DefaultGatePort {
		t.Fatalf("ports = %q/%q, want the defaults", runtime.novncPort, runtime.gatePort)
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

// TestMissingVolumeIsRecognised is the same classification for the other object a purge
// removes. Docker words it differently, and an unreachable daemon must not read as a
// volume that was already gone.
func TestMissingVolumeIsRecognised(t *testing.T) {
	missing := &dockerError{args: []string{"volume", "rm"}, stderr: "Error: No such volume: vcomputer-bot-a-home", err: errors.New("exit status 1")}
	if !missingVolume(missing) {
		t.Fatal("a missing volume was not recognised")
	}
	refused := &dockerError{args: []string{"volume", "rm"}, stderr: "Cannot connect to the Docker daemon at unix:///var/run/docker.sock", err: errors.New("exit status 1")}
	if missingVolume(refused) {
		t.Fatal("an unreachable daemon was reported as a missing volume")
	}
	if missingVolume(&dockerError{args: []string{"rm"}, stderr: "Error: No such container: vcomputer-bot-a", err: errors.New("exit status 1")}) {
		t.Fatal("a missing container was reported as a missing volume")
	}
}

// gateGet asks the fake gate for one endpoint with the secret attached, which is how
// the control plane reaches a sandbox's Chromium.
func gateGet(t *testing.T, address, secret string) *http.Response {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, address, nil)
	if err != nil {
		t.Fatalf("build the gate request: %v", err)
	}
	if secret != "" {
		request.Header.Set(SecretHeader, secret)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("ask the gate: %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

// isSecret reports whether value looks like a sandbox secret: 32 hex characters, which
// is the 16 bytes newSecret generates.
func isSecret(value string) bool {
	if len(value) != 32 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// envValue returns the value an argv passes for one environment variable, or "" when it
// passes none.
func envValue(argv []string, key string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--env" && strings.HasPrefix(argv[i+1], key+"=") {
			return strings.TrimPrefix(argv[i+1], key+"=")
		}
	}
	return ""
}

// findRequest returns the first request the fake gate received for a path.
func findRequest(t *testing.T, requests []cdpRequest, path string) cdpRequest {
	t.Helper()

	for _, request := range requests {
		if request.path == path {
			return request
		}
	}
	t.Fatalf("the gate never received %s: %v", path, requests)
	return cdpRequest{}
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
