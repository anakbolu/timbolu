package integration

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	approvaldomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/approval/domain"
	approvalrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/approval/repository"
	approvalservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/approval/service"
	authrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/repository"
	taskdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	taskrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/repository"
	workspacedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	workspacerepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/repository"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
)

// approvalFixture is one onboarded workspace with the approval runtime.
type approvalFixture struct {
	repo        *approvalrepo.Repository
	service     *approvalservice.Service
	tasks       *taskrepo.Repository
	workspace   *workspacerepo.Repository
	pool        *database.Pool
	workspaceID uuid.UUID
	userID      uuid.UUID
	agentID     uuid.UUID
	taskID      uuid.UUID
	scope       approvaldomain.Scope
}

func newApprovalFixture(t *testing.T, dsn string) *approvalFixture {
	t.Helper()

	ctx := t.Context()
	pool := poolFor(t, dsn)

	workspaces := workspacerepo.New(pool)
	approvals := approvalrepo.New(pool)
	workspaces.SetPolicyProvisioner(approvals)

	user, err := authrepo.New(pool.PgxPool()).CreateUser(ctx, uuid.NewString()+"@example.com", "$argon2id$hash")
	require.NoError(t, err)

	onboarded, err := workspaces.Onboard(ctx, user.ID, workspacedomain.OnboardRequest{
		Name:          "Toko Sinar",
		BusinessField: "Retail",
		Timezone:      "Asia/Jakarta",
	})
	require.NoError(t, err)

	agentID := firstAgentOf(t, pool, onboarded.Workspace.ID)

	tasks := taskrepo.New(pool)
	taskScope := taskdomain.Scope{UserID: user.ID, WorkspaceID: onboarded.Workspace.ID}
	task, err := tasks.Create(ctx, taskScope, taskdomain.NewTask{
		AgentID: agentID,
		Trigger: taskdomain.TriggerChat,
		Title:   "Kirim tagihan Maret",
	})
	require.NoError(t, err)

	scope := approvaldomain.Scope{UserID: user.ID, WorkspaceID: onboarded.Workspace.ID}

	return &approvalFixture{
		repo:        approvals,
		tasks:       tasks,
		workspace:   workspaces,
		pool:        pool,
		workspaceID: onboarded.Workspace.ID,
		userID:      user.ID,
		agentID:     agentID,
		taskID:      task.ID,
		scope:       scope,
		service: approvalservice.New(approvalservice.Deps{
			Drafts: approvals,
		}),
	}
}

// TestOnboardingSeedsAConservativePolicyForEveryExternalAction is the E7.1 gate:
// a new workspace never exists without a policy row for every action kind that
// reaches outside it.
func TestOnboardingSeedsAConservativePolicyForEveryExternalAction(t *testing.T) {
	fixture := newApprovalFixture(t, appDatabase(t))
	ctx := t.Context()

	policies, err := fixture.service.Policies(ctx, fixture.scope)
	require.NoError(t, err)
	require.NotEmpty(t, policies, "a new workspace has policies")

	for _, policy := range policies {
		require.Equal(t, approvaldomain.ModeAlwaysDraft, policy.Mode, policy.ActionKind)
	}

	// Every external action kind is covered, which is what makes "this workspace
	// has no policy for sending email" a state that cannot exist.
	kinds, err := fixture.repo.ActionKinds(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, kinds)

	seen := map[string]bool{}
	for _, policy := range policies {
		seen[policy.ActionKind] = true
	}
	for _, kind := range kinds {
		require.True(t, seen[kind], "%s must have a policy", kind)
	}
}

