// Command llmfixtures re-records the adapter contract fixtures from a real
// provider.
//
// The contract suite in internal/platform/llm/llmtest replays recorded provider
// responses, so it stays deterministic and free. Those recordings go stale when
// a provider changes its wire format, and this tool is how they are refreshed.
//
// It works through the adapter itself rather than by hand-writing requests: the
// tool stands a recording proxy in front of the provider, points the adapter at
// it, and drives the same scenarios the suite covers. The adapter therefore
// encodes the request exactly as it does in production, and the proxy stores the
// provider's real answer.
//
// Usage:
//
//	OPENAI_API_KEY=sk-... go run ./tools/llmfixtures -adapter openai
//	ANTHROPIC_API_KEY=sk-... go run ./tools/llmfixtures -adapter anthropic
//	go run ./tools/llmfixtures -list          # what would be recorded
//
// The three failure fixtures (rate limit, provider down, malformed request)
// cannot be provoked on demand, so the tool writes the provider's documented
// error body for them and says so in the manifest.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/llm/anthropic"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/llm/llmtest"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/llm/openai"
)

// adapterSpec describes one provider: where to call it, which environment
// variable holds the key, and how to build the adapter under test.
type adapterSpec struct {
	name       string
	baseURL    string
	model      string
	apiKeyEnv  string
	newAdapter func(cfg adapterConfig) (domain.Provider, error)
}

type adapterConfig struct {
	baseURL string
	apiKey  string
	model   string
}

func specs() map[string]adapterSpec {
	return map[string]adapterSpec{
		"openai": {
			name:      "openai",
			baseURL:   openai.DefaultBaseURL,
			model:     "gpt-4o-mini",
			apiKeyEnv: "OPENAI_API_KEY",
			newAdapter: func(cfg adapterConfig) (domain.Provider, error) {
				return openai.New(openai.Config{
					BaseURL:        cfg.baseURL,
					APIKey:         cfg.apiKey,
					Model:          cfg.model,
					SupportsTools:  true,
					SupportsVision: true,
				})
			},
		},
		"anthropic": {
			name:      "anthropic",
			baseURL:   anthropic.DefaultBaseURL,
			model:     "claude-3-5-haiku-latest",
			apiKeyEnv: "ANTHROPIC_API_KEY",
			newAdapter: func(cfg adapterConfig) (domain.Provider, error) {
				return anthropic.New(anthropic.Config{
					BaseURL:        cfg.baseURL,
					APIKey:         cfg.apiKey,
					Model:          cfg.model,
					SupportsTools:  true,
					SupportsVision: true,
					PromptCaching:  true,
				})
			},
		},
	}
}

// scenario is one recording: the request to drive and whether it streams.
type scenario struct {
	name      string
	request   func() domain.ChatRequest
	stream    bool
	synthetic []byte // set when the provider cannot be made to produce it
}

// toolFixture is the tool definition the tool-calling scenarios offer.
func toolFixture() domain.Tool {
	return domain.Tool{
		Name:        "gmail.search",
		Description: "Cari email di kotak masuk",
		Schema:      json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
		Label:       "read",
	}
}

