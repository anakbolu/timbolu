package integration

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/container"
	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	taskdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	workspacedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	workspacerepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/repository"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/config"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
)

// EPIC 13's second gate is this file: **the raw contents of a task step have a clock on them
// and the task's shape does not**, an attachment leaves with its object, and nothing outside the
// window is touched.
//
// It runs through the real container, so what is under test is the job as it is deployed: the
// task repository's bulk update, the chat service's two-half attachment removal, and the
// container's adapters between them. A test that called the repository directly would prove the
// statements and not the wiring, and the wiring is where a table gets pruned by the wrong module
// or not at all.

// retentionFixture is one onboarded workspace with the retention job reachable through the
// container's own wiring, plus a second workspace that is deliberately *not* due.
type retentionFixture struct {
	app    *container.Container
	pool   *database.Pool
	config *config.Config

	workspace uuid.UUID
	userID    uuid.UUID
	scope     taskdomain.Scope

	// agentID is the Bolu of this workspace, which every step and message needs.
	agentID uuid.UUID
}

// retentionDatabase is the DSN every test in this file shares, migrated once.
func retentionDatabase(t *testing.T) string {
	t.Helper()

	return appDatabase(t)
}

func newRetentionFixture(t *testing.T, dsn string) *retentionFixture {
	t.Helper()

	ctx := t.Context()
	pool := poolFor(t, dsn)

	user, err := createVerifiedUser(t, pool)
	require.NoError(t, err)

	workspace, err := onboardWorkspace(t, pool, user)
	require.NoError(t, err)

	cfg := retentionConfig(t, dsn)

	app, err := container.New(ctx, cfg, container.WithLogger(zap.NewNop()), container.WithoutHTTP())
	require.NoError(t, err)
	t.Cleanup(app.Close)

	require.NotNil(t, app.Retention, "the job must be wired for this file to mean anything")
	require.NotNil(t, app.Repositories.Retention)

	return &retentionFixture{
		app:       app,
		pool:      pool,
		config:    cfg,
		workspace: workspace,
		userID:    user,
		scope:     taskdomain.Scope{UserID: user, WorkspaceID: workspace},
		agentID:   firstAgentOf(t, pool, workspace),
	}
}

// retentionConfig is a deployment with both windows named, so each is exercised by its own
// number rather than by a default nobody stated.
func retentionConfig(t *testing.T, dsn string) *config.Config {
	t.Helper()

	return &config.Config{
		Application: config.AppConfig{Environment: "test", FrontendURL: "https://app.example.com"},
		Http:        config.HttpConfig{Address: ":0", ApiPrefix: "/api"},
		Database:    config.DatabaseConfig{URL: dsn, MaxOpenConns: 5, MaxIdleConns: 1},
		JWT:         config.JWTConfig{Secret: "integration-test-secret", ExpireHours: 1},
		Auth: config.AuthConfig{
			AccessTokenMinutes: 15, RefreshHours: 24, VerificationHours: 24,
			ResetMinutes: 60, StateMinutes: 10, LoginThrottleCapacity: 100, LoginThrottleRefill: 100,
		},
		Mail:    config.MailConfig{Driver: "log"},
		Logging: config.LoggingConfig{Level: "error", Format: "json"},
		// The local driver, under a directory the test owns: the attachment half of this
		// file is about bytes actually leaving storage, which is what a fake would not prove.
		Storage: config.StorageConfig{Driver: "local", LocalRoot: t.TempDir()},
		Retention: config.RetentionConfig{
			StepDays:       30,
			AttachmentDays: 90,
			// The timer is off — the pass is something the test calls rather than
			// something that fires while it runs.
			Interval:   time.Hour,
			Batch:      500,
			SweepLimit: 100,
		},
	}
}

