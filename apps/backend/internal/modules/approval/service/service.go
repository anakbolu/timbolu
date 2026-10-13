// Package service implements the approval use cases: asking for a decision,
// recording one, and carrying out what was allowed.
//
// The rule the package exists for is in Request: an action that reaches outside
// Bolu waits for a person unless the workspace has said otherwise for that
// specific kind of action. An action kind with no stored policy behaves as
// `always_draft`, so the safe answer is also the default one.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/approval/domain"
)

// SummaryMaxRunes bounds the one-line description a card shows.
const SummaryMaxRunes = 160

// Deps are the service dependencies.
type Deps struct {
	// Metrics reports what people did with the drafts, for the deployment's own charts.
	// Optional: without it the decisions happen and nothing is charted.
	Metrics domain.Metrics
	Drafts  domain.Repository
	// Sender carries an approved action out. Optional: without one a draft is
	// approved and held, and the reason says so, rather than being recorded as
	// sent when nothing left.
	Sender domain.Sender
	// Cards writes the draft card into the conversation. Optional: without it a
	// draft is still decidable, it is just not visible in the thread.
	Cards domain.Cards
	// Events publishes what a draft is doing, which is how the dashboard and the
	// card learn about a decision without polling.
	Events domain.EventSink
	// Hours reports the workspace's working hours. Optional: without it every
	// window is open, because a deployment that never configured hours should not
	// hold every send.
	Hours  HoursReader
	Logger *zap.Logger
	Clock  domain.Clock
}

// HoursReader reads the window a workspace lets actions leave in.
type HoursReader interface {
	// WorkingHours returns the window and its timezone for one workspace.
	WorkingHours(ctx context.Context, workspaceID uuid.UUID) (domain.WorkingHours, error)
}

// Service is the approval implementation.
type Service struct {
	deps  Deps
	clock domain.Clock
}

// New builds the service.
func New(deps Deps) *Service {
	clock := deps.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Service{deps: deps, clock: clock}
}

// Request either stores a proposal for a person or carries the action out now.
//
// The policy decides which, and the decision is made here rather than by the
// caller: a task proposes an action, and whether a human is asked is the
// workspace's rule, not the task's.
func (s *Service) Request(ctx context.Context, scope domain.Scope, req domain.Request) (domain.Outcome, error) {
	if err := s.validate(scope, req); err != nil {
		return domain.Outcome{}, err
	}

	policy, err := s.effectivePolicy(ctx, scope, req.ActionKind)
	if err != nil {
		return domain.Outcome{}, err
	}

	if !policy.NeedsDraft(req.Amount()) {
		// The workspace said this action kind may go out unattended, and the
		// value is within the ceiling it set. There is still a record: the draft
		// row is written and marked sent, so "what did the agents send" has an
		// answer even when nobody was asked.
		return s.carryOutNow(ctx, scope, req, policy)
	}

	draft, err := s.deps.Drafts.CreateDraft(ctx, scope, domain.NewDraft{
		TaskID:         req.TaskID,
		AgentID:        req.AgentID,
		ConversationID: req.ConversationID,
		ActionKind:     req.ActionKind,
		Title:          req.Title,
		Summary:        summaryOf(req),
		Payload:        req.Payload,
		IdempotencyKey: idempotencyKey(req),
	})
	if err != nil {
		return domain.Outcome{}, err
	}

	s.audit(ctx, scope, domain.AuditEntry{
		Action:       "draft.created",
		ObjectType:   "draft",
		ObjectID:     draft.ID.String(),
		ActorAgentID: req.AgentID,
		Metadata: map[string]any{
			"action_kind": req.ActionKind,
			"task_id":     req.TaskID.String(),
			"summary":     draft.Summary,
			"mode":        policy.Mode,
		},
	})

	// The card goes into the thread the task came from, so the person decides
	// where they are already reading. A card that could not be written does not
	// fail the proposal: the draft is stored and the dashboard still lists it.
	if s.deps.Cards != nil && draft.ConversationID != uuid.Nil {
		if messageID, err := s.deps.Cards.Write(ctx, scope, draft); err == nil && messageID != uuid.Nil {
			if stored, err := s.deps.Drafts.SetCardMessage(ctx, scope, draft.ID, messageID); err == nil {
				draft = stored
			}
		} else if err != nil && s.deps.Logger != nil {
			s.deps.Logger.Warn("could not write the draft card into the conversation",
				zap.String("draft_id", draft.ID.String()), zap.Error(err))
		}
	}

	s.emit(ctx, scope, draft, domain.EventDraftCreated)

	return domain.Outcome{
		DraftID: draft.ID,
		Status:  draft.Status,
		Title:   draft.Title,
		Summary: draft.Summary,
	}, nil
}

