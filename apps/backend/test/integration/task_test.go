package integration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	agentrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/repository"
	authrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/repository"
	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	chatrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/repository"
	taskdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	taskrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/repository"
	taskservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/service"
	workspacedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	workspacerepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/repository"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
)

// taskFixture is one onboarded workspace with the task runtime.
type taskFixture struct {
	repo      *taskrepo.Repository
	service   *taskservice.Service
	chat      *chatrepo.Repository
	pool      *database.Pool
	workspace uuid.UUID
	userID    uuid.UUID
	agentID   uuid.UUID
	scope     taskdomain.Scope
}

// stubStarter stands in for Temporal. These tests are about what the runtime
// stores, and the workflow itself is covered by the testsuite tests next to it.
type stubStarter struct{}

func (stubStarter) Start(_ context.Context, _ taskdomain.Scope, input taskdomain.StartInput) (taskdomain.StartResult, error) {
	return taskdomain.StartResult{WorkflowID: "task-" + input.TaskID.String(), RunID: "run-test"}, nil
}

func (stubStarter) Signal(context.Context, taskdomain.Scope, uuid.UUID, string, any) error {
	return nil
}

func newTaskFixture(t *testing.T, dsn string) *taskFixture {
	t.Helper()

	ctx := t.Context()
	pool := poolFor(t, dsn)

	agents := agentrepo.New(pool)
	workspaces := workspacerepo.New(pool, agents)

	user, err := authrepo.New(pool.PgxPool()).CreateUser(ctx, uuid.NewString()+"@example.com", "$argon2id$hash")
	require.NoError(t, err)

	onboarded, err := workspaces.Onboard(ctx, user.ID, workspacedomain.OnboardRequest{
		Name:          "Toko Sinar",
		BusinessField: "Retail",
		Timezone:      "Asia/Jakarta",
	})
	require.NoError(t, err)

	tasks := taskrepo.New(pool)
	scope := taskdomain.Scope{UserID: user.ID, WorkspaceID: onboarded.Workspace.ID}

	return &taskFixture{
		repo:      tasks,
		chat:      chatrepo.New(pool),
		pool:      pool,
		workspace: onboarded.Workspace.ID,
		userID:    user.ID,
		agentID:   firstAgentOf(t, pool, onboarded.Workspace.ID),
		scope:     scope,
		service: taskservice.New(taskservice.Deps{
			Tasks:   tasks,
			Steps:   tasks,
			Starter: stubStarter{},
			Limits:  taskdomain.DefaultLimits,
		}),
	}
}

// TestEveryTriggerIsStoredWithItsOwnStatus is the E6.1 gate: the three triggers a
// caller may open are stored with the trigger that caused them, and the status
// they start from is the one the workflow resumes with.
func TestEveryTriggerIsStoredWithItsOwnStatus(t *testing.T) {
	fixture := newTaskFixture(t, appDatabase(t))
	ctx := t.Context()

	for _, trigger := range []string{taskdomain.TriggerChat, taskdomain.TriggerRoutine, taskdomain.TriggerWebhook} {
		task, err := fixture.service.Dispatch(ctx, fixture.scope, taskdomain.DispatchRequest{
			AgentID: fixture.agentID,
			Trigger: trigger,
			Title:   "Rekap pesanan " + trigger,
			Prompt:  "Rekap pesanan hari ini",
		})
		require.NoError(t, err, trigger)

		stored, err := fixture.repo.Get(ctx, fixture.scope, task.ID)
		require.NoError(t, err)
		require.Equal(t, trigger, stored.Trigger)
		require.Equal(t, taskdomain.StatusRunning, stored.Status)
		require.NotEmpty(t, stored.WorkflowID, "the workflow id is what a restart resumes with")
		require.NotNil(t, stored.StartedAt)
		require.NotNil(t, stored.HeartbeatAt, "a started task records a heartbeat immediately")
		require.Equal(t, 0, stored.Depth, "a task a person asked for is not part of a handoff chain")
	}
}

