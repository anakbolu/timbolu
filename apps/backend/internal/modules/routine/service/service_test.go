package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/service"
)

// The doubles below are scripted rather than simulated. What the repository does
// with a cadence — the unique schedule index, the run history's survival of a
// deleted routine — is the integration tests' business against a real Postgres; a
// fake that re-implemented it would only be a second, untested version.

// recordingEngine stands in for the Temporal schedules.
type recordingEngine struct {
	registered []string
	paused     []bool
	removed    []string
	err        error
}

func (e *recordingEngine) Register(_ context.Context, routine domain.Routine, _ *time.Location) (string, error) {
	if e.err != nil {
		return "", e.err
	}
	id := "routine-" + routine.ID.String()
	e.registered = append(e.registered, id)
	return id, nil
}

func (e *recordingEngine) SetPaused(_ context.Context, scheduleID string, paused bool, _ string) error {
	if e.err != nil {
		return e.err
	}
	e.paused = append(e.paused, paused)
	return nil
}

func (e *recordingEngine) Remove(_ context.Context, scheduleID string) error {
	if e.err != nil {
		return e.err
	}
	e.removed = append(e.removed, scheduleID)
	return nil
}

// fakeRepository keeps routines and runs in memory.
type fakeRepository struct {
	routines []domain.Routine
	runs     []domain.Run
	agents   []domain.AgentInfo

	// registeredSchedule records the ids the service stored back on the rows.
	schedules map[uuid.UUID]string

	// unreadable makes one routine answer with a fixed error, which is how a test
	// scripts the race a schedule leaves behind: the routine is listed, then deleted,
	// and its tick reads as not-found.
	unreadable map[uuid.UUID]error
}

func newFakeRepository() *fakeRepository {
	return &fakeRepository{schedules: map[uuid.UUID]string{}, unreadable: map[uuid.UUID]error{}}
}

func (f *fakeRepository) Create(_ context.Context, scope domain.Scope, routine domain.Routine) (domain.Routine, error) {
	routine.ID = uuid.New()
	routine.WorkspaceID = scope.WorkspaceID
	routine.CreatedAt = time.Now()
	routine.UpdatedAt = routine.CreatedAt
	f.routines = append(f.routines, routine)
	return routine, nil
}

func (f *fakeRepository) Get(_ context.Context, _ domain.Scope, id uuid.UUID) (domain.Routine, error) {
	if cause, ok := f.unreadable[id]; ok {
		return domain.Routine{}, cause
	}
	for _, routine := range f.routines {
		if routine.ID == id {
			return routine, nil
		}
	}
	return domain.Routine{}, domain.ErrNotFound
}

func (f *fakeRepository) List(context.Context, domain.Scope, domain.Filter) ([]domain.Routine, error) {
	return f.routines, nil
}

func (f *fakeRepository) ListByTrigger(_ context.Context, _ domain.Scope, trigger string) ([]domain.Routine, error) {
	out := []domain.Routine{}
	for _, routine := range f.routines {
		if routine.TriggerSource == trigger && routine.Enabled {
			out = append(out, routine)
		}
	}
	return out, nil
}

func (f *fakeRepository) ListEnabled(context.Context, domain.Scope) ([]domain.Routine, error) {
	out := []domain.Routine{}
	for _, routine := range f.routines {
		if routine.Enabled {
			out = append(out, routine)
		}
	}
	return out, nil
}

func (f *fakeRepository) Update(_ context.Context, _ domain.Scope, routine domain.Routine) (domain.Routine, error) {
	for i, stored := range f.routines {
		if stored.ID != routine.ID {
			continue
		}
		// The schedule id is the engine's, so an update that does not carry one keeps
		// what is stored — which is what the real statement does too.
		routine.ScheduleID = stored.ScheduleID
		if routine.ScheduleID == "" {
			routine.ScheduleID = f.schedules[routine.ID]
		}
		routine.CreatedAt = stored.CreatedAt
		routine.UpdatedAt = time.Now()
		f.routines[i] = routine
		return routine, nil
	}
	return domain.Routine{}, domain.ErrNotFound
}

func (f *fakeRepository) SetScheduleID(_ context.Context, _ domain.Scope, id uuid.UUID, scheduleID string) error {
	f.schedules[id] = scheduleID
	for i, stored := range f.routines {
		if stored.ID == id {
			f.routines[i].ScheduleID = scheduleID
		}
	}
	return nil
}

func (f *fakeRepository) Delete(_ context.Context, _ domain.Scope, id uuid.UUID) error {
	for i, stored := range f.routines {
		if stored.ID != id {
			continue
		}
		f.routines = append(f.routines[:i], f.routines[i+1:]...)
		// The runs survive with their link dropped, which is what ON DELETE SET NULL
		// does in the schema.
		for j, run := range f.runs {
			if run.RoutineID == id {
				f.runs[j].RoutineID = uuid.Nil
			}
		}
		return nil
	}
	return domain.ErrNotFound
}

