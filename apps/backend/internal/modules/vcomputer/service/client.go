// Package service is the backend's side of the control plane seam: an HTTP client
// that implements domain.ComputerUseClient.
//
// It talks to apps/virtual-computer/control over the internal Docker network and
// nothing else. The control plane is never published, it speaks for every bot at
// once, and a caller that reached it from the internet could drive any bot's
// desktop (docs/adr/0003-virtual-computer.md §3.6) — so the address is a
// configuration value, and an empty one refuses every call instead of reaching a
// URL nobody meant.
//
// The control plane is not unauthenticated. Every /v1 route requires
// `Authorization: Bearer <token>`, the shared secret from VCOMPUTER_CONTROL_TOKEN,
// and this client puts it on every request it makes; a missing or wrong token comes
// back as a 401. That bearer is the same shared-secret seam the WhatsApp gateway uses
// between the API and the worker, and it is why a refused token is reported as a
// configuration mistake rather than as a failure worth retrying.
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/vcomputer/domain"
)

// maxResponse bounds an answer. A screenshot is the largest thing that crosses this
// seam, and 8 MiB is a generous ceiling for a 1280x800 PNG.
const maxResponse = 8 << 20

// DefaultTimeout bounds one call to the control plane when the configuration does not
// set a positive one. It is generous because a screenshot of a cold sandbox includes
// the container starting: the PoC would rather wait than report a failure that would
// have resolved itself.
//
// It is exported because three places have to agree on it: the client that enforces
// it, the container that logs the timeout in force at startup, and cmd/vcomputer-smoke,
// which reads the same variable a deployment does.
const DefaultTimeout = 30 * time.Second

// EffectiveTimeout is the timeout a call to the control plane really gets: the
// configured one when it is positive, DefaultTimeout otherwise. A zero or negative
// value is not a bound at all — an http.Client with no timeout waits forever — so it
// is replaced rather than honored, and every caller that reports the timeout in force
// asks here instead of repeating the rule and printing a value that is not the one
// being used (issue #257).
func EffectiveTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return DefaultTimeout
	}

	return timeout
}

// Client calls the control plane.
type Client struct {
	baseURL string
	token   string
	client  *http.Client
}

// Compile-time check that the client is the port the module declares.
var _ domain.ComputerUseClient = (*Client)(nil)

// New builds the client. An empty address leaves it unconfigured: every call then
// reports that, which is what a deployment without a virtual computer looks like.
//
// The token is the control plane's shared secret and is sent on every request. An
// empty one is not refused here, because the control plane is the one that knows
// whether it is wrong: it answers 401, which classify turns into ErrNotConfigured.
//
// The timeout is EffectiveTimeout, so a deployment that configured zero or a negative
// duration is bounded by DefaultTimeout rather than left unbounded.
func New(baseURL, token string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   strings.TrimSpace(token),
		client:  &http.Client{Timeout: EffectiveTimeout(timeout)},
	}
}

// EnsureSession creates the session if it is not there and returns it either way.
func (c *Client) EnsureSession(ctx context.Context, botID string) (domain.Session, error) {
	var session domain.Session
	err := c.do(ctx, http.MethodPost, "/v1/sessions", map[string]any{"bot_id": botID}, &session)
	return session, err
}

// StopSession ends the session and keeps its volume.
func (c *Client) StopSession(ctx context.Context, botID string) error {
	return c.do(ctx, http.MethodDelete, sessionPath(botID), nil, nil)
}

// Screenshot is the current desktop as PNG bytes.
//
// It is the one call whose answer is not JSON, so it reads the body directly
// rather than going through do.
func (c *Client) Screenshot(ctx context.Context, botID string) ([]byte, error) {
	if err := c.configured(); err != nil {
		return nil, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+sessionPath(botID)+"/screenshot", nil)
	if err != nil {
		return nil, fmt.Errorf("vcomputer: build request: %w", err)
	}

	// A screenshot is a /v1 route like any other, so it needs the bearer too: the
	// one call that skipped it would be the one call that can only ever answer 401.
	c.authorize(request)

	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrUnavailable, err)
	}
	defer func() { _ = response.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponse))
	if err != nil {
		return nil, fmt.Errorf("vcomputer: read screenshot: %w", err)
	}
	if response.StatusCode >= 400 {
		return nil, classify(response.StatusCode, payload)
	}
	if len(payload) == 0 {
		return nil, errors.New("vcomputer: the control plane answered an empty screenshot")
	}
	return payload, nil
}

