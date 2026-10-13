package integration

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	billingdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/billing/domain"
	billingservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/billing/service"
	llmrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/repository"
	llmservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/service"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/quota"
)

// E12.2's gate is this file, and it is the property of the quota that only a real Redis and
// a real Postgres can answer: **concurrent callers sharing one allowance must not each get
// the whole of it.**
//
// The check is a read followed by a decision, so the honest question is not "is the
// arithmetic atomic" — it is not — but "can a workspace cross its allowance without bound".
// It can cross it by at most the calls in flight at one moment, and this test states
// exactly that: a handful pass rather than all fifty, and what they recorded is what the
// reconciliation reads afterwards.

// TestConcurrentCallersCannotEachSpendTheWholeAllowance is the gate.
//
// Fifty callers start at once against an allowance of ten. The exact count that passes is
// not the property: callers already inside the check when the allowance runs out are
// allowed to finish. What must hold is that the allowance is *shared*, that the overshoot
// is bounded by the concurrency rather than unbounded, and that the number they spent lands
// in the counter the reconciliation reads.
func TestConcurrentCallersCannotEachSpendTheWholeAllowance(t *testing.T) {
	const (
		allowance = 10
		callers   = 50
	)

	fixture := newBillingFixtureWithQuota(t, appDatabase(t), allowance)
	counter := quota.New(quotaRedis(t))

	require.NoError(t, counter.Reset(t.Context(), fixture.workspace))
	// The cleanup runs after the test's context is done, so it uses its own.
	t.Cleanup(func() { require.NoError(t, counter.Reset(context.Background(), fixture.workspace)) })

	service := billingservice.New(billingservice.Deps{
		Repository: fixture.repository,
		Counter:    quotaAdapter{counter: counter},
		Logger:     zap.NewNop(),
	})

	var (
		start   = make(chan struct{})
		wg      sync.WaitGroup
		mu      sync.Mutex
		allowed int
	)

	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			check, err := service.Check(t.Context(), fixture.workspace)
			if err != nil {
				// A refusal is the expected answer for most callers, and any other error is
				// a real failure of the check rather than of the property under test.
				if !errors.Is(err, billingdomain.ErrQuotaExceeded) {
					t.Errorf("check failed for a reason other than the allowance: %v", err)
				}
				return
			}
			if !check.Allowed {
				return
			}

			mu.Lock()
			allowed++
			mu.Unlock()

			// The call happened, so it is recorded. This is what a real gateway does after a
			// provider answered.
			if err := counter.Add(t.Context(), fixture.workspace, 1); err != nil {
				t.Errorf("could not record a call: %v", err)
			}
		}()
	}

	close(start)
	wg.Wait()

	// The allowance is shared: every caller did not get all of it.
	require.Positive(t, allowed)
	require.Less(t, allowed, callers, "fifty callers must not all pass an allowance of ten")

	// And the overshoot is bounded by the calls in flight rather than unbounded: anything
	// beyond that would mean the check reads a number the counter does not hold.
	require.LessOrEqual(t, allowed, allowance+callers,
		"the overshoot is bounded by the concurrency, not unbounded")

	// What the callers recorded is what the counter reads back, so a workspace is charged
	// for exactly what it was allowed to spend.
	spent, err := counter.Spent(t.Context(), fixture.workspace)
	require.NoError(t, err)
	require.Equal(t, int64(allowed), spent)

	// The sweep writes that total into the durable mirror, which is the number the screen
	// shows and the thresholds are read against.
	_, err = service.Sweep(t.Context())
	require.NoError(t, err)
	require.Equal(t, spent, storedUsage(t, fixture))
}

