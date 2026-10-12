// Package handler exposes routines over HTTP. It talks to the domain service
// interface only.
package handler

import (
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
)

// Handler serves the routine endpoints.
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
// Routines are readable and editable by every member: a routine is the team's work
// rather than an administrative setting, and the person who notices a routine is
// running at the wrong hour is not always the owner.
func Routes(group fiber.Router, h *Handler) {
	group.Get("/routines", h.List)
	group.Post("/routines", h.Create)
	group.Get("/routines/:routineID", h.Get)
	group.Patch("/routines/:routineID", h.Update)
	group.Delete("/routines/:routineID", h.Delete)
	group.Post("/routines/:routineID/pause", h.Pause)
	group.Post("/routines/:routineID/resume", h.Resume)
	group.Get("/routines/:routineID/runs", h.Runs)
	group.Get("/routine-runs", h.AllRuns)
	group.Post("/routines/preview", h.Preview)
}

type writeRequest struct {
	AgentID       string `json:"agent_id"`
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	Minute        int    `json:"minute"`
	HourStart     int    `json:"hour_start"`
	HourEnd       int    `json:"hour_end"`
	DayOfWeek     int    `json:"day_of_week"`
	TriggerSource string `json:"trigger_source"`
	Instruction   string `json:"instruction"`
	Enabled       *bool  `json:"enabled"`
}

type previewRequest struct {
	Kind      string `json:"kind"`
	Minute    int    `json:"minute"`
	HourStart int    `json:"hour_start"`
	HourEnd   int    `json:"hour_end"`
	DayOfWeek int    `json:"day_of_week"`
}

// routinePayload is a routine as a client reads it.
//
// The next run times travel with the routine rather than in a second request: "when
// does this run next" is the first question a routine list answers, and a client that
// had to ask per row would make the list N+1.
type routinePayload struct {
	ID            string   `json:"id"`
	AgentID       string   `json:"agent_id"`
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	Minute        int      `json:"minute"`
	HourStart     int      `json:"hour_start"`
	HourEnd       int      `json:"hour_end"`
	DayOfWeek     int      `json:"day_of_week,omitempty"`
	TriggerSource string   `json:"trigger_source,omitempty"`
	Instruction   string   `json:"instruction"`
	Enabled       bool     `json:"enabled"`
	PausedReason  string   `json:"paused_reason,omitempty"`
	Schedule      string   `json:"schedule"`
	Scheduled     bool     `json:"scheduled"`
	NextRuns      []string `json:"next_runs"`
	CreatedAt     string   `json:"created_at"`
}

type runPayload struct {
	RunID       string `json:"run_id"`
	RoutineID   string `json:"routine_id,omitempty"`
	RoutineName string `json:"routine_name"`
	TaskID      string `json:"task_id,omitempty"`
	Status      string `json:"status"`
	Summary     string `json:"summary"`
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at,omitempty"`
}

// List returns the workspace's routines with their next run times.
func (h *Handler) List(c *fiber.Ctx) error {
	var filter domain.Filter

	agentID, err := parseOptionalID(c.Query("agent_id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "agent_id is not a uuid", "invalid_input", nil)
	}
	filter.AgentID = agentID

	if raw := strings.TrimSpace(c.Query("enabled")); raw != "" {
		enabled := raw == "true"
		filter.Enabled = &enabled
	}

	routines, err := h.service.Routines(c.UserContext(), scope(c), filter)
	if err != nil {
		return h.fail(c, "list routines", err)
	}

	payload := make([]routinePayload, 0, len(routines))
	for _, routine := range routines {
		payload = append(payload, wireRoutine(routine))
	}

	return response.OKWithMeta(c, "ok", payload, fiber.Map{"count": len(payload)})
}

// Get returns one routine.
func (h *Handler) Get(c *fiber.Ctx) error {
	id, err := parseID(c.Params("routineID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "routine id is not a uuid", "invalid_input", nil)
	}

	routine, err := h.service.Routine(c.UserContext(), scope(c), id)
	if err != nil {
		return h.fail(c, "get routine", err)
	}

	return response.OK(c, "ok", wireRoutine(routine))
}

// Create stores a routine and registers its schedule.
func (h *Handler) Create(c *fiber.Ctx) error {
	var body writeRequest
	if err := c.BodyParser(&body); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "the body is not valid JSON", "invalid_request", nil)
	}

	agentID, err := parseID(body.AgentID)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "agent_id is not a uuid", "invalid_input", nil)
	}

	routine, err := h.service.Create(c.UserContext(), scope(c), domain.WriteRequest{
		AgentID:       agentID,
		Name:          body.Name,
		Kind:          body.Kind,
		Minute:        body.Minute,
		HourStart:     body.HourStart,
		HourEnd:       body.HourEnd,
		DayOfWeek:     body.DayOfWeek,
		TriggerSource: body.TriggerSource,
		Instruction:   body.Instruction,
		Enabled:       enabledOr(body.Enabled, true),
	})
	if err != nil {
		return h.fail(c, "create routine", err)
	}

	return response.Created(c, "routine created", wireRoutine(routine))
}

