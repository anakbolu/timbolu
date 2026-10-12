// Package service implements the routine use cases: what a routine is, when it
// runs, what one run did, and how the engine is kept in step with the rows.
//
// Three rules run through the package:
//
//   - a routine that exists always has a schedule. Creating one registers the
//     schedule before it is reported as created, so a routine that could not be
//     scheduled is a failed write rather than a silent one that never runs.
//   - the times a screen shows and the times the engine fires come from one
//     translation (`domain.Schedule`). A second calculation anywhere is how a form
//     comes to promise a time the engine never fires.
//   - a run that finds nothing to do is `skipped` and costs no tokens. That is the
//     product's largest saving, so it is a first-class outcome rather than an
//     absence of one.
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/domain"
)

// nameMaxRunes bounds a routine's name, which is shown in a list and in a history
// line.
const nameMaxRunes = 120

// instructionMaxRunes bounds what a Bolu is told to do. It is a prompt, and a
// prompt nobody can read is one nobody can fix.
const instructionMaxRunes = 2000

// Deps are the service dependencies.
//
// The engine and the task starter are optional in the sense that the module still
// serves its reads, its history, and its deletes without them: only the writes that
// need a schedule or a task report that they cannot.
type Deps struct {
	Repository domain.Repository
	Engine     domain.Engine
	Tasks      domain.TaskStarter
	Queue      domain.WorkQueue
	Auditor    domain.Auditor
	Clock      domain.WorkspaceClock
	Logger     *zap.Logger
	// Now is the time source, so a preview is testable without waiting.
	Now func() time.Time
}

// Service is the routine implementation.
type Service struct {
	deps Deps
	now  func() time.Time
}

// New builds the service.
func New(deps Deps) *Service {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &Service{deps: deps, now: now}
}