func (f *fakeRepository) StartRun(_ context.Context, scope domain.Scope, run domain.Run) (domain.Run, error) {
	run.ID = uuid.New()
	run.WorkspaceID = scope.WorkspaceID
	run.StartedAt = time.Now()
	f.runs = append(f.runs, run)
	return run, nil
}

func (f *fakeRepository) FinishRun(_ context.Context, _ domain.Scope, finish domain.FinishRun) (domain.Run, error) {
	for i, run := range f.runs {
		if run.ID != finish.RunID {
			continue
		}
		if run.FinishedAt != nil {
			return run, nil
		}
		finished := finish.FinishedAt
		f.runs[i].Status = finish.Status
		f.runs[i].Summary = finish.Summary
		if finish.TaskID != uuid.Nil {
			f.runs[i].TaskID = finish.TaskID
		}
		f.runs[i].FinishedAt = &finished
		return f.runs[i], nil
	}
	return domain.Run{}, domain.ErrNotFound
}

func (f *fakeRepository) AttachTask(_ context.Context, _ domain.Scope, runID, taskID uuid.UUID) (domain.Run, error) {
	for i, run := range f.runs {
		if run.ID != runID {
			continue
		}
		f.runs[i].TaskID = taskID
		return f.runs[i], nil
	}
	return domain.Run{}, domain.ErrNotFound
}

func (f *fakeRepository) RunByTask(_ context.Context, _ domain.Scope, taskID uuid.UUID) (domain.Run, error) {
	for _, run := range f.runs {
		if run.TaskID == taskID {
			return run, nil
		}
	}
	return domain.Run{}, domain.ErrNotFound
}

func (f *fakeRepository) LatestRun(_ context.Context, _ domain.Scope, routineID uuid.UUID) (domain.Run, error) {
	for i := len(f.runs) - 1; i >= 0; i-- {
		if f.runs[i].RoutineID == routineID {
			return f.runs[i], nil
		}
	}
	return domain.Run{}, domain.ErrNotFound
}

func (f *fakeRepository) ListRuns(context.Context, domain.Scope, domain.RunFilter) ([]domain.Run, error) {
	return f.runs, nil
}

func (f *fakeRepository) Agents(context.Context, domain.Scope) ([]domain.AgentInfo, error) {
	return f.agents, nil
}

// fakeTasks records the tasks a run opened.
type fakeTasks struct {
	opened []domain.StartTask
	err    error
}

func (f *fakeTasks) Start(_ context.Context, _ domain.Scope, request domain.StartTask) (uuid.UUID, error) {
	if f.err != nil {
		return uuid.Nil, f.err
	}
	f.opened = append(f.opened, request)
	return uuid.New(), nil
}

// fakeQueue stands in for the inbound event queue.
type fakeQueue struct {
	events []domain.Event
	err    error
}

func (f *fakeQueue) Pending(context.Context, domain.Scope, string, int) ([]domain.Event, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.events, nil
}

// fixedClock answers the workspace's zone.
type fixedClock struct {
	zone string
}

func (c fixedClock) Location(context.Context, uuid.UUID) (*time.Location, error) {
	return time.LoadLocation(c.zone)
}

func (c fixedClock) WorkingHours(context.Context, uuid.UUID) (domain.WorkingHours, error) {
	return domain.WorkingHours{Start: "08:00", End: "21:00", Timezone: c.zone, WorkDays: []int{1, 2, 3, 4, 5, 6}}, nil
}

// fakeAuditor records the trail.
type fakeAuditor struct {
	entries []domain.AuditEntry
}

func (a *fakeAuditor) Record(_ context.Context, _ domain.Scope, entry domain.AuditEntry) error {
	a.entries = append(a.entries, entry)
	return nil
}

// harness wires the service over the doubles.
type harness struct {
	service *service.Service
	repo    *fakeRepository
	engine  *recordingEngine
	tasks   *fakeTasks
	queue   *fakeQueue
	auditor *fakeAuditor
	scope   domain.Scope
	agentID uuid.UUID
}

