// Package handler exposes the model configuration over HTTP: the provider list,
// the fallback order, the per-Bolu override, and the usage report.
//
// It talks to the domain gateway interface only, and the secret never travels
// back: a response says whether a key is stored, never what it is.
package handler

import (
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/middleware"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/response"
)

// Handler serves the model endpoints.
type Handler struct {
	service domain.Gateway
	// reporter is the usage screen's read. It is a second field rather than a wider Gateway
	// because the two have different consumers: every module that reaches a model fakes the
	// gateway, and none of them reports a period.
	reporter domain.UsageReporter
	log      *zap.Logger
}

// New builds the handler.
func New(service domain.Gateway, log *zap.Logger) *Handler {
	handler := &Handler{service: service, log: log}
	// The same object answers both when the deployment wired it: a gateway that reads usage can
	// read a report about it.
	if reporter, ok := service.(domain.UsageReporter); ok {
		handler.reporter = reporter
	}

	return handler
}

// WithReporter installs the usage reporter, which the container calls after the service is built.
//
// It exists because the handler is constructed from the gateway interface and the container is the
// only place that knows one object implements both — the same shape the auditors are installed with.
func (h *Handler) WithReporter(reporter domain.UsageReporter) *Handler {
	h.reporter = reporter

	return h
}

// Routes registers the endpoints every member of the workspace may read.
func Routes(group fiber.Router, h *Handler) {
	group.Get("/llm/providers", h.List)
	group.Get("/llm/adapters", h.Adapters)
	group.Get("/llm/usage", h.Usage)
	// The usage screen's read, and the export an accountant takes away. Both are reads, so both
	// sit behind the same guard as the rest of this group.
	group.Get("/llm/usage/report", h.Report)
	group.Get("/agents/:agentID/model", h.AgentModel)
}

// ManagerRoutes registers the endpoints that change the configuration. The
// container applies the tenant middleware and a managing-role check.
func ManagerRoutes(group fiber.Router, h *Handler) {
	group.Post("/llm/providers", h.Create)
	group.Put("/llm/providers/:providerID", h.Update)
	group.Delete("/llm/providers/:providerID", h.Delete)
	group.Post("/llm/providers/:providerID/test", h.Test)
	group.Put("/agents/:agentID/model", h.SetAgentModel)
}

type upsertProviderRequest struct {
	Name          string `json:"name"`
	Adapter       string `json:"adapter"`
	BaseURL       string `json:"base_url"`
	Model         string `json:"model"`
	APIKey        string `json:"api_key"`
	ClearAPIKey   bool   `json:"clear_api_key"`
	Priority      int    `json:"priority"`
	MaxTokens     int    `json:"max_tokens"`
	ContextTokens int    `json:"context_tokens"`
	IsDefault     bool   `json:"is_default"`
	// Enabled is optional: omitting it on create means enabled, and omitting it
	// on update keeps the stored value rather than silently disabling a provider.
	Enabled *bool `json:"enabled"`
}

type setModelRequest struct {
	ProviderID string `json:"provider_id"`
	Adapter    string `json:"adapter"`
	BaseURL    string `json:"base_url"`
	Model      string `json:"model"`
	APIKey     string `json:"api_key"`
	MaxTokens  int    `json:"max_tokens"`
}

type modelPayload struct {
	ProviderID string `json:"provider_id,omitempty"`
	Adapter    string `json:"adapter,omitempty"`
	BaseURL    string `json:"base_url,omitempty"`
	Model      string `json:"model,omitempty"`
	MaxTokens  int    `json:"max_tokens,omitempty"`
	HasAPIKey  bool   `json:"has_api_key"`
}

type usagePayload struct {
	Day              string `json:"day"`
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	Purpose          string `json:"purpose"`
	Calls            int64  `json:"calls"`
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	CostMicros       int64  `json:"cost_micros"`
}

// List returns the provider configuration in fallback order.
func (h *Handler) List(c *fiber.Ctx) error {
	providers, err := h.service.Providers(c.UserContext(), scope(c))
	if err != nil {
		return h.fail(c, "list providers", err)
	}

	return response.OK(c, "ok", providers)
}

// Adapters returns the adapter names the backend can serve, which is what the
// settings screen offers in its picker.
func (h *Handler) Adapters(c *fiber.Ctx) error {
	return response.OK(c, "ok", fiber.Map{"adapters": domain.KnownAdapters})
}

