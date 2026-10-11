// Package mcp speaks the Model Context Protocol to a server somebody else runs.
//
// The product's own adapters are written against a provider's API. This package
// is for the applications that already publish an MCP server: rather than
// reimplementing each one, the adapter asks the server what it offers and forwards
// calls to it.
//
// The transport here is the JSON-RPC-over-HTTP one: a POST per request, with the
// server answering either a result or an error. It is the smallest transport that
// real servers implement, and it needs no long-lived connection to keep correct —
// which matters because a task may be the first thing to talk to a server in days.
package mcp

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
)

// ProtocolVersion is the revision this client speaks.
const ProtocolVersion = "2025-06-18"

// maxBody bounds what is read from a server, so a broken or hostile one cannot
// exhaust memory. A tool listing is small; a tool result may be large, and this is
// generous enough for the latter.
const maxBody = 8 << 20

// Errors a server can produce.
var (
	// ErrServerUnavailable means the server could not be reached or answered
	// nonsense. It is retryable: a server that is down is usually down briefly.
	ErrServerUnavailable = errors.New("mcp: the server is unavailable")
	// ErrToolFailed means the server answered, and the call failed. It is the
	// tool's own failure rather than a transport one.
	ErrToolFailed = errors.New("mcp: the tool failed")
	// ErrUnauthorized means the server refused the credential.
	ErrUnauthorized = errors.New("mcp: the server refused the credential")
)

// Client talks to one server.
type Client struct {
	endpoint string
	// credential is what the server authenticates with. It is opened by the
	// caller only for the call it is needed for, so it lives here for the length
	// of one operation rather than for the life of the process.
	credential string
	http       *http.Client
}

// Config is what a client needs.
type Config struct {
	// Endpoint is the server's HTTP address.
	Endpoint string
	// Credential is sent as a bearer token. Empty means the server is open, which
	// is what a server on a private network looks like.
	Credential string
	// Timeout bounds one request. A server that does not answer within it is
	// treated as unavailable rather than waited on, because a task is blocked
	// while this runs.
	Timeout time.Duration
	// HTTPClient overrides the transport, which is how a test points the client
	// at its own server.
	HTTPClient *http.Client
}

// New builds a client.
func New(cfg Config) (*Client, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("mcp: an endpoint is required")
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}

	return &Client{
		endpoint:   endpoint,
		credential: cfg.Credential,
		http:       client,
	}, nil
}

// Tool is one thing a server offers.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	// Annotations carry the server's own hints about what the tool does. They are
	// hints, not promises: a server is free to say nothing, and this client
	// treats silence as the unsafe answer.
	Annotations struct {
		ReadOnlyHint *bool  `json:"readOnlyHint"`
		Title        string `json:"title"`
	} `json:"annotations"`
}

// ReadOnly reports whether the server says this tool changes nothing.
//
// A server that says nothing is treated as though it changes something. The
// product's rule is that a person holds the door to anything that leaves, and
// guessing "probably fine" from silence is how that rule stops being true.
func (t Tool) ReadOnly() bool {
	return t.Annotations.ReadOnlyHint != nil && *t.Annotations.ReadOnlyHint
}

// CallResult is what a tool answered.
type CallResult struct {
	// Content is the server's own content blocks, kept whole so the model reads
	// what the server said rather than this client's summary of it.
	Content []Content `json:"content"`
	// IsError says the server reported the call as failed while still answering.
	// The protocol allows it, so it is checked rather than assumed away.
	IsError bool `json:"isError"`
}

// Content is one block of a tool's answer.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
	// Data and MimeType carry an image or an embedded resource.
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

// Text joins the text blocks, which is what a model reads.
func (r CallResult) Text() string {
	parts := make([]string, 0, len(r.Content))
	for _, block := range r.Content {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// Initialize performs the protocol handshake and returns what the server calls
// itself.
//
// It is separate from the tool listing because a server may refuse the handshake
// for a reason worth showing a person — a version it does not speak, a credential
// it does not accept — and that is different from an empty tool list.
func (c *Client) Initialize(ctx context.Context) (string, error) {
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}

	if err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "bolu",
			"version": "1",
		},
	}, &result); err != nil {
		return "", err
	}

	if result.ServerInfo.Name == "" {
		return "mcp", nil
	}
	return result.ServerInfo.Name, nil
}

