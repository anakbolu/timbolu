package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/domain"
)

// nextRuns is a shorthand: the times a cadence next runs at, in one zone.
func nextRuns(t *testing.T, schedule domain.Schedule, zone string, from time.Time, count int) []time.Time {
	t.Helper()

	location, err := time.LoadLocation(zone)
	require.NoError(t, err)

	return schedule.NextRuns(location, from, count)
}

// TestDailyRunsAtTheStatedTimeInTheWorkspaceZone is the E11.2 gate for the simplest
// cadence: "06:00" means six in the morning where the business is.
func TestDailyRunsAtTheStatedTimeInTheWorkspaceZone(t *testing.T) {
	jakarta, err := time.LoadLocation("Asia/Jakarta")
	require.NoError(t, err)

	schedule := domain.Schedule{Kind: domain.KindDaily, HourStart: 6, Minute: 0}
	// 08:00 Jakarta, so today's 06:00 has passed and the next one is tomorrow.
	from := time.Date(2026, 3, 2, 8, 0, 0, 0, jakarta)

	runs := nextRuns(t, schedule, "Asia/Jakarta", from, 3)
	require.Len(t, runs, 3)

	require.Equal(t, 6, runs[0].In(jakarta).Hour())
	require.Equal(t, 3, runs[0].In(jakarta).Day(), "today's six has passed, so it is tomorrow")
	require.Equal(t, 4, runs[1].In(jakarta).Day())
	require.Equal(t, 5, runs[2].In(jakarta).Day())

	// The same instant read in UTC is 23:00 the day before, which is exactly why the
	// zone has to travel with the cadence.
	require.Equal(t, 23, runs[0].UTC().Hour())
	require.Equal(t, 2, runs[0].UTC().Day())
}

// TestDailyRunTodayWhenItHasNotPassedYet: the hour is inclusive of the moment it
// becomes that time.
func TestDailyRunTodayWhenItHasNotPassedYet(t *testing.T) {
	jakarta, err := time.LoadLocation("Asia/Jakarta")
	require.NoError(t, err)

	schedule := domain.Schedule{Kind: domain.KindDaily, HourStart: 18, Minute: 30}
	from := time.Date(2026, 3, 2, 8, 0, 0, 0, jakarta)

	runs := nextRuns(t, schedule, "Asia/Jakarta", from, 1)
	require.Len(t, runs, 1)
	require.Equal(t, 2, runs[0].In(jakarta).Day(), "18:30 today is still ahead")
	require.Equal(t, 18, runs[0].In(jakarta).Hour())
	require.Equal(t, 30, runs[0].In(jakarta).Minute())
}

// TestHourlyRangeCoversBothEnds is the "08:00–21:00" cadence: every hour in the
// window, inclusive of the last one.
func TestHourlyRangeCoversBothEnds(t *testing.T) {
	jakarta, err := time.LoadLocation("Asia/Jakarta")
	require.NoError(t, err)

	schedule := domain.Schedule{Kind: domain.KindHourlyRange, HourStart: 8, HourEnd: 21, Minute: 0}
	from := time.Date(2026, 3, 2, 7, 30, 0, 0, jakarta)

	runs := nextRuns(t, schedule, "Asia/Jakarta", from, 15)
	require.Len(t, runs, 15)

	// The first is 08:00 and the fifteenth is 22:00? No: the window ends at 21:00, so
	// the hours of one day are 08..21 inclusive — fourteen — and the fifteenth is the
	// next day's 08:00.
	require.Equal(t, 8, runs[0].In(jakarta).Hour())
	require.Equal(t, 2, runs[0].In(jakarta).Day())
	require.Equal(t, 21, runs[13].In(jakarta).Hour())
	require.Equal(t, 2, runs[13].In(jakarta).Day(), "the last hour of the window is the same day")
	require.Equal(t, 8, runs[14].In(jakarta).Hour())
	require.Equal(t, 3, runs[14].In(jakarta).Day(), "and the next one is tomorrow's first hour")
}

// TestHourlyRangeSkipsTheHoursAlreadyPast: a routine added at 10:15 does not run at
// 08:00 and 09:00 retroactively.
func TestHourlyRangeSkipsTheHoursAlreadyPast(t *testing.T) {
	jakarta, err := time.LoadLocation("Asia/Jakarta")
	require.NoError(t, err)

	schedule := domain.Schedule{Kind: domain.KindHourlyRange, HourStart: 8, HourEnd: 21, Minute: 0}
	from := time.Date(2026, 3, 2, 10, 15, 0, 0, jakarta)

	runs := nextRuns(t, schedule, "Asia/Jakarta", from, 2)
	require.Equal(t, 11, runs[0].In(jakarta).Hour(), "the next whole hour, not the window's start")
	require.Equal(t, 12, runs[1].In(jakarta).Hour())
}

