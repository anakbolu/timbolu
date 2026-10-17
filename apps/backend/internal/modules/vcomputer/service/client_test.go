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

// fakeToken is the shared secret the fake accepts, standing in for the value of
// VCOMPUTER_CONTROL_TOKEN on the control plane's side.
const fakeToken = "test-token"

// fakeControl is the control plane as this client sees it: a few routes that record
// what arrived and answer what the real one answers. It is a fake of the *contract*,
// not of the implementation, so a change in the client's paths or bodies fails here —
// and like the real one it refuses every /v1 route that arrives without the bearer.
type fakeControl struct {
	*httptest.Server
	requests []recorded
}

type recorded struct {
	method string
	path   string
	body   map[string]any
	// authorization is the header as it arrived. It is recorded rather than only
	// checked so a test can show which secret the client sent.
	authorization string
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
	mux.HandleFunc("POST /v1/sessions/{bot_id}/navigate", func(w http.ResponseWriter, r *http.Request) {
		body := fake.record(t, r)
		target, _ := body["url"].(string)
		if target == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a url is required"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"url": target})
	})
	mux.HandleFunc("POST /v1/sessions/{bot_id}/exec", func(w http.ResponseWriter, r *http.Request) {
		body := fake.record(t, r)
		command, _ := body["command"].(string)
		if command == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a command is required"})
			return
		}
		if command == "gone" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no session for that bot"})
			return
		}
		writeJSON(w, http.StatusOK, domain.ExecResult{Command: command, Stdout: "stub runtime: not executed", Stub: true})
	})

	// Every /v1 route needs the shared secret, exactly as the control plane does. The
	// check sits in front of the mux rather than in each handler so that a client which
	// forgot the header fails every test here, not only the one that looks for it.
	authorized := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeToken {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		mux.ServeHTTP(w, r)
	})

	fake.Server = httptest.NewServer(authorized)
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
	f.requests = append(f.requests, recorded{
		method:        r.Method,
		path:          r.URL.Path,
		body:          body,
		authorization: r.Header.Get("Authorization"),
	})
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
	client := New(fake.URL, fakeToken, time.Second)

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
	client := New(fake.URL, fakeToken, time.Second)

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
	client := New(fake.URL, fakeToken, time.Second)
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

// TestNavigateCarriesTheURL covers the one call that points the browser somewhere: the
// URL is the whole payload, and whether its scheme is allowed is the control plane's
// decision rather than this client's.
func TestNavigateCarriesTheURL(t *testing.T) {
	fake := newFakeControl(t)
	client := New(fake.URL, fakeToken, time.Second)

	if err := client.Navigate(context.Background(), "bot-a", "http://vcomputer-fixture:8080/"); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	if len(fake.requests) != 1 {
		t.Fatalf("requests = %+v, want one", fake.requests)
	}
	sent := fake.requests[0]
	if sent.method != http.MethodPost || sent.path != "/v1/sessions/bot-a/navigate" {
		t.Fatalf("request = %+v, want POST /v1/sessions/bot-a/navigate", sent)
	}
	if sent.body["url"] != "http://vcomputer-fixture:8080/" {
		t.Fatalf("body = %+v, want the url that was asked for", sent.body)
	}
}

// TestEveryRequestCarriesTheBearerToken is the seam the control plane closes: it
// refuses every /v1 route without the shared secret, so a client that forgot the
// header would fail every call at once. The screenshot is in the list on purpose —
// it builds its own request instead of going through do.
func TestEveryRequestCarriesTheBearerToken(t *testing.T) {
	fake := newFakeControl(t)
	client := New(fake.URL, fakeToken, time.Second)
	ctx := context.Background()

	if _, err := client.EnsureSession(ctx, "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := client.Screenshot(ctx, "bot-a"); err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	if err := client.Navigate(ctx, "bot-a", "http://vcomputer-fixture:8080/"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := client.StopSession(ctx, "bot-a"); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if len(fake.requests) != 4 {
		t.Fatalf("requests = %+v, want four", fake.requests)
	}
	for _, sent := range fake.requests {
		if sent.authorization != "Bearer "+fakeToken {
			t.Fatalf("%s %s: Authorization = %q, want the bearer token", sent.method, sent.path, sent.authorization)
		}
	}
}

