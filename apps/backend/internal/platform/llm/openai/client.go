// Package openai implements the [OI]-compatible chat completions wire
// format: POST {base_url}/v1/chat/completions.
//
// The same adapter therefore serves [OI] itself, OpenRouter, Groq, DeepSeek,
// vLLM, Ollama, and any other server that copies the format. It translates the
// internal message shape into this one and the answers back, so nothing above it
// knows which provider answered.
package openai

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

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/llm/sse"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/llm/vision"
)

// DefaultBaseURL is used when a configuration does not name one.
const DefaultBaseURL = "https://api.openai.com"

// DefaultContextTokens is the fallback context window when the configuration
// does not state one.
const DefaultContextTokens = 128000

// Config is one connection to an [OI]-compatible server.
type Config struct {
	BaseURL string
	APIKey  string
	Model   string
	// MaxTokens caps the answer length when a request does not set one.
	MaxTokens int
	// ContextTokens is the model's window, used for the pre-flight check.
	ContextTokens int
	// SupportsTools, SupportsVision, and ParallelToolCalls describe the model;
	// a small local model often has none of them.
	SupportsTools     bool
	SupportsVision    bool
	ParallelToolCalls bool
	// SupportsEmbeddings says this server implements the embeddings route of
	// this wire format. The format defines one; a given gateway (OpenRouter, for
	// instance) may still answer 404, which then falls to the next provider.
	SupportsEmbeddings bool
	// Organization and extra headers some gateways need.
	Headers map[string]string
	// HTTPClient lets a test point the adapter at a stub server.
	HTTPClient *http.Client
	// Timeout bounds one call. Streaming uses the context instead.
	Timeout time.Duration
}

// Client is the adapter.
type Client struct {
	cfg    Config
	client *http.Client
}

// New builds the adapter. A missing base URL falls back to the public endpoint;
// a missing model is an error, because a request without one cannot be served.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("%w: model is required", domain.ErrInvalidRequest)
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.ContextTokens <= 0 {
		cfg.ContextTokens = DefaultContextTokens
	}
	if cfg.HTTPClient == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 5 * time.Minute
		}
		cfg.HTTPClient = &http.Client{Timeout: timeout}
	}

	return &Client{cfg: cfg, client: cfg.HTTPClient}, nil
}

// Name reports the adapter name.
func (c *Client) Name() string { return domain.AdapterOpenAI }

// Model reports the configured model.
func (c *Client) Model() string { return c.cfg.Model }

// Capabilities reports what this configuration can do.
func (c *Client) Capabilities() domain.Capabilities {
	return domain.Capabilities{
		Tools:             c.cfg.SupportsTools,
		Vision:            c.cfg.SupportsVision,
		Streaming:         true,
		PromptCaching:     false,
		ParallelToolCalls: c.cfg.ParallelToolCalls,
		Embeddings:        c.cfg.SupportsEmbeddings,
		MaxContextTokens:  c.cfg.ContextTokens,
	}
}

// Chat performs one completion.
func (c *Client) Chat(ctx context.Context, req domain.ChatRequest) (domain.ChatResponse, error) {
	body, err := c.encodeRequest(req, false)
	if err != nil {
		return domain.ChatResponse{}, err
	}

	resp, err := c.do(ctx, body, false)
	if err != nil {
		return domain.ChatResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return domain.ChatResponse{}, fmt.Errorf("%w: read response: %w", domain.ErrProviderUnavailable, err)
	}

	var payload chatCompletion
	if err := json.Unmarshal(raw, &payload); err != nil {
		return domain.ChatResponse{}, fmt.Errorf("%w: decode response: %w", domain.ErrProviderUnavailable, err)
	}
	if payload.Error != nil {
		return domain.ChatResponse{}, payload.Error.err()
	}
	if len(payload.Choices) == 0 {
		return domain.ChatResponse{}, fmt.Errorf("%w: provider returned no choices", domain.ErrProviderUnavailable)
	}

	return c.decodeChoice(payload), nil
}

// Stream performs one completion and reports its progress.
func (c *Client) Stream(ctx context.Context, req domain.ChatRequest) (<-chan domain.StreamEvent, error) {
	body, err := c.encodeRequest(req, true)
	if err != nil {
		return nil, err
	}

	// The body is closed by the reader goroutine in streamPipe, which is the only
	// place that owns the response once the stream has started; bodyclose cannot
	// see into that goroutine.
	//nolint:bodyclose // closed in streamPipe
	resp, err := c.do(ctx, body, true)
	if err != nil {
		return nil, err
	}

	return streamPipe(ctx, c, resp), nil
}

