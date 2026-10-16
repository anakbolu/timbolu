package main

// The gate's whole job is what it does to a request on the way to an upstream, so
// every test here asserts from the far side of the proxy: two httptest servers stand
// in for Chromium's DevTools endpoint and noVNC's websockify, they write down what
// they were asked for, and the tests read those notes. Nothing in this file needs the
// container's addresses, a real Chromium, or a real websockify.

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// testSecret is what the fixtures' gates were started with. Its length matters:
// wrongSecret below has exactly the same length and different bytes.
const testSecret = "sandbox-secret-for-the-tests"

// wrongSecret is the guess of the right length. A comparison that answered on the
// first differing byte, or one that stopped at the first differing length, would get
// this case wrong, which is why it is tested next to a guess of the wrong length.
var wrongSecret = strings.Repeat("x", len(testSecret))

// recordedRequest is one request as an upstream saw it. It is the only place the
// gate's effect on the path, the method, the body and the Host header is visible.
type recordedRequest struct {
	method string
	// target is the request URI — path and query — which the gate must carry over
	// unchanged apart from the prefix.
	target string
	host   string
	body   string
	// headers is the whole incoming header set, so a test can ask about one header —
	// Upgrade, in the websocket cases — without the fake having to know which ones
	// matter.
	headers http.Header
}

// requestLog is what the two fakes share: the requests they were handed, and the
// questions a test asks about them. It is a separate type because one fake answers plain
// HTTP and the other speaks websockets, and both still have to be able to say what they
// saw and how many times.
type requestLog struct {
	mu   sync.Mutex
	seen []recordedRequest
}

func (l *requestLog) record(request recordedRequest) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, request)
}

// count is how many requests the upstream received, which is what the tests that assert
// a refusal never reached it look at.
func (l *requestLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.seen)
}

// only is the one request the upstream saw, failing the test if it saw any other number
// of them: every assertion below is about a single request, and both "it arrived twice"
// and "it never arrived" are failures worth naming as such.
func (l *requestLog) only(t *testing.T) recordedRequest {
	t.Helper()

	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.seen) != 1 {
		t.Fatalf("the upstream saw %d requests, want exactly 1", len(l.seen))
	}
	return l.seen[0]
}

// fakeUpstream stands in for one of the two things the gate forwards to. It answers every
// request the same way and keeps what it saw, so a test can assert both that a request
// arrived and that it arrived in the shape the upstream expects.
type fakeUpstream struct {
	*httptest.Server
	*requestLog
}

// newFakeUpstream serves one upstream's answer, and records every request it is asked.
func newFakeUpstream(t *testing.T, status int, body string) *fakeUpstream {
	t.Helper()

	upstream := &fakeUpstream{requestLog: &requestLog{}}
	upstream.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			// A body the fake cannot read is the fake's failure, not the gate's, so it is
			// answered loudly rather than recorded as empty.
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		upstream.record(recordedRequest{
			method:  r.Method,
			target:  r.URL.RequestURI(),
			host:    r.Host,
			body:    string(payload),
			headers: r.Header.Clone(),
		})

		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(upstream.Close)

	return upstream
}

// gateLog collects the gate's own log lines in memory. A buffer rather than a writer
// that calls t.Logf, because the proxy logs from the goroutine serving a request,
// which can outlive the test function and would then panic.
type gateLog struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (l *gateLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.Write(p)
}

func (l *gateLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.String()
}

// gateFixture is the gate under test behind a real listener, with one fake upstream
// behind each of its doors. The listener is httptest's and the upstreams are fakes:
// nothing here is a real desktop, and nothing binds the container's own address.
type gateFixture struct {
	gate  *httptest.Server
	cdp   *fakeUpstream
	novnc *fakeUpstream
	logs  *gateLog
}