// openTaskWithSteps opens a task and records the steps of one round: a tool call that read
// something, and the answer. It returns the task and the two steps.
func (f *retentionFixture) openTaskWithSteps(t *testing.T) (taskdomain.Task, []taskdomain.Step) {
	t.Helper()

	ctx := t.Context()

	task, err := f.app.Services.Task.Dispatch(ctx, f.scope, taskdomain.DispatchRequest{
		AgentID: f.agentID,
		Trigger: taskdomain.TriggerChat,
		Title:   "Rekap pesanan hari ini",
		Prompt:  "Rekap pesanan hari ini dan kirim ke pemilik toko",
	})
	require.NoError(t, err)

	steps := make([]taskdomain.Step, 0, 2)
	for _, step := range []taskdomain.Step{
		{
			TaskID: task.ID,
			Kind:   taskdomain.StepThink,
			Input:  []byte(`{"prompt":"Rekap pesanan hari ini dan kirim ke pemilik toko"}`),
			Output: []byte(`{"text":"Saya akan membaca pesanan dulu."}`),
		},
		{
			TaskID:    task.ID,
			Kind:      taskdomain.StepToolCall,
			ToolName:  "whatsapp.list_messages",
			ToolLabel: taskdomain.LabelRead,
			Input:     []byte(`{"chat":"Penjualan"}`),
			Output:    []byte(`{"messages":[{"text":"pesan pelanggan yang sensitif"}]}`),
		},
	} {
		stored, err := f.app.Repositories.Task.Append(ctx, f.scope, step)
		require.NoError(t, err)
		steps = append(steps, stored)
	}

	// The task finishes, which is what writes the one-line summary: it is the record that has
	// to survive the prune, so it is written here rather than asserted from a constant.
	_, err = f.app.Repositories.Task.Finish(ctx, f.scope, taskdomain.Finish{
		TaskID:       task.ID,
		Status:       taskdomain.StatusSucceeded,
		Summary:      "Rekap pesanan hari ini dikirim ke pemilik toko.",
		InputTokens:  1200,
		OutputTokens: 340,
		CostMicros:   2100,
		StepCount:    len(steps),
	})
	require.NoError(t, err)

	task, err = f.app.Repositories.Task.Get(ctx, f.scope, task.ID)
	require.NoError(t, err)
	require.Equal(t, taskdomain.StatusSucceeded, task.Status)

	return task, steps
}

// ageStep moves a step's creation moment back, which is how a test produces a step that is
// older than the window without waiting thirty days.
func (f *retentionFixture) ageStep(t *testing.T, stepID uuid.UUID, age time.Duration) {
	t.Helper()

	require.NoError(t, f.pool.InScope(t.Context(),
		database.Scope{UserID: f.userID, WorkspaceID: f.workspace}, func(tx pgx.Tx) error {
			_, err := tx.Exec(t.Context(),
				"UPDATE task_steps SET created_at = now() - $1::interval WHERE id = $2 AND workspace_id = $3",
				age.String(), stepID, f.workspace)

			return err
		}))
}