// streamPipe reads the response in the background and closes it when the stream
// ends. The goroutine owns the response from here on, which is why the body is
// handed over rather than closed by the caller.
func streamPipe(ctx context.Context, c *Client, resp *http.Response) <-chan domain.StreamEvent {
	events := make(chan domain.StreamEvent, 16)
	go func() {
		defer close(events)
		defer func() { _ = resp.Body.Close() }()
		c.readStream(ctx, resp.Body, events)
	}()

	return events
}

// readStream turns the provider's events into the normalised ones.
func (c *Client) readStream(ctx context.Context, body io.Reader, events chan<- domain.StreamEvent) {
	reader := sse.NewReader(body)

	// Tool-call arguments arrive in fragments; the call is only reported once
	// it is complete, which keeps the consumer from having to reassemble JSON.
	var pending []*domain.ToolCall

	for {
		if ctx.Err() != nil {
			events <- domain.StreamEvent{Type: domain.EventError, Err: ctx.Err()}
			return
		}

		event, err := reader.Next()
		if errors.Is(err, io.EOF) {
			// The provider closed the stream. Anything still being assembled is
			// reported before the terminal event, so a truncated tool call is
			// never silently dropped.
			c.flushToolCalls(pending, events)
			events <- domain.StreamEvent{Type: domain.EventDone, FinishReason: domain.FinishStop}
			return
		}
		if err != nil {
			events <- domain.StreamEvent{Type: domain.EventError, Err: fmt.Errorf("%w: read stream: %w", domain.ErrProviderUnavailable, err)}
			return
		}

		if sse.IsDone(event.Data) {
			c.flushToolCalls(pending, events)
			events <- domain.StreamEvent{Type: domain.EventDone, FinishReason: domain.FinishStop}
			return
		}
		if event.Data == "" {
			continue
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(event.Data), &chunk); err != nil {
			// A provider that sends something unparsable is a provider failure,
			// not a reason to hang.
			events <- domain.StreamEvent{Type: domain.EventError, Err: fmt.Errorf("%w: decode stream event: %w", domain.ErrProviderUnavailable, err)}
			return
		}
		if chunk.Error != nil {
			events <- domain.StreamEvent{Type: domain.EventError, Err: chunk.Error.err()}
			return
		}

		if chunk.Usage != nil {
			usage := chunk.Usage.usage()
			events <- domain.StreamEvent{Type: domain.EventUsage, Usage: &usage}
		}

		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				events <- domain.StreamEvent{Type: domain.EventText, Text: choice.Delta.Content}
			}
			for _, call := range choice.Delta.ToolCalls {
				pending = mergeToolCall(pending, call)
			}
			if choice.FinishReason != "" {
				reason := mapFinishReason(choice.FinishReason)
				c.flushToolCalls(pending, events)
				// pending is not cleared: the function returns right after, so
				// the assignment would be dead.
				events <- domain.StreamEvent{Type: domain.EventDone, FinishReason: reason}
				return
			}
		}
	}
}

// flushToolCalls reports every assembled call, dropping the ones with no name.
func (c *Client) flushToolCalls(pending []*domain.ToolCall, events chan<- domain.StreamEvent) {
	for _, call := range pending {
		if call == nil || call.Name == "" {
			continue
		}
		if len(call.Arguments) == 0 {
			call.Arguments = json.RawMessage("{}")
		}
		copied := *call
		events <- domain.StreamEvent{Type: domain.EventToolCall, ToolCall: &copied}
	}
}

// mergeToolCall appends a streamed fragment to the call it belongs to. The
// provider identifies the call by index, and only the first fragment carries the
// name.
func mergeToolCall(pending []*domain.ToolCall, fragment streamToolCall) []*domain.ToolCall {
	for len(pending) <= fragment.Index {
		pending = append(pending, &domain.ToolCall{})
	}

	call := pending[fragment.Index]
	if fragment.ID != "" {
		call.ID = fragment.ID
	}
	if fragment.Function.Name != "" {
		call.Name = fragment.Function.Name
	}
	if fragment.Function.Arguments != "" {
		call.Arguments = append(call.Arguments, fragment.Function.Arguments...)
	}

	return pending
}

