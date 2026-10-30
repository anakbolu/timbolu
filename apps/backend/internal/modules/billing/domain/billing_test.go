package domain_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/billing/domain"
)

// The domain holds rules rather than declarations, so they are tested here rather than
// only through the service.
//
// Two of them decide what a person experiences: when a workspace stops working, and how
// much of its allowance is gone. Both are arithmetic over times and counts, which is
// exactly the kind of thing a careless refactor changes without anybody noticing.

// TestTheEntitlementFollowsThePeriodAndThenTheGrace is the rule the whole module is
// shaped around, and its order is the rule: inside the period, then the grace days, then
// nothing but reads.
func TestTheEntitlementFollowsThePeriodAndThenTheGrace(t *testing.T) {
	const grace = 7 * 24 * time.Hour
	end := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)

	subscription := domain.Subscription{
		Status:             domain.StatusActive,
		CurrentPeriodStart: end.AddDate(0, 0, -30),
		CurrentPeriodEnd:   end,
	}

	require.Equal(t, domain.EntitlementActive, subscription.Entitlement(end.Add(-time.Hour), grace))
	require.Equal(t, domain.EntitlementGrace, subscription.Entitlement(end, grace), "the moment the period ends, the grace window begins")
	require.Equal(t, domain.EntitlementGrace, subscription.Entitlement(end.Add(grace-time.Second), grace))
	require.Equal(t, domain.EntitlementReadOnly, subscription.Entitlement(end.Add(grace), grace))
	require.Equal(t, domain.EntitlementReadOnly, subscription.Entitlement(end.AddDate(0, 0, 60), grace))

	// A grace window recorded on the row is the one that applies when it is later than
	// the derived one: a deployment that gave a workspace more time keeps it.
	longer := end.AddDate(0, 0, 30)
	extended := subscription
	extended.GraceUntil = &longer
	require.Equal(t, domain.EntitlementGrace, extended.Entitlement(end.Add(grace+time.Hour), grace))

	// A package somebody ended is read-only at once: whoever ended it did not ask for a
	// grace window.
	canceled := subscription
	canceled.Status = domain.StatusCanceled
	require.Equal(t, domain.EntitlementReadOnly, canceled.Entitlement(end.Add(-time.Hour), grace))

	// A negative grace is not a window: it is read-only the moment the period ends, and
	// clamping it keeps a caller's arithmetic from handing out time nobody granted.
	require.Equal(t, domain.EntitlementReadOnly, subscription.Entitlement(end.Add(time.Second), -time.Hour))

	// A zero grace is a deployment that gives none.
	require.Equal(t, domain.EntitlementReadOnly, subscription.Entitlement(end, 0))

	// A subscription whose period was never set is treated as inside it, because a row
	// with no times is not a row that has run out.
	timeless := domain.Subscription{Status: domain.StatusTrial}
	require.Equal(t, domain.EntitlementActive, timeless.Entitlement(end, grace))
}

// TestTheAllowanceIsThePackagePlusWhatWasBought: the two are separate promises — one is
// the package and one is a purchase inside the period — and the count a person sees is
// their sum.
func TestTheAllowanceIsThePackagePlusWhatWasBought(t *testing.T) {
	period := domain.QuotaPeriod{TokensGranted: 2_000_000, TokensToppedUp: 200_000}

	require.Equal(t, int64(2_200_000), period.Allowance())
	require.Equal(t, int64(1_100_000), (domain.QuotaPeriod{TokensGranted: 2_000_000, TokensToppedUp: 200_000, TokensUsed: 1_100_000}).Remaining())
	require.False(t, (domain.QuotaPeriod{TokensGranted: 2_000_000, TokensUsed: 1_500_000}).Exhausted())
	require.True(t, (domain.QuotaPeriod{TokensGranted: 2_000_000, TokensUsed: 2_000_000}).Exhausted())
	require.True(t, (domain.QuotaPeriod{TokensGranted: 2_000_000, TokensToppedUp: 50_000, TokensUsed: 2_050_000}).Exhausted())

	// A zero allowance is unlimited rather than nothing: reading it as zero would refuse
	// every call of a package that bounds no tokens.
	unlimited := domain.QuotaPeriod{TokensGranted: 0, TokensUsed: 99_999_999}
	require.False(t, unlimited.Exhausted())
	require.Zero(t, unlimited.Used())
	require.Equal(t, int64(-1), unlimited.Remaining(), "a negative says 'no bound' rather than a number that looks like one")

	// The percentage is of what the period grants, so the screen's bar matches the
	// count beside it.
	require.Equal(t, 50, (domain.QuotaPeriod{TokensGranted: 1000, TokensUsed: 500}).Used())
	require.Equal(t, 110, (domain.QuotaPeriod{TokensGranted: 1000, TokensUsed: 1100}).Used())
}