// TestAnAgedStepLosesItsContentAndKeepsItsShape is the issue's own verification: a step older
// than the window comes out of the job with its payload gone and everything the task history is
// built from still there — and the task's own summary untouched.
func TestAnAgedStepLosesItsContentAndKeepsItsShape(t *testing.T) {
	fixture := newRetentionFixture(t, retentionDatabase(t))
	ctx := t.Context()

	task, steps := fixture.openTaskWithSteps(t)
	aged, fresh := steps[0], steps[1]
	fixture.ageStep(t, aged.ID, 40*24*time.Hour)

	result, err := fixture.app.Retention.Run(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, result.StepsPruned, 1, "at least the aged step of this workspace")
	require.NotZero(t, result.Workspaces)

	// The read a person makes: the shape of the task, step by step.
	stored, err := fixture.app.Repositories.Task.ListSteps(ctx, fixture.scope, task.ID)
	require.NoError(t, err)
	require.Len(t, stored, 2)

	pruned := stored[0]
	require.Equal(t, aged.Seq, pruned.Seq, "the sequence survives, so the order still reads")
	require.Equal(t, taskdomain.StepThink, pruned.Kind)
	require.Equal(t, aged.InputTokens, pruned.InputTokens)
	require.True(t, pruned.Pruned, "the step says its payload was removed rather than looking empty")
	// The column is NOT NULL, so an emptied payload is an empty object and `Pruned` is what
	// tells it apart from a step that genuinely carried nothing.
	require.JSONEq(t, `{}`, string(pruned.Input))
	require.JSONEq(t, `{}`, string(pruned.Output))
	require.NotContains(t, string(pruned.Input), "sensitif")

	// Nothing younger than the window is touched, payload and all.
	kept := stored[1]
	require.Equal(t, fresh.ID, kept.ID)
	require.False(t, kept.Pruned)
	require.Equal(t, fresh.Input, kept.Input)
	require.Equal(t, fresh.Output, kept.Output)
	require.JSONEq(t, `{"chat":"Penjualan"}`, string(kept.Input))

	// And the task itself — its prompt, its status, its tokens — is untouched, which is what
	// makes "retention" and "the history stays readable" one design.
	after, err := fixture.app.Repositories.Task.Get(ctx, fixture.scope, task.ID)
	require.NoError(t, err)
	require.Equal(t, task.Summary, after.Summary, "the summary is what a person reads after a prune")
	require.NotEmpty(t, after.Summary)
	require.Equal(t, task.Title, after.Title)
	require.Equal(t, task.Status, after.Status)
	require.Equal(t, task.StepCount, after.StepCount)
	require.Equal(t, task.InputTokens, after.InputTokens, "the report the cost was counted from survives")
	require.Equal(t, task.CostMicros, after.CostMicros)
}

// TestASecondPassOverTheSameWindowRemovesNothing: the job is idempotent, which is what makes it
// safe to re-run and safe on two instances at once.
func TestASecondPassOverTheSameWindowRemovesNothing(t *testing.T) {
	fixture := newRetentionFixture(t, retentionDatabase(t))
	ctx := t.Context()

	_, steps := fixture.openTaskWithSteps(t)
	fixture.ageStep(t, steps[0].ID, 40*24*time.Hour)

	first, err := fixture.app.Retention.Run(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, first.StepsPruned, 1)

	// The next interval arrives, which is what makes the same workspace due again — a second
	// pass over the same window must find nothing left to remove.
	fixture.dueNow(t)

	second, err := fixture.app.Retention.Run(ctx)
	require.NoError(t, err)
	require.NotZero(t, second.Workspaces, "the pass still ran")
	require.Zero(t, second.StepsPruned, "a step already emptied is not a candidate again")
	require.Zero(t, second.Attachments)

	// And again, because idempotent means it holds however many times it is run.
	fixture.dueNow(t)

	third, err := fixture.app.Retention.Run(ctx)
	require.NoError(t, err)
	require.Zero(t, third.StepsPruned)
}

// TestAWorkspaceThatIsNotDueIsNotTouched: the window is the policy, and the queue is what says
// whose turn it is. A workspace whose next pass is in the future keeps its content, which is the
// assertion behind "no user data is deleted outside the policy".
func TestAWorkspaceThatIsNotDueIsNotTouched(t *testing.T) {
	fixture := newRetentionFixture(t, retentionDatabase(t))
	ctx := t.Context()

	task, steps := fixture.openTaskWithSteps(t)
	fixture.ageStep(t, steps[0].ID, 40*24*time.Hour)

	// The workspace is pushed out of the queue the way the job itself would, so the test
	// exercises the same column the schedule rests on.
	require.NoError(t, fixture.pool.InScope(ctx,
		database.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace}, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				"UPDATE retention_targets SET next_prune_at = now() + interval '1 day' WHERE workspace_id = $1",
				fixture.workspace)

			return err
		}))

	result, err := fixture.app.Retention.Run(ctx)
	require.NoError(t, err)
	require.Zero(t, result.StepsPruned, "nothing was due anywhere in this deployment")

	// The aged step still holds its payload, because its workspace's turn has not come.
	stored, err := fixture.app.Repositories.Task.ListSteps(ctx, fixture.scope, task.ID)
	require.NoError(t, err)
	require.Len(t, stored, 2)
	require.False(t, stored[0].Pruned)
	require.NotEmpty(t, stored[0].Input)
}

