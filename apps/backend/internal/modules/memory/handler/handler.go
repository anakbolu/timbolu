// Package handler exposes memory and knowledge over HTTP. It talks to the domain
// service interface only.
//
// The payloads are the module's own types: a memory has no secret in it — the
// sensitive filter runs before anything is stored — and the screen needs every
// field it carries, from the kind to the message a fact came from.
package handler

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/memory/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
)

// Handler serves the memory endpoints.
type Handler struct {
	service domain.Service
	log     *zap.Logger
}

// New builds the handler.
func New(service domain.Service, log *zap.Logger) *Handler {
	return &Handler{service: service, log: log}
}

// Routes registers the endpoints every member of the workspace may use.
//
// Memory is not restricted to owners: what a Bolu remembers is what it does, and
// the person who notices a wrong fact is not always the one who pays for the
// workspace.
func Routes(group fiber.Router, h *Handler) {
	group.Get("/memories", h.List)
	group.Post("/memories", h.Add)
	group.Patch("/memories/:memoryID", h.Edit)
	group.Delete("/memories/:memoryID", h.Forget)

	group.Get("/knowledge", h.Knowledge)
	group.Post("/knowledge", h.Ingest)
	group.Delete("/knowledge/:knowledgeID", h.ForgetKnowledge)
}

type addRequest struct {
	Content string `json:"content"`
	Kind    string `json:"kind"`
	// AgentID names the Bolu a private memory belongs to. Empty or "main" stores
	// it in the workspace-wide layer.
	AgentID string `json:"agent_id"`
	Pinned  bool   `json:"pinned"`
}

type editRequest struct {
	Content *string `json:"content"`
	Kind    *string `json:"kind"`
	Pinned  *bool   `json:"pinned"`
	// AgentID moves the memory to another Bolu. "main" moves it back to the
	// workspace-wide layer; absent leaves the layer alone.
	AgentID    *string `json:"agent_id"`
	MoveToMain *bool   `json:"move_to_main"`
}

type ingestRequest struct {
	Source  string `json:"source"`
	Text    string `json:"text"`
	AgentID string `json:"agent_id"`
}

// List returns the memories of the workspace.
func (h *Handler) List(c *fiber.Ctx) error {
	filter := domain.MemoryFilter{
		Query:             strings.TrimSpace(c.Query("q")),
		IncludeSuperseded: c.QueryBool("history", false),
		Limit:             c.QueryInt("limit", 0),
	}

	layer := strings.TrimSpace(c.Query("layer"))
	agentID, err := parseOptionalID(c.Query("agent_id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "agent_id is not a uuid", "invalid_input", nil)
	}
	if layer == domain.LayerMain || strings.EqualFold(strings.TrimSpace(c.Query("agent_id")), "main") {
		filter.Main = true
		agentID = uuid.Nil
	}
	if !filter.Main && agentID != uuid.Nil {
		filter.AgentID = agentID
	}

	memories, err := h.service.Memories(c.UserContext(), scope(c), filter)
	if err != nil {
		return h.fail(c, "list memories", err)
	}

	return response.OKWithMeta(c, "ok", memories, fiber.Map{"count": len(memories)})
}

// Add stores one memory a person wrote.
func (h *Handler) Add(c *fiber.Ctx) error {
	var body addRequest
	if err := c.BodyParser(&body); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "the body is not valid JSON", "invalid_request", nil)
	}

	agentID, err := parseLayerID(body.AgentID)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "agent_id is not a uuid", "invalid_input", nil)
	}

	memory, err := h.service.Add(c.UserContext(), scope(c), domain.AddRequest{
		Content: body.Content,
		Kind:    body.Kind,
		AgentID: agentID,
		Pinned:  body.Pinned,
	})
	if err != nil {
		return h.fail(c, "add memory", err)
	}

	return response.Created(c, "stored", memory)
}

// Edit changes one memory: its text, its kind, its pin, or its layer.
func (h *Handler) Edit(c *fiber.Ctx) error {
	id, err := parseID(c.Params("memoryID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "memory id is not a uuid", "invalid_input", nil)
	}

	var body editRequest
	if err := c.BodyParser(&body); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "the body is not valid JSON", "invalid_request", nil)
	}

	request := domain.EditRequest{
		ID:      id,
		Content: body.Content,
		Kind:    body.Kind,
		Pinned:  body.Pinned,
	}
	if body.MoveToMain != nil {
		request.MoveToMain = *body.MoveToMain
	}
	if body.AgentID != nil {
		agentID, main, err := parseMove(*body.AgentID)
		if err != nil {
			return response.Error(c, fiber.StatusBadRequest, "agent_id is not a uuid", "invalid_input", nil)
		}
		if main {
			request.MoveToMain = true
		} else {
			request.AgentID = &agentID
		}
	}

	memory, err := h.service.Edit(c.UserContext(), scope(c), request)
	if err != nil {
		return h.fail(c, "edit memory", err)
	}

	return response.OK(c, "updated", memory)
}

