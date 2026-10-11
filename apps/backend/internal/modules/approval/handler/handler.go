// Package handler exposes the approval runtime over HTTP: the drafts waiting for
// a decision, one draft with its revision trail, the policy screen, and the audit
// trail.
//
// A decision is the one write here, and it is deliberately not a generic status
// update: approving, sending back, and stopping are three different acts with
// three different consequences, so each has its own endpoint.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/approval/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
)

// Handler serves the approval endpoints.
type Handler struct {
	service domain.Service
	log     *zap.Logger
	// notify is how a decision reaches the workflow waiting on it. Optional:
	// without it the decision is stored and the running task reads its own status
	// at its next checkpoint, which is slower but still correct.
	notify Signaler
}

// Signaler hands a decision to the durable runtime.
type Signaler interface {
	// SignalApproval tells the task parked on a draft what a person decided.
	SignalApproval(ctx context.Context, scope domain.Scope, taskID, draftID uuid.UUID, approved bool, note string) error
}

// New builds the handler.
func New(service domain.Service, log *zap.Logger) *Handler {
	return &Handler{service: service, log: log}
}

// WithSignaler installs the runtime signal, so a decision unblocks the task
// immediately rather than at its next checkpoint.
func (h *Handler) WithSignaler(signaler Signaler) *Handler {
	h.notify = signaler
	return h
}

// Routes registers the endpoints every member of the workspace may use.
//
// Deciding is open to every member on purpose: the person who notices a wrong
// invoice is not always the owner, and a draft nobody can approve is a draft
// nobody will.
func Routes(group fiber.Router, h *Handler) {
	group.Get("/drafts", h.List)
	group.Get("/drafts/:draftID", h.Get)
	group.Get("/drafts/:draftID/revisions", h.Revisions)
	group.Post("/drafts/:draftID/approve", h.Approve)
	group.Post("/drafts/:draftID/revise", h.Revise)
	group.Post("/drafts/:draftID/cancel", h.Cancel)

	group.Get("/approval-policies", h.Policies)
	group.Get("/audit", h.Audit)
}

// ManagerRoutes registers the endpoints that change what the workspace allows.
//
// Changing a policy changes what may leave the workspace unattended, so it needs
// a managing role the way the model provider keys do.
func ManagerRoutes(group fiber.Router, h *Handler) {
	group.Put("/approval-policies", h.SetPolicy)
}

type decisionRequest struct {
	Note string `json:"note"`
}

type policyRequest struct {
	ActionKind string `json:"action_kind"`
	Mode       string `json:"mode"`
	// MaxAmount is the ceiling an auto policy may carry out unattended, in
	// rupiah. Absent means no ceiling, which is a decision the workspace is
	// allowed to make.
	MaxAmount *int64 `json:"max_amount"`
}

type draftPayload struct {
	DraftID        string `json:"draft_id"`
	TaskID         string `json:"task_id"`
	AgentID        string `json:"agent_id"`
	ConversationID string `json:"conversation_id,omitempty"`
	ActionKind     string `json:"action_kind"`
	Title          string `json:"title"`
	Summary        string `json:"summary,omitempty"`
	Status         string `json:"status"`
	Revision       int    `json:"revision"`
	// Payload is the action's own shape, which is what the card shows.
	Payload        map[string]any `json:"payload"`
	DecidedBy      string         `json:"decided_by,omitempty"`
	DecidedAt      string         `json:"decided_at,omitempty"`
	DecisionNote   string         `json:"decision_note,omitempty"`
	BlockedReason  string         `json:"blocked_reason,omitempty"`
	NotBefore      string         `json:"not_before,omitempty"`
	SentAt         string         `json:"sent_at,omitempty"`
	CardMessageID  string         `json:"card_message_id,omitempty"`
	IdempotencyKey string         `json:"idempotency_key,omitempty"`
	CreatedAt      string         `json:"created_at"`
}

type revisionPayload struct {
	Revision  int            `json:"revision"`
	Note      string         `json:"note"`
	Payload   map[string]any `json:"payload"`
	DecidedBy string         `json:"decided_by,omitempty"`
	CreatedAt string         `json:"created_at"`
}