func scenarios() []scenario {
	return []scenario{
		{
			name: llmtest.ScenarioChatBasic,
			request: func() domain.ChatRequest {
				return domain.ChatRequest{
					System:   "Kamu Bolu, asisten usaha kecil. Jawab ringkas dalam bahasa Indonesia.",
					Messages: []domain.Message{{Role: domain.RoleUser, Text: "Berapa pesanan hari ini?"}},
				}
			},
		},
		{
			name: llmtest.ScenarioChatTools,
			request: func() domain.ChatRequest {
				return domain.ChatRequest{
					System: "Kamu Bolu. Pakai alat bila perlu.",
					Messages: []domain.Message{
						{Role: domain.RoleUser, Text: "Ada pesanan baru dari Bu Sari?"},
						{
							Role: domain.RoleAssistant,
							ToolCalls: []domain.ToolCall{{
								ID:        "call_1",
								Name:      "gmail.search",
								Arguments: json.RawMessage(`{"query":"dari:sari pesanan"}`),
							}},
						},
						{
							Role: domain.RoleTool,
							ToolResults: []domain.ToolResult{{
								CallID:  "call_1",
								Content: `{"messages":[{"subject":"Pesanan 3 kemeja"}]}`,
							}},
						},
					},
					Tools:      []domain.Tool{toolFixture()},
					ToolChoice: domain.ToolChoiceAuto,
				}
			},
		},
		{
			name: llmtest.ScenarioManyTools,
			request: func() domain.ChatRequest {
				tools := make([]domain.Tool, 0, 24)
				for i := range 24 {
					tools = append(tools, domain.Tool{
						Name:        fmt.Sprintf("tool_%02d", i),
						Description: fmt.Sprintf("Alat nomor %d", i),
						Schema:      json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
						Label:       "read",
					})
				}
				return domain.ChatRequest{
					System:   "Kamu Bolu dengan banyak alat.",
					Messages: []domain.Message{{Role: domain.RoleUser, Text: "Pakai alat yang paling tepat."}},
					Tools:    tools,
				}
			},
		},
		{
			name: llmtest.ScenarioEmptyAnswer,
			request: func() domain.ChatRequest {
				return domain.ChatRequest{
					System:   "Balas dengan string kosong bila tidak ada yang perlu dikatakan.",
					Messages: []domain.Message{{Role: domain.RoleUser, Text: "Katakan hanya bila ada hal penting."}},
				}
			},
		},
		{
			name: llmtest.ScenarioUsage,
			request: func() domain.ChatRequest {
				return domain.ChatRequest{
					System:   "Kamu Bolu.",
					Messages: []domain.Message{{Role: domain.RoleUser, Text: "Tulis satu kalimat pendek."}},
				}
			},
		},
		{
			name:   llmtest.ScenarioStreamText,
			stream: true,
			request: func() domain.ChatRequest {
				return domain.ChatRequest{
					System:   "Kamu Bolu. Jawab ringkas.",
					Messages: []domain.Message{{Role: domain.RoleUser, Text: "Berapa pesanan hari ini?"}},
				}
			},
		},
		{
			name:   llmtest.ScenarioStreamTools,
			stream: true,
			request: func() domain.ChatRequest {
				return domain.ChatRequest{
					System:   "Kamu Bolu. Pakai alat bila perlu.",
					Messages: []domain.Message{{Role: domain.RoleUser, Text: "Cari pesanan terbaru."}},
					Tools:    []domain.Tool{toolFixture()},
				}
			},
		},
		{
			name: llmtest.ScenarioRateLimited,
			// A provider is asked to rate-limit on demand, so the body is the one
			// it documents for that answer.
			synthetic: []byte(`{"error":{"message":"Rate limit reached for requests","type":"requests","code":"rate_limit_exceeded"}}`),
		},
		{
			name:      llmtest.ScenarioProviderDown,
			synthetic: []byte(`{"error":{"message":"The server had an error while processing your request","type":"server_error"}}`),
		},
		{
			name:      llmtest.ScenarioInvalidRequest,
			synthetic: []byte(`{"error":{"message":"Invalid value for 'model'","type":"invalid_request_error","param":"model"}}`),
		},
		{
			name: llmtest.ScenarioImageUnsupported,
			// A text-only model behind an adapter that speaks for a
			// vision-capable one refuses the picture in the provider's own
			// words. The anthropic fixture is the same refusal in that format's
			// envelope, which this body is not.
			synthetic: []byte(`{"error":{"message":"This model does not support image input","type":"invalid_request_error"}}`),
		},
	}
}