func newHarness(t *testing.T, mutate ...func(*service.Deps)) *harness {
	t.Helper()

	repo := newFakeRepository()
	engine := &recordingEngine{}
	tasks := &fakeTasks{}
	queue := &fakeQueue{}
	auditor := &fakeAuditor{}
	agentID := uuid.New()

	repo.agents = []domain.AgentInfo{{ID: agentID, Name: "Oren"}}

	deps := service.Deps{
		Repository: repo,
		Engine:     engine,
		Tasks:      tasks,
		Queue:      queue,
		Auditor:    auditor,
		Clock:      fixedClock{zone: "Asia/Jakarta"},
	}
	for _, change := range mutate {
		change(&deps)
	}

	return &harness{
		service: service.New(deps),
		repo:    repo,
		engine:  engine,
		tasks:   tasks,
		queue:   queue,
		auditor: auditor,
		scope:   domain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()},
		agentID: agentID,
	}
}

// TestCreateRegistersASchedule is E11.1's first gate: a routine that exists always has
// a schedule, and it is stored on the row so the next write can update rather than
// create a second one.
func TestCreateRegistersASchedule(t *testing.T) {
	h := newHarness(t)

	view, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
		AgentID:     h.agentID,
		Name:        "Cek dokumen baru",
		Kind:        domain.KindDaily,
		HourStart:   6,
		Minute:      0,
		Instruction: "Buka dokumen baru dan rapikan.",
		Enabled:     true,
	})
	require.NoError(t, err)

	require.Len(t, h.engine.registered, 1, "exactly one schedule for one routine")
	require.Equal(t, "routine-"+view.ID.String(), h.repo.schedules[view.ID], "the id is stored on the row")
	require.Equal(t, "setiap hari 06:00", view.Schedule().Describe())
	require.NotEmpty(t, view.NextRuns, "a routine answers when it next runs")
	require.Len(t, h.auditor.entries, 1)
	require.Equal(t, domain.ActionCreated, h.auditor.entries[0].Action)
}

// TestATriggerRoutineGetsNoSchedule is E11.2's rule: an event-driven routine has no
// clock, so the engine is given nothing. A cron for something that happens when it
// happens would fire on an empty queue.
func TestATriggerRoutineGetsNoSchedule(t *testing.T) {
	h := newHarness(t)

	view, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
		AgentID:       h.agentID,
		Name:          "Balas email masuk",
		Kind:          domain.KindTrigger,
		TriggerSource: "gmail.message_received",
		Instruction:   "Balas email yang masuk.",
		Enabled:       true,
	})
	require.NoError(t, err)

	require.Empty(t, h.engine.registered, "a trigger routine creates no cron schedule")
	require.Empty(t, view.NextRuns, "and it has no next time to show")
	require.False(t, view.Schedule().Scheduled())
}

// TestATriggerRoutineNeedsItsEvent: a trigger routine that watches nothing would never
// fire, so it is refused rather than stored dead.
func TestATriggerRoutineNeedsItsEvent(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
		AgentID:     h.agentID,
		Name:        "Tanpa kejadian",
		Kind:        domain.KindTrigger,
		Instruction: "Kerjakan sesuatu.",
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
	require.Empty(t, h.repo.routines, "nothing may be stored")
}

