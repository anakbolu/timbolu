// Package handler exposes the plans Bolus keep over HTTP.
//
// It is read-only, and that is deliberate rather than unfinished. A plan belongs to the Bolu that is
// working from it: the tools are how one is written, ticked, and thrown away, and a client that could
// tick a line could make a Bolu believe work was done that nobody did. A screen shows the list; the
// Bolu is the only thing that moves it. That is the same line the order module draws, for the same
// reason.
package handler

import (
	"errors"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/checklist/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/middleware"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/response"
)

// Handler serves the plans.
type Handler struct {
	service domain.Service
	log     *zap.Logger
}

// New builds the handler.
func New(service domain.Service, log *zap.Logger) *Handler {
	return &Handler{service: service, log: log}
}

// Routes registers the endpoints every member of the workspace may read.
//
// A plan is the team's business rather than a private one: the person who notices a Bolu going round
// in circles is not always the owner, and "what is it working on" is the first question they ask.
func Routes(group fiber.Router, h *Handler) {
	group.Get("/checklists", h.List)
}

// List returns the workspace's plans, newest first.
func (h *Handler) List(c *fiber.Ctx) error {
	filter := domain.Filter{
		Status: c.Query("status"),
		Limit:  c.QueryInt("limit", 0),
	}

	// An agent id that is not a uuid is a client bug rather than an empty result, so it is refused
	// rather than passed down as a nil filter — which would quietly answer with every Bolu's plan.
	if raw := c.Query("agent_id"); raw != "" {
		agentID, err := uuid.Parse(raw)
		if err != nil {
			return response.Error(c, fiber.StatusBadRequest, "agent_id bukan uuid", "invalid_input", nil)
		}

		filter.AgentID = agentID
	}

	plans, err := h.service.List(c.UserContext(), scope(c), filter)
	if err != nil {
		return h.fail(c, "list checklists", err)
	}

	payload := make([]checklistPayload, 0, len(plans))
	for _, plan := range plans {
		payload = append(payload, toPayload(plan))
	}

	return response.OKWithMeta(c, "ok", payload, fiber.Map{"count": len(payload)})
}

// fail maps the module's errors onto responses.
func (h *Handler) fail(c *fiber.Ctx, operation string, err error) error {
	if errors.Is(err, domain.ErrInvalidInput) {
		return response.Error(c, fiber.StatusBadRequest, err.Error(), "invalid_input", nil)
	}

	h.log.Error("checklist request failed", zap.String("operation", operation), zap.Error(err))

	return response.Error(c, fiber.StatusInternalServerError, "permintaan gagal", "internal_error", nil)
}

// scope is the tenant the request runs as.
func scope(c *fiber.Ctx) domain.Scope {
	return domain.Scope{
		UserID:      middleware.CurrentUserID(c),
		WorkspaceID: middleware.WorkspaceUUID(c),
	}
}

// ---------------------------------------------------------------- the wire

// checklistPayload is one plan as a screen reads it.
type checklistPayload struct {
	ChecklistID string        `json:"checklist_id"`
	AgentID     string        `json:"agent_id"`
	TaskID      string        `json:"task_id,omitempty"`
	Title       string        `json:"title"`
	Status      string        `json:"status"`
	DoneCount   int           `json:"done_count"`
	TotalCount  int           `json:"total_count"`
	Items       []itemPayload `json:"items"`
	CreatedAt   string        `json:"created_at"`
	CompletedAt string        `json:"completed_at,omitempty"`
}

// itemPayload is one task, with its subtasks.
type itemPayload struct {
	ItemID   string        `json:"item_id"`
	Title    string        `json:"title"`
	Done     bool          `json:"done"`
	Subtasks []itemPayload `json:"subtasks,omitempty"`
}

// toPayload renders a plan.
func toPayload(plan domain.Checklist) checklistPayload {
	done, total := plan.Counts()

	payload := checklistPayload{
		ChecklistID: plan.ID.String(),
		AgentID:     plan.AgentID.String(),
		Title:       plan.Title,
		Status:      plan.Status,
		DoneCount:   done,
		TotalCount:  total,
		Items:       make([]itemPayload, 0, len(plan.Items)),
		CreatedAt:   plan.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if plan.TaskID != uuid.Nil {
		payload.TaskID = plan.TaskID.String()
	}
	if plan.CompletedAt != nil {
		payload.CompletedAt = plan.CompletedAt.Format("2006-01-02T15:04:05Z07:00")
	}

	for _, item := range plan.Items {
		rendered := itemPayload{ItemID: item.ID.String(), Title: item.Title, Done: item.Done}
		for _, subtask := range item.Subtasks {
			rendered.Subtasks = append(rendered.Subtasks, itemPayload{
				ItemID: subtask.ID.String(),
				Title:  subtask.Title,
				Done:   subtask.Done,
			})
		}

		payload.Items = append(payload.Items, rendered)
	}

	return payload
}
