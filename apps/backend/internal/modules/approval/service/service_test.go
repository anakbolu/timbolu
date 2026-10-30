package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/approval/domain"
)

// The service tests cover the rule the epic exists for: an action that reaches
// outside Bolu waits for a person unless the workspace said otherwise for that
// specific kind of action, and every decision is recorded once.

const (
	workspaceID    = "11111111-1111-1111-1111-111111111111"
	taskID         = "22222222-2222-2222-2222-222222222222"
	agentID        = "33333333-3333-3333-3333-333333333333"
	userID         = "44444444-4444-4444-4444-444444444444"
	conversationID = "55555555-5555-5555-5555-555555555555"
)

// memoryStore is an in-memory implementation of the approval repository.
//
// It is a real implementation rather than a mock because the behaviour under test
// is a sequence of writes: a test asserts what a draft's status became and what
// the trail holds, and a mock would only assert which calls happened.
type memoryStore struct {
	drafts    map[uuid.UUID]domain.Draft
	policies  map[string]domain.Policy
	revisions []domain.Revision
	audit     []domain.AuditRecord
	kinds     []string
	// failCreate makes the insert fail, which is how a database error is driven.
	failCreate bool
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		drafts:   map[uuid.UUID]domain.Draft{},
		policies: map[string]domain.Policy{},
		kinds:    []string{"gmail.send", "whatsapp.send_message"},
	}
}

func (m *memoryStore) CreateDraft(_ context.Context, scope domain.Scope, draft domain.NewDraft) (domain.Draft, error) {
	if m.failCreate {
		return domain.Draft{}, errors.New("the database is down")
	}

	// The idempotency key is honoured, which is what makes a retried activity
	// resolve to the draft it already opened.
	for _, stored := range m.drafts {
		if stored.IdempotencyKey == draft.IdempotencyKey {
			return stored, nil
		}
	}

	created := domain.Draft{
		ID:             uuid.New(),
		WorkspaceID:    scope.WorkspaceID,
		TaskID:         draft.TaskID,
		AgentID:        draft.AgentID,
		ConversationID: draft.ConversationID,
		ActionKind:     draft.ActionKind,
		Title:          draft.Title,
		Summary:        draft.Summary,
		Payload:        draft.Payload,
		Status:         domain.StatusPending,
		IdempotencyKey: draft.IdempotencyKey,
		CreatedAt:      time.Now(),
	}
	m.drafts[created.ID] = created
	return created, nil
}

func (m *memoryStore) GetDraft(_ context.Context, scope domain.Scope, id uuid.UUID) (domain.Draft, error) {
	stored, ok := m.drafts[id]
	if !ok || stored.WorkspaceID != scope.WorkspaceID {
		return domain.Draft{}, domain.ErrDraftNotFound
	}
	return stored, nil
}

func (m *memoryStore) ListDrafts(_ context.Context, scope domain.Scope, filter domain.Filter) ([]domain.Draft, error) {
	filter = filter.Normalise()

	var drafts []domain.Draft
	for _, stored := range m.drafts {
		if stored.WorkspaceID != scope.WorkspaceID {
			continue
		}
		if filter.Status != "" && stored.Status != filter.Status {
			continue
		}
		if filter.AgentID != uuid.Nil && stored.AgentID != filter.AgentID {
			continue
		}
		if filter.ActionKind != "" && stored.ActionKind != filter.ActionKind {
			continue
		}
		if filter.TaskID != uuid.Nil && stored.TaskID != filter.TaskID {
			continue
		}
		drafts = append(drafts, stored)
	}
	return drafts, nil
}

func (m *memoryStore) Decide(_ context.Context, scope domain.Scope, decision domain.Decision) (domain.Draft, error) {
	stored, ok := m.drafts[decision.DraftID]
	if !ok || stored.WorkspaceID != scope.WorkspaceID {
		return domain.Draft{}, domain.ErrDraftNotFound
	}
	// Only a pending draft can be decided, which is what makes a duplicate signal
	// harmless: the first answer stands.
	if stored.Status != domain.StatusPending {
		return domain.Draft{}, domain.ErrAlreadyDecided
	}

	now := time.Now()
	stored.Status = decision.Status
	stored.DecidedBy = decision.DecidedBy
	stored.DecidedAt = &now
	stored.DecisionNote = decision.Note
	if decision.Status == domain.StatusRevise {
		stored.Revision++
	}
	m.drafts[stored.ID] = stored
	return stored, nil
}

func (m *memoryStore) MarkSent(_ context.Context, scope domain.Scope, id uuid.UUID) (domain.Draft, error) {
	stored, ok := m.drafts[id]
	if !ok || stored.WorkspaceID != scope.WorkspaceID {
		return domain.Draft{}, domain.ErrDraftNotFound
	}
	if stored.Status != domain.StatusApproved {
		return domain.Draft{}, domain.ErrNotApproved
	}

	now := time.Now()
	stored.Status = domain.StatusSent
	stored.SentAt = &now
	stored.NotBefore = nil
	stored.BlockedReason = ""
	m.drafts[id] = stored
	return stored, nil
}

func (m *memoryStore) Hold(_ context.Context, scope domain.Scope, id uuid.UUID, notBefore time.Time, reason string) (domain.Draft, error) {
	stored, ok := m.drafts[id]
	if !ok || stored.WorkspaceID != scope.WorkspaceID {
		return domain.Draft{}, domain.ErrDraftNotFound
	}
	if stored.Status != domain.StatusApproved {
		return domain.Draft{}, domain.ErrNotApproved
	}

	stored.NotBefore = &notBefore
	stored.BlockedReason = reason
	m.drafts[id] = stored
	return stored, nil
}

