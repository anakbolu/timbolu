// Package httpapi's tests drive the real handler over a real listener, with the
// in-memory runtime behind it. Nothing is mocked: the point of the PoC is that the
// HTTP surface works end to end, so the test asserts what a caller sees.
//
// The one thing a test cannot do is reach a container, so the two proxy routes — noVNC
// and the DevTools API — are driven against a fake gate that stands in for the sandbox's
// own listener and records exactly what the control plane sent it.
package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/robzlabz/employeebot/apps/virtual-computer/control/internal/sandbox"
)

// testToken is the secret every test request presents. It is not a real secret: the
// point is that the surface refuses to answer without one.
const testToken = "test-token-abc123"

// newTestServer builds the handler with a quiet logger, a fresh stub runtime, and the
// same token requirement a deployment has.
func newTestServer(t *testing.T) (*httptest.Server, *sandbox.InMemoryRuntime) {
	t.Helper()

	runtime := sandbox.NewInMemory()
	server := httptest.NewServer(New(runtime, log.New(io.Discard, "", 0), WithToken(testToken)))
	t.Cleanup(server.Close)
	return server, runtime
}

// do sends one authenticated request and returns the status and body.
func do(t *testing.T, method, url, body string) (int, []byte) {
	t.Helper()
	return doAs(t, testToken, method, url, body)
}

// doAs sends one request as the given token, and sends no Authorization header at all
// when the token is empty.
func doAs(t *testing.T, token, method, url, body string) (int, []byte) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer response.Body.Close()

	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return response.StatusCode, payload
}

// TestEnsureThenStatus is the session lifecycle a worker depends on: ensure is
// idempotent, and status reports what ensure created.
func TestEnsureThenStatus(t *testing.T) {
	server, runtime := newTestServer(t)

	status, body := do(t, http.MethodPost, server.URL+"/v1/sessions", `{"bot_id":"bot-a"}`)
	if status != http.StatusOK {
		t.Fatalf("ensure: status = %d, body = %s", status, body)
	}

	var created sandbox.Session
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if created.BotID != "bot-a" || created.State != sandbox.StateRunning {
		t.Fatalf("ensure: session = %+v", created)
	}
	if created.Volume != "vcomputer-bot-a-home" {
		t.Fatalf("ensure: volume = %q, want the per-bot named volume", created.Volume)
	}

	// A second ensure must not start a second session: it returns the same record.
	status, body = do(t, http.MethodPost, server.URL+"/v1/sessions", `{"bot_id":"bot-a"}`)
	if status != http.StatusOK {
		t.Fatalf("second ensure: status = %d, body = %s", status, body)
	}
	var again sandbox.Session
	if err := json.Unmarshal(body, &again); err != nil {
		t.Fatalf("decode second session: %v", err)
	}
	if !again.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("ensure is not idempotent: created_at moved from %s to %s", created.CreatedAt, again.CreatedAt)
	}

	status, body = do(t, http.MethodGet, server.URL+"/v1/sessions/bot-a", "")
	if status != http.StatusOK {
		t.Fatalf("status: status = %d, body = %s", status, body)
	}

	// And the runtime really holds it: the API is not answering from its own copy.
	if _, err := runtime.Status(t.Context(), "bot-a"); err != nil {
		t.Fatalf("runtime status: %v", err)
	}
}

// TestEveryRouteExceptHealthRequiresTheToken is the check that matters most on this
// surface: it can start a sandbox and run a command inside it, so nothing but the
// health probe may be answered without the secret.
func TestEveryRouteExceptHealthRequiresTheToken(t *testing.T) {
	server, _ := newTestServer(t)

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/v1/sessions"},
		{http.MethodGet, "/v1/sessions/bot-a"},
		{http.MethodDelete, "/v1/sessions/bot-a"},
		{http.MethodPost, "/v1/sessions/bot-a/screenshot"},
		{http.MethodGet, "/v1/sessions/bot-a/screenshot"},
		{http.MethodPost, "/v1/sessions/bot-a/click"},
		{http.MethodPost, "/v1/sessions/bot-a/type"},
		{http.MethodPost, "/v1/sessions/bot-a/navigate"},
		{http.MethodPost, "/v1/sessions/bot-a/exec"},
		{http.MethodGet, "/v1/sessions/bot-a/novnc/vnc.html"},
		{http.MethodGet, "/v1/sessions/bot-a/novnc/websockify"},
		// Both doors are registered twice: the bare path is what Session.CDPURL hands a
		// caller, and everything under it is the DevTools API itself.
		{http.MethodGet, "/v1/sessions/bot-a/cdp"},
		{http.MethodPost, "/v1/sessions/bot-a/cdp"},
		{http.MethodGet, "/v1/sessions/bot-a/cdp/json/list"},
		{http.MethodPost, "/v1/sessions/bot-a/cdp/json/new?https://example.com"},
		{http.MethodGet, "/v1/sessions/bot-a/cdp/json/version"},
		{http.MethodPost, "/v1/sessions/bot-a/cdp/json/close/target-1"},
		// The cleanup route is not about one bot, which is exactly why it has to be on
		// this list: it removes containers and volumes, so an unauthenticated caller
		// must not reach it either.
		{http.MethodPost, "/v1/admin/cleanup"},
	}
	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			status, body := doAs(t, "", route.method, server.URL+route.path, "")
			if status != http.StatusUnauthorized {
				t.Fatalf("without a token: status = %d, body = %s, want 401", status, body)
			}
			if !strings.Contains(string(body), "token") {
				t.Fatalf("body = %s, want it to name the missing token", body)
			}
		})
	}

	// A wrong token is refused the same way, and a token in the query string is only
	// accepted on the noVNC route: it reaches logs and history, so it is not a second
	// way to call the API.
	if status, _ := doAs(t, "wrong-token", http.MethodGet, server.URL+"/v1/sessions/bot-a", ""); status != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d, want 401", status)
	}
	if status, _ := doAs(t, "", http.MethodGet, server.URL+"/v1/sessions/bot-a?token="+testToken, ""); status != http.StatusUnauthorized {
		t.Fatalf("query token off the noVNC route: status = %d, want 401", status)
	}

	// /health is open: the compose healthcheck and the e2e script both probe it
	// before anything else exists.
	if status, body := doAs(t, "", http.MethodGet, server.URL+"/health", ""); status != http.StatusOK {
		t.Fatalf("health without a token: status = %d, body = %s", status, body)
	}
}

