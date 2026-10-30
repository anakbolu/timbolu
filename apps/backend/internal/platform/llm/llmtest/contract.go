// Package llmtest holds the contract every LLM adapter must satisfy.
//
// One suite is written once and run against each adapter, so "swap the provider"
// is a configuration change rather than a rewrite. The suite drives the adapter
// against a stub server that replays recorded provider responses: the same
// scenario, the same assertions, both wire formats.
//
// The stub is built from fixtures rather than from a live provider, so the tests
// are deterministic and free. tools/llmfixtures re-records them from a real
// endpoint when a key is available.
package llmtest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
)

// Scenario names. Each adapter keeps a fixture per scenario.
const (
	ScenarioChatBasic        = "chat_basic"
	ScenarioChatTools        = "chat_tools"
	ScenarioStreamText       = "stream_text"
	ScenarioStreamTools      = "stream_tools"
	ScenarioManyTools        = "many_tools"
	ScenarioEmptyAnswer      = "empty_answer"
	ScenarioRateLimited      = "rate_limited"
	ScenarioProviderDown     = "provider_down"
	ScenarioInvalidRequest   = "invalid_request"
	ScenarioImageUnsupported = "image_unsupported"
	ScenarioUsage            = "usage_reporting"
)

// FixtureDir is where the recorded responses live, relative to this package.
const FixtureDir = "fixtures"

// SentRequest is the provider-neutral view of what an adapter actually sent.
// Each adapter decodes its own wire format into this shape, which is what lets
// one suite assert the translation for both.
type SentRequest struct {
	// System is the system prompt, however the provider carries it.
	System string
	// Messages in order, with the role the provider used translated back.
	Messages []SentMessage
	// Tools offered to the model.
	Tools []string
	// ToolChoice as the provider understood it: auto, none, required, or a name.
	ToolChoice string
	Model      string
	MaxTokens  int
	Stream     bool
	// Raw is the decoded request body, for an adapter-specific assertion.
	Raw map[string]any

	// PromptCached reports whether the adapter marked the stable prefix for
	// caching. Only providers that advertise the capability do.
	PromptCached bool
}

// SentMessage is one message as the provider received it.
type SentMessage struct {
	Role string
	Text string
	// Images is how many pictures the message carried. A refusal of those
	// pictures is what the suite tells apart from a refusal of the text.
	Images int
	// ToolCalls the assistant asked for.
	ToolCalls []SentToolCall
	// ToolResults the caller answered with.
	ToolResults []SentToolResult
}

// SentToolCall is a function call in the provider's request.
type SentToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// SentToolResult is a tool answer in the provider's request.
type SentToolResult struct {
	CallID  string
	Content string
	IsError bool
}

// Adapter is what an implementation supplies to run the contract.
type Adapter struct {
	// Name is used in the test names, e.g. "openai".
	Name string
	// Model the adapter is configured with.
	Model string
	// NewProvider builds the adapter under test pointed at the stub.
	NewProvider func(t *testing.T, baseURL string) domain.Provider
	// Fixture returns the recorded response body for a scenario. The suite wires
	// it into the stub server itself.
	Fixture func(t *testing.T, scenario string) []byte
	// DecodeRequest turns the captured request body into the neutral view.
	DecodeRequest func(t *testing.T, body []byte) SentRequest
}

// RunContract runs every scenario against one adapter.
func RunContract(t *testing.T, adapter Adapter) {
	t.Helper()

	t.Run("chat", func(t *testing.T) {
		t.Run("basic answer", func(t *testing.T) { testChatBasic(t, adapter) })
		t.Run("tool call round trip", func(t *testing.T) { testChatToolRoundTrip(t, adapter) })
		t.Run("many tools", func(t *testing.T) { testManyTools(t, adapter) })
		t.Run("empty answer", func(t *testing.T) { testEmptyAnswer(t, adapter) })
		t.Run("usage is reported", func(t *testing.T) { testUsage(t, adapter) })
	})

	t.Run("errors", func(t *testing.T) {
		t.Run("rate limited", func(t *testing.T) { testRateLimited(t, adapter) })
		t.Run("provider unavailable", func(t *testing.T) { testProviderDown(t, adapter) })
		t.Run("invalid request", func(t *testing.T) { testInvalidRequest(t, adapter) })
		t.Run("image input refused", func(t *testing.T) { testImageUnsupported(t, adapter) })
	})

	t.Run("stream", func(t *testing.T) {
		t.Run("text", func(t *testing.T) { testStreamText(t, adapter) })
		t.Run("tool call", func(t *testing.T) { testStreamToolCall(t, adapter) })
		t.Run("truncated stream still ends", func(t *testing.T) { testStreamWithoutTerminator(t, adapter) })
	})
}