// TestTheNextPassIsScheduledAndTheRunIsRecorded: a pass that never ran is a pass that is still
// due, and what each pass did is answerable from the database rather than from a log line that
// has already rotated.
func TestTheNextPassIsScheduledAndTheRunIsRecorded(t *testing.T) {
	fixture := newRetentionFixture(t, retentionDatabase(t))
	ctx := t.Context()

	_, steps := fixture.openTaskWithSteps(t)
	fixture.ageStep(t, steps[0].ID, 40*24*time.Hour)

	result, err := fixture.app.Retention.Run(ctx)
	require.NoError(t, err)
	require.NotZero(t, result.Workspaces)

	runs, err := fixture.app.Repositories.Retention.Runs(ctx, 5)
	require.NoError(t, err)
	require.NotEmpty(t, runs)

	newest := runs[0]
	require.True(t, newest.Done(), "the run closed")
	require.Empty(t, newest.Error)
	require.Equal(t, result.StepsPruned, newest.StepsPruned)

	// The queue moved forward, so the same workspace is not swept again on the next tick.
	var due bool
	require.NoError(t, fixture.pool.InScopeRead(ctx,
		database.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace}, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				"SELECT next_prune_at > now() FROM retention_targets WHERE workspace_id = $1",
				fixture.workspace).Scan(&due)
		}))
	require.True(t, due, "the workspace was scheduled for its next pass")
}

// TestAnAgedAttachmentLeavesWithItsRowAndItsObject: the bytes are the half a database cannot see,
// so the assertion is that storage no longer has them — a row removed without its object is a
// leak no query would ever find.
func TestAnAgedAttachmentLeavesWithItsRowAndItsObject(t *testing.T) {
	fixture := newRetentionFixture(t, retentionDatabase(t))
	ctx := t.Context()

	chatScope := chatdomain.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace}

	message := fixture.message(t)

	aged, err := fixture.app.Services.Chat.Attach(ctx, chatScope, chatdomain.AttachRequest{
		MessageID:   message.ID,
		Filename:    "nota-lama.pdf",
		ContentType: "application/pdf",
	}, []byte("isi nota yang harus hilang"))
	require.NoError(t, err)

	fresh, err := fixture.app.Services.Chat.Attach(ctx, chatScope, chatdomain.AttachRequest{
		MessageID:   message.ID,
		Filename:    "nota-baru.pdf",
		ContentType: "application/pdf",
	}, []byte("isi nota yang harus tinggal"))
	require.NoError(t, err)

	fixture.ageAttachment(t, aged.ID, 120*24*time.Hour)

	result, err := fixture.app.Retention.Run(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, result.Attachments, 1)
	require.GreaterOrEqual(t, result.BytesFreed, int64(len("isi nota yang harus hilang")))

	remaining, err := fixture.app.Repositories.Chat.ListForMessage(ctx, chatScope, message.ID)
	require.NoError(t, err)
	require.Len(t, remaining, 1, "the file inside the window stays")
	require.Equal(t, fresh.ID, remaining[0].ID)

	// The row is gone, so the metadata read fails...
	_, err = fixture.app.Repositories.Chat.Open(ctx, chatScope, aged.ID)
	require.Error(t, err)

	// ...and the bytes are gone from storage, which is the half a query cannot see.
	_, _, err = fixture.app.Services.Chat.AttachmentContent(ctx, chatScope, aged.ID)
	require.Error(t, err)
}