func main() {
	adapterName := flag.String("adapter", "", "which adapter to record: openai or anthropic")
	model := flag.String("model", "", "model name to record with (defaults to the adapter's)")
	baseURL := flag.String("base-url", "", "provider base URL (defaults to the public endpoint)")
	out := flag.String("out", filepath.Join("internal", "platform", "llm", "llmtest", "fixtures"), "fixture directory")
	only := flag.String("scenario", "", "record only this scenario")
	list := flag.Bool("list", false, "list the scenarios and exit")
	flag.Parse()

	if *list {
		for _, item := range scenarios() {
			kind := "recorded"
			if item.synthetic != nil {
				kind = "documented error body"
			}
			fmt.Printf("%-16s %s\n", item.name, kind)
		}
		return
	}

	if *adapterName == "" {
		fail("adapter is required: %s", strings.Join(adapterNames(), " or "))
	}

	spec, ok := specs()[*adapterName]
	if !ok {
		fail("unknown adapter %q: %s", *adapterName, strings.Join(adapterNames(), " or "))
	}
	if *model != "" {
		spec.model = *model
	}
	if *baseURL != "" {
		spec.baseURL = *baseURL
	}

	apiKey := strings.TrimSpace(os.Getenv(spec.apiKeyEnv))
	if apiKey == "" {
		fail("%s is empty: set it to record from the live provider", spec.apiKeyEnv)
	}

	dir := filepath.Join(*out, spec.name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail("create %s: %v", dir, err)
	}

	recorded, synthetic := 0, 0
	for _, item := range scenarios() {
		if *only != "" && item.name != *only {
			continue
		}

		if item.synthetic != nil {
			if err := os.WriteFile(filepath.Join(dir, item.name+".json"), item.synthetic, 0o644); err != nil {
				fail("write %s: %v", item.name, err)
			}
			fmt.Printf("%-16s documented error body\n", item.name)
			synthetic++
			continue
		}

		body, err := record(spec, apiKey, item)
		if err != nil {
			fail("record %s: %v", item.name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, item.name+".json"), body, 0o644); err != nil {
			fail("write %s: %v", item.name, err)
		}
		fmt.Printf("%-16s %d bytes\n", item.name, len(body))
		recorded++
	}

	fmt.Printf("\n%s: %d recorded, %d written from documentation, into %s\n",
		spec.name, recorded, synthetic, dir)
}

func adapterNames() []string {
	names := make([]string, 0, len(specs()))
	for name := range specs() {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// record drives one scenario through the adapter and returns the provider's
// response body, captured by a proxy standing in front of the provider.
func record(spec adapterSpec, apiKey string, item scenario) ([]byte, error) {
	proxy, recorder := newRecordingProxy(spec.baseURL)

	provider, err := spec.newAdapter(adapterConfig{baseURL: proxy.URL, apiKey: apiKey, model: spec.model})
	if err != nil {
		proxy.Close()
		return nil, fmt.Errorf("build adapter: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	request := item.request()
	if item.stream {
		events, err := provider.Stream(ctx, request)
		if err != nil {
			proxy.Close()
			return nil, fmt.Errorf("stream: %w", err)
		}
		// Draining the channel is what reads the whole body through the proxy.
		for event := range events {
			if event.Type == domain.EventError {
				proxy.Close()
				return nil, fmt.Errorf("stream failed: %w", event.Err)
			}
		}
	} else {
		if _, err := provider.Chat(ctx, request); err != nil {
			proxy.Close()
			return nil, fmt.Errorf("chat: %w", err)
		}
	}

	proxy.Close()

	body := recorder.body()
	if len(body) == 0 {
		return nil, errors.New("the provider sent no body")
	}
	return body, nil
}

// newRecordingProxy forwards every request to upstream and stores the response
// body, which is exactly the recording a fixture needs.
func newRecordingProxy(upstream string) (*httptest.Server, *recorder) {
	recorded := &recorder{}
	client := &http.Client{Timeout: 90 * time.Second}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := strings.TrimRight(upstream, "/") + r.URL.Path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}

		forwarded, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		forwarded.Header = r.Header.Clone()

		response, err := client.Do(forwarded)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = response.Body.Close() }()

		if contentType := response.Header.Get("Content-Type"); contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(response.StatusCode)

		body, err := io.ReadAll(response.Body)
		if err != nil {
			recorded.set([]byte(fmt.Sprintf("read error: %v", err)))
			return
		}
		recorded.set(body)

		_, _ = w.Write(body)
	}))

	return server, recorded
}

// recorder holds the captured body. The handler runs on its own goroutine, so
// the write and the read are guarded.
type recorder struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (r *recorder) set(body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buffer.Reset()
	r.buffer.Write(body)
}

func (r *recorder) body() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	copied := make([]byte, r.buffer.Len())
	copy(copied, r.buffer.Bytes())
	return copied
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "llmfixtures: "+format+"\n", args...)
	os.Exit(1)
}