// stub serves one recorded response.
type stub struct {
	server   *httptest.Server
	requests []SentRequest
	bodies   [][]byte
	status   int
	headers  map[string]string
}

// newStub starts a server that answers every request with the same body.
func newStub(t *testing.T, adapter Adapter, body []byte, status int, headers map[string]string) *stub {
	t.Helper()

	s := &stub{status: status, headers: headers}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		s.bodies = append(s.bodies, raw)
		s.requests = append(s.requests, adapter.DecodeRequest(t, raw))

		for key, value := range s.headers {
			w.Header().Set(key, value)
		}
		if s.status != 0 && s.status >= 400 {
			w.WriteHeader(s.status)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.server.Close)

	return s
}

// last returns the most recent request the adapter sent.
func (s *stub) last(t *testing.T) SentRequest {
	t.Helper()

	require.NotEmpty(t, s.requests, "the adapter sent no request")
	return s.requests[len(s.requests)-1]
}

// provider builds the adapter under test against the stub.
func (s *stub) provider(t *testing.T, adapter Adapter) domain.Provider {
	t.Helper()
	return adapter.NewProvider(t, s.server.URL)
}

// streamingStub answers with a recorded SSE body.
func streamingStub(t *testing.T, adapter Adapter, body []byte) *stub {
	t.Helper()
	return newStub(t, adapter, body, http.StatusOK, map[string]string{"Content-Type": "text/event-stream"})
}

// baseRequest is the request the suite reuses, with room to change one thing.
func baseRequest() domain.ChatRequest {
	temperature := 0.2
	return domain.ChatRequest{
		System: "Kamu Bolu, asisten kantor.",
		Messages: []domain.Message{
			{Role: domain.RoleUser, Text: "Rekap pesanan hari ini."},
		},
		MaxTokens:   256,
		Temperature: &temperature,
	}
}

func tool(name, description, label string) domain.Tool {
	return domain.Tool{
		Name:        name,
		Description: description,
		Label:       label,
		Schema:      json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
	}
}

func testChatBasic(t *testing.T, adapter Adapter) {
	t.Helper()

	t.Helper()

	stubServer := newStub(t, adapter, adapter.Fixture(t, ScenarioChatBasic), http.StatusOK, map[string]string{"Content-Type": "application/json"})
	provider := stubServer.provider(t, adapter)

	response, err := provider.Chat(context.Background(), baseRequest())
	require.NoError(t, err)

	require.NotEmpty(t, response.Text, "the answer must carry the model's text")
	require.False(t, response.HasToolCalls())
	require.Equal(t, domain.FinishStop, response.FinishReason)
	require.Equal(t, adapter.Name, response.Provider, "the response must name the adapter that answered")
	require.NotEmpty(t, response.Model)

	// The translation in the other direction: the provider must receive the
	// system prompt, the conversation, and the model.
	sent := stubServer.last(t)
	require.Equal(t, baseRequest().System, sent.System)
	require.Len(t, sent.Messages, 1)
	require.Equal(t, "user", sent.Messages[0].Role)
	require.Equal(t, "Rekap pesanan hari ini.", sent.Messages[0].Text)
	require.Equal(t, adapter.Model, sent.Model)
	require.False(t, sent.Stream)
}