func newGateFixture(t *testing.T) *gateFixture {
	t.Helper()

	fixture := &gateFixture{
		cdp:   newFakeUpstream(t, http.StatusOK, "cdp upstream"),
		novnc: newFakeUpstream(t, http.StatusOK, "novnc upstream"),
		logs:  &gateLog{},
	}

	cfg := config{
		secret:    testSecret,
		gatePort:  defaultGatePort,
		novncPort: defaultNOVNCPort,
		cdpURL:    mustURL(t, fixture.cdp.URL),
		novncURL:  mustURL(t, fixture.novnc.URL),
	}
	fixture.gate = newGateServer(t, cfg, fixture.logs)

	return fixture
}

// newGateServer serves a gate the way run does, on httptest's listener, and registers
// its shutdown. The fixture and the failure-path test both need one, and both want the
// gate's log kept for the report a failing test prints.
func newGateServer(t *testing.T, cfg config, logs *gateLog) *httptest.Server {
	t.Helper()

	// Registered before the server, so it runs after the server is closed: the last
	// log line a request writes is worth having in the failure output.
	t.Cleanup(func() {
		if logged := strings.TrimSpace(logs.String()); logged != "" {
			t.Logf("the gate logged:\n%s", logged)
		}
	})

	server := httptest.NewServer(newGate(cfg, log.New(logs, "gate ", log.LstdFlags)))
	t.Cleanup(server.Close)

	return server
}

// mustURL parses an httptest server's address into the upstream a config carries.
func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return parsed
}

// do sends one request to a gate, presenting the secret only when there is one: every
// test in this file is about the secret being absent, wrong, or right.
func do(t *testing.T, gate *httptest.Server, method, path, secret string, body io.Reader) *http.Response {
	t.Helper()

	request, err := http.NewRequest(method, gate.URL+path, body)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	if secret != "" {
		request.Header.Set(secretHeader, secret)
	}
	response, err := gate.Client().Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })

	return response
}

// get is do for the common case: a GET with the fixture's own secret.
func (f *gateFixture) get(t *testing.T, path, secret string) *http.Response {
	t.Helper()
	return do(t, f.gate, http.MethodGet, path, secret, nil)
}

// readBody reads a response body, which the callers compare against what the upstream
// answered.
func readBody(t *testing.T, response *http.Response) string {
	t.Helper()

	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the body: %v", err)
	}
	return string(payload)
}

// TestARequestWithoutTheSecretIsRefusedAndNeverReachesAnUpstream is the check the gate
// exists for. Both doors are tried, because a refusal that covered only one of them
// would leave the other one open.
func TestARequestWithoutTheSecretIsRefusedAndNeverReachesAnUpstream(t *testing.T) {
	fixture := newGateFixture(t)

	for _, path := range []string{"/cdp/json/version", "/novnc/vnc.html"} {
		response := fixture.get(t, path, "")
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want %d", path, response.StatusCode, http.StatusUnauthorized)
		}
		if got := response.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("%s: content type = %q, want JSON", path, got)
		}
		if body := readBody(t, response); !strings.Contains(body, `"error":"unauthorized"`) {
			t.Fatalf("%s: body = %q, want the unauthorized error", path, body)
		}
	}

	if hits := fixture.cdp.count(); hits != 0 {
		t.Fatalf("the CDP upstream saw %d requests, want 0", hits)
	}
	if hits := fixture.novnc.count(); hits != 0 {
		t.Fatalf("the noVNC upstream saw %d requests, want 0", hits)
	}
}

// TestAWrongSecretIsRefusedWhateverItsLength is the other half of that check: a guess
// of the right length and guesses of the wrong lengths all get nothing, so neither the
// secret nor its length can be learned by trying.
func TestAWrongSecretIsRefusedWhateverItsLength(t *testing.T) {
	fixture := newGateFixture(t)

	for _, guess := range []struct {
		name  string
		value string
	}{
		{"the right length", wrongSecret},
		{"too short", wrongSecret[:8]},
		{"too long", wrongSecret + wrongSecret},
		{"a different secret of the right length", "another-secret-of-the-same-"},
	} {
		response := fixture.get(t, "/cdp/json/version", guess.value)
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want %d", guess.name, response.StatusCode, http.StatusUnauthorized)
		}
	}

	if hits := fixture.cdp.count(); hits != 0 {
		t.Fatalf("the CDP upstream saw %d requests, want 0", hits)
	}
	if hits := fixture.novnc.count(); hits != 0 {
		t.Fatalf("the noVNC upstream saw %d requests, want 0", hits)
	}
}

