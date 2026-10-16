package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/vcomputer/domain"
)

// fakeControl is the control plane as this client sees it: a few routes that record
// what arrived and answer what the real one answers. It is a fake of the *contract*,
// not of the implementation, so a change in the client's paths or bodies fails here.
type fakeControl struct {
	*httptest.Server
	requests []recorded
}

type recorded struct {
	method string
	path   string
	body   map[string]any
}

func newFakeControl(t *testing.T) *fakeControl {
	t.Helper()

	fake := &fakeControl{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		body := fake.record(t, r)
		botID, _ := body["bot_id"].(string)
		if botID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a bot id is required"})
			return
		}
		writeJSON(w, http.StatusOK, domain.Session{
			BotID:   botID,
			State:   domain.StateRunning,
			Runtime: "memory",
			Volume:  "vcomputer-" + botID + "-home",
		})
	})
	mux.HandleFunc("DELETE /v1/sessions/{bot_id}", func(w http.ResponseWriter, r *http.Request) {
		fake.record(t, r)
		writeJSON(w, http.StatusOK, map[string]bool{"stopped": true})
	})
	mux.HandleFunc("POST /v1/sessions/{bot_id}/screenshot", func(w http.ResponseWriter, r *http.Request) {
		fake.record(t, r)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes(t))
	})
	mux.HandleFunc("POST /v1/sessions/{bot_id}/click", func(w http.ResponseWriter, r *http.Request) {
		fake.record(t, r)
		writeJSON(w, http.StatusOK, map[string]any{"clicked": true})
	})
	mux.HandleFunc("POST /v1/sessions/{bot_id}/type", func(w http.ResponseWriter, r *http.Request) {
		fake.record(t, r)
		writeJSON(w, http.StatusOK, map[string]any{"typed": true})
	})
	mux.HandleFunc("POST /v1/sessions/{bot_id}/exec", func(w http.ResponseWriter, r *http.Request) {
		body := fake.record(t, r)
		command, _ := body["command"].(string)
		if command == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a command is required"})
			return
		}
		if command == "boom" {
			writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "the docker runtime is a skeleton"})
			return
		}
		if command == "gone" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no session for that bot"})
			return
		}
		writeJSON(w, http.StatusOK, domain.ExecResult{Command: command, Stdout: "stub runtime: not executed", Stub: true})
	})

	fake.Server = httptest.NewServer(mux)
	t.Cleanup(fake.Close)
	return fake
}

// record keeps what arrived, so a test can assert the body the client sent rather
// than only that it got an answer.
func (f *fakeControl) record(t *testing.T, r *http.Request) map[string]any {
	t.Helper()

	body := map[string]any{}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			// An empty body is legitimate on the routes that carry nothing.
			body = map[string]any{}
		}
	}
	f.requests = append(f.requests, recorded{method: r.Method, path: r.URL.Path, body: body})
	return body
}

// writeJSON answers one object, the same way the control plane does.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// pngBytes is a real PNG: the client must pass bytes through untouched, and a
// decoder check is the only way to see that it did.
func pngBytes(t *testing.T) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := range 4 {
		img.SetRGBA(x, 0, color.RGBA{R: 0x4c, G: 0x9a, B: 0x7a, A: 0xff})
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// TestEnsureSessionSendsTheBotIDAndReadsTheSession covers the one call a worker makes
// before every task, including the body the control plane expects.
func TestEnsureSessionSendsTheBotIDAndReadsTheSession(t *testing.T) {
	fake := newFakeControl(t)
	client := New(fake.URL, time.Second)

	session, err := client.EnsureSession(context.Background(), "bot-a")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if session.BotID != "bot-a" || session.State != domain.StateRunning {
		t.Fatalf("session = %+v", session)
	}
	if session.Volume != "vcomputer-bot-a-home" {
		t.Fatalf("volume = %q, want the per-bot volume", session.Volume)
	}

	if len(fake.requests) != 1 {
		t.Fatalf("requests = %+v, want one", fake.requests)
	}
	sent := fake.requests[0]
	if sent.method != http.MethodPost || sent.path != "/v1/sessions" {
		t.Fatalf("request = %+v, want POST /v1/sessions", sent)
	}
	if sent.body["bot_id"] != "bot-a" {
		t.Fatalf("body = %+v, want bot_id bot-a", sent.body)
	}
}

// TestScreenshotPassesBytesThrough is the difference between this client and a JSON
// client: the answer is an image, and it must survive the trip byte for byte.
func TestScreenshotPassesBytesThrough(t *testing.T) {
	fake := newFakeControl(t)
	client := New(fake.URL, time.Second)

	captured, err := client.Screenshot(context.Background(), "bot-a")
	if err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	if _, err := png.Decode(bytes.NewReader(captured)); err != nil {
		t.Fatalf("screenshot is not a PNG: %v", err)
	}

	// The route is a POST: it takes a fresh capture, so it is an action rather than a
	// cached resource.
	if fake.requests[0].method != http.MethodPost {
		t.Fatalf("method = %s, want POST", fake.requests[0].method)
	}
	if fake.requests[0].path != "/v1/sessions/bot-a/screenshot" {
		t.Fatalf("path = %s", fake.requests[0].path)
	}
}

// TestClickTypeAndExecCarryTheirBodies covers the three actions, including the one
// whose answer is a struct rather than an acknowledgement.
func TestClickTypeAndExecCarryTheirBodies(t *testing.T) {
	fake := newFakeControl(t)
	client := New(fake.URL, time.Second)
	ctx := context.Background()

	if err := client.Click(ctx, "bot-a", 640, 400); err != nil {
		t.Fatalf("click: %v", err)
	}
	if err := client.Type(ctx, "bot-a", "halo"); err != nil {
		t.Fatalf("type: %v", err)
	}
	result, err := client.Exec(ctx, "bot-a", "ls -la")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}

	if !result.Stub {
		t.Fatal("exec must carry the stub flag: a caller that reads exit_code alone would believe the command ran")
	}
	if result.Command != "ls -la" {
		t.Fatalf("command = %q", result.Command)
	}

	want := []string{"/v1/sessions/bot-a/click", "/v1/sessions/bot-a/type", "/v1/sessions/bot-a/exec"}
	if len(fake.requests) != len(want) {
		t.Fatalf("requests = %+v, want %d", fake.requests, len(want))
	}
	for i, path := range want {
		if fake.requests[i].path != path {
			t.Fatalf("request %d path = %q, want %q", i, fake.requests[i].path, path)
		}
	}
	if fake.requests[0].body["x"] != float64(640) || fake.requests[0].body["y"] != float64(400) {
		t.Fatalf("click body = %+v", fake.requests[0].body)
	}
	if fake.requests[1].body["text"] != "halo" {
		t.Fatalf("type body = %+v", fake.requests[1].body)
	}
}