// Click presses a point on the desktop.
func (c *Client) Click(ctx context.Context, botID string, x, y int) error {
	return c.do(ctx, http.MethodPost, sessionPath(botID)+"/click", map[string]any{"x": x, "y": y}, nil)
}

// Type sends text to the focused window.
func (c *Client) Type(ctx context.Context, botID string, text string) error {
	return c.do(ctx, http.MethodPost, sessionPath(botID)+"/type", map[string]any{"text": text}, nil)
}

// Navigate points the browser at a page.
//
// It is the one action that changes what the desktop is showing, so the control
// plane is what validates the URL — it accepts the http, https, data and about
// schemes and refuses anything else with a 400 — and nothing here second-guesses
// that. Whether the navigation is allowed at all is the approval gate's answer, not
// this layer's: a page that is not the caller's own is write_external
// (docs/riset-microvm.md §5.1).
func (c *Client) Navigate(ctx context.Context, botID string, url string) error {
	return c.do(ctx, http.MethodPost, sessionPath(botID)+"/navigate", map[string]any{"url": url}, nil)
}

// Exec runs a command inside the sandbox.
//
// Nothing here decides whether the command is allowed: by the time it reaches this
// client, the approval gate has already answered (docs/riset-microvm.md §5.1).
// Whether the runtime really ran it is in the answer's `stub` field: the docker
// runtime does (`stub: false`), the memory runtime echoes the command instead
// (`stub: true`), so a caller has to read that field rather than the exit code.
func (c *Client) Exec(ctx context.Context, botID string, command string) (domain.ExecResult, error) {
	var result domain.ExecResult
	err := c.do(ctx, http.MethodPost, sessionPath(botID)+"/exec", map[string]any{"command": command}, &result)
	return result, err
}

// do performs one JSON call.
func (c *Client) do(ctx context.Context, method, path string, body, into any) error {
	if err := c.configured(); err != nil {
		return err
	}

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("vcomputer: encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("vcomputer: build request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	c.authorize(request)

	response, err := c.client.Do(request)
	if err != nil {
		// Unreachable is not a refusal: the sandbox may simply not be up yet, and the
		// caller is expected to retry rather than to change what it asked for.
		return fmt.Errorf("%w: %w", domain.ErrUnavailable, err)
	}
	defer func() { _ = response.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("vcomputer: read answer: %w", err)
	}
	if response.StatusCode >= 400 {
		return classify(response.StatusCode, payload)
	}

	if into == nil || len(payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, into); err != nil {
		return fmt.Errorf("vcomputer: read control plane answer: %w", err)
	}
	return nil
}

// configured reports whether a control plane address was set.
func (c *Client) configured() error {
	if c == nil || c.baseURL == "" {
		return domain.ErrNotConfigured
	}
	return nil
}

// authorize puts the shared secret on a request. Every /v1 route requires it, so a
// request built here without the header is a request that can only ever come back
// 401 — which is why the header is set in one place instead of at each call site.
func (c *Client) authorize(request *http.Request) {
	request.Header.Set("Authorization", "Bearer "+c.token)
}

// sessionPath is the resource path for one bot. The id is not escaped because the
// control plane refuses an id that would need escaping: a bot id is an identifier,
// not free text, and letting a caller build a path out of it is how a control plane
// gets reached at a route nobody wrote.
func sessionPath(botID string) string {
	return "/v1/sessions/" + botID
}

// classify maps a status code back onto the sentinel the control plane put it in.
//
// The mapping is the whole reason the classification survives the seam: a 404 means
// "no session, create one", a 401 means the token is missing or wrong, and a 400
// means the request itself was refused. Flattening them into a string is how a caller
// ends up retrying something that will never succeed: the same rejected header is
// rejected again. The sentence the control plane wrote is carried through rather than
// replaced.
func classify(status int, payload []byte) error {
	var failure struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(payload, &failure)

	message := strings.TrimSpace(failure.Error)
	if message == "" {
		message = http.StatusText(status)
	}

	switch status {
	case http.StatusBadRequest:
		return fmt.Errorf("%w: %s", domain.ErrInvalidInput, message)
	case http.StatusUnauthorized:
		// A refused token is a deployment misconfiguration rather than a transient
		// failure: retrying sends the same header the control plane just rejected.
		return fmt.Errorf("%w: %s", domain.ErrNotConfigured, message)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", domain.ErrNoSession, message)
	default:
		return fmt.Errorf("%w: %s", domain.ErrUnavailable, message)
	}
}
