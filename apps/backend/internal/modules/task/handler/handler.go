// Package handler exposes the task runtime over HTTP: the history, one task with
// its recorded rounds, the children a task handed work to, and the cancel.
//
// It talks to the domain service interface only. Opening a task from a browser
// is deliberately absent: a task is opened by chat, by a routine, by a webhook,
// or by another task, and each of those has its own entry point. A client that
// could open one directly could claim a trigger that never happened.
package handler

import (
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/service"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
)

// Handler serves the task endpoints.
type Handler struct {
	service domain.Service
	log     *zap.Logger
}

// New builds the handler.
func New(service domain.Service, log *zap.Logger) *Handler {
	return &Handler{service: service, log: log}
}

// Routes registers the endpoints every member of the workspace may read.
func Routes(group fiber.Router, h *Handler) {
	group.Get("/tasks", h.History)
	group.Get("/tasks/:taskID", h.Get)
	group.Get("/tasks/:taskID/steps", h.Steps)
	group.Get("/tasks/:taskID/children", h.Children)
	group.Post("/tasks/:taskID/cancel", h.Cancel)
}

// History returns the tasks of the workspace, newest first.
func (h *Handler) History(c *fiber.Ctx) error {
	filter := domain.Filter{
		Status: c.Query("status"),
		Live:   c.QueryBool("live"),
		Limit:  c.QueryInt("limit", 0),
	}

	agentID, err := parseOptionalID(c.Query("agent_id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "agent_id is not a uuid", "invalid_input", nil)
	}
	filter.AgentID = agentID

	parentID, err := parseOptionalID(c.Query("parent_task_id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "parent_task_id is not a uuid", "invalid_input", nil)
	}
	filter.ParentTaskID = parentID

	tasks, err := h.service.History(c.UserContext(), scope(c), filter)
	if err != nil {
		return h.fail(c, "list tasks", err)
	}

	payload := make([]service.PayloadShape, 0, len(tasks))
	for _, task := range tasks {
		payload = append(payload, service.Payload(task, h.now()))
	}

	return response.OKWithMeta(c, "ok", payload, fiber.Map{"count": len(payload)})
}

// Get returns one task.
func (h *Handler) Get(c *fiber.Ctx) error {
	id, err := parseID(c.Params("taskID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "task id is not a uuid", "invalid_input", nil)
	}

	task, err := h.service.Get(c.UserContext(), scope(c), id)
	if err != nil {
		return h.fail(c, "get task", err)
	}

	return response.OK(c, "ok", service.Payload(task, h.now()))
}

// Steps returns a task's recorded rounds, in order.
func (h *Handler) Steps(c *fiber.Ctx) error {
	id, err := parseID(c.Params("taskID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "task id is not a uuid", "invalid_input", nil)
	}

	steps, err := h.service.Steps(c.UserContext(), scope(c), id)
	if err != nil {
		return h.fail(c, "list steps", err)
	}

	payload := make([]service.StepPayload, 0, len(steps))
	for _, step := range steps {
		payload = append(payload, service.StepWire(step))
	}

	return response.OKWithMeta(c, "ok", payload, fiber.Map{"count": len(payload)})
}

// Children returns the tasks this one handed work to, which is how a handoff
// chain is read from its root.
func (h *Handler) Children(c *fiber.Ctx) error {
	id, err := parseID(c.Params("taskID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "task id is not a uuid", "invalid_input", nil)
	}

	children, err := h.service.Children(c.UserContext(), scope(c), id)
	if err != nil {
		return h.fail(c, "list children", err)
	}

	payload := make([]service.PayloadShape, 0, len(children))
	for _, child := range children {
		payload = append(payload, service.Payload(child, h.now()))
	}

	return response.OKWithMeta(c, "ok", payload, fiber.Map{"count": len(payload)})
}

// Cancel stops a task that has not finished.
func (h *Handler) Cancel(c *fiber.Ctx) error {
	id, err := parseID(c.Params("taskID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "task id is not a uuid", "invalid_input", nil)
	}

	task, err := h.service.Cancel(c.UserContext(), scope(c), id)
	if err != nil {
		return h.fail(c, "cancel task", err)
	}

	return response.OK(c, "task canceled", service.Payload(task, h.now()))
}

// now is the clock the health mark is derived from. The service owns the real
// clock; this reads the wall clock for a display field, which is why it is not
// injectable.
func (h *Handler) now() time.Time { return time.Now() }

func (h *Handler) fail(c *fiber.Ctx, operation string, err error) error {
	switch {
	case errors.Is(err, domain.ErrTaskNotFound):
		return response.Error(c, fiber.StatusNotFound, "task not found", "task_not_found", nil)
	case errors.Is(err, domain.ErrInvalidInput):
		return response.Error(c, fiber.StatusBadRequest, err.Error(), "invalid_input", nil)
	case errors.Is(err, domain.ErrUnknownTrigger):
		return response.Error(c, fiber.StatusBadRequest, err.Error(), "unknown_trigger", nil)
	case errors.Is(err, domain.ErrStepLimitReached),
		errors.Is(err, domain.ErrTokenLimitReached),
		errors.Is(err, domain.ErrCostLimitReached),
		errors.Is(err, domain.ErrDepthLimitReached):
		return response.Error(c, fiber.StatusConflict, err.Error(), "task_limit_reached", nil)
	default:
		h.log.Error("task request failed", zap.String("operation", operation), zap.Error(err))
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

func parseID(value string) (uuid.UUID, error) {
	return uuid.Parse(value)
}

func parseOptionalID(value string) (uuid.UUID, error) {
	if value == "" {
		return uuid.Nil, nil
	}
	return uuid.Parse(value)
}