// Routines returns the workspace's routines, each with its next run times.
//
// The workspace's zone is read once for the whole list rather than per row: the
// times are what the screen shows beside every routine, and one location is one
// answer for all of them.
func (s *Service) Routines(ctx context.Context, scope domain.Scope, filter domain.Filter) ([]domain.View, error) {
	if scope.IsZero() {
		return nil, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	routines, err := s.deps.Repository.List(ctx, scope, filter)
	if err != nil {
		return nil, err
	}

	location, err := s.location(ctx, scope)
	if err != nil {
		return nil, err
	}

	views := make([]domain.View, 0, len(routines))
	for _, routine := range routines {
		views = append(views, s.view(routine, location))
	}
	return views, nil
}

// Routine returns one routine with its next run times.
func (s *Service) Routine(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.View, error) {
	if scope.IsZero() {
		return domain.View{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	routine, err := s.deps.Repository.Get(ctx, scope, id)
	if err != nil {
		return domain.View{}, err
	}

	location, err := s.location(ctx, scope)
	if err != nil {
		return domain.View{}, err
	}

	return s.view(routine, location), nil
}

// view renders one routine with its next run times.
func (s *Service) view(routine domain.Routine, location *time.Location) domain.View {
	return domain.View{
		Routine:  routine,
		NextRuns: routine.Schedule().NextRuns(location, s.now(), domain.Count),
	}
}

// RoutinesFor returns the enabled routines watching one event.
func (s *Service) RoutinesFor(ctx context.Context, scope domain.Scope, triggerSource string) ([]domain.Routine, error) {
	if scope.IsZero() {
		return nil, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}
	if strings.TrimSpace(triggerSource) == "" {
		return nil, fmt.Errorf("%w: trigger source is required", domain.ErrInvalidInput)
	}

	return s.deps.Repository.ListByTrigger(ctx, scope, strings.TrimSpace(triggerSource))
}

// Runs returns the run history.
func (s *Service) Runs(ctx context.Context, scope domain.Scope, filter domain.RunFilter) ([]domain.Run, error) {
	if scope.IsZero() {
		return nil, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	return s.deps.Repository.ListRuns(ctx, scope, filter)
}

// Preview answers the next run times for a cadence that has not been saved.
//
// It reads the workspace's own timezone, because that is what the times mean: a
// preview computed in UTC would be wrong for every workspace in Jakarta, which is
// every workspace the product has.
func (s *Service) Preview(ctx context.Context, scope domain.Scope, request domain.PreviewRequest) ([]time.Time, error) {
	if scope.IsZero() {
		return nil, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	location, err := s.location(ctx, scope)
	if err != nil {
		return nil, err
	}

	schedule, err := s.validate(domain.WriteRequest{
		Kind:      request.Kind,
		Minute:    request.Minute,
		HourStart: request.HourStart,
		HourEnd:   request.HourEnd,
		DayOfWeek: request.DayOfWeek,
	})
	if err != nil {
		return nil, err
	}

	return schedule.NextRuns(location, s.now(), domain.Count), nil
}

// ------------------------------------------------------------------ writes

// Create stores a routine and registers its schedule.
func (s *Service) Create(ctx context.Context, scope domain.Scope, request domain.WriteRequest) (domain.View, error) {
	if scope.IsZero() {
		return domain.View{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}
	if request.AgentID == uuid.Nil {
		return domain.View{}, fmt.Errorf("%w: a routine belongs to a bolu", domain.ErrInvalidInput)
	}

	schedule, err := s.validate(request)
	if err != nil {
		return domain.View{}, err
	}

	name, err := s.acceptName(request.Name)
	if err != nil {
		return domain.View{}, err
	}

	instruction, err := s.acceptInstruction(request.Instruction)
	if err != nil {
		return domain.View{}, err
	}

	location, err := s.location(ctx, scope)
	if err != nil {
		return domain.View{}, err
	}

	stored, err := s.deps.Repository.Create(ctx, scope, domain.Routine{
		AgentID:       request.AgentID,
		Name:          name,
		Kind:          schedule.Kind,
		Minute:        schedule.Minute,
		HourStart:     schedule.HourStart,
		HourEnd:       schedule.HourEnd,
		DayOfWeek:     schedule.DayOfWeek,
		TriggerSource: strings.TrimSpace(request.TriggerSource),
		Instruction:   instruction,
		Enabled:       request.Enabled,
	})
	if err != nil {
		return domain.View{}, err
	}

	registered, err := s.register(ctx, scope, stored, location)
	if err != nil {
		return domain.View{}, err
	}

	s.audit(ctx, scope, domain.AuditEntry{
		Action:     domain.ActionCreated,
		ObjectType: "routine",
		ObjectID:   registered.ID.String(),
		Metadata: map[string]any{
			"kind":      registered.Kind,
			"schedule":  registered.Schedule().Describe(),
			"scheduled": registered.Schedule().Scheduled(),
		},
	})

	return s.view(registered, location), nil
}

// Update stores a change and re-registers the schedule.
func (s *Service) Update(ctx context.Context, scope domain.Scope, id uuid.UUID, request domain.WriteRequest) (domain.View, error) {
	if scope.IsZero() {
		return domain.View{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	stored, err := s.deps.Repository.Get(ctx, scope, id)
	if err != nil {
		return domain.View{}, err
	}

	schedule, err := s.validate(request)
	if err != nil {
		return domain.View{}, err
	}

	name, err := s.acceptName(request.Name)
	if err != nil {
		return domain.View{}, err
	}

	instruction, err := s.acceptInstruction(request.Instruction)
	if err != nil {
		return domain.View{}, err
	}

	agentID := request.AgentID
	if agentID == uuid.Nil {
		agentID = stored.AgentID
	}

	location, err := s.location(ctx, scope)
	if err != nil {
		return domain.View{}, err
	}

	updated, err := s.deps.Repository.Update(ctx, scope, domain.Routine{
		ID:        stored.ID,
		AgentID:   agentID,
		Name:      name,
		Kind:      schedule.Kind,
		Minute:    schedule.Minute,
		HourStart: schedule.HourStart,
		HourEnd:   schedule.HourEnd,
		DayOfWeek: schedule.DayOfWeek,
		// Paused reason is cleared by an edit: the change is the answer to it.
		PausedReason:  "",
		TriggerSource: strings.TrimSpace(request.TriggerSource),
		Instruction:   instruction,
		Enabled:       request.Enabled,
	})
	if err != nil {
		return domain.View{}, err
	}

	registered, err := s.register(ctx, scope, updated, location)
	if err != nil {
		return domain.View{}, err
	}

	s.audit(ctx, scope, domain.AuditEntry{
		Action:     domain.ActionUpdated,
		ObjectType: "routine",
		ObjectID:   registered.ID.String(),
		Metadata: map[string]any{
			"kind":     registered.Kind,
			"schedule": registered.Schedule().Describe(),
		},
	})

	return s.view(registered, location), nil
}

// SetEnabled pauses or resumes a routine's schedule.
//
// Pausing keeps the schedule and the history: the routine is still in the list,
// with its runs, and resuming puts it back without a new one. That is the difference
// between pausing and deleting, and it is why this goes through the engine rather
// than through the row.
func (s *Service) SetEnabled(ctx context.Context, scope domain.Scope, id uuid.UUID, enabled bool) (domain.View, error) {
	if scope.IsZero() {
		return domain.View{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	stored, err := s.deps.Repository.Get(ctx, scope, id)
	if err != nil {
		return domain.View{}, err
	}

	location, err := s.location(ctx, scope)
	if err != nil {
		return domain.View{}, err
	}

	updated, err := s.deps.Repository.Update(ctx, scope, domain.Routine{
		ID:            stored.ID,
		AgentID:       stored.AgentID,
		Name:          stored.Name,
		Kind:          stored.Kind,
		Minute:        stored.Minute,
		HourStart:     stored.HourStart,
		HourEnd:       stored.HourEnd,
		DayOfWeek:     stored.DayOfWeek,
		TriggerSource: stored.TriggerSource,
		Instruction:   stored.Instruction,
		Enabled:       enabled,
		PausedReason:  "",
	})
	if err != nil {
		return domain.View{}, err
	}

	if err := s.sync(ctx, scope, updated, location); err != nil {
		return domain.View{}, err
	}

	action := domain.ActionPaused
	if enabled {
		action = domain.ActionResumed
	}
	s.audit(ctx, scope, domain.AuditEntry{
		Action:     action,
		ObjectType: "routine",
		ObjectID:   updated.ID.String(),
		Metadata:   map[string]any{"kind": updated.Kind},
	})

	return s.view(updated, location), nil
}

// Delete removes a routine and its schedule, keeping its runs.
func (s *Service) Delete(ctx context.Context, scope domain.Scope, id uuid.UUID) error {
	if scope.IsZero() {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	stored, err := s.deps.Repository.Get(ctx, scope, id)
	if err != nil {
		return err
	}

	// The schedule goes first. A row deleted while its schedule still fires would
	// leave the engine starting workflows for a routine nothing can look up, which
	// is a run that fails forever.
	if err := s.remove(ctx, stored.ScheduleID); err != nil {
		return err
	}

	if err := s.deps.Repository.Delete(ctx, scope, id); err != nil {
		return err
	}

	// The runs survive: their link is dropped and their name was snapshotted, so
	// "what did this routine do last month" is still answerable.
	s.audit(ctx, scope, domain.AuditEntry{
		Action:     domain.ActionDeleted,
		ObjectType: "routine",
		ObjectID:   id.String(),
		Metadata:   map[string]any{"kind": stored.Kind, "name": stored.Name},
	})

	return nil
}

// Reschedule re-registers every enabled routine of the workspace.
//
// It is what a timezone or working-hours change calls. The next run moves by
// itself, because the times are computed from the routine and the location each
// time they are asked for — this walks the routines so the *engine* is told, not so
// a cached time is refreshed. There is no cache to invalidate.
func (s *Service) Reschedule(ctx context.Context, scope domain.Scope) error {
	if scope.IsZero() {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}
	if s.deps.Engine == nil {
		return nil
	}

	routines, err := s.deps.Repository.ListEnabled(ctx, scope)
	if err != nil {
		return err
	}

	location, err := s.location(ctx, scope)
	if err != nil {
		return err
	}

	var failures []string
	for _, routine := range routines {
		if _, err := s.register(ctx, scope, routine, location); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", routine.Name, err))
		}
	}

	if len(failures) > 0 {
		return fmt.Errorf("%w: %s", domain.ErrScheduleUnavailable, strings.Join(failures, "; "))
	}

	return nil
}

// ------------------------------------------------------------------- runs

// Fire starts the routines watching an event, and reports how many started.
//
// It is the only thing that starts a trigger routine: such a routine has no
// schedule, so an event is the whole of its cadence. It goes through RunOnce, so a
// fired routine records a run exactly the way a scheduled one does — one path, not
// two, which is what keeps "every execution produces one run" true for both.
func (s *Service) Fire(ctx context.Context, scope domain.Scope, triggerSource string) (int, error) {
	routines, err := s.RoutinesFor(ctx, scope, triggerSource)
	if err != nil {
		return 0, err
	}

	started := 0
	var failure error

	// Every routine watching this event is attempted, and one that fails does not
	// stop the others: they are independent, and a delivery is one event that
	// several routines may have asked for. Aborting the loop would leave the
	// routines after the failing one silently unwatched, which is the harder
	// failure to notice of the two.
	for _, routine := range routines {
		run, err := s.RunOnce(ctx, scope, routine.ID)
		if err != nil {
			if failure == nil {
				failure = fmt.Errorf("%s: %w", routine.Name, err)
			}
			continue
		}
		// Only a run that actually opened its task counts as started. A skipped run
		// found nothing to do and a failed one wrote its reason instead of opening a
		// task; neither did the work the caller is asking about.
		if run.Status == domain.RunRunning {
			started++
		}
	}

	return started, failure
}

// RunOnce executes one routine now, which is what the engine calls on each tick of
// a schedule.
//
// The cheap check comes first (E11.5): a routine that watches events looks at the
// queue before anything else happens, and a queue with nothing waiting finishes the
// run as `skipped` without a model call. The saving is recorded as a run rather than
// as an absence, so it can be counted.
//
// The run stays `running` after this returns. Its task runs out of process, and the
// summary the history shows is the task's own — written when the task reports back
// through RecordTaskOutcome. Finishing it here would mean inventing a summary from
// the fact that a task was opened.
func (s *Service) RunOnce(ctx context.Context, scope domain.Scope, routineID uuid.UUID) (domain.Run, error) {
	if scope.IsZero() {
		return domain.Run{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	routine, err := s.deps.Repository.Get(ctx, scope, routineID)
	if err != nil {
		return domain.Run{}, err
	}
	if !routine.Enabled {
		return domain.Run{}, fmt.Errorf("%w: the routine is paused", domain.ErrInvalidInput)
	}

	pending := s.claimPending(ctx, scope, routine)

	// Why a run did nothing, when it did nothing. Each of these is a real saving:
	// no event arrived, or the Bolu is not taking work, and either way no model is
	// called and no token is spent.
	if routine.Schedule().Kind == domain.KindTrigger && pending.claimed == 0 {
		return s.recordSkip(ctx, scope, routine, "tidak ada kejadian baru")
	}

	agents := s.agentNames(ctx, scope)
	if info, ok := agents[routine.AgentID]; ok && info.Resting {
		return s.recordSkip(ctx, scope, routine, "Bolu sedang istirahat")
	}

	run, err := s.deps.Repository.StartRun(ctx, scope, domain.Run{
		RoutineID:   routine.ID,
		RoutineName: routine.Name,
		Status:      domain.RunRunning,
	})
	if err != nil {
		return domain.Run{}, err
	}

	if s.deps.Tasks == nil {
		return s.failRun(ctx, scope, run, errors.New("the task runtime is not configured"))
	}

	taskID, err := s.deps.Tasks.Start(ctx, scope, domain.StartTask{
		AgentID: routine.AgentID,
		Title:   routine.Name,
		Prompt:  s.prompt(routine, pending),
	})
	if err != nil {
		return s.failRun(ctx, scope, run, err)
	}

	// The link is written before the run is reported as dispatched, so a task that
	// finished immediately still finds the run it belongs to.
	attached, err := s.deps.Repository.AttachTask(ctx, scope, run.ID, taskID)
	if err != nil {
		return domain.Run{}, err
	}

	return attached, nil
}

// RecordTaskOutcome finishes the run a task belongs to.
//
// It is why the history shows a one-line summary rather than the routine's own
// reasoning: the task already writes that line onto itself, and this copies it where
// the routine's history reads it.
//
// A task no routine opened answers ErrNotFound, which the caller ignores: most tasks
// in the product were opened by chat.
func (s *Service) RecordTaskOutcome(ctx context.Context, scope domain.Scope, taskID uuid.UUID, outcome domain.Outcome) error {
	if scope.IsZero() {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	run, err := s.deps.Repository.RunByTask(ctx, scope, taskID)
	if err != nil {
		return err
	}
	if run.FinishedAt != nil {
		// Already recorded, which a retried activity does on purpose. The first
		// outcome stands.
		return nil
	}

	runStatus := domain.RunSucceeded
	if outcome.Status != "" && outcome.Status != domain.RunSucceeded {
		runStatus = domain.RunFailed
	}

	// A run that failed reads as the task's own reason rather than as its step
	// count: "batas langkah terlampaui" is what a person needs, and "0 langkah" is
	// what the task's summary happens to be when it never got started.
	summary := outcome.Summary
	if runStatus == domain.RunFailed && strings.TrimSpace(outcome.Reason) != "" {
		summary = outcome.Reason
	}

	_, err = s.deps.Repository.FinishRun(ctx, scope, domain.FinishRun{
		RunID:      run.ID,
		Status:     runStatus,
		Summary:    summary,
		TaskID:     taskID,
		FinishedAt: s.now(),
	})
	return err
}

// recordSkip writes a run that found nothing to do, without opening a task.
func (s *Service) recordSkip(ctx context.Context, scope domain.Scope, routine domain.Routine, reason string) (domain.Run, error) {
	run, err := s.deps.Repository.StartRun(ctx, scope, domain.Run{
		RoutineID:   routine.ID,
		RoutineName: routine.Name,
		Status:      domain.RunSkipped,
	})
	if err != nil {
		return domain.Run{}, err
	}

	return s.deps.Repository.FinishRun(ctx, scope, domain.FinishRun{
		RunID:      run.ID,
		Status:     domain.RunSkipped,
		Summary:    reason,
		FinishedAt: s.now(),
	})
}

// failRun records a run whose task could not be opened. The schedule keeps firing:
// one bad run must not stop a routine forever.
func (s *Service) failRun(ctx context.Context, scope domain.Scope, run domain.Run, cause error) (domain.Run, error) {
	finished, err := s.deps.Repository.FinishRun(ctx, scope, domain.FinishRun{
		RunID:      run.ID,
		Status:     domain.RunFailed,
		Summary:    cause.Error(),
		FinishedAt: s.now(),
	})
	if err != nil {
		return domain.Run{}, err
	}

	if s.deps.Logger != nil {
		s.deps.Logger.Warn("a routine run could not open its task",
			zap.String("routine_id", run.RoutineID.String()), zap.Error(cause))
	}

	return finished, nil
}

// claimed is what the cheap check consumed.
type claimed struct {
	events  []domain.Event
	claimed int
}

// claimPending reads and claims the events waiting for one routine.
//
// It is one step rather than two: the queue's read marks the events as taken, so two
// routines watching one event cannot both act on it. A routine that watches nothing
// — or a deployment with no queue wired — claims nothing, and then the run is judged
// on its own instructions.
func (s *Service) claimPending(ctx context.Context, scope domain.Scope, routine domain.Routine) claimed {
	if s.deps.Queue == nil || routine.TriggerSource == "" {
		return claimed{}
	}

	events, err := s.deps.Queue.Pending(ctx, scope, routine.TriggerSource, pendingLimit)
	if err != nil {
		if s.deps.Logger != nil {
			s.deps.Logger.Warn("could not read the events a routine watches; it will run without them",
				zap.String("routine_id", routine.ID.String()), zap.Error(err))
		}
		return claimed{}
	}

	return claimed{events: events, claimed: len(events)}
}

// pendingLimit bounds how many events one run acts on, so one delivery storm does
// not become one unbounded prompt.
const pendingLimit = 20

// prompt renders what the Bolu is asked to do.
//
// The instruction comes first and the event bodies after it, marked as data: an
// email's text is something the Bolu reads, not something it is told, and the
// difference is the same guardrail the retrieval block uses.
func (s *Service) prompt(routine domain.Routine, pending claimed) string {
	parts := []string{routine.Instruction}
	if len(pending.events) > 0 {
		parts = append(parts, fmt.Sprintf("Ada %d kejadian menunggu:", len(pending.events)))
		for _, event := range pending.events {
			parts = append(parts, fmt.Sprintf("<data sumber=%q>\n%s\n</data>", event.Event, string(event.Payload)))
		}
	}
	parts = append(parts, "Kerjakan rutinitas ini dan simpulkan hasilnya dalam satu kalimat.")

	return strings.Join(parts, "\n")
}

// ----------------------------------------------------------------- helpers

// register makes the engine's schedules match one routine, and stores the id.
//
// It is create-or-update rather than create: the same routine registered twice is
// one schedule, which is the epic's first gate. A routine whose cadence has no
// times gets no schedule at all and its stored id is cleared, so a trigger routine
// that used to be daily does not leave a daily schedule behind.
func (s *Service) register(ctx context.Context, scope domain.Scope, routine domain.Routine, location *time.Location) (domain.Routine, error) {
	// A routine with no schedule needs no engine: a trigger routine's cadence is
	// the event that starts it, so demanding a scheduler here would make
	// "setiap ada email masuk" unsavable in a deployment without one — for a reason
	// that has nothing to do with the routine. What such a routine may have left
	// behind is a schedule it no longer needs, so that is removed and its id cleared.
	if !routine.Enabled || !routine.Schedule().Scheduled() {
		if err := s.remove(ctx, routine.ScheduleID); err != nil {
			return domain.Routine{}, err
		}
		if err := s.deps.Repository.SetScheduleID(ctx, scope, routine.ID, ""); err != nil {
			return domain.Routine{}, err
		}
		routine.ScheduleID = ""
		return routine, nil
	}

	if s.deps.Engine == nil {
		return domain.Routine{}, fmt.Errorf("%w: no engine is configured", domain.ErrScheduleUnavailable)
	}

	scheduleID, err := s.deps.Engine.Register(ctx, routine, location)
	if err != nil {
		return domain.Routine{}, fmt.Errorf("%w: %w", domain.ErrScheduleUnavailable, err)
	}

	if scheduleID != routine.ScheduleID {
		if err := s.deps.Repository.SetScheduleID(ctx, scope, routine.ID, scheduleID); err != nil {
			return domain.Routine{}, err
		}
		routine.ScheduleID = scheduleID
	}

	return routine, nil
}

// sync makes one routine's schedule match its enabled state, for the pause path.
func (s *Service) sync(ctx context.Context, scope domain.Scope, routine domain.Routine, location *time.Location) error {
	if s.deps.Engine == nil {
		return fmt.Errorf("%w: no engine is configured", domain.ErrScheduleUnavailable)
	}

	if !routine.Schedule().Scheduled() {
		// A trigger routine has nothing to pause: its cadence is the event.
		return nil
	}

	if routine.ScheduleID == "" {
		// Never registered, which happens when the routine was created before an
		// engine existed. Registering it now is what the user's click asked for.
		if !routine.Enabled {
			return nil
		}
		_, err := s.register(ctx, scope, routine, location)
		return err
	}

	return s.deps.Engine.SetPaused(ctx, routine.ScheduleID, !routine.Enabled, "dijeda dari layar Rutinitas")
}

func (s *Service) remove(ctx context.Context, scheduleID string) error {
	if s.deps.Engine == nil || scheduleID == "" {
		return nil
	}
	return s.deps.Engine.Remove(ctx, scheduleID)
}

// validate turns a write into a cadence, refusing one the engine could not fire.
func (s *Service) validate(request domain.WriteRequest) (domain.Schedule, error) {
	schedule := domain.Schedule{
		Kind:      request.Kind,
		Minute:    request.Minute,
		HourStart: request.HourStart,
		HourEnd:   request.HourEnd,
		DayOfWeek: request.DayOfWeek,
	}

	if !knownKind(schedule.Kind) {
		return domain.Schedule{}, fmt.Errorf("%w: unknown kind %q", domain.ErrInvalidInput, schedule.Kind)
	}
	if schedule.Minute < 0 || schedule.Minute > 59 {
		return domain.Schedule{}, fmt.Errorf("%w: minute %d is not a minute of the hour", domain.ErrInvalidInput, schedule.Minute)
	}

	switch schedule.Kind {
	case domain.KindDaily, domain.KindWeekly:
		if schedule.HourStart < 0 || schedule.HourStart > 23 {
			return domain.Schedule{}, fmt.Errorf("%w: hour %d is not an hour of the day", domain.ErrInvalidInput, schedule.HourStart)
		}
	case domain.KindHourlyRange:
		if schedule.HourStart < 0 || schedule.HourStart > 23 || schedule.HourEnd < 0 || schedule.HourEnd > 23 {
			return domain.Schedule{}, fmt.Errorf("%w: the window must be between 00 and 23", domain.ErrInvalidInput)
		}
		if schedule.HourEnd < schedule.HourStart {
			// A window that ends before it starts is a typo rather than a night
			// shift: a routine is a schedule, not a guardrail, so it is refused
			// instead of being read as spanning midnight.
			return domain.Schedule{}, fmt.Errorf("%w: the window ends before it starts", domain.ErrInvalidInput)
		}
	}

	if schedule.Kind == domain.KindWeekly && (schedule.DayOfWeek < 1 || schedule.DayOfWeek > 7) {
		return domain.Schedule{}, fmt.Errorf("%w: a weekly routine needs a weekday", domain.ErrInvalidInput)
	}
	if schedule.Kind == domain.KindTrigger && strings.TrimSpace(request.TriggerSource) == "" {
		return domain.Schedule{}, fmt.Errorf("%w: a trigger routine needs the event it watches", domain.ErrInvalidInput)
	}
	if schedule.Kind != domain.KindTrigger && strings.TrimSpace(request.TriggerSource) != "" {
		return domain.Schedule{}, fmt.Errorf("%w: only a trigger routine watches an event", domain.ErrInvalidInput)
	}

	return schedule, nil
}

func (s *Service) acceptName(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("%w: the routine needs a name", domain.ErrInvalidInput)
	}
	if len([]rune(trimmed)) > nameMaxRunes {
		return "", fmt.Errorf("%w: the name is longer than %d characters", domain.ErrInvalidInput, nameMaxRunes)
	}
	return trimmed, nil
}

func (s *Service) acceptInstruction(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("%w: the routine needs an instruction", domain.ErrInvalidInput)
	}
	if len([]rune(trimmed)) > instructionMaxRunes {
		return "", fmt.Errorf("%w: the instruction is longer than %d characters", domain.ErrInvalidInput, instructionMaxRunes)
	}
	return trimmed, nil
}

// location reads the workspace's timezone, falling back to its configured name
// when the module that owns the column is not wired.
func (s *Service) location(ctx context.Context, scope domain.Scope) (*time.Location, error) {
	if s.deps.Clock == nil {
		return time.UTC, nil
	}

	location, err := s.deps.Clock.Location(ctx, scope.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if location == nil {
		return time.UTC, nil
	}
	return location, nil
}

// agentNames maps the Bolu of a workspace to their names and switches, so a
// routine's label and its dispatch gate both read one lookup.
func (s *Service) agentNames(ctx context.Context, scope domain.Scope) map[uuid.UUID]domain.AgentInfo {
	if s.deps.Repository == nil {
		return nil
	}

	agents, err := s.deps.Repository.Agents(ctx, scope)
	if err != nil {
		return nil
	}

	byID := make(map[uuid.UUID]domain.AgentInfo, len(agents))
	for _, agent := range agents {
		byID[agent.ID] = agent
	}
	return byID
}

func (s *Service) audit(ctx context.Context, scope domain.Scope, entry domain.AuditEntry) {
	if s.deps.Auditor == nil {
		return
	}

	entry.ActorUserID = scope.UserID
	if err := s.deps.Auditor.Record(ctx, scope, entry); err != nil && s.deps.Logger != nil {
		s.deps.Logger.Warn("could not record a routine change in the audit trail",
			zap.String("action", entry.Action), zap.Error(err))
	}
}

func knownKind(kind string) bool {
	for _, candidate := range domain.Kinds {
		if candidate == kind {
			return true
		}
	}
	return false
}