// TestTheCDPRouteIsForwardedWithThePrefixStripped covers what the proxy has to carry
// over — the method, the body, the query string, the upstream's status and its body —
// and the one thing it must not: the route prefix, which Chromium has never heard of.
func TestTheCDPRouteIsForwardedWithThePrefixStripped(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		method string
		path   string
		body   string
		want   string
		// wantBody is what the caller should read back. It is per case because net/http
		// strips the body of a HEAD response, so that one has nothing to compare.
		wantBody string
	}{
		{
			name:     "a GET keeps its query string",
			method:   http.MethodGet,
			path:     "/cdp/json/version?verbose=1",
			want:     "/json/version?verbose=1",
			wantBody: "cdp upstream",
		},
		{
			name:     "a PUT keeps its body and its query string",
			method:   http.MethodPut,
			path:     "/cdp/devtools/page/ABC?timeout=1",
			body:     `{"url":"about:blank"}`,
			want:     "/devtools/page/ABC?timeout=1",
			wantBody: "cdp upstream",
		},
		{
			name:     "a POST without a body still arrives",
			method:   http.MethodPost,
			path:     "/cdp/json/new",
			want:     "/json/new",
			wantBody: "cdp upstream",
		},
		{
			name:     "a DELETE is not special-cased away",
			method:   http.MethodDelete,
			path:     "/cdp/devtools/page/ABC",
			want:     "/devtools/page/ABC",
			wantBody: "cdp upstream",
		},
		{
			name:     "a PATCH is forwarded like any other verb",
			method:   http.MethodPatch,
			path:     "/cdp/json/version",
			want:     "/json/version",
			wantBody: "cdp upstream",
		},
		{
			name:     "a HEAD keeps its method",
			method:   http.MethodHead,
			path:     "/cdp/json/version",
			want:     "/json/version",
			wantBody: "",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newGateFixture(t)

			var body io.Reader
			if testCase.body != "" {
				body = strings.NewReader(testCase.body)
			}
			response := do(t, fixture.gate, testCase.method, testCase.path, testSecret, body)

			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
			}
			if got := readBody(t, response); got != testCase.wantBody {
				t.Fatalf("body = %q, want %q", got, testCase.wantBody)
			}

			seen := fixture.cdp.only(t)
			if seen.method != testCase.method {
				t.Fatalf("the upstream saw %s, want %s", seen.method, testCase.method)
			}
			if seen.target != testCase.want {
				t.Fatalf("the upstream saw %q, want %q", seen.target, testCase.want)
			}
			if seen.body != testCase.body {
				t.Fatalf("the upstream saw body %q, want %q", seen.body, testCase.body)
			}
			// Chromium answers 500 to a DevTools request whose Host header is neither an
			// IP nor localhost, so this is not cosmetic: it is the difference between a
			// working endpoint and one that refuses everything.
			if seen.host != upstreamHost {
				t.Fatalf("the upstream saw Host %q, want %q", seen.host, upstreamHost)
			}
			if hits := fixture.novnc.count(); hits != 0 {
				t.Fatalf("the noVNC upstream saw %d requests, want 0", hits)
			}
		})
	}
}

// TestTheNoVNCRouteIsForwardedToItsOwnUpstream is the same walk through the other door,
// and it pins that the two routes do not share an upstream: a noVNC page answered by
// Chromium's DevTools port, or the other way round, is a routing bug that a test with
// one upstream could not see.
func TestTheNoVNCRouteIsForwardedToItsOwnUpstream(t *testing.T) {
	fixture := newGateFixture(t)

	response := fixture.get(t, "/novnc/vnc.html?autoconnect=1", testSecret)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if got := readBody(t, response); got != "novnc upstream" {
		t.Fatalf("body = %q, want the noVNC upstream's own body", got)
	}

	seen := fixture.novnc.only(t)
	if seen.target != "/vnc.html?autoconnect=1" {
		t.Fatalf("the upstream saw %q, want the prefix stripped", seen.target)
	}
	if seen.host != upstreamHost {
		t.Fatalf("the upstream saw Host %q, want %q", seen.host, upstreamHost)
	}
	if hits := fixture.cdp.count(); hits != 0 {
		t.Fatalf("the CDP upstream saw %d requests, want 0", hits)
	}
}

