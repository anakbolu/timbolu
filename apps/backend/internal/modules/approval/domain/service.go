package domain

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Request is an action a task wants to take that reaches outside Bolu.
type Request struct {
	TaskID         uuid.UUID
	AgentID        uuid.UUID
	ConversationID uuid.UUID
	ActionKind     string
	Title          string
	Summary        string
	Payload        json.RawMessage
}

// Amount reads the value an action carries, which is what a policy's ceiling is
// compared against.
//
// The field is a convention rather than a schema, because the action's own shape
// belongs to its integration: a payload that states no amount is treated as
// unbounded by Policy.NeedsDraft, which is the conservative reading.
func (r Request) Amount() *int64 {
	var payload map[string]any
	if err := json.Unmarshal(r.Payload, &payload); err != nil {
		return nil
	}

	for _, key := range []string{"amount", "total", "total_amount", "value"} {
		raw, ok := payload[key]
		if !ok {
			continue
		}
		switch value := raw.(type) {
		case float64:
			amount := int64(value)
			return &amount
		case json.Number:
			amount, err := value.Int64()
			if err != nil {
				return nil
			}
			return &amount
		}
	}
	return nil
}

// Outcome is what happened to a proposal.
type Outcome struct {
	// DraftID is set when a person has to decide.
	DraftID uuid.UUID
	// Status is the draft's status: pending when it is waiting, sent when the
	// policy carried it out.
	Status  string
	Title   string
	Summary string
	// Executed says the action ran without a person, and Content is what it
	// answered.
	Executed bool
	Content  json.RawMessage
	// Reason explains a refusal or a hold, in the user's words.
	Reason string
	// NotBefore is when a held action may leave.
	NotBefore *time.Time
}

// PolicyRequest is one policy to store.
type PolicyRequest struct {
	ActionKind string
	Mode       string
	// MaxAmount is the ceiling an auto policy may carry out unattended, in
	// rupiah. Nil means no ceiling.
	MaxAmount *int64
}

// Service is the approval use case contract.
// Metrics is where the approval gate reports what people did with its drafts.
//
// It is a port because the module owns no telemetry: the container implements it over whatever the
// deployment configured, and a deployment with metrics off is handed nothing.
//
// What it carries is deliberately two values and nothing else. A draft's identity, its task, and its
// workspace are unbounded — one time series per draft — and the per-draft question is answered from
// the `drafts` table, where the row already is. What the fleet's charts need is the *rate*: how
// often a person approves what the bot produced, and how often they have to send it back first.
type Metrics interface {
	// DraftDecided reports one decision. outcome is the deployment's own vocabulary — approved,
	// revised, canceled — and revision is whether this decision followed a revision, which is
	// the difference between "approved as written" and "approved after being corrected".
	DraftDecided(ctx context.Context, outcome string, revision bool)
}

type Service interface {
	// Request either stores a proposal for a person or, when the workspace's
	// policy allows it, carries the action out now.
	Request(ctx context.Context, scope Scope, req Request) (Outcome, error)
	// Approve records that a person allowed a draft.
	Approve(ctx context.Context, scope Scope, draftID, userID uuid.UUID, note string) (Draft, error)
	// Revise records that a person sent a draft back, with the note the agent
	// reads on its next round.
	Revise(ctx context.Context, scope Scope, draftID, userID uuid.UUID, note string) (Draft, error)
	// Cancel records that a person stopped a draft.
	Cancel(ctx context.Context, scope Scope, draftID, userID uuid.UUID, note string) (Draft, error)
	// CarryOut executes an approved draft, holding it when the workspace is
	// closed and reporting the moment it may leave.
	CarryOut(ctx context.Context, scope Scope, draftID uuid.UUID) (Outcome, error)
	// SendWindow reports the earliest moment an action may leave this workspace.
	SendWindow(ctx context.Context, scope Scope) (time.Time, error)

	// Drafts lists the workspace's drafts, newest first.
	Drafts(ctx context.Context, scope Scope, filter Filter) ([]Draft, error)
	// Draft returns one draft.
	Draft(ctx context.Context, scope Scope, id uuid.UUID) (Draft, error)
	// Revisions returns a draft's trail, oldest first.
	Revisions(ctx context.Context, scope Scope, draftID uuid.UUID) ([]Revision, error)

	// Policies returns the effective policy for every action kind that reaches
	// outside Bolu, materialising the conservative default where a workspace
	// never set one.
	Policies(ctx context.Context, scope Scope) ([]Policy, error)
	// SetPolicy stores a workspace's decision about one action kind.
	SetPolicy(ctx context.Context, scope Scope, req PolicyRequest) (Policy, error)

	// Audit returns the trail the filter names, newest first.
	Audit(ctx context.Context, scope Scope, filter AuditFilter) ([]AuditRecord, error)
}
