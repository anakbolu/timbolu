// Package domain defines the routine contract: the cadence a routine runs on, the
// run it produces, and the ports the service and the workflow need.
//
// Two things are deliberately pure here and both are load-bearing:
//
//   - `Schedule` is the cadence, translated from a routine's columns. The preview a
//     form shows and the Temporal Schedule the engine fires come from the *same*
//     value, so "the next run is 06:00" and "the engine will fire at 06:00" cannot
//     disagree — which is the class of bug a routine product cannot survive,
//     because the user cannot see the engine.
//   - `Schedule.NextRuns` computes the next occurrences in pure Go. It is what the
//     API answers a preview with, and it is unit-testable without a Temporal server
//     or a database.
package domain

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Kinds, matching the check constraint on `routines.kind`.
const (
	// KindDaily runs once a day at a fixed time, e.g. "06:00 cek dokumen baru".
	KindDaily = "daily"
	// KindHourlyRange runs every hour inside a window, e.g. "08:00–21:00".
	KindHourlyRange = "hourly_range"
	// KindWeekly runs once a week on one weekday at a fixed time.
	KindWeekly = "weekly"
	// KindTrigger runs when an event arrives rather than at a time. It gets no
	// Temporal Schedule at all: a cron expression for something that happens when
	// it happens would fire on an empty queue.
	KindTrigger = "trigger"
)

// Kinds lists the kinds a write may use, in the order a form offers them.
var Kinds = []string{KindDaily, KindHourlyRange, KindWeekly, KindTrigger}

// Run statuses, matching the check constraint on `routine_runs.status`.
const (
	// RunRunning is a run whose task is still in flight.
	RunRunning = "running"
	// RunSucceeded is a run whose work finished.
	RunSucceeded = "succeeded"
	// RunFailed is a run whose task failed. It is recorded and the schedule keeps
	// firing: one bad run must not stop a routine forever.
	RunFailed = "failed"
	// RunSkipped is a run that found nothing to do and never called a model. It is
	// a success in the product's terms and the largest saving it has.
	RunSkipped = "skipped"
)

// Errors the service and the handler map onto responses.
var (
	ErrNotFound     = errors.New("routine: not found")
	ErrInvalidInput = errors.New("routine: invalid input")
	// ErrScheduleUnavailable means no engine can be reached, so a routine cannot be
	// scheduled. It is a configuration fact a deployment fixes, not a retry.
	ErrScheduleUnavailable = errors.New("routine: the scheduler is not configured")
)

// Scope is the tenant identity a query runs as.
type Scope struct {
	UserID      uuid.UUID
	WorkspaceID uuid.UUID
}

// IsZero reports whether the scope carries no workspace at all.
func (s Scope) IsZero() bool { return s.WorkspaceID == uuid.Nil }

