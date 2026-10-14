package container

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	llmservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/service"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/llm"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/llm/pricing"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/quota"
	"github.com/robzlabz/employeebot/packages/platform/config"
	"github.com/robzlabz/employeebot/packages/platform/crypto"
)

// openLLM builds the model gateway: the provider configuration, the adapters,
// and the usage accounting.
//
// Everything here is an adapter between a platform implementation and a port the
// llm domain declares, which is why this file is the only one that knows both
// sides. Without a database the feature stays unwired and its routes answer 503,
// the same way the other modules behave.
func (c *Container) openLLM(_ context.Context, cfg *config.Config) error {
	if c.Repositories == nil || c.Repositories.LLM == nil {
		c.Logger.Warn("model gateway is disabled: no database connection")
		return nil
	}

	box, err := openSecretBox(cfg)
	if err != nil {
		return err
	}
	if box == nil {
		c.Logger.Warn("SECRET_ENCRYPTION_KEY is empty: provider API keys cannot be stored")
	}

	// The per-Bolu override lives in agents.default_model, so it is read and
	// written by a store that holds the encryption, not by the agent module.
	if c.Repositories.LLMAgents == nil {
		c.Repositories.LLMAgents = c.Repositories.LLM.NewAgentStore(box)
	}

	// The price table is kept on the container as well as handed to the gateway:
	// the task runtime prices its own steps with the same table, so a task's
	// total and the ledger cannot disagree about what a token cost.
	c.Costs = pricing.New(pricing.Default(), pricing.DefaultFallback)

	deps := llmservice.Deps{
		// The tracer is the process's provider, so a model call made inside a workflow is a
		// child of that workflow's span rather than a new trace per call.
		Tracer:   c.telemetryTracer(),
		Metrics:  c.metricsFor(),
		Store:    c.Repositories.LLM,
		Agents:   c.Repositories.LLMAgents,
		Factory:  llm.Factory{},
		Box:      secretBox(box),
		Recorder: c.Repositories.LLM,
		Reader:   c.Repositories.LLM,
		Costs:    c.Costs,
		Default:  platformProvider(cfg),
		Settings: llmservice.Config{
			QuotaEnabled: cfg.Llm.QuotaEnabled,
			ChainLimit:   cfg.Llm.ChainLimit,
		},
	}

	if c.Redis != nil {
		deps.Counter = quota.New(c.Redis.Raw())
	}
	if cfg.Llm.QuotaEnabled {
		if deps.Counter == nil {
			// Without the live counter the check still works: it falls back to
			// the ledger, which is slower but correct.
			c.Logger.Warn("quota is enabled but redis is not configured: every check reads the ledger")
		}
		// The subscription is what a workspace may spend, when this deployment sells
		// packages at all. The configured allowance is what a deployment without
		// billing runs with, which is why it stays: a self-hosted installation that
		// wants one bound for everybody should not have to seed a plan to get it.
		if c.Billing != nil {
			deps.Quota = billingQuotaPolicy{service: c.Billing}
		} else {
			deps.Quota = fixedAllowance{
				tokens:      cfg.Llm.TokensPerPeriod,
				dailyMicros: cfg.Plan.DailyCostMicros,
			}
		}
	}

	c.Services.LLM = llmservice.New(deps)

	return nil
}

// openSecretBox parses the configured encryption key. A missing key is not an
// error at startup: the API runs, and only storing a provider key is refused.
func openSecretBox(cfg *config.Config) (*crypto.Box, error) {
	if strings.TrimSpace(cfg.Llm.SecretEncryptionKey) == "" {
		return nil, nil
	}

	box, err := crypto.NewFromString(cfg.Llm.SecretEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("parse SECRET_ENCRYPTION_KEY: %w", err)
	}
	return box, nil
}

// platformProvider is the provider the platform offers a workspace that
// configured none of its own.
func platformProvider(cfg *config.Config) llmdomain.ProviderConfig {
	def := cfg.Llm.Default
	return llmdomain.ProviderConfig{
		Name:          "platform",
		Adapter:       strings.TrimSpace(def.Adapter),
		BaseURL:       strings.TrimSpace(def.BaseURL),
		Model:         strings.TrimSpace(def.Model),
		APIKey:        strings.TrimSpace(def.APIKey),
		MaxTokens:     def.MaxTokens,
		ContextTokens: def.ContextTokens,
		Priority:      0,
		Enabled:       true,
	}
}

// secretBox adapts the crypto box to the port the llm domain declares. A nil box
// stays nil, so the service can tell "no key configured" from "encryption
// failed" instead of calling a method on a nil interface value.
func secretBox(box *crypto.Box) llmdomain.SecretBox {
	if box == nil {
		return nil
	}
	return box
}

// fixedAllowance is the quota policy before subscriptions exist: every workspace
// of the deployment gets the same period allowance and the same daily ceiling,
// and zero on either means unlimited. EPIC 12 (#97) replaces it with the
// subscription's package.
type fixedAllowance struct {
	tokens int64
	// dailyMicros is the day's cost ceiling, in micro-rupiah.
	dailyMicros int64
}

// Allowance implements the quota policy port. A zero allowance means unlimited,
// which is what a deployment without subscriptions runs with.
func (f fixedAllowance) Allowance(context.Context, uuid.UUID) (int64, error) {
	return f.tokens, nil
}

// DailyCostAllowance implements the quota policy port for the day's ceiling.
func (f fixedAllowance) DailyCostAllowance(context.Context, uuid.UUID) (int64, error) {
	return f.dailyMicros, nil
}

// compile-time checks that the adapters satisfy the ports they are wired to.
var (
	_ llmdomain.SecretBox   = (*crypto.Box)(nil)
	_ llmdomain.QuotaPolicy = fixedAllowance{}
	_ llmdomain.Factory     = llm.Factory{}
)
