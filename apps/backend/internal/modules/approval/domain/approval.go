// Package domain defines the approval contract: the draft a Bolu proposes, the
// policy that decides whether a human is asked at all, and the trail of who
// decided what.
//
// The rule the whole package exists for is in Policy.NeedsDraft: an action that
// reaches outside Bolu waits for a person unless the workspace has said, in
// writing and for that specific kind of action, that it need not.
package domain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Policy modes, matching the check constraint on `approval_policies.mode`.
const (
	// ModeAlwaysDraft asks a person every time. It is the default, and it is what
	// an action kind with no policy at all behaves as.
	ModeAlwaysDraft = "always_draft"
	// ModeAuto carries the action out unattended, but only while the value it
	// carries is within the policy's ceiling.
	ModeAuto = "auto"
)

// Draft statuses, matching the check constraint on `drafts.status`.
const (
	// StatusPending is a draft nobody has decided on.
	StatusPending = "pending"
	// StatusApproved is a draft a person allowed, which may still be held until
	// the workspace opens.
	StatusApproved = "approved"
	// StatusRevise is a draft a person sent back with a note.
	StatusRevise = "revise"
	// StatusSent is an action that actually left.
	StatusSent = "sent"
	// StatusCanceled is a draft nobody will act on.
	StatusCanceled = "canceled"
)

// DefaultListLimit is the page a client gets when it does not ask for one.
const DefaultListLimit = 50

// MaxListLimit bounds one page.
const MaxListLimit = 200

// Errors the service and the handlers map onto responses.
var (
	ErrDraftNotFound  = errors.New("approval: draft not found")
	ErrPolicyNotFound = errors.New("approval: policy not found")
	ErrInvalidInput   = errors.New("approval: invalid input")
	// ErrUnknownMode means a caller asked for a mode the schema does not have.
	ErrUnknownMode = errors.New("approval: unknown policy mode")
	// ErrAlreadyDecided means a decision arrived for a draft that has one.
	ErrAlreadyDecided = errors.New("approval: the draft has already been decided")
	// ErrNotApproved means the runtime tried to send a draft nobody approved.
	ErrNotApproved = errors.New("approval: the draft is not approved")
	// ErrNoSender means nothing can carry the action out, which is a
	// configuration fact rather than a failure to retry.
	ErrNoSender = errors.New("approval: no sender is configured")
)

// Scope is the tenant identity a query runs as.
type Scope struct {
	UserID      uuid.UUID
	WorkspaceID uuid.UUID
}

// IsZero reports whether the scope carries no workspace, which every approval
// read requires.
func (s Scope) IsZero() bool { return s.WorkspaceID == uuid.Nil }

// Policy decides whether one kind of action needs a person.
type Policy struct {
	ActionKind string
	Mode       string
	// MaxAmount caps what an auto policy may carry out unattended, in rupiah.
	// Nil means no ceiling.
	MaxAmount *int64
	UpdatedBy uuid.UUID
	CreatedAt time.Time
	UpdatedAt time.Time
}

// DefaultPolicy is what an action kind with no stored policy behaves as.
//
// It is a function rather than a shared value so a caller cannot edit the
// conservative default by accident: the zero-cost answer here is the safe one,
// and a mutable package-level value would be a way to lose that.
func DefaultPolicy(actionKind string) Policy {
	return Policy{ActionKind: actionKind, Mode: ModeAlwaysDraft}
}

// NeedsDraft reports whether an action must wait for a person.
//
// The answer is yes unless the workspace opted out for this action kind, and even
// then only while the value is known and within the ceiling. An action whose
// amount cannot be read is treated as unbounded: a policy that names a ceiling
// cannot be satisfied by a payload that never states one.
func (p Policy) NeedsDraft(amount *int64) bool {
	if p.Mode != ModeAuto {
		return true
	}
	if p.MaxAmount == nil {
		// An auto policy with no ceiling means the workspace trusts this action
		// kind entirely, which is a choice it is allowed to make.
		return false
	}
	if amount == nil {
		return true
	}
	return *amount > *p.MaxAmount
}

// NewDraft is a draft to store.
type NewDraft struct {
	TaskID         uuid.UUID
	AgentID        uuid.UUID
	ConversationID uuid.UUID
	ActionKind     string
	Title          string
	Summary        string
	Payload        json.RawMessage
	// IdempotencyKey identifies the proposal, so a retried activity cannot open
	// the same draft twice.
	IdempotencyKey string
}