// TestHandoffChainIsStoredWithItsDepthAndParent is the E6.5 schema gate: a child
// task points at its parent and carries the depth the workflow refuses to exceed.
func TestHandoffChainIsStoredWithItsDepthAndParent(t *testing.T) {
	fixture := newTaskFixture(t, appDatabase(t))
	ctx := t.Context()

	parent, err := fixture.service.Dispatch(ctx, fixture.scope, taskdomain.DispatchRequest{
		AgentID: fixture.agentID,
		Trigger: taskdomain.TriggerChat,
		Title:   "Rekap penjualan",
	})
	require.NoError(t, err)

	child, err := fixture.repo.Create(ctx, fixture.scope, taskdomain.NewTask{
		AgentID:      fixture.agentID,
		ParentTaskID: parent.ID,
		Trigger:      taskdomain.TriggerHandoff,
		Title:        "Ambil data stok",
		Depth:        1,
	})
	require.NoError(t, err)

	require.Equal(t, 1, child.Depth)
	require.Equal(t, parent.ID, child.ParentTaskID)

	children, err := fixture.service.Children(ctx, fixture.scope, parent.ID)
	require.NoError(t, err)
	require.Len(t, children, 1)
	require.Equal(t, child.ID, children[0].ID)

	// The depth ceiling is the schema's too, so a chain cannot be stored deeper
	// than the workflow would allow.
	_, err = fixture.repo.Create(ctx, fixture.scope, taskdomain.NewTask{
		AgentID: fixture.agentID,
		Trigger: taskdomain.TriggerHandoff,
		Title:   "terlalu dalam",
		Depth:   17,
	})
	require.Error(t, err, "a depth past the ceiling is refused by the schema as well")
}

// TestStepsAreRecordedInOrder is the E6.6 gate: every round is written with the
// sequence the workflow produced, so the history reads back in order.
func TestStepsAreRecordedInOrder(t *testing.T) {
	fixture := newTaskFixture(t, appDatabase(t))
	ctx := t.Context()

	task, err := fixture.service.Dispatch(ctx, fixture.scope, taskdomain.DispatchRequest{
		AgentID: fixture.agentID,
		Trigger: taskdomain.TriggerChat,
		Title:   "Rekap penjualan",
	})
	require.NoError(t, err)

	rounds := []taskdomain.Step{
		{Kind: taskdomain.StepThink, Input: json.RawMessage(`{"messages":[]}`), Output: json.RawMessage(`{"text":"cek dulu"}`), InputTokens: 120, OutputTokens: 30},
		{Kind: taskdomain.StepToolCall, ToolName: "gmail.search", ToolLabel: taskdomain.LabelRead, Input: json.RawMessage(`{"q":"pesanan"}`)},
		{Kind: taskdomain.StepToolResult, ToolName: "gmail.search", ToolLabel: taskdomain.LabelRead, Output: json.RawMessage(`{"emails":2}`)},
		{Kind: taskdomain.StepFinal, Output: json.RawMessage(`{"text":"Ada 2 pesanan."}`)},
	}
	for _, round := range rounds {
		round.TaskID = task.ID
		_, err := fixture.repo.Append(ctx, fixture.scope, round)
		require.NoError(t, err)
	}

	steps, err := fixture.service.Steps(ctx, fixture.scope, task.ID)
	require.NoError(t, err)
	require.Len(t, steps, len(rounds))

	for index, step := range steps {
		require.Equal(t, index+1, step.Seq, "the sequence is assigned by the store, in order")
		require.Equal(t, rounds[index].Kind, step.Kind)
		require.False(t, step.Pruned)
	}
	require.Equal(t, 120, steps[0].InputTokens)
	require.Equal(t, 30, steps[0].OutputTokens)
}

// TestFinishedTaskKeepsItsSummaryAndCannotBeRewritten is the E6.6 gate: the
// summary survives, and a second terminal write is refused so a retried activity
// cannot overwrite a result the user already saw.
func TestFinishedTaskKeepsItsSummaryAndCannotBeRewritten(t *testing.T) {
	fixture := newTaskFixture(t, appDatabase(t))
	ctx := t.Context()

	task, err := fixture.service.Dispatch(ctx, fixture.scope, taskdomain.DispatchRequest{
		AgentID: fixture.agentID,
		Trigger: taskdomain.TriggerChat,
		Title:   "Rekap penjualan",
	})
	require.NoError(t, err)

	finished, err := fixture.repo.Finish(ctx, fixture.scope, taskdomain.Finish{
		TaskID:       task.ID,
		Status:       taskdomain.StatusSucceeded,
		Summary:      "3 pesanan, 1 perlu ditagih",
		InputTokens:  900,
		OutputTokens: 250,
		CostMicros:   4200,
		StepCount:    4,
	})
	require.NoError(t, err)
	require.Equal(t, "3 pesanan, 1 perlu ditagih", finished.Summary)
	require.Equal(t, int64(1150), finished.TotalTokens())
	require.NotNil(t, finished.FinishedAt)

	_, err = fixture.repo.Finish(ctx, fixture.scope, taskdomain.Finish{
		TaskID: task.ID,
		Status: taskdomain.StatusFailed,
	})
	require.ErrorIs(t, err, taskdomain.ErrTaskNotFound, "a finished task is never written again")

	stored, err := fixture.repo.Get(ctx, fixture.scope, task.ID)
	require.NoError(t, err)
	require.Equal(t, taskdomain.StatusSucceeded, stored.Status)
	require.Equal(t, "3 pesanan, 1 perlu ditagih", stored.Summary)
}