func (m *memoryStore) SetCardMessage(_ context.Context, scope domain.Scope, id, messageID uuid.UUID) (domain.Draft, error) {
	stored, ok := m.drafts[id]
	if !ok || stored.WorkspaceID != scope.WorkspaceID {
		return domain.Draft{}, domain.ErrDraftNotFound
	}
	stored.CardMessageID = messageID
	m.drafts[id] = stored
	return stored, nil
}

func (m *memoryStore) AppendRevision(_ context.Context, _ domain.Scope, revision domain.Revision) (domain.Revision, error) {
	revision.ID = uuid.New()
	revision.CreatedAt = time.Now()
	m.revisions = append(m.revisions, revision)
	return revision, nil
}

func (m *memoryStore) Revisions(_ context.Context, _ domain.Scope, draftID uuid.UUID) ([]domain.Revision, error) {
	var revisions []domain.Revision
	for _, revision := range m.revisions {
		if revision.DraftID == draftID {
			revisions = append(revisions, revision)
		}
	}
	return revisions, nil
}

func (m *memoryStore) Policies(_ context.Context, _ domain.Scope) ([]domain.Policy, error) {
	policies := make([]domain.Policy, 0, len(m.policies))
	for _, policy := range m.policies {
		policies = append(policies, policy)
	}
	return policies, nil
}

func (m *memoryStore) Policy(_ context.Context, _ domain.Scope, actionKind string) (domain.Policy, error) {
	policy, ok := m.policies[actionKind]
	if !ok {
		return domain.Policy{}, domain.ErrPolicyNotFound
	}
	return policy, nil
}

func (m *memoryStore) SetPolicy(_ context.Context, _ domain.Scope, policy domain.Policy) (domain.Policy, error) {
	policy.CreatedAt = time.Now()
	policy.UpdatedAt = time.Now()
	m.policies[policy.ActionKind] = policy
	return policy, nil
}

func (m *memoryStore) ActionKinds(context.Context) ([]string, error) { return m.kinds, nil }

func (m *memoryStore) RecordAudit(_ context.Context, _ domain.Scope, entry domain.AuditEntry) error {
	m.audit = append(m.audit, domain.AuditRecord{
		ID:           int64(len(m.audit) + 1),
		ActorUserID:  entry.ActorUserID,
		ActorAgentID: entry.ActorAgentID,
		Action:       entry.Action,
		ObjectType:   entry.ObjectType,
		ObjectID:     entry.ObjectID,
		Metadata:     entry.Metadata,
		CreatedAt:    time.Now(),
	})
	return nil
}

// AuditTrail answers the filter the way the statement does: every field narrows, and a
// value ending in a dot in Action is a prefix. The fake restates the *rule* rather than the
// SQL, which is what makes it worth having: the filter's behaviour is a product decision,
// and the statement is only one way of expressing it.
func (m *memoryStore) AuditTrail(_ context.Context, _ domain.Scope, filter domain.AuditFilter) ([]domain.AuditRecord, error) {
	matched := make([]domain.AuditRecord, 0, len(m.audit))
	for _, record := range m.audit {
		if !matchesAuditFilter(record, filter) {
			continue
		}
		matched = append(matched, record)
	}

	limit := filter.Limit
	if limit <= 0 || limit > len(matched) {
		limit = len(matched)
	}
	// Newest first, which is the order the statement returns.
	return matched[len(matched)-limit:], nil
}

// matchesAuditFilter is the filter's rule, stated once.
func matchesAuditFilter(record domain.AuditRecord, filter domain.AuditFilter) bool {
	if filter.Action != "" {
		prefix := strings.HasSuffix(filter.Action, ".")
		if prefix && !strings.HasPrefix(record.Action, filter.Action) {
			return false
		}
		if !prefix && record.Action != filter.Action {
			return false
		}
	}
	if filter.ObjectType != "" && record.ObjectType != filter.ObjectType {
		return false
	}
	if filter.ObjectID != "" && record.ObjectID != filter.ObjectID {
		return false
	}
	if filter.ActorAgentID != uuid.Nil && record.ActorAgentID != filter.ActorAgentID {
		return false
	}
	if filter.ActorUserID != uuid.Nil && record.ActorUserID != filter.ActorUserID {
		return false
	}
	if !filter.Since.IsZero() && record.CreatedAt.Before(filter.Since) {
		return false
	}
	// Until is exclusive, so two windows that abut do not return the same entry twice.
	if !filter.Until.IsZero() && !record.CreatedAt.Before(filter.Until) {
		return false
	}
	return true
}

// actions returns the audit actions recorded, in order.
func (m *memoryStore) actions() []string {
	out := make([]string, 0, len(m.audit))
	for _, record := range m.audit {
		out = append(out, record.Action)
	}
	return out
}

// countingSender records what it was asked to send.
type countingSender struct {
	sent    []domain.Draft
	content []byte
	err     error
}

func (s *countingSender) Send(_ context.Context, _ domain.Scope, draft domain.Draft) (json.RawMessage, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.sent = append(s.sent, draft)
	return s.content, nil
}

// recordingCards records the cards written and rewritten.
type recordingCards struct {
	written   []domain.Draft
	rewritten []domain.Draft
}

