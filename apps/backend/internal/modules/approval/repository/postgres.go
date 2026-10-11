// Package repository implements the approval data access with pgx and the
// sqlc-generated queries.
//
// Every statement runs inside the caller's tenant scope, so Row Level Security
// filters it even when a query forgets its workspace filter. The writes are
// guarded by status in the statement itself: a second decision on a draft
// changes nothing, which is what makes a duplicate signal harmless.
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/approval/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/approval/repository/sqlcgen"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
)

// Repository is the Postgres-backed implementation of the approval ports.
type Repository struct {
	pool *database.Pool
}

// New builds the repository.
func New(pool *database.Pool) *Repository {
	return &Repository{pool: pool}
}

// CreateDraft stores a proposal, returning the stored draft.
//
// The statement carries an ON CONFLICT on the idempotency key, so a retried
// activity returns the draft it already opened rather than a second one for the
// same action.
func (r *Repository) CreateDraft(ctx context.Context, scope domain.Scope, draft domain.NewDraft) (domain.Draft, error) {
	var stored domain.Draft

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.CreateDraft(ctx, sqlcgen.CreateDraftParams{
			WorkspaceID:    scope.WorkspaceID,
			TaskID:         draft.TaskID,
			AgentID:        draft.AgentID,
			ActionKind:     draft.ActionKind,
			Payload:        payloadOr(draft.Payload),
			Title:          draft.Title,
			Summary:        draft.Summary,
			IdempotencyKey: draft.IdempotencyKey,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// The insert is a SELECT, so a task or an agent outside this
				// workspace comes back as no rows rather than as a constraint
				// violation.
				return fmt.Errorf("%w: the task or the Bolu is not in this workspace", domain.ErrInvalidInput)
			}
			return translateWriteError(err, "create draft")
		}

		stored = toDraft(row, draft.ConversationID)
		return nil
	})
	if err != nil {
		return domain.Draft{}, err
	}

	return stored, nil
}

// GetDraft returns one draft.
func (r *Repository) GetDraft(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Draft, error) {
	var draft domain.Draft

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetDraft(ctx, sqlcgen.GetDraftParams{ID: id, WorkspaceID: scope.WorkspaceID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrDraftNotFound
			}
			return fmt.Errorf("approval: get draft: %w", err)
		}

		draft = toDraft(row.Draft, deref(row.ConversationID))
		return nil
	})
	if err != nil {
		return domain.Draft{}, err
	}

	return draft, nil
}

// ListDrafts returns the workspace's drafts, newest first.
func (r *Repository) ListDrafts(ctx context.Context, scope domain.Scope, filter domain.Filter) ([]domain.Draft, error) {
	filter = filter.Normalise()

	var drafts []domain.Draft

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListDrafts(ctx, sqlcgen.ListDraftsParams{
			WorkspaceID: scope.WorkspaceID,
			Status:      optionalString(filter.Status),
			AgentID:     optional(filter.AgentID),
			ActionKind:  optionalString(filter.ActionKind),
			TaskID:      optional(filter.TaskID),
			LimitCount:  int32(filter.Limit),
		})
		if err != nil {
			return fmt.Errorf("approval: list drafts: %w", err)
		}

		drafts = make([]domain.Draft, 0, len(rows))
		for _, row := range rows {
			drafts = append(drafts, toDraft(row.Draft, deref(row.ConversationID)))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return drafts, nil
}

// Decide records a person's answer.
//
// The statement only matches a draft that is still pending, so a second decision
// changes nothing: the caller gets ErrAlreadyDecided and the stored answer
// stands.
func (r *Repository) Decide(ctx context.Context, scope domain.Scope, decision domain.Decision) (domain.Draft, error) {
	var draft domain.Draft

	bump := int32(0)
	if decision.Status == domain.StatusRevise {
		bump = 1
	}

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.DecideDraft(ctx, sqlcgen.DecideDraftParams{
			ID:           decision.DraftID,
			WorkspaceID:  scope.WorkspaceID,
			Status:       decision.Status,
			DecidedBy:    optional(decision.DecidedBy),
			Note:         decision.Note,
			BumpRevision: bump,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return distinguish(ctx, queries, scope, decision.DraftID, domain.ErrAlreadyDecided)
			}
			return translateWriteError(err, "decide draft")
		}

		draft = toDraft(row, uuid.Nil)
		return nil
	})
	if err != nil {
		return domain.Draft{}, err
	}

	return draft, nil
}