// TestWeeklyLandsOnTheStatedIsoWeekday is the weekly cadence, and it is the one place
// a weekday numbering can be lost: the domain counts from Monday the way the database
// and the form do.
func TestWeeklyLandsOnTheStatedIsoWeekday(t *testing.T) {
	jakarta, err := time.LoadLocation("Asia/Jakarta")
	require.NoError(t, err)

	// 2026-03-02 is a Monday, so ISO weekday 3 is Wednesday the 4th.
	from := time.Date(2026, 3, 2, 9, 0, 0, 0, jakarta)
	require.Equal(t, time.Monday, from.Weekday())

	schedule := domain.Schedule{Kind: domain.KindWeekly, HourStart: 7, Minute: 0, DayOfWeek: 3}
	runs := nextRuns(t, schedule, "Asia/Jakarta", from, 3)

	require.Len(t, runs, 3)
	for i, run := range runs {
		require.Equal(t, time.Wednesday, run.In(jakarta).Weekday())
		require.Equal(t, 7, run.In(jakarta).Hour())
		require.Equal(t, 4+7*i, run.In(jakarta).Day(), "one Wednesday a week")
	}
}

// TestWeeklyPicksTheNextWeekWhenTheDayHasPassed: asking on the day itself, after its
// hour, gives next week's.
func TestWeeklyPicksTheNextWeekWhenTheDayHasPassed(t *testing.T) {
	jakarta, err := time.LoadLocation("Asia/Jakarta")
	require.NoError(t, err)

	schedule := domain.Schedule{Kind: domain.KindWeekly, HourStart: 7, Minute: 0, DayOfWeek: 1}
	from := time.Date(2026, 3, 2, 9, 0, 0, 0, jakarta)

	runs := nextRuns(t, schedule, "Asia/Jakarta", from, 1)
	require.Equal(t, 9, runs[0].In(jakarta).Day(), "Monday the 9th, not the 2nd")
}

// TestATriggerRoutineHasNoTimes is E11.2's other half: a routine started by an event
// does not run on a clock, so the engine is given no schedule for it.
func TestATriggerRoutineHasNoTimes(t *testing.T) {
	schedule := domain.Schedule{Kind: domain.KindTrigger}

	require.False(t, schedule.Scheduled())
	require.Empty(t, schedule.NextRuns(time.UTC, time.Now(), 5))
	require.True(t, schedule.NextRun(time.UTC, time.Now()).IsZero())
}

// TestATimezoneChangeMovesEveryFutureRun is the E11.3 gate, stated as the epic states
// it: Jakarta to Makassar shifts the next run by an hour.
//
// Nothing is cached, so there is nothing to invalidate: the same routine asked in
// another zone answers another instant, and that is the whole mechanism.
func TestATimezoneChangeMovesEveryFutureRun(t *testing.T) {
	schedule := domain.Schedule{Kind: domain.KindDaily, HourStart: 6, Minute: 0}
	from := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)

	jakarta := nextRuns(t, schedule, "Asia/Jakarta", from, 1)[0]
	makassar := nextRuns(t, schedule, "Asia/Makassar", from, 1)[0]

	// Makassar is one hour ahead, so its 06:00 is one hour earlier in absolute time.
	require.Equal(t, time.Hour, jakarta.Sub(makassar))
	require.Equal(t, 23, jakarta.UTC().Hour(), "06:00 Jakarta is 23:00 UTC the day before")
	require.Equal(t, 22, makassar.UTC().Hour(), "06:00 Makassar is 22:00 UTC the day before")
}