// TestATimedRoutineMayNotWatchAnEvent: the two cadences are different things, and a
// routine claiming both would be scheduled *and* fired.
func TestATimedRoutineMayNotWatchAnEvent(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
		AgentID:       h.agentID,
		Name:          "Dua-duanya",
		Kind:          domain.KindDaily,
		HourStart:     6,
		TriggerSource: "gmail.message_received",
		Instruction:   "Kerjakan sesuatu.",
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestAWindowThatEndsBeforeItStartsIsRefused: "21:00–08:00" is a typo in a form, and a
// routine is a schedule rather than a guardrail, so it is refused instead of being read
// as spanning midnight.
func TestAWindowThatEndsBeforeItStartsIsRefused(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
		AgentID:     h.agentID,
		Name:        "Terbalik",
		Kind:        domain.KindHourlyRange,
		HourStart:   21,
		HourEnd:     8,
		Instruction: "Kerjakan sesuatu.",
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestAWeeklyRoutineNeedsAWeekday: without one the engine would fire every day, which
// is not what the form offered.
func TestAWeeklyRoutineNeedsAWeekday(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
		AgentID:     h.agentID,
		Name:        "Mingguan tanpa hari",
		Kind:        domain.KindWeekly,
		HourStart:   7,
		Instruction: "Rekap mingguan.",
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestTheHourAndMinuteAreValidated: a form that posts 99 must be told rather than
// scheduled at an hour the engine rejects at registration time.
func TestTheHourAndMinuteAreValidated(t *testing.T) {
	h := newHarness(t)

	for _, request := range []domain.WriteRequest{
		{AgentID: h.agentID, Name: "Jam aneh", Kind: domain.KindDaily, HourStart: 99, Instruction: "Kerjakan."},
		{AgentID: h.agentID, Name: "Menit aneh", Kind: domain.KindDaily, HourStart: 6, Minute: 99, Instruction: "Kerjakan."},
		{AgentID: h.agentID, Name: "Kind aneh", Kind: "kadang-kadang", Instruction: "Kerjakan."},
		{AgentID: h.agentID, Name: "", Kind: domain.KindDaily, HourStart: 6, Instruction: "Kerjakan."},
		{AgentID: h.agentID, Name: "Tanpa instruksi", Kind: domain.KindDaily, HourStart: 6},
		{AgentID: uuid.Nil, Name: "Tanpa Bolu", Kind: domain.KindDaily, HourStart: 6, Instruction: "Kerjakan."},
	} {
		_, err := h.service.Create(t.Context(), h.scope, request)
		require.ErrorIs(t, err, domain.ErrInvalidInput, "request %q must be refused", request.Name)
	}

	require.Empty(t, h.repo.routines, "nothing may be stored")
	require.Empty(t, h.engine.registered, "and nothing may be scheduled")
}

// TestAnUnreachableEngineFailsTheCreate is why registration is part of the create: a
// routine stored without a schedule silently never runs, which is worse than a failed
// write the user can retry.
func TestAnUnreachableEngineFailsTheCreate(t *testing.T) {
	h := newHarness(t)
	h.engine.err = errors.New("temporal is down")

	_, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
		AgentID:     h.agentID,
		Name:        "Tidak terjadwal",
		Kind:        domain.KindDaily,
		HourStart:   6,
		Instruction: "Kerjakan.",
		Enabled:     true,
	})
	require.ErrorIs(t, err, domain.ErrScheduleUnavailable)
}

// TestPauseKeepsTheScheduleAndTheHistory is E11.5: pausing does not remove the
// routine, and resuming puts it back without a new one.
func TestPauseKeepsTheScheduleAndTheHistory(t *testing.T) {
	h := newHarness(t)

	view, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
		AgentID:     h.agentID,
		Name:        "Cek dokumen",
		Kind:        domain.KindDaily,
		HourStart:   6,
		Instruction: "Kerjakan.",
		Enabled:     true,
	})
	require.NoError(t, err)

	paused, err := h.service.SetEnabled(t.Context(), h.scope, view.ID, false)
	require.NoError(t, err)
	require.False(t, paused.Enabled)
	require.Equal(t, []bool{true}, h.engine.paused, "the schedule is paused, not removed")
	require.Empty(t, h.engine.removed)
	require.Len(t, h.repo.routines, 1, "the routine is still in the list")

	resumed, err := h.service.SetEnabled(t.Context(), h.scope, view.ID, true)
	require.NoError(t, err)
	require.True(t, resumed.Enabled)
	require.Equal(t, []bool{true, false}, h.engine.paused, "resuming unpauses the same schedule")
	require.Len(t, h.engine.registered, 1, "no second schedule was created")

	require.Equal(t, domain.ActionPaused, h.auditor.entries[1].Action)
	require.Equal(t, domain.ActionResumed, h.auditor.entries[2].Action)
}

// TestDeleteRemovesTheScheduleAndKeepsTheRuns is E11.1's second gate.
func TestDeleteRemovesTheScheduleAndKeepsTheRuns(t *testing.T) {
	h := newHarness(t)

	view, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
		AgentID:     h.agentID,
		Name:        "Cek dokumen",
		Kind:        domain.KindDaily,
		HourStart:   6,
		Instruction: "Kerjakan.",
		Enabled:     true,
	})
	require.NoError(t, err)

	// A run happened, so its survival is something to prove.
	_, err = h.service.RunOnce(t.Context(), h.scope, view.ID)
	require.NoError(t, err)
	require.Len(t, h.repo.runs, 1)

	require.NoError(t, h.service.Delete(t.Context(), h.scope, view.ID))

	require.Equal(t, []string{"routine-" + view.ID.String()}, h.engine.removed)
	require.Empty(t, h.repo.routines)
	require.Len(t, h.repo.runs, 1, "the history stays")
	require.Equal(t, uuid.Nil, h.repo.runs[0].RoutineID, "with its link dropped")
	require.Equal(t, "Cek dokumen", h.repo.runs[0].RoutineName, "and its name snapshotted")
}

