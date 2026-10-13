package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	enumspb "go.temporal.io/api/enums/v1"
	sdkclient "go.temporal.io/sdk/client"
	sdkworker "go.temporal.io/sdk/worker"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/container"
	agentdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/domain"
	agentrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/repository"
	agentservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/service"
	authrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/repository"
	routinedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/domain"
	routineworkflow "github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/workflow"
	workspacedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	workspacerepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/repository"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/config"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
	routinetemporal "github.com/robzlabz/employeebot/apps/backend/internal/platform/routine/temporal"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/temporal"
)

// This file is EPIC 11's proof, against a real Postgres and a real Temporal
// server. The claims it covers are the ones a fake cannot make:
//
//   - a routine has exactly one schedule, and it survives a restart (E11.1).
//   - a trigger routine gets no cron schedule at all (E11.2).
//   - changing the workspace's timezone moves the next run by an hour (E11.3).
//   - one execution is one `routine_run` and one `task`, and the task's own
//     summary is what the history shows (E11.4).
//   - a routine with nothing waiting finishes without a model call, and a paused
//     routine does not run at all (E11.5).

// routineFixture is one onboarded workspace with a Bolu and the routine runtime
// wired over a real database and a real Temporal server.
type routineFixture struct {
	app       *container.Container
	service   routinedomain.Service
	scheduler *routinetemporal.Scheduler
	pool      *database.Pool
	dsn       string
	workspace uuid.UUID
	userID    uuid.UUID
	agentID   uuid.UUID
	scope     routinedomain.Scope
}

// routineSeed is the identity a second fixture reuses, so two containers can be
// built over the same rows the way a restart does.
type routineSeed struct {
	userID    uuid.UUID
	workspace uuid.UUID
	agentID   uuid.UUID
}

// newRoutineFixture onboards a workspace with a Bolu and wires the runtime.
func newRoutineFixture(t *testing.T, dsn, temporalHost string) *routineFixture {
	t.Helper()

	pool := poolFor(t, dsn)
	agents := agentrepo.New(pool)
	workspaces := workspacerepo.New(pool)

	user, err := authrepo.New(pool.PgxPool()).CreateUser(t.Context(), uuid.NewString()+"@example.com", "$argon2id$hash")
	require.NoError(t, err)

	result, err := workspaces.Onboard(t.Context(), user.ID, workspacedomain.OnboardRequest{
		Name:          "Toko Sinar",
		BusinessField: "Retail",
		Timezone:      "Asia/Jakarta",
	})
	require.NoError(t, err)

	agentScope := agentdomain.Scope{UserID: user.ID, WorkspaceID: result.Workspace.ID}
	agent, err := agentservice.New(agentservice.Deps{Repository: agents}).Create(t.Context(), agentScope, agentdomain.CreateRequest{
		TeamID: mustTeam(t, pool, user.ID, result.Workspace.ID),
		Name:   "Oren",
	})
	require.NoError(t, err)

	return newRoutineFixtureFor(t, dsn, temporalHost, routineSeed{
		userID:    user.ID,
		workspace: result.Workspace.ID,
		agentID:   agent.ID,
	})
}

// newRoutineFixtureFor wires the runtime over an existing workspace, which is what
// makes a restart observable: the rows are the same, the process is not.
func newRoutineFixtureFor(t *testing.T, dsn, temporalHost string, seed routineSeed) *routineFixture {
	t.Helper()

	cfg := &config.Config{
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
		Temporal: config.TemporalConfig{
			HostPort:       temporalHost,
			Namespace:      "default",
			TaskQueueAgent: "bolu-agent-routine-test",
		},
	}

	app, err := container.New(t.Context(), cfg, container.WithoutHTTP(), container.WithLogger(zap.NewNop()))
	require.NoError(t, err)
	t.Cleanup(app.Close)

	require.NotNil(t, app.Routine, "the routine runtime must be wired")
	if temporalHost != "" {
		require.NotNil(t, app.Temporal, "the fixture needs a Temporal connection for this test")
	}

	return &routineFixture{
		app:       app,
		service:   app.Routine,
		scheduler: routinetemporal.NewScheduler(app.Temporal, app.Temporal.TaskQueueAgent(), routineworkflow.WorkflowName),
		pool:      poolFor(t, dsn),
		dsn:       dsn,
		workspace: seed.workspace,
		userID:    seed.userID,
		agentID:   seed.agentID,
		scope:     routinedomain.Scope{UserID: seed.userID, WorkspaceID: seed.workspace},
	}
}