// carryOutNow sends an action the policy allowed, recording it first.
func (s *Service) carryOutNow(ctx context.Context, scope domain.Scope, req domain.Request, policy domain.Policy) (domain.Outcome, error) {
	draft, err := s.deps.Drafts.CreateDraft(ctx, scope, domain.NewDraft{
		TaskID:         req.TaskID,
		AgentID:        req.AgentID,
		ConversationID: req.ConversationID,
		ActionKind:     req.ActionKind,
		Title:          req.Title,
		Summary:        summaryOf(req),
		Payload:        req.Payload,
		IdempotencyKey: idempotencyKey(req),
	})
	if err != nil {
		return domain.Outcome{}, err
	}

	// The row is moved straight to approved so the send path below is the same
	// one a person's approval takes: an auto policy skips the person, not the
	// machinery.
	approved, err := s.deps.Drafts.Decide(ctx, scope, domain.Decision{
		DraftID: draft.ID,
		Status:  domain.StatusApproved,
		Note:    "disetujui otomatis oleh kebijakan workspace",
	})
	if err != nil {
		return domain.Outcome{}, err
	}

	s.audit(ctx, scope, domain.AuditEntry{
		Action:       "draft.approved",
		ObjectType:   "draft",
		ObjectID:     approved.ID.String(),
		ActorAgentID: req.AgentID,
		Metadata: map[string]any{
			"action_kind": req.ActionKind,
			"mode":        policy.Mode,
			"automatic":   true,
		},
	})

	outcome, err := s.CarryOut(ctx, scope, approved.ID)
	if err != nil {
		return domain.Outcome{}, err
	}
	outcome.Summary = approved.Summary
	return outcome, nil
}

// Approve records that a person allowed a draft.
func (s *Service) Approve(ctx context.Context, scope domain.Scope, draftID, userID uuid.UUID, note string) (domain.Draft, error) {
	return s.decide(ctx, scope, domain.Decision{
		DraftID:   draftID,
		Status:    domain.StatusApproved,
		Note:      note,
		DecidedBy: userID,
	}, "draft.approved")
}

// Revise records that a person sent a draft back.
//
// The note is what the agent reads on its next round, so an empty one is refused
// rather than stored: a revision without a reason sends the agent back to work
// with nothing to change.
func (s *Service) Revise(ctx context.Context, scope domain.Scope, draftID, userID uuid.UUID, note string) (domain.Draft, error) {
	if strings.TrimSpace(note) == "" {
		return domain.Draft{}, fmt.Errorf("%w: a revision needs a note", domain.ErrInvalidInput)
	}

	return s.decide(ctx, scope, domain.Decision{
		DraftID:   draftID,
		Status:    domain.StatusRevise,
		Note:      note,
		DecidedBy: userID,
	}, "draft.revised")
}

// Cancel records that a person stopped a draft.
func (s *Service) Cancel(ctx context.Context, scope domain.Scope, draftID, userID uuid.UUID, note string) (domain.Draft, error) {
	return s.decide(ctx, scope, domain.Decision{
		DraftID:   draftID,
		Status:    domain.StatusCanceled,
		Note:      note,
		DecidedBy: userID,
	}, "draft.canceled")
}