// TestAPathThatIsNotADoorIsNotFound: the gate is not a general forwarder. /json/list is
// the case that matters — it is a real DevTools path, and it is only reachable here
// through /cdp/, so a caller that forgot the prefix must not be forwarded either. The
// match is on the path as it arrives, so /CDP/ is not the same door as /cdp/.
func TestAPathThatIsNotADoorIsNotFound(t *testing.T) {
	fixture := newGateFixture(t)

	for _, path := range []string{"/", "/json/list", "/CDP/json/version", "/vnc.html", "/healthz/extra"} {
		response := fixture.get(t, path, testSecret)
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want %d", path, response.StatusCode, http.StatusNotFound)
		}
	}

	if hits := fixture.cdp.count(); hits != 0 {
		t.Fatalf("the CDP upstream saw %d requests, want 0", hits)
	}
	if hits := fixture.novnc.count(); hits != 0 {
		t.Fatalf("the noVNC upstream saw %d requests, want 0", hits)
	}
}

// TestABareDoorForwardsToTheUpstreamRoot: the control plane's route for a session is
// exactly /v1/sessions/<bot>/cdp — that path is what Session.CDPURL hands a caller —
// so the gate receives /cdp with nothing after it, and a 404 there would be a door shut
// on its own doorstep. What that request forwards to is the upstream's root, which is
// the only thing it could have meant, and the upstream's answer is what comes back.
func TestABareDoorForwardsToTheUpstreamRoot(t *testing.T) {
	fixture := newGateFixture(t)

	for _, door := range []string{"/cdp", "/novnc"} {
		response := fixture.get(t, door, testSecret)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want the upstream's own %d", door, response.StatusCode, http.StatusOK)
		}
	}

	if got := fixture.cdp.only(t).target; got != "/" {
		t.Fatalf("the CDP upstream was asked for %q, want %q: a bare door is the upstream's root", got, "/")
	}
	if got := fixture.novnc.only(t).target; got != "/" {
		t.Fatalf("the noVNC upstream was asked for %q, want %q: a bare door is the upstream's root", got, "/")
	}
}

// TestHealthzIsOpenAndTouchesNoUpstream: the probe that decides whether the container is
// alive cannot present a secret, because the command that runs it has no way to hold
// one, and it must not depend on either desktop process being up — the gate answering is
// exactly what it is asking about.
func TestHealthzIsOpenAndTouchesNoUpstream(t *testing.T) {
	fixture := newGateFixture(t)

	response := fixture.get(t, healthPath, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if got := readBody(t, response); got != "ok\n" {
		t.Fatalf("body = %q, want the health line", got)
	}

	if hits := fixture.cdp.count(); hits != 0 {
		t.Fatalf("the CDP upstream saw %d requests, want 0", hits)
	}
	if hits := fixture.novnc.count(); hits != 0 {
		t.Fatalf("the noVNC upstream saw %d requests, want 0", hits)
	}
}

// TestAnUpstreamThatIsNotThereIsABadGateway is the answer when the desktop behind the
// gate is not listening: Chromium dying is the container's own failure and entrypoint.sh
// treats it as fatal, so this covers the window before the container goes away. The
// caller should see a 502 and a log line naming the proxy, not a hang and not an empty
// 200.
func TestAnUpstreamThatIsNotThereIsABadGateway(t *testing.T) {
	// A server that was up and is not any more: the address is real and the listener is
	// closed, so the dial is refused rather than the name failing to resolve.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := dead.URL
	dead.Close()

	logs := &gateLog{}
	gate := newGateServer(t, config{
		secret:   testSecret,
		cdpURL:   mustURL(t, address),
		novncURL: mustURL(t, address),
	}, logs)

	response := do(t, gate, http.MethodGet, "/cdp/json/version", testSecret, nil)
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadGateway)
	}

	logged := logs.String()
	if !strings.Contains(logged, "cdp") {
		t.Fatalf("the gate logged %q, want a line naming the cdp proxy", logged)
	}
}