// TestStopSessionUsesDelete covers the half of the lifecycle that ends a session.
func TestStopSessionUsesDelete(t *testing.T) {
	fake := newFakeControl(t)
	client := New(fake.URL, fakeToken, time.Second)

	if err := client.StopSession(context.Background(), "bot-a"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if fake.requests[0].method != http.MethodDelete {
		t.Fatalf("method = %s, want DELETE", fake.requests[0].method)
	}
}

// TestStatusCodesBecomeSentinels is the mapping the caller branches on: a missing
// session is created, a refused token is a configuration mistake, and a request the
// control plane would not take is the caller's own fault. Flattening them into one
// error is how a task retries something that will never succeed.
func TestStatusCodesBecomeSentinels(t *testing.T) {
	fake := newFakeControl(t)
	client := New(fake.URL, fakeToken, time.Second)
	ctx := context.Background()

	if _, err := client.Exec(ctx, "bot-a", ""); !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("empty command: err = %v, want ErrInvalidInput", err)
	}
	if _, err := client.Exec(ctx, "bot-a", "gone"); !errors.Is(err, domain.ErrNoSession) {
		t.Fatalf("missing session: err = %v, want ErrNoSession", err)
	}

	// A wrong token is a deployment mistake, not a transient failure: the control plane
	// refuses it exactly as it refuses a missing one, and retrying would send the same
	// rejected header again.
	misconfigured := New(fake.URL, "not-the-token", time.Second)
	_, err := misconfigured.EnsureSession(ctx, "bot-a")
	if !errors.Is(err, domain.ErrNotConfigured) {
		t.Fatalf("refused token: err = %v, want ErrNotConfigured", err)
	}

	// A refusal the control plane wrote is carried through, not replaced: the caller
	// needs the sentence to show a person.
	if !bytes.Contains([]byte(err.Error()), []byte("unauthorized")) {
		t.Fatalf("err = %v, want the control plane's message", err)
	}
}

// TestUnreachableControlPlaneIsRetryable covers the difference between "no" and
// "not now": a closed listener is ErrUnavailable, not a refusal.
func TestUnreachableControlPlaneIsRetryable(t *testing.T) {
	fake := newFakeControl(t)
	url := fake.URL
	fake.Close()

	client := New(url, fakeToken, time.Second)
	if _, err := client.EnsureSession(context.Background(), "bot-a"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

// TestUnconfiguredClientRefuses covers the deployment that never set an address: it
// must be told so, and it must not be told by a panic or a request to a URL nobody
// meant.
func TestUnconfiguredClientRefuses(t *testing.T) {
	client := New("", fakeToken, time.Second)

	if _, err := client.EnsureSession(context.Background(), "bot-a"); !errors.Is(err, domain.ErrNotConfigured) {
		t.Fatalf("ensure: err = %v, want ErrNotConfigured", err)
	}
	if _, err := client.Screenshot(context.Background(), "bot-a"); !errors.Is(err, domain.ErrNotConfigured) {
		t.Fatalf("screenshot: err = %v, want ErrNotConfigured", err)
	}
	if err := client.Click(context.Background(), "bot-a", 1, 2); !errors.Is(err, domain.ErrNotConfigured) {
		t.Fatalf("click: err = %v, want ErrNotConfigured", err)
	}
	if err := client.Navigate(context.Background(), "bot-a", "http://example.com/"); !errors.Is(err, domain.ErrNotConfigured) {
		t.Fatalf("navigate: err = %v, want ErrNotConfigured", err)
	}
}

// TestEffectiveTimeoutIsTheBoundACallReallyGets: a zero or negative timeout is not a bound —
// an http.Client built with one waits forever — so it is replaced rather than honored, and the
// replacement is reported by the same function the container logs from, which is what keeps a
// startup line from naming a timeout no call waits (issue #257).
func TestEffectiveTimeoutIsTheBoundACallReallyGets(t *testing.T) {
	cases := map[string]struct {
		configured time.Duration
		want       time.Duration
	}{
		"negative": {-5 * time.Second, DefaultTimeout},
		"zero":     {0, DefaultTimeout},
		"positive": {90 * time.Second, 90 * time.Second},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if got := EffectiveTimeout(testCase.configured); got != testCase.want {
				t.Fatalf("EffectiveTimeout(%s) = %s, want %s", testCase.configured, got, testCase.want)
			}

			client := New("http://vcomputer-control:8090", fakeToken, testCase.configured)
			if got := client.client.Timeout; got != testCase.want {
				t.Fatalf("client timeout = %s, want %s", got, testCase.want)
			}
		})
	}
}