// TestAnAttachmentInsideTheWindowIsLeftAlone is the other half of the attachment policy: a
// deployment that sweeps must sweep by the window it named, not by everything it can see.
func TestAnAttachmentInsideTheWindowIsLeftAlone(t *testing.T) {
	fixture := newRetentionFixture(t, retentionDatabase(t))
	ctx := t.Context()

	chatScope := chatdomain.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace}

	message := fixture.message(t)

	kept, err := fixture.app.Services.Chat.Attach(ctx, chatScope, chatdomain.AttachRequest{
		MessageID:   message.ID,
		Filename:    "nota-bulan-ini.pdf",
		ContentType: "application/pdf",
	}, []byte("isi yang masih berlaku"))
	require.NoError(t, err)

	// Younger than ninety days, so the sweep has no business with it.
	fixture.ageAttachment(t, kept.ID, 10*24*time.Hour)

	_, err = fixture.app.Retention.Run(ctx)
	require.NoError(t, err)

	attachment, content, err := fixture.app.Services.Chat.AttachmentContent(ctx, chatScope, kept.ID)
	require.NoError(t, err)
	require.Equal(t, kept.ID, attachment.ID)
	require.Equal(t, []byte("isi yang masih berlaku"), content)
}

// message stores one message in the workspace's 1:1 thread with its first Bolu.
//
// An attachment points at a message, so this is what the attachment half of these tests needs
// before it can store anything — and it goes through the real repository rather than an insert,
// so the row it produces is the row the deployment produces.
func (f *retentionFixture) message(t *testing.T) chatdomain.Message {
	t.Helper()

	scope := chatdomain.Scope{UserID: f.userID, WorkspaceID: f.workspace}

	conversation, err := f.app.Repositories.Chat.Direct(t.Context(), scope, f.agentID)
	require.NoError(t, err)

	stored, err := f.app.Repositories.Chat.Append(t.Context(), scope, chatdomain.Message{
		ConversationID: conversation.ID,
		AuthorUserID:   f.userID,
		Status:         chatdomain.MessageComplete,
	})
	require.NoError(t, err)

	return stored
}

// dueNow puts the fixture's workspace back at the head of the queue, which is what the next
// interval does to it in a real deployment.
func (f *retentionFixture) dueNow(t *testing.T) {
	t.Helper()

	require.NoError(t, f.pool.InScope(t.Context(),
		database.Scope{UserID: f.userID, WorkspaceID: f.workspace}, func(tx pgx.Tx) error {
			_, err := tx.Exec(t.Context(),
				"UPDATE retention_targets SET next_prune_at = now() WHERE workspace_id = $1",
				f.workspace)

			return err
		}))
}

// ageAttachment moves an attachment's creation moment back.
func (f *retentionFixture) ageAttachment(t *testing.T, id uuid.UUID, age time.Duration) {
	t.Helper()

	require.NoError(t, f.pool.InScope(t.Context(),
		database.Scope{UserID: f.userID, WorkspaceID: f.workspace}, func(tx pgx.Tx) error {
			_, err := tx.Exec(t.Context(),
				"UPDATE message_attachments SET created_at = now() - $1::interval WHERE id = $2 AND workspace_id = $3",
				age.String(), id, f.workspace)

			return err
		}))
}

// TestANewWorkspaceIsQueuedByTheDatabase: a workspace that nobody remembers to queue is a
// workspace whose customer's words are kept forever, so the queue is a trigger rather than a
// caller's habit.
func TestANewWorkspaceIsQueuedByTheDatabase(t *testing.T) {
	fixture := newRetentionFixture(t, retentionDatabase(t))
	ctx := t.Context()

	other, err := createVerifiedUser(t, fixture.pool)
	require.NoError(t, err)

	workspaces := workspacerepo.New(fixture.pool)
	created, err := workspaces.Onboard(ctx, other, workspacedomain.OnboardRequest{
		Name:          "Toko Kedua",
		BusinessField: "Retail",
		Timezone:      "Asia/Jakarta",
	})
	require.NoError(t, err)

	var queued bool
	require.NoError(t, fixture.pool.InScopeRead(ctx,
		database.Scope{UserID: other, WorkspaceID: created.Workspace.ID}, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				"SELECT next_prune_at <= now() FROM retention_targets WHERE workspace_id = $1",
				created.Workspace.ID).Scan(&queued)
		}))
	require.True(t, queued, "a workspace created after the migration is due immediately")
}