// TestADraftWaitsAndIsDecidedOnce is the E7.1 verification: store a draft, wait,
// decide, and check the status and the history are intact.
func TestADraftWaitsAndIsDecidedOnce(t *testing.T) {
	fixture := newApprovalFixture(t, appDatabase(t))
	ctx := t.Context()

	draft, err := fixture.repo.CreateDraft(ctx, fixture.scope, approvaldomain.NewDraft{
		TaskID:         fixture.taskID,
		AgentID:        fixture.agentID,
		ActionKind:     "gmail.send",
		Title:          "Kirim tagihan Maret",
		Summary:        "Tagihan untuk pelanggan A",
		Payload:        json.RawMessage(`{"to":"a@b.c","subject":"Tagihan Maret"}`),
		IdempotencyKey: "smoke-1",
	})
	require.NoError(t, err)
	require.Equal(t, approvaldomain.StatusPending, draft.Status)
	require.True(t, draft.Waiting())

	// The stored draft reads back with its payload and its task.
	loaded, err := fixture.repo.GetDraft(ctx, fixture.scope, draft.ID)
	require.NoError(t, err)
	require.Equal(t, draft.ID, loaded.ID)
	require.Equal(t, fixture.taskID, loaded.TaskID)
	require.Equal(t, fixture.agentID, loaded.AgentID)
	require.Equal(t, "Kirim tagihan Maret", loaded.Title, "the card needs a title to show")
	require.Equal(t, "Tagihan untuk pelanggan A", loaded.Summary)
	require.JSONEq(t, `{"to":"a@b.c","subject":"Tagihan Maret"}`, string(loaded.Payload))

	// A decision is recorded once.
	approved, err := fixture.repo.Decide(ctx, fixture.scope, approvaldomain.Decision{
		DraftID:   draft.ID,
		Status:    approvaldomain.StatusApproved,
		Note:      "kirim",
		DecidedBy: fixture.userID,
	})
	require.NoError(t, err)
	require.Equal(t, approvaldomain.StatusApproved, approved.Status)
	require.Equal(t, fixture.userID, approved.DecidedBy)
	require.NotNil(t, approved.DecidedAt)

	// And a second decision changes nothing.
	_, err = fixture.repo.Decide(ctx, fixture.scope, approvaldomain.Decision{
		DraftID: draft.ID, Status: approvaldomain.StatusCanceled, DecidedBy: fixture.userID,
	})
	require.ErrorIs(t, err, approvaldomain.ErrAlreadyDecided)

	stored, err := fixture.repo.GetDraft(ctx, fixture.scope, draft.ID)
	require.NoError(t, err)
	require.Equal(t, approvaldomain.StatusApproved, stored.Status, "the first answer stands")
}

// TestARevisionIsRecordedAndCounted is the trail: the note and what the person was
// looking at are both kept.
func TestARevisionIsRecordedAndCounted(t *testing.T) {
	fixture := newApprovalFixture(t, appDatabase(t))
	ctx := t.Context()

	draft := createDraft(t, fixture, "smoke-rev", `{"to":"a@b.c"}`)

	revised, err := fixture.repo.Decide(ctx, fixture.scope, approvaldomain.Decision{
		DraftID: draft.ID, Status: approvaldomain.StatusRevise, Note: "salah penerima", DecidedBy: fixture.userID,
	})
	require.NoError(t, err)
	require.Equal(t, 1, revised.Revision)

	_, err = fixture.repo.AppendRevision(ctx, fixture.scope, approvaldomain.Revision{
		DraftID:   draft.ID,
		Revision:  revised.Revision,
		Note:      "salah penerima",
		Payload:   draft.Payload,
		DecidedBy: fixture.userID,
	})
	require.NoError(t, err)

	trail, err := fixture.repo.Revisions(ctx, fixture.scope, draft.ID)
	require.NoError(t, err)
	require.Len(t, trail, 1)
	require.Equal(t, "salah penerima", trail[0].Note)
	require.Equal(t, 1, trail[0].Revision)
	require.JSONEq(t, `{"to":"a@b.c"}`, string(trail[0].Payload))
}