type policyPayload struct {
	ActionKind string `json:"action_kind"`
	Mode       string `json:"mode"`
	MaxAmount  *int64 `json:"max_amount"`
	UpdatedBy  string `json:"updated_by,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
}

type auditPayload struct {
	ID           int64          `json:"id"`
	Action       string         `json:"action"`
	ObjectType   string         `json:"object_type"`
	ObjectID     string         `json:"object_id"`
	ActorUserID  string         `json:"actor_user_id,omitempty"`
	ActorAgentID string         `json:"actor_agent_id,omitempty"`
	Metadata     map[string]any `json:"metadata"`
	CreatedAt    string         `json:"created_at"`
}

// List returns the workspace's drafts, newest first.
func (h *Handler) List(c *fiber.Ctx) error {
	filter := domain.Filter{
		Status:     c.Query("status"),
		ActionKind: c.Query("action_kind"),
		Limit:      c.QueryInt("limit", 0),
	}

	agentID, err := parseOptionalID(c.Query("agent_id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "agent_id is not a uuid", "invalid_input", nil)
	}
	filter.AgentID = agentID

	taskID, err := parseOptionalID(c.Query("task_id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "task_id is not a uuid", "invalid_input", nil)
	}
	filter.TaskID = taskID

	drafts, err := h.service.Drafts(c.UserContext(), scope(c), filter)
	if err != nil {
		return h.fail(c, "list drafts", err)
	}

	payload := make([]draftPayload, 0, len(drafts))
	for _, draft := range drafts {
		payload = append(payload, wireDraft(draft))
	}

	return response.OKWithMeta(c, "ok", payload, fiber.Map{"count": len(payload)})
}

// Get returns one draft.
func (h *Handler) Get(c *fiber.Ctx) error {
	id, err := parseID(c.Params("draftID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "draft id is not a uuid", "invalid_input", nil)
	}

	draft, err := h.service.Draft(c.UserContext(), scope(c), id)
	if err != nil {
		return h.fail(c, "get draft", err)
	}

	return response.OK(c, "ok", wireDraft(draft))
}

// Revisions returns a draft's trail, oldest first.
func (h *Handler) Revisions(c *fiber.Ctx) error {
	id, err := parseID(c.Params("draftID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "draft id is not a uuid", "invalid_input", nil)
	}

	revisions, err := h.service.Revisions(c.UserContext(), scope(c), id)
	if err != nil {
		return h.fail(c, "list revisions", err)
	}

	payload := make([]revisionPayload, 0, len(revisions))
	for _, revision := range revisions {
		payload = append(payload, wireRevision(revision))
	}

	return response.OKWithMeta(c, "ok", payload, fiber.Map{"count": len(payload)})
}

// Approve allows a draft and hands the decision to the task waiting on it.
func (h *Handler) Approve(c *fiber.Ctx) error {
	return h.decide(c, "approve")
}

// Revise sends a draft back to the agent, with the note it reads.
func (h *Handler) Revise(c *fiber.Ctx) error {
	return h.decide(c, "revise")
}

// Cancel stops a draft.
func (h *Handler) Cancel(c *fiber.Ctx) error {
	return h.decide(c, "cancel")
}

// decide runs one of the three decisions.
func (h *Handler) decide(c *fiber.Ctx, kind string) error {
	id, err := parseID(c.Params("draftID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "draft id is not a uuid", "invalid_input", nil)
	}

	var body decisionRequest
	if len(c.Body()) > 0 {
		if err := c.BodyParser(&body); err != nil {
			return response.Error(c, fiber.StatusBadRequest, "the body is not valid JSON", "invalid_request", nil)
		}
	}

	ctx := c.UserContext()
	actor := scope(c)

	var draft domain.Draft
	switch kind {
	case "approve":
		draft, err = h.service.Approve(ctx, actor, id, actor.UserID, body.Note)
	case "revise":
		draft, err = h.service.Revise(ctx, actor, id, actor.UserID, body.Note)
	default:
		draft, err = h.service.Cancel(ctx, actor, id, actor.UserID, body.Note)
	}
	if err != nil {
		return h.fail(c, kind+" draft", err)
	}

	// The task is told after the decision is stored. A signal that cannot be
	// delivered is not an error: the workflow reads its own status at its next
	// checkpoint, so the decision still takes effect.
	if h.notify != nil && draft.TaskID != uuid.Nil {
		if err := h.notify.SignalApproval(ctx, actor, draft.TaskID, draft.ID,
			draft.Status == domain.StatusApproved, draft.DecisionNote); err != nil && h.log != nil {
			h.log.Warn("the decision was stored but the running task could not be told; it will read its own status",
				zap.String("draft_id", draft.ID.String()), zap.Error(err))
		}
	}

	return response.OK(c, "draft "+kind+"d", wireDraft(draft))
}

// Policies returns the effective policy for every action kind that reaches
// outside Bolu.
func (h *Handler) Policies(c *fiber.Ctx) error {
	policies, err := h.service.Policies(c.UserContext(), scope(c))
	if err != nil {
		return h.fail(c, "list policies", err)
	}

	payload := make([]policyPayload, 0, len(policies))
	for _, policy := range policies {
		payload = append(payload, wirePolicy(policy))
	}

	return response.OKWithMeta(c, "ok", payload, fiber.Map{"count": len(payload)})
}

// SetPolicy stores a workspace's decision about one action kind.
func (h *Handler) SetPolicy(c *fiber.Ctx) error {
	var body policyRequest
	if err := c.BodyParser(&body); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "the body is not valid JSON", "invalid_request", nil)
	}

	policy, err := h.service.SetPolicy(c.UserContext(), scope(c), domain.PolicyRequest{
		ActionKind: body.ActionKind,
		Mode:       body.Mode,
		MaxAmount:  body.MaxAmount,
	})
	if err != nil {
		return h.fail(c, "set policy", err)
	}

	return response.OK(c, "policy saved", wirePolicy(policy))
}

// Audit returns the workspace's approval trail, newest first.
func (h *Handler) Audit(c *fiber.Ctx) error {
	records, err := h.service.Audit(c.UserContext(), scope(c), c.QueryInt("limit", 0))
	if err != nil {
		return h.fail(c, "list audit", err)
	}

	payload := make([]auditPayload, 0, len(records))
	for _, record := range records {
		payload = append(payload, wireAudit(record))
	}

	return response.OKWithMeta(c, "ok", payload, fiber.Map{"count": len(payload)})
}

func (h *Handler) fail(c *fiber.Ctx, operation string, err error) error {
	switch {
	case errors.Is(err, domain.ErrDraftNotFound):
		return response.Error(c, fiber.StatusNotFound, "draft not found", "draft_not_found", nil)
	case errors.Is(err, domain.ErrAlreadyDecided):
		return response.Error(c, fiber.StatusConflict,
			"the draft has already been decided", "draft_already_decided", nil)
	case errors.Is(err, domain.ErrNotApproved):
		return response.Error(c, fiber.StatusConflict,
			"the draft has not been approved", "draft_not_approved", nil)
	case errors.Is(err, domain.ErrUnknownMode):
		return response.Error(c, fiber.StatusBadRequest, err.Error(), "unknown_mode", nil)
	case errors.Is(err, domain.ErrInvalidInput):
		return response.Error(c, fiber.StatusBadRequest, err.Error(), "invalid_input", nil)
	case errors.Is(err, domain.ErrNoSender):
		return response.Error(c, fiber.StatusServiceUnavailable,
			"nothing can carry this action out yet", "approval_not_configured", nil)
	default:
		h.log.Error("approval request failed", zap.String("operation", operation), zap.Error(err))
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
	if strings.TrimSpace(value) == "" {
		return uuid.Nil, nil
	}
	return uuid.Parse(value)
}

func wireDraft(draft domain.Draft) draftPayload {
	payload := draftPayload{
		DraftID:        draft.ID.String(),
		TaskID:         draft.TaskID.String(),
		AgentID:        draft.AgentID.String(),
		ConversationID: wireID(draft.ConversationID),
		ActionKind:     draft.ActionKind,
		Title:          draft.Title,
		Summary:        draft.Summary,
		Status:         draft.Status,
		Revision:       draft.Revision,
		Payload:        decodePayload(draft.Payload),
		DecidedBy:      wireID(draft.DecidedBy),
		DecisionNote:   draft.DecisionNote,
		BlockedReason:  draft.BlockedReason,
		NotBefore:      wireTime(draft.NotBefore),
		SentAt:         wireTime(draft.SentAt),
		CardMessageID:  wireID(draft.CardMessageID),
		IdempotencyKey: draft.IdempotencyKey,
		CreatedAt:      draft.CreatedAt.Format(timeFormat),
	}

	if draft.DecidedAt != nil {
		payload.DecidedAt = draft.DecidedAt.Format(timeFormat)
	}
	return payload
}

func wireRevision(revision domain.Revision) revisionPayload {
	return revisionPayload{
		Revision:  revision.Revision,
		Note:      revision.Note,
		Payload:   decodePayload(revision.Payload),
		DecidedBy: wireID(revision.DecidedBy),
		CreatedAt: revision.CreatedAt.Format(timeFormat),
	}
}

func wirePolicy(policy domain.Policy) policyPayload {
	payload := policyPayload{
		ActionKind: policy.ActionKind,
		Mode:       policy.Mode,
		MaxAmount:  policy.MaxAmount,
		UpdatedBy:  wireID(policy.UpdatedBy),
	}
	if !policy.UpdatedAt.IsZero() {
		payload.UpdatedAt = policy.UpdatedAt.Format(timeFormat)
	}
	return payload
}

func wireAudit(record domain.AuditRecord) auditPayload {
	metadata := record.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}

	return auditPayload{
		ID:           record.ID,
		Action:       record.Action,
		ObjectType:   record.ObjectType,
		ObjectID:     record.ObjectID,
		ActorUserID:  wireID(record.ActorUserID),
		ActorAgentID: wireID(record.ActorAgentID),
		Metadata:     metadata,
		CreatedAt:    record.CreatedAt.Format(timeFormat),
	}
}

// decodePayload renders the action's own shape for the card.
func decodePayload(raw json.RawMessage) map[string]any {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil || payload == nil {
		return map[string]any{}
	}
	return payload
}

// timeFormat is what every timestamp in this module is rendered as, so a client
// parses one shape.
const timeFormat = time.RFC3339Nano

// wireID renders an id, or the empty string when there is none. Absence is an
// empty string because encoding/json cannot omit a zero uuid.UUID: it is an
// array, so `omitempty` would publish it.
func wireID(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

func wireTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.Format(timeFormat)
}