// MarkSent records that the action left.
func (r *Repository) MarkSent(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Draft, error) {
	var draft domain.Draft

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.MarkDraftSent(ctx, sqlcgen.MarkDraftSentParams{ID: id, WorkspaceID: scope.WorkspaceID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// The draft is not approved, or already sent: either way there is
				// nothing to send, and saying so is honest — unless it is not this
				// workspace's draft at all.
				return distinguish(ctx, queries, scope, id, domain.ErrNotApproved)
			}
			return fmt.Errorf("approval: mark sent: %w", err)
		}

		draft = toDraft(row, uuid.Nil)
		return nil
	})
	if err != nil {
		return domain.Draft{}, err
	}

	return draft, nil
}

// Hold records that an approved action is waiting for the working day.
func (r *Repository) Hold(ctx context.Context, scope domain.Scope, id uuid.UUID, notBefore time.Time, reason string) (domain.Draft, error) {
	var draft domain.Draft

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.HoldDraft(ctx, sqlcgen.HoldDraftParams{
			ID:            id,
			WorkspaceID:   scope.WorkspaceID,
			NotBefore:     &notBefore,
			BlockedReason: reason,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return distinguish(ctx, queries, scope, id, domain.ErrNotApproved)
			}
			return translateWriteError(err, "hold draft")
		}

		draft = toDraft(row, uuid.Nil)
		return nil
	})
	if err != nil {
		return domain.Draft{}, err
	}

	return draft, nil
}

// SetCardMessage records the message carrying the card.
func (r *Repository) SetCardMessage(ctx context.Context, scope domain.Scope, id, messageID uuid.UUID) (domain.Draft, error) {
	var draft domain.Draft

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.SetDraftCard(ctx, sqlcgen.SetDraftCardParams{
			ID:            id,
			WorkspaceID:   scope.WorkspaceID,
			CardMessageID: optional(messageID),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrDraftNotFound
			}
			return fmt.Errorf("approval: set card: %w", err)
		}

		draft = toDraft(row, uuid.Nil)
		return nil
	})
	if err != nil {
		return domain.Draft{}, err
	}

	return draft, nil
}

// AppendRevision adds one turn to the trail.
func (r *Repository) AppendRevision(ctx context.Context, scope domain.Scope, revision domain.Revision) (domain.Revision, error) {
	var stored domain.Revision

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.AppendDraftRevision(ctx, sqlcgen.AppendDraftRevisionParams{
			WorkspaceID: scope.WorkspaceID,
			DraftID:     revision.DraftID,
			Revision:    int32(revision.Revision),
			Note:        revision.Note,
			Payload:     payloadOr(revision.Payload),
			DecidedBy:   optional(revision.DecidedBy),
		})
		if err != nil {
			return translateWriteError(err, "append revision")
		}

		stored = toRevision(row)
		return nil
	})
	if err != nil {
		return domain.Revision{}, err
	}

	return stored, nil
}

