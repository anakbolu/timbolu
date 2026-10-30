package llm

import (
	"fmt"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/llm/anthropic"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/llm/openai"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/llm/vision"
)

// Factory turns a stored configuration into a ready-to-call adapter.
//
// It is the Factory the llm domain declares, which is what keeps the module
// unaware of the adapters: adding a provider is a change in this package and in
// the migration's check constraint, nowhere else.
type Factory struct {
	// Vision is the deployment's list of models that cannot read a picture. A
	// nil policy keeps the wire format's own default, which is that a model sees.
	Vision *vision.Policy
}

// Build implements domain.Factory.
func (f Factory) Build(cfg domain.ProviderConfig) (domain.Provider, error) {
	return build(cfg, f.Vision.Sees(cfg.Model))
}

// build assembles the adapter for one configuration. Whether the model sees is
// decided by the caller, because it is a fact about the model and not about the
// wire format the adapter speaks.
func build(cfg domain.ProviderConfig, sees bool) (domain.Provider, error) {
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
			SupportsVision:    sees,
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
			SupportsVision: sees,
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