// TestStatusUnknownBotIs404 is the error a caller has to be able to branch on.
func TestStatusUnknownBotIs404(t *testing.T) {
	server, _ := newTestServer(t)

	status, body := do(t, http.MethodGet, server.URL+"/v1/sessions/nobody", "")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404", status, body)
	}
}

// TestScreenshotAnswersDecodablePNG is the deliverable the demo page embeds: real
// PNG bytes, not a placeholder string.
func TestScreenshotAnswersDecodablePNG(t *testing.T) {
	server, _ := newTestServer(t)

	if status, body := do(t, http.MethodPost, server.URL+"/v1/sessions", `{"bot_id":"bot-png"}`); status != http.StatusOK {
		t.Fatalf("ensure: status = %d, body = %s", status, body)
	}

	status, body := do(t, http.MethodPost, server.URL+"/v1/sessions/bot-png/screenshot", "")
	if status != http.StatusOK {
		t.Fatalf("screenshot: status = %d, body = %s", status, body)
	}
	if len(body) == 0 {
		t.Fatal("screenshot: empty body")
	}

	image, err := png.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("screenshot is not a PNG: %v", err)
	}
	bounds := image.Bounds()
	if bounds.Dx() < 100 || bounds.Dy() < 100 {
		t.Fatalf("screenshot is %dx%d, too small to be a desktop", bounds.Dx(), bounds.Dy())
	}
	// The same capture over GET: an <img> cannot send a POST, and the demo page
	// embeds this route directly.
	status, body = do(t, http.MethodGet, server.URL+"/v1/sessions/bot-png/screenshot", "")
	if status != http.StatusOK {
		t.Fatalf("screenshot over GET: status = %d, body = %s", status, body)
	}
	if _, err := png.Decode(bytes.NewReader(body)); err != nil {
		t.Fatalf("screenshot over GET is not a PNG: %v", err)
	}
}

// TestClickTypeNavigateAndExecAreRecorded covers the actions the runtime performs:
// each one must reach it, and the stub's exec must report that it did not run.
func TestClickTypeNavigateAndExecAreRecorded(t *testing.T) {
	server, runtime := newTestServer(t)

	if status, body := do(t, http.MethodPost, server.URL+"/v1/sessions", `{"bot_id":"bot-actions"}`); status != http.StatusOK {
		t.Fatalf("ensure: status = %d, body = %s", status, body)
	}

	if status, body := do(t, http.MethodPost, server.URL+"/v1/sessions/bot-actions/click", `{"x":640,"y":400}`); status != http.StatusOK {
		t.Fatalf("click: status = %d, body = %s", status, body)
	}
	if status, body := do(t, http.MethodPost, server.URL+"/v1/sessions/bot-actions/type", `{"text":"halo"}`); status != http.StatusOK {
		t.Fatalf("type: status = %d, body = %s", status, body)
	}
	if status, body := do(t, http.MethodPost, server.URL+"/v1/sessions/bot-actions/navigate", `{"url":"https://example.com/riset"}`); status != http.StatusOK {
		t.Fatalf("navigate: status = %d, body = %s", status, body)
	}

	status, body := do(t, http.MethodPost, server.URL+"/v1/sessions/bot-actions/exec", `{"command":"ls -la /home/bolu"}`)
	if status != http.StatusOK {
		t.Fatalf("exec: status = %d, body = %s", status, body)
	}
	var result sandbox.ExecResult
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode exec result: %v", err)
	}
	if !result.Stub {
		t.Fatal("exec must report that it is a stub: the memory runtime never runs a command")
	}
	if !strings.Contains(result.Stdout, "ls -la /home/bolu") {
		t.Fatalf("exec stdout = %q, want the echoed command", result.Stdout)
	}

	// The record is the evidence: every call arrived, in order, with its body.
	actions := runtime.Actions("bot-actions")
	want := []string{
		"click:640,400",
		"type:halo",
		"navigate:https://example.com/riset",
		"exec:ls -la /home/bolu",
	}
	if len(actions) != len(want) {
		t.Fatalf("actions = %+v, want %d", actions, len(want))
	}
	for i, action := range actions {
		if got := action.Kind + ":" + action.Detail; got != want[i] {
			t.Fatalf("action %d = %q, want %q", i, got, want[i])
		}
	}
}

// TestNavigateRefusesWhatABrowserShouldNotOpen is the scheme rule as a caller sees it:
// a caller error, not a runtime failure, and the answer names the rule.
func TestNavigateRefusesWhatABrowserShouldNotOpen(t *testing.T) {
	server, _ := newTestServer(t)

	if status, body := do(t, http.MethodPost, server.URL+"/v1/sessions", `{"bot_id":"bot-url"}`); status != http.StatusOK {
		t.Fatalf("ensure: status = %d, body = %s", status, body)
	}

	for _, target := range []string{"", "file:///etc/passwd", "javascript:alert(1)", "chrome://settings"} {
		body := fmt.Sprintf(`{"url":%q}`, target)
		status, payload := do(t, http.MethodPost, server.URL+"/v1/sessions/bot-url/navigate", body)
		if status != http.StatusBadRequest {
			t.Fatalf("navigate(%q): status = %d, body = %s, want 400", target, status, payload)
		}
	}
}