func (c *recordingCards) Write(_ context.Context, _ domain.Scope, draft domain.Draft) (uuid.UUID, error) {
	c.written = append(c.written, draft)
	return uuid.New(), nil
}

func (c *recordingCards) Rewrite(_ context.Context, _ domain.Scope, _ uuid.UUID, draft domain.Draft) error {
	c.rewritten = append(c.rewritten, draft)
	return nil
}

// fixedHours answers the same window every time.
type fixedHours struct {
	hours domain.WorkingHours
	err   error
}

func (h fixedHours) WorkingHours(context.Context, uuid.UUID) (domain.WorkingHours, error) {
	return h.hours, h.err
}

// countingEvents records the events published.
type countingEvents struct{ types []string }

func (c *countingEvents) Emit(_ context.Context, event domain.Event) error {
	c.types = append(c.types, event.Type)
	return nil
}

// harness is the service under test with its fakes.
type harness struct {
	service *Service
	store   *memoryStore
	sender  *countingSender
	cards   *recordingCards
	events  *countingEvents
	scope   domain.Scope
	now     time.Time
}

func newHarness(t *testing.T, mutate ...func(*Deps)) *harness {
	t.Helper()

	store := newMemoryStore()
	sender := &countingSender{content: []byte(`{"ok":true}`)}
	cards := &recordingCards{}
	events := &countingEvents{}
	now := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)

	deps := Deps{
		Drafts: store,
		Sender: sender,
		Cards:  cards,
		Events: events,
		Clock:  func() time.Time { return now },
	}
	for _, change := range mutate {
		change(&deps)
	}

	return &harness{
		service: New(deps),
		store:   store,
		sender:  sender,
		cards:   cards,
		events:  events,
		scope:   domain.Scope{UserID: uuid.MustParse(userID), WorkspaceID: uuid.MustParse(workspaceID)},
		now:     now,
	}
}

func (h *harness) request(kind string, payload string) domain.Request {
	return domain.Request{
		TaskID:         uuid.MustParse(taskID),
		AgentID:        uuid.MustParse(agentID),
		ConversationID: uuid.MustParse(conversationID),
		ActionKind:     kind,
		Title:          "Kirim tagihan Maret",
		Summary:        "Tagihan untuk pelanggan A",
		Payload:        json.RawMessage(payload),
	}
}

// TestAnActionWithNoPolicyWaitsForAPerson is the product's first design
// principle, stated as the case that matters most: a kind of action the
// workspace never made a decision about is always drafted.
func TestAnActionWithNoPolicyWaitsForAPerson(t *testing.T) {
	h := newHarness(t)

	outcome, err := h.service.Request(context.Background(), h.scope, h.request("gmail.send", `{"to":"a@b.c"}`))
	require.NoError(t, err)

	require.NotEqual(t, uuid.Nil, outcome.DraftID, "a draft was opened")
	require.Equal(t, domain.StatusPending, outcome.Status)
	require.False(t, outcome.Executed, "nothing left the building")
	require.Empty(t, h.sender.sent, "the sender was never asked")

	// The card is in the thread and the workspace was told.
	require.Len(t, h.cards.written, 1)
	require.Contains(t, h.events.types, domain.EventDraftCreated)
	require.Equal(t, []string{"draft.created"}, h.store.actions())
}

// TestAnExplicitAutoPolicyWithinItsCeilingSends is the other side: a workspace
// may decide that one kind of action does not need a person, and it is held to
// the ceiling it set.
func TestAnExplicitAutoPolicyWithinItsCeilingSends(t *testing.T) {
	h := newHarness(t)

	ceiling := int64(500_000)
	_, err := h.service.SetPolicy(context.Background(), h.scope, domain.PolicyRequest{
		ActionKind: "gmail.send",
		Mode:       domain.ModeAuto,
		MaxAmount:  &ceiling,
	})
	require.NoError(t, err)

	outcome, err := h.service.Request(context.Background(), h.scope, h.request("gmail.send", `{"amount":100000}`))
	require.NoError(t, err)

	require.True(t, outcome.Executed, "the policy allowed it to go out")
	require.Equal(t, domain.StatusSent, outcome.Status)
	require.Len(t, h.sender.sent, 1)
	require.Contains(t, h.store.actions(), "draft.approved")
	require.Contains(t, h.store.actions(), "draft.sent")

	// The record is kept even though nobody was asked, so "what did the agents
	// send" still has an answer.
	require.Len(t, h.store.drafts, 1)
}

// TestAnAutoPolicyOverItsCeilingStillAsks is the ceiling doing its job: the same
// policy, a larger amount, and a person is asked after all.
func TestAnAutoPolicyOverItsCeilingStillAsks(t *testing.T) {
	h := newHarness(t)

	ceiling := int64(50_000)
	_, err := h.service.SetPolicy(context.Background(), h.scope, domain.PolicyRequest{
		ActionKind: "gmail.send",
		Mode:       domain.ModeAuto,
		MaxAmount:  &ceiling,
	})
	require.NoError(t, err)

	outcome, err := h.service.Request(context.Background(), h.scope, h.request("gmail.send", `{"amount":900000}`))
	require.NoError(t, err)

	require.False(t, outcome.Executed)
	require.Equal(t, domain.StatusPending, outcome.Status)
	require.Empty(t, h.sender.sent)
}