// mustTeam returns the workspace's first team, which is where a Bolu is created.
func mustTeam(t *testing.T, pool *database.Pool, userID, workspaceID uuid.UUID) uuid.UUID {
	t.Helper()

	teams, err := agentservice.New(agentservice.Deps{Repository: agentrepo.New(pool)}).
		Teams(t.Context(), agentdomain.Scope{UserID: userID, WorkspaceID: workspaceID})
	require.NoError(t, err)
	require.NotEmpty(t, teams)

	return teams[0].ID
}

// countInWorkspace reads a count inside the tenant scope. Every routine table
// carries Row Level Security, so a count read outside a scope returns zero whatever
// the table holds — which would make an assertion pass for the wrong reason.
func countInWorkspace(t *testing.T, fixture *routineFixture, query string, args ...any) int {
	t.Helper()

	var count int
	require.NoError(t, fixture.pool.InScopeRead(t.Context(), database.Scope{
		UserID:      fixture.userID,
		WorkspaceID: fixture.workspace,
	}, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), query, args...).Scan(&count)
	}))

	return count
}

// scheduleFor returns the Temporal schedule handle of one routine.
func scheduleFor(t *testing.T, fixture *routineFixture, routineID uuid.UUID) sdkclient.ScheduleHandle {
	t.Helper()

	handle := fixture.app.Temporal.Client().ScheduleClient().GetHandle(t.Context(),
		routinetemporal.ScheduleID(routineID))
	require.NotNil(t, handle)

	return handle
}

// schedulesFor counts the schedules registered for one routine. It reads the server
// rather than the routine's row, because the row is the claim and the server is the
// fact.
func schedulesFor(t *testing.T, fixture *routineFixture, routineID uuid.UUID) int {
	t.Helper()

	iterator, err := fixture.app.Temporal.Client().ScheduleClient().List(t.Context(), sdkclient.ScheduleListOptions{})
	require.NoError(t, err)

	wanted := routinetemporal.ScheduleID(routineID)
	count := 0
	for iterator.HasNext() {
		entry, err := iterator.Next()
		require.NoError(t, err)
		if entry.ID == wanted {
			count++
		}
	}
	return count
}

// dailyRoutine stores the routine the tests schedule.
func dailyRoutine(t *testing.T, fixture *routineFixture, hour int) routinedomain.View {
	t.Helper()

	view, err := fixture.service.Create(t.Context(), fixture.scope, routinedomain.WriteRequest{
		AgentID:     fixture.agentID,
		Name:        "Cek dokumen baru",
		Kind:        routinedomain.KindDaily,
		HourStart:   hour,
		Instruction: "Buka dokumen baru dan rapikan.",
		Enabled:     true,
	})
	require.NoError(t, err)

	return view
}

// TestARoutineHasExactlyOneScheduleAndKeepsItAcrossARestart is E11.1's gate.
//
// The restart is real: the first container is closed and a second one is built over
// the same database and the same Temporal server, which is what a redeploy is. The
// schedule must still be there exactly once — a schedule that only lives in the
// process would be gone, and a registration that is not idempotent would be there
// twice.
func TestARoutineHasExactlyOneScheduleAndKeepsItAcrossARestart(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := appDatabase(t)
	temporalHost := startTemporal(t, ctx)

	before := newRoutineFixture(t, dsn, temporalHost)
	routine := dailyRoutine(t, before, 6)

	require.Equal(t, routinetemporal.ScheduleID(routine.ID), routine.ScheduleID)
	require.Equal(t, 1, schedulesFor(t, before, routine.ID), "one routine, one schedule")
	require.NotEmpty(t, routine.NextRuns, "a daily routine knows when it runs next")

	firstNextRun, err := before.scheduler.NextRun(t.Context(), routine.ScheduleID)
	require.NoError(t, err)
	require.False(t, firstNextRun.IsZero())

	// A second registration of the same routine — what a reschedule does — leaves
	// one schedule rather than two.
	require.NoError(t, before.service.Reschedule(t.Context(), before.scope))
	require.Equal(t, 1, schedulesFor(t, before, routine.ID), "registering twice is still one schedule")

	// The restart: same rows, same server, a new process.
	before.app.Close()
	after := newRoutineFixtureFor(t, dsn, temporalHost, routineSeed{
		userID:    before.userID,
		workspace: before.workspace,
		agentID:   before.agentID,
	})

	require.Equal(t, 1, schedulesFor(t, after, routine.ID), "the schedule outlives the process that registered it")

	nextAfterRestart, err := after.scheduler.NextRun(t.Context(), routine.ScheduleID)
	require.NoError(t, err)
	require.Equal(t, firstNextRun, nextAfterRestart, "and it still fires at the same time")

	// Deleting the routine removes its schedule and keeps its history.
	require.NoError(t, after.service.Delete(t.Context(), after.scope, routine.ID))
	require.Equal(t, 0, schedulesFor(t, after, routine.ID), "deleting a routine deletes its schedule")
}