// decide records one answer, once.
//
// The store refuses a second decision on the same draft, which is what makes a
// duplicate signal harmless: the first answer stands and the second is reported
// as already decided rather than overwriting it.
func (s *Service) decide(ctx context.Context, scope domain.Scope, decision domain.Decision, action string) (domain.Draft, error) {
	if err := requireScope(scope); err != nil {
		return domain.Draft{}, err
	}
	if decision.DraftID == uuid.Nil {
		return domain.Draft{}, fmt.Errorf("%w: draft id is required", domain.ErrInvalidInput)
	}

	draft, err := s.deps.Drafts.Decide(ctx, scope, decision)
	if err != nil {
		return domain.Draft{}, err
	}

	// A revision keeps what the person was looking at, so the trail answers "why
	// was this sent, after three attempts" without the old payload being lost.
	if draft.Status == domain.StatusRevise {
		if _, err := s.deps.Drafts.AppendRevision(ctx, scope, domain.Revision{
			DraftID:   draft.ID,
			Revision:  draft.Revision,
			Note:      decision.Note,
			Payload:   draft.Payload,
			DecidedBy: decision.DecidedBy,
		}); err != nil && s.deps.Logger != nil {
			s.deps.Logger.Warn("could not append the revision trail",
				zap.String("draft_id", draft.ID.String()), zap.Error(err))
		}
	}

	s.audit(ctx, scope, domain.AuditEntry{
		Action:       action,
		ObjectType:   "draft",
		ObjectID:     draft.ID.String(),
		ActorUserID:  decision.DecidedBy,
		ActorAgentID: draft.AgentID,
		Metadata: map[string]any{
			"action_kind": draft.ActionKind,
			"task_id":     draft.TaskID.String(),
			"note":        decision.Note,
			"revision":    draft.Revision,
		},
	})

	// The card is rewritten so a reload shows what happened rather than a card
	// still offering to approve something already decided.
	if s.deps.Cards != nil && draft.CardMessageID != uuid.Nil {
		if err := s.deps.Cards.Rewrite(ctx, scope, draft.CardMessageID, draft); err != nil && s.deps.Logger != nil {
			s.deps.Logger.Warn("could not rewrite the draft card",
				zap.String("draft_id", draft.ID.String()), zap.Error(err))
		}
	}

	// Recorded before the event, so a chart and a listener see the same decision in the same
	// order. The revision flag is the draft's own count at decision time: a draft decided after
	// a person sent it back is a different fact from one approved as written, and averaging the
	// two would hide the correction rate that says whether the bot is producing usable work.
	if s.deps.Metrics != nil {
		s.deps.Metrics.DraftDecided(ctx, draft.Status, draft.Revision > 0)
	}

	s.emit(ctx, scope, draft, domain.EventDraftDecided)
	return draft, nil
}

