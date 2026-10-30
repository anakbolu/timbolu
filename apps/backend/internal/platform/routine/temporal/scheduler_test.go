package temporal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/routine/domain"
)

// E11.2's verification is this file: the four cadences, translated into the schedule
// spec Temporal actually fires. It is a unit test because the translation is pure —
// a cadence in, a spec out — and because a cadence rendered into the wrong spec is
// the defect that reaches production silently: the routine runs, just not when
// anybody said it would.
//
// The spec is read field by field rather than compared whole, because a spec is a
// structure of ranges and a diff of one says nothing about which field moved.

// calendarOf renders one routine's cadence as a single Temporal calendar.
func calendarOf(t *testing.T, routine domain.Routine, location *time.Location) (client.ScheduleCalendarSpec, string) {
	t.Helper()

	spec, err := specFor(routine.Schedule(), location)
	require.NoError(t, err)
	require.Len(t, spec.Calendars, 1, "every cadence this deployment offers is one calendar")

	return spec.Calendars[0], spec.TimeZoneName
}

// values expands a list of Temporal ranges into the numbers it covers, which is what
// an assertion can name.
func values(ranges []client.ScheduleRange) []int {
	out := []int{}
	for _, span := range ranges {
		for value := span.Start; value <= span.End; value++ {
			out = append(out, value)
		}
	}
	return out
}

func routineOf(kind string, mutate ...func(*domain.Routine)) domain.Routine {
	routine := domain.Routine{
		ID:          uuid.New(),
		WorkspaceID: uuid.New(),
		AgentID:     uuid.New(),
		Name:        "Cek dokumen",
		Kind:        kind,
		Enabled:     true,
	}
	for _, change := range mutate {
		change(&routine)
	}
	return routine
}

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()

	location, err := time.LoadLocation(name)
	require.NoError(t, err)
	return location
}

// TestADailyRoutineFiresAtItsOwnMinute is the first cadence: "06:30 cek dokumen baru"
// must not become 06:00 sharp, and it must not fire every minute of the hour. The
// minute column exists on the routine precisely so this is expressible; a spec that
// ignored it would look right in the UI and run at the wrong time.
func TestADailyRoutineFiresAtItsOwnMinute(t *testing.T) {
	routine := routineOf(domain.KindDaily, func(r *domain.Routine) {
		r.HourStart = 6
		r.Minute = 30
	})

	calendar, zone := calendarOf(t, routine, mustLocation(t, "Asia/Jakarta"))

	require.Equal(t, []int{30}, values(calendar.Minute), "the minute is the routine's own, not the top of the hour")
	require.Equal(t, []int{6}, values(calendar.Hour))

	// An empty list is a wildcard in a Temporal calendar spec, which is why the
	// fields a cadence does not constrain are left empty rather than enumerated.
	require.Empty(t, calendar.DayOfMonth, "every day of the month")
	require.Empty(t, calendar.Month, "every month")
	require.Empty(t, calendar.DayOfWeek, "every day of the week")
	require.Empty(t, calendar.Second, "a minute-resolution cadence does not name a second")

	require.Equal(t, "Asia/Jakarta", zone, "the workspace's zone, not the server's")
	require.Equal(t, "setiap hari 06:30", calendar.Comment, "the Temporal UI shows the cadence in words")
}

// TestAnHourlyRangeCoversBothEndsInclusively: "08.00–21.00" means the 21:00 run
// happens. An off-by-one here is a routine that silently never runs its last slot of
// the day, which nobody notices until the day it matters.
func TestAnHourlyRangeCoversBothEndsInclusively(t *testing.T) {
	routine := routineOf(domain.KindHourlyRange, func(r *domain.Routine) {
		r.HourStart = 8
		r.HourEnd = 21
		r.Minute = 30
	})

	calendar, _ := calendarOf(t, routine, mustLocation(t, "Asia/Jakarta"))

	require.Equal(t, []int{8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21},
		values(calendar.Hour), "both ends of the window are included")
	require.Equal(t, []int{30}, values(calendar.Minute))
	require.Empty(t, calendar.DayOfWeek, "every day of the week")
	require.Equal(t, "tiap jam 08:30–21:30", calendar.Comment)
}

