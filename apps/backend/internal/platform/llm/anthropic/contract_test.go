package anthropic_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/llm/anthropic"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/llm/llmtest"
)

const testModel = "claude-3-5-sonnet-20241022"

// TestContract runs the same suite the [OI]-compatible adapter runs. A provider
// that passes this is interchangeable from the worker's point of view.
func TestContract(t *testing.T) {
	llmtest.RunContract(t, llmtest.Adapter{
		Name:  domain.AdapterAnthropic,
		Model: testModel,
		NewProvider: func(t *testing.T, baseURL string) domain.Provider {
			t.Helper()

			client, err := anthropic.New(anthropic.Config{
				BaseURL:       baseURL,
				APIKey:        "test-key",
				Model:         testModel,
				SupportsTools: true,
				HTTPClient:    &http.Client{Timeout: 10 * time.Second},
			})
			require.NoError(t, err)
			return client
		},
		Fixture: func(t *testing.T, scenario string) []byte {
			t.Helper()
			return llmtest.RecordedFixture(t, "anthropic", scenario)
		},
		DecodeRequest: decodeAnthropicRequest,
	})
}

// decodeAnthropicRequest reads this format's request into the neutral view.
// Where the two formats differ, the decoding differs; the assertions above them
// are the same.
func decodeAnthropicRequest(t *testing.T, body []byte) llmtest.SentRequest {
	t.Helper()

	var payload struct {
		Model      string          `json:"model"`
		MaxTokens  int             `json:"max_tokens"`
		Stream     bool            `json:"stream"`
		ToolChoice any             `json:"tool_choice"`
		System     json.RawMessage `json:"system"`
		Messages   []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(body, &payload))

	sent := llmtest.SentRequest{
		Model:     payload.Model,
		MaxTokens: payload.MaxTokens,
		Stream:    payload.Stream,
		Raw:       map[string]any{},
	}
	_ = json.Unmarshal(body, &sent.Raw)

	sent.System, sent.PromptCached = systemText(t, payload.System)

	for _, message := range payload.Messages {
		decoded := llmtest.SentMessage{Role: message.Role}

		blocks, ok := decodeBlocks(message.Content)
		if !ok {
			// Plain string content.
			_ = json.Unmarshal(message.Content, &decoded.Text)
			sent.Messages = append(sent.Messages, decoded)
			continue
		}

		for _, block := range blocks {
			switch block.Type {
			case "text":
				decoded.Text += block.Text
			case "image":
				decoded.Images++
			case "tool_use":
				decoded.ToolCalls = append(decoded.ToolCalls, llmtest.SentToolCall{
					ID:        block.ID,
					Name:      block.Name,
					Arguments: string(block.Input),
				})
			case "tool_result":
				decoded.ToolResults = append(decoded.ToolResults, llmtest.SentToolResult{
					CallID:  block.ToolUseID,
					Content: block.Content,
					IsError: block.IsError,
				})
			}
		}

		// Tool answers arrive as a user message here; the suite asserts on the
		// results, so the role is reported as the format's own.
		sent.Messages = append(sent.Messages, decoded)
	}

	for _, tool := range payload.Tools {
		sent.Tools = append(sent.Tools, tool.Name)
	}

	switch choice := payload.ToolChoice.(type) {
	case map[string]any:
		kind, _ := choice["type"].(string)
		switch kind {
		case "tool":
			name, _ := choice["name"].(string)
			sent.ToolChoice = name
		case "any":
			sent.ToolChoice = "required"
		default:
			sent.ToolChoice = kind
		}
	}

	return sent
}

type wireBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   string          `json:"content"`
	IsError   bool            `json:"is_error"`
}

func decodeBlocks(raw json.RawMessage) ([]wireBlock, bool) {
	var blocks []wireBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, false
	}
	return blocks, true
}

// systemText reads the system prompt, which this API carries as its own field
// and may wrap in a cacheable block.
func systemText(t *testing.T, raw json.RawMessage) (string, bool) {
	t.Helper()

	if len(raw) == 0 {
		return "", false
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, false
	}

	var blocks []struct {
		Type         string `json:"type"`
		Text         string `json:"text"`
		CacheControl *struct {
			Type string `json:"type"`
		} `json:"cache_control"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", false
	}

	result := ""
	cached := false
	for _, block := range blocks {
		result += block.Text
		if block.CacheControl != nil && block.CacheControl.Type == "ephemeral" {
			cached = true
		}
	}
	return result, cached
}