// TestADaylightSavingChangeKeepsTheWallClockTime is why the arithmetic goes through
// the calendar rather than through 24 hours. In a zone that shifts, adding a day to an
// instant lands an hour off; adding a day to a date does not.
func TestADaylightSavingChangeKeepsTheWallClockTime(t *testing.T) {
	// Europe/London moves its clocks on 2026-03-29.
	london, err := time.LoadLocation("Europe/London")
	require.NoError(t, err)

	schedule := domain.Schedule{Kind: domain.KindDaily, HourStart: 6, Minute: 0}
	from := time.Date(2026, 3, 27, 12, 0, 0, 0, london)

	runs := nextRuns(t, schedule, "Europe/London", from, 4)
	require.Len(t, runs, 4)

	for _, run := range runs {
		require.Equal(t, 6, run.In(london).Hour(), "six in the morning every day, across the change")
		require.Equal(t, 0, run.In(london).Minute())
	}

	// The days are consecutive, so the shift did not lose or repeat one.
	require.Equal(t, 28, runs[0].In(london).Day())
	require.Equal(t, 29, runs[1].In(london).Day())
	require.Equal(t, 30, runs[2].In(london).Day())
	require.Equal(t, 31, runs[3].In(london).Day())

	// And the absolute gap really did change: one of these days is 23 hours long.
	require.Equal(t, 23*time.Hour, runs[1].Sub(runs[0]))
	require.Equal(t, 24*time.Hour, runs[2].Sub(runs[1]))
}

// TestAnOvernightHourOfZeroIsARealTime: a routine at 00:30 runs at half past midnight,
// which is what makes the hour column not-optional in the sense of "zero means absent".
func TestAnOvernightHourOfZeroIsARealTime(t *testing.T) {
	jakarta, err := time.LoadLocation("Asia/Jakarta")
	require.NoError(t, err)

	schedule := domain.Schedule{Kind: domain.KindDaily, HourStart: 0, Minute: 30}
	from := time.Date(2026, 3, 2, 8, 0, 0, 0, jakarta)

	runs := nextRuns(t, schedule, "Asia/Jakarta", from, 1)
	require.Equal(t, 3, runs[0].In(jakarta).Day())
	require.Equal(t, 0, runs[0].In(jakarta).Hour())
	require.Equal(t, 30, runs[0].In(jakarta).Minute())
}

// TestAnUnparseableZoneFallsBackToUTC: the caller passes the location, so this is
// about the zero value rather than about a bad name — a nil location must not panic.
func TestANilLocationIsUTC(t *testing.T) {
	schedule := domain.Schedule{Kind: domain.KindDaily, HourStart: 6, Minute: 0}
	from := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)

	runs := schedule.NextRuns(nil, from, 1)
	require.Len(t, runs, 1)
	require.Equal(t, 6, runs[0].UTC().Hour())
}

// TestCountZeroAsksForNothing: the empty answer must not be a panic or an infinite
// loop.
func TestCountZeroAsksForNothing(t *testing.T) {
	schedule := domain.Schedule{Kind: domain.KindHourlyRange, HourStart: 8, HourEnd: 21}
	require.Empty(t, schedule.NextRuns(time.UTC, time.Now(), 0))
	require.Empty(t, schedule.NextRuns(time.UTC, time.Now(), -1))
}

// TestDescribeReadsAsTheScheduleIsWorded: the same sentence the screen shows beside a
// routine, which is what a history line and the Temporal schedule's comment use.
func TestDescribeReadsAsTheScheduleIsWorded(t *testing.T) {
	cases := []struct {
		schedule domain.Schedule
		want     string
	}{
		{domain.Schedule{Kind: domain.KindDaily, HourStart: 6, Minute: 0}, "setiap hari 06:00"},
		{domain.Schedule{Kind: domain.KindHourlyRange, HourStart: 8, HourEnd: 21, Minute: 0}, "tiap jam 08:00–21:00"},
		{domain.Schedule{Kind: domain.KindWeekly, HourStart: 7, Minute: 30, DayOfWeek: 3}, "setiap Rabu 07:30"},
		{domain.Schedule{Kind: domain.KindTrigger}, "saat ada kejadian"},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			require.Equal(t, tc.want, tc.schedule.Describe())
		})
	}
}

// TestTheKindsAreTheFourTheDocumentNames: a fifth kind added without the engine and
// the form knowing about it is the drift this list prevents.
func TestTheKindsAreTheFourTheDocumentNames(t *testing.T) {
	require.Equal(t, []string{"daily", "hourly_range", "weekly", "trigger"}, domain.Kinds)

	// Only the trigger kind has no times, which is what makes the other three the ones
	// the engine is given a schedule for.
	for _, kind := range domain.Kinds {
		schedule := domain.Schedule{Kind: kind, HourStart: 8, HourEnd: 9, DayOfWeek: 1}
		require.Equal(t, kind != domain.KindTrigger, schedule.Scheduled(), kind)
	}
}