// CarryOut executes an approved draft.
//
// It holds the action when the workspace is closed, reporting the moment it may
// leave rather than sending: the person approved what to send, not when, and the
// workspace's hours decide the when.
func (s *Service) CarryOut(ctx context.Context, scope domain.Scope, draftID uuid.UUID) (domain.Outcome, error) {
	if err := requireScope(scope); err != nil {
		return domain.Outcome{}, err
	}

	draft, err := s.deps.Drafts.GetDraft(ctx, scope, draftID)
	if err != nil {
		return domain.Outcome{}, err
	}
	if draft.Status != domain.StatusApproved {
		return domain.Outcome{}, fmt.Errorf("%w: status is %q", domain.ErrNotApproved, draft.Status)
	}

	open, next := s.window(ctx, scope)
	if !open {
		reason := "ditahan sampai jam kerja: " + next.Format("02 Jan 15:04")
		held, err := s.deps.Drafts.Hold(ctx, scope, draft.ID, next, reason)
		if err != nil {
			return domain.Outcome{}, err
		}

		s.emit(ctx, scope, held, domain.EventDraftDecided)
		return domain.Outcome{
			DraftID:   held.ID,
			Status:    held.Status,
			Title:     held.Title,
			Summary:   held.Summary,
			Reason:    held.BlockedReason,
			NotBefore: held.NotBefore,
		}, nil
	}

	if s.deps.Sender == nil {
		// Nothing can carry the action out. Saying so is the honest answer: a
		// draft marked sent when nothing left would be a lie the audit trail
		// would repeat forever.
		return domain.Outcome{}, fmt.Errorf("%w: an approved action cannot be executed", domain.ErrNoSender)
	}

	content, err := s.deps.Sender.Send(ctx, scope, draft)
	if err != nil {
		return domain.Outcome{}, fmt.Errorf("approval: send: %w", err)
	}

	sent, err := s.deps.Drafts.MarkSent(ctx, scope, draft.ID)
	if err != nil {
		return domain.Outcome{}, err
	}

	s.audit(ctx, scope, domain.AuditEntry{
		Action:       "draft.sent",
		ObjectType:   "draft",
		ObjectID:     sent.ID.String(),
		ActorUserID:  sent.DecidedBy,
		ActorAgentID: sent.AgentID,
		Metadata: map[string]any{
			"action_kind":     sent.ActionKind,
			"task_id":         sent.TaskID.String(),
			"idempotency_key": sent.IdempotencyKey,
			"approved_by":     sent.DecidedBy.String(),
		},
	})

	if s.deps.Cards != nil && sent.CardMessageID != uuid.Nil {
		if err := s.deps.Cards.Rewrite(ctx, scope, sent.CardMessageID, sent); err != nil && s.deps.Logger != nil {
			s.deps.Logger.Warn("could not rewrite the draft card after sending",
				zap.String("draft_id", sent.ID.String()), zap.Error(err))
		}
	}

	s.emit(ctx, scope, sent, domain.EventDraftDecided)

	return domain.Outcome{
		DraftID:  sent.ID,
		Status:   sent.Status,
		Title:    sent.Title,
		Summary:  sent.Summary,
		Executed: true,
		Content:  content,
	}, nil
}

// SendWindow reports the earliest moment an action may leave this workspace.
func (s *Service) SendWindow(ctx context.Context, scope domain.Scope) (time.Time, error) {
	if err := requireScope(scope); err != nil {
		return time.Time{}, err
	}

	open, next := s.window(ctx, scope)
	if open {
		return s.clock(), nil
	}
	return next, nil
}

// window reports whether actions may leave now, and when they may next.
//
// The hours are read per call rather than cached, so changing them or the
// timezone recomputes the hold instead of leaving a stale one behind.
func (s *Service) window(ctx context.Context, scope domain.Scope) (bool, time.Time) {
	if s.deps.Hours == nil {
		return true, s.clock()
	}

	hours, err := s.deps.Hours.WorkingHours(ctx, scope.WorkspaceID)
	if err != nil {
		if s.deps.Logger != nil {
			s.deps.Logger.Warn("could not read the workspace's working hours; treating the window as open",
				zap.String("workspace_id", scope.WorkspaceID.String()), zap.Error(err))
		}
		return true, s.clock()
	}

	return hours.Open(s.clock())
}

// Drafts lists the workspace's drafts, newest first.
func (s *Service) Drafts(ctx context.Context, scope domain.Scope, filter domain.Filter) ([]domain.Draft, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	if filter.Status != "" && !knownStatus(filter.Status) {
		return nil, fmt.Errorf("%w: %q", domain.ErrInvalidInput, filter.Status)
	}

	return s.deps.Drafts.ListDrafts(ctx, scope, filter)
}

// Draft returns one draft.
func (s *Service) Draft(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Draft, error) {
	if err := requireScope(scope); err != nil {
		return domain.Draft{}, err
	}
	if id == uuid.Nil {
		return domain.Draft{}, fmt.Errorf("%w: draft id is required", domain.ErrInvalidInput)
	}

	return s.deps.Drafts.GetDraft(ctx, scope, id)
}

// Revisions returns a draft's trail, oldest first.
func (s *Service) Revisions(ctx context.Context, scope domain.Scope, draftID uuid.UUID) ([]domain.Revision, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}

	// Reading the draft first confirms the caller may see it: a draft id from
	// another workspace answers not found rather than an empty list.
	if _, err := s.deps.Drafts.GetDraft(ctx, scope, draftID); err != nil {
		return nil, err
	}

	return s.deps.Drafts.Revisions(ctx, scope, draftID)
}

