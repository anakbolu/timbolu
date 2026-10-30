package service_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/billing/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/billing/domain/mocks"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/billing/service"
)

// The module's own rules are this file: what a workspace may spend, what it is told
// when it runs out, what it owes when a period ends, and what a payment does.
//
// Everything here is about the product's decisions rather than about Postgres: the
// store is a mock, and what is under test is the arithmetic and the order.
//
// The clock is a field rather than the wall clock, and that is not a convenience:
// every one of these rules is about a moment — a period ending, a grace window
// closing, a day's ceiling — so a test that read `time.Now` would pass in the
// morning and fail after midnight.

// harness is a service over scripted ports.
type harness struct {
	service  *service.Service
	store    *mocks.Repository
	usage    *mocks.UsageReader
	counter  *mocks.SpendCounter
	gateway  *mocks.Gateway
	notifier *mocks.Notifier

	clock *fakeClock

	workspace uuid.UUID
	scope     domain.Scope
}

// fakeClock is the source of "now".
type fakeClock struct{ at time.Time }

func (c *fakeClock) now() time.Time { return c.at }

func newHarness(t *testing.T, options ...func(*service.Deps)) *harness {
	t.Helper()

	h := &harness{
		store:     mocks.NewRepository(t),
		usage:     mocks.NewUsageReader(t),
		counter:   mocks.NewSpendCounter(t),
		gateway:   mocks.NewGateway(t),
		notifier:  mocks.NewNotifier(t),
		clock:     &fakeClock{at: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)},
		workspace: uuid.New(),
	}
	h.scope = domain.Scope{UserID: uuid.New(), WorkspaceID: h.workspace}

	deps := service.Deps{
		Repository: h.store,
		Usage:      h.usage,
		Counter:    h.counter,
		Gateway:    h.gateway,
		Notifier:   h.notifier,
		Clock:      h.clock.now,
	}
	for _, option := range options {
		option(&deps)
	}

	h.service = service.New(deps)
	return h
}

// plan is one package, ready to be acted on.
func (h *harness) plan(mutate ...func(*domain.Plan)) domain.Plan {
	plan := domain.Plan{
		ID:              uuid.New(),
		Key:             "keluarga",
		Name:            "Paket Keluarga",
		MonthlyPriceIDR: 149000,
		TokenQuota:      1000,
		DailyCostMicros: 5_000_000,
		PeriodDays:      30,
		MaxAgents:       6,
		MaxIntegrations: 3,
		MaxMembers:      3,
	}
	for _, change := range mutate {
		change(&plan)
	}
	return plan
}

// subscription is the live package of the harness's workspace.
func (h *harness) subscription(plan domain.Plan, mutate ...func(*domain.Subscription)) domain.Subscription {
	subscription := domain.Subscription{
		ID:                 uuid.New(),
		WorkspaceID:        h.workspace,
		PlanID:             plan.ID,
		Status:             domain.StatusActive,
		CurrentPeriodStart: h.clock.at.AddDate(0, 0, -10),
		CurrentPeriodEnd:   h.clock.at.AddDate(0, 0, 20),
		CreatedAt:          h.clock.at.AddDate(0, 0, -10),
		UpdatedAt:          h.clock.at.AddDate(0, 0, -10),
	}
	for _, change := range mutate {
		change(&subscription)
	}
	return subscription
}

// period is the quota period the harness's workspace is in.
func (h *harness) period(subscription domain.Subscription, plan domain.Plan, mutate ...func(*domain.QuotaPeriod)) domain.QuotaPeriod {
	period := domain.QuotaPeriod{
		ID:            uuid.New(),
		WorkspaceID:   h.workspace,
		PeriodStart:   subscription.CurrentPeriodStart,
		PeriodEnd:     subscription.CurrentPeriodEnd,
		TokensGranted: plan.TokenQuota,
		CreatedAt:     subscription.CurrentPeriodStart,
		UpdatedAt:     h.clock.at,
	}
	for _, change := range mutate {
		change(&period)
	}
	return period
}