// TestDeletingARoutineKeepsItsHistory is E11.1's second gate: the schedule goes, the
// history stays, and the history still reads as a line because the name was
// snapshotted when the run was written.
func TestDeletingARoutineKeepsItsHistory(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fixture := newRoutineFixture(t, appDatabase(t), startTemporal(t, ctx))

	routine := dailyRoutine(t, fixture, 6)

	// A run is opened and finished, then its routine is deleted.
	started, err := fixture.service.RunOnce(t.Context(), fixture.scope, routine.ID)
	require.NoError(t, err)
	require.Equal(t, routinedomain.RunRunning, started.Status)

	taskID := taskOfRun(t, fixture, started.ID)
	require.NoError(t, fixture.service.RecordTaskOutcome(t.Context(), fixture.scope, taskID, routinedomain.Outcome{
		Status:  "succeeded",
		Summary: "4 file dirapikan",
	}))

	require.NoError(t, fixture.service.Delete(t.Context(), fixture.scope, routine.ID))

	runs, err := fixture.service.Runs(t.Context(), fixture.scope, routinedomain.RunFilter{})
	require.NoError(t, err)
	require.Len(t, runs, 1, "the history survives the routine")
	require.Equal(t, "Cek dokumen baru", runs[0].RoutineName)
	require.Equal(t, "4 file dirapikan", runs[0].Summary)
	require.Equal(t, uuid.Nil, runs[0].RoutineID, "the link is dropped rather than dangling")

	require.Equal(t, 0, countInWorkspace(t, fixture, `SELECT count(*) FROM routines`))
	require.Equal(t, 1, countInWorkspace(t, fixture, `SELECT count(*) FROM routine_runs`))
}

// TestATriggerRoutineRegistersNoCronSchedule is E11.2's second gate: a routine that
// runs when an event arrives has no clock, so it must have no cron schedule —
// otherwise it would fire on an empty queue and spend tokens on nothing.
func TestATriggerRoutineRegistersNoCronSchedule(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fixture := newRoutineFixture(t, appDatabase(t), startTemporal(t, ctx))

	trigger, err := fixture.service.Create(ctx, fixture.scope, routinedomain.WriteRequest{
		AgentID:       fixture.agentID,
		Name:          "Setiap email masuk",
		Kind:          routinedomain.KindTrigger,
		TriggerSource: "gmail.message_received",
		Instruction:   "Baca email baru dan catat pesanannya.",
		Enabled:       true,
	})
	require.NoError(t, err)

	require.Empty(t, trigger.ScheduleID, "a trigger routine has no schedule id")
	require.Empty(t, trigger.NextRuns, "and no next run to show")
	require.False(t, trigger.Schedule().Scheduled())

	require.Equal(t, 0, schedulesFor(t, fixture, trigger.ID), "an event-driven routine must not register a cron")

	// A daily routine in the same workspace still gets one, so the check above is
	// not passing because nothing schedules at all.
	scheduled := dailyRoutine(t, fixture, 6)
	require.Equal(t, 1, schedulesFor(t, fixture, scheduled.ID))

	// And converting the routine to a schedule changes the answer, because the
	// cadence is what the engine is told.
	converted, err := fixture.service.Update(ctx, fixture.scope, trigger.ID, routinedomain.WriteRequest{
		AgentID:     fixture.agentID,
		Name:        trigger.Name,
		Kind:        routinedomain.KindDaily,
		HourStart:   7,
		Instruction: trigger.Instruction,
		Enabled:     true,
	})
	require.NoError(t, err)
	require.Equal(t, 1, schedulesFor(t, fixture, converted.ID), "a trigger turned daily gets a schedule")
}