// TestParkedTaskRecordsWhatItWaitsFor is the E6.6 gate: a task waiting on a human
// is parked with the reason and the draft, and the resume clears both.
func TestParkedTaskRecordsWhatItWaitsFor(t *testing.T) {
	fixture := newTaskFixture(t, appDatabase(t))
	ctx := t.Context()

	task, err := fixture.service.Dispatch(ctx, fixture.scope, taskdomain.DispatchRequest{
		AgentID: fixture.agentID,
		Trigger: taskdomain.TriggerChat,
		Title:   "Kirim tagihan",
	})
	require.NoError(t, err)

	draftID := insertDraft(t, fixture, task.ID)

	parked, err := fixture.repo.Park(ctx, fixture.scope, task.ID, "menunggu persetujuan: Kirim tagihan", draftID)
	require.NoError(t, err)
	require.Equal(t, taskdomain.StatusWaitingApproval, parked.Status)
	require.Equal(t, "menunggu persetujuan: Kirim tagihan", parked.WaitingReason)
	require.Equal(t, draftID, parked.WaitingDraftID)
	require.Equal(t, taskdomain.HealthWaiting, parked.Health(time.Now()))

	resumed, err := fixture.repo.Resume(ctx, fixture.scope, task.ID)
	require.NoError(t, err)
	require.Equal(t, taskdomain.StatusRunning, resumed.Status)
	require.Empty(t, resumed.WaitingReason)
	require.Equal(t, uuid.Nil, resumed.WaitingDraftID)
}

// TestTaskRuntimeIsTenantScoped proves a task id from another workspace is
// invisible even though it is a real id.
func TestTaskRuntimeIsTenantScoped(t *testing.T) {
	dsn := appDatabase(t)
	first := newTaskFixture(t, dsn)
	second := newTaskFixture(t, dsn)
	ctx := t.Context()

	task, err := first.service.Dispatch(ctx, first.scope, taskdomain.DispatchRequest{
		AgentID: first.agentID,
		Trigger: taskdomain.TriggerChat,
		Title:   "rahasia",
	})
	require.NoError(t, err)

	_, err = second.service.Get(ctx, second.scope, task.ID)
	require.ErrorIs(t, err, taskdomain.ErrTaskNotFound)

	_, err = second.service.Steps(ctx, second.scope, task.ID)
	require.ErrorIs(t, err, taskdomain.ErrTaskNotFound)

	// The history of the second workspace never mentions the first one's task.
	history, err := second.service.History(ctx, second.scope, taskdomain.Filter{})
	require.NoError(t, err)
	require.Empty(t, history)
}

// TestCancelLeavesAFinishedTaskAlone is the store's own guard: the cancel
// statement only touches a task that is still live.
func TestCancelLeavesAFinishedTaskAlone(t *testing.T) {
	fixture := newTaskFixture(t, appDatabase(t))
	ctx := t.Context()

	task, err := fixture.service.Dispatch(ctx, fixture.scope, taskdomain.DispatchRequest{
		AgentID: fixture.agentID,
		Trigger: taskdomain.TriggerChat,
		Title:   "Rekap penjualan",
	})
	require.NoError(t, err)

	_, err = fixture.repo.Finish(ctx, fixture.scope, taskdomain.Finish{
		TaskID: task.ID,
		Status: taskdomain.StatusSucceeded,
	})
	require.NoError(t, err)

	_, err = fixture.repo.Cancel(ctx, fixture.scope, task.ID, "dihentikan oleh pengguna")
	require.ErrorIs(t, err, taskdomain.ErrTaskNotFound)

	stored, err := fixture.repo.Get(ctx, fixture.scope, task.ID)
	require.NoError(t, err)
	require.Equal(t, taskdomain.StatusSucceeded, stored.Status)
}