// Policies returns the effective policy for every action kind that reaches
// outside Bolu.
//
// A kind the workspace never set a policy for is reported as the conservative
// default rather than omitted: the screen then shows what is actually in force,
// and there is no kind that silently has no policy.
func (s *Service) Policies(ctx context.Context, scope domain.Scope) ([]domain.Policy, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}

	stored, err := s.deps.Drafts.Policies(ctx, scope)
	if err != nil {
		return nil, err
	}

	kinds, err := s.deps.Drafts.ActionKinds(ctx)
	if err != nil {
		return nil, err
	}

	byKind := make(map[string]domain.Policy, len(stored))
	for _, policy := range stored {
		byKind[policy.ActionKind] = policy
	}

	policies := make([]domain.Policy, 0, len(kinds)+len(stored))
	seen := make(map[string]bool, len(kinds))
	for _, kind := range kinds {
		seen[kind] = true
		if policy, ok := byKind[kind]; ok {
			policies = append(policies, policy)
			continue
		}
		policies = append(policies, domain.DefaultPolicy(kind))
	}

	// A stored policy for an action kind the catalogue no longer ships is still
	// reported: dropping it would hide a decision the workspace made.
	for _, policy := range stored {
		if !seen[policy.ActionKind] {
			policies = append(policies, policy)
		}
	}

	return policies, nil
}

// SetPolicy stores a workspace's decision about one action kind.
func (s *Service) SetPolicy(ctx context.Context, scope domain.Scope, req domain.PolicyRequest) (domain.Policy, error) {
	if err := requireScope(scope); err != nil {
		return domain.Policy{}, err
	}

	kind := strings.TrimSpace(req.ActionKind)
	if kind == "" {
		return domain.Policy{}, fmt.Errorf("%w: action kind is required", domain.ErrInvalidInput)
	}
	if !knownMode(req.Mode) {
		return domain.Policy{}, fmt.Errorf("%w: %q", domain.ErrUnknownMode, req.Mode)
	}
	if req.MaxAmount != nil && *req.MaxAmount < 0 {
		return domain.Policy{}, fmt.Errorf("%w: the ceiling cannot be negative", domain.ErrInvalidInput)
	}

	policy, err := s.deps.Drafts.SetPolicy(ctx, scope, domain.Policy{
		ActionKind: kind,
		Mode:       req.Mode,
		MaxAmount:  req.MaxAmount,
		UpdatedBy:  scope.UserID,
	})
	if err != nil {
		return domain.Policy{}, err
	}

	s.audit(ctx, scope, domain.AuditEntry{
		Action:      "policy.updated",
		ObjectType:  "approval_policy",
		ObjectID:    policy.ActionKind,
		ActorUserID: scope.UserID,
		Metadata: map[string]any{
			"action_kind": policy.ActionKind,
			"mode":        policy.Mode,
			"max_amount":  policy.MaxAmount,
		},
	})

	return policy, nil
}

// Audit returns the trail the filter names, newest first.
func (s *Service) Audit(ctx context.Context, scope domain.Scope, filter domain.AuditFilter) ([]domain.AuditRecord, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}

	return s.deps.Drafts.AuditTrail(ctx, scope, filter)
}

// effectivePolicy reads the policy in force for one action kind.
//
// An action kind the workspace never set a policy for is `always_draft`. That is
// the product's first design principle, and it is enforced here rather than by
// hoping every workspace has a row: a missing row is the safe answer, not an
// error and not an implicit yes.
func (s *Service) effectivePolicy(ctx context.Context, scope domain.Scope, actionKind string) (domain.Policy, error) {
	policy, err := s.deps.Drafts.Policy(ctx, scope, actionKind)
	if err == nil {
		return policy, nil
	}
	if errors.Is(err, domain.ErrPolicyNotFound) {
		return domain.DefaultPolicy(actionKind), nil
	}
	return domain.Policy{}, err
}