// do performs the HTTP call and turns a failure into the shared error set.
func (c *Client) do(ctx context.Context, body []byte, stream bool) (*http.Response, error) {
	return c.post(ctx, "/v1/chat/completions", body, stream)
}

// post sends one body to one route of the provider's API. The path is a
// parameter because this wire format serves completions and embeddings from the
// same host with the same headers.
func (c *Client) post(ctx context.Context, path string, body []byte, stream bool) (*http.Response, error) {
	url := c.cfg.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %w", domain.ErrInvalidRequest, err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	for key, value := range c.cfg.Headers {
		req.Header.Set(key, value)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrProviderUnavailable, err)
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return resp, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		defer func() { _ = resp.Body.Close() }()
		return nil, rateLimitError(resp)
	case resp.StatusCode == http.StatusRequestEntityTooLarge, resp.StatusCode == http.StatusBadRequest:
		defer func() { _ = resp.Body.Close() }()
		message := errorBody(resp)
		// A provider that names the picture is not complaining about the request: it
		// is saying this model cannot read one, which is a capability the chain
		// lacks — another provider may have it, and the caller can answer without
		// the picture. A 413 is a bound, and stays a malformed request.
		if resp.StatusCode == http.StatusBadRequest && vision.Refused(message) {
			return nil, statusError(resp, message, domain.ErrUnsupported)
		}
		return nil, statusError(resp, message, domain.ErrInvalidRequest)
	case resp.StatusCode >= 500:
		defer func() { _ = resp.Body.Close() }()
		return nil, statusError(resp, errorBody(resp), domain.ErrProviderUnavailable)
	default:
		defer func() { _ = resp.Body.Close() }()
		return nil, statusError(resp, errorBody(resp), domain.ErrProviderUnavailable)
	}
}

// rateLimitError carries the provider's retry hint when it sends one.
func rateLimitError(resp *http.Response) error {
	message := errorBody(resp)
	if seconds := resp.Header.Get("Retry-After"); seconds != "" {
		if duration, err := time.ParseDuration(seconds + "s"); err == nil {
			return fmt.Errorf("%w: %s (retry after %s)", domain.ErrRateLimited, message, duration)
		}
	}
	return fmt.Errorf("%w: %s", domain.ErrRateLimited, message)
}

func statusError(resp *http.Response, message string, sentinel error) error {
	return fmt.Errorf("%w: %s returned %d: %s", sentinel, resp.Request.URL.Host, resp.StatusCode, message)
}

// errorBody reads the provider's message, which is what says whether a refusal
// was about the picture or about the request.
func errorBody(resp *http.Response) string {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return "unreadable response"
	}

	var payload struct {
		Error *struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Error != nil {
		if payload.Error.Code != "" {
			return payload.Error.Code + ": " + payload.Error.Message
		}
		return payload.Error.Message
	}

	text := strings.TrimSpace(string(body))
	if len(text) > 300 {
		return text[:300] + "..."
	}
	return text
}

// decodeChoice turns one provider choice into the internal response.
func (c *Client) decodeChoice(payload chatCompletion) domain.ChatResponse {
	choice := payload.Choices[0]

	response := domain.ChatResponse{
		Text:            choice.Message.Content,
		FinishReason:    mapFinishReason(choice.FinishReason),
		RawFinishReason: choice.FinishReason,
		Model:           payload.Model,
		Provider:        domain.AdapterOpenAI,
	}
	if response.Model == "" {
		response.Model = c.cfg.Model
	}

	for _, call := range choice.Message.ToolCalls {
		arguments := json.RawMessage(call.Function.Arguments)
		if len(arguments) == 0 {
			arguments = json.RawMessage("{}")
		}
		response.ToolCalls = append(response.ToolCalls, domain.ToolCall{
			ID:        call.ID,
			Name:      call.Function.Name,
			Arguments: arguments,
		})
	}

	if payload.Usage != nil {
		response.Usage = payload.Usage.usage()
	}

	return response
}

// mapFinishReason normalises the provider's reason.
func mapFinishReason(reason string) domain.FinishReason {
	switch reason {
	case "stop":
		return domain.FinishStop
	case "tool_calls", "function_call":
		return domain.FinishToolCalls
	case "length":
		return domain.FinishLength
	case "content_filter":
		return domain.FinishContentFilter
	case "":
		return domain.FinishStop
	default:
		return domain.FinishUnknown
	}
}

// compile-time check.
var _ domain.Provider = (*Client)(nil)