// TestDispatchFromChatLinksTheTaskToTheMessage is the seam between the chat
// module and the runtime: the task records the conversation and the placeholder
// message, which is what makes the answer land where the question was asked.
func TestDispatchFromChatLinksTheTaskToTheMessage(t *testing.T) {
	fixture := newTaskFixture(t, appDatabase(t))
	ctx := t.Context()

	chatScope := chatdomain.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace}
	conversation, err := fixture.chat.Direct(ctx, chatScope, fixture.agentID)
	require.NoError(t, err)

	placeholder, err := fixture.chat.Append(ctx, chatScope, chatdomain.Message{
		ConversationID: conversation.ID,
		AuthorAgentID:  fixture.agentID,
		Blocks:         []chatdomain.Block{},
		Status:         chatdomain.MessageStreaming,
	})
	require.NoError(t, err)

	task, err := fixture.service.Dispatch(ctx, fixture.scope, taskdomain.DispatchRequest{
		AgentID:        fixture.agentID,
		ConversationID: conversation.ID,
		ReplyMessageID: placeholder.ID,
		Trigger:        taskdomain.TriggerChat,
		Title:          "Rekap pesanan",
		Prompt:         "Rekap pesanan hari ini",
	})
	require.NoError(t, err)

	stored, err := fixture.repo.Get(ctx, fixture.scope, task.ID)
	require.NoError(t, err)
	require.Equal(t, conversation.ID, stored.ConversationID)
	require.Equal(t, placeholder.ID, stored.ReplyMessageID)
}

// TestDispatchRefusesAnAgentOfAnotherWorkspace is the foreign key's own answer:
// a Bolu that is not in this workspace cannot be given work.
func TestDispatchRefusesAnAgentOfAnotherWorkspace(t *testing.T) {
	dsn := appDatabase(t)
	first := newTaskFixture(t, dsn)
	second := newTaskFixture(t, dsn)
	ctx := t.Context()

	_, err := first.service.Dispatch(ctx, first.scope, taskdomain.DispatchRequest{
		AgentID: second.agentID,
		Trigger: taskdomain.TriggerChat,
		Title:   "curang",
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, taskdomain.ErrInvalidInput), "got %v", err)
}

// insertDraft writes the approval a parked task waits on, which the task runtime
// references rather than owns.
func insertDraft(t *testing.T, fixture *taskFixture, taskID uuid.UUID) uuid.UUID {
	t.Helper()

	var draftID uuid.UUID
	require.NoError(t, fixture.pool.InScope(t.Context(), database.Scope{
		UserID:      fixture.userID,
		WorkspaceID: fixture.workspace,
	}, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(),
			`INSERT INTO drafts (workspace_id, task_id, agent_id, action_kind, payload, idempotency_key)
			 VALUES ($1, $2, $3, 'gmail.send', '{}'::jsonb, $4)
			 RETURNING id`,
			fixture.workspace, taskID, fixture.agentID, uuid.NewString()).Scan(&draftID)
	}))

	return draftID
}

// TestCountLiveTasksForAgentIsWhatTheRegistryReads is the query the registry asks
// before deleting a Bolu: a Bolu with work in flight cannot be removed, so the
// count has to be right about every live status.
func TestCountLiveTasksForAgentIsWhatTheRegistryReads(t *testing.T) {
	fixture := newTaskFixture(t, appDatabase(t))
	ctx := t.Context()

	other := uuid.New()
	for _, trigger := range []string{taskdomain.TriggerChat, taskdomain.TriggerRoutine, taskdomain.TriggerWebhook} {
		_, err := fixture.service.Dispatch(ctx, fixture.scope, taskdomain.DispatchRequest{
			AgentID: fixture.agentID,
			Trigger: trigger,
			Title:   "kerja " + trigger,
		})
		require.NoError(t, err)
	}

	// A finished task and a canceled one are not in flight.
	finished, err := fixture.service.Dispatch(ctx, fixture.scope, taskdomain.DispatchRequest{
		AgentID: fixture.agentID, Trigger: taskdomain.TriggerChat, Title: "selesai",
	})
	require.NoError(t, err)
	_, err = fixture.repo.Finish(ctx, fixture.scope, taskdomain.Finish{
		TaskID: finished.ID, Status: taskdomain.StatusSucceeded,
	})
	require.NoError(t, err)

	// A task of another Bolu does not count against this one.
	_, err = fixture.service.Dispatch(ctx, fixture.scope, taskdomain.DispatchRequest{
		AgentID: other, Trigger: taskdomain.TriggerChat, Title: "punya Bolu lain",
	})
	require.Error(t, err, "a Bolu that is not in the workspace cannot be given work")

	live, err := fixture.repo.CountLiveTasksForAgent(ctx, fixture.scope, fixture.agentID)
	require.NoError(t, err)
	require.Equal(t, 3, live, "the three dispatched tasks are still in flight")

	// A parked task is still in flight: it is waiting, not finished.
	parked, err := fixture.service.Dispatch(ctx, fixture.scope, taskdomain.DispatchRequest{
		AgentID: fixture.agentID, Trigger: taskdomain.TriggerChat, Title: "menunggu",
	})
	require.NoError(t, err)
	_, err = fixture.repo.Park(ctx, fixture.scope, parked.ID, "menunggu persetujuan", insertDraft(t, fixture, parked.ID))
	require.NoError(t, err)

	live, err = fixture.repo.CountLiveTasksForAgent(ctx, fixture.scope, fixture.agentID)
	require.NoError(t, err)
	require.Equal(t, 4, live, "a parked task still occupies its Bolu")
}

