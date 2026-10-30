// Package anthropic implements the Messages API wire format:
// POST {base_url}/v1/messages.
//
// Its shape differs from the [OI]-compatible one in the ways that matter: the
// system prompt is a top-level field, content is a list of typed blocks, tool
// calls are `tool_use` blocks, and tool answers are `tool_result` blocks inside a
// user message. The adapter hides all of that behind the same domain contract.
package anthropic

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

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/llm/sse"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/llm/vision"
)

// DefaultBaseURL is the public endpoint.
const DefaultBaseURL = "https://api.anthropic.com"

// APIVersion is the header value Anthropic requires.
const APIVersion = "2023-06-01"

// DefaultContextTokens is the fallback context window.
const DefaultContextTokens = 200000

// DefaultMaxTokens is required by the API; a request without one is rejected.
const DefaultMaxTokens = 4096

// Config is one connection to an Anthropic-compatible server.
type Config struct {
	BaseURL       string
	APIKey        string
	Model         string
	MaxTokens     int
	ContextTokens int
	// PromptCaching enables cache breakpoints on the stable prefix.
	PromptCaching  bool
	SupportsTools  bool
	SupportsVision bool
	Headers        map[string]string
	HTTPClient     *http.Client
	Timeout        time.Duration
}

// Client is the adapter.
type Client struct {
	cfg    Config
	client *http.Client
}

// New builds the adapter.
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
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = DefaultMaxTokens
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
func (c *Client) Name() string { return domain.AdapterAnthropic }

// Model reports the configured model.
func (c *Client) Model() string { return c.cfg.Model }

// Capabilities reports what this configuration can do.
func (c *Client) Capabilities() domain.Capabilities {
	return domain.Capabilities{
		Tools:             c.cfg.SupportsTools,
		Vision:            c.cfg.SupportsVision,
		Streaming:         true,
		PromptCaching:     c.cfg.PromptCaching,
		ParallelToolCalls: true,
		// No embedding endpoint exists in this API at all.
		Embeddings:       false,
		MaxContextTokens: c.cfg.ContextTokens,
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

	var payload messagesResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		return domain.ChatResponse{}, fmt.Errorf("%w: decode response: %w", domain.ErrProviderUnavailable, err)
	}
	if payload.Error != nil {
		return domain.ChatResponse{}, payload.Error.err()
	}

	return c.decodeContent(payload), nil
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

// readStream normalises the Messages API events.
func (c *Client) readStream(ctx context.Context, body io.Reader, events chan<- domain.StreamEvent) {
	reader := sse.NewReader(body)

	// The API streams a tool call in two parts: a content_block_start carrying
	// the name and id, then input_json_delta fragments carrying the arguments.
	// The call is only reported once the block closes.
	var openCall *domain.ToolCall
	var openIndex int
	open := false

	for {
		if ctx.Err() != nil {
			events <- domain.StreamEvent{Type: domain.EventError, Err: ctx.Err()}
			return
		}

		event, err := reader.Next()
		if errors.Is(err, io.EOF) {
			if open {
				flushCall(openCall, events)
			}
			events <- domain.StreamEvent{Type: domain.EventDone, FinishReason: domain.FinishStop}
			return
		}
		if err != nil {
			events <- domain.StreamEvent{Type: domain.EventError, Err: fmt.Errorf("%w: read stream: %w", domain.ErrProviderUnavailable, err)}
			return
		}
		if event.Data == "" {
			continue
		}

		var payload streamEvent
		if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
			events <- domain.StreamEvent{Type: domain.EventError, Err: fmt.Errorf("%w: decode stream event: %w", domain.ErrProviderUnavailable, err)}
			return
		}
		if payload.Error != nil {
			events <- domain.StreamEvent{Type: domain.EventError, Err: payload.Error.err()}
			return
		}

		// The event name arrives either as a `type` field or as the SSE event
		// name, depending on the producer.
		name := payload.Type
		if name == "" {
			name = event.Name
		}

		switch name {
		case "message_start":
			if payload.Message != nil && payload.Message.Usage != nil {
				usage := payload.Message.Usage.usage()
				events <- domain.StreamEvent{Type: domain.EventUsage, Usage: &usage}
			}

		case "content_block_start":
			if payload.ContentBlock != nil && payload.ContentBlock.Type == "tool_use" {
				openCall = &domain.ToolCall{
					ID:        payload.ContentBlock.ID,
					Name:      payload.ContentBlock.Name,
					Arguments: json.RawMessage{},
				}
				openIndex = payload.Index
				open = true
			}

		case "content_block_delta":
			if payload.Delta == nil {
				continue
			}
			switch payload.Delta.Type {
			case "text_delta":
				if payload.Delta.Text != "" {
					events <- domain.StreamEvent{Type: domain.EventText, Text: payload.Delta.Text}
				}
			case "input_json_delta":
				if open && payload.Index == openIndex {
					openCall.Arguments = append(openCall.Arguments, payload.Delta.PartialJSON...)
				}
			}

		case "content_block_stop":
			if open && payload.Index == openIndex {
				flushCall(openCall, events)
				openCall = nil
				open = false
			}

		case "message_delta":
			if payload.Usage != nil {
				usage := payload.Usage.usage()
				events <- domain.StreamEvent{Type: domain.EventUsage, Usage: &usage}
			}
			if payload.Delta != nil && payload.Delta.StopReason != "" {
				events <- domain.StreamEvent{Type: domain.EventDone, FinishReason: mapStopReason(payload.Delta.StopReason)}
				return
			}

		case "message_stop":
			if open {
				// open is not cleared: the function returns right after.
				flushCall(openCall, events)
			}
			events <- domain.StreamEvent{Type: domain.EventDone, FinishReason: domain.FinishStop}
			return

		case "ping":
			// Keep-alive.

		default:
			// An unknown event is ignored rather than fatal: the API adds new
			// ones over time and a stream should not break because of that.
		}
	}
}