// TestSendingIsRecordedWithItsTimestamp is the pair the schema enforces: a draft
// that has been sent says when, and one that has not says nothing.
func TestSendingIsRecordedWithItsTimestamp(t *testing.T) {
	fixture := newApprovalFixture(t, appDatabase(t))
	ctx := t.Context()

	draft := createDraft(t, fixture, "smoke-send", `{"to":"a@b.c"}`)

	// Sending before approval is refused, so nothing claims to have left.
	_, err := fixture.repo.MarkSent(ctx, fixture.scope, draft.ID)
	require.ErrorIs(t, err, approvaldomain.ErrNotApproved)

	_, err = fixture.repo.Decide(ctx, fixture.scope, approvaldomain.Decision{
		DraftID: draft.ID, Status: approvaldomain.StatusApproved, DecidedBy: fixture.userID,
	})
	require.NoError(t, err)

	sent, err := fixture.repo.MarkSent(ctx, fixture.scope, draft.ID)
	require.NoError(t, err)
	require.Equal(t, approvaldomain.StatusSent, sent.Status)
	require.NotNil(t, sent.SentAt)
	require.Nil(t, sent.NotBefore, "a sent draft is not held any more")

	// A second send is refused: the row is no longer approved.
	_, err = fixture.repo.MarkSent(ctx, fixture.scope, draft.ID)
	require.ErrorIs(t, err, approvaldomain.ErrNotApproved)
}

// TestAHoldRecordsWhyAndUntilWhen is the E7.4 storage: an approved action waiting
// for the working day says so, and the check constraint refuses a hold without a
// reason.
func TestAHoldRecordsWhyAndUntilWhen(t *testing.T) {
	fixture := newApprovalFixture(t, appDatabase(t))
	ctx := t.Context()

	draft := createDraft(t, fixture, "smoke-hold", `{"to":"a@b.c"}`)
	_, err := fixture.repo.Decide(ctx, fixture.scope, approvaldomain.Decision{
		DraftID: draft.ID, Status: approvaldomain.StatusApproved, DecidedBy: fixture.userID,
	})
	require.NoError(t, err)

	opensAt := time.Now().Add(10 * time.Hour).UTC().Truncate(time.Second)
	held, err := fixture.repo.Hold(ctx, fixture.scope, draft.ID, opensAt, "ditahan sampai jam kerja: 08:00")
	require.NoError(t, err)
	require.NotNil(t, held.NotBefore)
	require.WithinDuration(t, opensAt, *held.NotBefore, time.Second)
	require.Contains(t, held.BlockedReason, "jam kerja")
	require.True(t, held.Held(), "an approved draft with a future window is held")

	// The database refuses a hold that says nothing about why.
	err = fixture.pool.InScope(ctx, database.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspaceID}, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE drafts SET not_before = now() + interval '1 hour', blocked_reason = '' WHERE id = $1", draft.ID)
		return err
	})
	require.Error(t, err, "a held draft always says why")
}

// TestTheWorkingHoursAreReadAsConfigured is the E7.4 input: the window comes from
// the workspace's own settings and timezone.
func TestTheWorkingHoursAreReadAsConfigured(t *testing.T) {
	fixture := newApprovalFixture(t, appDatabase(t))
	ctx := t.Context()

	window, err := fixture.workspace.WorkingHours(ctx, fixture.workspaceID)
	require.NoError(t, err)
	require.Equal(t, "08:00", window.Start)
	require.Equal(t, "21:00", window.End)
	require.Equal(t, "Asia/Jakarta", window.Timezone)

	// Changing them recomputes the hold, because the rule reads them per call
	// rather than caching them when the draft was made.
	jakarta, err := time.LoadLocation("Asia/Jakarta")
	require.NoError(t, err)

	hours := approvaldomain.WorkingHours{Start: window.Start, End: window.End, Timezone: window.Timezone}

	night := time.Date(2026, 3, 2, 23, 0, 0, 0, jakarta)
	open, next := hours.Open(night)
	require.False(t, open, "23:00 is outside 08:00-21:00")
	require.Equal(t, 3, next.In(jakarta).Day())

	narrowed := approvaldomain.WorkingHours{Start: "09:00", End: "17:00", Timezone: window.Timezone}
	open, next = narrowed.Open(night)
	require.False(t, open)
	require.Equal(t, 9, next.In(jakarta).Hour(), "the narrowed window opens later")

	midday := time.Date(2026, 3, 2, 12, 0, 0, 0, jakarta)
	open, _ = narrowed.Open(midday)
	require.True(t, open)
}