// TestStopSessionUsesDelete covers the half of the lifecycle that ends a session.
func TestStopSessionUsesDelete(t *testing.T) {
	fake := newFakeControl(t)
	client := New(fake.URL, time.Second)

	if err := client.StopSession(context.Background(), "bot-a"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if fake.requests[0].method != http.MethodDelete {
		t.Fatalf("method = %s, want DELETE", fake.requests[0].method)
	}
}

// TestStatusCodesBecomeSentinels is the mapping the caller branches on: a missing
// session is created, a skeleton runtime is a configuration mistake, and an
// unreachable control plane is worth retrying. Flattening them into one error is how
// a task retries something that will never succeed.
func TestStatusCodesBecomeSentinels(t *testing.T) {
	fake := newFakeControl(t)
	client := New(fake.URL, time.Second)
	ctx := context.Background()

	if _, err := client.Exec(ctx, "bot-a", ""); !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("empty command: err = %v, want ErrInvalidInput", err)
	}
	if _, err := client.Exec(ctx, "bot-a", "gone"); !errors.Is(err, domain.ErrNoSession) {
		t.Fatalf("missing session: err = %v, want ErrNoSession", err)
	}
	if _, err := client.Exec(ctx, "bot-a", "boom"); !errors.Is(err, domain.ErrNotImplemented) {
		t.Fatalf("skeleton runtime: err = %v, want ErrNotImplemented", err)
	}

	// A refusal the control plane wrote is carried through, not replaced: the caller
	// needs the sentence to show a person.
	if _, err := client.Exec(ctx, "bot-a", "boom"); !bytes.Contains([]byte(err.Error()), []byte("skeleton")) {
		t.Fatalf("err = %v, want the control plane's message", err)
	}
}

// TestUnreachableControlPlaneIsRetryable covers the difference between "no" and
// "not now": a closed listener is ErrUnavailable, not a refusal.
func TestUnreachableControlPlaneIsRetryable(t *testing.T) {
	fake := newFakeControl(t)
	url := fake.URL
	fake.Close()

	client := New(url, time.Second)
	if _, err := client.EnsureSession(context.Background(), "bot-a"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

// TestUnconfiguredClientRefuses covers the deployment that never set an address: it
// must be told so, and it must not be told by a panic or a request to a URL nobody
// meant.
func TestUnconfiguredClientRefuses(t *testing.T) {
	client := New("", time.Second)

	if _, err := client.EnsureSession(context.Background(), "bot-a"); !errors.Is(err, domain.ErrNotConfigured) {
		t.Fatalf("ensure: err = %v, want ErrNotConfigured", err)
	}
	if _, err := client.Screenshot(context.Background(), "bot-a"); !errors.Is(err, domain.ErrNotConfigured) {
		t.Fatalf("screenshot: err = %v, want ErrNotConfigured", err)
	}
	if err := client.Click(context.Background(), "bot-a", 1, 2); !errors.Is(err, domain.ErrNotConfigured) {
		t.Fatalf("click: err = %v, want ErrNotConfigured", err)
	}
}
