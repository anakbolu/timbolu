package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
)

// The service tests cover the three triggers the epic names — chat, routine, and
// webhook — and the rules around them: a trigger a client may not claim, a Bolu
// that is resting, a task that nothing can run, and a cancel that must not
// overwrite a finished task.

func newService(t *testing.T, starter domain.Starter, agents domain.AgentDirectory, events domain.EventSink) (*Service, *memoryTasks, domain.Scope) {
	t.Helper()

	tasks := newMemoryTasks()
	service := New(Deps{
		Tasks:   tasks,
		Steps:   tasks,
		Starter: starter,
		Agents:  agents,
		Events:  events,
		Limits:  domain.DefaultLimits,
	})

	return service, tasks, domain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}
}

// TestDispatchStoresEveryTrigger is the E6.1 gate: a task opened from chat, from
// a routine, and from a webhook is stored with the trigger that caused it and
// the status the workflow starts from.
func TestDispatchStoresEveryTrigger(t *testing.T) {
	for _, trigger := range []string{domain.TriggerChat, domain.TriggerRoutine, domain.TriggerWebhook} {
		t.Run(trigger, func(t *testing.T) {
			starter := &recordingStarter{}
			service, _, scope := newService(t, starter, nil, nil)

			task, err := service.Dispatch(context.Background(), scope, domain.DispatchRequest{
				AgentID: uuid.New(),
				Trigger: trigger,
				Title:   "Rekap pesanan",
				Prompt:  "Rekap pesanan hari ini",
			})
			require.NoError(t, err)

			require.Equal(t, trigger, task.Trigger)
			require.Equal(t, domain.StatusRunning, task.Status, "a started task is running, not queued")
			require.NotEmpty(t, task.WorkflowID, "a running task always records the workflow that owns it")
			require.Len(t, starter.started, 1)
			require.Equal(t, task.ID, starter.started[0].TaskID)
		})
	}
}

// TestDispatchTakesTheTitleFromThePrompt covers the fallback: a caller that names
// no title still gets a readable one, which is what the feed shows.
func TestDispatchTakesTheTitleFromThePrompt(t *testing.T) {
	service, _, scope := newService(t, &recordingStarter{}, nil, nil)

	task, err := service.Dispatch(context.Background(), scope, domain.DispatchRequest{
		AgentID: uuid.New(),
		Trigger: domain.TriggerChat,
		Prompt:  "Rekap pesanan hari ini\nlalu kirim ke Oren",
	})
	require.NoError(t, err)

	require.Equal(t, "Rekap pesanan hari ini", task.Title)
}

// TestDispatchRefusesATriggerAClientMayNotClaim is the guard the epic asks for: a
// chain of handoffs is opened by the runtime, so a caller cannot claim that
// trigger and make the chain look like something a person asked for.
func TestDispatchRefusesATriggerAClientMayNotClaim(t *testing.T) {
	service, _, scope := newService(t, &recordingStarter{}, nil, nil)

	_, err := service.Dispatch(context.Background(), scope, domain.DispatchRequest{
		AgentID: uuid.New(),
		Trigger: domain.TriggerHandoff,
		Title:   "curang",
	})
	require.ErrorIs(t, err, domain.ErrUnknownTrigger)

	for _, trigger := range TriggerWhitelist {
		require.NotEqual(t, domain.TriggerHandoff, trigger, "a client must never be able to claim a handoff")
	}
}