// testChatToolRoundTrip is the case that breaks most adapters: an assistant tool
// call answered by a tool result, then a final answer.
func testChatToolRoundTrip(t *testing.T, adapter Adapter) {
	t.Helper()

	t.Helper()

	stubServer := newStub(t, adapter, adapter.Fixture(t, ScenarioChatTools), http.StatusOK, map[string]string{"Content-Type": "application/json"})
	provider := stubServer.provider(t, adapter)

	request := baseRequest()
	request.Tools = []domain.Tool{tool("gmail.search", "Cari email", "read")}
	request.Messages = append(request.Messages,
		domain.Message{
			Role: domain.RoleAssistant,
			ToolCalls: []domain.ToolCall{{
				ID:        "call_1",
				Name:      "gmail.search",
				Arguments: json.RawMessage(`{"query":"pesanan"}`),
			}},
		},
		domain.Message{
			Role: domain.RoleTool,
			ToolResults: []domain.ToolResult{{
				CallID:  "call_1",
				Content: `[{"subject":"Pesanan #12"}]`,
			}},
		},
	)

	response, err := provider.Chat(context.Background(), request)
	require.NoError(t, err)
	require.NotEmpty(t, response.Text)

	sent := stubServer.last(t)
	require.Equal(t, []string{"gmail.search"}, sent.Tools, "the tool must reach the provider")

	// Walk the conversation the provider received, in order.
	require.Len(t, sent.Messages, 3)
	require.Equal(t, "assistant", sent.Messages[1].Role)
	require.Len(t, sent.Messages[1].ToolCalls, 1)
	require.Equal(t, "gmail.search", sent.Messages[1].ToolCalls[0].Name)
	require.JSONEq(t, `{"query":"pesanan"}`, sent.Messages[1].ToolCalls[0].Arguments)

	// The tool result must arrive as a tool answer, not as a user message: a
	// provider that loses this pairing cannot continue the conversation.
	require.Len(t, sent.Messages[2].ToolResults, 1)
	require.Equal(t, "call_1", sent.Messages[2].ToolResults[0].CallID)
	require.Contains(t, sent.Messages[2].ToolResults[0].Content, "Pesanan #12")
}

func testManyTools(t *testing.T, adapter Adapter) {
	t.Helper()

	t.Helper()

	stubServer := newStub(t, adapter, adapter.Fixture(t, ScenarioManyTools), http.StatusOK, map[string]string{"Content-Type": "application/json"})
	provider := stubServer.provider(t, adapter)

	request := baseRequest()
	labels := []string{"read", "write_internal", "write_external"}
	for i, name := range []string{"gmail.search", "gmail.create_draft", "gmail.send", "drive.search", "jira.search"} {
		request.Tools = append(request.Tools, tool(name, "alat "+name, labels[i%len(labels)]))
	}
	request.ToolChoice = domain.ToolChoiceRequired

	response, err := provider.Chat(context.Background(), request)
	require.NoError(t, err)
	require.NotEmpty(t, response.Text)

	sent := stubServer.last(t)
	require.Len(t, sent.Tools, 5, "every tool must reach the provider")
	require.Equal(t, "required", sent.ToolChoice, "a required choice must survive the translation")
}

func testEmptyAnswer(t *testing.T, adapter Adapter) {
	t.Helper()

	stubServer := newStub(t, adapter, adapter.Fixture(t, ScenarioEmptyAnswer), http.StatusOK, map[string]string{"Content-Type": "application/json"})
	provider := stubServer.provider(t, adapter)

	response, err := provider.Chat(context.Background(), baseRequest())
	require.NoError(t, err, "an empty answer is not an error: the provider answered")

	require.Empty(t, response.Text)
	require.False(t, response.HasToolCalls())
}

func testUsage(t *testing.T, adapter Adapter) {
	t.Helper()

	stubServer := newStub(t, adapter, adapter.Fixture(t, ScenarioUsage), http.StatusOK, map[string]string{"Content-Type": "application/json"})
	provider := stubServer.provider(t, adapter)

	response, err := provider.Chat(context.Background(), baseRequest())
	require.NoError(t, err)

	require.Positive(t, response.Usage.InputTokens, "the prompt tokens must be reported")
	require.Positive(t, response.Usage.OutputTokens, "the answer tokens must be reported")
	require.Positive(t, response.Usage.Total())
}

func testRateLimited(t *testing.T, adapter Adapter) {
	t.Helper()

	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		stubServer := newStub(t, adapter, adapter.Fixture(t, ScenarioRateLimited), status, map[string]string{
			"Content-Type": "application/json",
			"Retry-After":  "30",
		})
		provider := stubServer.provider(t, adapter)

		_, err := provider.Chat(context.Background(), baseRequest())
		require.Error(t, err)

		if status == http.StatusTooManyRequests {
			require.ErrorIs(t, err, domain.ErrRateLimited, "a 429 must be reported as rate limiting")
			require.True(t, domain.Retryable(err), "rate limiting is worth a fallback")
		} else {
			require.ErrorIs(t, err, domain.ErrProviderUnavailable)
			require.True(t, domain.Retryable(err))
		}
	}
}