// TestEachThresholdIsCrossedOnceAndBothAreSaidInOrder: a warning is what gives a person
// time to act, so the 80% one is not skipped when a period jumps straight past it.
func TestEachThresholdIsCrossedOnceAndBothAreSaidInOrder(t *testing.T) {
	period := domain.QuotaPeriod{TokensGranted: 1000, TokensUsed: 700}

	require.Empty(t, domain.WarningsAt(period).Claim())

	period.TokensUsed = 800
	require.Equal(t, []int{domain.WarningThreshold}, domain.WarningsAt(period).Claim())

	// The mark is what stops it repeating.
	reported := time.Now()
	period.Warning80SentAt = &reported
	require.Empty(t, domain.WarningsAt(period).Claim())

	// A period that jumped from under the warning to past the allowance in one interval
	// says both, in order: the first is the one that arrived in time.
	jumped := domain.QuotaPeriod{TokensGranted: 1000, TokensUsed: 1200}
	require.Equal(t, []int{domain.WarningThreshold, domain.ExhaustedThreshold}, domain.WarningsAt(jumped).Claim())

	// With the 80% one already reported, only the 100% one is claimed: each threshold is
	// its own event.
	period.TokensUsed = 1200
	require.Equal(t, []int{domain.ExhaustedThreshold}, domain.WarningsAt(period).Claim())

	period.Warning100SentAt = &reported
	require.Empty(t, domain.WarningsAt(period).Claim(), "both were reported, so neither repeats")

	// A package with no allowance never warns: a percentage of infinity is not a number.
	unlimited := domain.QuotaPeriod{TokensGranted: 0, TokensUsed: 99_999_999}
	require.Empty(t, domain.WarningsAt(unlimited).Claim())
}

// TestADayCeilingThatIsZeroBoundsNothing: the second bound is optional, and reading a
// zero as "nothing may be spent" would refuse every call of a package that only counts
// tokens.
func TestADayCeilingThatIsZeroBoundsNothing(t *testing.T) {
	require.False(t, domain.Check{DailyCostMicros: 0, SpentTodayMicros: 99_999_999}.DailyExhausted())
	require.False(t, domain.Check{DailyCostMicros: 1_000_000, SpentTodayMicros: 999_999}.DailyExhausted())
	require.True(t, domain.Check{DailyCostMicros: 1_000_000, SpentTodayMicros: 1_000_000}.DailyExhausted())
}

// TestOnlyACanceledSubscriptionIsOutsideTheLiveOne: the unique index on the table
// covers the same set, so a live row and a row the index allows cannot disagree.
func TestOnlyACanceledSubscriptionIsOutsideTheLiveOne(t *testing.T) {
	for _, status := range []string{domain.StatusTrial, domain.StatusActive, domain.StatusPastDue} {
		require.True(t, domain.Subscription{Status: status}.Live(), status)
	}
	require.False(t, domain.Subscription{Status: domain.StatusCanceled}.Live())
}

// TestOnlyAPendingInvoiceIsOutstanding: a paid, failed, or expired invoice is history,
// and a screen that offered "pay" on one would be offering nothing.
func TestOnlyAPendingInvoiceIsOutstanding(t *testing.T) {
	require.True(t, domain.Invoice{Status: domain.StatusPending}.Outstanding())
	for _, status := range []string{domain.StatusPaid, domain.StatusFailed, domain.StatusExpired} {
		require.False(t, domain.Invoice{Status: status}.Outstanding(), status)
	}
}

// TestAScopeNeedsAWorkspace: every read in this module is a workspace's own, so a scope
// with no tenant is refused rather than answered with somebody else's allowance.
func TestAScopeNeedsAWorkspace(t *testing.T) {
	require.True(t, domain.Scope{}.IsZero())
	require.True(t, domain.Scope{UserID: uuid.New()}.IsZero())
	require.False(t, domain.Scope{WorkspaceID: uuid.New()}.IsZero())
}