// Revisions returns a draft's trail, oldest first.
func (r *Repository) Revisions(ctx context.Context, scope domain.Scope, draftID uuid.UUID) ([]domain.Revision, error) {
	var revisions []domain.Revision

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListDraftRevisions(ctx, sqlcgen.ListDraftRevisionsParams{
			WorkspaceID: scope.WorkspaceID,
			DraftID:     draftID,
		})
		if err != nil {
			return fmt.Errorf("approval: list revisions: %w", err)
		}

		revisions = make([]domain.Revision, 0, len(rows))
		for _, row := range rows {
			revisions = append(revisions, toRevision(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return revisions, nil
}

// Policies returns the workspace's stored policies.
func (r *Repository) Policies(ctx context.Context, scope domain.Scope) ([]domain.Policy, error) {
	var policies []domain.Policy

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListPolicies(ctx, scope.WorkspaceID)
		if err != nil {
			return fmt.Errorf("approval: list policies: %w", err)
		}

		policies = make([]domain.Policy, 0, len(rows))
		for _, row := range rows {
			policies = append(policies, toPolicy(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return policies, nil
}

// Policy returns one action kind's policy.
func (r *Repository) Policy(ctx context.Context, scope domain.Scope, actionKind string) (domain.Policy, error) {
	var policy domain.Policy

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetPolicy(ctx, sqlcgen.GetPolicyParams{
			WorkspaceID: scope.WorkspaceID,
			ActionKind:  actionKind,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrPolicyNotFound
			}
			return fmt.Errorf("approval: get policy: %w", err)
		}

		policy = toPolicy(row)
		return nil
	})
	if err != nil {
		return domain.Policy{}, err
	}

	return policy, nil
}

// SetPolicy stores a decision about one action kind.
func (r *Repository) SetPolicy(ctx context.Context, scope domain.Scope, policy domain.Policy) (domain.Policy, error) {
	var stored domain.Policy

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.UpsertPolicy(ctx, sqlcgen.UpsertPolicyParams{
			WorkspaceID: scope.WorkspaceID,
			ActionKind:  policy.ActionKind,
			Mode:        policy.Mode,
			MaxAmount:   policy.MaxAmount,
			UpdatedBy:   optional(policy.UpdatedBy),
		})
		if err != nil {
			return translateWriteError(err, "set policy")
		}

		stored = toPolicy(row)
		return nil
	})
	if err != nil {
		return domain.Policy{}, err
	}

	return stored, nil
}

// SeedPolicies gives a workspace the conservative rows it starts with.
//
// It is called inside the onboarding transaction, so a workspace never exists
// without a policy for every action that reaches outside it. An existing
// decision is left alone.
func (r *Repository) SeedPolicies(ctx context.Context, tx pgx.Tx, workspaceID uuid.UUID) error {
	if r == nil {
		return fmt.Errorf("approval: repository is not configured")
	}
	if _, err := sqlcgen.New(tx).SeedPolicies(ctx, workspaceID); err != nil {
		return fmt.Errorf("approval: seed policies: %w", err)
	}
	return nil
}

// ActionKinds lists the action kinds that reach outside Bolu.
func (r *Repository) ActionKinds(ctx context.Context) ([]string, error) {
	if r == nil || r.pool == nil {
		return nil, fmt.Errorf("approval: database is not configured")
	}

	var kinds []string
	err := r.pool.InScopeRead(ctx, database.Scope{UserID: uuid.New()}, func(tx pgx.Tx) error {
		rows, err := sqlcgen.New(tx).ListExternalActionKinds(ctx)
		if err != nil {
			return fmt.Errorf("approval: list action kinds: %w", err)
		}
		kinds = rows
		return nil
	})
	if err != nil {
		return nil, err
	}

	return kinds, nil
}

// RecordAudit appends one entry to the audit trail.
func (r *Repository) RecordAudit(ctx context.Context, scope domain.Scope, entry domain.AuditEntry) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		metadata, err := json.Marshal(entry.Metadata)
		if err != nil {
			// An entry whose detail will not encode is still worth recording: the
			// fact that something happened is the load-bearing part.
			metadata = []byte("{}")
		}

		if _, err := queries.InsertAuditEntry(ctx, sqlcgen.InsertAuditEntryParams{
			WorkspaceID:  scope.WorkspaceID,
			ActorUserID:  optional(entry.ActorUserID),
			ActorAgentID: optional(entry.ActorAgentID),
			Action:       entry.Action,
			ObjectType:   entry.ObjectType,
			ObjectID:     entry.ObjectID,
			Metadata:     metadata,
		}); err != nil {
			return translateWriteError(err, "record audit")
		}
		return nil
	})
}

