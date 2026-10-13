package container

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
	routineactivity "github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/activity"
	routinedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/domain/mocks"
	routinehandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/handler"
	taskdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	workspacerepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/repository"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
)

// The wiring tests for the routine module cover what only the container can get
// wrong: which routes exist, what they answer when the module is not configured, and
// whether the three adapters hold — the task starter, the event queue, and the outcome
// sink that carries a finished task's summary back to its run.

// TestOpenRoutineWithoutADatabaseLeavesTheModuleUnwired keeps the container's
// "running without Postgres" contract.
func TestOpenRoutineWithoutADatabaseLeavesTheModuleUnwired(t *testing.T) {
	c := &Container{
		Config:       testConfig(),
		Logger:       zap.NewNop(),
		Repositories: &Repositories{},
		Services:     &Services{},
	}

	require.NoError(t, c.openRoutine(context.Background(), c.Config))
	require.Nil(t, c.Routine, "the service stays nil, so its routes report not configured")
	require.Nil(t, c.RoutineTasks, "and the worker registers nothing of it")
	require.ErrorIs(t,
		taskOutcomeSink{container: c}.Report(context.Background(), taskdomain.Scope{WorkspaceID: uuid.New()}, taskdomain.OutcomeReport{TaskID: uuid.New()}),
		taskdomain.ErrOutcomeUnclaimed,
		"and the task runtime is handed a sink that claims nothing")
}

// TestRoutineRoutesAnswer503WhenTheModuleIsMissing is the honest answer for a
// deployment without a database.
func TestRoutineRoutesAnswer503WhenTheModuleIsMissing(t *testing.T) {
	app := routineApp(t, nil)

	for _, request := range []struct{ method, path string }{
		{http.MethodGet, "/api/routines"},
		{http.MethodPost, "/api/routines"},
		{http.MethodPost, "/api/routines/preview"},
		{http.MethodGet, "/api/routines/" + uuid.NewString()},
		{http.MethodPatch, "/api/routines/" + uuid.NewString()},
		{http.MethodDelete, "/api/routines/" + uuid.NewString()},
		{http.MethodPost, "/api/routines/" + uuid.NewString() + "/pause"},
		{http.MethodPost, "/api/routines/" + uuid.NewString() + "/resume"},
		{http.MethodGet, "/api/routines/" + uuid.NewString() + "/runs"},
		{http.MethodGet, "/api/routine-runs"},
	} {
		req := httptest.NewRequest(request.method, request.path, nil)
		resp, err := app.Test(req, -1)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		require.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode, request.path)
		require.Contains(t, string(body), "routine_not_configured", request.path)
	}
}

// TestRoutineRoutesAreMountedUnderTheTenantGroup proves the endpoints exist once the
// module is configured, and that the scope the handler passes on is the workspace the
// middleware resolved.
func TestRoutineRoutesAreMountedUnderTheTenantGroup(t *testing.T) {
	service := mocks.NewService(t)
	app := routineApp(t, service)

	service.EXPECT().
		Routines(mock.Anything, mock.MatchedBy(func(scope routinedomain.Scope) bool {
			return scope.WorkspaceID == routineWorkspaceID
		}), mock.Anything).
		Return(nil, nil).
		Once()

	req := httptest.NewRequest(http.MethodGet, "/api/routines", nil)
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestRoutineTaskStarterForwardsTheTrigger is the seam a run's work goes through: what
// the routine module calls a run becomes what the runtime calls a task, with the
// trigger that says a routine opened it.
func TestRoutineTaskStarterForwardsTheTrigger(t *testing.T) {
	service := &recordingTaskService{}
	starter := routineTaskStarter{service: service}

	scope := routinedomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}
	agentID := uuid.New()

	taskID, err := starter.Start(context.Background(), scope, routinedomain.StartTask{
		AgentID: agentID,
		Title:   "Cek dokumen baru",
		Prompt:  "Buka dokumen baru dan rapikan.",
	})
	require.NoError(t, err)
	require.Equal(t, service.taskID, taskID)

	require.Equal(t, scope.WorkspaceID, service.scope.WorkspaceID)
	require.Equal(t, agentID, service.dispatch.AgentID)
	require.Equal(t, taskdomain.TriggerRoutine, service.dispatch.Trigger)
	require.Equal(t, "Cek dokumen baru", service.dispatch.Title)
	require.Equal(t, "Buka dokumen baru dan rapikan.", service.dispatch.Prompt)
	require.Equal(t, uuid.Nil, service.dispatch.ConversationID,
		"a routine has no conversation: its answer is the task's own summary")
}