// TestPreviewAnswersTheNextTimesInTheWorkspaceZone is E11.2's preview gate: the form
// asks before anything is stored.
func TestPreviewAnswersTheNextTimesInTheWorkspaceZone(t *testing.T) {
	h := newHarness(t)

	times, err := h.service.Preview(t.Context(), h.scope, domain.PreviewRequest{
		Kind:      domain.KindDaily,
		HourStart: 6,
		Minute:    0,
	})
	require.NoError(t, err)
	require.Len(t, times, domain.Count)

	jakarta, err := time.LoadLocation("Asia/Jakarta")
	require.NoError(t, err)

	for _, at := range times {
		require.Equal(t, 6, at.In(jakarta).Hour(), "the preview is in the workspace's own zone")
		require.Equal(t, 0, at.In(jakarta).Minute())
	}

	// A cadence the engine could not fire is refused here rather than at registration.
	_, err = h.service.Preview(t.Context(), h.scope, domain.PreviewRequest{Kind: domain.KindHourlyRange, HourStart: 21, HourEnd: 8})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestThePreviewMovesWithTheTimezone is E11.3 at the read boundary: the same request
// asked by a workspace in another zone answers another instant.
func TestThePreviewMovesWithTheTimezone(t *testing.T) {
	jakarta := newHarness(t)

	makassar := newHarness(t, func(deps *service.Deps) {
		deps.Clock = fixedClock{zone: "Asia/Makassar"}
	})

	request := domain.PreviewRequest{Kind: domain.KindDaily, HourStart: 6, Minute: 0}

	inJakarta, err := jakarta.service.Preview(t.Context(), jakarta.scope, request)
	require.NoError(t, err)

	inMakassar, err := makassar.service.Preview(t.Context(), makassar.scope, request)
	require.NoError(t, err)

	require.Len(t, inJakarta, 1+len(inMakassar)-1)
	// One hour apart: 06:00 in Makassar happens an hour earlier in absolute time.
	require.Equal(t, time.Hour, inJakarta[0].Sub(inMakassar[0]))
}

// TestRescheduleRegistersEveryEnabledRoutine is what a timezone change calls.
func TestRescheduleRegistersEveryEnabledRoutine(t *testing.T) {
	h := newHarness(t)

	for _, name := range []string{"Pagi", "Siang", "Sore"} {
		_, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
			AgentID:     h.agentID,
			Name:        name,
			Kind:        domain.KindDaily,
			HourStart:   6,
			Instruction: "Kerjakan.",
			Enabled:     true,
		})
		require.NoError(t, err)
	}

	// One that is paused is not walked: a paused schedule has no next run to move.
	_, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
		AgentID:     h.agentID,
		Name:        "Dijeda",
		Kind:        domain.KindDaily,
		HourStart:   6,
		Instruction: "Kerjakan.",
		Enabled:     false,
	})
	require.NoError(t, err)

	before := len(h.engine.registered)
	require.NoError(t, h.service.Reschedule(t.Context(), h.scope))
	require.Equal(t, before+3, len(h.engine.registered), "the three enabled ones, re-registered")
}

// TestRunOnceSkipsWhenNothingArrived is E11.5's central case and the epic's third
// gate: a trigger routine with an empty queue finishes without a model call.
func TestRunOnceSkipsWhenNothingArrived(t *testing.T) {
	h := newHarness(t)

	view, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
		AgentID:       h.agentID,
		Name:          "Balas email",
		Kind:          domain.KindTrigger,
		TriggerSource: "gmail.message_received",
		Instruction:   "Balas email yang masuk.",
		Enabled:       true,
	})
	require.NoError(t, err)

	run, err := h.service.RunOnce(t.Context(), h.scope, view.ID)
	require.NoError(t, err)

	require.Equal(t, domain.RunSkipped, run.Status)
	require.Equal(t, "tidak ada kejadian baru", run.Summary)
	require.Empty(t, h.tasks.opened, "nothing was asked of the model")
	require.NotNil(t, run.FinishedAt, "the run is closed, not left running")
}

// TestRunOnceDispatchesWhenWorkIsWaiting: the other half of the same check.
func TestRunOnceDispatchesWhenWorkIsWaiting(t *testing.T) {
	h := newHarness(t)

	view, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
		AgentID:       h.agentID,
		Name:          "Balas email",
		Kind:          domain.KindTrigger,
		TriggerSource: "gmail.message_received",
		Instruction:   "Balas email yang masuk.",
		Enabled:       true,
	})
	require.NoError(t, err)

	h.queue.events = []domain.Event{{
		ID:      uuid.New(),
		App:     "gmail",
		Event:   "gmail.message_received",
		Payload: []byte(`{"subject":"Pesanan baru"}`),
	}}

	run, err := h.service.RunOnce(t.Context(), h.scope, view.ID)
	require.NoError(t, err)

	require.Equal(t, domain.RunRunning, run.Status, "the task runs out of process, so the run is still going")
	require.NotEqual(t, uuid.Nil, run.TaskID, "the run carries the task it opened")
	require.Len(t, h.tasks.opened, 1)

	// The event body travels as data, not as an instruction, and the instruction comes
	// first: an email's text is something the Bolu reads.
	prompt := h.tasks.opened[0].Prompt
	require.Contains(t, prompt, "Balas email yang masuk.")
	require.Contains(t, prompt, `<data sumber="gmail.message_received">`)
	require.Contains(t, prompt, "Pesanan baru")
	require.Less(t, indexOfText(prompt, "Balas email yang masuk."), indexOfText(prompt, "<data"))
}