// TestASingleHourWindowStillRenders: a one-hour window is a legitimate cadence, and
// the loop that builds the ranges must not lose it.
func TestASingleHourWindowStillRenders(t *testing.T) {
	routine := routineOf(domain.KindHourlyRange, func(r *domain.Routine) {
		r.HourStart = 9
		r.HourEnd = 9
	})

	calendar, _ := calendarOf(t, routine, mustLocation(t, "Asia/Jakarta"))

	require.Equal(t, []int{9}, values(calendar.Hour))
	require.Equal(t, []int{0}, values(calendar.Minute))
}

// TestAWeeklyRoutineLandsOnTheStatedWeekday is the translation that is easiest to get
// wrong, because the two sides number the week differently: the domain counts from
// Monday (ISO, matching the form and the database), and Temporal counts from Sunday.
// A routine set to Wednesday must fire on Wednesday, not Tuesday.
func TestAWeeklyRoutineLandsOnTheStatedWeekday(t *testing.T) {
	// The mapping is stated here again on purpose: if either side's numbering ever
	// changed, this table is what would catch it.
	for _, tc := range []struct {
		iso      int
		temporal []int
		day      string
	}{
		{1, []int{1}, "Senin"},
		{2, []int{2}, "Selasa"},
		{3, []int{3}, "Rabu"},
		{4, []int{4}, "Kamis"},
		{5, []int{5}, "Jumat"},
		{6, []int{6}, "Sabtu"},
		{7, []int{0}, "Minggu"},
	} {
		routine := routineOf(domain.KindWeekly, func(r *domain.Routine) {
			r.HourStart = 7
			r.Minute = 15
			r.DayOfWeek = tc.iso
		})

		calendar, _ := calendarOf(t, routine, mustLocation(t, "Asia/Jakarta"))

		require.Equal(t, tc.temporal, values(calendar.DayOfWeek), tc.day)
		require.Equal(t, []int{7}, values(calendar.Hour), tc.day)
		require.Equal(t, []int{15}, values(calendar.Minute), tc.day)
		require.Empty(t, calendar.DayOfMonth, "every week of the month")
		require.Equal(t, "setiap "+tc.day+" 07:15", calendar.Comment, tc.day)
	}
}