// TestNavigateReportsWhereTheBrowserEndedUp is the answer shape the route promises: not
// "the command was sent", but where the browser went and what the page calls itself. A
// redirect is a normal outcome, so `final_url` is the runtime's answer and not an echo of
// the request — a caller holding only the URL it asked for could not tell that one
// happened.
func TestNavigateReportsWhereTheBrowserEndedUp(t *testing.T) {
	runtime := newGateRuntime("http://127.0.0.1:1")
	runtime.navigation = &sandbox.Navigation{FinalURL: "https://example.com/riset/", Title: "Riset microVM"}
	if _, err := runtime.Ensure(t.Context(), "bot-nav"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	server := httptest.NewServer(New(runtime, log.New(io.Discard, "", 0), WithToken(testToken)))
	t.Cleanup(server.Close)

	status, body := do(t, http.MethodPost, server.URL+"/v1/sessions/bot-nav/navigate", `{"url":"https://example.com/riset"}`)
	if status != http.StatusOK {
		t.Fatalf("navigate: status = %d, body = %s", status, body)
	}
	var answer struct {
		Navigated bool   `json:"navigated"`
		FinalURL  string `json:"final_url"`
		Title     string `json:"title"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decode the navigate answer: %v (body = %s)", err, body)
	}
	if !answer.Navigated {
		t.Fatalf("answer = %+v, want navigated", answer)
	}
	if answer.FinalURL != "https://example.com/riset/" {
		t.Fatalf("final_url = %q, want where the browser landed", answer.FinalURL)
	}
	if answer.Title != "Riset microVM" {
		t.Fatalf("title = %q, want the document's title", answer.Title)
	}
	if len(runtime.navigations) != 1 || runtime.navigations[0] != "https://example.com/riset" {
		t.Fatalf("navigations = %v, want the url the caller asked for", runtime.navigations)
	}
}

// TestNavigateReportsAFailedNavigationAs502 covers the one failure that carries facts the
// caller needs. The request was well formed and the browser could not carry it out — a
// sandbox with no egress lands on chrome-error:// for every address outside its own
// network — so the answer is 502 with both URLs rather than the 400 a bad request gets or
// the 200 a plain success would produce.
func TestNavigateReportsAFailedNavigationAs502(t *testing.T) {
	runtime := newGateRuntime("http://127.0.0.1:1")
	runtime.navigateErr = &sandbox.NavigationError{
		URL:      "https://example.com/",
		FinalURL: "chrome-error://chromewebdata/",
		Reason:   "the page ended on the browser's error page",
	}
	server := httptest.NewServer(New(runtime, log.New(io.Discard, "", 0), WithToken(testToken)))
	t.Cleanup(server.Close)

	status, body := do(t, http.MethodPost, server.URL+"/v1/sessions/bot-nav/navigate", `{"url":"https://example.com/"}`)
	if status != http.StatusBadGateway {
		t.Fatalf("navigate: status = %d, body = %s, want 502", status, body)
	}
	var failure struct {
		Error    string `json:"error"`
		URL      string `json:"url"`
		FinalURL string `json:"final_url"`
	}
	if err := json.Unmarshal(body, &failure); err != nil {
		t.Fatalf("decode the failure: %v (body = %s)", err, body)
	}
	if failure.Error != "navigation failed" {
		t.Fatalf("error = %q, want the one sentence a caller branches on", failure.Error)
	}
	if failure.URL != "https://example.com/" || failure.FinalURL != "chrome-error://chromewebdata/" {
		t.Fatalf("failure = %+v, want both URLs", failure)
	}
}

// TestActionsOnUnknownBotAre404 keeps the action routes from creating a session as
// a side effect of being called.
func TestActionsOnUnknownBotAre404(t *testing.T) {
	server, _ := newTestServer(t)

	for _, path := range []string{"/click", "/type", "/navigate", "/screenshot"} {
		body := `{"x":1,"y":2}`
		if path == "/type" {
			body = `{"text":"halo"}`
		}
		if path == "/navigate" {
			body = `{"url":"https://example.com"}`
		}
		status, payload := do(t, http.MethodPost, server.URL+"/v1/sessions/nobody"+path, body)
		if status != http.StatusNotFound {
			t.Fatalf("%s: status = %d, body = %s, want 404", path, status, payload)
		}
	}
}

// TestStopIsHonest covers the half of the lifecycle that removes a session: stop
// answers once, and a second stop reports that there is nothing to stop.
func TestStopIsHonest(t *testing.T) {
	server, _ := newTestServer(t)

	if status, body := do(t, http.MethodPost, server.URL+"/v1/sessions", `{"bot_id":"bot-stop"}`); status != http.StatusOK {
		t.Fatalf("ensure: status = %d, body = %s", status, body)
	}
	if status, body := do(t, http.MethodDelete, server.URL+"/v1/sessions/bot-stop", ""); status != http.StatusOK {
		t.Fatalf("stop: status = %d, body = %s", status, body)
	}
	if status, body := do(t, http.MethodGet, server.URL+"/v1/sessions/bot-stop", ""); status != http.StatusNotFound {
		t.Fatalf("status after stop = %d, body = %s, want 404", status, body)
	}
	// Deleting twice is a caller that retried, and the second answer has to say the
	// session is gone rather than that something failed.
	if status, body := do(t, http.MethodDelete, server.URL+"/v1/sessions/bot-stop", ""); status != http.StatusNotFound {
		t.Fatalf("second stop = %d, body = %s, want 404", status, body)
	}
}

// TestInvalidInputIs400 covers the refusals that must be a caller error and not a
// runtime failure: no bot id, an id that cannot name a container, an unknown field,
// and an exec with no command.
func TestInvalidInputIs400(t *testing.T) {
	server, _ := newTestServer(t)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"no bot id", http.MethodPost, "/v1/sessions", `{}`},
		{"bot id with a slash", http.MethodPost, "/v1/sessions", `{"bot_id":"../etc"}`},
		{"unknown field", http.MethodPost, "/v1/sessions", `{"bot":"bot-a"}`},
		{"exec without a command", http.MethodPost, "/v1/sessions/bot-a/exec", `{}`},
		{"navigate without a url", http.MethodPost, "/v1/sessions/bot-a/navigate", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A session exists, so the refusal is about the input and not the lookup.
			if status, body := do(t, http.MethodPost, server.URL+"/v1/sessions", `{"bot_id":"bot-a"}`); status != http.StatusOK {
				t.Fatalf("ensure: status = %d, body = %s", status, body)
			}
			status, body := do(t, tc.method, server.URL+tc.path, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s, want 400", status, body)
			}
		})
	}
}

// gateRequest is one request as the fake gate received it. The point of recording
// rather than asserting inside the handler is that the handler answers on a different
// goroutine than the test's: what arrived is read after the answer, and it is read as a
// value rather than through a shared connection.
type gateRequest struct {
	method string
	path   string
	query  string
	secret string
	// authorization and cookie are the control plane's own credentials, recorded so a
	// test can prove they were stripped before the request left for the sandbox.
	authorization string
	cookie        string
	body          string
}

// gateRuntime is the stub with one difference: the gate its sessions report is one the
// test owns, so both proxies are exercised against a real server instead of against a
// container name that does not resolve here.
//
// It answers whatever the test put in it: the address, the secret, and — for the
// lifecycle routes — whether a purge or a cleanup should fail. The recorded requests and
// the arguments the routes were called with are what the tests assert on, because those
// are the things the control plane is responsible for getting right.
type gateRuntime struct {
	*sandbox.InMemoryRuntime

	target string
	secret string

	mu       sync.Mutex
	requests []gateRequest
	purges   []string
	stops    []string
	cleans   []bool
	// purgeErr and cleanupResult are what the two admin routes should report: a purge
	// that failed, and a cleanup that removed something or failed to.
	purgeErr      error
	cleanupResult sandbox.CleanupResult
	// navigations records what Navigate was asked to open, and navigation and
	// navigateErr are the answer it gives. A nil navigation means "ask the stub", which
	// is what a test that does not care about the result wants.
	navigations []string
	navigation  *sandbox.Navigation
	navigateErr error
}

// newGateRuntime builds the fake with the secret a real session would have.
func newGateRuntime(target string) *gateRuntime {
	return &gateRuntime{
		InMemoryRuntime: sandbox.NewInMemory(),
		target:          target,
		secret:          "sandbox-secret-abc123",
	}
}

// GateTarget answers the address and secret the test chose, whatever bot is asked for:
// the session lookup is the stub's job and it is already covered elsewhere.
func (r *gateRuntime) GateTarget(context.Context, string) (sandbox.SandboxTarget, error) {
	return sandbox.SandboxTarget{URL: r.target, Secret: r.secret}, nil
}

// Navigate records what the route asked for and answers what the test configured.
//
// The docker runtime is where a navigation really fails — a page that ended on the
// browser's error page, or that never finished loading — and the control plane's job is
// to report that failure as itself, so this is the only way to reach the 502 branch from
// a test without a container.
func (r *gateRuntime) Navigate(ctx context.Context, botID, targetURL string) (sandbox.Navigation, error) {
	r.mu.Lock()
	r.navigations = append(r.navigations, targetURL)
	answer, err := r.navigation, r.navigateErr
	r.mu.Unlock()

	switch {
	case err != nil:
		return sandbox.Navigation{}, err
	case answer != nil:
		return *answer, nil
	default:
		return r.InMemoryRuntime.Navigate(ctx, botID, targetURL)
	}
}

// Purge records the call and forgets the session, unless the test asked for a failure.
func (r *gateRuntime) Purge(ctx context.Context, botID string) error {
	r.mu.Lock()
	r.purges = append(r.purges, botID)
	err := r.purgeErr
	r.mu.Unlock()
	if err != nil {
		return err
	}
	return r.InMemoryRuntime.Purge(ctx, botID)
}

// Stop records the call, so a test can tell the plain DELETE from the purging one.
func (r *gateRuntime) Stop(ctx context.Context, botID string) error {
	r.mu.Lock()
	r.stops = append(r.stops, botID)
	r.mu.Unlock()
	return r.InMemoryRuntime.Stop(ctx, botID)
}

// Cleanup records the flag it was called with — the volumes half is the destructive one
// and must not be assumed — and answers what the test configured.
func (r *gateRuntime) Cleanup(_ context.Context, volumes bool) (sandbox.CleanupResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleans = append(r.cleans, volumes)
	return r.cleanupResult, nil
}

// record keeps one request as the gate saw it.
func (r *gateRuntime) record(request *http.Request) {
	body, _ := io.ReadAll(request.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, gateRequest{
		method:        request.Method,
		path:          request.URL.Path,
		query:         request.URL.RawQuery,
		secret:        request.Header.Get(sandbox.SecretHeader),
		authorization: request.Header.Get("Authorization"),
		cookie:        request.Header.Get("Cookie"),
		body:          string(body),
	})
}

// seen answers the requests the gate has received so far, oldest first.
func (r *gateRuntime) seen() []gateRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]gateRequest(nil), r.requests...)
}

// only answers the single request the gate received, and fails the test when there is
// not exactly one: every route here sends one request, and a second one is a bug worth
// hearing about.
func (r *gateRuntime) only(t *testing.T) gateRequest {
	t.Helper()
	requests := r.seen()
	if len(requests) != 1 {
		t.Fatalf("the gate received %d requests, want exactly 1: %+v", len(requests), requests)
	}
	return requests[0]
}

// TestNoVNCProxyForwardsTheDesktopAndTradesTheTokenForACookie covers the whole noVNC
// path: the page, the token trade, and the websocket upgrade that carries the
// framebuffer.
//
// A browser cannot send an Authorization header for a page load or a websocket
// handshake, so the URL carries the token once and the answer sets a cookie scoped to
// this API's session paths. Neither the token nor the cookie may reach the sandbox:
// the desktop needs no credentials, and the sandbox is the one place in this system
// that runs code a model asked for. The sandbox's own secret is the opposite case — the
// gate refuses anything without it, so the control plane has to attach it here.
func TestNoVNCProxyForwardsTheDesktopAndTradesTheTokenForACookie(t *testing.T) {
	upgraded := make(chan string, 4)

	runtime := newGateRuntime("")
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runtime.record(r)
		if r.URL.Path != "/novnc/websockify" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "novnc page")
			return
		}

		upgraded <- r.Header.Get("Upgrade")
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = conn.Close()
	}))
	t.Cleanup(gate.Close)
	runtime.target = gate.URL

	if _, err := runtime.Ensure(t.Context(), "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	control := httptest.NewServer(New(runtime, log.New(io.Discard, "", 0), WithToken(testToken)))
	t.Cleanup(control.Close)

	// The page load carries the token in the URL and no Authorization header.
	page := control.URL + "/v1/sessions/bot-a/novnc/vnc.html?autoconnect=true&resize=scale&token=" + testToken
	response, err := http.Get(page)
	if err != nil {
		t.Fatalf("get the novnc page: %v", err)
	}
	payload, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("read the novnc page: %v", err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(payload), "novnc page") {
		t.Fatalf("page: status = %d, body = %s", response.StatusCode, payload)
	}

	cookie := findCookie(response.Cookies(), tokenCookie)
	if cookie == nil {
		t.Fatalf("cookies = %v, want %s to be set", response.Cookies(), tokenCookie)
	}
	if !cookie.HttpOnly || cookie.Path != tokenCookieDir || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie = %+v, want an HttpOnly SameSite=Strict cookie scoped to %s", cookie, tokenCookieDir)
	}

	first := runtime.only(t)
	// The gate publishes noVNC under /novnc, so the door is added and the route prefix
	// the caller used is taken off: what the gate sees is its own path and nothing about
	// this API's layout.
	if first.path != "/novnc/vnc.html" {
		t.Fatalf("proxied path = %q, want the route prefix replaced by the gate's door", first.path)
	}
	if first.method != http.MethodGet {
		t.Fatalf("proxied method = %q, want GET", first.method)
	}
	if strings.Contains(first.query, "token") || first.cookie != "" || first.authorization != "" {
		t.Fatalf("the control plane's credentials reached the sandbox: query = %q, cookie = %q, authorization = %q",
			first.query, first.cookie, first.authorization)
	}
	if first.secret != runtime.secret {
		t.Fatalf("secret at the gate = %q, want %q in %s", first.secret, runtime.secret, sandbox.SecretHeader)
	}

	// The websocket upgrade: a raw connection, because a browser's is raw too. It
	// presents the cookie and no query token, and the gate has to see the upgrade.
	address := strings.TrimPrefix(control.URL, "http://")
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("dial the control plane: %v", err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn,
		"GET /v1/sessions/bot-a/novnc/websockify HTTP/1.1\r\nHost: %s\r\nCookie: %s=%s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n",
		address, tokenCookie, testToken); err != nil {
		t.Fatalf("write the upgrade request: %v", err)
	}

	statusLine, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read the upgrade answer: %v", err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("upgrade answer = %q, want 101", strings.TrimSpace(statusLine))
	}
	if got := <-upgraded; got != "websocket" {
		t.Fatalf("upgrade header at the sandbox = %q, want websocket", got)
	}

	// The upgrade is a second request, and it carries the same two facts as the first:
	// the sandbox's secret, and none of the control plane's own credentials.
	requests := runtime.seen()
	if len(requests) != 2 {
		t.Fatalf("the gate received %d requests, want the page and the upgrade: %+v", len(requests), requests)
	}
	upgrade := requests[1]
	if upgrade.path != "/novnc/websockify" {
		t.Fatalf("upgraded path = %q, want /novnc/websockify", upgrade.path)
	}
	if upgrade.secret != runtime.secret || upgrade.cookie != "" || upgrade.authorization != "" {
		t.Fatalf("the upgrade carried secret = %q, cookie = %q, authorization = %q",
			upgrade.secret, upgrade.cookie, upgrade.authorization)
	}
}

// TestNoVNCProxyAnswers502WhenTheDesktopIsUnreachable is the honest answer for a
// session whose sandbox is not there: the route exists, the sandbox is not answering,
// and that is not the caller's mistake.
func TestNoVNCProxyAnswers502WhenTheDesktopIsUnreachable(t *testing.T) {
	runtime := newGateRuntime("http://127.0.0.1:1")
	if _, err := runtime.Ensure(t.Context(), "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	server := httptest.NewServer(New(runtime, log.New(io.Discard, "", 0), WithToken(testToken)))
	t.Cleanup(server.Close)

	status, body := do(t, http.MethodGet, server.URL+"/v1/sessions/bot-a/novnc/vnc.html", "")
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s, want 502", status, body)
	}
	if !strings.Contains(string(body), "not reachable") {
		t.Fatalf("body = %s, want the sentence a caller branches on", body)
	}
}

// TestCDPProxyForwardsTheDevToolsAPIToTheGate covers the DevTools door: the same proxy
// as noVNC, under the gate's /cdp prefix, with the sandbox's secret attached and the
// control plane's own credentials stripped.
func TestCDPProxyForwardsTheDevToolsAPIToTheGate(t *testing.T) {
	runtime := newGateRuntime("")
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runtime.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"target-1","type":"page"}`)
	}))
	t.Cleanup(gate.Close)
	runtime.target = gate.URL

	if _, err := runtime.Ensure(t.Context(), "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	server := httptest.NewServer(New(runtime, log.New(io.Discard, "", 0), WithToken(testToken)))
	t.Cleanup(server.Close)

	// The door is a thin proxy: the method, the path under the gate's own prefix, and the
	// query string all travel as they arrived. `?<url>` in particular is the DevTools
	// call's own argument — Chromium takes the address it should open as the query string
	// itself — so a control plane that parsed it as a parameter would drop it.
	//
	// PUT, not POST, because that is what /json/new is: the DevTools HTTP API is not
	// method-uniform (list and version are GETs, new is a PUT, close is a GET), so this
	// door names no method and forwards whatever arrives. A mux pattern that listed
	// methods would answer 405 here, and the caller would be told it used the wrong
	// verb for a request it is entitled to make.
	status, body := do(t, http.MethodPut, server.URL+"/v1/sessions/bot-a/cdp/json/new?url=https%3A%2F%2Fexample.com%2Friset", "")
	if status != http.StatusOK {
		t.Fatalf("new target: status = %d, body = %s", status, body)
	}
	if !strings.Contains(string(body), "target-1") {
		t.Fatalf("body = %s, want the gate's answer passed through", body)
	}

	request := runtime.only(t)
	if request.method != http.MethodPut {
		t.Fatalf("proxied method = %q, want PUT", request.method)
	}
	if request.path != "/cdp/json/new" {
		t.Fatalf("proxied path = %q, want the gate's own prefix", request.path)
	}
	query, err := url.ParseQuery(request.query)
	if err != nil {
		t.Fatalf("parse the proxied query %q: %v", request.query, err)
	}
	if query.Get("url") != "https://example.com/riset" {
		t.Fatalf("proxied query = %q, want the url Chromium was asked to open", request.query)
	}
	if request.secret != runtime.secret {
		t.Fatalf("secret at the gate = %q, want %q", request.secret, runtime.secret)
	}
	if request.authorization != "" || request.cookie != "" {
		t.Fatalf("the control plane's credentials reached the sandbox: authorization = %q, cookie = %q",
			request.authorization, request.cookie)
	}
}

