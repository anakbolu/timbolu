package anthropic

import (
	"encoding/json"
	"fmt"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
)

// The wire types of the Messages API. Declared here so the adapter stays a thin
// translation layer instead of pulling in an SDK.

type messagesResponse struct {
	Model      string         `json:"model"`
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      *wireUsage     `json:"usage"`
	Error      *wireError     `json:"error"`
}

type contentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type wireUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// usage converts the provider's counts. Cached tokens are reported separately by
// this API and are not part of input_tokens.
func (u wireUsage) usage() domain.Usage {
	return domain.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
}

type wireError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// err turns a provider error into the shared set.
func (e *wireError) err() error {
	detail := e.Message
	if e.Type != "" {
		detail = e.Type + ": " + e.Message
	}

	switch e.Type {
	case "rate_limit_error", "overloaded_error":
		return fmt.Errorf("%w: %s", domain.ErrRateLimited, detail)
	case "invalid_request_error":
		return fmt.Errorf("%w: %s", domain.ErrInvalidRequest, detail)
	case "authentication_error", "permission_error":
		return fmt.Errorf("%w: %s", domain.ErrProviderUnavailable, detail)
	}

	return fmt.Errorf("%w: %s", domain.ErrProviderUnavailable, detail)
}

// streamEvent is one event of the streaming API. Different event types use
// different fields, so most of them are optional.
type streamEvent struct {
	Type         string        `json:"type"`
	Index        int           `json:"index"`
	Message      *streamStart  `json:"message"`
	ContentBlock *contentBlock `json:"content_block"`
	Delta        *streamDelta  `json:"delta"`
	Usage        *wireUsage    `json:"usage"`
	Error        *wireError    `json:"error"`
}

type streamStart struct {
	Model string     `json:"model"`
	Usage *wireUsage `json:"usage"`
}

type streamDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
}

// encodeRequest builds the provider payload. This API takes streaming as a body
// field rather than as a separate endpoint.
func (c *Client) encodeRequest(req domain.ChatRequest, stream bool) ([]byte, error) {
	messages := make([]map[string]any, 0, len(req.Messages))
	for _, message := range req.Messages {
		encoded, err := encodeMessage(message)
		if err != nil {
			return nil, err
		}
		messages = append(messages, encoded)
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = c.cfg.MaxTokens
	}

	payload := map[string]any{
		"model":      modelFor(c.cfg.Model, req.Model),
		"max_tokens": maxTokens,
		"messages":   messages,
		// Streaming is a body field here, not a separate endpoint.
		"stream": stream,
	}

	// This API takes the system prompt as its own field, not as a message. A
	// retrieval block travels as a second block of that field, so the cache
	// breakpoint lands after the stable prefix rather than after the retrieved
	// text: the persona, the instructions, and the main memory are what is worth
	// caching, and the search result is not.
	if req.System != "" || req.Retrieved != "" {
		blocks := make([]map[string]any, 0, 2)

		if req.System != "" {
			block := map[string]any{"type": "text", "text": req.System}
			if c.cfg.PromptCaching {
				// A cache breakpoint on the stable prefix measurably cuts the cost
				// of every later turn.
				block["cache_control"] = map[string]any{"type": "ephemeral"}
			}
			blocks = append(blocks, block)
		}
		if req.Retrieved != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": req.Retrieved})
		}

		payload["system"] = blocks
	}

	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if len(req.StopSequences) > 0 {
		payload["stop_sequences"] = req.StopSequences
	}

	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			schema := tool.Schema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools = append(tools, map[string]any{
				"name":         tool.Name,
				"description":  tool.Description,
				"input_schema": schema,
			})
		}
		payload["tools"] = tools

		if choice := toolChoiceFor(req); choice != nil {
			payload["tool_choice"] = choice
		}
	}

	return json.Marshal(payload)
}

// encodeMessage projects one internal message onto the block shape.
func encodeMessage(message domain.Message) (map[string]any, error) {
	switch message.Role {
	case domain.RoleAssistant:
		blocks := make([]map[string]any, 0, len(message.ToolCalls)+1)
		if message.Text != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": message.Text})
		}
		for _, call := range message.ToolCalls {
			arguments := call.Arguments
			if len(arguments) == 0 {
				arguments = json.RawMessage("{}")
			}
			blocks = append(blocks, map[string]any{
				"type":  "tool_use",
				"id":    call.ID,
				"name":  call.Name,
				"input": arguments,
			})
		}
		return map[string]any{"role": "assistant", "content": blocks}, nil

	case domain.RoleTool:
		// Tool answers are user-role messages carrying tool_result blocks.
		blocks := make([]map[string]any, 0, len(message.ToolResults)+1)
		for _, result := range message.ToolResults {
			content := result.Content
			if content == "" {
				content = "(no output)"
			}
			blocks = append(blocks, map[string]any{
				"type":        "tool_result",
				"tool_use_id": result.CallID,
				"content":     content,
				"is_error":    result.IsError,
			})
		}
		if len(blocks) == 0 && message.Text != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": message.Text})
		}
		return map[string]any{"role": "user", "content": blocks}, nil
	}

	// User messages: text, or blocks when images are attached.
	if len(message.Images) == 0 {
		return map[string]any{"role": string(message.Role), "content": message.Text}, nil
	}

	blocks := make([]map[string]any, 0, len(message.Images)+1)
	if message.Text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": message.Text})
	}
	for _, image := range message.Images {
		blocks = append(blocks, map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": image.MediaType,
				"data":       image.Data,
			},
		})
	}
	return map[string]any{"role": "user", "content": blocks}, nil
}

// toolChoiceFor maps the internal choice onto this API's shape.
func toolChoiceFor(req domain.ChatRequest) any {
	if req.ToolName != "" {
		return map[string]any{"type": "tool", "name": req.ToolName}
	}
	switch req.ToolChoice {
	case domain.ToolChoiceNone:
		// There is no "none": omitting the tools is how this API disables them.
		return nil
	case domain.ToolChoiceRequired:
		return map[string]any{"type": "any"}
	default:
		return map[string]any{"type": "auto"}
	}
}

func modelFor(configured, requested string) string {
	if requested != "" {
		return requested
	}
	return configured
}
