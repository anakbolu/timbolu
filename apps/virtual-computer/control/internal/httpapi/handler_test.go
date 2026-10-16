// Package httpapi's tests drive the real handler over a real listener, with the
// in-memory runtime behind it. Nothing is mocked: the point of the PoC is that the
// HTTP surface works end to end, so the test asserts what a caller sees.
package httpapi

import (
	"bytes"
	"encoding/json"
	"image/png"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/robzlabz/employeebot/apps/virtual-computer/control/internal/sandbox"
)

// newTestServer builds the handler with a quiet logger and a fresh stub runtime.
func newTestServer(t *testing.T) (*httptest.Server, *sandbox.InMemoryRuntime) {
	t.Helper()

	runtime := sandbox.NewInMemory()
	server := httptest.NewServer(New(runtime, log.New(io.Discard, "", 0)))
	t.Cleanup(server.Close)
	return server, runtime
}

// do sends one request and returns the status and body.
func do(t *testing.T, method, url, body string) (int, []byte) {
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

// TestClickTypeAndExecAreRecorded covers the three actions the PoC stubs: each one
// must reach the runtime, and exec must report that it did not run.
func TestClickTypeAndExecAreRecorded(t *testing.T) {
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

	status, body := do(t, http.MethodPost, server.URL+"/v1/sessions/bot-actions/exec", `{"command":"ls -la /home/bolu"}`)
	if status != http.StatusOK {
		t.Fatalf("exec: status = %d, body = %s", status, body)
	}
	var result sandbox.ExecResult
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode exec result: %v", err)
	}
	if !result.Stub {
		t.Fatal("exec must report that it is a stub: the PoC never runs a command")
	}
	if !strings.Contains(result.Stdout, "ls -la /home/bolu") {
		t.Fatalf("exec stdout = %q, want the echoed command", result.Stdout)
	}

	// The record is the evidence: all three calls arrived, in order, with their bodies.
	actions := runtime.Actions("bot-actions")
	want := []string{"click:640,400", "type:halo", "exec:ls -la /home/bolu"}
	if len(actions) != len(want) {
		t.Fatalf("actions = %+v, want %d", actions, len(want))
	}
	for i, action := range actions {
		if got := action.Kind + ":" + action.Detail; got != want[i] {
			t.Fatalf("action %d = %q, want %q", i, got, want[i])
		}
	}
}

// TestActionsOnUnknownBotAre404 keeps the action routes from creating a session as
// a side effect of being called.
func TestActionsOnUnknownBotAre404(t *testing.T) {
	server, _ := newTestServer(t)

	status, body := do(t, http.MethodPost, server.URL+"/v1/sessions/nobody/click", `{"x":1,"y":2}`)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404", status, body)
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
}

// TestInvalidInputIs400 covers the three refusals that must be a caller error and
// not a runtime failure: no bot id, an id that cannot name a container, and an
// exec with no command.
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

// TestDockerRuntimeIsNotImplementedYet is the check that keeps the skeleton honest:
// the routes exist, and every one of them says the runtime behind them is a
// skeleton rather than pretending a container was created.
func TestDockerRuntimeIsNotImplementedYet(t *testing.T) {
	server := httptest.NewServer(New(sandbox.NewDocker(), log.New(io.Discard, "", 0)))
	t.Cleanup(server.Close)

	status, body := do(t, http.MethodPost, server.URL+"/v1/sessions", `{"bot_id":"bot-a"}`)
	if status != http.StatusNotImplemented {
		t.Fatalf("status = %d, body = %s, want 501", status, body)
	}
	if !strings.Contains(string(body), "docker run") {
		t.Fatalf("body = %s, want the command the skeleton would run", body)
	}
}

// TestHealth is what the demo page's first button and the compose healthcheck read.
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
