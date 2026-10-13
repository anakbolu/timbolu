// Package temporal adapts the Temporal schedule API to the routine module's
// engine port.
//
// It lives in the platform layer rather than in the module because the module must
// stay runnable without a Temporal server: the API answers its reads and its
// history, and only the writes that need a schedule report that none can be
// registered. The package knows the SDK; the module knows only the port.
package temporal

import (
	"context"
	"errors"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/google/uuid"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/domain"
	platformtemporal "github.com/robzlabz/employeebot/apps/backend/internal/platform/temporal"
)

// Scheduler registers the schedules a routine is fired by.
type Scheduler struct {
	client *platformtemporal.Client
	queue  string
	// workflowName is the routine workflow's registered name. It is a parameter
	// rather than an import because the module that owns the workflow is not this
	// package's to reach into: the container knows both and passes the name.
	workflowName string
}

// NewScheduler builds the adapter. The queue is the agent worker's, which is where
// the routine workflow is registered.
func NewScheduler(client *platformtemporal.Client, queue, workflowName string) *Scheduler {
	return &Scheduler{client: client, queue: queue, workflowName: workflowName}
}

// scheduleTimeout bounds one routine run. It is longer than a task's own timeout
// because a routine may open a task and wait for it inside the same run.
const scheduleTimeout = 2 * time.Hour

// Prefix is what every schedule id this deployment creates begins with, so a
// schedule created for a routine is recognisable in the Temporal UI and a test can
// count them.
const Prefix = "routine-"

// Register creates or replaces one routine's schedule.
//
// It is create-or-update against the id derived from the routine, which is what
// makes it idempotent: registering the same routine twice leaves exactly one
// schedule. The id is derived rather than random so a routine whose schedule id was
// lost (a restored database, say) is repaired by registering it again instead of
// leaving an orphan behind.
func (s *Scheduler) Register(ctx context.Context, routine domain.Routine, location *time.Location) (string, error) {
	if s == nil || s.client == nil || s.client.Client() == nil {
		return "", fmt.Errorf("%w: temporal is not configured", domain.ErrScheduleUnavailable)
	}

	spec, err := specFor(routine.Schedule(), location)
	if err != nil {
		return "", err
	}

	scheduleID := ScheduleID(routine.ID)
	options := client.ScheduleOptions{
		ID:   scheduleID,
		Spec: spec,
		Action: &client.ScheduleWorkflowAction{
			ID:       scheduleID,
			Workflow: s.workflowName,
			Args: []any{domain.RunInput{
				RoutineID:   routine.ID.String(),
				WorkspaceID: routine.WorkspaceID.String(),
			}},
			TaskQueue: s.queue,
			// One run at a time. A routine that is still working when its next slot
			// arrives is skipped rather than piled on: the overlap policy is what
			// implements "never two runs for one execution slot".
			WorkflowExecutionTimeout: scheduleTimeout,
		},
		Overlap: enumspb.SCHEDULE_OVERLAP_POLICY_SKIP,
		// A short catch-up window, so a restarted server does not replay a day of
		// missed runs at once. One missed slot is worth running late; a week of
		// them is a storm the workspace did not ask for.
		CatchupWindow: time.Hour,
		// A schedule that cannot fire must be visible rather than silently dead.
		PauseOnFailure: false,
		Note:           routine.Name,
	}

	policies := client.SchedulePolicies{Overlap: options.Overlap, CatchupWindow: options.CatchupWindow}
	state := client.ScheduleState{Note: routine.Name}

	handle := s.client.Client().ScheduleClient().GetHandle(ctx, scheduleID)
	if err := handle.Update(ctx, client.ScheduleUpdateOptions{
		DoUpdate: func(client.ScheduleUpdateInput) (*client.ScheduleUpdate, error) {
			return &client.ScheduleUpdate{
				Schedule: &client.Schedule{
					Action: options.Action,
					Spec:   &options.Spec,
					Policy: &policies,
					State:  &state,
				},
			}, nil
		},
	}); err == nil {
		return scheduleID, nil
	}

	if _, err := s.client.Client().ScheduleClient().Create(ctx, options); err != nil {
		return "", fmt.Errorf("routine: register schedule: %w", err)
	}

	return scheduleID, nil
}

// SetPaused pauses or resumes a schedule without removing it.
//
// A schedule that is not there is not an error on resume: the routine's row is what
// the caller is changing, and the next registration recreates the schedule.
func (s *Scheduler) SetPaused(ctx context.Context, scheduleID string, paused bool, reason string) error {
	if scheduleID == "" {
		// There is no schedule to pause. It is the same answer as one that was
		// already gone — the routine's row is what the caller is changing — and it
		// is checked before the client so a routine that was never registered can
		// still be paused rather than reported as an unreachable scheduler.
		return nil
	}
	if s == nil || s.client == nil || s.client.Client() == nil {
		return fmt.Errorf("%w: temporal is not configured", domain.ErrScheduleUnavailable)
	}

	handle := s.client.Client().ScheduleClient().GetHandle(ctx, scheduleID)

	if paused {
		if err := handle.Pause(ctx, client.SchedulePauseOptions{Note: reason}); err != nil {
			return fmt.Errorf("routine: pause schedule: %w", err)
		}
		return nil
	}

	if err := handle.Unpause(ctx, client.ScheduleUnpauseOptions{Note: reason}); err != nil {
		return fmt.Errorf("routine: resume schedule: %w", err)
	}
	return nil
}