// TestTheApprovalRuntimeIsTenantScoped proves a draft id from another workspace is
// invisible even though it is a real id.
func TestTheApprovalRuntimeIsTenantScoped(t *testing.T) {
	dsn := appDatabase(t)
	first := newApprovalFixture(t, dsn)
	second := newApprovalFixture(t, dsn)
	ctx := t.Context()

	draft := createDraft(t, first, "smoke-tenant", `{"to":"a@b.c"}`)

	_, err := second.repo.GetDraft(ctx, second.scope, draft.ID)
	require.ErrorIs(t, err, approvaldomain.ErrDraftNotFound)

	// A decision from the other workspace does not reach it.
	_, err = second.repo.Decide(ctx, second.scope, approvaldomain.Decision{
		DraftID: draft.ID, Status: approvaldomain.StatusApproved, DecidedBy: second.userID,
	})
	require.ErrorIs(t, err, approvaldomain.ErrDraftNotFound)

	// And the other workspace's list never mentions it.
	drafts, err := second.repo.ListDrafts(ctx, second.scope, approvaldomain.Filter{})
	require.NoError(t, err)
	for _, candidate := range drafts {
		require.NotEqual(t, draft.ID, candidate.ID)
	}

	// The first workspace's draft is untouched.
	stored, err := first.repo.GetDraft(ctx, first.scope, draft.ID)
	require.NoError(t, err)
	require.Equal(t, approvaldomain.StatusPending, stored.Status)
}

// TestADraftCannotReferenceAnotherWorkspacesTask is the statement's own guard: a
// foreign key check bypasses RLS, so the insert has to refuse a cross-tenant task
// itself.
func TestADraftCannotReferenceAnotherWorkspacesTask(t *testing.T) {
	dsn := appDatabase(t)
	first := newApprovalFixture(t, dsn)
	second := newApprovalFixture(t, dsn)
	ctx := t.Context()

	_, err := second.repo.CreateDraft(ctx, second.scope, approvaldomain.NewDraft{
		TaskID:         first.taskID,
		AgentID:        first.agentID,
		ActionKind:     "gmail.send",
		Title:          "curang",
		IdempotencyKey: "cross-tenant",
	})
	require.ErrorIs(t, err, approvaldomain.ErrInvalidInput)
}

// TestTheAuditTrailIsComplete is the E7.6 gate: every step of a draft's life is
// attributable, and a send can be traced to its draft, its task, and the person
// who allowed it.
func TestTheAuditTrailIsComplete(t *testing.T) {
	fixture := newApprovalFixture(t, appDatabase(t))
	ctx := t.Context()

	draft := createDraft(t, fixture, "smoke-audit", `{"to":"a@b.c"}`)

	for _, entry := range []approvaldomain.AuditEntry{
		{Action: "draft.created", ObjectType: "draft", ObjectID: draft.ID.String(), ActorAgentID: fixture.agentID,
			Metadata: map[string]any{"action_kind": "gmail.send", "task_id": fixture.taskID.String()}},
		{Action: "draft.approved", ObjectType: "draft", ObjectID: draft.ID.String(),
			ActorUserID: fixture.userID, ActorAgentID: fixture.agentID,
			Metadata: map[string]any{"note": "kirim"}},
	} {
		require.NoError(t, fixture.repo.RecordAudit(ctx, fixture.scope, entry))
	}

	require.NoError(t, fixture.repo.RecordAudit(ctx, fixture.scope, approvaldomain.AuditEntry{
		Action:       "draft.sent",
		ObjectType:   "draft",
		ObjectID:     draft.ID.String(),
		ActorUserID:  fixture.userID,
		ActorAgentID: fixture.agentID,
		Metadata: map[string]any{
			"action_kind":     "gmail.send",
			"task_id":         fixture.taskID.String(),
			"idempotency_key": draft.IdempotencyKey,
			"approved_by":     fixture.userID.String(),
		},
	}))

	records, err := fixture.repo.AuditTrail(ctx, fixture.scope, 10)
	require.NoError(t, err)
	require.Len(t, records, 3)

	// Newest first, so the send is the first row.
	require.Equal(t, "draft.sent", records[0].Action)
	require.Equal(t, "draft", records[0].ObjectType)
	require.Equal(t, draft.ID.String(), records[0].ObjectID)
	require.Equal(t, fixture.userID, records[0].ActorUserID)
	require.Equal(t, fixture.taskID.String(), records[0].Metadata["task_id"])
	require.Equal(t, fixture.userID.String(), records[0].Metadata["approved_by"])
	require.NotZero(t, records[0].CreatedAt)

	// The creation names the Bolu and no person, because none acted.
	created := records[2]
	require.Equal(t, "draft.created", created.Action)
	require.Equal(t, fixture.agentID, created.ActorAgentID)
	require.Equal(t, uuid.Nil, created.ActorUserID)
}