// TestTheWebsocketUpgradeSurvivesTheProxy is the test the gate cannot do without: noVNC's
// desktop is a websocket and so is every CDP page socket, so a proxy that answered plain
// HTTP correctly but dropped the upgrade would leave a session with a picture of a desktop
// and no way to move the mouse. Both routes are driven, because they are two proxies.
//
// The upstream is hand-rolled rather than a websocket library, because what is under test
// is the gate: the upstream only has to hijack the connection, answer 101 with the accept
// the client's key asks for, and echo one frame back.
func TestTheWebsocketUpgradeSurvivesTheProxy(t *testing.T) {
	const payload = "hello desktop"

	for _, route := range []struct {
		name string
		// upstream is the door the request has to come out of.
		upstream string
		path     string
		// want is the request URI the upstream should see: the route prefix is the gate's
		// own invention and must not travel.
		want string
	}{
		{name: "the CDP route", upstream: "cdp", path: "/cdp/devtools/page/ABC", want: "/devtools/page/ABC"},
		{name: "the noVNC route", upstream: "novnc", path: "/novnc/websockify", want: "/websockify"},
	} {
		t.Run(route.name, func(t *testing.T) {
			fixture := newUpgradeFixture(t)

			address := strings.TrimPrefix(fixture.gate(t).URL, "http://")
			conn, reader := dialGate(t, address)
			key := base64.StdEncoding.EncodeToString([]byte("the-test-key-16"))
			// A handshake written by hand: the upgrade headers are what the proxy has to
			// carry through, so the test spells them out rather than letting a library
			// decide which ones a websocket needs.
			_, _ = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n%s: %s\r\n\r\n",
				route.path, address, key, secretHeader, testSecret)

			headers := readUpgradeResponse(t, reader)
			if got, want := headers.Get("Sec-WebSocket-Accept"), websocketAccept(key); got != want {
				t.Fatalf("Sec-WebSocket-Accept = %q, want %q", got, want)
			}

			if err := writeMaskedTextFrame(conn, payload); err != nil {
				t.Fatalf("write the client frame: %v", err)
			}
			opcode, echoed, err := readServerFrame(reader)
			if err != nil {
				t.Fatalf("read the echoed frame: %v", err)
			}
			if opcode != opcodeText {
				t.Fatalf("the echo has opcode %d, want a text frame", opcode)
			}
			if echoed != payload {
				t.Fatalf("the echo carried %q, want %q", echoed, payload)
			}

			// The handshake as the upstream saw it. An upgrade request is where the proxy
			// has the most to get wrong — the headers it has to keep, the one it has to
			// rewrite, and the prefix it has to strip — so it is asserted from that side and
			// not only from the client's.
			hit, missed := fixture.at(route.upstream)
			seen := hit.only(t)
			if seen.target != route.want {
				t.Fatalf("the upstream saw %q, want %q", seen.target, route.want)
			}
			if seen.host != upstreamHost {
				t.Fatalf("the upstream saw Host %q, want %q", seen.host, upstreamHost)
			}
			if got := seen.headers.Get("Upgrade"); got != "websocket" {
				t.Fatalf("the upstream saw Upgrade %q, want websocket", got)
			}
			if got := seen.headers.Get("Connection"); !strings.Contains(got, "Upgrade") {
				t.Fatalf("the upstream saw Connection %q, want it to carry Upgrade", got)
			}
			if hits := missed.count(); hits != 0 {
				t.Fatalf("the other upstream saw %d requests, want 0", hits)
			}
		})
	}
}

// upgradeUpstream is the upstream a websocket test needs: it hijacks the connection, does
// the handshake, and echoes one frame back. Chromium's page socket and websockify both
// speak this, so one fake stands in for either of them.
type upgradeUpstream struct {
	*httptest.Server
	*requestLog
}