// AuditTrail returns the entries, newest first.
func (r *Repository) AuditTrail(ctx context.Context, scope domain.Scope, limit int) ([]domain.AuditRecord, error) {
	switch {
	case limit <= 0:
		limit = domain.DefaultListLimit
	case limit > domain.MaxListLimit:
		limit = domain.MaxListLimit
	}

	var records []domain.AuditRecord

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListAuditEntries(ctx, sqlcgen.ListAuditEntriesParams{
			WorkspaceID: scope.WorkspaceID,
			Limit:       int32(limit),
		})
		if err != nil {
			return fmt.Errorf("approval: list audit: %w", err)
		}

		records = make([]domain.AuditRecord, 0, len(rows))
		for _, row := range rows {
			records = append(records, toAuditRecord(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return records, nil
}

// distinguish tells apart the two reasons a status-guarded write can match
// nothing.
//
// The statement only touches a row in the caller's workspace and in the expected
// status, so no rows means either that the draft is not in that state or that it
// is not in that workspace at all. Row Level Security makes the second case
// invisible, and reporting it as "already decided" would tell a caller that
// somebody else's draft is theirs. The extra read only happens on this path.
func distinguish(ctx context.Context, queries *sqlcgen.Queries, scope domain.Scope, id uuid.UUID, otherwise error) error {
	if _, err := queries.GetDraft(ctx, sqlcgen.GetDraftParams{ID: id, WorkspaceID: scope.WorkspaceID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrDraftNotFound
		}
		return fmt.Errorf("approval: look up draft: %w", err)
	}
	return otherwise
}

func (r *Repository) read(ctx context.Context, scope domain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("approval: database is not configured")
	}
	return r.pool.InScopeRead(ctx, database.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}, func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

func (r *Repository) write(ctx context.Context, scope domain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("approval: database is not configured")
	}
	return r.pool.InScope(ctx, database.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}, func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

// ------------------------------------------------------------- conversions

// toDraft renders a stored row.
//
// The conversation is passed in rather than read from the row: a draft reaches
// its thread through the task that produced it, and the two reads that need it
// join the task while a write already knows it.
func toDraft(row sqlcgen.Draft, conversationID uuid.UUID) domain.Draft {
	return domain.Draft{
		ID:             row.ID,
		WorkspaceID:    row.WorkspaceID,
		TaskID:         row.TaskID,
		AgentID:        row.AgentID,
		ConversationID: conversationID,
		ActionKind:     row.ActionKind,
		Title:          row.Title,
		Payload:        row.Payload,
		Status:         row.Status,
		Revision:       int(row.Revision),
		DecidedBy:      deref(row.DecidedBy),
		DecidedAt:      row.DecidedAt,
		DecisionNote:   row.DecisionNote,
		NotBefore:      row.NotBefore,
		BlockedReason:  row.BlockedReason,
		Summary:        row.Summary,
		SentAt:         row.SentAt,
		CardMessageID:  deref(row.CardMessageID),
		IdempotencyKey: row.IdempotencyKey,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
	}
}

func toRevision(row sqlcgen.DraftRevision) domain.Revision {
	return domain.Revision{
		ID:        row.ID,
		DraftID:   row.DraftID,
		Revision:  int(row.Revision),
		Note:      row.Note,
		Payload:   row.Payload,
		DecidedBy: deref(row.DecidedBy),
		CreatedAt: row.CreatedAt,
	}
}

func toPolicy(row sqlcgen.ApprovalPolicy) domain.Policy {
	return domain.Policy{
		ActionKind: row.ActionKind,
		Mode:       row.Mode,
		MaxAmount:  row.MaxAmount,
		UpdatedBy:  deref(row.UpdatedBy),
		CreatedAt:  row.CreatedAt,
		UpdatedAt:  row.UpdatedAt,
	}
}

func toAuditRecord(row sqlcgen.AuditLog) domain.AuditRecord {
	var metadata map[string]any
	if err := json.Unmarshal(row.Metadata, &metadata); err != nil {
		metadata = map[string]any{}
	}

	return domain.AuditRecord{
		ID:           row.ID,
		ActorUserID:  deref(row.ActorUserID),
		ActorAgentID: deref(row.ActorAgentID),
		Action:       row.Action,
		ObjectType:   row.ObjectType,
		ObjectID:     row.ObjectID,
		Metadata:     metadata,
		CreatedAt:    row.CreatedAt,
	}
}

func optional(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func deref(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

func payloadOr(payload []byte) []byte {
	if len(payload) == 0 {
		return []byte("{}")
	}
	return payload
}

// translateWriteError turns the constraint violations the runtime can actually
// hit into the errors the service reports, and leaves the rest wrapped.
func translateWriteError(err error, action string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23503":
			// A foreign key failure means the row referenced is not in this
			// workspace, which RLS also refuses.
			return fmt.Errorf("%w: a referenced row is not in this workspace", domain.ErrInvalidInput)
		case "23514":
			// A check constraint: an unknown mode, or a hold without a reason.
			return fmt.Errorf("%w: a value is outside the allowed set", domain.ErrInvalidInput)
		}
	}
	return fmt.Errorf("approval: %s: %w", action, err)
}