// TestATimedRoutineRunsWithoutWaitingForAnEvent: a scheduled routine's work is its
// instruction, so it does not check a queue it does not watch.
func TestATimedRoutineRunsWithoutWaitingForAnEvent(t *testing.T) {
	h := newHarness(t)

	view, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
		AgentID:     h.agentID,
		Name:        "Cek dokumen",
		Kind:        domain.KindDaily,
		HourStart:   6,
		Instruction: "Rapikan folder dokumen.",
		Enabled:     true,
	})
	require.NoError(t, err)

	run, err := h.service.RunOnce(t.Context(), h.scope, view.ID)
	require.NoError(t, err)

	require.Equal(t, domain.RunRunning, run.Status)
	require.Len(t, h.tasks.opened, 1)
}

// TestARestingBoluTakesNoRoutineRun: the rest switch is the user's, so a routine whose
// Bolu is resting is recorded as skipped rather than run.
func TestARestingBoluTakesNoRoutineRun(t *testing.T) {
	h := newHarness(t)
	h.repo.agents = []domain.AgentInfo{{ID: h.agentID, Name: "Oren", Resting: true}}

	view, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
		AgentID:     h.agentID,
		Name:        "Cek dokumen",
		Kind:        domain.KindDaily,
		HourStart:   6,
		Instruction: "Kerjakan.",
		Enabled:     true,
	})
	require.NoError(t, err)

	run, err := h.service.RunOnce(t.Context(), h.scope, view.ID)
	require.NoError(t, err)

	require.Equal(t, domain.RunSkipped, run.Status)
	require.Equal(t, "Bolu sedang istirahat", run.Summary)
	require.Empty(t, h.tasks.opened)
}

// TestRunOnceRefusesAPausedRoutine: a paused routine's schedule is paused, but a tick
// already in flight still arrives, and it must not run.
func TestRunOnceRefusesAPausedRoutine(t *testing.T) {
	h := newHarness(t)

	view, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
		AgentID:     h.agentID,
		Name:        "Cek dokumen",
		Kind:        domain.KindDaily,
		HourStart:   6,
		Instruction: "Kerjakan.",
		Enabled:     false,
	})
	require.NoError(t, err)

	_, err = h.service.RunOnce(t.Context(), h.scope, view.ID)
	require.ErrorIs(t, err, domain.ErrInvalidInput)
	require.Empty(t, h.tasks.opened)
}

// TestAFailedDispatchIsRecordedAndTheScheduleGoesOn is E11.4's failure rule: the run
// is `failed` with the reason, and the routine is left schedulable.
func TestAFailedDispatchIsRecordedAndTheScheduleGoesOn(t *testing.T) {
	h := newHarness(t)
	h.tasks.err = errors.New("the task runtime is not configured")

	view, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
		AgentID:     h.agentID,
		Name:        "Cek dokumen",
		Kind:        domain.KindDaily,
		HourStart:   6,
		Instruction: "Kerjakan.",
		Enabled:     true,
	})
	require.NoError(t, err)

	run, err := h.service.RunOnce(t.Context(), h.scope, view.ID)
	require.NoError(t, err, "a failed dispatch is the run's outcome, not the caller's error")

	require.Equal(t, domain.RunFailed, run.Status)
	require.Contains(t, run.Summary, "task runtime")

	// The routine is still there and still enabled, so the next tick tries again.
	stored, err := h.service.Routine(t.Context(), h.scope, view.ID)
	require.NoError(t, err)
	require.True(t, stored.Enabled)
}

// TestRecordTaskOutcomeClosesTheRun is E11.4's summary path: the task reports its own
// one line, and that is what the history shows.
func TestRecordTaskOutcomeClosesTheRun(t *testing.T) {
	h := newHarness(t)

	view, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
		AgentID:     h.agentID,
		Name:        "Cek dokumen",
		Kind:        domain.KindDaily,
		HourStart:   6,
		Instruction: "Kerjakan.",
		Enabled:     true,
	})
	require.NoError(t, err)

	run, err := h.service.RunOnce(t.Context(), h.scope, view.ID)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, run.TaskID)

	require.NoError(t, h.service.RecordTaskOutcome(t.Context(), h.scope, run.TaskID, domain.Outcome{
		Status:  "succeeded",
		Summary: "4 file dirapikan",
	}))

	runs, err := h.service.Runs(t.Context(), h.scope, domain.RunFilter{RoutineID: view.ID})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, domain.RunSucceeded, runs[0].Status)
	require.Equal(t, "4 file dirapikan", runs[0].Summary, "the history shows the task's own line")
	require.NotNil(t, runs[0].FinishedAt)
}