// TestAnAutoPolicyCannotBeSatisfiedByAnAmountlessAction is the conservative
// reading: a policy with a ceiling cannot be met by a payload that never states
// an amount, so the person is asked.
func TestAnAutoPolicyCannotBeSatisfiedByAnAmountlessAction(t *testing.T) {
	h := newHarness(t)

	ceiling := int64(50_000)
	_, err := h.service.SetPolicy(context.Background(), h.scope, domain.PolicyRequest{
		ActionKind: "gmail.send",
		Mode:       domain.ModeAuto,
		MaxAmount:  &ceiling,
	})
	require.NoError(t, err)

	outcome, err := h.service.Request(context.Background(), h.scope, h.request("gmail.send", `{"to":"a@b.c"}`))
	require.NoError(t, err)

	require.False(t, outcome.Executed, "an unbounded action cannot fit under a ceiling")
	require.Equal(t, domain.StatusPending, outcome.Status)
}

// TestApproveCarriesTheActionOutOnce is the decision path: a person approves, the
// action leaves, and a second decision changes nothing.
func TestApproveCarriesTheActionOutOnce(t *testing.T) {
	h := newHarness(t)

	outcome, err := h.service.Request(context.Background(), h.scope, h.request("gmail.send", `{"to":"a@b.c"}`))
	require.NoError(t, err)

	approved, err := h.service.Approve(context.Background(), h.scope, outcome.DraftID, h.scope.UserID, "kirim")
	require.NoError(t, err)
	require.Equal(t, domain.StatusApproved, approved.Status)

	sent, err := h.service.CarryOut(context.Background(), h.scope, outcome.DraftID)
	require.NoError(t, err)
	require.True(t, sent.Executed)
	require.Equal(t, domain.StatusSent, sent.Status)
	require.Len(t, h.sender.sent, 1)

	// A second approval is refused rather than sending again.
	_, err = h.service.Approve(context.Background(), h.scope, outcome.DraftID, h.scope.UserID, "kirim lagi")
	require.ErrorIs(t, err, domain.ErrAlreadyDecided)
	require.Len(t, h.sender.sent, 1, "the action left exactly once")

	// And a second carry-out is refused too.
	_, err = h.service.CarryOut(context.Background(), h.scope, outcome.DraftID)
	require.ErrorIs(t, err, domain.ErrNotApproved)
	require.Len(t, h.sender.sent, 1)
}

// TestCarryOutRefusesADraftNobodyApproved is the guardrail at the last step: the
// runtime cannot send something a person never allowed.
func TestCarryOutRefusesADraftNobodyApproved(t *testing.T) {
	h := newHarness(t)

	outcome, err := h.service.Request(context.Background(), h.scope, h.request("gmail.send", `{"to":"a@b.c"}`))
	require.NoError(t, err)

	_, err = h.service.CarryOut(context.Background(), h.scope, outcome.DraftID)
	require.ErrorIs(t, err, domain.ErrNotApproved)
	require.Empty(t, h.sender.sent)
}

// TestReviseRecordsTheTrailAndNeedsANote is the revision path: the note is what
// the agent reads, so an empty one is refused, and what the person was looking at
// is kept.
func TestReviseRecordsTheTrailAndNeedsANote(t *testing.T) {
	h := newHarness(t)

	outcome, err := h.service.Request(context.Background(), h.scope, h.request("gmail.send", `{"to":"a@b.c"}`))
	require.NoError(t, err)

	_, err = h.service.Revise(context.Background(), h.scope, outcome.DraftID, h.scope.UserID, "   ")
	require.ErrorIs(t, err, domain.ErrInvalidInput, "a revision without a reason sends the agent back with nothing to change")

	revised, err := h.service.Revise(context.Background(), h.scope, outcome.DraftID, h.scope.UserID, "salah penerima")
	require.NoError(t, err)
	require.Equal(t, domain.StatusRevise, revised.Status)
	require.Equal(t, 1, revised.Revision)
	require.Equal(t, "salah penerima", revised.DecisionNote)

	require.Len(t, h.store.revisions, 1)
	require.Equal(t, "salah penerima", h.store.revisions[0].Note)
	require.JSONEq(t, `{"to":"a@b.c"}`, string(h.store.revisions[0].Payload),
		"the trail keeps what the person was looking at")
	require.Empty(t, h.sender.sent, "a revised draft never leaves")

	// The card is rewritten so a reload does not offer to approve something
	// already sent back.
	require.Len(t, h.cards.rewritten, 1)
	require.Equal(t, domain.StatusRevise, h.cards.rewritten[0].Status)
}

// TestCarryOutHoldsOutsideWorkingHours is the E7.4 rule: a person approved what
// to send, and the workspace's hours decide when.
func TestCarryOutHoldsOutsideWorkingHours(t *testing.T) {
	// 22:00 in Jakarta, and the workspace works 08:00 to 21:00.
	jakarta, err := time.LoadLocation("Asia/Jakarta")
	require.NoError(t, err)
	night := time.Date(2026, 3, 2, 22, 0, 0, 0, jakarta)

	h := newHarness(t, func(deps *Deps) {
		deps.Clock = func() time.Time { return night }
		deps.Hours = fixedHours{hours: domain.WorkingHours{
			Start: "08:00", End: "21:00", Timezone: "Asia/Jakarta",
		}}
	})

	outcome, err := h.service.Request(context.Background(), h.scope, h.request("gmail.send", `{"to":"a@b.c"}`))
	require.NoError(t, err)
	_, err = h.service.Approve(context.Background(), h.scope, outcome.DraftID, h.scope.UserID, "kirim")
	require.NoError(t, err)

	held, err := h.service.CarryOut(context.Background(), h.scope, outcome.DraftID)
	require.NoError(t, err)

	require.False(t, held.Executed, "nothing left outside working hours")
	require.NotNil(t, held.NotBefore)
	require.Empty(t, h.sender.sent)
	require.Contains(t, held.Reason, "jam kerja")

	// The hold is on the draft, so the card can say why it has not gone out.
	stored, err := h.service.Draft(context.Background(), h.scope, outcome.DraftID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusApproved, stored.Status)
	require.NotNil(t, stored.NotBefore)
	require.Contains(t, stored.BlockedReason, "jam kerja")

	// And it is not held before the window opens.
	require.Equal(t, 8, held.NotBefore.In(jakarta).Hour())
}