// Usage returns the aggregated spend of the workspace, most recent day first.
func (h *Handler) Usage(c *fiber.Ctx) error {
	days := c.QueryInt("days", 0)

	usage, err := h.service.Usage(c.UserContext(), scope(c), days)
	if err != nil {
		return h.fail(c, "read usage", err)
	}

	payload := make([]usagePayload, 0, len(usage))
	for _, day := range usage {
		payload = append(payload, usagePayload{
			Day:              day.Day.Format(time.DateOnly),
			Provider:         day.Provider,
			Model:            day.Model,
			Purpose:          day.Purpose,
			Calls:            day.Calls,
			InputTokens:      day.InputTokens,
			OutputTokens:     day.OutputTokens,
			CacheReadTokens:  day.CacheReadTokens,
			CacheWriteTokens: day.CacheWriteTokens,
			TotalTokens:      day.Total(),
			CostMicros:       day.CostMicros,
		})
	}

	return response.OK(c, "ok", payload)
}

// Create adds a provider.
func (h *Handler) Create(c *fiber.Ctx) error {
	var req upsertProviderRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	provider, err := h.service.Upsert(c.UserContext(), scope(c), domain.UpsertRequest{
		ProviderConfig: domain.ProviderConfig{
			Name:          req.Name,
			Adapter:       req.Adapter,
			BaseURL:       req.BaseURL,
			Model:         req.Model,
			APIKey:        req.APIKey,
			Priority:      req.Priority,
			MaxTokens:     req.MaxTokens,
			ContextTokens: req.ContextTokens,
			IsDefault:     req.IsDefault,
			Enabled:       enabled,
		},
	})
	if err != nil {
		return h.fail(c, "create provider", err)
	}

	return response.Success(c, fiber.StatusCreated, "provider created", provider)
}

// Update replaces a provider. An empty api_key keeps the stored secret.
func (h *Handler) Update(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("providerID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid provider id", "invalid_request", nil)
	}

	var req upsertProviderRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
	}

	enabled, err := h.resolveEnabled(c, req.Enabled)
	if err != nil {
		return h.fail(c, "resolve provider state", err)
	}

	provider, err := h.service.Upsert(c.UserContext(), scope(c), domain.UpsertRequest{
		ProviderConfig: domain.ProviderConfig{
			ID:            id,
			Name:          req.Name,
			Adapter:       req.Adapter,
			BaseURL:       req.BaseURL,
			Model:         req.Model,
			APIKey:        req.APIKey,
			Priority:      req.Priority,
			MaxTokens:     req.MaxTokens,
			ContextTokens: req.ContextTokens,
			IsDefault:     req.IsDefault,
			Enabled:       enabled,
		},
		ClearKey: req.ClearAPIKey,
	})
	if err != nil {
		return h.fail(c, "update provider", err)
	}

	return response.OK(c, "provider updated", provider)
}

// Delete removes a provider.
func (h *Handler) Delete(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("providerID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid provider id", "invalid_request", nil)
	}

	if err := h.service.Delete(c.UserContext(), scope(c), id); err != nil {
		return h.fail(c, "delete provider", err)
	}

	return response.OK(c, "provider deleted", nil)
}

// Test calls the provider once, which is how the screen proves a key works.
func (h *Handler) Test(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("providerID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid provider id", "invalid_request", nil)
	}

	capabilities, err := h.service.Test(c.UserContext(), scope(c), id)
	if err != nil {
		return h.fail(c, "test provider", err)
	}

	return response.OK(c, "provider answered", fiber.Map{
		"tools":               capabilities.Tools,
		"vision":              capabilities.Vision,
		"streaming":           capabilities.Streaming,
		"prompt_caching":      capabilities.PromptCaching,
		"parallel_tool_calls": capabilities.ParallelToolCalls,
		"max_context_tokens":  capabilities.MaxContextTokens,
	})
}

// AgentModel returns the model override of one Bolu.
func (h *Handler) AgentModel(c *fiber.Ctx) error {
	agentScope, err := h.agentScope(c)
	if err != nil {
		return h.fail(c, "resolve agent scope", err)
	}

	override, err := h.service.AgentModel(c.UserContext(), agentScope)
	if err != nil {
		return h.fail(c, "read agent model", err)
	}

	return response.OK(c, "ok", modelPayload{
		ProviderID: idString(override.ProviderID),
		Adapter:    override.Adapter,
		BaseURL:    override.BaseURL,
		Model:      override.Model,
		MaxTokens:  override.MaxTokens,
		HasAPIKey:  override.APIKey != "",
	})
}