// TestRecordTaskOutcomeMapsAFailure: a task that failed makes its run failed, and the
// history reads the task's reason rather than its step count — "batas langkah
// terlampaui" is what a person needs to see, and "0 langkah" is what the task's own
// summary happens to be when it never got started.
func TestRecordTaskOutcomeMapsAFailure(t *testing.T) {
	h := newHarness(t)

	view, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
		AgentID:     h.agentID,
		Name:        "Cek dokumen",
		Kind:        domain.KindDaily,
		HourStart:   6,
		Instruction: "Kerjakan.",
		Enabled:     true,
	})
	require.NoError(t, err)

	run, err := h.service.RunOnce(t.Context(), h.scope, view.ID)
	require.NoError(t, err)
	require.NoError(t, h.service.RecordTaskOutcome(t.Context(), h.scope, run.TaskID, domain.Outcome{
		Status:  "failed",
		Summary: "0 langkah",
		Reason:  "batas langkah terlampaui",
	}))

	runs, _ := h.service.Runs(t.Context(), h.scope, domain.RunFilter{})
	require.Equal(t, domain.RunFailed, runs[0].Status)
	require.Equal(t, "batas langkah terlampaui", runs[0].Summary)
}

// TestAFailureWithoutAReasonKeepsTheSummary: not every failed task carries a reason,
// and a summary that exists is better than an empty line.
func TestAFailureWithoutAReasonKeepsTheSummary(t *testing.T) {
	h := newHarness(t)

	view, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
		AgentID:     h.agentID,
		Name:        "Cek dokumen",
		Kind:        domain.KindDaily,
		HourStart:   6,
		Instruction: "Kerjakan.",
		Enabled:     true,
	})
	require.NoError(t, err)

	run, err := h.service.RunOnce(t.Context(), h.scope, view.ID)
	require.NoError(t, err)
	require.NoError(t, h.service.RecordTaskOutcome(t.Context(), h.scope, run.TaskID, domain.Outcome{
		Status:  "failed",
		Summary: "1 langkah, 1 alat dipakai",
	}))

	runs, _ := h.service.Runs(t.Context(), h.scope, domain.RunFilter{})
	require.Equal(t, "1 langkah, 1 alat dipakai", runs[0].Summary)
}

// TestRecordingTheSameOutcomeTwiceChangesNothing: a retried activity reports twice, and
// the first outcome stands.
func TestRecordingTheSameOutcomeTwiceChangesNothing(t *testing.T) {
	h := newHarness(t)

	view, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
		AgentID:     h.agentID,
		Name:        "Cek dokumen",
		Kind:        domain.KindDaily,
		HourStart:   6,
		Instruction: "Kerjakan.",
		Enabled:     true,
	})
	require.NoError(t, err)

	run, err := h.service.RunOnce(t.Context(), h.scope, view.ID)
	require.NoError(t, err)

	require.NoError(t, h.service.RecordTaskOutcome(t.Context(), h.scope, run.TaskID, domain.Outcome{
		Status:  "succeeded",
		Summary: "yang pertama",
	}))
	require.NoError(t, h.service.RecordTaskOutcome(t.Context(), h.scope, run.TaskID, domain.Outcome{
		Status:  "failed",
		Summary: "yang kedua",
		Reason:  "gagal setelahnya",
	}))

	runs, _ := h.service.Runs(t.Context(), h.scope, domain.RunFilter{})
	require.Len(t, runs, 1)
	require.Equal(t, "yang pertama", runs[0].Summary, "the first outcome stands")
}

// TestATaskNoRoutineOpenedIsNotAnError: most tasks in the product came from chat, and
// the sink has nothing to say about them.
func TestATaskNoRoutineOpenedIsNotAnError(t *testing.T) {
	h := newHarness(t)

	err := h.service.RecordTaskOutcome(t.Context(), h.scope, uuid.New(), domain.Outcome{Status: "succeeded", Summary: "apa saja"})
	require.ErrorIs(t, err, domain.ErrNotFound)
}