// TestATriggerRoutineHasNoSpec: a routine started by an event must not be renderable
// as a cron. If it were, it would fire on an empty queue and spend tokens on nothing —
// and having no spec at all is what makes that impossible rather than merely unlikely.
func TestATriggerRoutineHasNoSpec(t *testing.T) {
	routine := routineOf(domain.KindTrigger, func(r *domain.Routine) {
		r.TriggerSource = "gmail.email_received"
	})

	_, err := specFor(routine.Schedule(), mustLocation(t, "Asia/Jakarta"))
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestAnUnknownCadenceIsRefused: a row holding a kind this build does not know must
// be refused rather than rendered into a spec that fires at some default time.
func TestAnUnknownCadenceIsRefused(t *testing.T) {
	routine := routineOf("every_fortnight")

	_, err := specFor(routine.Schedule(), mustLocation(t, "Asia/Jakarta"))
	require.ErrorIs(t, err, domain.ErrInvalidInput)
	require.Contains(t, err.Error(), "every_fortnight", "the refusal names the kind it did not understand")
}

// TestTheZoneFollowsTheWorkspace is E11.3 at the engine's own boundary: the spec
// carries the workspace's zone name, so Temporal fires "06:00 in Jakarta" and
// re-registering after a zone change is all it takes to move every future run. There
// is no stored time to invalidate.
func TestTheZoneFollowsTheWorkspace(t *testing.T) {
	routine := routineOf(domain.KindDaily, func(r *domain.Routine) { r.HourStart = 6 })

	jakartaSpec, err := specFor(routine.Schedule(), mustLocation(t, "Asia/Jakarta"))
	require.NoError(t, err)
	require.Equal(t, "Asia/Jakarta", jakartaSpec.TimeZoneName)

	makassarSpec, err := specFor(routine.Schedule(), mustLocation(t, "Asia/Makassar"))
	require.NoError(t, err)
	require.Equal(t, "Asia/Makassar", makassarSpec.TimeZoneName)

	// The cadence itself is untouched: only the zone moved. That is the seam E11.3
	// turns on — the routine says "06:00", and the zone says which 06:00.
	require.Equal(t, jakartaSpec.Calendars[0].Hour, makassarSpec.Calendars[0].Hour)
	require.Equal(t, jakartaSpec.Calendars[0].Minute, makassarSpec.Calendars[0].Minute)
}

// TestTheScheduleIdIsDerivedFromTheRoutine is E11.1's idempotence at its root: the id
// is a function of the routine, so registering the same routine twice addresses the
// same schedule. A random id would leave an orphan behind on every re-registration,
// and two schedules fire one routine twice — the double execution the epic forbids.
func TestTheScheduleIdIsDerivedFromTheRoutine(t *testing.T) {
	routineID := uuid.New()

	require.Equal(t, "routine-"+routineID.String(), ScheduleID(routineID))
	require.Equal(t, ScheduleID(routineID), ScheduleID(routineID), "the same routine always addresses the same schedule")

	other := uuid.New()
	require.NotEqual(t, ScheduleID(routineID), ScheduleID(other), "two routines never share one schedule")
	require.True(t, len(ScheduleID(routineID)) > len(Prefix), "the id starts with the prefix and names its routine")
}

// TestWithoutTemporalNothingCanBeScheduled is the module's "runnable without a
// scheduler" contract at the engine's own edge: every call says so rather than
// dereferencing nil, which is what lets the API serve its reads and its history in a
// deployment with no Temporal.
func TestWithoutTemporalNothingCanBeScheduled(t *testing.T) {
	routine := routineOf(domain.KindDaily, func(r *domain.Routine) { r.HourStart = 6 })

	for _, tc := range []struct {
		name string
		call func(*Scheduler) error
	}{
		{"register", func(s *Scheduler) error {
			_, err := s.Register(context.Background(), routine, mustLocation(t, "Asia/Jakarta"))
			return err
		}},
		{"pause", func(s *Scheduler) error { return s.SetPaused(context.Background(), "routine-x", true, "dijeda") }},
		{"remove", func(s *Scheduler) error { return s.Remove(context.Background(), "routine-x") }},
		{"exists", func(s *Scheduler) error { _, err := s.Exists(context.Background(), "routine-x"); return err }},
		{"next run", func(s *Scheduler) error { _, err := s.NextRun(context.Background(), "routine-x"); return err }},
	} {
		for _, scheduler := range []*Scheduler{nil, {}} {
			err := tc.call(scheduler)
			require.Error(t, err, tc.name)
			require.ErrorIs(t, err, domain.ErrScheduleUnavailable, tc.name)
		}
	}
}

// TestActingOnNoScheduleIsANoOp: a routine whose schedule id is empty has nothing to
// pause or delete, and refusing would make such a routine impossible to pause — which
// happens for a trigger routine, and for one created before an engine existed.
func TestActingOnNoScheduleIsANoOp(t *testing.T) {
	scheduler := &Scheduler{}

	require.NoError(t, scheduler.SetPaused(context.Background(), "", true, "dijeda"))
	require.NoError(t, scheduler.Remove(context.Background(), ""))
}

// TestIsNotFoundOnlyAnswersForANotFound: the helper decides whether a describe or a
// delete is the caller's goal already met. Answering yes for any error would swallow
// an outage as "the schedule is gone", which is how a routine silently stops running
// and nobody is told.
func TestIsNotFoundOnlyAnswersForANotFound(t *testing.T) {
	require.False(t, isNotFound(nil))
	require.False(t, isNotFound(errors.New("connection refused")))
	require.False(t, isNotFound(context.DeadlineExceeded))
}

// TestTemporalNumbersTheWeekFromSunday states the one conversion in the file, so a
// change to it has to be deliberate: everything else here projects the domain's own
// fields.
func TestTemporalNumbersTheWeekFromSunday(t *testing.T) {
	require.Equal(t, 0, temporalWeekday(7), "ISO Sunday is Temporal 0")
	for iso := 1; iso <= 6; iso++ {
		require.Equal(t, iso, temporalWeekday(iso), "Monday to Saturday keep their numbers")
	}
}