// TestThePolicyScreenReportsTheDefaultForEveryKind is the screen's read: a kind
// the workspace never decided on is reported as the conservative default.
func TestThePolicyScreenReportsTheDefaultForEveryKind(t *testing.T) {
	fixture := newApprovalFixture(t, appDatabase(t))
	ctx := t.Context()

	policies, err := fixture.service.Policies(ctx, fixture.scope)
	require.NoError(t, err)
	require.NotEmpty(t, policies)

	ceiling := int64(250_000)
	updated, err := fixture.service.SetPolicy(ctx, fixture.scope, approvaldomain.PolicyRequest{
		ActionKind: "gmail.send", Mode: approvaldomain.ModeAuto, MaxAmount: &ceiling,
	})
	require.NoError(t, err)
	require.Equal(t, approvaldomain.ModeAuto, updated.Mode)
	require.NotNil(t, updated.MaxAmount)
	require.Equal(t, ceiling, *updated.MaxAmount)
	require.Equal(t, fixture.userID, updated.UpdatedBy)

	// The change survives a re-read and applies to that kind alone.
	again, err := fixture.service.Policies(ctx, fixture.scope)
	require.NoError(t, err)

	byKind := map[string]approvaldomain.Policy{}
	for _, policy := range again {
		byKind[policy.ActionKind] = policy
	}
	require.Equal(t, approvaldomain.ModeAuto, byKind["gmail.send"].Mode)
	require.Equal(t, approvaldomain.ModeAlwaysDraft, byKind["whatsapp.send_message"].Mode)
}

// TestTheDraftListFilters covers the read the dashboard makes.
func TestTheDraftListFilters(t *testing.T) {
	fixture := newApprovalFixture(t, appDatabase(t))
	ctx := t.Context()

	sending := createDraft(t, fixture, "smoke-list-1", `{"to":"a@b.c"}`)
	createDraft(t, fixture, "smoke-list-2", `{"to":"z@b.c"}`)

	_, err := fixture.repo.Decide(ctx, fixture.scope, approvaldomain.Decision{
		DraftID: sending.ID, Status: approvaldomain.StatusApproved, DecidedBy: fixture.userID,
	})
	require.NoError(t, err)

	waiting, err := fixture.service.Drafts(ctx, fixture.scope, approvaldomain.Filter{Status: approvaldomain.StatusPending})
	require.NoError(t, err)
	require.Len(t, waiting, 1)

	approved, err := fixture.service.Drafts(ctx, fixture.scope, approvaldomain.Filter{Status: approvaldomain.StatusApproved})
	require.NoError(t, err)
	require.Len(t, approved, 1)
	require.Equal(t, sending.ID, approved[0].ID)

	byAgent, err := fixture.service.Drafts(ctx, fixture.scope, approvaldomain.Filter{AgentID: fixture.agentID})
	require.NoError(t, err)
	require.Len(t, byAgent, 2)

	byTask, err := fixture.service.Drafts(ctx, fixture.scope, approvaldomain.Filter{TaskID: fixture.taskID})
	require.NoError(t, err)
	require.Len(t, byTask, 2)
}

// createDraft stores one pending proposal.
func createDraft(t *testing.T, fixture *approvalFixture, key, payload string) approvaldomain.Draft {
	t.Helper()

	draft, err := fixture.repo.CreateDraft(t.Context(), fixture.scope, approvaldomain.NewDraft{
		TaskID:         fixture.taskID,
		AgentID:        fixture.agentID,
		ActionKind:     "gmail.send",
		Title:          "Kirim tagihan Maret",
		Summary:        "Tagihan untuk pelanggan A",
		Payload:        json.RawMessage(payload),
		IdempotencyKey: key,
	})
	require.NoError(t, err)
	return draft
}