// live is the two reads every path starts with.
func (h *harness) live(subscription domain.Subscription, plan domain.Plan) {
	h.store.EXPECT().Subscription(mock.Anything, mock.Anything).Return(subscription, nil).Maybe()
	h.store.EXPECT().PlanByID(mock.Anything, subscription.PlanID).Return(plan, nil).Maybe()
}

// expectCheck is the read path of one quota check: the period, the counter's answer,
// and the day's cost.
func (h *harness) expectCheck(subscription domain.Subscription, plan domain.Plan, period domain.QuotaPeriod, liveTokens int64, spentMicros int64) {
	h.live(subscription, plan)
	h.store.EXPECT().Period(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(period, nil).Maybe()
	h.counter.EXPECT().PeriodTokens(mock.Anything, h.workspace).Return(liveTokens, nil).Maybe()
	h.usage.EXPECT().PeriodTokens(mock.Anything, h.workspace, mock.Anything).Return(int64(0), nil).Maybe()
	h.counter.EXPECT().DailyCost(mock.Anything, h.workspace).Return(spentMicros, nil).Maybe()
}

// TestAnAllowanceThatIsSpentRefusesWithBothNumbers: the message is what a person
// reads, so it names what was used and what the package grants. "Kuota habis" alone
// tells nobody whether they are one token over or a month over.
func TestAnAllowanceThatIsSpentRefusesWithBothNumbers(t *testing.T) {
	h := newHarness(t)
	plan := h.plan()
	subscription := h.subscription(plan)
	period := h.period(subscription, plan)

	h.expectCheck(subscription, plan, period, 1000, 0)

	check, err := h.service.Check(t.Context(), h.workspace)

	require.ErrorIs(t, err, domain.ErrQuotaExceeded)
	require.False(t, check.Allowed)
	require.Contains(t, err.Error(), "1000")
	require.Contains(t, err.Error(), "beli tambahan", "and says what to do about it")

	// One token short of the allowance is still allowed: the bound is the allowance
	// being reached, not nearly reached.
	h2 := newHarness(t)
	plan2 := h2.plan()
	subscription2 := h2.subscription(plan2)
	h2.expectCheck(subscription2, plan2, h2.period(subscription2, plan2), 999, 0)

	allowed, err := h2.service.Check(t.Context(), h2.workspace)
	require.NoError(t, err)
	require.True(t, allowed.Allowed)
}

// TestATopUpExtendsTheAllowanceItWasBoughtIn: the tokens a workspace bought are part
// of what it may spend in that period, and the check adds them rather than replacing
// the package with them.
func TestATopUpExtendsTheAllowanceItWasBoughtIn(t *testing.T) {
	h := newHarness(t)
	plan := h.plan()
	subscription := h.subscription(plan)
	period := h.period(subscription, plan, func(p *domain.QuotaPeriod) { p.TokensToppedUp = 500 })

	h.expectCheck(subscription, plan, period, 1200, 0)

	check, err := h.service.Check(t.Context(), h.workspace)

	require.NoError(t, err, "1500 tokens may be spent, and 1200 were")
	require.Equal(t, int64(1500), check.Period.Allowance())
	require.Equal(t, int64(300), check.Period.Remaining())

	// Once the top-up is spent too, the refusal counts both.
	h2 := newHarness(t)
	plan2 := h2.plan()
	subscription2 := h2.subscription(plan2)
	h2.expectCheck(subscription2, plan2, h2.period(subscription2, plan2, func(p *domain.QuotaPeriod) { p.TokensToppedUp = 500 }), 1500, 0)

	_, err = h2.service.Check(t.Context(), h2.workspace)
	require.ErrorIs(t, err, domain.ErrQuotaExceeded)
}

// TestTheLiveCounterAndTheLedgerCorrectEachOther is the reconciliation rule, and the
// direction matters: whichever mirror knows of more spending is the one that counts,
// so an outage cannot hand a workspace a fresh allowance.
func TestTheLiveCounterAndTheLedgerCorrectEachOther(t *testing.T) {
	plan := func(plan *domain.Plan) { plan.TokenQuota = 100 }

	// A flushed Redis under-reports and the ledger corrects it.
	h := newHarness(t)
	p := h.plan(plan)
	s := h.subscription(p)
	h.live(s, p)
	h.store.EXPECT().Period(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(h.period(s, p), nil).Maybe()
	h.counter.EXPECT().PeriodTokens(mock.Anything, h.workspace).Return(int64(10), nil).Maybe()
	h.usage.EXPECT().PeriodTokens(mock.Anything, h.workspace, mock.Anything).Return(int64(100), nil).Maybe()

	_, err := h.service.Check(t.Context(), h.workspace)
	require.ErrorIs(t, err, domain.ErrQuotaExceeded, "the ledger knew about the spending the counter lost")

	// A ledger write that has not landed yet under-reports and the counter corrects it.
	other := newHarness(t)
	op := other.plan(plan)
	os := other.subscription(op)
	other.live(os, op)
	other.store.EXPECT().Period(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(other.period(os, op), nil).Maybe()
	other.counter.EXPECT().PeriodTokens(mock.Anything, other.workspace).Return(int64(100), nil).Maybe()
	other.usage.EXPECT().PeriodTokens(mock.Anything, other.workspace, mock.Anything).Return(int64(20), nil).Maybe()

	_, err = other.service.Check(t.Context(), other.workspace)
	require.ErrorIs(t, err, domain.ErrQuotaExceeded, "the counter knew about the call the ledger had not recorded")

	// Neither answering is an error rather than a clean slate: returning zero would
	// hand the workspace its month again.
	third := newHarness(t)
	tp := third.plan(plan)
	ts := third.subscription(tp)
	third.live(ts, tp)
	third.store.EXPECT().Period(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(third.period(ts, tp), nil).Maybe()
	third.counter.EXPECT().PeriodTokens(mock.Anything, third.workspace).Return(int64(0), errors.New("redis is down")).Maybe()
	third.usage.EXPECT().PeriodTokens(mock.Anything, third.workspace, mock.Anything).Return(int64(0), errors.New("the ledger is down")).Maybe()

	check, err := third.service.Check(t.Context(), third.workspace)
	require.Error(t, err)
	require.False(t, check.Allowed)
	require.NotErrorIs(t, err, domain.ErrQuotaExceeded, "this is a failure to read, not a refusal")
}

// TestAPackageThatDoesNotBoundADayLeavesTheDayAlone: a zero ceiling means "no daily
// bound", which is how the trial runs. Reading it as zero spending allowed would
// refuse every call.
func TestAPackageThatDoesNotBoundADayLeavesTheDayAlone(t *testing.T) {
	h := newHarness(t)
	plan := h.plan(func(p *domain.Plan) { p.DailyCostMicros = 0 })
	subscription := h.subscription(plan)

	h.expectCheck(subscription, plan, h.period(subscription, plan), 10, 999_999_999)

	check, err := h.service.Check(t.Context(), h.workspace)
	require.NoError(t, err)
	require.False(t, check.DailyExhausted())

	// A package that does bound the day refuses when it is reached, and the message
	// says it is about today rather than about the allowance.
	other := newHarness(t)
	op := other.plan(func(p *domain.Plan) { p.DailyCostMicros = 1_000_000 })
	os := other.subscription(op)
	other.expectCheck(os, op, other.period(os, op), 10, 1_000_000)

	_, err = other.service.Check(t.Context(), other.workspace)
	require.ErrorIs(t, err, domain.ErrQuotaExceeded)
	require.Contains(t, err.Error(), "harian")
}

// TestAWorkspacePastItsGraceWindowIsReadOnly is the epic's fourth gate, and it is the
// answer the gateway's pre-flight check and the task runtime's round check both read:
// one implementation, so the two cannot disagree about whether a workspace stopped.
func TestAWorkspacePastItsGraceWindowIsReadOnly(t *testing.T) {
	h := newHarness(t)
	plan := h.plan()
	// The period ended ten days ago and the grace window is a week, so it is over.
	subscription := h.subscription(plan, func(s *domain.Subscription) {
		s.CurrentPeriodEnd = h.clock.at.AddDate(0, 0, -10)
		s.Status = domain.StatusPastDue
	})
	h.live(subscription, plan)

	check, err := h.service.Check(t.Context(), h.workspace)

	require.ErrorIs(t, err, domain.ErrReadOnly)
	require.False(t, check.Allowed)
	require.Equal(t, domain.EntitlementReadOnly, check.Entitlement)

	// Inside the grace window the workspace still works, and says it is on borrowed
	// time rather than pretending it is fine.
	h2 := newHarness(t)
	plan2 := h2.plan()
	inside := h2.subscription(plan2, func(s *domain.Subscription) {
		s.CurrentPeriodEnd = h2.clock.at.AddDate(0, 0, -3)
		s.Status = domain.StatusPastDue
	})
	h2.live(inside, plan2)
	h2.store.EXPECT().Period(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(h2.period(inside, plan2), nil).Maybe()
	h2.counter.EXPECT().PeriodTokens(mock.Anything, h2.workspace).Return(int64(10), nil).Maybe()
	h2.usage.EXPECT().PeriodTokens(mock.Anything, h2.workspace, mock.Anything).Return(int64(0), nil).Maybe()
	h2.counter.EXPECT().DailyCost(mock.Anything, h2.workspace).Return(int64(0), nil).Maybe()

	grace, err := h2.service.Check(t.Context(), h2.workspace)
	require.NoError(t, err)
	require.True(t, grace.Allowed)
	require.Equal(t, domain.EntitlementGrace, grace.Entitlement)

	// ReadOnly is the same answer the middleware asks, and a canceled package is
	// read-only at once: whoever ended it did not ask for a grace window.
	h3 := newHarness(t)
	plan3 := h3.plan()
	canceled := h3.subscription(plan3, func(s *domain.Subscription) { s.Status = domain.StatusCanceled })
	h3.live(canceled, plan3)

	readOnly, err := h3.service.ReadOnly(t.Context(), h3.workspace)
	require.NoError(t, err)
	require.True(t, readOnly)
}

// TestTheLimitsComeFromThePackageInForce: a cap changes when the package changes and
// not when somebody clicks a screen, so a workspace cannot outgrow a package it has
// not paid for.
func TestTheLimitsComeFromThePackageInForce(t *testing.T) {
	h := newHarness(t)
	plan := h.plan(func(p *domain.Plan) {
		p.MaxAgents = 6
		p.MaxIntegrations = 3
		p.MaxMembers = 3
	})
	subscription := h.subscription(plan)
	h.live(subscription, plan)

	limits, err := h.service.Limits(t.Context(), h.workspace)
	require.NoError(t, err)
	require.Equal(t, 6, limits.MaxAgents)
	require.Equal(t, 3, limits.MaxIntegrations)
	require.Equal(t, 3, limits.MaxMembers)

	// A zero is unlimited rather than a cap of none, which is how a package that
	// bounds nothing is written.
	other := newHarness(t)
	op := other.plan(func(p *domain.Plan) { p.MaxAgents = 0 })
	os := other.subscription(op)
	other.live(os, op)

	unbounded, err := other.service.Limits(t.Context(), other.workspace)
	require.NoError(t, err)
	require.Zero(t, unbounded.MaxAgents)
}

// TestAWorkspaceWithNoSubscriptionIsAnError: the migration backfilled one onto every
// workspace, so a read that finds none is a broken row. Answering "unlimited" would
// serve that workspace for free forever.
func TestAWorkspaceWithNoSubscriptionIsAnError(t *testing.T) {
	h := newHarness(t)

	h.store.EXPECT().Subscription(mock.Anything, mock.Anything).
		Return(domain.Subscription{}, domain.ErrSubscriptionNotFound).Maybe()

	_, err := h.service.Check(t.Context(), h.workspace)
	require.ErrorIs(t, err, domain.ErrSubscriptionNotFound)
}

// TestWhoPaysIsWorthARefusal: the plumbing is asserted once here because every path
// depends on it — a workspace that cannot be identified is a workspace that cannot be
// billed.
func TestWhoPaysIsWorthARefusal(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.Check(t.Context(), uuid.Nil)
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Snapshot(t.Context(), domain.Scope{})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Change(t.Context(), domain.Scope{}, domain.ChangeRequest{PlanKey: "keluarga"})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.BuyTopUp(t.Context(), domain.Scope{}, domain.TopUpRequest{PackageKey: "tambahan-200k"})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Pay(t.Context(), domain.Scope{}, uuid.New())
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestTheScreenShowsOneMomentInOneRead: the package, the usage, the invoices, and the
// limits are one snapshot because a screen that read them at four moments could show a
// workspace that is over its allowance beside a period that says it is not.
func TestTheScreenShowsOneMomentInOneRead(t *testing.T) {
	h := newHarness(t)
	plan := h.plan()
	subscription := h.subscription(plan)
	period := h.period(subscription, plan, func(p *domain.QuotaPeriod) {
		p.TokensUsed = 400
		p.TokensToppedUp = 100
	})

	h.live(subscription, plan)
	h.store.EXPECT().Period(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(period, nil).Once()

	owed := domain.Invoice{ID: uuid.New(), WorkspaceID: h.workspace, Status: domain.StatusPending, AmountIDR: 149000}
	h.store.EXPECT().Outstanding(mock.Anything, mock.Anything).Return([]domain.Invoice{owed}, nil).Once()
	h.store.EXPECT().Invoices(mock.Anything, mock.Anything, mock.Anything).Return([]domain.Invoice{owed}, nil).Once()
	h.store.EXPECT().TopUps(mock.Anything, mock.Anything).
		Return([]domain.TopUpPackage{{Key: "tambahan-200k", Name: "Tambahan", TokenAmount: 200000, PriceIDR: 25000}}, nil).Once()
	h.counter.EXPECT().DailyCost(mock.Anything, h.workspace).Return(int64(1_500_000), nil).Once()

	snapshot, err := h.service.Snapshot(t.Context(), h.scope)
	require.NoError(t, err)

	require.Equal(t, "keluarga", snapshot.Plan.Key)
	require.Equal(t, domain.EntitlementActive, snapshot.Entitlement)
	require.False(t, snapshot.ReadOnly)
	require.Equal(t, int64(400), snapshot.Period.TokensUsed)
	require.Equal(t, int64(1100), snapshot.Period.Allowance())
	require.Equal(t, int64(700), snapshot.Period.Remaining())
	require.Equal(t, 36, snapshot.Period.Used())
	require.Equal(t, 6, snapshot.Limits.MaxAgents)
	require.Equal(t, int64(1_500_000), snapshot.UsageToday.CostMicros)
	require.Len(t, snapshot.Outstanding, 1)
	require.Len(t, snapshot.TopUps, 1)
	require.Equal(t, 7, snapshot.GraceDays, "the window a person is given is stated rather than implied")

	// A package that grants nothing is unlimited, and the remaining figure says so
	// with a negative rather than a number that looks like a real bound.
	h2 := newHarness(t)
	unlimited := h2.plan(func(p *domain.Plan) { p.TokenQuota = 0 })
	subscription2 := h2.subscription(unlimited)
	h2.live(subscription2, unlimited)
	h2.store.EXPECT().Period(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(h2.period(subscription2, unlimited), nil).Maybe()
	h2.store.EXPECT().Outstanding(mock.Anything, mock.Anything).Return(nil, nil).Once()
	h2.store.EXPECT().Invoices(mock.Anything, mock.Anything, mock.Anything).Return(nil, nil).Once()
	h2.store.EXPECT().TopUps(mock.Anything, mock.Anything).Return(nil, nil).Once()
	h2.counter.EXPECT().DailyCost(mock.Anything, h2.workspace).Return(int64(0), nil).Once()

	open, err := h2.service.Snapshot(t.Context(), h2.scope)
	require.NoError(t, err)
	require.Equal(t, int64(-1), open.Period.Remaining())
	require.Zero(t, open.Period.Used())
}

// TestChangingAPackageAppliesToThisPeriodOnlyFromATrial is the product's rule about
// money: a period that was already paid for cannot be upgraded for free, and a trial is
// not a period anybody paid for.
func TestChangingAPackageAppliesToThisPeriodOnlyFromATrial(t *testing.T) {
	target := domain.Plan{ID: uuid.New(), Key: "rombongan", Name: "Paket Rombongan", MonthlyPriceIDR: 449000, PeriodDays: 30}

	// On a paid package the change is deferred: the workspace keeps what it paid for.
	h := newHarness(t)
	plan := h.plan()
	subscription := h.subscription(plan)

	// The row as it is read before the change, and as it is read after: a deferred
	// change leaves the package in force alone and records the one that follows.
	deferred := subscription
	deferred.PendingPlanID = target.ID

	h.store.EXPECT().Subscription(mock.Anything, mock.Anything).Return(subscription, nil).Once()
	h.store.EXPECT().Subscription(mock.Anything, mock.Anything).Return(deferred, nil).Once()
	h.store.EXPECT().PlanByID(mock.Anything, plan.ID).Return(plan, nil).Maybe()
	h.store.EXPECT().Plan(mock.Anything, "rombongan").Return(target, nil).Once()
	h.store.EXPECT().SetSubscription(mock.Anything, h.scope, mock.MatchedBy(func(updated domain.Subscription) bool {
		return updated.PlanID == plan.ID && updated.PendingPlanID == target.ID
	})).Return(deferred, nil).Once()
	h.store.EXPECT().Outstanding(mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	h.notifier.EXPECT().Notify(mock.Anything, mock.Anything, mock.MatchedBy(func(notice domain.Notice) bool {
		return notice.Kind == NoticePlanChangedForTest &&
			notice.PlanName == "Paket Rombongan" &&
			notice.Reason != ""
	})).Return(nil).Once()
	h.store.EXPECT().PlanByID(mock.Anything, target.ID).Return(target, nil).Maybe()
	h.expectSnapshotAfterChange(subscription, plan)

	snapshot, err := h.service.Change(t.Context(), h.scope, domain.ChangeRequest{PlanKey: "rombongan"})
	require.NoError(t, err)
	require.Equal(t, "keluarga", snapshot.Plan.Key, "the package in force has not changed yet")
	require.Equal(t, "rombongan", snapshot.PendingPlanKey)
	require.Equal(t, "Paket Rombongan", snapshot.PendingPlanName)

	// On a trial the change applies at once: nothing has been paid for the period the
	// workspace is in.
	h2 := newHarness(t)
	trial := h2.plan(func(p *domain.Plan) {
		p.Key = "coba"
		p.Name = "Paket Coba"
		p.MonthlyPriceIDR = 0
		p.IsTrial = true
	})
	trialSubscription := h2.subscription(trial, func(s *domain.Subscription) { s.Status = domain.StatusTrial })

	h2.live(trialSubscription, trial)
	h2.store.EXPECT().Plan(mock.Anything, "keluarga").Return(target, nil).Once()
	h2.store.EXPECT().SetSubscription(mock.Anything, h2.scope, mock.MatchedBy(func(updated domain.Subscription) bool {
		return updated.PlanID == target.ID && updated.PendingPlanID == uuid.Nil
	})).Return(trialSubscription, nil).Once()
	h2.store.EXPECT().Outstanding(mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	h2.notifier.EXPECT().Notify(mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	h2.store.EXPECT().PlanByID(mock.Anything, mock.Anything).Return(target, nil).Maybe()
	h2.store.EXPECT().Period(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(h2.period(trialSubscription, trial), nil).Maybe()
	h2.store.EXPECT().Invoices(mock.Anything, mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	h2.store.EXPECT().TopUps(mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	h2.counter.EXPECT().DailyCost(mock.Anything, h2.workspace).Return(int64(0), nil).Maybe()

	_, err = h2.service.Change(t.Context(), h2.scope, domain.ChangeRequest{PlanKey: "keluarga"})
	require.NoError(t, err)
}

// expectSnapshotAfterChange is the read a change ends with.
func (h *harness) expectSnapshotAfterChange(subscription domain.Subscription, plan domain.Plan) {
	h.store.EXPECT().Period(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(h.period(subscription, plan), nil).Maybe()
	h.store.EXPECT().Invoices(mock.Anything, mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	h.store.EXPECT().TopUps(mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	h.counter.EXPECT().DailyCost(mock.Anything, h.workspace).Return(int64(0), nil).Maybe()
}

// TestChangingToThePackageItIsOnIsRefused: a change to where a workspace already is
// would issue nothing and mean nothing, and saying so is better than a success that did
// nothing.
func TestChangingToThePackageItIsOnIsRefused(t *testing.T) {
	h := newHarness(t)
	plan := h.plan()
	subscription := h.subscription(plan)

	h.live(subscription, plan)
	h.store.EXPECT().Plan(mock.Anything, "keluarga").Return(plan, nil).Once()

	_, err := h.service.Change(t.Context(), h.scope, domain.ChangeRequest{PlanKey: "keluarga"})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
	require.Contains(t, err.Error(), "sudah memakai")

	// A package nobody sells is the store's own refusal.
	h2 := newHarness(t)
	h2.live(subscription, plan)
	h2.store.EXPECT().Plan(mock.Anything, "nobody").Return(domain.Plan{}, domain.ErrPlanNotFound).Once()

	_, err = h2.service.Change(t.Context(), h2.scope, domain.ChangeRequest{PlanKey: "nobody"})
	require.ErrorIs(t, err, domain.ErrPlanNotFound)

	// An empty choice is the caller's error.
	_, err = h2.service.Change(t.Context(), h2.scope, domain.ChangeRequest{})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestAnUpcomingInvoiceIsWithdrawnWhenThePriceChanges: the amount it asked for is no
// longer the amount owed, so it is closed as history rather than edited — and a paid
// one is never touched, because the money already moved.
func TestAnUpcomingInvoiceIsWithdrawnWhenThePriceChanges(t *testing.T) {
	h := newHarness(t)
	plan := h.plan()
	target := domain.Plan{ID: uuid.New(), Key: "rombongan", Name: "Paket Rombongan", MonthlyPriceIDR: 449000, PeriodDays: 30}
	subscription := h.subscription(plan)

	upcoming := domain.Invoice{
		ID:          uuid.New(),
		WorkspaceID: h.workspace,
		Kind:        domain.KindSubscription,
		Status:      domain.StatusPending,
		AmountIDR:   plan.MonthlyPriceIDR,
		PeriodStart: subscription.CurrentPeriodEnd,
		PeriodEnd:   subscription.CurrentPeriodEnd.AddDate(0, 0, 30),
	}
	// One invoice for the period the workspace is in, which is not the upcoming one and
	// must not be withdrawn.
	current := domain.Invoice{
		ID:          uuid.New(),
		WorkspaceID: h.workspace,
		Kind:        domain.KindSubscription,
		Status:      domain.StatusPending,
		AmountIDR:   plan.MonthlyPriceIDR,
		PeriodStart: subscription.CurrentPeriodStart,
		PeriodEnd:   subscription.CurrentPeriodEnd,
	}

	h.live(subscription, plan)
	h.store.EXPECT().Plan(mock.Anything, "rombongan").Return(target, nil).Once()
	h.store.EXPECT().SetSubscription(mock.Anything, h.scope, mock.Anything).Return(subscription, nil).Once()
	h.store.EXPECT().Outstanding(mock.Anything, mock.Anything).
		Return([]domain.Invoice{current, upcoming}, nil).Maybe()
	h.store.EXPECT().ExpireInvoice(mock.Anything, h.scope, upcoming.ID).Return(upcoming, true, nil).Once()
	h.notifier.EXPECT().Notify(mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	h.expectSnapshotAfterChange(subscription, plan)

	_, err := h.service.Change(t.Context(), h.scope, domain.ChangeRequest{PlanKey: "rombongan"})
	require.NoError(t, err)
}

// TestBuyingATopUpPricesItFromThePackAndNamesThePeriod is what the screen's buy button
// does: one invoice for one pack, in the period the workspace is in.
func TestBuyingATopUpPricesItFromThePackAndNamesThePeriod(t *testing.T) {
	h := newHarness(t)
	plan := h.plan()
	subscription := h.subscription(plan)
	period := h.period(subscription, plan)
	pack := domain.TopUpPackage{ID: uuid.New(), Key: "tambahan-200k", Name: "Tambahan 200 ribu token", TokenAmount: 200000, PriceIDR: 25000}

	h.store.EXPECT().TopUp(mock.Anything, h.scope, "tambahan-200k").Return(pack, nil).Once()
	h.live(subscription, plan)
	h.store.EXPECT().Period(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(period, nil).Once()

	var created domain.Invoice
	h.store.EXPECT().CreateInvoice(mock.Anything, h.scope, mock.MatchedBy(func(invoice domain.Invoice) bool {
		created = invoice
		return true
	})).Return(domain.Invoice{ID: uuid.New(), WorkspaceID: h.workspace, Kind: domain.KindTopup, Status: domain.StatusPending}, nil).Once()
	h.gateway.EXPECT().Name().Return("duitku").Maybe()
	h.gateway.EXPECT().Charge(mock.Anything, mock.Anything, mock.Anything).
		Return(domain.Charge{Reference: "bolu-t", Instructions: "Transfer ke BCA virtual account 123"}, nil).Maybe()
	h.store.EXPECT().SetInvoicePayment(mock.Anything, h.scope, mock.Anything).
		Return(domain.Invoice{ID: uuid.New(), Instructions: "Transfer ke BCA virtual account 123"}, nil).Once()

	invoice, err := h.service.BuyTopUp(t.Context(), h.scope, domain.TopUpRequest{PackageKey: "tambahan-200k"})
	require.NoError(t, err)
	require.Equal(t, domain.KindTopup, created.Kind)
	require.Equal(t, int64(25000), created.AmountIDR)
	require.Equal(t, int64(200000), created.TokenAmount)
	require.Equal(t, period.PeriodStart, created.PeriodStart, "a top-up is spent in the period it was bought in")
	require.Equal(t, period.PeriodEnd, created.PeriodEnd)
	require.Contains(t, invoice.Instructions, "virtual account")

	// A pack nobody sells is the store's own refusal, and an empty choice is the
	// caller's.
	_, err = h.service.BuyTopUp(t.Context(), h.scope, domain.TopUpRequest{})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	h.store.EXPECT().TopUp(mock.Anything, h.scope, "nobody").Return(domain.TopUpPackage{}, domain.ErrTopUpNotFound).Once()
	_, err = h.service.BuyTopUp(t.Context(), h.scope, domain.TopUpRequest{PackageKey: "nobody"})
	require.ErrorIs(t, err, domain.ErrTopUpNotFound)
}

// NoticePlanChangedForTest is the notice a package change writes, re-exported here so
// the test names it once.
const NoticePlanChangedForTest = "billing.plan_changed"