// SetAgentModel stores the model override of one Bolu.
func (h *Handler) SetAgentModel(c *fiber.Ctx) error {
	agentScope, err := h.agentScope(c)
	if err != nil {
		return h.fail(c, "resolve agent scope", err)
	}

	var req setModelRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
	}

	providerID, err := parseOptionalID(req.ProviderID, "provider id")
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error(), "invalid_request", nil)
	}

	override := domain.AgentOverride{
		ProviderID: providerID,
		Adapter:    req.Adapter,
		BaseURL:    req.BaseURL,
		Model:      req.Model,
		APIKey:     req.APIKey,
		MaxTokens:  req.MaxTokens,
	}

	if err := h.service.SetAgentModel(c.UserContext(), agentScope, override); err != nil {
		return h.fail(c, "write agent model", err)
	}

	return response.OK(c, "model updated", modelPayload{
		ProviderID: idString(override.ProviderID),
		Adapter:    override.Adapter,
		BaseURL:    override.BaseURL,
		Model:      override.Model,
		MaxTokens:  override.MaxTokens,
		HasAPIKey:  override.APIKey != "",
	})
}

// requestError is a failure the handler itself detected. It carries the status
// and the code the client sees, so a helper can refuse a request without
// returning an already-written response (which a Fiber error handler would then
// overwrite).
type requestError struct {
	status  int
	code    string
	message string
}

func (e requestError) Error() string { return e.message }

// resolveEnabled keeps the stored value when the request omits the field, so a
// partial update cannot disable a provider by accident.
func (h *Handler) resolveEnabled(c *fiber.Ctx, requested *bool) (bool, error) {
	if requested != nil {
		return *requested, nil
	}

	id, err := uuid.Parse(c.Params("providerID"))
	if err != nil {
		return false, requestError{status: fiber.StatusBadRequest, code: "invalid_request", message: "invalid provider id"}
	}

	providers, err := h.service.Providers(c.UserContext(), scope(c))
	if err != nil {
		return false, err
	}
	for _, provider := range providers {
		if provider.ID == id {
			return provider.Enabled, nil
		}
	}

	return false, domain.ErrProviderNotFound
}

func (h *Handler) agentScope(c *fiber.Ctx) (domain.Scope, error) {
	agentID, err := uuid.Parse(c.Params("agentID"))
	if err != nil {
		return domain.Scope{}, requestError{status: fiber.StatusBadRequest, code: "invalid_request", message: "invalid agent id"}
	}

	return domain.Scope{
		UserID:      middleware.CurrentUserID(c),
		WorkspaceID: middleware.WorkspaceUUID(c),
		AgentID:     agentID,
	}, nil
}

func (h *Handler) fail(c *fiber.Ctx, operation string, err error) error {
	var requestErr requestError
	if errors.As(err, &requestErr) {
		return response.Error(c, requestErr.status, requestErr.message, requestErr.code, nil)
	}

	switch {
	case errors.Is(err, domain.ErrProviderNotFound):
		return response.Error(c, fiber.StatusNotFound, "provider not found", "provider_not_found", nil)
	case errors.Is(err, domain.ErrDuplicateProvider):
		return response.Error(c, fiber.StatusConflict, "a provider with that name already exists", "provider_exists", nil)
	case errors.Is(err, domain.ErrNoProvider):
		return response.Error(c, fiber.StatusConflict, err.Error(), "no_provider", nil)
	case errors.Is(err, domain.ErrQuotaExceeded):
		return response.Error(c, fiber.StatusTooManyRequests, err.Error(), "quota_exceeded", nil)
	case errors.Is(err, domain.ErrUnsupported):
		return response.Error(c, fiber.StatusBadRequest, err.Error(), "unsupported", nil)
	case errors.Is(err, domain.ErrContextTooLong):
		return response.Error(c, fiber.StatusBadRequest, err.Error(), "context_too_long", nil)
	case errors.Is(err, domain.ErrInvalidRequest):
		return response.Error(c, fiber.StatusBadRequest, err.Error(), "invalid_request", nil)
	case errors.Is(err, domain.ErrRateLimited):
		return response.Error(c, fiber.StatusTooManyRequests, err.Error(), "rate_limited", nil)
	case errors.Is(err, domain.ErrProviderUnavailable):
		return response.Error(c, fiber.StatusBadGateway, err.Error(), "provider_unavailable", nil)
	case errors.Is(err, domain.ErrUsageNotRecorded):
		return response.Error(c, fiber.StatusInternalServerError, err.Error(), "usage_not_recorded", nil)
	default:
		h.log.Error("llm request failed", zap.String("operation", operation), zap.Error(err))
		return response.Error(c, fiber.StatusInternalServerError, "internal server error", "internal_error", nil)
	}
}

// scope builds the module scope from the identity the tenant middleware stored.
func scope(c *fiber.Ctx) domain.Scope {
	return domain.Scope{
		UserID:      middleware.CurrentUserID(c),
		WorkspaceID: middleware.WorkspaceUUID(c),
	}
}

func parseOptionalID(value, field string) (uuid.UUID, error) {
	if value == "" {
		return uuid.Nil, nil
	}
	id, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil, errors.New("invalid " + field)
	}
	return id, nil
}

func idString(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}