// Tools lists what the server offers.
func (c *Client) Tools(ctx context.Context) ([]Tool, error) {
	var result struct {
		Tools []Tool `json:"tools"`
		// NextCursor is set when a server paginates. This client reads the first
		// page only, which is stated rather than silently truncating: a server
		// with more tools than fit one page is rare, and following the cursor
		// needs a bound this does not have.
		NextCursor string `json:"nextCursor"`
	}

	if err := c.call(ctx, "tools/list", map[string]any{}, &result); err != nil {
		return nil, err
	}
	return result.Tools, nil
}

// Call runs one tool.
func (c *Client) Call(ctx context.Context, name string, arguments json.RawMessage) (CallResult, error) {
	parsed := map[string]any{}
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &parsed); err != nil {
			return CallResult{}, fmt.Errorf("mcp: the arguments are not an object: %w", err)
		}
	}

	var result CallResult
	if err := c.call(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": parsed,
	}, &result); err != nil {
		return CallResult{}, err
	}

	// The protocol allows a call to answer with an error while still returning a
	// result. Reporting it as a failure is what keeps a tool that could not do
	// its job from reading as though it did.
	if result.IsError {
		return result, fmt.Errorf("%w: %s", ErrToolFailed, result.Text())
	}
	return result, nil
}

// ------------------------------------------------------------------ transport

// rpcRequest is one JSON-RPC call.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcResponse is one JSON-RPC answer.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// rpcError is a server's own failure.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (e *rpcError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("mcp: server error %d", e.Code)
	}
	return fmt.Sprintf("mcp: server error %d: %s", e.Code, e.Message)
}

// call performs one request and decodes its result.
func (c *Client) call(ctx context.Context, method string, params any, target any) error {
	document, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return fmt.Errorf("mcp: encode %s: %w", method, err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(document))
	if err != nil {
		return fmt.Errorf("mcp: build %s request: %w", method, err)
	}
	request.Header.Set("Content-Type", "application/json")
	// The protocol asks a client to accept both, because a server may answer with
	// either depending on how it is deployed.
	request.Header.Set("Accept", "application/json, text/event-stream")
	if c.credential != "" {
		request.Header.Set("Authorization", "Bearer "+c.credential)
	}

	response, err := c.http.Do(request)
	if err != nil {
		// A transport failure is retryable: a server that is down is usually down
		// briefly, and a task should wait rather than fail.
		return fmt.Errorf("%w: %s: %w", ErrServerUnavailable, method, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w (status %d)", ErrUnauthorized, response.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody))
	if err != nil {
		return fmt.Errorf("%w: read %s answer: %w", ErrServerUnavailable, method, err)
	}

	if response.StatusCode >= 400 {
		// A server that answers 5xx is down; one that answers 4xx has refused the
		// request itself, which is not a reason to retry it unchanged.
		if response.StatusCode >= 500 {
			return fmt.Errorf("%w: %s answered %d", ErrServerUnavailable, method, response.StatusCode)
		}
		return fmt.Errorf("mcp: %s answered %d: %s", method, response.StatusCode, truncate(string(body), 200))
	}

	// A server may answer with a server-sent event stream instead of a body. The
	// protocol allows it, so it is read rather than reported as nonsense.
	payload := body
	if contentType := response.Header.Get("Content-Type"); strings.Contains(contentType, "text/event-stream") {
		payload = firstEventData(body)
	}

	var decoded rpcResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return fmt.Errorf("%w: %s answered something that is not JSON-RPC: %w",
			ErrServerUnavailable, method, err)
	}
	if decoded.Error != nil {
		return decoded.Error
	}
	if len(decoded.Result) == 0 {
		return fmt.Errorf("%w: %s answered with no result", ErrServerUnavailable, method)
	}

	if target == nil {
		return nil
	}
	if err := json.Unmarshal(decoded.Result, target); err != nil {
		return fmt.Errorf("mcp: %s answered a result this client cannot read: %w", method, err)
	}
	return nil
}

// firstEventData reads the payload of the first `data:` line of an event stream.
func firstEventData(body []byte) []byte {
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if after, found := strings.CutPrefix(trimmed, "data:"); found {
			return []byte(strings.TrimSpace(after))
		}
	}
	return body
}

// truncate bounds what an error message carries, so a server's whole body does
// not end up in a log line.
func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
