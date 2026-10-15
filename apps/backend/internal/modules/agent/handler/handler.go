// Package handler exposes the agent registry over HTTP. It talks to the domain
// service interface only.
package handler

import (
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
)

// Handler serves the registry endpoints.
type Handler struct {
	service domain.Service
	log     *zap.Logger
}

// New builds the handler.
func New(service domain.Service, log *zap.Logger) *Handler {
	return &Handler{service: service, log: log}
}

type createAgentRequest struct {
	TeamID      string   `json:"team_id"`
	Name        string   `json:"name"`
	Role        string   `json:"role"`
	Persona     string   `json:"persona"`
	Tone        string   `json:"tone"`
	Shape       string   `json:"shape"`
	Color       string   `json:"color"`
	Tools       []string `json:"tools"`
	TemplateKey string   `json:"template_key"`
	CopyFrom    string   `json:"copy_from"`
}

type updateAgentRequest struct {
	Name         string         `json:"name"`
	Role         string         `json:"role"`
	Persona      string         `json:"persona"`
	Tone         string         `json:"tone"`
	Shape        string         `json:"shape"`
	Color        string         `json:"color"`
	Tools        []string       `json:"tools"`
	DefaultModel map[string]any `json:"default_model"`
}

type setStatusRequest struct {
	Status string `json:"status"`
}

// arrangeRequest is the sidebar's layout as the client read it.
//
// The whole list is sent rather than one move, so the write is idempotent and two
// clients cannot interleave into a sidebar that matches neither of them. `teams`
// carries the position too: a drag that crosses a team boundary is the same
// gesture as one that does not, and a body that could only reorder inside a team
// would make the two into different requests.
type arrangeRequest struct {
	Teams []arrangeTeamRequest `json:"teams"`
}

type arrangeTeamRequest struct {
	TeamID   string   `json:"team_id"`
	AgentIDs []string `json:"agent_ids"`
}

type setMemoryExtractionRequest struct {
	// Enabled is a pointer so an absent field is a bad request rather than a
	// silent "turn it off": the switch decides whether a Bolu learns from
	// conversations, and a body that forgot it must not change it.
	Enabled *bool `json:"enabled"`
}

type setGrantRequest struct {
	IntegrationID string `json:"integration_id"`
	Permission    string `json:"permission"`
}

type reasonPayload struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`
	Status    string `json:"status,omitempty"`
	CreatedAt string `json:"created_at"`
}

type agentPayload struct {
	ID       string `json:"id"`
	TeamID   string `json:"team_id"`
	TeamName string `json:"team_name"`
	TeamKind string `json:"team_kind"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Persona  string `json:"persona"`
	Tone     string `json:"tone"`
	Shape    string `json:"shape"`
	Color    string `json:"color"`
	Status   string `json:"status"`
	Display  string `json:"display_status"`
	// Position is where the Bolu sits in its team's list, which is what a drag
	// changes and what the sidebar orders by.
	Position int32 `json:"position"`
	// MemoryExtraction is the switch the memory screen reads and writes.
	MemoryExtraction bool           `json:"memory_extraction_enabled"`
	Reason           *reasonPayload `json:"reason,omitempty"`
	TemplateKey      string         `json:"template_key,omitempty"`
	Tools            []string       `json:"tools"`
	Model            map[string]any `json:"default_model"`
	CreatedAt        string         `json:"created_at"`
}

type teamPayload struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Kind   string         `json:"kind"`
	Agents []agentPayload `json:"agents"`
}

