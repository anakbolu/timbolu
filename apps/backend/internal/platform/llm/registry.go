package llm

import (
	"fmt"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/llm/anthropic"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/llm/openai"
)

// Build turns a stored configuration into a ready-to-call adapter.
//
// It is the Factory the llm domain declares, which is what keeps the module
// unaware of the adapters: adding a provider is a change in this package and in
// the migration's check constraint, nowhere else.
func Build(cfg domain.ProviderConfig) (domain.Provider, error) {
	switch cfg.Adapter {
	case domain.AdapterOpenAI:
		// One adapter serves every OpenAI-compatible endpoint — OpenAI itself,
		// OpenRouter, Groq, DeepSeek, vLLM, Ollama — because they share the
		// wire format. The base URL decides which one is called.
		return openai.New(openai.Config{
			BaseURL:           cfg.BaseURL,
			APIKey:            cfg.APIKey,
			Model:             cfg.Model,
			MaxTokens:         cfg.MaxTokens,
			ContextTokens:     cfg.ContextTokens,
			SupportsTools:     true,
			SupportsVision:    true,
			ParallelToolCalls: true,
			// The format defines an embeddings route; the workspace's key is what
			// embeds, so memory and knowledge retrieval work without a second
			// configuration.
			SupportsEmbeddings: true,
		})

	case domain.AdapterAnthropic:
		return anthropic.New(anthropic.Config{
			BaseURL:        cfg.BaseURL,
			APIKey:         cfg.APIKey,
			Model:          cfg.Model,
			MaxTokens:      cfg.MaxTokens,
			ContextTokens:  cfg.ContextTokens,
			SupportsTools:  true,
			SupportsVision: true,
			// The adapter emits cache breakpoints on the stable prefix, which is
			// what makes the persona and the main memory cheap on every turn.
			PromptCaching: true,
		})

	default:
		return nil, fmt.Errorf("%w: unknown adapter %q", domain.ErrInvalidRequest, cfg.Adapter)
	}
}

// compile-time check: the package satisfies the port the module declares.
var _ domain.Factory = Factory{}

// Factory adapts Build to the domain's Factory interface.
type Factory struct{}

// Build implements domain.Factory.
func (Factory) Build(cfg domain.ProviderConfig) (domain.Provider, error) { return Build(cfg) }