// audit records one entry. A trail that could not be written is reported rather
// than failing the action: the decision already happened, and refusing it after
// the fact would leave the draft in a state nobody decided.
func (s *Service) audit(ctx context.Context, scope domain.Scope, entry domain.AuditEntry) {
	if err := s.deps.Drafts.RecordAudit(ctx, scope, entry); err != nil && s.deps.Logger != nil {
		s.deps.Logger.Warn("could not record an approval audit entry",
			zap.String("action", entry.Action), zap.Error(err))
	}
}

// emit publishes a draft's state.
func (s *Service) emit(ctx context.Context, scope domain.Scope, draft domain.Draft, eventType string) {
	if s.deps.Events == nil {
		return
	}

	payload, err := json.Marshal(map[string]any{
		"draft_id":    draft.ID.String(),
		"task_id":     draft.TaskID.String(),
		"agent_id":    draft.AgentID.String(),
		"action_kind": draft.ActionKind,
		"status":      draft.Status,
		"summary":     draft.Summary,
		"title":       draft.Title,
		"revision":    draft.Revision,
		"not_before":  wireTime(draft.NotBefore),
		"blocked":     draft.BlockedReason,
	})
	if err != nil {
		return
	}

	event := domain.Event{
		WorkspaceID:    scope.WorkspaceID,
		Type:           eventType,
		ActorAgentID:   draft.AgentID,
		ActorUserID:    draft.DecidedBy,
		ConversationID: draft.ConversationID,
		TaskID:         draft.TaskID,
		DraftID:        draft.ID,
		Payload:        payload,
	}

	if err := s.deps.Events.Emit(ctx, event); err != nil && s.deps.Logger != nil {
		s.deps.Logger.Warn("draft event published only to the store",
			zap.String("type", eventType), zap.Error(err))
	}
}

func (s *Service) validate(scope domain.Scope, req domain.Request) error {
	if err := requireScope(scope); err != nil {
		return err
	}
	if req.TaskID == uuid.Nil {
		return fmt.Errorf("%w: task id is required", domain.ErrInvalidInput)
	}
	if req.AgentID == uuid.Nil {
		return fmt.Errorf("%w: agent id is required", domain.ErrInvalidInput)
	}
	if strings.TrimSpace(req.ActionKind) == "" {
		return fmt.Errorf("%w: action kind is required", domain.ErrInvalidInput)
	}
	return nil
}

func requireScope(scope domain.Scope) error {
	if scope.IsZero() {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}
	return nil
}

func knownStatus(status string) bool {
	switch status {
	case domain.StatusPending, domain.StatusApproved, domain.StatusRevise,
		domain.StatusSent, domain.StatusCanceled:
		return true
	default:
		return false
	}
}

func knownMode(mode string) bool {
	return mode == domain.ModeAlwaysDraft || mode == domain.ModeAuto
}

// summaryOf is the one line a card shows. A caller that supplied one keeps it;
// otherwise the title stands in, because a card with no description is worse
// than a short one.
func summaryOf(req domain.Request) string {
	summary := strings.TrimSpace(req.Summary)
	if summary == "" {
		summary = strings.TrimSpace(req.Title)
	}

	runes := []rune(summary)
	if len(runes) <= SummaryMaxRunes {
		return summary
	}
	return string(runes[:SummaryMaxRunes-1]) + "…"
}

// idempotencyKey identifies one proposal.
//
// It is derived from what the proposal is rather than generated, so a retried
// activity — the same task proposing the same action with the same arguments —
// resolves to the draft already opened instead of a second one. A payload that
// differs is a different proposal, which is why the arguments are part of it.
func idempotencyKey(req domain.Request) string {
	payload := strings.TrimSpace(string(req.Payload))
	if payload == "" {
		payload = "{}"
	}
	return req.TaskID.String() + ":" + req.ActionKind + ":" + hashPayload(payload)
}

func wireTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.Format(time.RFC3339Nano)
}
