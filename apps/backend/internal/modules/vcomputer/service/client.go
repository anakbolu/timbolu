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
// The PoC does not authenticate. That is a gap and it is written down here rather
// than hidden: the control plane is reachable only from the sandbox network today,
// and the first thing wiring it into production does is put it behind the same
// shared-secret seam the WhatsApp gateway uses.
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

// Client calls the control plane.
type Client struct {
	baseURL string
	client  *http.Client
}

// Compile-time check that the client is the port the module declares.
var _ domain.ComputerUseClient = (*Client)(nil)

// New builds the client. An empty address leaves it unconfigured: every call then
// reports that, which is what a deployment without a virtual computer looks like.
//
// The default timeout is generous because a screenshot of a cold sandbox includes
// the container starting: the PoC would rather wait than report a failure that
// would have resolved itself.
func New(baseURL string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		client:  &http.Client{Timeout: timeout},
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

// Exec runs a command inside the sandbox.
//
// Nothing here decides whether the command is allowed: by the time it reaches this
// client, the approval gate has already answered (docs/riset-microvm.md §5.1). The
// PoC's control plane echoes the command rather than running it, and the answer says
// so in its `stub` field.
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
// "no session, create one", a 501 means "the runtime behind this is a skeleton", and
// flattening either into a string is how a caller ends up retrying something that
// will never succeed. The sentence the control plane wrote is carried through rather
// than replaced.
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
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", domain.ErrNoSession, message)
	case http.StatusNotImplemented:
		return fmt.Errorf("%w: %s", domain.ErrNotImplemented, message)
	default:
		return fmt.Errorf("%w: %s", domain.ErrUnavailable, message)
	}
}