// TestTheOutcomeSinkCarriesTheTaskSummary is why a run's history shows a one-line
// summary: the task writes that line onto itself, and this is how it reaches the run.
func TestTheOutcomeSinkCarriesTheTaskSummary(t *testing.T) {
	service := mocks.NewService(t)
	c := &Container{Routine: service}

	scope := taskdomain.Scope{UserID: uuid.New(), WorkspaceID: reportWorkspaceID}
	taskID := uuid.New()

	service.EXPECT().
		RecordTaskOutcome(mock.Anything, mock.MatchedBy(func(scope routinedomain.Scope) bool {
			return scope.WorkspaceID == reportWorkspaceID
		}), taskID, routinedomain.Outcome{
			Status:  "succeeded",
			Summary: "4 file dirapikan",
		}).
		Return(nil).
		Once()

	require.NoError(t, taskOutcomeSink{container: c}.Report(context.Background(), scope, taskdomain.OutcomeReport{
		TaskID:  taskID,
		Status:  "succeeded",
		Summary: "4 file dirapikan",
	}))
}

// TestTheOutcomeSinkReadsTheServiceWhenItIsCalled is the ordering trap the sink is
// built this way for: the task runtime is assembled before the routine runtime, so a
// sink captured at assembly time holds a nil service and every finished routine task
// reports its outcome to nobody — a run that stays `running` forever while its task
// is already done.
func TestTheOutcomeSinkReadsTheServiceWhenItIsCalled(t *testing.T) {
	c := &Container{}
	sink := taskOutcomeSink{container: c}

	// Installed while the routines are not built yet, which is the real order.
	require.ErrorIs(t, sink.Report(context.Background(), taskdomain.Scope{WorkspaceID: uuid.New()}, taskdomain.OutcomeReport{
		TaskID: uuid.New(),
		Status: "succeeded",
	}), taskdomain.ErrOutcomeUnclaimed)

	// And the same sink, called after the routine runtime exists, writes the outcome
	// through. Nothing was re-installed: the service is read at call time.
	service := mocks.NewService(t)
	c.Routine = service

	scope := taskdomain.Scope{UserID: uuid.New(), WorkspaceID: reportWorkspaceID}
	taskID := uuid.New()

	service.EXPECT().
		RecordTaskOutcome(mock.Anything, mock.Anything, taskID, routinedomain.Outcome{Status: "failed", Reason: "tidak ada penyedia"}).
		Return(nil).
		Once()

	require.NoError(t, sink.Report(context.Background(), scope, taskdomain.OutcomeReport{
		TaskID: taskID,
		Status: "failed",
		Reason: "tidak ada penyedia",
	}))
}

// TestTheOutcomeSinkIsAbsentWithoutTheModule: a deployment with no routines reports
// "nothing claimed this task" rather than dereferencing a nil service.
func TestTheOutcomeSinkIsAbsentWithoutTheModule(t *testing.T) {
	c := &Container{}

	require.ErrorIs(t, taskOutcomeSink{container: c}.Report(context.Background(), taskdomain.Scope{}, taskdomain.OutcomeReport{
		TaskID: uuid.New(),
	}), taskdomain.ErrOutcomeUnclaimed)

	// A sink with no container at all answers the same way rather than panicking.
	require.ErrorIs(t, taskOutcomeSink{}.Report(context.Background(), taskdomain.Scope{}, taskdomain.OutcomeReport{}), taskdomain.ErrOutcomeUnclaimed)
}