// TestFireStartsTheRoutinesWatchingAnEvent: an event is the whole cadence of a trigger
// routine, so this is what starts one.
func TestFireStartsTheRoutinesWatchingAnEvent(t *testing.T) {
	h := newHarness(t)

	for _, name := range []string{"Balas A", "Balas B"} {
		_, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
			AgentID:       h.agentID,
			Name:          name,
			Kind:          domain.KindTrigger,
			TriggerSource: "gmail.message_received",
			Instruction:   "Balas email yang masuk.",
			Enabled:       true,
		})
		require.NoError(t, err)
	}

	h.queue.events = []domain.Event{{ID: uuid.New(), Event: "gmail.message_received", Payload: []byte(`{}`)}}

	started, err := h.service.Fire(t.Context(), h.scope, "gmail.message_received")
	require.NoError(t, err)
	require.Equal(t, 2, started, "both routines watching the event ran")
	require.Len(t, h.tasks.opened, 2)
}

// TestOneRoutineThatCannotBeReadDoesNotStopTheOthers: several routines may watch one
// event, and they are independent. Aborting the dispatch at the first failure would
// leave the routines after it silently unwatched — a delivery that arrived and started
// nothing, with only the failing one visibly wrong. The failure is still reported, so
// the event is not mistaken for one that was acted on.
//
// The failure modelled here is the one that is really reachable: a routine deleted
// between the list and its tick, which reads as not-found.
func TestOneRoutineThatCannotBeReadDoesNotStopTheOthers(t *testing.T) {
	h := newHarness(t)

	broken, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
		AgentID:       h.agentID,
		Name:          "Balas Biru",
		Kind:          domain.KindTrigger,
		TriggerSource: "gmail.message_received",
		Instruction:   "Balas email yang masuk.",
		Enabled:       true,
	})
	require.NoError(t, err)

	working, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
		AgentID:       h.agentID,
		Name:          "Balas Oren",
		Kind:          domain.KindTrigger,
		TriggerSource: "gmail.message_received",
		Instruction:   "Balas email yang masuk.",
		Enabled:       true,
	})
	require.NoError(t, err)

	h.queue.events = []domain.Event{{ID: uuid.New(), Event: "gmail.message_received", Payload: []byte(`{}`)}}
	h.repo.unreadable[broken.ID] = domain.ErrNotFound

	started, err := h.service.Fire(t.Context(), h.scope, "gmail.message_received")
	require.Error(t, err, "a failure is reported rather than hidden")
	require.Contains(t, err.Error(), "Balas Biru", "and it names the routine that failed")
	require.Equal(t, 1, started, "the routine that could run still ran")
	require.Len(t, h.tasks.opened, 1)

	// The routine that could be read has a run on the record. The one that could not
	// has nothing to record against, so it has no run — which is the honest outcome:
	// there is no routine to attribute one to.
	runs, err := h.service.Runs(t.Context(), h.scope, domain.RunFilter{})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, working.Name, runs[0].RoutineName)
	require.Equal(t, domain.RunRunning, runs[0].Status)
}

// TestFireWithNothingWaitingRecordsSkips: the saving is recorded rather than absent, so
// it can be counted.
func TestFireWithNothingWaitingRecordsSkips(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.Create(t.Context(), h.scope, domain.WriteRequest{
		AgentID:       h.agentID,
		Name:          "Balas email",
		Kind:          domain.KindTrigger,
		TriggerSource: "gmail.message_received",
		Instruction:   "Balas email yang masuk.",
		Enabled:       true,
	})
	require.NoError(t, err)

	started, err := h.service.Fire(t.Context(), h.scope, "gmail.message_received")
	require.NoError(t, err)
	require.Zero(t, started, "nothing started, so the count is zero")

	runs, _ := h.service.Runs(t.Context(), h.scope, domain.RunFilter{})
	require.Len(t, runs, 1)
	require.Equal(t, domain.RunSkipped, runs[0].Status)
}

// TestTheWorkspaceIsRequired: a write or read with no tenant is refused rather than run
// against whatever scope the connection happens to carry.
func TestTheWorkspaceIsRequired(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.Create(t.Context(), domain.Scope{}, domain.WriteRequest{
		AgentID: h.agentID, Name: "X", Kind: domain.KindDaily, HourStart: 6, Instruction: "Kerjakan.",
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Routines(t.Context(), domain.Scope{}, domain.Filter{})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Runs(t.Context(), domain.Scope{}, domain.RunFilter{})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Preview(t.Context(), domain.Scope{}, domain.PreviewRequest{Kind: domain.KindDaily})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	require.ErrorIs(t, h.service.Delete(t.Context(), domain.Scope{}, uuid.New()), domain.ErrInvalidInput)
	require.ErrorIs(t, h.service.Reschedule(t.Context(), domain.Scope{}), domain.ErrInvalidInput)
	require.ErrorIs(t, h.service.RecordTaskOutcome(t.Context(), domain.Scope{}, uuid.New(), domain.Outcome{Status: "succeeded"}), domain.ErrInvalidInput)
}

// indexOfText is strings.Index with the arguments in reading order, for the two
// assertions above.
func indexOfText(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
