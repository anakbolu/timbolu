package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
)

// TestCheckEnforcesTheDailyCostCeiling is the second bound: a workspace with
// tokens left for the month still stops once the day is spent, so one runaway
// task cannot spend a month in an afternoon.
func TestCheckEnforcesTheDailyCostCeiling(t *testing.T) {
	ctx := t.Context()
	workspaceID := uuid.New()

	policy := &stubQuota{allowance: 1_000_000, dailyCost: 5_000}
	counter := &stubCounter{spentToday: 5_000}
	service := newQuotaService(t, policy, counter, nil)

	err := service.Check(ctx, workspaceID)
	require.ErrorIs(t, err, domain.ErrQuotaExceeded)
	require.Contains(t, err.Error(), "micro-rupiah")
	require.Contains(t, err.Error(), "hari ini")

	// A day that is not yet spent lets the call through.
	counter.spentToday = 4_999
	require.NoError(t, service.Check(ctx, workspaceID))
}

// TestCheckFallsBackToTheDailyAggregationWithoutARedisCounter is the outage
// rule: a Redis that is down must not hand every workspace a fresh ceiling, so
// the daily aggregate answers instead.
func TestCheckFallsBackToTheDailyAggregationWithoutARedisCounter(t *testing.T) {
	ctx := t.Context()
	workspaceID := uuid.New()

	policy := &stubQuota{allowance: 1_000_000, dailyCost: 5_000}
	reader := &stubUsageReader{costSince: 9_000}
	service := newQuotaService(t, policy, nil, reader)

	err := service.Check(ctx, workspaceID)
	require.ErrorIs(t, err, domain.ErrQuotaExceeded)
	require.Contains(t, err.Error(), "micro-rupiah")

	// The read is scoped to today in Jakarta, which is where the customers are.
	require.False(t, reader.since.IsZero())
	require.Equal(t, 0, reader.since.Hour())
}

// TestCheckFallsBackToTheAggregationWhenTheCounterFails is the same rule when the
// counter is configured but unreachable.
func TestCheckFallsBackToTheAggregationWhenTheCounterFails(t *testing.T) {
	ctx := t.Context()

	policy := &stubQuota{allowance: 1_000_000, dailyCost: 5_000}
	counter := &stubCounter{costErr: errors.New("redis is unreachable")}
	reader := &stubUsageReader{costSince: 9_000}
	service := newQuotaService(t, policy, counter, reader)

	require.ErrorIs(t, service.Check(ctx, uuid.New()), domain.ErrQuotaExceeded)
}

// TestCheckSkipsAZeroDailyCeiling is the "unlimited" rule: a deployment that
// states no daily ceiling runs without one, rather than refusing every call.
func TestCheckSkipsAZeroDailyCeiling(t *testing.T) {
	ctx := t.Context()

	policy := &stubQuota{allowance: 1_000_000, dailyCost: 0}
	counter := &stubCounter{spentToday: 999_999_999}
	service := newQuotaService(t, policy, counter, nil)

	require.NoError(t, service.Check(ctx, uuid.New()))
}

// TestDayStartFollowsJakarta is the day boundary the ceiling follows: the
// customers are in Jakarta, so their midnight is the one that resets it.
func TestDayStartFollowsJakarta(t *testing.T) {
	// 2026-03-01 02:00 UTC is 09:00 in Jakarta on the same day.
	start := DayStart(time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC))

	jakarta, err := time.LoadLocation("Asia/Jakarta")
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 3, 1, 0, 0, 0, 0, jakarta), start)

	// And a moment just before Jakarta's midnight belongs to the previous day.
	before := DayStart(time.Date(2026, 2, 28, 16, 59, 0, 0, time.UTC))
	require.Equal(t, 28, before.In(jakarta).Day())
}

// stubQuota answers both bounds.
type stubQuota struct {
	allowance int64
	dailyCost int64
	err       error
}

func (s *stubQuota) Allowance(context.Context, uuid.UUID) (int64, error) {
	return s.allowance, s.err
}

func (s *stubQuota) DailyCostAllowance(context.Context, uuid.UUID) (int64, error) {
	return s.dailyCost, s.err
}

// stubCounter is the live spend, with either window able to fail.
type stubCounter struct {
	spent      int64
	spentToday int64
	costErr    error
}

func (s *stubCounter) Spent(context.Context, uuid.UUID) (int64, error) { return s.spent, nil }
func (s *stubCounter) Add(context.Context, uuid.UUID, int64) error     { return nil }
func (s *stubCounter) SpentCostToday(context.Context, uuid.UUID) (int64, error) {
	return s.spentToday, s.costErr
}
func (s *stubCounter) AddCost(context.Context, uuid.UUID, int64) error { return nil }

// stubUsageReader is the durable fallback.
type stubUsageReader struct {
	sumSince  int64
	costSince int64
	since     time.Time
}

func (s *stubUsageReader) Daily(context.Context, uuid.UUID, time.Time) ([]domain.UsageDaily, error) {
	return nil, nil
}

func (s *stubUsageReader) SumSince(_ context.Context, _ uuid.UUID, since time.Time) (int64, error) {
	s.since = since
	return s.sumSince, nil
}

func (s *stubUsageReader) CostSince(_ context.Context, _ uuid.UUID, since time.Time) (int64, error) {
	s.since = since
	return s.costSince, nil
}

// newQuotaService builds a gateway with the quota check on and no providers: the
// check runs before any provider is resolved, which is what makes it testable
// without one.
func newQuotaService(t *testing.T, policy domain.QuotaPolicy, counter domain.QuotaCounter, reader domain.UsageReader) *Service {
	t.Helper()

	return New(Deps{
		Quota:   policy,
		Counter: counter,
		Reader:  reader,
		Settings: Config{
			QuotaEnabled: true,
		},
	})
}