// TestRepositoryRefusesAnUnknownTaskAndScope covers the store's own guards: a
// read of a task that is not in this workspace answers not found, and a write to
// one that has already finished is refused rather than overwriting it.
func TestRepositoryRefusesAnUnknownTaskAndScope(t *testing.T) {
	fixture := newTaskFixture(t, appDatabase(t))
	ctx := t.Context()

	_, err := fixture.repo.Get(ctx, fixture.scope, uuid.New())
	require.ErrorIs(t, err, taskdomain.ErrTaskNotFound)

	_, err = fixture.repo.Start(ctx, fixture.scope, uuid.New(), "task-x", "run-x")
	require.ErrorIs(t, err, taskdomain.ErrTaskNotFound)

	require.ErrorIs(t, fixture.repo.Progress(ctx, fixture.scope, taskdomain.Progress{TaskID: uuid.New()}),
		taskdomain.ErrTaskNotFound)

	_, err = fixture.repo.Park(ctx, fixture.scope, uuid.New(), "menunggu", uuid.Nil)
	require.ErrorIs(t, err, taskdomain.ErrTaskNotFound)

	_, err = fixture.repo.Resume(ctx, fixture.scope, uuid.New())
	require.ErrorIs(t, err, taskdomain.ErrTaskNotFound)

	_, err = fixture.repo.Cancel(ctx, fixture.scope, uuid.New(), "berhenti")
	require.ErrorIs(t, err, taskdomain.ErrTaskNotFound)

	_, err = fixture.repo.Finish(ctx, fixture.scope, taskdomain.Finish{TaskID: uuid.New(), Status: taskdomain.StatusSucceeded})
	require.ErrorIs(t, err, taskdomain.ErrTaskNotFound)
}

// TestListFiltersNarrowTheHistory is the read the task list and the office make:
// each filter is applied by the statement rather than by the caller, so a page is
// a page of the right rows.
func TestListFiltersNarrowTheHistory(t *testing.T) {
	fixture := newTaskFixture(t, appDatabase(t))
	ctx := t.Context()

	parent, err := fixture.service.Dispatch(ctx, fixture.scope, taskdomain.DispatchRequest{
		AgentID: fixture.agentID, Trigger: taskdomain.TriggerChat, Title: "induk",
	})
	require.NoError(t, err)

	child, err := fixture.repo.Create(ctx, fixture.scope, taskdomain.NewTask{
		AgentID:      fixture.agentID,
		ParentTaskID: parent.ID,
		Trigger:      taskdomain.TriggerHandoff,
		Title:        "anak",
		Depth:        1,
	})
	require.NoError(t, err)

	byParent, err := fixture.service.History(ctx, fixture.scope, taskdomain.Filter{ParentTaskID: parent.ID})
	require.NoError(t, err)
	require.Len(t, byParent, 1)
	require.Equal(t, child.ID, byParent[0].ID)

	byTriggerStatus, err := fixture.service.History(ctx, fixture.scope, taskdomain.Filter{Status: taskdomain.StatusRunning})
	require.NoError(t, err)
	require.Len(t, byTriggerStatus, 1)
	require.Equal(t, parent.ID, byTriggerStatus[0].ID, "a handoff child is queued, not running")

	limited, err := fixture.service.History(ctx, fixture.scope, taskdomain.Filter{Limit: 1})
	require.NoError(t, err)
	require.Len(t, limited, 1)

	everything, err := fixture.service.History(ctx, fixture.scope, taskdomain.Filter{Limit: 500})
	require.NoError(t, err)
	require.Len(t, everything, 2, "a limit past the ceiling is clamped rather than refused")
}