// TestDispatchRefusesARestingBolu covers the registry's rule at the runtime's
// door: a Bolu that is resting takes no new work, and a task that started anyway
// would contradict the switch the user set.
func TestDispatchRefusesARestingBolu(t *testing.T) {
	agents := fakeAgents{agent: domain.AgentRef{ID: uuid.New(), Name: "Ijo"}, resting: true}
	service, _, scope := newService(t, &recordingStarter{}, agents, nil)

	_, err := service.Dispatch(context.Background(), scope, domain.DispatchRequest{
		AgentID: agents.agent.ID,
		Trigger: domain.TriggerChat,
		Title:   "apa saja",
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
	require.Contains(t, err.Error(), "Ijo")
}

// TestDispatchMarksATaskNothingWillRun covers the honest failure: a task whose
// workflow could not start is finished as failed rather than left queued looking
// pending forever.
func TestDispatchMarksATaskNothingWillRun(t *testing.T) {
	starter := &recordingStarter{fail: errors.New("temporal is unreachable")}
	service, _, scope := newService(t, starter, nil, nil)

	_, err := service.Dispatch(context.Background(), scope, domain.DispatchRequest{
		AgentID: uuid.New(),
		Trigger: domain.TriggerChat,
		Title:   "apa saja",
	})
	require.Error(t, err)

	history, err := service.History(context.Background(), scope, domain.Filter{})
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.Equal(t, domain.StatusFailed, history[0].Status)
	require.Contains(t, history[0].StoppedReason, "tidak bisa dijalankan")
}

// TestDispatchWithoutAnEngineLeavesTheTaskQueued covers the deployment that runs
// the API without a worker: the task is recorded and left queued, which is the
// honest state rather than a task reported as started.
func TestDispatchWithoutAnEngineLeavesTheTaskQueued(t *testing.T) {
	service, _, scope := newService(t, nil, nil, nil)

	task, err := service.Dispatch(context.Background(), scope, domain.DispatchRequest{
		AgentID: uuid.New(),
		Trigger: domain.TriggerWebhook,
		Title:   "pesanan baru",
	})
	require.NoError(t, err)

	require.Equal(t, domain.StatusQueued, task.Status)
	require.Empty(t, task.WorkflowID)
}

// TestHistoryFiltersByStatusAndAgent covers the history endpoint's filters, which
// is what the office and the task list read.
func TestHistoryFiltersByStatusAndAgent(t *testing.T) {
	service, tasks, scope := newService(t, &recordingStarter{}, nil, nil)
	agentA, agentB := uuid.New(), uuid.New()

	first, err := service.Dispatch(context.Background(), scope, domain.DispatchRequest{
		AgentID: agentA, Trigger: domain.TriggerChat, Title: "satu",
	})
	require.NoError(t, err)
	_, err = service.Dispatch(context.Background(), scope, domain.DispatchRequest{
		AgentID: agentB, Trigger: domain.TriggerChat, Title: "dua",
	})
	require.NoError(t, err)

	byAgent, err := service.History(context.Background(), scope, domain.Filter{AgentID: agentA})
	require.NoError(t, err)
	require.Len(t, byAgent, 1)
	require.Equal(t, first.ID, byAgent[0].ID)

	_, err = tasks.Finish(context.Background(), scope, domain.Finish{TaskID: first.ID, Status: domain.StatusSucceeded})
	require.NoError(t, err)

	live, err := service.History(context.Background(), scope, domain.Filter{Live: true})
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.Equal(t, agentB, live[0].AgentID)

	require.ErrorIs(t, func() error {
		_, err := service.History(context.Background(), scope, domain.Filter{Status: "selesai"})
		return err
	}(), domain.ErrInvalidInput, "an unknown status is refused rather than silently ignored")
}

// TestStepsAreRefusedForATaskOfAnotherWorkspace is the tenant guard: a task id
// from another workspace answers not found rather than an empty list.
func TestStepsAreRefusedForATaskOfAnotherWorkspace(t *testing.T) {
	service, _, scope := newService(t, &recordingStarter{}, nil, nil)

	task, err := service.Dispatch(context.Background(), scope, domain.DispatchRequest{
		AgentID: uuid.New(), Trigger: domain.TriggerChat, Title: "satu",
	})
	require.NoError(t, err)

	other := domain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}
	_, err = service.Steps(context.Background(), other, task.ID)
	require.ErrorIs(t, err, domain.ErrTaskNotFound)

	steps, err := service.Steps(context.Background(), scope, task.ID)
	require.NoError(t, err)
	require.Empty(t, steps)
}

// TestCancelStopsATaskAndSignalsTheWorkflow covers the cancel path: the status is
// written first and the running workflow is told second, so a worker that is down
// cannot leave a task running.
func TestCancelStopsATaskAndSignalsTheWorkflow(t *testing.T) {
	starter := &recordingStarter{}
	events := &countingEvents{}
	service, _, scope := newService(t, starter, nil, events)

	task, err := service.Dispatch(context.Background(), scope, domain.DispatchRequest{
		AgentID: uuid.New(), Trigger: domain.TriggerChat, Title: "satu",
	})
	require.NoError(t, err)

	canceled, err := service.Cancel(context.Background(), scope, task.ID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusCanceled, canceled.Status)
	require.Equal(t, "dihentikan oleh pengguna", canceled.StoppedReason)
	require.Len(t, starter.signals, 1, "the running workflow is told as well as the store")

	require.Contains(t, events.types, EventTaskStarted)
	require.Contains(t, events.types, EventTaskFinished)
}

// TestCancelRefusesAFinishedTask is the guard against rewriting history: a task
// the user already saw finish is not canceled afterwards.
func TestCancelRefusesAFinishedTask(t *testing.T) {
	service, tasks, scope := newService(t, &recordingStarter{}, nil, nil)

	task, err := service.Dispatch(context.Background(), scope, domain.DispatchRequest{
		AgentID: uuid.New(), Trigger: domain.TriggerChat, Title: "satu",
	})
	require.NoError(t, err)
	_, err = tasks.Finish(context.Background(), scope, domain.Finish{
		TaskID: task.ID, Status: domain.StatusSucceeded, Summary: "selesai",
	})
	require.NoError(t, err)

	_, err = service.Cancel(context.Background(), scope, task.ID)
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	stored, err := service.Get(context.Background(), scope, task.ID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusSucceeded, stored.Status)
}

// TestChildrenReturnsTheHandoffChain covers the read the office uses to follow a
// handoff from its root.
func TestChildrenReturnsTheHandoffChain(t *testing.T) {
	service, tasks, scope := newService(t, &recordingStarter{}, nil, nil)

	parent, err := service.Dispatch(context.Background(), scope, domain.DispatchRequest{
		AgentID: uuid.New(), Trigger: domain.TriggerChat, Title: "induk",
	})
	require.NoError(t, err)

	child, err := tasks.Create(context.Background(), scope, domain.NewTask{
		AgentID:      uuid.New(),
		ParentTaskID: parent.ID,
		Trigger:      domain.TriggerHandoff,
		Title:        "anak",
		Depth:        1,
	})
	require.NoError(t, err)

	children, err := service.Children(context.Background(), scope, parent.ID)
	require.NoError(t, err)
	require.Len(t, children, 1)
	require.Equal(t, child.ID, children[0].ID)
}

// TestHealthSeparatesWorkingFromStuck is the derived status the office shows: a
// task whose worker died reads `running` in the store, and only the heartbeat
// tells the two apart.
func TestHealthSeparatesWorkingFromStuck(t *testing.T) {
	service, tasks, scope := newService(t, &recordingStarter{}, nil, nil)

	task, err := service.Dispatch(context.Background(), scope, domain.DispatchRequest{
		AgentID: uuid.New(), Trigger: domain.TriggerChat, Title: "satu",
	})
	require.NoError(t, err)

	fresh, err := service.Get(context.Background(), scope, task.ID)
	require.NoError(t, err)
	require.Equal(t, domain.HealthRunning, fresh.Health(time.Now()))

	// The heartbeat is moved into the past, which is what a worker that died
	// leaves behind.
	stale := time.Now().Add(-10 * time.Minute)
	stored := tasks.tasks[task.ID]
	stored.HeartbeatAt = &stale
	tasks.tasks[task.ID] = stored

	again, err := service.Get(context.Background(), scope, task.ID)
	require.NoError(t, err)
	require.Equal(t, domain.HealthStuck, again.Health(time.Now()))
}

// TestTriggerWhitelistExcludesHandoff is the one trigger a client may not claim,
// stated as a test of the list rather than only of a call: a future edit that
// adds it would make the chain look like something a person asked for.
func TestTriggerWhitelistExcludesHandoff(t *testing.T) {
	require.NotContains(t, TriggerWhitelist, domain.TriggerHandoff)

	for _, trigger := range TriggerWhitelist {
		service, _, scope := newService(t, &recordingStarter{}, nil, nil)
		_, err := service.Dispatch(context.Background(), scope, domain.DispatchRequest{
			AgentID: uuid.New(), Trigger: trigger, Title: "satu",
		})
		require.NoError(t, err, trigger)
	}
}

// TestDispatchRefusesAnEmptyScope is the tenant guard at the service's door: a
// task cannot be opened without a workspace, which is what keeps every later read
// scoped.
func TestDispatchRefusesAnEmptyScope(t *testing.T) {
	service, _, _ := newService(t, &recordingStarter{}, nil, nil)

	_, err := service.Dispatch(context.Background(), domain.Scope{}, domain.DispatchRequest{
		AgentID: uuid.New(), Trigger: domain.TriggerChat, Title: "satu",
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
	require.Contains(t, err.Error(), "workspace")
}

// TestDispatchRefusesAnOversizedTitle covers the bound the feed depends on: a
// title is one line, and a task whose title is a whole document would be
// unreadable in the list.
func TestDispatchRefusesAnOversizedTitle(t *testing.T) {
	service, _, scope := newService(t, &recordingStarter{}, nil, nil)

	_, err := service.Dispatch(context.Background(), scope, domain.DispatchRequest{
		AgentID: uuid.New(),
		Trigger: domain.TriggerChat,
		Title:   strings.Repeat("a", TitleMaxRunes+1),
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
	require.Contains(t, err.Error(), "longer than")
}

// TestDispatchRefusesADepthPastTheCeiling is the second guard on the handoff
// chain: the workflow refuses a deep handoff, and the service refuses a task
// created at one.
func TestDispatchRefusesADepthPastTheCeiling(t *testing.T) {
	service, _, scope := newService(t, &recordingStarter{}, nil, nil)

	_, err := service.Dispatch(context.Background(), scope, domain.DispatchRequest{
		AgentID: uuid.New(),
		Trigger: domain.TriggerChat,
		Title:   "terlalu dalam",
		Depth:   domain.DefaultLimits.MaxHandoffDepth + 1,
	})
	require.ErrorIs(t, err, domain.ErrDepthLimitReached)

	// The ceiling itself is allowed: a chain that reaches it is refused at the
	// next handoff, not at its own creation.
	_, err = service.Dispatch(context.Background(), scope, domain.DispatchRequest{
		AgentID: uuid.New(),
		Trigger: domain.TriggerChat,
		Title:   "di batas",
		Depth:   domain.DefaultLimits.MaxHandoffDepth,
	})
	require.NoError(t, err)
}

// TestDispatchRefusesAMissingAgent covers the cheapest validation: a request
// without a Bolu never reaches the store.
func TestDispatchRefusesAMissingAgent(t *testing.T) {
	service, tasks, scope := newService(t, &recordingStarter{}, nil, nil)

	_, err := service.Dispatch(context.Background(), scope, domain.DispatchRequest{
		Trigger: domain.TriggerChat, Title: "satu",
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	history, err := service.History(context.Background(), scope, domain.Filter{})
	require.NoError(t, err)
	require.Empty(t, history, "nothing was written for a request that never had a Bolu")
	require.Empty(t, tasks.tasks)
}

// TestGetRefusesAnEmptyScopeAndId covers the two reads the service refuses
// without touching the store.
func TestGetRefusesAnEmptyScopeAndId(t *testing.T) {
	service, _, scope := newService(t, &recordingStarter{}, nil, nil)

	_, err := service.Get(context.Background(), domain.Scope{}, uuid.New())
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = service.Get(context.Background(), scope, uuid.Nil)
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = service.Steps(context.Background(), domain.Scope{}, uuid.New())
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = service.Children(context.Background(), domain.Scope{}, uuid.New())
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = service.Cancel(context.Background(), domain.Scope{}, uuid.New())
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = service.History(context.Background(), domain.Scope{}, domain.Filter{})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestCancelRefusesAnUnknownTask is the honest read: canceling a task that is not
// in this workspace answers not found rather than creating one.
func TestCancelRefusesAnUnknownTask(t *testing.T) {
	service, _, scope := newService(t, &recordingStarter{}, nil, nil)

	_, err := service.Cancel(context.Background(), scope, uuid.New())
	require.ErrorIs(t, err, domain.ErrTaskNotFound)
}

// TestHistoryRefusesAnUnknownStatus is the query guard: a status the schema does
// not have is refused rather than silently returning everything.
func TestHistoryRefusesAnUnknownStatus(t *testing.T) {
	service, _, scope := newService(t, &recordingStarter{}, nil, nil)

	for _, status := range []string{domain.StatusQueued, domain.StatusRunning, domain.StatusWaitingApproval,
		domain.StatusSucceeded, domain.StatusFailed, domain.StatusCanceled} {
		_, err := service.History(context.Background(), scope, domain.Filter{Status: status})
		require.NoError(t, err, status)
	}

	_, err := service.History(context.Background(), scope, domain.Filter{Status: "selesai"})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestDispatchSignalsTheEngineWithTheTasksOwnIdentity covers what the workflow is
// started with: every field is an id the runtime re-reads its state from, so a
// restart resumes rather than replaying a snapshot.
func TestDispatchSignalsTheEngineWithTheTasksOwnIdentity(t *testing.T) {
	starter := &recordingStarter{}
	service, _, scope := newService(t, starter, nil, nil)

	conversationID := uuid.New()
	messageID := uuid.New()

	task, err := service.Dispatch(context.Background(), scope, domain.DispatchRequest{
		AgentID:        uuid.New(),
		ConversationID: conversationID,
		ReplyMessageID: messageID,
		Trigger:        domain.TriggerChat,
		Title:          "Rekap pesanan",
		Prompt:         "Rekap pesanan hari ini",
	})
	require.NoError(t, err)

	require.Len(t, starter.started, 1)
	started := starter.started[0]
	require.Equal(t, task.ID, started.TaskID)
	require.Equal(t, conversationID, started.ConversationID)
	require.Equal(t, messageID, started.ReplyMessageID,
		"the task fills the placeholder the chat module wrote")
	require.Equal(t, "Rekap pesanan hari ini", started.Prompt)
	require.Equal(t, task.WorkflowID, "task-"+task.ID.String())
}