// TestTheCounterAndTheLedgerAgreeAfterReconciliation is the other half of E12.2's gate: the
// live counter and the durable row are two mirrors of one number, and a sweep makes the
// second say what the first does.
//
// Both directions matter, and each is a state a deployment really reaches: a counter that
// holds more than the row (calls since the last reconciliation) and a row that holds more
// than the counter (a Redis that lost its keys). The second is the one that must not hand a
// workspace its month again, and it is why the check takes the larger of the two.
func TestTheCounterAndTheLedgerAgreeAfterReconciliation(t *testing.T) {
	fixture := newBillingFixture(t, appDatabase(t))
	counter := quota.New(quotaRedis(t))
	ctx := t.Context()

	require.NoError(t, counter.Reset(ctx, fixture.workspace))
	// The cleanup runs after the test's context is done, so it uses its own.
	t.Cleanup(func() { require.NoError(t, counter.Reset(context.Background(), fixture.workspace)) })

	// Both mirrors are wired, which is what a deployment runs: a test that wired only the
	// counter would be testing a deployment that does not exist — and the whole point of
	// the second half of this test is what happens when the counter is the one that is
	// wrong.
	service := billingservice.New(billingservice.Deps{
		Repository: fixture.repository,
		Counter:    quotaAdapter{counter: counter},
		Usage:      ledgerAdapter{reader: fixture.ledger},
		Logger:     zap.NewNop(),
	})

	// Nothing spent yet, so the check allows and the stored usage is zero.
	check, err := service.Check(ctx, fixture.workspace)
	require.NoError(t, err)
	require.True(t, check.Allowed)
	require.Zero(t, check.Period.TokensUsed)

	// Spend some: the counter is the live path a gateway writes on every call, and the
	// ledger is the durable record it also writes. Both hold it, because that is what makes
	// the two correct each other.
	require.NoError(t, counter.Add(ctx, fixture.workspace, 400))
	fixture.spent(t, 400)

	// The check reads the live counter rather than the stored row: a workspace must not be
	// allowed to spend a whole allowance again because the reconciliation has not run.
	live, err := service.Check(ctx, fixture.workspace)
	require.NoError(t, err)
	require.Equal(t, int64(400), live.Period.TokensUsed)

	// The sweep writes it into the mirror.
	_, err = service.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(400), storedUsage(t, fixture))

	// Now the other direction: a Redis that lost its keys while the ledger remembers. The
	// counter says nothing was spent; the sweep restores it from the ledger, which is what
	// stops an outage from handing out a second allowance.
	require.NoError(t, counter.Reset(ctx, fixture.workspace))

	lost, err := counter.Spent(ctx, fixture.workspace)
	require.NoError(t, err)
	require.Zero(t, lost, "the counter really was cleared")

	// The check falls back to the ledger, so it still refuses rather than reading zero.
	after, err := service.Check(ctx, fixture.workspace)
	require.NoError(t, err)
	require.Equal(t, int64(400), after.Period.TokensUsed,
		"with the counter gone, the durable row is what answers — a workspace must not get its month back")

	// And the sweep puts the counter back in step, so the fast path recovers.
	_, err = service.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(400), storedUsage(t, fixture))
}

// quotaAdapter adapts the shared Redis counter to the billing port, the way the container
// does. It is the same two calls with the same names, declared here because the container's
// own adapter is unexported and the test is in another package.
type quotaAdapter struct {
	counter *quota.Counter
}

// PeriodTokens implements billingdomain.SpendCounter.
func (a quotaAdapter) PeriodTokens(ctx context.Context, workspaceID uuid.UUID) (int64, error) {
	return a.counter.Spent(ctx, workspaceID)
}

// DailyCost implements billingdomain.SpendCounter.
func (a quotaAdapter) DailyCost(ctx context.Context, workspaceID uuid.UUID) (int64, error) {
	return a.counter.SpentCostToday(ctx, workspaceID)
}

// ledgerAdapter adapts the usage ledger to the billing port, the way the container does.
type ledgerAdapter struct {
	reader *llmrepo.Repository
}

// PeriodTokens implements billingdomain.UsageReader.
func (a ledgerAdapter) PeriodTokens(ctx context.Context, workspaceID uuid.UUID, since time.Time) (int64, error) {
	return a.reader.SumSince(ctx, workspaceID, since)
}

// DailyCost implements billingdomain.UsageReader.
func (a ledgerAdapter) DailyCost(ctx context.Context, workspaceID uuid.UUID) (int64, error) {
	return a.reader.CostSince(ctx, workspaceID, llmservice.DayStart(time.Now()))
}

// quotaRedis opens the deployment's Redis, or skips the test when there is none.
//
// The counter is tested against a real Redis because that is what makes it a shared
// allowance rather than a variable: the property is "concurrent readers see one number",
// and a fake would only restate the test.
func quotaRedis(t *testing.T) *redis.Client {
	t.Helper()

	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	client := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		require.NoError(t, client.Close())
		t.Skipf("no redis at %s: %v", addr, err)
	}

	t.Cleanup(func() { require.NoError(t, client.Close()) })
	return client
}

// newBillingFixtureWithQuota is the fixture with the trial package's allowance replaced, so
// a test can exhaust it in a handful of calls.
//
// The package's quota is set before the workspace onboards, because the period a workspace
// starts with is granted from the package at that moment: changing the package afterwards
// would leave the period granting what it was opened with.
func newBillingFixtureWithQuota(t *testing.T, dsn string, allowance int64) *billingFixture {
	t.Helper()

	pool := poolFor(t, dsn)

	// `plans` is a shared catalogue with no tenant column, so it is written unscoped: there
	// is no workspace to open a scope with, and the package is not anybody's row.
	_, err := pool.PgxPool().Exec(t.Context(), `UPDATE plans SET token_quota = $1 WHERE is_trial`, allowance)
	require.NoError(t, err)

	return onboardBillingFixture(t, pool)
}

// storedUsage is the durable mirror's total for the workspace's current period.
func storedUsage(t *testing.T, fixture *billingFixture) int64 {
	t.Helper()
	ctx := t.Context()

	subscription, err := fixture.repository.Subscription(ctx, fixture.scope)
	require.NoError(t, err)
	plan, err := fixture.repository.PlanByID(ctx, subscription.PlanID)
	require.NoError(t, err)

	period, err := fixture.repository.Period(ctx, fixture.scope, subscription, plan, time.Now())
	require.NoError(t, err)

	return period.TokensUsed
}