// Routine is one thing a Bolu does on its own.
type Routine struct {
	ID          uuid.UUID `json:"-"`
	WorkspaceID uuid.UUID `json:"-"`
	AgentID     uuid.UUID `json:"-"`
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	// Minute, HourStart, and HourEnd are wall-clock times in the workspace's own
	// zone as the schedule document describes them: a daily routine uses HourStart,
	// an hourly range uses both ends inclusive, and a weekly one adds DayOfWeek.
	Minute    int `json:"minute"`
	HourStart int `json:"hour_start"`
	HourEnd   int `json:"hour_end"`
	// DayOfWeek is an ISO weekday (1 = Monday … 7 = Sunday), 0 when the kind does
	// not use one. It matches `workspaces.work_days` so the two cannot disagree
	// about what "Wednesday" is.
	DayOfWeek int `json:"day_of_week,omitempty"`
	// TriggerSource is the event this routine watches, as `app.thing`. Non-empty
	// makes the routine event-driven: it waits for the event instead of a clock.
	TriggerSource string `json:"trigger_source,omitempty"`
	Instruction   string `json:"instruction"`
	Enabled       bool   `json:"enabled"`
	// PausedReason explains a routine the engine stopped, in the user's words.
	PausedReason string `json:"paused_reason,omitempty"`
	// ScheduleID is the engine's id for this routine's schedule. One schedule per
	// routine is what makes create-or-update idempotent, and the database enforces
	// the uniqueness rather than the service hoping for it.
	ScheduleID string    `json:"-"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Run is one execution of a routine.
type Run struct {
	ID          uuid.UUID `json:"-"`
	WorkspaceID uuid.UUID `json:"-"`
	// RoutineID is zero once the routine that produced this run has been deleted:
	// the link is dropped so the history can stay.
	RoutineID uuid.UUID `json:"-"`
	// RoutineName is the routine's name at the time it ran. The history reads from
	// this rather than from a join, which is what keeps a deleted routine's runs
	// readable.
	RoutineName string     `json:"routine_name"`
	TaskID      uuid.UUID  `json:"-"`
	Status      string     `json:"status"`
	Summary     string     `json:"summary"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

// Schedule is a routine's cadence, on its own.
//
// It is a value rather than a method over the columns so the same translation can
// serve three readers: the preview the API answers, the Temporal spec the engine
// fires, and the tests between them. A second translation living anywhere else is
// how a screen comes to promise a time the engine never fires.
type Schedule struct {
	Kind      string
	Minute    int
	HourStart int
	HourEnd   int
	DayOfWeek int
}

// Schedule renders a routine's cadence.
func (r Routine) Schedule() Schedule {
	return Schedule{
		Kind:      r.Kind,
		Minute:    r.Minute,
		HourStart: r.HourStart,
		HourEnd:   r.HourEnd,
		DayOfWeek: r.DayOfWeek,
	}
}

// Scheduled reports whether the cadence has times at all.
//
// A trigger routine does not: it is started by an event, so the engine is given no
// schedule for it. Registration is skipped rather than given a cron that fires on
// nothing, which would spend a task per hour to find an empty queue.
func (s Schedule) Scheduled() bool { return s.Kind != KindTrigger }

// Describe renders the cadence in the user's words, which is what a history line
// says about when a routine runs.
func (s Schedule) Describe() string {
	switch s.Kind {
	case KindDaily:
		return fmt.Sprintf("setiap hari %02d:%02d", s.HourStart, s.Minute)
	case KindHourlyRange:
		return fmt.Sprintf("tiap jam %02d:%02d–%02d:%02d", s.HourStart, s.Minute, s.HourEnd, s.Minute)
	case KindWeekly:
		return fmt.Sprintf("setiap %s %02d:%02d", weekdayName(s.DayOfWeek), s.HourStart, s.Minute)
	case KindTrigger:
		return "saat ada kejadian"
	}
	return s.Kind
}

// NextRuns returns the next occurrences after `from`, in order.
//
// The computation is in the workspace's own location, because "06:00" in a routine
// means six in the morning where the business is — not in UTC and not where the
// server happens to run. A timezone change therefore moves every future run, and
// that is the whole of E11.3: there is no cached time to invalidate, because the
// times are computed from the routine and the location each time they are asked
// for.
//
// A trigger routine has no times, so the answer is empty rather than an error: "it
// does not run on a clock" is a fact about it, not a failure.
func (s Schedule) NextRuns(location *time.Location, from time.Time, count int) []time.Time {
	if count <= 0 || !s.Scheduled() {
		return nil
	}
	if location == nil {
		location = time.UTC
	}

	local := from.In(location)
	runs := make([]time.Time, 0, count)

	switch s.Kind {
	case KindDaily:
		day := time.Date(local.Year(), local.Month(), local.Day(), s.HourStart, s.Minute, 0, 0, location)
		for len(runs) < count {
			if day.After(local) {
				runs = append(runs, day)
			}
			// Adding a day through the calendar rather than by 24 hours is what
			// makes a DST change land on the right wall-clock time.
			day = day.AddDate(0, 0, 1)
		}

	case KindHourlyRange:
		// Two days are enough for any window: the last hour of one day and the
		// first of the next are both reachable within it.
		for offset := range 2 {
			day := local.AddDate(0, 0, offset)
			for hour := s.HourStart; hour <= s.HourEnd && len(runs) < count; hour++ {
				candidate := time.Date(day.Year(), day.Month(), day.Day(), hour, s.Minute, 0, 0, location)
				if candidate.After(local) {
					runs = append(runs, candidate)
				}
			}
			if len(runs) >= count {
				break
			}
		}

	case KindWeekly:
		// The wanted weekday may be today, and today may already be past its hour, so
		// the search walks forward to the first matching day and then steps a week at a
		// time. Stepping by the calendar rather than by 7×24 hours is what keeps the
		// wall-clock time across a daylight-saving change.
		day := local
		for missed := 0; missed < 7; missed++ {
			if isoWeekday(day.Weekday()) == s.DayOfWeek {
				candidate := time.Date(day.Year(), day.Month(), day.Day(), s.HourStart, s.Minute, 0, 0, location)
				if candidate.After(local) {
					break
				}
			}
			day = day.AddDate(0, 0, 1)
		}

		for len(runs) < count {
			runs = append(runs, time.Date(day.Year(), day.Month(), day.Day(), s.HourStart, s.Minute, 0, 0, location))
			day = day.AddDate(0, 0, 7)
		}
	}

	return runs
}

// NextRun returns the first occurrence, or the zero time.
func (s Schedule) NextRun(location *time.Location, from time.Time) time.Time {
	runs := s.NextRuns(location, from, 1)
	if len(runs) == 0 {
		return time.Time{}
	}
	return runs[0]
}

// isoWeekday renders a Go weekday as the ISO number (1 = Monday … 7 = Sunday),
// which is the form the database and the API both use.
func isoWeekday(day time.Weekday) int {
	if day == time.Sunday {
		return 7
	}
	return int(day)
}

func weekdayName(iso int) string {
	switch iso {
	case 1:
		return "Senin"
	case 2:
		return "Selasa"
	case 3:
		return "Rabu"
	case 4:
		return "Kamis"
	case 5:
		return "Jumat"
	case 6:
		return "Sabtu"
	case 7:
		return "Minggu"
	}
	return "hari yang tidak dikenal"
}