// newUpgradeUpstream serves a websocket echo, and records the handshake it was handed.
func newUpgradeUpstream(t *testing.T) *upgradeUpstream {
	t.Helper()

	upstream := &upgradeUpstream{requestLog: &requestLog{}}
	upstream.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.record(recordedRequest{
			method:  r.Method,
			target:  r.URL.RequestURI(),
			host:    r.Host,
			headers: r.Header.Clone(),
		})

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("the upstream cannot hijack a %T", w)
			return
		}
		conn, buffered, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()

		_, _ = fmt.Fprintf(buffered, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
			websocketAccept(r.Header.Get("Sec-WebSocket-Key")))
		if err := buffered.Flush(); err != nil {
			t.Errorf("write the handshake: %v", err)
			return
		}

		// One frame in, the same frame out, unmasked as a server must. A frame that
		// arrives unmasked means the proxy did not carry the client's bytes as they were,
		// and readClientFrame reports that as the error it is.
		opcode, frame, err := readClientFrame(buffered.Reader)
		if err != nil {
			t.Errorf("read the client frame: %v", err)
			return
		}
		if err := writeServerFrame(buffered, opcode, frame); err != nil {
			t.Errorf("write the echo: %v", err)
			return
		}
		if err := buffered.Flush(); err != nil {
			t.Errorf("flush the echo: %v", err)
		}
	}))
	t.Cleanup(upstream.Close)

	return upstream
}

// upgradeFixture is the two websocket upstreams and a gate in front of them, so a test
// can follow one request to the door it should have used and assert the other door
// untouched.
type upgradeFixture struct {
	cdp   *upgradeUpstream
	novnc *upgradeUpstream
}

func newUpgradeFixture(t *testing.T) *upgradeFixture {
	t.Helper()

	return &upgradeFixture{cdp: newUpgradeUpstream(t), novnc: newUpgradeUpstream(t)}
}

// gate serves a gate whose two upstreams are this fixture's.
func (f *upgradeFixture) gate(t *testing.T) *httptest.Server {
	t.Helper()

	return newGateServer(t, config{
		secret:    testSecret,
		gatePort:  defaultGatePort,
		novncPort: defaultNOVNCPort,
		cdpURL:    mustURL(t, f.cdp.URL),
		novncURL:  mustURL(t, f.novnc.URL),
	}, &gateLog{})
}

// at is the upstream a route is supposed to reach, and the one it must not.
func (f *upgradeFixture) at(route string) (hit, missed *upgradeUpstream) {
	if route == "cdp" {
		return f.cdp, f.novnc
	}
	return f.novnc, f.cdp
}

// dialGate opens a raw connection to the gate's listener. net.Dial rather than an HTTP
// client, because after the 101 this is a byte stream and not a response, so the test has
// to speak the protocol itself.
func dialGate(t *testing.T, address string) (net.Conn, *bufio.Reader) {
	t.Helper()

	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("dial the gate: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	// A deadline, so a proxy that never answers fails this test instead of hanging it.
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatalf("set the deadline: %v", err)
	}

	return conn, bufio.NewReader(conn)
}

// TestRunRefusesAnEmptySecret is the startup check. A gate with no secret would have to
// either refuse every request (useless) or accept every request (an open DevTools
// endpoint), so it must not come up at all. It is tested through run rather than through
// parseConfig because "does not start" is the promise, and run is where the process would
// otherwise reach its listener.
func TestRunRefusesAnEmptySecret(t *testing.T) {
	err := run([]string{"vcomputer-gate"}, func(string) string { return "" })
	if err == nil {
		t.Fatal("run came up with an empty SANDBOX_SECRET")
	}
	if !strings.Contains(err.Error(), "SANDBOX_SECRET") {
		t.Fatalf("error = %v, want it to name SANDBOX_SECRET", err)
	}
}

