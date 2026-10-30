package openai_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/llm/llmtest"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/llm/openai"
)

const testModel = "gpt-4o-mini"

// TestContract runs the shared suite against this adapter. Every provider must
// pass it unchanged, which is what makes a provider swap a configuration change.
func TestContract(t *testing.T) {
	llmtest.RunContract(t, llmtest.Adapter{
		Name:  domain.AdapterOpenAI,
		Model: testModel,
		NewProvider: func(t *testing.T, baseURL string) domain.Provider {
			t.Helper()

			client, err := openai.New(openai.Config{
				BaseURL:       baseURL,
				APIKey:        "test-key",
				Model:         testModel,
				SupportsTools: true,
				HTTPClient:    serverClient(t),
			})
			require.NoError(t, err)
			return client
		},
		Fixture: func(t *testing.T, scenario string) []byte {
			t.Helper()
			return llmtest.RecordedFixture(t, testModelDir(), scenario)
		},
		DecodeRequest: decodeOpenAIRequest,
	})
}

func testModelDir() string { return "openai" }

// serverClient keeps a stuck stub from hanging the suite.
func serverClient(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Timeout: 10 * time.Second}
}

// decodeOpenAIRequest reads this format's request body into the neutral view the
// suite asserts against.
func decodeOpenAIRequest(t *testing.T, body []byte) llmtest.SentRequest {
	t.Helper()

	var payload struct {
		Model      string `json:"model"`
		MaxTokens  int    `json:"max_tokens"`
		Stream     bool   `json:"stream"`
		ToolChoice any    `json:"tool_choice"`
		System     string `json:"-"`
		Messages   []struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
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

	// The system prompt is the first message in this format.
	if len(payload.Messages) > 0 && payload.Messages[0].Role == "system" {
		sent.System = contentText(t, payload.Messages[0].Content)
		payload.Messages = payload.Messages[1:]
	}

	for _, message := range payload.Messages {
		decoded := llmtest.SentMessage{Role: message.Role, Text: contentText(t, message.Content), Images: contentImages(message.Content)}

		if message.Role == "tool" {
			decoded.ToolResults = []llmtest.SentToolResult{{
				CallID:  message.ToolCallID,
				Content: decoded.Text,
				IsError: false,
			}}
			decoded.Text = ""
		}

		for _, call := range message.ToolCalls {
			decoded.ToolCalls = append(decoded.ToolCalls, llmtest.SentToolCall{
				ID:        call.ID,
				Name:      call.Function.Name,
				Arguments: call.Function.Arguments,
			})
		}

		sent.Messages = append(sent.Messages, decoded)
	}

	for _, tool := range payload.Tools {
		sent.Tools = append(sent.Tools, tool.Function.Name)
	}

	switch choice := payload.ToolChoice.(type) {
	case string:
		sent.ToolChoice = choice
	case map[string]any:
		if function, ok := choice["function"].(map[string]any); ok {
			name, _ := function["name"].(string)
			sent.ToolChoice = name
		}
	}

	return sent
}

// contentText reads either a plain string or the multimodal parts array.
func contentText(t *testing.T, raw json.RawMessage) string {
	t.Helper()

	if len(raw) == 0 {
		return ""
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}

	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}

	result := ""
	for _, part := range parts {
		if part.Type == "text" {
			result += part.Text
		}
	}
	return result
}

// contentImages counts the pictures in the multimodal parts array, which is what
// tells a refusal of the picture apart from a refusal of the text beside it.
func contentImages(raw json.RawMessage) int {
	var parts []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return 0
	}

	images := 0
	for _, part := range parts {
		if part.Type == "image_url" {
			images++
		}
	}
	return images
}