func flushCall(call *domain.ToolCall, events chan<- domain.StreamEvent) {
	if call == nil || call.Name == "" {
		return
	}
	if len(call.Arguments) == 0 {
		call.Arguments = json.RawMessage("{}")
	}
	copied := *call
	events <- domain.StreamEvent{Type: domain.EventToolCall, ToolCall: &copied}
}

// do performs the HTTP call and normalises failures.
func (c *Client) do(ctx context.Context, body []byte, stream bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %w", domain.ErrInvalidRequest, err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", APIVersion)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if c.cfg.APIKey != "" {
		req.Header.Set("x-api-key", c.cfg.APIKey)
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
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity:
		defer func() { _ = resp.Body.Close() }()
		message := errorBody(resp)
		// A refusal that names the picture is a capability this model lacks rather
		// than a complaint about the request: another provider in the chain may read
		// it, and the caller can answer without it.
		if vision.Refused(message) {
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

func rateLimitError(resp *http.Response) error {
	message := errorBody(resp)
	if seconds := resp.Header.Get("retry-after"); seconds != "" {
		return fmt.Errorf("%w: %s (retry after %ss)", domain.ErrRateLimited, message, seconds)
	}
	return fmt.Errorf("%w: %s", domain.ErrRateLimited, message)
}

func statusError(resp *http.Response, message string, sentinel error) error {
	return fmt.Errorf("%w: %s returned %d: %s", sentinel, resp.Request.URL.Host, resp.StatusCode, message)
}

func errorBody(resp *http.Response) string {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return "unreadable response"
	}

	var payload struct {
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Error != nil {
		return payload.Error.Type + ": " + payload.Error.Message
	}

	text := strings.TrimSpace(string(body))
	if len(text) > 300 {
		return text[:300] + "..."
	}
	return text
}

// decodeContent turns the response blocks into the internal shape. Text blocks
// are concatenated, tool_use blocks become tool calls.
func (c *Client) decodeContent(payload messagesResponse) domain.ChatResponse {
	response := domain.ChatResponse{
		FinishReason:    mapStopReason(payload.StopReason),
		RawFinishReason: payload.StopReason,
		Model:           payload.Model,
		Provider:        domain.AdapterAnthropic,
	}
	if response.Model == "" {
		response.Model = c.cfg.Model
	}

	var text strings.Builder
	for _, block := range payload.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			arguments := block.Input
			if len(arguments) == 0 {
				arguments = json.RawMessage("{}")
			}
			response.ToolCalls = append(response.ToolCalls, domain.ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: arguments,
			})
		}
	}
	response.Text = text.String()

	if payload.Usage != nil {
		response.Usage = payload.Usage.usage()
	}

	return response
}

// mapStopReason normalises the provider's stop reason.
func mapStopReason(reason string) domain.FinishReason {
	switch reason {
	case "end_turn", "stop_sequence", "":
		return domain.FinishStop
	case "tool_use":
		return domain.FinishToolCalls
	case "max_tokens":
		return domain.FinishLength
	case "refusal":
		return domain.FinishContentFilter
	default:
		return domain.FinishUnknown
	}
}

var _ domain.Provider = (*Client)(nil)