// TestThePortsDefaultToTheOnesTheStackUses pins the two numbers the control plane and the
// sandbox image agree on: an unset GATE_PORT or NOVNC_PORT must not silently become
// something else, and the noVNC upstream has to be built from the port that was read. It
// asserts on the parsed configuration rather than by listening, because the value the
// gate would use is the point, not the socket.
func TestThePortsDefaultToTheOnesTheStackUses(t *testing.T) {
	t.Setenv("SANDBOX_SECRET", testSecret)
	t.Setenv("GATE_PORT", "")
	t.Setenv("NOVNC_PORT", "")

	cfg, err := parseConfig(os.Getenv)
	if err != nil {
		t.Fatalf("parse the environment: %v", err)
	}
	if cfg.secret != testSecret {
		t.Fatalf("secret = %q, want the one in the environment", cfg.secret)
	}
	if cfg.gatePort != "8443" {
		t.Fatalf("gate port = %q, want 8443", cfg.gatePort)
	}
	if cfg.novncPort != "6080" {
		t.Fatalf("novnc port = %q, want 6080", cfg.novncPort)
	}
	if got := cfg.cdpURL.String(); got != "http://127.0.0.1:9222" {
		t.Fatalf("cdp upstream = %q, want http://127.0.0.1:9222", got)
	}
	if got := cfg.novncURL.String(); got != "http://127.0.0.1:6080" {
		t.Fatalf("novnc upstream = %q, want http://127.0.0.1:6080", got)
	}
}

// TestThePortsComeFromTheEnvironment is the same read with every value set: a deployment
// that moves a port moves the upstream the gate proxies to with it, or it would forward a
// session to a port nothing listens on.
func TestThePortsComeFromTheEnvironment(t *testing.T) {
	t.Setenv("SANDBOX_SECRET", "the-session-secret")
	t.Setenv("GATE_PORT", "9443")
	t.Setenv("NOVNC_PORT", "8080")

	cfg, err := parseConfig(os.Getenv)
	if err != nil {
		t.Fatalf("parse the environment: %v", err)
	}
	if cfg.secret != "the-session-secret" {
		t.Fatalf("secret = %q, want the one in the environment", cfg.secret)
	}
	if cfg.gatePort != "9443" || cfg.novncPort != "8080" {
		t.Fatalf("ports = %q/%q, want 9443/8080", cfg.gatePort, cfg.novncPort)
	}
	if got := cfg.novncURL.String(); got != "http://127.0.0.1:8080" {
		t.Fatalf("novnc upstream = %q, want http://127.0.0.1:8080", got)
	}
}

// TestAPortThatIsNotAPortFallsBackToTheDefault keeps a typo from turning into an address
// that cannot work. The control plane reads the ports it passes in the same way, so the
// two ends of the contract degrade to the same numbers.
func TestAPortThatIsNotAPortFallsBackToTheDefault(t *testing.T) {
	t.Setenv("SANDBOX_SECRET", testSecret)
	t.Setenv("GATE_PORT", "not-a-port")
	t.Setenv("NOVNC_PORT", "0x17")

	cfg, err := parseConfig(os.Getenv)
	if err != nil {
		t.Fatalf("parse the environment: %v", err)
	}
	if cfg.gatePort != defaultGatePort || cfg.novncPort != defaultNOVNCPort {
		t.Fatalf("ports = %q/%q, want the defaults", cfg.gatePort, cfg.novncPort)
	}
}

// opcodeText is the websocket opcode for a text frame, which is the only kind the
// hand-rolled upstream and this test exchange.
const opcodeText = 1

