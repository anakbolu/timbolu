package quota

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The counter is a cache in front of the ledger, so these tests are about the two
// windows it keeps and the keys it keeps them under: a wrong key would let a
// workspace spend a month's allowance twice, once per window.

// TestCounterKeepsTheTwoWindowsApart is the shape of the cache: the period total
// and the day total are different questions and different keys.
func TestCounterKeepsTheTwoWindowsApart(t *testing.T) {
	counter := &Counter{}
	workspaceID := uuid.New()

	require.NotEqual(t, counter.key(workspaceID), counter.costKey(workspaceID),
		"the period and the day must not share a key")

	require.Contains(t, counter.key(workspaceID), workspaceID.String())
	require.Contains(t, counter.costKey(workspaceID), workspaceID.String())

	// The period key is the calendar month and the day key is the calendar day,
	// both in Jakarta, which is where the customers are.
	require.Equal(t, "quota:spent:"+workspaceID.String()+":"+time.Now().In(jakartaLocation).Format("2006-01"), counter.key(workspaceID))
	require.Equal(t, "quota:cost:"+workspaceID.String()+":"+time.Now().In(jakartaLocation).Format("2006-01-02"), counter.costKey(workspaceID))
}

// TestCounterKeysFollowTheClock is what makes a period roll over: the key is
// derived from the clock, so the month and the day change with it rather than
// being reset by a job that might not run.
func TestCounterKeysFollowTheClock(t *testing.T) {
	jakarta, err := time.LoadLocation("Asia/Jakarta")
	require.NoError(t, err)

	counter := &Counter{}
	workspaceID := uuid.New()

	counter.SetClock(func() time.Time { return time.Date(2026, 1, 31, 23, 0, 0, 0, jakarta) })
	january := counter.key(workspaceID)
	lastDay := counter.costKey(workspaceID)

	counter.SetClock(func() time.Time { return time.Date(2026, 2, 1, 1, 0, 0, 0, jakarta) })
	february := counter.key(workspaceID)
	firstDay := counter.costKey(workspaceID)

	require.NotEqual(t, january, february, "a new month is a new allowance")
	require.NotEqual(t, lastDay, firstDay, "a new day is a new ceiling")
	require.Contains(t, january, "2026-01")
	require.Contains(t, february, "2026-02")
	require.Contains(t, firstDay, "2026-02-01")
}

// TestCounterWithoutRedisReportsItRatherThanZero is the honest degradation: a
// missing client must not answer "nothing spent", because that would hand every
// workspace a fresh allowance. The caller falls back to the ledger instead.
func TestCounterWithoutRedisReportsItRatherThanZero(t *testing.T) {
	counter := New(nil)
	workspaceID := uuid.New()

	_, err := counter.Spent(t.Context(), workspaceID)
	require.ErrorContains(t, err, "redis is not configured")

	_, err = counter.SpentCostToday(t.Context(), workspaceID)
	require.ErrorContains(t, err, "redis is not configured")

	require.ErrorContains(t, counter.Add(t.Context(), workspaceID, 10), "redis is not configured")
	require.ErrorContains(t, counter.AddCost(t.Context(), workspaceID, 10), "redis is not configured")
	require.ErrorContains(t, counter.Reset(t.Context(), workspaceID), "redis is not configured")
}

// TestCounterWithoutRedisRefusesEveryWrite covers the write path's degradation.
//
// A missing client reports "not configured" for a write of any size, including
// an empty one: the counter is a cache in front of the ledger, and the ledger is
// what the caller falls back to. Silently accepting the write would suggest the
// spend was recorded when nothing was.
func TestCounterWithoutRedisRefusesEveryWrite(t *testing.T) {
	counter := New(nil)
	workspaceID := uuid.New()

	for _, tokens := range []int64{0, -1, 10} {
		require.ErrorContains(t, counter.Add(t.Context(), workspaceID, tokens), "redis is not configured")
		require.ErrorContains(t, counter.AddCost(t.Context(), workspaceID, tokens), "redis is not configured")
	}
}
