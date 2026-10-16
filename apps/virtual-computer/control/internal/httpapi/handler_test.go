// Package httpapi's tests drive the real handler over a real listener, with the
// in-memory runtime behind it. Nothing is mocked: the point of the PoC is that the
// HTTP surface works end to end, so the test asserts what a caller sees.
package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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
	}
	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			status, body := doAs(t, "", route.method, server.URL+route.path, "")
			if status != http.StatusUnauthorized {
				t.Fatalf("without a token: status = %d, body = %s, want 401", status, body)
			}
			// The sentence is the one a caller repeats to a person, and it is the
			// one that says no credential arrived at all rather than that the one
			// that arrived is wrong (issue #218).
			if !strings.Contains(string(body), "a bearer token is required") {
				t.Fatalf("body = %s, want it to name the missing token", body)
			}
		})
	}

	// A wrong token is refused the same way, and the sentence says which of the two
	// mistakes it is: the caller is a worker that reports this to a person, and "a
	// bearer token is required" for a token that was sent and merely wrong sends them
	// looking for a variable they did set. A token in the query string is only accepted
	// on the noVNC route: it reaches logs and history, so it is not a second way to call
	// the API.
	status, body := doAs(t, "wrong-token", http.MethodGet, server.URL+"/v1/sessions/bot-a", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d, want 401", status)
	}
	if !strings.Contains(string(body), "not valid") || strings.Contains(string(body), "required") {
		t.Fatalf("body = %s, want a wrong token reported as invalid rather than missing", body)
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

// proxyRuntime is the stub with one difference: the address its desktop lives at is
// one the test owns, so the noVNC proxy is exercised against a real server instead of
// against a container name that does not resolve here.
type proxyRuntime struct {
	*sandbox.InMemoryRuntime
	target string
}

func (r proxyRuntime) NoVNCTarget(context.Context, string) (string, error) { return r.target, nil }

// TestNoVNCProxyForwardsTheDesktopAndTradesTheTokenForACookie covers the whole noVNC
// path: the page, the token trade, and the websocket upgrade that carries the
// framebuffer.
//
// A browser cannot send an Authorization header for a page load or a websocket
// handshake, so the URL carries the token once and the answer sets a cookie scoped to
// this API's session paths. Neither the token nor the cookie may reach the sandbox:
// the desktop needs no credentials, and the sandbox is the one place in this system
// that runs code a model asked for.
func TestNoVNCProxyForwardsTheDesktopAndTradesTheTokenForACookie(t *testing.T) {
	proxied := make(chan *http.Request, 4)
	upgraded := make(chan string, 4)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied <- r
		if r.URL.Path != "/websockify" {
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
	t.Cleanup(backend.Close)

	runtime := proxyRuntime{InMemoryRuntime: sandbox.NewInMemory(), target: backend.URL}
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

	first := <-proxied
	if first.URL.Path != "/vnc.html" {
		t.Fatalf("proxied path = %q, want the prefix stripped", first.URL.Path)
	}
	if first.URL.Query().Get("token") != "" || first.Header.Get("Cookie") != "" || first.Header.Get("Authorization") != "" {
		t.Fatalf("the control plane's credentials reached the sandbox: query = %q, cookie = %q, authorization = %q",
			first.URL.RawQuery, first.Header.Get("Cookie"), first.Header.Get("Authorization"))
	}

	// The websocket upgrade: a raw connection, because a browser's is raw too. It
	// presents the cookie and no query token, and the backend has to see the upgrade.
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
}

// TestNoVNCProxyAnswers502WhenTheDesktopIsUnreachable is the honest answer for a
// session whose sandbox is not there: the route exists, the sandbox is not answering,
// and that is not the caller's mistake.
func TestNoVNCProxyAnswers502WhenTheDesktopIsUnreachable(t *testing.T) {
	runtime := proxyRuntime{InMemoryRuntime: sandbox.NewInMemory(), target: "http://127.0.0.1:1"}
	if _, err := runtime.Ensure(t.Context(), "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	server := httptest.NewServer(New(runtime, log.New(io.Discard, "", 0), WithToken(testToken)))
	t.Cleanup(server.Close)

	status, body := do(t, http.MethodGet, server.URL+"/v1/sessions/bot-a/novnc/vnc.html", "")
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s, want 502", status, body)
	}
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