// websocketAccept is the handshake answer RFC 6455 asks for: SHA-1 of the client's key
// and the fixed GUID, base64 encoded. It is computed here rather than imported, because
// neither the gate nor this test needs a websocket library — the gate only has to move
// bytes once the upgrade is done, and the test only has to speak the first two frames.
func websocketAccept(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// readUpgradeResponse reads the status line and the headers of the 101 the gate forwards,
// and fails the test on anything else: a proxy that answered 200 here has broken the
// session even though it answered the request.
func readUpgradeResponse(t *testing.T, reader *bufio.Reader) textproto.MIMEHeader {
	t.Helper()

	lines := textproto.NewReader(reader)
	status, err := lines.ReadLine()
	if err != nil {
		t.Fatalf("read the status line: %v", err)
	}
	if !strings.Contains(status, " 101 ") {
		t.Fatalf("status line = %q, want a 101", status)
	}
	headers, err := lines.ReadMIMEHeader()
	if err != nil {
		t.Fatalf("read the handshake headers: %v", err)
	}
	return headers
}

// writeMaskedTextFrame writes one client-to-server text frame: FIN, the text opcode, the
// length, and the four-byte mask RFC 6455 requires a client to apply. Masking here is not
// decoration — an upstream that receives an unmasked client frame is entitled to close the
// connection, and readClientFrame below fails the test if that is what happened.
func writeMaskedTextFrame(w io.Writer, payload string) error {
	if len(payload) > maxShortFramePayload {
		return fmt.Errorf("this helper writes the short frame form only, and %d bytes needs the extended one", len(payload))
	}

	mask := [4]byte{0x37, 0xfa, 0x21, 0x3d}
	frame := []byte{0x80 | opcodeText, 0x80 | byte(len(payload))}
	frame = append(frame, mask[:]...)
	for i := range len(payload) {
		frame = append(frame, payload[i]^mask[i%4])
	}

	_, err := w.Write(frame)
	return err
}

// readClientFrame is the upstream's side of the exchange: it reads one masked client frame
// and unmasks it. A frame that arrives unmasked means the proxy did not carry the client's
// bytes through as they were.
func readClientFrame(reader *bufio.Reader) (byte, string, error) {
	header, err := readFrameHeader(reader)
	if err != nil {
		return 0, "", err
	}
	if header[1]&0x80 == 0 {
		return 0, "", errors.New("the client frame arrived unmasked")
	}

	mask := make([]byte, 4)
	if _, err := io.ReadFull(reader, mask); err != nil {
		return 0, "", err
	}
	payload, err := readFramePayload(reader, header)
	if err != nil {
		return 0, "", err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}

	return header[0] & 0x0f, string(payload), nil
}

// writeServerFrame writes one unmasked frame back, which is what an echo has to be: a
// server may not mask.
func writeServerFrame(w io.Writer, opcode byte, payload string) error {
	if len(payload) > maxShortFramePayload {
		return fmt.Errorf("this helper writes the short frame form only, and %d bytes needs the extended one", len(payload))
	}

	frame := append([]byte{0x80 | opcode, byte(len(payload))}, payload...)
	_, err := w.Write(frame)
	return err
}

// readServerFrame reads one frame from the upstream and fails on anything a server is not
// allowed to send, so a mask bit here is reported as the protocol error it is rather than
// decoded into nonsense.
func readServerFrame(reader *bufio.Reader) (byte, string, error) {
	header, err := readFrameHeader(reader)
	if err != nil {
		return 0, "", err
	}
	if header[0]&0x80 == 0 {
		return 0, "", errors.New("the upstream did not finish the frame")
	}
	if header[1]&0x80 != 0 {
		return 0, "", errors.New("the upstream masked a server frame")
	}

	payload, err := readFramePayload(reader, header)
	if err != nil {
		return 0, "", err
	}

	return header[0] & 0x0f, string(payload), nil
}

// maxShortFramePayload is the largest payload the two-byte frame header can carry. The
// helpers below handle that form only: the payloads here are a greeting, and a silent
// truncation of a longer one would be worse than an error.
const maxShortFramePayload = 125

// readFrameHeader reads the two-byte header both directions share.
func readFrameHeader(reader *bufio.Reader) ([2]byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return header, err
	}
	if header[1]&0x7f > maxShortFramePayload {
		return header, fmt.Errorf("the frame uses the extended length form (%d), which these helpers do not read", header[1]&0x7f)
	}
	return header, nil
}

// readFramePayload reads the payload the header announced, without touching the mask bit:
// the two directions treat that bit differently, so it is the caller's business.
func readFramePayload(reader *bufio.Reader, header [2]byte) ([]byte, error) {
	payload := make([]byte, header[1]&0x7f)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	return payload, nil
}