func testProviderDown(t *testing.T, adapter Adapter) {
	t.Helper()

	stubServer := newStub(t, adapter, adapter.Fixture(t, ScenarioProviderDown), http.StatusInternalServerError, map[string]string{"Content-Type": "application/json"})
	provider := stubServer.provider(t, adapter)

	_, err := provider.Chat(context.Background(), baseRequest())
	require.ErrorIs(t, err, domain.ErrProviderUnavailable)
	require.True(t, domain.Retryable(err), "a fallen provider must let the gateway fall back")

	t.Run("connection refused", func(t *testing.T) {
		// A provider that is not listening at all is the same class of failure.
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := dead.URL
		dead.Close()

		offline := adapter.NewProvider(t, url)
		_, err := offline.Chat(context.Background(), baseRequest())
		require.ErrorIs(t, err, domain.ErrProviderUnavailable)
	})
}

func testInvalidRequest(t *testing.T, adapter Adapter) {
	t.Helper()

	stubServer := newStub(t, adapter, adapter.Fixture(t, ScenarioInvalidRequest), http.StatusBadRequest, map[string]string{"Content-Type": "application/json"})
	provider := stubServer.provider(t, adapter)

	_, err := provider.Chat(context.Background(), baseRequest())
	require.ErrorIs(t, err, domain.ErrInvalidRequest)
	require.False(t, domain.Retryable(err),
		"a malformed request fails everywhere, so falling back would only waste a call")
}