// Update changes a routine and re-registers its schedule.
func (h *Handler) Update(c *fiber.Ctx) error {
	id, err := parseID(c.Params("routineID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "routine id is not a uuid", "invalid_input", nil)
	}

	var body writeRequest
	if err := c.BodyParser(&body); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "the body is not valid JSON", "invalid_request", nil)
	}

	routine, err := h.service.Update(c.UserContext(), scope(c), id, domain.WriteRequest{
		AgentID:       optionalParse(body.AgentID),
		Name:          body.Name,
		Kind:          body.Kind,
		Minute:        body.Minute,
		HourStart:     body.HourStart,
		HourEnd:       body.HourEnd,
		DayOfWeek:     body.DayOfWeek,
		TriggerSource: body.TriggerSource,
		Instruction:   body.Instruction,
		Enabled:       enabledOr(body.Enabled, true),
	})
	if err != nil {
		return h.fail(c, "update routine", err)
	}

	return response.OK(c, "routine updated", wireRoutine(routine))
}

// Delete removes a routine and its schedule, keeping its history.
func (h *Handler) Delete(c *fiber.Ctx) error {
	id, err := parseID(c.Params("routineID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "routine id is not a uuid", "invalid_input", nil)
	}

	if err := h.service.Delete(c.UserContext(), scope(c), id); err != nil {
		return h.fail(c, "delete routine", err)
	}

	return response.OK(c, "routine deleted", nil)
}

// Pause stops a routine firing without losing its history.
func (h *Handler) Pause(c *fiber.Ctx) error {
	return h.setEnabled(c, false)
}

// Resume puts a paused routine back.
func (h *Handler) Resume(c *fiber.Ctx) error {
	return h.setEnabled(c, true)
}

func (h *Handler) setEnabled(c *fiber.Ctx, enabled bool) error {
	id, err := parseID(c.Params("routineID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "routine id is not a uuid", "invalid_input", nil)
	}

	routine, err := h.service.SetEnabled(c.UserContext(), scope(c), id, enabled)
	if err != nil {
		return h.fail(c, "set routine enabled", err)
	}

	message := "rutinitas dijeda"
	if enabled {
		message = "rutinitas dilanjutkan"
	}

	return response.OK(c, message, wireRoutine(routine))
}

// Runs returns one routine's history.
func (h *Handler) Runs(c *fiber.Ctx) error {
	id, err := parseID(c.Params("routineID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "routine id is not a uuid", "invalid_input", nil)
	}

	// Reading the routine first confirms the caller may see it: another workspace's
	// id answers "not found" rather than an empty list.
	if _, err := h.service.Routine(c.UserContext(), scope(c), id); err != nil {
		return h.fail(c, "get routine", err)
	}

	runs, err := h.service.Runs(c.UserContext(), scope(c), domain.RunFilter{
		RoutineID: id,
		Limit:     c.QueryInt("limit", 0),
	})
	if err != nil {
		return h.fail(c, "list runs", err)
	}

	return response.OKWithMeta(c, "ok", h.wireRuns(runs), fiber.Map{"count": len(runs)})
}

// AllRuns returns the workspace's history, so the screen can filter it by Bolu.
func (h *Handler) AllRuns(c *fiber.Ctx) error {
	filter := domain.RunFilter{
		Status: strings.TrimSpace(c.Query("status")),
		Limit:  c.QueryInt("limit", 0),
	}

	agentID, err := parseOptionalID(c.Query("agent_id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "agent_id is not a uuid", "invalid_input", nil)
	}
	filter.AgentID = agentID

	routineID, err := parseOptionalID(c.Query("routine_id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "routine_id is not a uuid", "invalid_input", nil)
	}
	filter.RoutineID = routineID

	runs, err := h.service.Runs(c.UserContext(), scope(c), filter)
	if err != nil {
		return h.fail(c, "list runs", err)
	}

	return response.OKWithMeta(c, "ok", h.wireRuns(runs), fiber.Map{"count": len(runs)})
}

// Preview answers the next run times for a cadence a form is filling in.
func (h *Handler) Preview(c *fiber.Ctx) error {
	var body previewRequest
	if err := c.BodyParser(&body); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "the body is not valid JSON", "invalid_request", nil)
	}

	times, err := h.service.Preview(c.UserContext(), scope(c), domain.PreviewRequest{
		Kind:      body.Kind,
		Minute:    body.Minute,
		HourStart: body.HourStart,
		HourEnd:   body.HourEnd,
		DayOfWeek: body.DayOfWeek,
	})
	if err != nil {
		return h.fail(c, "preview schedule", err)
	}

	payload := make([]string, 0, len(times))
	for _, at := range times {
		payload = append(payload, at.Format(time.RFC3339))
	}

	return response.OK(c, "ok", fiber.Map{"next_runs": payload})
}

// fail maps the module's errors onto responses.
func (h *Handler) fail(c *fiber.Ctx, operation string, err error) error {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return response.Error(c, fiber.StatusNotFound, "routine not found", "routine_not_found", nil)
	case errors.Is(err, domain.ErrScheduleUnavailable):
		// A deployment whose engine is unreachable cannot schedule anything. That is
		// the platform, not the request, so it is a 503.
		return response.Error(c, fiber.StatusServiceUnavailable, err.Error(), "routine_unavailable", nil)
	case errors.Is(err, domain.ErrInvalidInput):
		return response.Error(c, fiber.StatusBadRequest, userMessage(err, domain.ErrInvalidInput), "invalid_input", nil)
	default:
		h.log.Error("routine request failed", zap.String("operation", operation), zap.Error(err))
		return response.Error(c, fiber.StatusInternalServerError, "internal server error", "internal_error", nil)
	}
}

// wireRoutine renders one routine with the times it will next run at. The times
// come from the service, which is where the workspace's zone is known.
func wireRoutine(view domain.View) routinePayload {
	schedule := view.Schedule()

	runs := make([]string, 0, len(view.NextRuns))
	for _, at := range view.NextRuns {
		runs = append(runs, at.Format(time.RFC3339))
	}

	return routinePayload{
		ID:            view.ID.String(),
		AgentID:       view.AgentID.String(),
		Name:          view.Name,
		Kind:          view.Kind,
		Minute:        view.Minute,
		HourStart:     view.HourStart,
		HourEnd:       view.HourEnd,
		DayOfWeek:     view.DayOfWeek,
		TriggerSource: view.TriggerSource,
		Instruction:   view.Instruction,
		Enabled:       view.Enabled,
		PausedReason:  view.PausedReason,
		Schedule:      schedule.Describe(),
		Scheduled:     schedule.Scheduled(),
		NextRuns:      runs,
		CreatedAt:     view.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func (h *Handler) wireRuns(runs []domain.Run) []runPayload {
	payload := make([]runPayload, 0, len(runs))
	for _, run := range runs {
		row := runPayload{
			RunID:       run.ID.String(),
			RoutineName: run.RoutineName,
			Status:      run.Status,
			Summary:     run.Summary,
			StartedAt:   run.StartedAt.UTC().Format(time.RFC3339),
		}
		// The ids travel as empty strings when absent rather than as the zero uuid,
		// because `encoding/json` cannot omit a typed uuid and a client would read
		// `00000000-…` as a real routine.
		if run.RoutineID != uuid.Nil {
			row.RoutineID = run.RoutineID.String()
		}
		if run.TaskID != uuid.Nil {
			row.TaskID = run.TaskID.String()
		}
		if run.FinishedAt != nil {
			row.FinishedAt = run.FinishedAt.UTC().Format(time.RFC3339)
		}
		payload = append(payload, row)
	}
	return payload
}

func scope(c *fiber.Ctx) domain.Scope {
	return domain.Scope{
		UserID:      middleware.CurrentUserID(c),
		WorkspaceID: middleware.WorkspaceUUID(c),
	}
}

func parseID(value string) (uuid.UUID, error) { return uuid.Parse(value) }

func optionalParse(value string) uuid.UUID {
	id, err := uuid.Parse(strings.TrimSpace(value))
	if err != nil {
		return uuid.Nil
	}
	return id
}

func parseOptionalID(value string) (uuid.UUID, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return uuid.Nil, nil
	}
	return uuid.Parse(trimmed)
}

// enabledOr reads the switch, defaulting as the caller asked. An absent field on a
// create means "on", because a routine somebody just wrote is one they want to run.
func enabledOr(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

// userMessage renders an error for a person: the detail after the sentinel's own
// text, because everything before it is this module's name and the rule's internals.
func userMessage(err error, sentinel error) string {
	detail := strings.TrimSpace(strings.TrimPrefix(err.Error(), sentinel.Error()))
	return strings.TrimPrefix(detail, ":")
}
