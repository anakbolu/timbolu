package openai

import (
	"encoding/json"
	"fmt"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
)

// The wire types of the [OI]-compatible chat completions format. They are
// declared here rather than pulled from a client SDK, so the adapter stays a
// thin, testable translation layer.

type chatCompletion struct {
	Model   string     `json:"model"`
	Choices []choice   `json:"choices"`
	Usage   *wireUsage `json:"usage"`
	Error   *wireError `json:"error"`
}

type choice struct {
	Index        int     `json:"index"`
	Message      message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type message struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls"`
	ToolCallID string         `json:"tool_call_id"`
}

type wireToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type streamChunk struct {
	Model   string        `json:"model"`
	Choices []streamDelta `json:"choices"`
	Usage   *wireUsage    `json:"usage"`
	Error   *wireError    `json:"error"`
}

// streamToolCall is a tool-call fragment inside a delta. The call is
// identified by index; only the first fragment carries the name.
type streamToolCall = wireToolCall

type streamDelta struct {
	Index        int     `json:"index"`
	Delta        message `json:"delta"`
	FinishReason string  `json:"finish_reason"`
}

type wireUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// usage converts the provider's counts. The cached tokens are a subset of the
// prompt tokens, so they are reported separately and not added again.
func (u wireUsage) usage() domain.Usage {
	usage := domain.Usage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
	}
	if u.PromptTokensDetails != nil {
		usage.CacheReadTokens = u.PromptTokensDetails.CachedTokens
	}
	return usage
}

type wireError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// err turns a provider error body into the shared error set, so the gateway can
// decide whether to fall back.
func (e *wireError) err() error {
	detail := e.Message
	if e.Code != "" {
		detail = e.Code + ": " + detail
	}

	switch e.Code {
	case "rate_limit_exceeded":
		return fmt.Errorf("%w: %s", domain.ErrRateLimited, detail)
	case "context_length_exceeded", "invalid_request_error":
		return fmt.Errorf("%w: %s", domain.ErrInvalidRequest, detail)
	}
	switch e.Type {
	case "invalid_request_error":
		return fmt.Errorf("%w: %s", domain.ErrInvalidRequest, detail)
	case "rate_limit_error":
		return fmt.Errorf("%w: %s", domain.ErrRateLimited, detail)
	}

	return fmt.Errorf("%w: %s", domain.ErrProviderUnavailable, detail)
}

// encodeRequest builds the provider payload from the internal request.
func (c *Client) encodeRequest(req domain.ChatRequest, stream bool) ([]byte, error) {
	messages := make([]map[string]any, 0, len(req.Messages)+1)

	// [OI] carries the system prompt as the first message. The retrieved block is
	// appended to it rather than sent as a second system message: some compatible
	// gateways accept only one, and the order — instructions first, then data — is
	// what matters for the model.
	system := req.System
	if req.Retrieved != "" {
		if system != "" {
			system += "\n\n"
		}
		system += req.Retrieved
	}
	if system != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}

	for _, message := range req.Messages {
		encoded, err := encodeMessage(message)
		if err != nil {
			return nil, err
		}
		messages = append(messages, encoded)
	}

	payload := map[string]any{
		"model":    modelFor(c.cfg.Model, req.Model),
		"messages": messages,
		"stream":   stream,
	}

	if maxTokens := maxTokensFor(c.cfg.MaxTokens, req.MaxTokens); maxTokens > 0 {
		payload["max_tokens"] = maxTokens
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if len(req.StopSequences) > 0 {
		payload["stop"] = req.StopSequences
	}

	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			schema := tool.Schema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        tool.Name,
					"description": tool.Description,
					"parameters":  schema,
				},
			})
		}
		payload["tools"] = tools

		if choice := toolChoiceFor(req); choice != nil {
			payload["tool_choice"] = choice
		}
	}

	// Some gateways return token counts in the final streamed chunk only when
	// asked.
	if stream {
		payload["stream_options"] = map[string]any{"include_usage": true}
	}

	return json.Marshal(payload)
}

// encodeMessage projects one internal message onto the provider shape.
func encodeMessage(message domain.Message) (map[string]any, error) {
	encoded := map[string]any{"role": string(message.Role)}

	switch message.Role {
	case domain.RoleTool:
		// A tool result is one message per call in this format.
		if len(message.ToolResults) == 0 {
			encoded["content"] = message.Text
			return encoded, nil
		}
		result := message.ToolResults[0]
		encoded["tool_call_id"] = result.CallID
		encoded["content"] = result.Content
		return encoded, nil

	case domain.RoleAssistant:
		if len(message.ToolCalls) > 0 {
			calls := make([]map[string]any, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				arguments := string(call.Arguments)
				if arguments == "" {
					arguments = "{}"
				}
				calls = append(calls, map[string]any{
					"id":   call.ID,
					"type": "function",
					"function": map[string]any{
						"name":      call.Name,
						"arguments": arguments,
					},
				})
			}
			encoded["tool_calls"] = calls
		}
		if message.Text != "" {
			encoded["content"] = message.Text
		}
		return encoded, nil
	}

	// User and system: text, or the multimodal parts array when images are
	// attached.
	if len(message.Images) == 0 {
		encoded["content"] = message.Text
		return encoded, nil
	}

	parts := make([]map[string]any, 0, len(message.Images)+1)
	if message.Text != "" {
		parts = append(parts, map[string]any{"type": "text", "text": message.Text})
	}
	for _, image := range message.Images {
		parts = append(parts, map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": "data:" + image.MediaType + ";base64," + image.Data,
			},
		})
	}
	encoded["content"] = parts
	return encoded, nil
}

// toolChoiceFor maps the internal choice onto the provider's.
func toolChoiceFor(req domain.ChatRequest) any {
	if req.ToolName != "" {
		return map[string]any{"type": "function", "function": map[string]any{"name": req.ToolName}}
	}
	switch req.ToolChoice {
	case domain.ToolChoiceNone:
		return "none"
	case domain.ToolChoiceRequired:
		return "required"
	default:
		return "auto"
	}
}

// modelFor lets a per-call model win over the configured one.
func modelFor(configured, requested string) string {
	if requested != "" {
		return requested
	}
	return configured
}

// maxTokensFor prefers the per-call cap over the configured one.
func maxTokensFor(configured, requested int) int {
	if requested > 0 {
		return requested
	}
	return configured
}