// TestTheQueueAdapterPassesTheClaimThrough is the cheap check's seam: the routine
// runtime reads the integration hub's queue through this, and claiming is what stops two
// routines acting on one delivery.
func TestTheQueueAdapterPassesTheClaimThrough(t *testing.T) {
	integrations := &recordingIntegrationService{events: []integrationdomain.QueuedEvent{{
		ID:      uuid.New(),
		App:     "gmail",
		Event:   "gmail.message_received",
		Payload: []byte(`{"subject":"Pesanan baru"}`),
	}}}

	queue := routineQueue{integrations: integrations}
	scope := routinedomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}

	events, err := queue.Pending(context.Background(), scope, "gmail.message_received", 5)
	require.NoError(t, err)

	require.Len(t, events, 1)
	require.Equal(t, "gmail.message_received", events[0].Event)
	require.Contains(t, string(events[0].Payload), "Pesanan baru")

	require.Equal(t, "gmail.message_received", integrations.claimedEvent)
	require.Equal(t, 5, integrations.claimedLimit)
}

// TestTheWorkspaceClockReadsTheZone is the E11.3 input: the schedule is registered in
// the workspace's own zone, which is the only thing this adapter reads it for.
func TestTheWorkspaceClockReadsTheZone(t *testing.T) {
	workspace := &recordingWorkspaceRepository{window: workspacerepo.WorkingHours{
		Start: "08:00", End: "21:00", Timezone: "Asia/Makassar",
		Days: []int{1, 2, 3, 4, 5, 6},
	}}

	clock := workspaceClock{workspace: workspace}
	location, err := clock.Location(context.Background(), uuid.New())
	require.NoError(t, err)
	require.Equal(t, "Asia/Makassar", location.String())

	// A zone this build cannot load is reported, not guessed at: scheduling the
	// workspace in UTC would fire every routine at a wall-clock time nobody chose
	// while the settings screen still claimed the real zone.
	broken := workspaceClock{workspace: &recordingWorkspaceRepository{window: workspacerepo.WorkingHours{
		Timezone: "Bukan/Zona",
	}}}
	_, err = broken.Location(context.Background(), uuid.New())
	require.ErrorContains(t, err, "Bukan/Zona")
}

// TestRescheduleWithoutTheModuleIsANoOp: a deployment with no routines has nothing to
// move when a workspace's clock changes.
func TestRescheduleWithoutTheModuleIsANoOp(t *testing.T) {
	c := &Container{Logger: zap.NewNop()}
	require.NotPanics(t, func() { c.RescheduleRoutines(context.Background(), uuid.New()) })
}

// TestRegisterRoutineTasksNeedsAWorker: a deployment without the module registers
// nothing rather than panicking on a nil activity set.
func TestRegisterRoutineTasksNeedsAWorker(t *testing.T) {
	worker := &recordingWorker{}

	RegisterRoutineTasks(worker, &Container{})
	require.Empty(t, worker.workflows)

	RegisterRoutineTasks(worker, &Container{RoutineTasks: routineactivity.New(routineactivity.Deps{})})
	require.Len(t, worker.workflows, 1)
	require.Len(t, worker.activities, 1)
}

// routineWorkspaceID is the workspace the mounted route table resolves.
var routineWorkspaceID = uuid.New()

// The workspaces the sink and trigger tests assert on. They are named rather than
// built inline because the mocked call has to be matched against the same value the
// call carries: `x == x` in a matcher compares nothing and would pass for any scope.
var (
	reportWorkspaceID = uuid.New()
	fireWorkspaceID   = uuid.New()
)