// Draft is one proposed action and what became of it.
type Draft struct {
	ID             uuid.UUID
	WorkspaceID    uuid.UUID
	TaskID         uuid.UUID
	AgentID        uuid.UUID
	ConversationID uuid.UUID
	ActionKind     string
	Title          string
	Summary        string
	Payload        json.RawMessage
	Status         string
	// Revision counts how many times this draft was sent back.
	Revision     int
	DecidedBy    uuid.UUID
	DecidedAt    *time.Time
	DecisionNote string
	// NotBefore is the earliest moment the action may leave, set when it was
	// decided outside the workspace's working hours.
	NotBefore *time.Time
	// BlockedReason says why it has not gone out, in the user's words.
	BlockedReason string
	SentAt        *time.Time
	// CardMessageID is the message in the thread carrying the card.
	CardMessageID  uuid.UUID
	IdempotencyKey string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Waiting reports whether the draft still needs a person.
func (d Draft) Waiting() bool { return d.Status == StatusPending }

// Held reports whether an approved action is waiting for the working day.
func (d Draft) Held() bool {
	return d.Status == StatusApproved && d.NotBefore != nil && d.NotBefore.After(time.Now())
}

// Revision is one turn of the trail: what the person was looking at, and what
// they said about it.
type Revision struct {
	ID        uuid.UUID
	DraftID   uuid.UUID
	Revision  int
	Note      string
	Payload   json.RawMessage
	DecidedBy uuid.UUID
	CreatedAt time.Time
}

// Decision is a person's answer about a draft.
type Decision struct {
	DraftID uuid.UUID
	// Status is approved, revise, or canceled.
	Status string
	// Note is what the person said, which the agent reads when the draft was
	// sent back.
	Note string
	// DecidedBy is who answered.
	DecidedBy uuid.UUID
}

// AuditEntry is one thing that happened, for the record.
type AuditEntry struct {
	// Action names what happened, as `<object>.<verb>`: draft.created,
	// draft.approved, draft.sent, policy.updated, grant.set, member.role_changed,
	// provider.created. The vocabulary is not enumerated in a constraint on purpose —
	// a new module recording its own action should not need a migration — so the
	// convention is what keeps the filter useful.
	Action string
	// ObjectType and ObjectID name what it happened to.
	ObjectType string
	ObjectID   string
	// ActorUserID is who did it, when a person did.
	ActorUserID uuid.UUID
	// ActorAgentID is the Bolu that proposed it, when one did.
	ActorAgentID uuid.UUID
	// Metadata is the detail: a payload summary, never a secret.
	Metadata map[string]any
}

// Filter narrows a draft list.
type Filter struct {
	// Status limits the read to one status; empty means every status.
	Status string
	// AgentID limits the read to one Bolu.
	AgentID uuid.UUID
	// ActionKind limits the read to one kind of action.
	ActionKind string
	// TaskID limits the read to the drafts one task produced.
	TaskID uuid.UUID
	Limit  int
}

// Normalise fills the limit from the defaults.
func (f Filter) Normalise() Filter {
	switch {
	case f.Limit <= 0:
		f.Limit = DefaultListLimit
	case f.Limit > MaxListLimit:
		f.Limit = MaxListLimit
	}
	return f
}

// Repository is the persistence contract for approvals.
type Repository interface {
	// CreateDraft stores a proposal. A repeated idempotency key returns the
	// stored draft rather than a second one.
	CreateDraft(ctx context.Context, scope Scope, draft NewDraft) (Draft, error)
	// GetDraft returns one draft.
	GetDraft(ctx context.Context, scope Scope, id uuid.UUID) (Draft, error)
	// ListDrafts returns the workspace's drafts, newest first.
	ListDrafts(ctx context.Context, scope Scope, filter Filter) ([]Draft, error)
	// Decide records a person's answer, but only while the draft is still
	// pending: a second decision changes nothing.
	Decide(ctx context.Context, scope Scope, decision Decision) (Draft, error)
	// MarkSent records that the action left.
	MarkSent(ctx context.Context, scope Scope, id uuid.UUID) (Draft, error)
	// Hold records that an approved action is waiting for the working day.
	Hold(ctx context.Context, scope Scope, id uuid.UUID, notBefore time.Time, reason string) (Draft, error)
	// SetCardMessage records the message carrying the card.
	SetCardMessage(ctx context.Context, scope Scope, id, messageID uuid.UUID) (Draft, error)
	// AppendRevision adds one turn to the trail.
	AppendRevision(ctx context.Context, scope Scope, revision Revision) (Revision, error)
	// Revisions returns a draft's trail, oldest first.
	Revisions(ctx context.Context, scope Scope, draftID uuid.UUID) ([]Revision, error)

	// Policies returns the workspace's stored policies.
	Policies(ctx context.Context, scope Scope) ([]Policy, error)
	// Policy returns one action kind's policy, or ErrPolicyNotFound when the
	// workspace never set one.
	Policy(ctx context.Context, scope Scope, actionKind string) (Policy, error)
	// SetPolicy stores a decision about one action kind.
	SetPolicy(ctx context.Context, scope Scope, policy Policy) (Policy, error)
	// ActionKinds lists the action kinds that reach outside Bolu, which is what a
	// workspace has policies for.
	ActionKinds(ctx context.Context) ([]string, error)

	// RecordAudit appends one entry to the audit trail.
	RecordAudit(ctx context.Context, scope Scope, entry AuditEntry) error
	// AuditTrail returns the entries the filter names, newest first.
	AuditTrail(ctx context.Context, scope Scope, filter AuditFilter) ([]AuditRecord, error)
}

// AuditFilter narrows a trail read.
//
// Every field is optional and they combine, because the three questions an incident asks
// are different and one of them is always the axis: what did this Bolu do, who touched
// this thing, and what happened in this window.
type AuditFilter struct {
	// Action narrows to one action. A value ending in a dot is a prefix, so
	// `grant.` reads every grant change without naming each verb.
	Action string
	// ObjectType and ObjectID narrow to one kind of thing, or one thing.
	ObjectType string
	ObjectID   string
	// ActorAgentID narrows to one Bolu, ActorUserID to one person.
	ActorAgentID uuid.UUID
	ActorUserID  uuid.UUID
	// Since and Until bound the window. Until is exclusive, so two windows that abut
	// do not return the same entry twice.
	Since time.Time
	Until time.Time
	// Limit caps one page. Zero takes the default.
	Limit int
}

// Window reports whether the filter asks for a bounded window, which is what tells a
// repository whether the time clauses belong in the statement at all.
func (f AuditFilter) Window() bool { return !f.Since.IsZero() || !f.Until.IsZero() }

// AuditRecord is one stored entry.
type AuditRecord struct {
	ID           int64
	ActorUserID  uuid.UUID
	ActorAgentID uuid.UUID
	Action       string
	ObjectType   string
	ObjectID     string
	Metadata     map[string]any
	CreatedAt    time.Time
}

// Sender carries out an approved action.
//
// It is a port rather than a direct call because sending is an integration's job:
// EPIC 8 owns the real executor and this is how the approval runtime asks for
// one. A deployment without a sender holds the draft and says so, rather than
// recording a send that never happened.
type Sender interface {
	Send(ctx context.Context, scope Scope, draft Draft) (json.RawMessage, error)
}

// Cards writes the draft card into the conversation a task came from, so a person
// decides in the thread they are already reading.
type Cards interface {
	// Write puts the card in the thread and returns the message carrying it.
	Write(ctx context.Context, scope Scope, draft Draft) (uuid.UUID, error)
	// Rewrite updates that message after a decision, so a reload shows what
	// happened rather than a card still offering to approve something sent.
	Rewrite(ctx context.Context, scope Scope, messageID uuid.UUID, draft Draft) error
}

// Event names the approval publishes, which is how the dashboard and the chat
// card learn about a decision without polling.
const (
	EventDraftCreated = "draft.created"
	EventDraftDecided = "draft.decided"
)

// Event is one entry of the activity stream.
//
// It mirrors the chat module's event rather than importing it: the container
// adapts the two, so neither module reaches into the other.
type Event struct {
	WorkspaceID    uuid.UUID
	Type           string
	ActorAgentID   uuid.UUID
	ActorUserID    uuid.UUID
	ConversationID uuid.UUID
	TaskID         uuid.UUID
	DraftID        uuid.UUID
	Payload        json.RawMessage
}

// EventSink publishes what a draft is doing.
type EventSink interface {
	Emit(ctx context.Context, event Event) error
}

// WorkingHours is the window a workspace lets actions leave in.
//
// It is read at the moment a decision is carried out rather than when the draft
// was made, so changing the hours or the timezone recomputes the hold instead of
// leaving a stale one.
type WorkingHours struct {
	// Start and End are wall-clock times in the workspace's own zone, e.g.
	// 08:00 and 21:00.
	Start string
	End   string
	// Timezone is the IANA name, e.g. Asia/Jakarta.
	Timezone string
}

// Open reports whether an action may leave at now, and when it may next.
//
// A window that closes at or before it opens is read as spanning midnight, which
// is what a workspace that works nights means. An unparsable setting leaves the
// window open: refusing every send because a timezone name is wrong would be a
// worse failure than letting one through, and the mistake is visible in the
// policy screen.
func (w WorkingHours) Open(now time.Time) (bool, time.Time) {
	location, err := time.LoadLocation(strings.TrimSpace(w.Timezone))
	if err != nil || location == nil {
		return true, now
	}

	start, okStart := parseClock(w.Start)
	end, okEnd := parseClock(w.End)
	if !okStart || !okEnd {
		return true, now
	}

	local := now.In(location)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)

	opens := today.Add(start)
	closes := today.Add(end)

	if start <= end {
		if !local.Before(opens) && local.Before(closes) {
			return true, now
		}
		if local.Before(opens) {
			return false, opens
		}
		return false, opens.AddDate(0, 0, 1)
	}

	// A window that spans midnight is open from `start` today until `end`
	// tomorrow, and it is also still open in the early hours from yesterday.
	if !local.Before(opens) || local.Before(closes) {
		return true, now
	}
	return false, opens
}

// parseClock reads an "HH:MM" setting into a duration since midnight.
func parseClock(value string) (time.Duration, bool) {
	parsed, err := time.Parse("15:04", strings.TrimSpace(value))
	if err != nil {
		return 0, false
	}
	return time.Duration(parsed.Hour())*time.Hour + time.Duration(parsed.Minute())*time.Minute, true
}

// Clock reports the time, so the working-hours rule is testable without waiting.
type Clock func() time.Time