// Forget deletes one memory and its vector.
func (h *Handler) Forget(c *fiber.Ctx) error {
	id, err := parseID(c.Params("memoryID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "memory id is not a uuid", "invalid_input", nil)
	}

	if err := h.service.Forget(c.UserContext(), scope(c), id); err != nil {
		return h.fail(c, "delete memory", err)
	}

	return response.OK(c, "deleted", nil)
}

// Knowledge returns the workspace's document chunks, including the ones whose
// embedding failed.
func (h *Handler) Knowledge(c *fiber.Ctx) error {
	filter := domain.KnowledgeFilter{
		Status: strings.TrimSpace(c.Query("status")),
		Query:  strings.TrimSpace(c.Query("q")),
		Source: strings.TrimSpace(c.Query("source")),
		Limit:  c.QueryInt("limit", 0),
	}

	agentID, err := parseOptionalID(c.Query("agent_id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "agent_id is not a uuid", "invalid_input", nil)
	}
	filter.AgentID = agentID

	items, err := h.service.Knowledge(c.UserContext(), scope(c), filter)
	if err != nil {
		return h.fail(c, "list knowledge", err)
	}

	return response.OKWithMeta(c, "ok", items, fiber.Map{"count": len(items)})
}

// Ingest chunks one document, embeds it, and stores the chunks.
func (h *Handler) Ingest(c *fiber.Ctx) error {
	var body ingestRequest
	if err := c.BodyParser(&body); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "the body is not valid JSON", "invalid_request", nil)
	}

	agentID, err := parseLayerID(body.AgentID)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "agent_id is not a uuid", "invalid_input", nil)
	}

	items, err := h.service.Ingest(c.UserContext(), scope(c), domain.IngestRequest{
		AgentID: agentID,
		Source:  body.Source,
		Text:    body.Text,
	})
	if err != nil {
		return h.fail(c, "ingest document", err)
	}

	return response.Created(c, "stored", items)
}

// ForgetKnowledge deletes one chunk and its vector.
func (h *Handler) ForgetKnowledge(c *fiber.Ctx) error {
	id, err := parseID(c.Params("knowledgeID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "knowledge id is not a uuid", "invalid_input", nil)
	}

	if err := h.service.ForgetKnowledge(c.UserContext(), scope(c), id); err != nil {
		return h.fail(c, "delete knowledge", err)
	}

	return response.OK(c, "deleted", nil)
}

// fail maps the module's errors onto responses.
func (h *Handler) fail(c *fiber.Ctx, operation string, err error) error {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return response.Error(c, fiber.StatusNotFound, "not found", "memory_not_found", nil)
	case errors.Is(err, domain.ErrSensitive):
		// The message is the service's own sentence, which names what was found.
		// Stripping the error's prefix keeps "memory: " out of what a person reads.
		return response.Error(c, fiber.StatusUnprocessableEntity, userMessage(err, domain.ErrSensitive), "sensitive_content", nil)
	case errors.Is(err, domain.ErrInvalidInput), errors.Is(err, domain.ErrVectorWidth):
		return response.Error(c, fiber.StatusBadRequest, err.Error(), "invalid_input", nil)
	case errors.Is(err, domain.ErrNoEmbedding), errors.Is(err, domain.ErrNoModel):
		// A deployment without an embedding provider cannot store a memory. That
		// is a 503 rather than a 400: the request was fine, the platform is not
		// ready for it.
		return response.Error(c, fiber.StatusServiceUnavailable, err.Error(), "memory_unavailable", nil)
	default:
		h.log.Error("memory request failed", zap.String("operation", operation), zap.Error(err))
		return response.Error(c, fiber.StatusInternalServerError, "internal server error", "internal_error", nil)
	}
}

// userMessage renders an error for a person: the detail after the sentinel's own
// text, because everything before it is this module's name and the rule's
// internals.
func userMessage(err error, sentinel error) string {
	detail := strings.TrimSpace(strings.TrimPrefix(err.Error(), sentinel.Error()))
	return strings.TrimPrefix(detail, ":")
}

func scope(c *fiber.Ctx) domain.Scope {
	return domain.Scope{
		UserID:      middleware.CurrentUserID(c),
		WorkspaceID: middleware.WorkspaceUUID(c),
	}
}

func parseID(value string) (uuid.UUID, error) {
	return uuid.Parse(value)
}

func parseOptionalID(value string) (uuid.UUID, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || strings.EqualFold(trimmed, "main") {
		return uuid.Nil, nil
	}
	return uuid.Parse(trimmed)
}

// parseLayerID reads the layer an add or an ingest names. An empty value and
// "main" both mean the workspace-wide layer.
func parseLayerID(value string) (uuid.UUID, error) {
	return parseOptionalID(value)
}

// parseMove reads a move request and reports whether it asks for the main layer.
func parseMove(value string) (uuid.UUID, bool, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || strings.EqualFold(trimmed, "main") {
		return uuid.Nil, true, nil
	}

	id, err := uuid.Parse(trimmed)
	if err != nil {
		return uuid.Nil, false, err
	}

	return id, false, nil
}