// Remove deletes a schedule. One that is already gone is not an error.
func (s *Scheduler) Remove(ctx context.Context, scheduleID string) error {
	if scheduleID == "" {
		// Nothing was ever registered, so there is nothing to delete. It is the
		// caller's goal already met, for the same reason a schedule that is already
		// gone is not an error.
		return nil
	}
	if s == nil || s.client == nil || s.client.Client() == nil {
		return fmt.Errorf("%w: temporal is not configured", domain.ErrScheduleUnavailable)
	}

	if err := s.client.Client().ScheduleClient().GetHandle(ctx, scheduleID).Delete(ctx); err != nil {
		// Deleting a schedule that does not exist is the caller's goal already met.
		// Reporting it would make a routine whose schedule was lost undeletable.
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("routine: delete schedule: %w", err)
	}
	return nil
}

// Exists reports whether a schedule id is present, which is what a test asserts the
// engine's state with.
func (s *Scheduler) Exists(ctx context.Context, scheduleID string) (bool, error) {
	if s == nil || s.client == nil || s.client.Client() == nil {
		return false, fmt.Errorf("%w: temporal is not configured", domain.ErrScheduleUnavailable)
	}

	if _, err := s.client.Client().ScheduleClient().GetHandle(ctx, scheduleID).Describe(ctx); err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ScheduleID is the deterministic id of one routine's schedule.
//
// Derived from the routine rather than random, so registering a routine whose
// schedule id was lost repairs it instead of leaving an orphan schedule behind.
func ScheduleID(routineID uuid.UUID) string { return Prefix + routineID.String() }

// NextRun reads the next time the engine will fire a schedule, which is how a test
// proves that a timezone change moved it.
func (s *Scheduler) NextRun(ctx context.Context, scheduleID string) (time.Time, error) {
	if s == nil || s.client == nil || s.client.Client() == nil {
		return time.Time{}, fmt.Errorf("%w: temporal is not configured", domain.ErrScheduleUnavailable)
	}

	description, err := s.client.Client().ScheduleClient().GetHandle(ctx, scheduleID).Describe(ctx)
	if err != nil {
		return time.Time{}, err
	}
	times := description.Info.NextActionTimes
	if len(times) == 0 {
		return time.Time{}, nil
	}
	return times[0], nil
}

// specFor renders a cadence as the Temporal schedule spec.
//
// The translation lives here rather than in the module for one reason: the spec type
// is the SDK's, and the module is not allowed to import it. The *decisions* it
// encodes are the module's — `domain.Schedule` already says which fields a kind uses
// and what the next times are — so this function only projects them.
func specFor(schedule domain.Schedule, location *time.Location) (client.ScheduleSpec, error) {
	if !schedule.Scheduled() {
		return client.ScheduleSpec{}, fmt.Errorf("%w: a trigger routine has no schedule", domain.ErrInvalidInput)
	}

	zone := "UTC"
	if location != nil {
		zone = location.String()
	}

	spec := client.ScheduleSpec{
		// The zone is the workspace's own. This is the whole of E11.3: the engine
		// fires "06:00 in Jakarta", so changing the workspace's timezone moves every
		// future run without touching the routine.
		//
		// Each calendar carries the cadence in words as its Comment, which is what
		// the Temporal UI shows beside the schedule.
		TimeZoneName: zone,
	}

	switch schedule.Kind {
	case domain.KindDaily:
		spec.Calendars = []client.ScheduleCalendarSpec{{
			Minute:  []client.ScheduleRange{{Start: schedule.Minute, End: schedule.Minute}},
			Hour:    []client.ScheduleRange{{Start: schedule.HourStart, End: schedule.HourStart}},
			Comment: schedule.Describe(),
		}}

	case domain.KindHourlyRange:
		hours := make([]client.ScheduleRange, 0, schedule.HourEnd-schedule.HourStart+1)
		for hour := schedule.HourStart; hour <= schedule.HourEnd; hour++ {
			hours = append(hours, client.ScheduleRange{Start: hour, End: hour})
		}
		spec.Calendars = []client.ScheduleCalendarSpec{{
			Minute:  []client.ScheduleRange{{Start: schedule.Minute, End: schedule.Minute}},
			Hour:    hours,
			Comment: schedule.Describe(),
		}}

	case domain.KindWeekly:
		spec.Calendars = []client.ScheduleCalendarSpec{{
			Minute: []client.ScheduleRange{{Start: schedule.Minute, End: schedule.Minute}},
			Hour:   []client.ScheduleRange{{Start: schedule.HourStart, End: schedule.HourStart}},
			// Temporal numbers the week from Sunday. The domain numbers it from
			// Monday, matching the database and the form, so the conversion is
			// here — and it is the one place a weekday can be lost.
			DayOfWeek: []client.ScheduleRange{{Start: temporalWeekday(schedule.DayOfWeek), End: temporalWeekday(schedule.DayOfWeek)}},
			Comment:   schedule.Describe(),
		}}

	default:
		return client.ScheduleSpec{}, fmt.Errorf("%w: unknown kind %q", domain.ErrInvalidInput, schedule.Kind)
	}

	return spec, nil
}

// temporalWeekday converts an ISO weekday (1 = Monday … 7 = Sunday) to the
// Sunday-first numbering the Temporal calendar spec uses (0 = Sunday … 6 = Saturday).
func temporalWeekday(iso int) int {
	if iso == 7 {
		return 0
	}
	return iso
}

// isNotFound reports whether a schedule is simply not there, which for describe and
// delete is the caller's goal already met rather than a failure.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}

	var notFound *serviceerror.NotFound
	return errors.As(err, &notFound)
}