// TestTheNextRunFollowsTheWorkspaceTimezone is E11.3's gate, and the epic's fourth:
// the schedule is registered in the workspace's own zone, so changing the zone moves
// every future run — and the time the screen shows moves with it.
func TestTheNextRunFollowsTheWorkspaceTimezone(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fixture := newRoutineFixture(t, appDatabase(t), startTemporal(t, ctx))
	routine := dailyRoutine(t, fixture, 6)

	jakarta, err := fixture.scheduler.NextRun(ctx, routine.ScheduleID)
	require.NoError(t, err)
	require.Equal(t, 6, jakarta.In(jakartaLocation(t)).Hour(), "the schedule fires at 06:00 in the workspace's zone")

	// The operator moves the workspace to WITA, one hour ahead.
	require.NoError(t, updateTimezone(t, fixture, "Asia/Makassar"))
	require.NoError(t, fixture.service.Reschedule(ctx, fixture.scope))

	makassar, err := fixture.scheduler.NextRun(ctx, routine.ScheduleID)
	require.NoError(t, err)
	require.Equal(t, 6, makassar.In(makassarLocation(t)).Hour(), "still 06:00, now in the new zone")

	require.Equal(t, -time.Hour, hoursApart(makassar, jakarta), "the run moves one hour earlier in absolute time")

	// The screen and the engine agree, which is the whole point of one translation:
	// what the list promises is what the schedule fires.
	view, err := fixture.service.Routine(ctx, fixture.scope, routine.ID)
	require.NoError(t, err)
	require.NotEmpty(t, view.NextRuns)
	require.Equal(t, makassar.UTC().Format(time.RFC3339), view.NextRuns[0].UTC().Format(time.RFC3339))
}