// testImageUnsupported is the text-only model behind an adapter that speaks for
// a vision-capable one: the provider answers 400 to a request that carries a
// picture. That is "this route cannot", which the chain answers by asking again
// without the pictures, not "this request is malformed", which fails on every
// provider and is what the 400 means otherwise.
func testImageUnsupported(t *testing.T, adapter Adapter) {
	t.Helper()

	stubServer := newStub(t, adapter, adapter.Fixture(t, ScenarioImageUnsupported), http.StatusBadRequest, map[string]string{"Content-Type": "application/json"})
	provider := stubServer.provider(t, adapter)

	request := baseRequest()
	request.Messages = append(request.Messages, domain.Message{
		Role:   domain.RoleUser,
		Text:   "Stok di foto ini tinggal berapa?",
		Images: []domain.Image{{MediaType: "image/png", Data: base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nlampiran"))}},
	})

	_, err := provider.Chat(context.Background(), request)
	require.Error(t, err)
	require.ErrorIs(t, err, domain.ErrUnsupported,
		"a refused picture is a capability the route lacks, which the chain may answer without")
	require.False(t, domain.Retryable(err),
		"the same picture fails everywhere; what helps is the request without it")

	sent := stubServer.last(t)
	require.Equal(t, 1, sent.Messages[len(sent.Messages)-1].Images,
		"the picture must have reached the provider, which is what it refused")
}

// testStreamText is the streaming contract: text arrives in pieces, the usage
// arrives, and the stream ends with exactly one terminal event.
func testStreamText(t *testing.T, adapter Adapter) {
	t.Helper()

	stubServer := streamingStub(t, adapter, adapter.Fixture(t, ScenarioStreamText))
	provider := stubServer.provider(t, adapter)

	request := baseRequest()
	events, err := provider.Stream(context.Background(), request)
	require.NoError(t, err)

	collected := drain(t, events)

	require.NotEmpty(t, collected.Text, "the streamed text must be reassembled")
	require.Contains(t, collected.Text, "pesanan", "the text must be the model's answer")
	require.Equal(t, 1, collected.Done, "the stream must end exactly once")
	require.Zero(t, collected.Errors)
	require.Equal(t, domain.FinishStop, collected.FinishReason)

	require.True(t, stubServer.last(t).Stream, "the adapter must ask the provider to stream")
}

func testStreamToolCall(t *testing.T, adapter Adapter) {
	t.Helper()

	stubServer := streamingStub(t, adapter, adapter.Fixture(t, ScenarioStreamTools))
	provider := stubServer.provider(t, adapter)

	request := baseRequest()
	request.Tools = []domain.Tool{tool("gmail.search", "Cari email", "read")}

	events, err := provider.Stream(context.Background(), request)
	require.NoError(t, err)

	collected := drain(t, events)

	require.Len(t, collected.ToolCalls, 1, "a streamed tool call must be reported once, complete")

	call := collected.ToolCalls[0]
	require.Equal(t, "gmail.search", call.Name)
	require.NotEmpty(t, call.ID, "the call needs an id to answer it")
	require.True(t, json.Valid(call.Arguments), "the streamed arguments must be valid JSON: %s", call.Arguments)
	require.JSONEq(t, `{"query":"pesanan"}`, string(call.Arguments),
		"the argument fragments must be joined, not truncated")

	require.Equal(t, 1, collected.Done)
	require.Zero(t, collected.Errors)
}

// testStreamWithoutTerminator covers the stream that ends because the connection
// dropped, without the provider's final event. The consumer must still get a
// terminal event, otherwise the chat would spin forever.
func testStreamWithoutTerminator(t *testing.T, adapter Adapter) {
	t.Helper()

	body := adapter.Fixture(t, ScenarioStreamText)
	truncated := truncateStream(body)

	stubServer := streamingStub(t, adapter, truncated)
	provider := stubServer.provider(t, adapter)

	events, err := provider.Stream(context.Background(), baseRequest())
	require.NoError(t, err)

	collected := drain(t, events)

	require.Equal(t, 1, collected.Done, "a stream that ends early must still be reported as finished")
	require.Zero(t, collected.Errors, "a dropped connection after content is a finished answer, not a failure")
}

// truncateStream cuts a recording at the last complete event, which is what a
// dropped connection looks like.
func truncateStream(body []byte) []byte {
	text := string(body)
	if index := strings.LastIndex(text, "\n\n"); index > 0 {
		return []byte(text[:index+2])
	}
	return body
}

// collected is what the suite asserts against.
type collected struct {
	Text         string
	ToolCalls    []domain.ToolCall
	Usage        domain.Usage
	Done         int
	Errors       int
	FinishReason domain.FinishReason
}

// drain consumes a stream and aggregates it, with a deadline so a broken adapter
// fails the test instead of hanging it.
func drain(t *testing.T, events <-chan domain.StreamEvent) collected {
	t.Helper()

	var result collected
	var text strings.Builder
	timeout := time.After(20 * time.Second)

	for {
		select {
		case event, ok := <-events:
			if !ok {
				result.Text = text.String()
				return result
			}
			switch event.Type {
			case domain.EventText:
				text.WriteString(event.Text)
			case domain.EventToolCall:
				require.NotNil(t, event.ToolCall)
				result.ToolCalls = append(result.ToolCalls, *event.ToolCall)
			case domain.EventUsage:
				if event.Usage != nil {
					result.Usage = *event.Usage
				}
			case domain.EventDone:
				result.Done++
				if event.FinishReason != "" {
					result.FinishReason = event.FinishReason
				}
			case domain.EventError:
				result.Errors++
				require.Error(t, event.Err, "an error event must carry the error")
			}
		case <-timeout:
			t.Fatal("the stream never closed")
		}
	}
}

// RecordedFixture reads a fixture file from an adapter's directory.
//
// The path is resolved from this source file, not from the caller's working
// directory: a test in another package would otherwise look for the fixtures
// next to itself.
func RecordedFixture(t *testing.T, dir, scenario string) []byte {
	t.Helper()

	path := filepath.Join(packageDir(t), FixtureDir, dir, scenario+".json")
	body, err := os.ReadFile(path)
	require.NoError(t, err, "missing fixture %s", path)

	return body
}

// packageDir is where this package's fixtures live.
func packageDir(t *testing.T) string {
	t.Helper()

	_, source, _, ok := runtime.Caller(0)
	require.True(t, ok, "cannot locate the llmtest package")
	return filepath.Dir(source)
}

// ErrNoFixture marks a scenario an adapter does not record.
var ErrNoFixture = errors.New("llmtest: no fixture")