type toolPayload struct {
	Name        string `json:"name"`
	Integration string `json:"integration_app"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

type grantPayload struct {
	ID            string `json:"id"`
	AgentID       string `json:"agent_id"`
	IntegrationID string `json:"integration_id"`
	App           string `json:"app"`
	AccountLabel  string `json:"account_label"`
	Status        string `json:"status"`
	Permission    string `json:"permission"`
}

type integrationPayload struct {
	ID           string `json:"id"`
	App          string `json:"app"`
	AccountLabel string `json:"account_label"`
	Status       string `json:"status"`
}

type templatePayload struct {
	Key          string   `json:"key"`
	Name         string   `json:"name"`
	Role         string   `json:"role"`
	Persona      string   `json:"persona"`
	Tone         string   `json:"tone"`
	Shape        string   `json:"shape"`
	Color        string   `json:"color"`
	Tools        []string `json:"tools"`
	Integrations []string `json:"integrations"`
}

// List returns every Bolu of the active workspace with its derived status.
func (h *Handler) List(c *fiber.Ctx) error {
	agents, err := h.service.List(c.UserContext(), scope(c))
	if err != nil {
		return h.fail(c, "list agents", err)
	}

	payload := make([]agentPayload, 0, len(agents))
	for _, agent := range agents {
		payload = append(payload, toAgentPayload(agent))
	}

	return response.OK(c, "ok", payload)
}

// Teams returns the teams with their Bolu, which the team screen renders.
func (h *Handler) Teams(c *fiber.Ctx) error {
	teams, err := h.service.Teams(c.UserContext(), scope(c))
	if err != nil {
		return h.fail(c, "list teams", err)
	}

	return response.OK(c, "ok", toTeamPayloads(teams))
}

// toTeamPayloads renders the teams and their Bolu. It is shared by the read and by
// the arrangement's answer, so the sidebar a drag re-renders is the same shape as
// the one it was drawn from.
func toTeamPayloads(teams []domain.Team) []teamPayload {
	payload := make([]teamPayload, 0, len(teams))
	for _, team := range teams {
		agents := make([]agentPayload, 0, len(team.Agents))
		for _, agent := range team.Agents {
			agents = append(agents, toAgentPayload(agent))
		}
		payload = append(payload, teamPayload{ID: team.ID.String(), Name: team.Name, Kind: team.Kind, Agents: agents})
	}
	return payload
}

// Arrange applies the sidebar's layout and answers with the teams as they now read.
//
// The answer is the stored arrangement rather than an acknowledgement, because a
// drag is a guess about a list: the client sends the layout it wants, and what it
// renders next should be what the workspace kept.
func (h *Handler) Arrange(c *fiber.Ctx) error {
	var req arrangeRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
	}

	arrangement := domain.Arrangement{Teams: make([]domain.TeamArrangement, 0, len(req.Teams))}
	for _, team := range req.Teams {
		teamID, err := uuid.Parse(team.TeamID)
		if err != nil {
			return response.Error(c, fiber.StatusBadRequest, "invalid team id", "invalid_request", nil)
		}

		agentIDs := make([]uuid.UUID, 0, len(team.AgentIDs))
		for _, raw := range team.AgentIDs {
			id, err := uuid.Parse(raw)
			if err != nil {
				return response.Error(c, fiber.StatusBadRequest, "invalid agent id", "invalid_request", nil)
			}
			agentIDs = append(agentIDs, id)
		}

		arrangement.Teams = append(arrangement.Teams, domain.TeamArrangement{TeamID: teamID, Agents: agentIDs})
	}

	teams, err := h.service.Arrange(c.UserContext(), scope(c), arrangement)
	if err != nil {
		return h.fail(c, "arrange agents", err)
	}

	return response.OK(c, "urutan diperbarui", toTeamPayloads(teams))
}

// Get returns one Bolu.
func (h *Handler) Get(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("agentID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid agent id", "invalid_request", nil)
	}

	agent, err := h.service.Get(c.UserContext(), scope(c), id)
	if err != nil {
		return h.fail(c, "get agent", err)
	}

	return response.OK(c, "ok", toAgentPayload(agent))
}

// Create adds a Bolu, from scratch, from a template, or by copying one.
func (h *Handler) Create(c *fiber.Ctx) error {
	var req createAgentRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
	}

	teamID, err := parseOptionalID(req.TeamID, "team id")
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error(), "invalid_request", nil)
	}
	copyFrom, err := parseOptionalID(req.CopyFrom, "copy_from")
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error(), "invalid_request", nil)
	}

	agent, err := h.service.Create(c.UserContext(), scope(c), domain.CreateRequest{
		TeamID:      teamID,
		Name:        req.Name,
		Role:        req.Role,
		Persona:     req.Persona,
		Tone:        req.Tone,
		Shape:       req.Shape,
		Color:       req.Color,
		Tools:       req.Tools,
		TemplateKey: req.TemplateKey,
		CopyFrom:    copyFrom,
	})
	if err != nil {
		return h.fail(c, "create agent", err)
	}

	return response.Created(c, "bolu created", toAgentPayload(agent))
}

// Update edits a Bolu profile.
func (h *Handler) Update(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("agentID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid agent id", "invalid_request", nil)
	}

	var req updateAgentRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
	}

	agent, err := h.service.Update(c.UserContext(), scope(c), id, domain.UpdateRequest{
		Name:         req.Name,
		Role:         req.Role,
		Persona:      req.Persona,
		Tone:         req.Tone,
		Shape:        req.Shape,
		Color:        req.Color,
		Tools:        req.Tools,
		DefaultModel: req.DefaultModel,
	})
	if err != nil {
		return h.fail(c, "update agent", err)
	}

	return response.OK(c, "bolu updated", toAgentPayload(agent))
}

// SetStatus flips the rest switch.
func (h *Handler) SetStatus(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("agentID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid agent id", "invalid_request", nil)
	}

	var req setStatusRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
	}

	agent, err := h.service.SetStatus(c.UserContext(), scope(c), id, req.Status)
	if err != nil {
		return h.fail(c, "set agent status", err)
	}

	return response.OK(c, "bolu status updated", toAgentPayload(agent))
}

// SetMemoryExtraction flips automatic memory extraction for one Bolu.
func (h *Handler) SetMemoryExtraction(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("agentID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid agent id", "invalid_request", nil)
	}

	var req setMemoryExtractionRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
	}
	if req.Enabled == nil {
		return response.Error(c, fiber.StatusBadRequest, "enabled is required", "invalid_request", nil)
	}

	agent, err := h.service.SetMemoryExtraction(c.UserContext(), scope(c), id, *req.Enabled)
	if err != nil {
		return h.fail(c, "set memory extraction", err)
	}

	return response.OK(c, "memory extraction updated", toAgentPayload(agent))
}

// Delete removes a Bolu from the registry.
func (h *Handler) Delete(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("agentID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid agent id", "invalid_request", nil)
	}

	if err := h.service.Delete(c.UserContext(), scope(c), id); err != nil {
		return h.fail(c, "delete agent", err)
	}

	return response.OK(c, "bolu removed", nil)
}

// Templates lists the seeded Bolu profiles.
func (h *Handler) Templates(c *fiber.Ctx) error {
	templates, err := h.service.Templates(c.UserContext())
	if err != nil {
		return h.fail(c, "list templates", err)
	}

	payload := make([]templatePayload, 0, len(templates))
	for _, template := range templates {
		payload = append(payload, templatePayload{
			Key:          template.Key,
			Name:         template.Name,
			Role:         template.Role,
			Persona:      template.Persona,
			Tone:         template.Tone,
			Shape:        template.Shape,
			Color:        template.Color,
			Tools:        template.Tools,
			Integrations: template.Integrations,
		})
	}

	return response.OK(c, "ok", payload)
}

// Grants lists the grants of one Bolu.
func (h *Handler) Grants(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("agentID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid agent id", "invalid_request", nil)
	}

	grants, err := h.service.Grants(c.UserContext(), scope(c), id)
	if err != nil {
		return h.fail(c, "list grants", err)
	}

	payload := make([]grantPayload, 0, len(grants))
	for _, grant := range grants {
		payload = append(payload, toGrantPayload(grant))
	}

	return response.OK(c, "ok", payload)
}

// SetGrant gives a Bolu access to an integration.
func (h *Handler) SetGrant(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("agentID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid agent id", "invalid_request", nil)
	}

	var req setGrantRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
	}

	integrationID, err := uuid.Parse(req.IntegrationID)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid integration id", "invalid_request", nil)
	}

	grant, err := h.service.SetGrant(c.UserContext(), scope(c), id, integrationID, req.Permission)
	if err != nil {
		return h.fail(c, "set grant", err)
	}

	return response.OK(c, "grant saved", toGrantPayload(grant))
}

// DeleteGrant revokes a grant.
func (h *Handler) DeleteGrant(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("agentID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid agent id", "invalid_request", nil)
	}
	integrationID, err := uuid.Parse(c.Params("integrationID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid integration id", "invalid_request", nil)
	}

	if err := h.service.DeleteGrant(c.UserContext(), scope(c), id, integrationID); err != nil {
		return h.fail(c, "delete grant", err)
	}

	return response.OK(c, "grant revoked", nil)
}

// AllowedTools is the tool list a task may offer this Bolu.
func (h *Handler) AllowedTools(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("agentID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid agent id", "invalid_request", nil)
	}

	tools, err := h.service.AllowedTools(c.UserContext(), scope(c), id)
	if err != nil {
		return h.fail(c, "allowed tools", err)
	}

	payload := make([]toolPayload, 0, len(tools))
	for _, tool := range tools {
		payload = append(payload, toToolPayload(tool))
	}

	return response.OK(c, "ok", payload)
}

// ToolCatalog lists every known tool with its label.
func (h *Handler) ToolCatalog(c *fiber.Ctx) error {
	tools, err := h.service.ToolCatalog(c.UserContext())
	if err != nil {
		return h.fail(c, "tool catalog", err)
	}

	payload := make([]toolPayload, 0, len(tools))
	for _, tool := range tools {
		payload = append(payload, toToolPayload(tool))
	}

	return response.OK(c, "ok", payload)
}

// Integrations lists the workspace's connected applications.
func (h *Handler) Integrations(c *fiber.Ctx) error {
	integrations, err := h.service.Integrations(c.UserContext(), scope(c))
	if err != nil {
		return h.fail(c, "list integrations", err)
	}

	payload := make([]integrationPayload, 0, len(integrations))
	for _, integration := range integrations {
		payload = append(payload, integrationPayload{
			ID:           integration.ID.String(),
			App:          integration.App,
			AccountLabel: integration.AccountLabel,
			Status:       integration.Status,
		})
	}

	return response.OK(c, "ok", payload)
}

// Routes registers the registry endpoints that need an active workspace. The
// container applies the tenant middleware to this group.
// CatalogRoutes registers the reads that describe the deployment rather than a
// workspace.
//
// The seeded profiles and the tool catalogue are deployment-wide facts: a client
// asking what it could start from has not necessarily picked a workspace yet, and
// answering "workspace id is required" to that question is what made the profile
// picker empty on the screen that offers it.
func CatalogRoutes(group fiber.Router, h *Handler) {
	group.Get("/agents/templates", h.Templates)
	group.Get("/agents/tool-catalog", h.ToolCatalog)
}

// Routes registers the endpoints every member of the workspace may use.
func Routes(group fiber.Router, h *Handler) {
	group.Get("/agents", h.List)
	group.Get("/agents/integrations", h.Integrations)
	group.Get("/teams", h.Teams)
	group.Get("/agents/:agentID", h.Get)
	group.Get("/agents/:agentID/grants", h.Grants)
	group.Get("/agents/:agentID/tools", h.AllowedTools)
}

// ManagerRoutes registers the endpoints that change the registry. The container
// applies the tenant middleware and a managing-role check.
func ManagerRoutes(group fiber.Router, h *Handler) {
	group.Post("/agents", h.Create)
	group.Put("/agents/order", h.Arrange)
	group.Patch("/agents/:agentID", h.Update)
	group.Patch("/agents/:agentID/status", h.SetStatus)
	group.Patch("/agents/:agentID/memory-extraction", h.SetMemoryExtraction)
	group.Delete("/agents/:agentID", h.Delete)
	group.Put("/agents/:agentID/grants", h.SetGrant)
	group.Delete("/agents/:agentID/grants/:integrationID", h.DeleteGrant)
}

// scope builds the module scope from the identity the tenant middleware stored.
func scope(c *fiber.Ctx) domain.Scope {
	return domain.Scope{
		UserID:      middleware.CurrentUserID(c),
		WorkspaceID: middleware.WorkspaceUUID(c),
	}
}

func (h *Handler) fail(c *fiber.Ctx, operation string, err error) error {
	switch {
	case errors.Is(err, domain.ErrAgentNotFound):
		return response.Error(c, fiber.StatusNotFound, "bolu not found", "agent_not_found", nil)
	case errors.Is(err, domain.ErrTeamNotFound):
		return response.Error(c, fiber.StatusNotFound, "team not found", "team_not_found", nil)
	case errors.Is(err, domain.ErrIntegrationNotFound):
		return response.Error(c, fiber.StatusNotFound, "integration not found", "integration_not_found", nil)
	case errors.Is(err, domain.ErrInvalidInput):
		return response.Error(c, fiber.StatusBadRequest, err.Error(), "invalid_input", nil)
	case errors.Is(err, domain.ErrInvalidStatus):
		return response.Error(c, fiber.StatusBadRequest, err.Error(), "invalid_status", nil)
	case errors.Is(err, domain.ErrInvalidPermission):
		return response.Error(c, fiber.StatusBadRequest, err.Error(), "invalid_permission", nil)
	case errors.Is(err, domain.ErrOrderIncomplete):
		return response.Error(c, fiber.StatusBadRequest, err.Error(), "order_incomplete", nil)
	case errors.Is(err, domain.ErrAgentHasRunningTask):
		return response.Error(c, fiber.StatusConflict, err.Error(), "agent_busy", nil)
	case errors.Is(err, domain.ErrAgentLimitReached):
		return response.Error(c, fiber.StatusConflict, err.Error(), "agent_limit_reached", nil)
	default:
		h.log.Error("agent request failed", zap.String("operation", operation), zap.Error(err))
		return response.Error(c, fiber.StatusInternalServerError, "internal server error", "internal_error", nil)
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

func toAgentPayload(agent domain.Agent) agentPayload {
	payload := agentPayload{
		ID:               agent.ID.String(),
		TeamID:           agent.TeamID.String(),
		TeamName:         agent.TeamName,
		TeamKind:         agent.TeamKind,
		Name:             agent.Name,
		Role:             agent.Role,
		Persona:          agent.Persona,
		Tone:             agent.Tone,
		Shape:            agent.Shape,
		Color:            agent.Color,
		Status:           agent.Status,
		Display:          agent.Display,
		Position:         agent.Position,
		MemoryExtraction: agent.MemoryExtraction,
		TemplateKey:      agent.TemplateKey,
		Tools:            agent.Tools,
		Model:            agent.DefaultModel,
		CreatedAt:        agent.CreatedAt.UTC().Format(time.RFC3339),
	}
	if payload.Tools == nil {
		payload.Tools = []string{}
	}
	if payload.Model == nil {
		payload.Model = map[string]any{}
	}

	if agent.Reason != nil {
		payload.Reason = &reasonPayload{
			Kind:      agent.Reason.Kind,
			ID:        agent.Reason.ID.String(),
			Title:     agent.Reason.Title,
			Status:    agent.Reason.Status,
			CreatedAt: agent.Reason.CreatedAt.UTC().Format(time.RFC3339),
		}
	}

	return payload
}

func toGrantPayload(grant domain.Grant) grantPayload {
	return grantPayload{
		ID:            grant.ID.String(),
		AgentID:       grant.AgentID.String(),
		IntegrationID: grant.IntegrationID.String(),
		App:           grant.App,
		AccountLabel:  grant.AccountLabel,
		Status:        grant.Status,
		Permission:    grant.Permission,
	}
}

func toToolPayload(tool domain.Tool) toolPayload {
	return toolPayload{
		Name:        tool.Name,
		Integration: tool.Integration,
		Label:       tool.Label,
		Description: tool.Description,
	}
}