// TestCarryOutSendsInsideWorkingHours is the same rule from the other side.
func TestCarryOutSendsInsideWorkingHours(t *testing.T) {
	jakarta, err := time.LoadLocation("Asia/Jakarta")
	require.NoError(t, err)
	morning := time.Date(2026, 3, 2, 10, 0, 0, 0, jakarta)

	h := newHarness(t, func(deps *Deps) {
		deps.Clock = func() time.Time { return morning }
		deps.Hours = fixedHours{hours: domain.WorkingHours{
			Start: "08:00", End: "21:00", Timezone: "Asia/Jakarta",
		}}
	})

	outcome, err := h.service.Request(context.Background(), h.scope, h.request("gmail.send", `{"to":"a@b.c"}`))
	require.NoError(t, err)
	_, err = h.service.Approve(context.Background(), h.scope, outcome.DraftID, h.scope.UserID, "kirim")
	require.NoError(t, err)

	sent, err := h.service.CarryOut(context.Background(), h.scope, outcome.DraftID)
	require.NoError(t, err)
	require.True(t, sent.Executed)
	require.Len(t, h.sender.sent, 1)
}

// TestCarryOutWithoutASenderSaysSo is the honest failure: a draft marked sent when
// nothing left would be a lie the audit trail repeats forever.
func TestCarryOutWithoutASenderSaysSo(t *testing.T) {
	h := newHarness(t, func(deps *Deps) { deps.Sender = nil })

	outcome, err := h.service.Request(context.Background(), h.scope, h.request("gmail.send", `{"to":"a@b.c"}`))
	require.NoError(t, err)
	_, err = h.service.Approve(context.Background(), h.scope, outcome.DraftID, h.scope.UserID, "kirim")
	require.NoError(t, err)

	_, err = h.service.CarryOut(context.Background(), h.scope, outcome.DraftID)
	require.ErrorIs(t, err, domain.ErrNoSender)

	stored, err := h.service.Draft(context.Background(), h.scope, outcome.DraftID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusApproved, stored.Status, "it stays approved rather than pretending to have sent")
}

// TestRequestIsIdempotentForTheSameProposal is what makes a retried activity
// harmless: the same task proposing the same action resolves to the draft already
// open rather than a second one.
func TestRequestIsIdempotentForTheSameProposal(t *testing.T) {
	h := newHarness(t)

	first, err := h.service.Request(context.Background(), h.scope, h.request("gmail.send", `{"to":"a@b.c"}`))
	require.NoError(t, err)
	second, err := h.service.Request(context.Background(), h.scope, h.request("gmail.send", `{"to":"a@b.c"}`))
	require.NoError(t, err)

	require.Equal(t, first.DraftID, second.DraftID, "a retry resolves to the same draft")
	require.Len(t, h.store.drafts, 1)

	// A different payload is a different proposal.
	third, err := h.service.Request(context.Background(), h.scope, h.request("gmail.send", `{"to":"z@b.c"}`))
	require.NoError(t, err)
	require.NotEqual(t, first.DraftID, third.DraftID)
	require.Len(t, h.store.drafts, 2)
}

// TestPoliciesShowTheDefaultForEveryExternalKind is the screen's contract: a kind
// the workspace never decided on is reported as the conservative default rather
// than omitted, so there is no kind that silently has no policy.
func TestPoliciesShowTheDefaultForEveryExternalKind(t *testing.T) {
	h := newHarness(t)

	policies, err := h.service.Policies(context.Background(), h.scope)
	require.NoError(t, err)
	require.Len(t, policies, 2, "both external kinds are reported")

	for _, policy := range policies {
		require.Equal(t, domain.ModeAlwaysDraft, policy.Mode, policy.ActionKind)
	}

	// A workspace's own decision replaces the default for that kind alone.
	ceiling := int64(1000)
	_, err = h.service.SetPolicy(context.Background(), h.scope, domain.PolicyRequest{
		ActionKind: "gmail.send", Mode: domain.ModeAuto, MaxAmount: &ceiling,
	})
	require.NoError(t, err)

	policies, err = h.service.Policies(context.Background(), h.scope)
	require.NoError(t, err)

	byKind := map[string]domain.Policy{}
	for _, policy := range policies {
		byKind[policy.ActionKind] = policy
	}
	require.Equal(t, domain.ModeAuto, byKind["gmail.send"].Mode)
	require.Equal(t, domain.ModeAlwaysDraft, byKind["whatsapp.send_message"].Mode,
		"the other kind keeps the conservative default")
}