// TestCDPProxyServesTheBareDoor: Session.CDPURL is exactly /v1/sessions/<bot>/cdp, with
// nothing under it, so that path has to answer as a door rather than redirect to one. A
// caller handed that address and then following a 301 would be following a redirect on a
// websocket handshake, which is not a thing a browser does.
func TestCDPProxyServesTheBareDoor(t *testing.T) {
	runtime := newGateRuntime("")
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runtime.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"Browser":"Chrome/120"}`)
	}))
	t.Cleanup(gate.Close)
	runtime.target = gate.URL

	if _, err := runtime.Ensure(t.Context(), "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	server := httptest.NewServer(New(runtime, log.New(io.Discard, "", 0), WithToken(testToken)))
	t.Cleanup(server.Close)

	status, body := do(t, http.MethodGet, server.URL+"/v1/sessions/bot-a/cdp", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want the gate's own answer", status, body)
	}
	if !strings.Contains(string(body), "Chrome") {
		t.Fatalf("body = %s, want the gate's answer passed through", body)
	}

	// The gate's door, with nothing under it but the slash that separates the two. The
	// gate takes either form — its own tests pin the bare one — and the trailing slash
	// is what joinPath leaves when the remainder is empty.
	request := runtime.only(t)
	if request.path != "/cdp/" {
		t.Fatalf("proxied path = %q, want %q: the gate's door, with nothing under it", request.path, "/cdp/")
	}
	if request.secret != runtime.secret {
		t.Fatalf("secret at the gate = %q, want %q", request.secret, runtime.secret)
	}
}

// TestCDPProxyRewritesTheDebuggerAddress is the reason the DevTools door is not a plain
// proxy: Chromium names its own loopback address for itself, which is inside the sandbox
// network and dialable by nothing outside it. The path is kept, because it names the
// target, and the address becomes this route on the host the caller used.
func TestCDPProxyRewritesTheDebuggerAddress(t *testing.T) {
	runtime := newGateRuntime("")
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runtime.record(r)
		w.Header().Set("Content-Type", "application/json")
		// The shape Chromium answers /json/list with, addresses and all.
		_, _ = io.WriteString(w, `[
			{"id":"target-1","type":"page","url":"https://example.com/",
			 "webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/page/target-1"},
			{"id":"target-2","type":"page","url":"about:blank",
			 "webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/page/target-2"}
		]`)
	}))
	t.Cleanup(gate.Close)
	runtime.target = gate.URL

	if _, err := runtime.Ensure(t.Context(), "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	server := httptest.NewServer(New(runtime, log.New(io.Discard, "", 0), WithToken(testToken)))
	t.Cleanup(server.Close)

	status, body := do(t, http.MethodGet, server.URL+"/v1/sessions/bot-a/cdp/json/list", "")
	if status != http.StatusOK {
		t.Fatalf("list: status = %d, body = %s", status, body)
	}
	request := runtime.only(t)
	if request.path != "/cdp/json/list" {
		t.Fatalf("proxied path = %q, want /cdp/json/list", request.path)
	}
	if request.secret != runtime.secret {
		t.Fatalf("secret at the gate = %q, want %q", request.secret, runtime.secret)
	}

	var targets []struct {
		ID                string `json:"id"`
		WebSocketDebugger string `json:"webSocketDebuggerUrl"`
		URL               string `json:"url"`
	}
	if err := json.Unmarshal(body, &targets); err != nil {
		t.Fatalf("decode the target list: %v (body = %s)", err, body)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %+v, want both", targets)
	}
	// The address the caller is handed has to be one it can actually dial: this host,
	// this route, and the path that names the target.
	host := strings.TrimPrefix(server.URL, "http://")
	for i, want := range []string{
		"ws://" + host + "/v1/sessions/bot-a/cdp/devtools/page/target-1",
		"ws://" + host + "/v1/sessions/bot-a/cdp/devtools/page/target-2",
	} {
		if targets[i].WebSocketDebugger != want {
			t.Fatalf("target %d debugger url = %q, want %q", i, targets[i].WebSocketDebugger, want)
		}
	}
	// Nothing else in the answer is touched: the target's own url is not a debugger
	// address, and rewriting it would hand the caller a page that does not exist.
	if targets[0].URL != "https://example.com/" {
		t.Fatalf("target url = %q, want it unchanged", targets[0].URL)
	}
}

// TestCDPProxyPassesThroughWhatItDoesNotUnderstand keeps the rewrite honest: a body that
// is not JSON, and a JSON body with no debugger address in it, must arrive byte for byte.
// A rewrite that touched either would be rewriting something it does not understand.
func TestCDPProxyPassesThroughWhatItDoesNotUnderstand(t *testing.T) {
	answers := map[string]struct {
		contentType string
		body        string
	}{
		"not json":             {"text/plain", "DevTools listening on ws://127.0.0.1:9222/devtools/browser/abc\n"},
		"json with no address": {"application/json", `{"Browser":"Chrome/120.0","Protocol-Version":"1.3"}`},
	}

	for name, answer := range answers {
		t.Run(name, func(t *testing.T) {
			runtime := newGateRuntime("")
			gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				runtime.record(r)
				w.Header().Set("Content-Type", answer.contentType)
				_, _ = io.WriteString(w, answer.body)
			}))
			t.Cleanup(gate.Close)
			runtime.target = gate.URL

			if _, err := runtime.Ensure(t.Context(), "bot-a"); err != nil {
				t.Fatalf("ensure: %v", err)
			}
			server := httptest.NewServer(New(runtime, log.New(io.Discard, "", 0), WithToken(testToken)))
			t.Cleanup(server.Close)

			status, body := do(t, http.MethodGet, server.URL+"/v1/sessions/bot-a/cdp/json/version", "")
			if status != http.StatusOK {
				t.Fatalf("version: status = %d, body = %s", status, body)
			}
			if string(body) != answer.body {
				t.Fatalf("body = %q, want %q untouched", body, answer.body)
			}
		})
	}
}

// TestCDPProxyAnswers502WhenTheGateRefuses is the same honest answer as the noVNC case,
// for the other failure a caller can meet: a gate that is listening but does not accept
// the secret it was sent, which is what a session whose secret was rotated out of its
// container looks like.
func TestCDPProxyAnswers502WhenTheGateRefuses(t *testing.T) {
	refusals := make(chan string, 4)

	runtime := newGateRuntime("")
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refusals <- r.Header.Get(sandbox.SecretHeader)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
	}))
	t.Cleanup(gate.Close)
	runtime.target = gate.URL

	if _, err := runtime.Ensure(t.Context(), "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	server := httptest.NewServer(New(runtime, log.New(io.Discard, "", 0), WithToken(testToken)))
	t.Cleanup(server.Close)

	status, body := do(t, http.MethodGet, server.URL+"/v1/sessions/bot-a/cdp/json/list", "")
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s, want 502", status, body)
	}
	// The gate's own error is not forwarded: it is about the sandbox, not about the
	// request, and the caller can do nothing with it. What the control plane says is that
	// it could not reach a browser, which is the caller's whole view of the sandbox.
	var failure map[string]any
	if err := json.Unmarshal(body, &failure); err != nil {
		t.Fatalf("decode the refusal: %v (body = %s)", err, body)
	}
	if failure["error"] != "the session's browser is not reachable" {
		t.Fatalf("error = %v, want the one sentence this surface answers", failure["error"])
	}
	if got := <-refusals; got != runtime.secret {
		t.Fatalf("secret presented at the gate = %q, want %q: the refusal is the gate's answer, not a missing header", got, runtime.secret)
	}
}

// TestCDPProxyAnswers502WhenTheGateIsNotListening is the other half: nothing is at the
// address at all.
func TestCDPProxyAnswers502WhenTheGateIsNotListening(t *testing.T) {
	runtime := newGateRuntime("http://127.0.0.1:1")
	if _, err := runtime.Ensure(t.Context(), "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	server := httptest.NewServer(New(runtime, log.New(io.Discard, "", 0), WithToken(testToken)))
	t.Cleanup(server.Close)

	status, body := do(t, http.MethodGet, server.URL+"/v1/sessions/bot-a/cdp/json/version", "")
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s, want 502", status, body)
	}
	if !strings.Contains(string(body), "the session's browser is not reachable") {
		t.Fatalf("body = %s, want the sentence a caller branches on", body)
	}
}

// TestNoVNCProxyAnswers502WhenTheGateRefusesTheSecret is the noVNC half of the same
// failure the DevTools door reports: the gate is listening and does not accept the secret
// the control plane itself attached, which is what a session whose secret no longer
// matches its container looks like.
//
// The gate's own answer is not forwarded. A 401 from behind this proxy would tell the
// caller its control token is wrong, and the caller would go and rotate the wrong
// credential; the sandbox is what failed, so the sandbox is what the answer names.
func TestNoVNCProxyAnswers502WhenTheGateRefusesTheSecret(t *testing.T) {
	secrets := make(chan string, 4)

	runtime := newGateRuntime("")
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secrets <- r.Header.Get(sandbox.SecretHeader)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
	}))
	t.Cleanup(gate.Close)
	runtime.target = gate.URL

	if _, err := runtime.Ensure(t.Context(), "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	server := httptest.NewServer(New(runtime, log.New(io.Discard, "", 0), WithToken(testToken)))
	t.Cleanup(server.Close)

	status, body := do(t, http.MethodGet, server.URL+"/v1/sessions/bot-a/novnc/vnc.html", "")
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s, want 502", status, body)
	}
	if !strings.Contains(string(body), "the session's browser is not reachable") {
		t.Fatalf("body = %s, want the sentence this surface answers for an unreachable sandbox", body)
	}
	if got := <-secrets; got != runtime.secret {
		t.Fatalf("secret presented at the gate = %q, want %q", got, runtime.secret)
	}
}

// TestProxyRoutesOnAnUnknownBotAre404 keeps the proxy routes from inventing a sandbox:
// the runtime is what says there is no session, and the answer is the same 404 every
// other per-bot route gives.
func TestProxyRoutesOnAnUnknownBotAre404(t *testing.T) {
	server, _ := newTestServer(t)

	for _, path := range []string{"/cdp", "/cdp/json/list", "/novnc/vnc.html"} {
		status, body := do(t, http.MethodGet, server.URL+"/v1/sessions/nobody"+path, "")
		if status != http.StatusNotFound {
			t.Fatalf("%s: status = %d, body = %s, want 404", path, status, body)
		}
	}
}

// TestPurgeRemovesTheSessionAndItsVolume covers the destructive half of the lifecycle as
// a caller reaches it: `?purge=true` on the delete route. It is the same route as stop
// because it is the same intent, with one flag deciding whether the browser profile
// survives — which is why the test pins that the flag is what chooses between the two
// runtime calls and not the route.
func TestPurgeRemovesTheSessionAndItsVolume(t *testing.T) {
	runtime := newGateRuntime("http://127.0.0.1:1")
	if _, err := runtime.Ensure(t.Context(), "bot-purge"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	server := httptest.NewServer(New(runtime, log.New(io.Discard, "", 0), WithToken(testToken)))
	t.Cleanup(server.Close)

	status, body := do(t, http.MethodDelete, server.URL+"/v1/sessions/bot-purge?purge=true", "")
	if status != http.StatusOK {
		t.Fatalf("purge: status = %d, body = %s", status, body)
	}
	var answer map[string]any
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decode the purge answer: %v (body = %s)", err, body)
	}
	if answer["stopped"] != true || answer["purged"] != true {
		t.Fatalf("answer = %v, want stopped and purged", answer)
	}
	if len(runtime.purges) != 1 || runtime.purges[0] != "bot-purge" {
		t.Fatalf("purges = %v, want one purge of bot-purge", runtime.purges)
	}
	if len(runtime.stops) != 0 {
		t.Fatalf("stops = %v, want none: a purge is not a stop", runtime.stops)
	}
	// The session is really gone, and not merely reported as gone.
	if _, err := runtime.Status(t.Context(), "bot-purge"); !errors.Is(err, sandbox.ErrNoSession) {
		t.Fatalf("status after purge: err = %v, want ErrNoSession", err)
	}
}

// TestStopDoesNotPurge is the other side of the flag: a plain DELETE keeps the volume, so
// the next ensure reuses the browser profile. A route that purged on both would silently
// throw away every login a bot has.
func TestStopDoesNotPurge(t *testing.T) {
	runtime := newGateRuntime("http://127.0.0.1:1")
	if _, err := runtime.Ensure(t.Context(), "bot-stop"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	server := httptest.NewServer(New(runtime, log.New(io.Discard, "", 0), WithToken(testToken)))
	t.Cleanup(server.Close)

	status, body := do(t, http.MethodDelete, server.URL+"/v1/sessions/bot-stop", "")
	if status != http.StatusOK {
		t.Fatalf("stop: status = %d, body = %s", status, body)
	}
	var answer map[string]any
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decode the stop answer: %v (body = %s)", err, body)
	}
	if answer["stopped"] != true {
		t.Fatalf("answer = %v, want stopped", answer)
	}
	if _, ok := answer["purged"]; ok {
		t.Fatalf("answer = %v, want no purged key: nothing was purged", answer)
	}
	if len(runtime.purges) != 0 {
		t.Fatalf("purges = %v, want none", runtime.purges)
	}
	if len(runtime.stops) != 1 || runtime.stops[0] != "bot-stop" {
		t.Fatalf("stops = %v, want one stop of bot-stop", runtime.stops)
	}
}

// TestCleanupRouteReportsWhatItRemoved covers the one route that is not about a single
// bot. The two counts are what an operator reads, and `failed` is the list of what was
// selected and could not be removed — a key that is absent when nothing failed, so a
// caller can tell "everything is gone" from "some of it is not" without reading a list.
func TestCleanupRouteReportsWhatItRemoved(t *testing.T) {
	t.Run("nothing failed", func(t *testing.T) {
		runtime := newGateRuntime("http://127.0.0.1:1")
		runtime.cleanupResult = sandbox.CleanupResult{RemovedContainers: 2, RemovedVolumes: 1}
		server := httptest.NewServer(New(runtime, log.New(io.Discard, "", 0), WithToken(testToken)))
		t.Cleanup(server.Close)

		status, body := do(t, http.MethodPost, server.URL+"/v1/admin/cleanup", "")
		if status != http.StatusOK {
			t.Fatalf("cleanup: status = %d, body = %s", status, body)
		}
		var answer map[string]any
		if err := json.Unmarshal(body, &answer); err != nil {
			t.Fatalf("decode the cleanup answer: %v (body = %s)", err, body)
		}
		if answer["removed_containers"] != float64(2) || answer["removed_volumes"] != float64(1) {
			t.Fatalf("answer = %v, want the two counts", answer)
		}
		if _, ok := answer["failed"]; ok {
			t.Fatalf("answer = %v, want no failed key when nothing failed", answer)
		}
		// The default is the conservative half: containers, not the home volumes that
		// hold every bot's logins.
		if len(runtime.cleans) != 1 || runtime.cleans[0] {
			t.Fatalf("cleanup calls = %v, want one call without volumes", runtime.cleans)
		}
	})

	t.Run("some of it failed", func(t *testing.T) {
		runtime := newGateRuntime("http://127.0.0.1:1")
		runtime.cleanupResult = sandbox.CleanupResult{
			RemovedContainers: 1,
			Failed:            []string{"volume vcomputer-bot-a-home: still in use"},
		}
		server := httptest.NewServer(New(runtime, log.New(io.Discard, "", 0), WithToken(testToken)))
		t.Cleanup(server.Close)

		status, body := do(t, http.MethodPost, server.URL+"/v1/admin/cleanup?volumes=true", "")
		if status != http.StatusOK {
			t.Fatalf("cleanup: status = %d, body = %s", status, body)
		}
		var answer struct {
			RemovedContainers int      `json:"removed_containers"`
			RemovedVolumes    int      `json:"removed_volumes"`
			Failed            []string `json:"failed"`
		}
		if err := json.Unmarshal(body, &answer); err != nil {
			t.Fatalf("decode the cleanup answer: %v (body = %s)", err, body)
		}
		if answer.RemovedContainers != 1 || answer.RemovedVolumes != 0 {
			t.Fatalf("answer = %+v, want the counts that actually happened", answer)
		}
		if len(answer.Failed) != 1 || !strings.Contains(answer.Failed[0], "still in use") {
			t.Fatalf("failed = %v, want the line the runtime reported", answer.Failed)
		}
		// `volumes=true` is what makes the more destructive half happen, and it has to
		// reach the runtime: a route that dropped it would look like a successful
		// cleanup that removed nothing.
		if len(runtime.cleans) != 1 || !runtime.cleans[0] {
			t.Fatalf("cleanup calls = %v, want one call with volumes", runtime.cleans)
		}
	})
}

// TestHealth is what the compose healthcheck and the e2e script read.
func TestHealth(t *testing.T) {
	server, _ := newTestServer(t)

	status, body := do(t, http.MethodGet, server.URL+"/health", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	var health map[string]any
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if health["status"] != "ok" {
		t.Fatalf("health = %v", health)
	}
}

// TestValidToken covers the rule the control plane applies at startup: a token is
// presented as a header, a query parameter, and a cookie, and a value that a cookie
// cannot hold would fail silently in a browser.
func TestValidToken(t *testing.T) {
	for _, token := range []string{"a", "dev-token", "AZaz09-_.~!@#$%^&*()+=[]{}|:<>?/", strings.Repeat("x", 200)} {
		if !ValidToken(token) {
			t.Fatalf("ValidToken(%q) = false, want true", token)
		}
	}
	for _, token := range []string{"", " ", "two words", "semi;colon", "quote\"d", "back\\slash", "com,ma", "new\nline", "tab\t"} {
		if ValidToken(token) {
			t.Fatalf("ValidToken(%q) = true, want false", token)
		}
	}
}

// findCookie returns the named cookie, or nil.
func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}