// routineApp mounts the routine routes the way the container does.
func routineApp(t *testing.T, service routinedomain.Service) *fiber.App {
	t.Helper()

	app := fiber.New(fiber.Config{DisableStartupMessage: true})

	handlers := &Handlers{}
	if service != nil {
		handlers.Routine = routinehandler.New(service, zap.NewNop())
	}

	tenant := app.Group("/api", func(ctx *fiber.Ctx) error {
		userID := uuid.New()
		middleware.SetIdentity(ctx, userID, "owner@example.com")
		middleware.SetScope(ctx, database.Scope{UserID: userID, WorkspaceID: routineWorkspaceID})
		middleware.SetRole(ctx, "owner")
		return ctx.Next()
	})

	if handlers.Routine == nil {
		registerRoutineFallback(tenant)
	} else {
		routinehandler.Routes(tenant, handlers.Routine)
	}

	return app
}

// recordingIntegrationService answers a claim with a script.
type recordingIntegrationService struct {
	events       []integrationdomain.QueuedEvent
	claimedEvent string
	claimedLimit int
}

func (s *recordingIntegrationService) ClaimPendingEvents(
	_ context.Context,
	_ integrationdomain.Scope,
	event string,
	limit int,
) ([]integrationdomain.QueuedEvent, error) {
	s.claimedEvent = event
	s.claimedLimit = limit
	if event == "" {
		return nil, errors.New("integration: an event name is required")
	}
	return s.events, nil
}

// recordingWorkspaceRepository answers the window a routine is scheduled against.
type recordingWorkspaceRepository struct {
	window workspacerepo.WorkingHours
	err    error
}

func (r *recordingWorkspaceRepository) WorkingHours(context.Context, uuid.UUID) (workspacerepo.WorkingHours, error) {
	if r.err != nil {
		return workspacerepo.WorkingHours{}, r.err
	}
	return r.window, nil
}

// Compile-time checks: the doubles satisfy the ports the adapters take, so a rename in
// either module breaks here rather than at the call site.
var (
	_ eventClaimer            = (*recordingIntegrationService)(nil)
	_ workspaceHoursReader    = (*recordingWorkspaceRepository)(nil)
	_ routinedomain.WorkQueue = routineQueue{}
)

// TestTheClockReportsAFailureRatherThanGuessing: a workspace whose zone cannot be read
// is reported, so a reschedule does not schedule routines in a zone nobody verified.
func TestTheClockReportsAFailureRatherThanGuessing(t *testing.T) {
	broken := workspaceClock{workspace: &recordingWorkspaceRepository{err: errors.New("no such workspace")}}

	_, err := broken.Location(context.Background(), uuid.New())
	require.Error(t, err)
}

// TestTheIntegrationAdapterStartsTheRoutineWatchingAnEvent is E11.2's trigger path
// at the seam: the hub delivers an event and asks this adapter to start whatever
// watches it, and the adapter reads the routine runtime when it is called rather
// than when it was installed — the hub is assembled first, so a captured runtime
// would be nil forever and "setiap ada email masuk" would quietly never happen.
func TestTheIntegrationAdapterStartsTheRoutineWatchingAnEvent(t *testing.T) {
	c := &Container{}
	adapter := integrationTriggers{container: c}

	// Installed before the routines exist, which is the real assembly order: no
	// routine is watching, so nothing starts and nothing fails.
	started, err := adapter.Fire(context.Background(), integrationdomain.Scope{WorkspaceID: uuid.New()}, "gmail.email_received")
	require.NoError(t, err)
	require.Zero(t, started)

	// The same adapter, called after the runtime exists, reaches it.
	service := mocks.NewService(t)
	c.Routine = service

	scope := integrationdomain.Scope{UserID: uuid.New(), WorkspaceID: fireWorkspaceID}
	service.EXPECT().
		Fire(mock.Anything, mock.MatchedBy(func(scope routinedomain.Scope) bool {
			return scope.WorkspaceID == fireWorkspaceID
		}), "gmail.email_received").
		Return(2, nil).
		Once()

	started, err = adapter.Fire(context.Background(), scope, "gmail.email_received")
	require.NoError(t, err)
	require.Equal(t, 2, started)

	// An adapter with no container at all answers the same way rather than panicking.
	started, err = integrationTriggers{}.Fire(context.Background(), scope, "gmail.email_received")
	require.NoError(t, err)
	require.Zero(t, started)
}