// TestSetPolicyRefusesAnUnknownMode is the query guard: a mode the schema does not
// have is refused rather than stored and ignored.
func TestSetPolicyRefusesAnUnknownMode(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.SetPolicy(context.Background(), h.scope, domain.PolicyRequest{
		ActionKind: "gmail.send", Mode: "kadang-kadang",
	})
	require.ErrorIs(t, err, domain.ErrUnknownMode)

	_, err = h.service.SetPolicy(context.Background(), h.scope, domain.PolicyRequest{
		ActionKind: "  ", Mode: domain.ModeAuto,
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	negative := int64(-1)
	_, err = h.service.SetPolicy(context.Background(), h.scope, domain.PolicyRequest{
		ActionKind: "gmail.send", Mode: domain.ModeAuto, MaxAmount: &negative,
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	// A refused decision changes nothing and records nothing.
	require.Empty(t, h.store.policies, "no policy was stored")
	require.Empty(t, h.store.actions(), "and no decision was recorded")
}

// TestDraftsAreRefusedWithoutAWorkspace is the tenant guard: no approval read or
// write runs unscoped.
func TestDraftsAreRefusedWithoutAWorkspace(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	_, err := h.service.Request(ctx, domain.Scope{}, h.request("gmail.send", `{}`))
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Drafts(ctx, domain.Scope{}, domain.Filter{})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Draft(ctx, domain.Scope{}, uuid.New())
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Policies(ctx, domain.Scope{})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.CarryOut(ctx, domain.Scope{}, uuid.New())
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Audit(ctx, domain.Scope{}, domain.AuditFilter{})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestDraftsRefuseAMalformedRequest covers the cheapest validation: a proposal
// without the ids the draft row needs never reaches the store.
func TestDraftsRefuseAMalformedRequest(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.Request(context.Background(), h.scope, domain.Request{ActionKind: "gmail.send"})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Request(context.Background(), h.scope, domain.Request{
		TaskID: uuid.MustParse(taskID), AgentID: uuid.MustParse(agentID),
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Drafts(context.Background(), h.scope, domain.Filter{Status: "entah"})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestTheSummaryIsBounded is the card's shape: a summary that is a whole document
// would make the card unreadable.
func TestTheSummaryIsBounded(t *testing.T) {
	h := newHarness(t)

	req := h.request("gmail.send", `{"to":"a@b.c"}`)
	req.Summary = ""

	outcome, err := h.service.Request(context.Background(), h.scope, req)
	require.NoError(t, err)
	require.Equal(t, "Kirim tagihan Maret", outcome.Summary, "a proposal with no summary falls back to its title")

	// A different payload, or the idempotency key would resolve this to the draft
	// the previous request already opened.
	long := h.request("gmail.send", `{"to":"panjang@b.c"}`)
	long.Summary = ""
	for range 400 {
		long.Summary += "a"
	}

	outcome, err = h.service.Request(context.Background(), h.scope, long)
	require.NoError(t, err)
	require.Len(t, []rune(outcome.Summary), SummaryMaxRunes)
}

// TestTheAuditTrailRecordsEveryDecision is the E7.6 gate: every step of a draft's
// life is attributable to a person or a Bolu.
func TestTheAuditTrailRecordsEveryDecision(t *testing.T) {
	h := newHarness(t)

	outcome, err := h.service.Request(context.Background(), h.scope, h.request("gmail.send", `{"to":"a@b.c"}`))
	require.NoError(t, err)
	_, err = h.service.Approve(context.Background(), h.scope, outcome.DraftID, h.scope.UserID, "kirim")
	require.NoError(t, err)
	_, err = h.service.CarryOut(context.Background(), h.scope, outcome.DraftID)
	require.NoError(t, err)

	require.Equal(t, []string{"draft.created", "draft.approved", "draft.sent"}, h.store.actions())

	records, err := h.service.Audit(context.Background(), h.scope, domain.AuditFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, records, 3)

	// The send is traceable to the draft, the task, and the person who allowed it.
	sent := records[2]
	require.Equal(t, "draft.sent", sent.Action)
	require.Equal(t, "draft", sent.ObjectType)
	require.Equal(t, outcome.DraftID.String(), sent.ObjectID)
	require.Equal(t, h.scope.UserID, sent.ActorUserID)
	require.Equal(t, uuid.MustParse(taskID), uuid.MustParse(sent.Metadata["task_id"].(string)))
	require.Equal(t, h.scope.UserID.String(), sent.Metadata["approved_by"])

	// The creation names the Bolu that proposed it, because no person did.
	created := records[0]
	require.Equal(t, uuid.MustParse(agentID), created.ActorAgentID)
	require.Equal(t, uuid.Nil, created.ActorUserID)
	require.Equal(t, "gmail.send", created.Metadata["action_kind"])
	require.Equal(t, domain.ModeAlwaysDraft, created.Metadata["mode"])
}

// TestAFailingAuditDoesNotFailTheDecision keeps the trail from becoming a way to
// lose a decision: the answer already happened, and refusing it afterwards would
// leave the draft in a state nobody decided.
func TestAFailingAuditDoesNotFailTheDecision(t *testing.T) {
	h := newHarness(t, func(deps *Deps) {
		deps.Drafts = &failingAuditStore{memoryStore: newMemoryStore()}
	})

	outcome, err := h.service.Request(context.Background(), h.scope, h.request("gmail.send", `{"to":"a@b.c"}`))
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, outcome.DraftID)
}

// failingAuditStore refuses every audit write.
type failingAuditStore struct{ *memoryStore }

func (s *failingAuditStore) RecordAudit(context.Context, domain.Scope, domain.AuditEntry) error {
	return errors.New("the audit table is unavailable")
}

// TestRequestStillWorksWhenTheCardCannotBeWritten is the honest degradation: the
// draft is stored and the dashboard lists it, so a thread that could not be
// written does not lose the proposal.
func TestRequestStillWorksWhenTheCardCannotBeWritten(t *testing.T) {
	h := newHarness(t, func(deps *Deps) { deps.Cards = nil })

	outcome, err := h.service.Request(context.Background(), h.scope, h.request("gmail.send", `{"to":"a@b.c"}`))
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, outcome.DraftID)

	drafts, err := h.service.Drafts(context.Background(), h.scope, domain.Filter{})
	require.NoError(t, err)
	require.Len(t, drafts, 1, "the draft is still decidable from the dashboard")
}

// TestSendWindowReportsWhenTheWindowOpens is what the workflow waits on.
func TestSendWindowReportsWhenTheWindowOpens(t *testing.T) {
	jakarta, err := time.LoadLocation("Asia/Jakarta")
	require.NoError(t, err)
	night := time.Date(2026, 3, 2, 23, 0, 0, 0, jakarta)

	h := newHarness(t, func(deps *Deps) {
		deps.Clock = func() time.Time { return night }
		deps.Hours = fixedHours{hours: domain.WorkingHours{
			Start: "08:00", End: "21:00", Timezone: "Asia/Jakarta",
		}}
	})

	next, err := h.service.SendWindow(context.Background(), h.scope)
	require.NoError(t, err)

	require.True(t, next.After(night))
	require.Equal(t, 3, next.In(jakarta).Day(), "the next window opens tomorrow")
	require.Equal(t, 8, next.In(jakarta).Hour())

	// Inside the window the answer is now, not a moment in the future.
	midday := time.Date(2026, 3, 2, 12, 0, 0, 0, jakarta)
	h2 := newHarness(t, func(deps *Deps) {
		deps.Clock = func() time.Time { return midday }
		deps.Hours = fixedHours{hours: domain.WorkingHours{
			Start: "08:00", End: "21:00", Timezone: "Asia/Jakarta",
		}}
	})

	now, err := h2.service.SendWindow(context.Background(), h2.scope)
	require.NoError(t, err)
	require.Equal(t, midday, now, "an open window means the action may leave now")
}

// TestCancelStopsADraftWithoutSending is the third decision: a person can stop a
// proposal, and stopping it never sends.
func TestCancelStopsADraftWithoutSending(t *testing.T) {
	h := newHarness(t)

	outcome, err := h.service.Request(context.Background(), h.scope, h.request("gmail.send", `{"to":"a@b.c"}`))
	require.NoError(t, err)

	canceled, err := h.service.Cancel(context.Background(), h.scope, outcome.DraftID, h.scope.UserID, "tidak jadi")
	require.NoError(t, err)
	require.Equal(t, domain.StatusCanceled, canceled.Status)
	require.Equal(t, "tidak jadi", canceled.DecisionNote)

	// And the runtime refuses to carry it out.
	_, err = h.service.CarryOut(context.Background(), h.scope, outcome.DraftID)
	require.ErrorIs(t, err, domain.ErrNotApproved)
	require.Empty(t, h.sender.sent)

	require.Contains(t, h.store.actions(), "draft.canceled")
}

// TestRevisionsReadsATrailAndRefusesAnUnknownDraft is the read the card's history
// uses, including its tenant guard.
func TestRevisionsReadsATrailAndRefusesAnUnknownDraft(t *testing.T) {
	h := newHarness(t)

	outcome, err := h.service.Request(context.Background(), h.scope, h.request("gmail.send", `{"to":"a@b.c"}`))
	require.NoError(t, err)
	_, err = h.service.Revise(context.Background(), h.scope, outcome.DraftID, h.scope.UserID, "salah penerima")
	require.NoError(t, err)

	trail, err := h.service.Revisions(context.Background(), h.scope, outcome.DraftID)
	require.NoError(t, err)
	require.Len(t, trail, 1)
	require.Equal(t, "salah penerima", trail[0].Note)

	// A draft that is not in this workspace answers not found rather than an empty
	// list, which is what keeps a task id from another tenant invisible.
	other := domain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}
	_, err = h.service.Revisions(context.Background(), other, outcome.DraftID)
	require.ErrorIs(t, err, domain.ErrDraftNotFound)

	_, err = h.service.Revisions(context.Background(), h.scope, uuid.Nil)
	require.ErrorIs(t, err, domain.ErrDraftNotFound)
}

// TestARevisionWithoutADraftIsRefused covers the guard on a decision that carries
// no id.
func TestARevisionWithoutADraftIsRefused(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.Revise(context.Background(), h.scope, uuid.Nil, h.scope.UserID, "apa saja")
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestCarryOutRefusesANilDraft is the same guard on the send path.
func TestCarryOutRefusesANilDraft(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.CarryOut(context.Background(), h.scope, uuid.Nil)
	require.ErrorIs(t, err, domain.ErrDraftNotFound)
}

// TestTheWindowIsOpenWithoutConfiguredHours keeps a deployment that never set
// working hours from holding every send forever.
func TestTheWindowIsOpenWithoutConfiguredHours(t *testing.T) {
	h := newHarness(t)

	outcome, err := h.service.Request(context.Background(), h.scope, h.request("gmail.send", `{"to":"a@b.c"}`))
	require.NoError(t, err)
	_, err = h.service.Approve(context.Background(), h.scope, outcome.DraftID, h.scope.UserID, "kirim")
	require.NoError(t, err)

	sent, err := h.service.CarryOut(context.Background(), h.scope, outcome.DraftID)
	require.NoError(t, err)
	require.True(t, sent.Executed, "a deployment with no hours lets the action leave")
}

// TestAFailingHoursReadLeavesTheWindowOpen is the safe direction for a read that
// cannot answer: refusing every send because a settings row is unreachable would
// be a worse failure than letting one through.
func TestAFailingHoursReadLeavesTheWindowOpen(t *testing.T) {
	h := newHarness(t, func(deps *Deps) {
		deps.Hours = fixedHours{err: errors.New("the settings row is unreachable")}
	})

	outcome, err := h.service.Request(context.Background(), h.scope, h.request("gmail.send", `{"to":"a@b.c"}`))
	require.NoError(t, err)
	_, err = h.service.Approve(context.Background(), h.scope, outcome.DraftID, h.scope.UserID, "kirim")
	require.NoError(t, err)

	sent, err := h.service.CarryOut(context.Background(), h.scope, outcome.DraftID)
	require.NoError(t, err)
	require.True(t, sent.Executed)
}

// TestAFailingSendIsReportedAndNotRecorded is the honest failure: an action that
// could not leave is reported rather than marked sent, so the audit trail does not
// claim something that never happened.
func TestAFailingSendIsReportedAndNotRecorded(t *testing.T) {
	h := newHarness(t, func(deps *Deps) {
		deps.Sender = &countingSender{err: errors.New("the provider is unreachable")}
	})

	outcome, err := h.service.Request(context.Background(), h.scope, h.request("gmail.send", `{"to":"a@b.c"}`))
	require.NoError(t, err)
	_, err = h.service.Approve(context.Background(), h.scope, outcome.DraftID, h.scope.UserID, "kirim")
	require.NoError(t, err)

	_, err = h.service.CarryOut(context.Background(), h.scope, outcome.DraftID)
	require.ErrorContains(t, err, "unreachable")

	stored, err := h.service.Draft(context.Background(), h.scope, outcome.DraftID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusApproved, stored.Status, "it stays approved rather than claiming to have sent")
	require.Nil(t, stored.SentAt)
	require.NotContains(t, h.store.actions(), "draft.sent")
}

// TestACreateFailureIsReported is the store's own failure reaching the caller.
func TestACreateFailureIsReported(t *testing.T) {
	h := newHarness(t, func(deps *Deps) {
		store := newMemoryStore()
		store.failCreate = true
		deps.Drafts = store
	})

	_, err := h.service.Request(context.Background(), h.scope, h.request("gmail.send", `{"to":"a@b.c"}`))
	require.ErrorContains(t, err, "database is down")
}

// TestAProposalWithoutATaskIsRefused is the request guard the draft row needs.
func TestAProposalWithoutATaskIsRefused(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.Request(context.Background(), h.scope, domain.Request{
		AgentID:    uuid.MustParse(agentID),
		ActionKind: "gmail.send",
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Request(context.Background(), h.scope, domain.Request{
		TaskID:     uuid.MustParse(taskID),
		ActionKind: "gmail.send",
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestADraftListRefusesAnUnknownStatus is the query guard on the dashboard read.
func TestADraftListRefusesAnUnknownStatus(t *testing.T) {
	h := newHarness(t)

	for _, status := range []string{
		domain.StatusPending, domain.StatusApproved, domain.StatusRevise,
		domain.StatusSent, domain.StatusCanceled,
	} {
		_, err := h.service.Drafts(context.Background(), h.scope, domain.Filter{Status: status})
		require.NoError(t, err, status)
	}

	_, err := h.service.Drafts(context.Background(), h.scope, domain.Filter{Status: "entah"})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestTheAmountIsReadFromTheShapesActionsUse covers the convention the ceiling
// compares against, including the case where it is absent.
func TestTheAmountIsReadFromTheShapesActionsUse(t *testing.T) {
	for _, payload := range []string{`{"amount":150000}`, `{"total":200}`, `{"total_amount":3}`, `{"value":4}`} {
		req := domain.Request{Payload: json.RawMessage(payload)}
		require.NotNil(t, req.Amount(), payload)
	}

	require.Nil(t, domain.Request{Payload: json.RawMessage(`{"to":"a@b.c"}`)}.Amount())
	require.Nil(t, domain.Request{Payload: json.RawMessage(`bukan json`)}.Amount())
	require.Nil(t, domain.Request{}.Amount())

	amount := domain.Request{Payload: json.RawMessage(`{"amount":150000}`)}.Amount()
	require.Equal(t, int64(150000), *amount)
}

// TestDefaultPolicyIsAlwaysDraft is the product's first design principle stated as
// a value: the answer for a kind nobody decided on is the safe one.
func TestDefaultPolicyIsAlwaysDraft(t *testing.T) {
	policy := domain.DefaultPolicy("gmail.send")
	require.Equal(t, domain.ModeAlwaysDraft, policy.Mode)
	require.Nil(t, policy.MaxAmount)

	require.True(t, policy.NeedsDraft(nil))
	require.True(t, policy.NeedsDraft(&[]int64{1}[0]))

	// An auto policy with no ceiling trusts the kind entirely, which a workspace is
	// allowed to decide.
	require.False(t, domain.Policy{Mode: domain.ModeAuto}.NeedsDraft(nil))
}