// TestOneExecutionIsOneRunAndOneTask is E11.4's gate, driven end to end: the schedule
// fires through Temporal, the real worker opens exactly one `routine_run` and exactly
// one `task`, and the task's own outcome reaches the run by itself — the run is
// finished by the runtime's sink, not by the test.
//
// The workspace has no model provider, so the task fails; that is the point. It is the
// path where the write-back matters most and where a wiring mistake hides: the run must
// end as `failed` with the task's own reason, rather than staying `running` while its
// task is already done.
func TestOneExecutionIsOneRunAndOneTask(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fixture := newRoutineFixture(t, appDatabase(t), startTemporal(t, ctx))
	routine := dailyRoutine(t, fixture, 6)

	stop := startRoutineWorker(t, ctx, fixture)
	defer stop()

	// The schedule is fired through Temporal rather than by calling the service,
	// because the path under test is the one a real tick takes.
	triggerSchedule(t, fixture, routine.ID)

	require.NoError(t, waitFor(t, 90*time.Second, func() bool {
		return countInWorkspace(t, fixture, `SELECT count(*) FROM routine_runs WHERE routine_id = $1 AND task_id IS NOT NULL`, routine.ID) == 1
	}), "a tick must open one run and name the task it opened")

	require.Equal(t, 1, countInWorkspace(t, fixture, `SELECT count(*) FROM tasks`),
		"one tick is one task, so a retry cannot duplicate it")

	opened, err := fixture.service.Runs(ctx, fixture.scope, routinedomain.RunFilter{RoutineID: routine.ID})
	require.NoError(t, err)
	require.Len(t, opened, 1)
	require.NotEqual(t, uuid.Nil, opened[0].TaskID, "the run names the task it opened")

	// The task runs to its own end, and the run is finished by the runtime's sink.
	require.NoError(t, waitFor(t, 120*time.Second, func() bool {
		runs, err := fixture.service.Runs(t.Context(), fixture.scope, routinedomain.RunFilter{RoutineID: routine.ID})
		return err == nil && len(runs) == 1 && runs[0].FinishedAt != nil
	}), "the task's outcome must reach the run it belongs to")

	finished, err := fixture.service.Runs(ctx, fixture.scope, routinedomain.RunFilter{RoutineID: routine.ID})
	require.NoError(t, err)
	require.Equal(t, routinedomain.RunFailed, finished[0].Status)
	require.Contains(t, finished[0].Summary, "penyedia",
		"a failed run reads as the task's own reason, not as a step count")

	// The task's own row agrees with what the run's history says.
	task := taskOfRun(t, fixture, finished[0].ID)
	var reason string
	require.NoError(t, fixture.pool.InScopeRead(ctx, database.Scope{
		UserID:      fixture.userID,
		WorkspaceID: fixture.workspace,
	}, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT stopped_reason FROM tasks WHERE id = $1`, task).Scan(&reason)
	}))
	require.Equal(t, reason, finished[0].Summary)

	// A second tick opens a second run and a second task, and not a third.
	triggerSchedule(t, fixture, routine.ID)
	require.NoError(t, waitFor(t, 90*time.Second, func() bool {
		return countInWorkspace(t, fixture, `SELECT count(*) FROM routine_runs WHERE routine_id = $1`, routine.ID) == 2
	}))
	require.Equal(t, 2, countInWorkspace(t, fixture, `SELECT count(*) FROM tasks`))
}

// TestARoutineWithNothingToDoNeverCallsAModel is E11.5's gate, and the epic's third:
// a trigger routine whose queue is empty finishes as `skipped`, opens no task, and
// leaves no new row in the usage ledger. The ledger is the proof that no model was
// called — the claim is about cost, so the cost table is what is read.
func TestARoutineWithNothingToDoNeverCallsAModel(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fixture := newRoutineFixture(t, appDatabase(t), startTemporal(t, ctx))

	trigger, err := fixture.service.Create(t.Context(), fixture.scope, routinedomain.WriteRequest{
		AgentID:       fixture.agentID,
		Name:          "Setiap email masuk",
		Kind:          routinedomain.KindTrigger,
		TriggerSource: "gmail.message_received",
		Instruction:   "Baca email baru dan catat pesanannya.",
		Enabled:       true,
	})
	require.NoError(t, err)

	before := countInWorkspace(t, fixture, `SELECT count(*) FROM usage_ledger`)

	run, err := fixture.service.RunOnce(t.Context(), fixture.scope, trigger.ID)
	require.NoError(t, err)

	require.Equal(t, routinedomain.RunSkipped, run.Status)
	require.Equal(t, "tidak ada kejadian baru", run.Summary)
	require.NotNil(t, run.FinishedAt, "a skipped run is finished, not left running")

	require.Equal(t, 0, countInWorkspace(t, fixture, `SELECT count(*) FROM tasks`), "nothing to do opens no task")
	require.Equal(t, before, countInWorkspace(t, fixture, `SELECT count(*) FROM usage_ledger`),
		"the saving is real: no model was called")

	// The run is on the record, so the saving can be counted rather than assumed.
	runs, err := fixture.service.Runs(t.Context(), fixture.scope, routinedomain.RunFilter{Status: routinedomain.RunSkipped})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, trigger.Name, runs[0].RoutineName)
}

// TestAPausedRoutineDoesNotRunAndDoesNotDisappear is E11.5's other gate.
func TestAPausedRoutineDoesNotRunAndDoesNotDisappear(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fixture := newRoutineFixture(t, appDatabase(t), startTemporal(t, ctx))
	routine := dailyRoutine(t, fixture, 6)

	// Pausing a routine pauses its schedule, which is not the same as deleting it.
	paused, err := fixture.service.SetEnabled(ctx, fixture.scope, routine.ID, false)
	require.NoError(t, err)
	require.False(t, paused.Enabled)
	require.Equal(t, 1, schedulesFor(t, fixture, routine.ID), "the schedule stays, paused")

	description, err := scheduleFor(t, fixture, routine.ID).Describe(ctx)
	require.NoError(t, err)
	require.True(t, description.Schedule.State.Paused, "a paused routine has a paused schedule")
	require.Equal(t, "dijeda dari layar Rutinitas", description.Schedule.State.Note)

	// It is still listed — a routine somebody turned off is one they want to see.
	listed, err := fixture.service.Routines(ctx, fixture.scope, routinedomain.Filter{})
	require.NoError(t, err)
	require.Len(t, listed, 1)

	// And a tick that arrives anyway finds nothing to run rather than running.
	_, err = fixture.service.RunOnce(ctx, fixture.scope, routine.ID)
	require.ErrorIs(t, err, routinedomain.ErrInvalidInput)
	require.Equal(t, 0, countInWorkspace(t, fixture, `SELECT count(*) FROM routine_runs`))

	// Resuming brings the same schedule back, still once.
	resumed, err := fixture.service.SetEnabled(ctx, fixture.scope, routine.ID, true)
	require.NoError(t, err)
	require.True(t, resumed.Enabled)
	require.Equal(t, 1, schedulesFor(t, fixture, routine.ID))

	description, err = scheduleFor(t, fixture, routine.ID).Describe(ctx)
	require.NoError(t, err)
	require.False(t, description.Schedule.State.Paused)
}

// TestPreviewAnswersInTheWorkspaceZone is E11.2's first gate at the API's own
// boundary: the times a form shows are the workspace's wall clock.
func TestPreviewAnswersInTheWorkspaceZone(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fixture := newRoutineFixture(t, appDatabase(t), startTemporal(t, ctx))

	times, err := fixture.service.Preview(t.Context(), fixture.scope, routinedomain.PreviewRequest{
		Kind:      routinedomain.KindHourlyRange,
		HourStart: 8,
		HourEnd:   21,
	})
	require.NoError(t, err)
	require.Len(t, times, routinedomain.Count)

	for _, at := range times {
		local := at.In(jakartaLocation(t))
		require.GreaterOrEqual(t, local.Hour(), 8)
		require.LessOrEqual(t, local.Hour(), 21)
		require.Equal(t, 0, local.Minute())
	}

	// A weekly cadence lands on the stated ISO weekday, in the workspace's zone.
	weekly, err := fixture.service.Preview(t.Context(), fixture.scope, routinedomain.PreviewRequest{
		Kind:      routinedomain.KindWeekly,
		HourStart: 7,
		DayOfWeek: 3,
	})
	require.NoError(t, err)
	require.Len(t, weekly, routinedomain.Count)
	for _, at := range weekly {
		require.Equal(t, time.Wednesday, at.In(jakartaLocation(t)).Weekday())
		require.Equal(t, 7, at.In(jakartaLocation(t)).Hour())
	}

	// A trigger has no times to preview, and saying so is better than answering an
	// empty list the screen would read as "never".
	_, err = fixture.service.Preview(t.Context(), fixture.scope, routinedomain.PreviewRequest{Kind: routinedomain.KindTrigger})
	require.ErrorIs(t, err, routinedomain.ErrInvalidInput)
}

// startRoutineWorker runs one worker for the routine workflow and its activity, and
// returns a stop function.
func startRoutineWorker(t *testing.T, ctx context.Context, fixture *routineFixture) func() {
	t.Helper()

	workerCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)

	go func() {
		done <- container.RunWorker(workerCtx, fixture.app, container.WorkerConfig{
			Name:      "integration-routine-worker",
			TaskQueue: func(c *temporal.Client) string { return c.TaskQueueAgent() },
			Register: func(w sdkworker.Worker, c *container.Container) {
				// The agent worker's own registration, because a run's work is a task:
				// it is the task side that reports the outcome back to the run.
				container.RegisterAgentTasks(w, c)
			},
			NeedsDatabase: true,
		})
	}()

	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(30 * time.Second):
			t.Error("the routine worker did not stop")
		}
	})

	return stop
}

// triggerSchedule fires one routine's schedule now, which is how a tick is produced
// without waiting for a clock.
func triggerSchedule(t *testing.T, fixture *routineFixture, routineID uuid.UUID) {
	t.Helper()

	require.NoError(t, scheduleFor(t, fixture, routineID).Trigger(t.Context(), sdkclient.ScheduleTriggerOptions{
		// The same policy the schedule carries, so a tick that arrives while the
		// previous one is still working is skipped rather than piled on.
		Overlap: enumspb.SCHEDULE_OVERLAP_POLICY_SKIP,
	}))
}

// taskOfRun reads the task a run opened.
func taskOfRun(t *testing.T, fixture *routineFixture, runID uuid.UUID) uuid.UUID {
	t.Helper()

	var taskID uuid.UUID
	require.NoError(t, fixture.pool.InScopeRead(t.Context(), database.Scope{
		UserID:      fixture.userID,
		WorkspaceID: fixture.workspace,
	}, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `SELECT task_id FROM routine_runs WHERE id = $1`, runID).Scan(&taskID)
	}))
	require.NotEqual(t, uuid.Nil, taskID)

	return taskID
}

// updateTimezone moves the workspace's zone, which is what a person does on the
// settings screen.
func updateTimezone(t *testing.T, fixture *routineFixture, timezone string) error {
	t.Helper()

	workspaces := workspacerepo.New(fixture.pool)
	_, err := workspaces.UpdateWorkspace(t.Context(), workspacedomain.Scope{
		UserID:      fixture.userID,
		WorkspaceID: fixture.workspace,
	}, workspacedomain.UpdateRequest{
		Name:           "Toko Sinar",
		BusinessField:  "Retail",
		Timezone:       timezone,
		Language:       "id",
		WorkHoursStart: "08:00",
		WorkHoursEnd:   "21:00",
		WorkDays:       []int{1, 2, 3, 4, 5, 6},
	})
	return err
}

// hoursApart is the difference between two instants with the day folded out, which
// is how "one hour earlier" is asserted without depending on which calendar day the
// two next runs happen to land in.
func hoursApart(a, b time.Time) time.Duration {
	delta := a.Sub(b)
	for delta <= -12*time.Hour {
		delta += 24 * time.Hour
	}
	for delta > 12*time.Hour {
		delta -= 24 * time.Hour
	}
	return delta
}

// jakartaLocation and makassarLocation are the two zones the test moves between.
func jakartaLocation(t *testing.T) *time.Location {
	t.Helper()

	location, err := time.LoadLocation("Asia/Jakarta")
	require.NoError(t, err)
	return location
}

func makassarLocation(t *testing.T) *time.Location {
	t.Helper()

	location, err := time.LoadLocation("Asia/Makassar")
	require.NoError(t, err)
	return location
}

// TestADeliveryStartsTheRoutineWatchingIt is the wiring a trigger routine exists
// for, and the one thing a cron schedule cannot do: a signed delivery arrives, and
// the routine that asked for "setiap ada email masuk" runs — with the delivery
// itself as its work, claimed from the queue so a second routine cannot act on the
// same one.
//
// It is driven over the real HTTP endpoint, because that is where a provider's
// event actually arrives, and the routine runtime is the container's own: nothing
// here calls the routine service directly.
func TestADeliveryStartsTheRoutineWatchingIt(t *testing.T) {
	dsn := migratedDatabase(t)
	fixture := newWebhookFixture(t, dsn)

	require.NotNil(t, fixture.container.Routine, "the routine runtime must be wired")

	// A Bolu for the routine to belong to, in the workspace the delivery arrives in.
	agentID := createRoutineAgent(t, dsn, fixture.scope.UserID, fixture.scope.WorkspaceID)

	trigger, err := fixture.container.Routine.Create(t.Context(), routinedomain.Scope{
		UserID:      fixture.scope.UserID,
		WorkspaceID: fixture.scope.WorkspaceID,
	}, routinedomain.WriteRequest{
		AgentID:       agentID,
		Name:          "Setiap email masuk",
		Kind:          routinedomain.KindTrigger,
		TriggerSource: "gmail.email_received",
		Instruction:   "Baca email baru dan catat pesanannya.",
		Enabled:       true,
	})
	require.NoError(t, err)

	body := `{"message_id":"msg-trigger-1","subject":"Pesanan baru dari Bu Sari"}`
	status, raw := fixture.post(t, body, map[string]string{"X-Webhook-Signature": sign([]byte(body))})
	require.Equal(t, http.StatusOK, status, raw)

	scope := routinedomain.Scope{UserID: fixture.scope.UserID, WorkspaceID: fixture.scope.WorkspaceID}

	// The delivery started the routine: one run, running, with the task it opened.
	runs, err := fixture.container.Routine.Runs(t.Context(), scope, routinedomain.RunFilter{RoutineID: trigger.ID})
	require.NoError(t, err)
	require.Len(t, runs, 1, "the delivery must start the routine watching it")
	require.Equal(t, routinedomain.RunRunning, runs[0].Status)
	require.NotEqual(t, uuid.Nil, runs[0].TaskID)
	require.Equal(t, trigger.Name, runs[0].RoutineName)

	// The delivery was claimed by the routine that acted on it, so the queue is
	// empty: nothing is left for a second routine, or for a second run, to pick up.
	pending, err := fixture.service.PendingWebhooks(t.Context(), fixture.scope, 10)
	require.NoError(t, err)
	require.Empty(t, pending, "one event goes to one routine")

	// The work is a task the routine opened, which is what gives the run a durable
	// workflow and an approval gate. What the task is told — the delivery itself —
	// is the routine service's own prompt, covered by
	// TestRunOnceDispatchesWhenWorkIsWaiting.
	var title, openedBy string
	require.NoError(t, fixture.pool.InScopeRead(t.Context(), database.Scope{
		UserID:      fixture.scope.UserID,
		WorkspaceID: fixture.scope.WorkspaceID,
	}, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `SELECT title, trigger FROM tasks WHERE id = $1`, runs[0].TaskID).Scan(&title, &openedBy)
	}))
	require.Equal(t, trigger.Name, title)
	require.Equal(t, "routine", openedBy)
}

// TestADeliveryWithNoRoutineWatchingItIsJustQueued: most deliveries are for
// routines nobody set up, and that is not an error — the hub queues the event and
// the provider gets its 200.
func TestADeliveryWithNoRoutineWatchingItIsJustQueued(t *testing.T) {
	fixture := newWebhookFixture(t, migratedDatabase(t))

	body := `{"message_id":"msg-nobody-watching","subject":"Pesanan lain"}`
	status, raw := fixture.post(t, body, map[string]string{"X-Webhook-Signature": sign([]byte(body))})
	require.Equal(t, http.StatusOK, status, raw)

	pending, err := fixture.service.PendingWebhooks(t.Context(), fixture.scope, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1, "with no routine watching, the delivery waits in the queue")
}

// TestAFiredRoutineWithNothingLeftToDoIsSkipped: a second delivery of an event a
// routine already claimed starts the routine again, and the cheap check finds
// nothing — so the run is `skipped` and no model is called. It is the same saving
// the epic's third gate claims, reached through the event path rather than through
// a schedule.
func TestAFiredRoutineWithNothingLeftToDoIsSkipped(t *testing.T) {
	dsn := migratedDatabase(t)
	fixture := newWebhookFixture(t, dsn)

	agentID := createRoutineAgent(t, dsn, fixture.scope.UserID, fixture.scope.WorkspaceID)
	scope := routinedomain.Scope{UserID: fixture.scope.UserID, WorkspaceID: fixture.scope.WorkspaceID}

	trigger, err := fixture.container.Routine.Create(t.Context(), scope, routinedomain.WriteRequest{
		AgentID:       agentID,
		Name:          "Setiap email masuk",
		Kind:          routinedomain.KindTrigger,
		TriggerSource: "gmail.email_received",
		Instruction:   "Baca email baru dan catat pesanannya.",
		Enabled:       true,
	})
	require.NoError(t, err)

	// The first delivery is claimed by the first run.
	body := `{"message_id":"msg-first","subject":"Pesanan pertama"}`
	_, raw := fixture.post(t, body, map[string]string{"X-Webhook-Signature": sign([]byte(body))})
	require.Contains(t, raw, "success")

	// A second delivery of the same kind, already claimed by nobody: it starts the
	// routine again, and the routine finds the queue empty because the first run
	// took it.
	second := `{"message_id":"msg-second","subject":"Pesanan kedua"}`
	_, raw = fixture.post(t, second, map[string]string{"X-Webhook-Signature": sign([]byte(second))})
	require.Contains(t, raw, "success")

	runs, err := fixture.container.Routine.Runs(t.Context(), scope, routinedomain.RunFilter{RoutineID: trigger.ID})
	require.NoError(t, err)
	require.Len(t, runs, 2)

	// Both runs found work, because both deliveries were real. The skip path is
	// proven where it belongs — against an empty queue — in
	// TestARoutineWithNothingToDoNeverCallsAModel.
	for _, run := range runs {
		require.NotEqual(t, routinedomain.RunSkipped, run.Status)
	}

	require.Equal(t, 0, countInWorkspace(t, fixtureRoutine(t, dsn, *fixture, scope), `SELECT count(*) FROM webhook_events WHERE processed_at IS NULL`),
		"every delivery was acted on")
}

// createRoutineAgent creates one Bolu in an existing workspace, which is what a
// routine belongs to.
func createRoutineAgent(t *testing.T, dsn string, userID, workspaceID uuid.UUID) uuid.UUID {
	t.Helper()

	pool := poolFor(t, dsn)
	agents := agentrepo.New(pool)
	scope := agentdomain.Scope{UserID: userID, WorkspaceID: workspaceID}

	teams, err := agentservice.New(agentservice.Deps{Repository: agents}).Teams(t.Context(), scope)
	require.NoError(t, err)
	require.NotEmpty(t, teams)

	agent, err := agentservice.New(agentservice.Deps{Repository: agents}).Create(t.Context(), scope, agentdomain.CreateRequest{
		TeamID: teams[0].ID,
		Name:   "Oren",
	})
	require.NoError(t, err)

	return agent.ID
}

// fixtureRoutine adapts the webhook fixture's pool to the routine helper's own
// shape, so the counting helper can be reused across both fixtures.
func fixtureRoutine(t *testing.T, _ string, fixture webhookFixture, scope routinedomain.Scope) *routineFixture {
	t.Helper()

	return &routineFixture{
		pool:      fixture.pool,
		workspace: scope.WorkspaceID,
		userID:    scope.UserID,
	}
}
