package service_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/billing/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/billing/service"
)

// The periodic pass and the money are this file.
//
// The sweep is what makes the durable mirror honest and what issues each period's
// invoice, and it is the only place any of that happens — so what is under test is
// that one pass does each thing exactly once, and that a second pass does nothing. A
// repeated warning and a second invoice for one month are the two failures that would
// reach a customer.

// due is the sweep's starting read: this workspace is the only one due.
func (h *harness) due(t *testing.T) {
	t.Helper()
	h.store.EXPECT().DueWorkspaces(mock.Anything, mock.Anything, mock.Anything).
		Return([]uuid.UUID{h.workspace}, nil).Once()
}

// touched is the sweep rescheduling the workspace it just reconciled.
func (h *harness) touched() {
	h.store.EXPECT().TouchDirectory(mock.Anything, mock.Anything, h.workspace, mock.Anything).Return(nil).Maybe()
}

// TestApproachingTheAllowanceIsSaidOncePerThreshold: the warning is what gives a
// person time to buy a top-up, and a warning that arrived every pass would be a
// warning nobody reads.
func TestApproachingTheAllowanceIsSaidOncePerThreshold(t *testing.T) {
	h := newHarness(t)
	plan := h.plan() // 1000 tokens
	subscription := h.subscription(plan)
	period := h.period(subscription, plan, func(p *domain.QuotaPeriod) { p.TokensUsed = 100 })

	h.due(t)
	h.touched()
	h.live(subscription, plan)
	h.store.EXPECT().Period(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(period, nil).Once()
	// The counter says 800 of 1000, so the 80% threshold is crossed.
	h.counter.EXPECT().PeriodTokens(mock.Anything, h.workspace).Return(int64(800), nil).Maybe()
	h.usage.EXPECT().PeriodTokens(mock.Anything, h.workspace, mock.Anything).Return(int64(0), nil).Maybe()
	h.store.EXPECT().Outstanding(mock.Anything, mock.Anything).Return(nil, nil).Maybe()

	saved := period
	saved.TokensUsed = 800
	saved.Warning80SentAt = &h.clock.at
	h.store.EXPECT().SavePeriod(mock.Anything, mock.Anything, mock.MatchedBy(func(written domain.QuotaPeriod) bool {
		return written.TokensUsed == 800 && written.Warning80SentAt != nil && written.Warning100SentAt == nil
	})).Return(saved, nil).Once()

	h.notifier.EXPECT().Notify(mock.Anything, mock.Anything, mock.MatchedBy(func(notice domain.Notice) bool {
		require.Equal(t, domain.NoticeWarning, notice.Kind)
		require.Equal(t, domain.WarningThreshold, notice.Threshold)
		require.Equal(t, int64(800), notice.Used)
		require.Equal(t, int64(1000), notice.Allowance)
		require.Contains(t, notice.Reason, "80%")
		return true
	})).Return(nil).Once()

	result, err := h.service.Sweep(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, result.PeriodsSynced)
	require.Equal(t, 1, result.Warnings80)
	require.Zero(t, result.Warnings100)

	// The second pass sees the same usage and the mark that says it was reported, so
	// nothing is written and nothing is sent.
	h2 := newHarness(t)
	h2.clock.at = h.clock.at
	h2.due(t)
	h2.touched()
	h2.live(subscription, plan)
	already := h2.period(subscription, plan, func(p *domain.QuotaPeriod) {
		p.TokensUsed = 800
		p.Warning80SentAt = &h.clock.at
	})
	h2.store.EXPECT().Period(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(already, nil).Once()
	h2.counter.EXPECT().PeriodTokens(mock.Anything, h2.workspace).Return(int64(800), nil).Maybe()
	h2.usage.EXPECT().PeriodTokens(mock.Anything, h2.workspace, mock.Anything).Return(int64(0), nil).Maybe()
	h2.store.EXPECT().Outstanding(mock.Anything, mock.Anything).Return(nil, nil).Maybe()

	again, err := h2.service.Sweep(t.Context())
	require.NoError(t, err)
	require.Zero(t, again.PeriodsSynced, "nothing changed, so nothing was written")
	require.Zero(t, again.Warnings80)
}

// TestAPeriodThatJumpsBothThresholdsSaysBoth: a workspace can go from 70% to 105%
// between two passes, and the 80% message is the one that gives a person time to act —
// so it is sent as well as the one that says the allowance is gone.
func TestAPeriodThatJumpsBothThresholdsSaysBoth(t *testing.T) {
	h := newHarness(t)
	plan := h.plan()
	subscription := h.subscription(plan)
	period := h.period(subscription, plan)

	h.due(t)
	h.touched()
	h.live(subscription, plan)
	h.store.EXPECT().Period(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(period, nil).Once()
	h.counter.EXPECT().PeriodTokens(mock.Anything, h.workspace).Return(int64(1050), nil).Maybe()
	h.usage.EXPECT().PeriodTokens(mock.Anything, h.workspace, mock.Anything).Return(int64(0), nil).Maybe()
	h.store.EXPECT().Outstanding(mock.Anything, mock.Anything).Return(nil, nil).Maybe()

	saved := period
	saved.TokensUsed = 1050
	saved.Warning80SentAt = &h.clock.at
	saved.Warning100SentAt = &h.clock.at
	h.store.EXPECT().SavePeriod(mock.Anything, mock.Anything, mock.MatchedBy(func(written domain.QuotaPeriod) bool {
		return written.TokensUsed == 1050 && written.Warning80SentAt != nil && written.Warning100SentAt != nil
	})).Return(saved, nil).Once()

	var thresholds []int
	h.notifier.EXPECT().Notify(mock.Anything, mock.Anything, mock.MatchedBy(func(notice domain.Notice) bool {
		thresholds = append(thresholds, notice.Threshold)
		return true
	})).Return(nil).Twice()

	result, err := h.service.Sweep(t.Context())
	require.NoError(t, err)
	require.Equal(t, []int{domain.WarningThreshold, domain.ExhaustedThreshold}, thresholds)
	require.Equal(t, 1, result.Warnings80)
	require.Equal(t, 1, result.Warnings100)
}

// TestAPeriodThatEndsIssuesTheInvoiceForTheNextOne is the recurring half of the
// product: nothing is a mandate, so each period ends with an invoice for the next —
// priced at the package that will be in force then, which is the one a person chose
// while the invoice was outstanding.
func TestAPeriodThatEndsIssuesTheInvoiceForTheNextOne(t *testing.T) {
	h := newHarness(t)
	plan := h.plan()
	subscription := h.subscription(plan, func(s *domain.Subscription) {
		s.CurrentPeriodStart = h.clock.at.AddDate(0, 0, -30)
		s.CurrentPeriodEnd = h.clock.at.AddDate(0, 0, -1)
	})
	period := h.period(subscription, plan, func(p *domain.QuotaPeriod) { p.PeriodEnd = subscription.CurrentPeriodEnd })

	h.due(t)
	h.touched()
	h.live(subscription, plan)
	h.store.EXPECT().Period(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(period, nil).Once()
	// The counter reads zero here, and that is not the point of this test: what is
	// under test is the invoice and the transition.
	h.counter.EXPECT().PeriodTokens(mock.Anything, h.workspace).Return(int64(0), nil).Maybe()
	h.usage.EXPECT().PeriodTokens(mock.Anything, h.workspace, mock.Anything).Return(int64(0), nil).Maybe()
	h.store.EXPECT().Outstanding(mock.Anything, mock.Anything).Return(nil, nil).Once()

	// No invoice for the next period exists yet, so one is created: it starts when the
	// current period ends and runs for the package's own length.
	h.store.EXPECT().InvoiceForPeriod(mock.Anything, mock.Anything, subscription.ID, domain.KindSubscription, subscription.CurrentPeriodEnd).
		Return(domain.Invoice{}, domain.ErrNotFound).Once()

	var created domain.Invoice
	h.store.EXPECT().CreateInvoice(mock.Anything, mock.Anything, mock.MatchedBy(func(invoice domain.Invoice) bool {
		created = invoice
		return true
	})).Return(domain.Invoice{
		ID:          uuid.New(),
		WorkspaceID: h.workspace,
		Kind:        domain.KindSubscription,
		Status:      domain.StatusPending,
		AmountIDR:   plan.MonthlyPriceIDR,
	}, nil).Once()

	// A payment link is asked for and the provider is unavailable, which is a real
	// state: the invoice stands and the screen says to transfer.
	h.gateway.EXPECT().Name().Return("duitku").Maybe()
	h.gateway.EXPECT().Charge(mock.Anything, mock.Anything, mock.Anything).
		Return(domain.Charge{}, errors.New("the provider is down")).Maybe()
	h.store.EXPECT().SetInvoicePayment(mock.Anything, mock.Anything, mock.Anything).
		Return(domain.Invoice{}, nil).Maybe()

	// And the subscription moves past due, with the notice that says what to do.
	h.store.EXPECT().SetSubscription(mock.Anything, mock.Anything, mock.MatchedBy(func(updated domain.Subscription) bool {
		return updated.Status == domain.StatusPastDue && updated.PastDueSince != nil
	})).Return(subscription, nil).Once()

	var kinds []string
	h.notifier.EXPECT().Notify(mock.Anything, mock.Anything, mock.MatchedBy(func(notice domain.Notice) bool {
		kinds = append(kinds, notice.Kind)
		return true
	})).Return(nil).Maybe()

	result, err := h.service.Sweep(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, result.InvoicesCreated)
	require.Equal(t, 1, result.PastDue)

	require.Equal(t, subscription.CurrentPeriodEnd, created.PeriodStart, "the invoice covers the period that starts when this one ends")
	require.Equal(t, plan.MonthlyPriceIDR, created.AmountIDR)
	require.Equal(t, domain.KindSubscription, created.Kind)
	require.Equal(t, domain.StatusPending, created.Status)
	require.Contains(t, kinds, domain.NoticePastDue)
}

// TestATrialIsNotRenewedAndSaysWhatToDo is the one package that must not repeat: it
// has no price, so re-issuing it would be a free month every month.
func TestATrialIsNotRenewedAndSaysWhatToDo(t *testing.T) {
	h := newHarness(t)
	plan := h.plan(func(p *domain.Plan) {
		p.Key = "coba"
		p.Name = "Paket Coba"
		p.MonthlyPriceIDR = 0
		p.IsTrial = true
		p.PeriodDays = 14
	})
	subscription := h.subscription(plan, func(s *domain.Subscription) {
		s.Status = domain.StatusTrial
		s.CurrentPeriodStart = h.clock.at.AddDate(0, 0, -14)
		s.CurrentPeriodEnd = h.clock.at.AddDate(0, 0, -1)
	})
	period := h.period(subscription, plan)

	h.due(t)
	h.touched()
	h.live(subscription, plan)
	h.store.EXPECT().Period(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(period, nil).Once()
	h.counter.EXPECT().PeriodTokens(mock.Anything, h.workspace).Return(int64(0), nil).Maybe()
	h.usage.EXPECT().PeriodTokens(mock.Anything, h.workspace, mock.Anything).Return(int64(0), nil).Maybe()
	h.store.EXPECT().Outstanding(mock.Anything, mock.Anything).Return(nil, nil).Once()

	// No invoice is asked for and none is created.
	h.store.EXPECT().InvoiceForPeriod(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(domain.Invoice{}, domain.ErrNotFound).Once()
	h.store.EXPECT().SetSubscription(mock.Anything, mock.Anything, mock.MatchedBy(func(updated domain.Subscription) bool {
		return updated.Status == domain.StatusPastDue
	})).Return(subscription, nil).Once()

	var reasons []string
	h.notifier.EXPECT().Notify(mock.Anything, mock.Anything, mock.MatchedBy(func(notice domain.Notice) bool {
		reasons = append(reasons, notice.Reason)
		return true
	})).Return(nil).Maybe()

	result, err := h.service.Sweep(t.Context())
	require.NoError(t, err)
	require.Zero(t, result.InvoicesCreated)
	require.Equal(t, 1, result.PastDue)

	require.NotEmpty(t, reasons)
	require.Contains(t, reasons[0], "belum dibayar")
}

// TestAPastDueInvoiceIsExpiredRatherThanLeftOpen: a payment link that ran out is
// closed, so the period can be re-issued — the partial index allows exactly that, and
// leaving the invoice pending would block the month forever.
func TestAPastDueInvoiceIsExpiredRatherThanLeftOpen(t *testing.T) {
	h := newHarness(t)
	plan := h.plan()
	subscription := h.subscription(plan, func(s *domain.Subscription) {
		s.CurrentPeriodStart = h.clock.at.AddDate(0, 0, -30)
		s.CurrentPeriodEnd = h.clock.at.AddDate(0, 0, -1)
		s.PastDueSince = timePointer(h.clock.at.AddDate(0, 0, -1))
		s.PastDueNotifiedAt = timePointer(h.clock.at.AddDate(0, 0, -1))
	})
	period := h.period(subscription, plan)

	expired := h.clock.at.Add(-2 * time.Hour)
	stale := domain.Invoice{
		ID:          uuid.New(),
		WorkspaceID: h.workspace,
		Kind:        domain.KindSubscription,
		Status:      domain.StatusPending,
		AmountIDR:   plan.MonthlyPriceIDR,
		PeriodStart: subscription.CurrentPeriodEnd,
		ExpiresAt:   &expired,
		PeriodEnd:   subscription.CurrentPeriodEnd.AddDate(0, 0, 30),
	}

	h.due(t)
	h.touched()
	h.live(subscription, plan)
	h.store.EXPECT().Period(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(period, nil).Once()
	h.counter.EXPECT().PeriodTokens(mock.Anything, h.workspace).Return(int64(0), nil).Maybe()
	h.usage.EXPECT().PeriodTokens(mock.Anything, h.workspace, mock.Anything).Return(int64(0), nil).Maybe()
	// The expired one is the only thing outstanding, so it is closed and then the
	// period has an invoice again.
	h.store.EXPECT().Outstanding(mock.Anything, mock.Anything).Return([]domain.Invoice{stale}, nil).Once()
	h.store.EXPECT().ExpireInvoice(mock.Anything, mock.Anything, stale.ID).
		Return(stale, true, nil).Once()
	h.store.EXPECT().InvoiceForPeriod(mock.Anything, mock.Anything, subscription.ID, domain.KindSubscription, subscription.CurrentPeriodEnd).
		Return(domain.Invoice{}, domain.ErrNotFound).Once()
	h.store.EXPECT().CreateInvoice(mock.Anything, mock.Anything, mock.Anything).
		Return(domain.Invoice{ID: uuid.New(), WorkspaceID: h.workspace, Status: domain.StatusPending}, nil).Once()
	h.gateway.EXPECT().Name().Return("duitku").Maybe()
	h.gateway.EXPECT().Charge(mock.Anything, mock.Anything, mock.Anything).Return(domain.Charge{}, nil).Maybe()
	h.store.EXPECT().SetInvoicePayment(mock.Anything, mock.Anything, mock.Anything).Return(domain.Invoice{}, nil).Maybe()
	h.store.EXPECT().SetSubscription(mock.Anything, mock.Anything, mock.Anything).Return(subscription, nil).Maybe()
	h.notifier.EXPECT().Notify(mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	result, err := h.service.Sweep(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, result.InvoicesExpired)
	require.Equal(t, 1, result.InvoicesCreated)
}

// TestTheGraceWindowIsSaidOnceAndThenTheStopIs is the pair of notices that mark the
// two moments a person can act. They are remembered with a marker because the state
// itself is derived: without one, every pass would repeat the same sentence.
func TestTheGraceWindowIsSaidOnceAndThenTheStopIs(t *testing.T) {
	h := newHarness(t)
	plan := h.plan()
	subscription := h.subscription(plan, func(s *domain.Subscription) {
		s.CurrentPeriodStart = h.clock.at.AddDate(0, 0, -40)
		s.CurrentPeriodEnd = h.clock.at.AddDate(0, 0, -10)
		s.PastDueSince = timePointer(h.clock.at.AddDate(0, 0, -10))
		s.PastDueNotifiedAt = timePointer(h.clock.at.AddDate(0, 0, -10))
	})
	period := h.period(subscription, plan)

	h.due(t)
	h.touched()
	h.live(subscription, plan)
	h.store.EXPECT().Period(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(period, nil).Once()
	h.counter.EXPECT().PeriodTokens(mock.Anything, h.workspace).Return(int64(0), nil).Maybe()
	h.usage.EXPECT().PeriodTokens(mock.Anything, h.workspace, mock.Anything).Return(int64(0), nil).Maybe()
	h.store.EXPECT().Outstanding(mock.Anything, mock.Anything).Return(nil, nil).Once()
	h.store.EXPECT().InvoiceForPeriod(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(domain.Invoice{}, domain.ErrNotFound).Once()
	h.store.EXPECT().CreateInvoice(mock.Anything, mock.Anything, mock.Anything).
		Return(domain.Invoice{ID: uuid.New(), WorkspaceID: h.workspace, Status: domain.StatusPending}, nil).Once()
	h.gateway.EXPECT().Name().Return("duitku").Maybe()
	h.gateway.EXPECT().Charge(mock.Anything, mock.Anything, mock.Anything).Return(domain.Charge{}, nil).Maybe()
	h.store.EXPECT().SetInvoicePayment(mock.Anything, mock.Anything, mock.Anything).Return(domain.Invoice{}, nil).Maybe()

	// The workspace is ten days past its period and the grace window is seven, so it
	// has stopped — and the notice says so once.
	h.store.EXPECT().SetSubscription(mock.Anything, mock.Anything, mock.MatchedBy(func(updated domain.Subscription) bool {
		return updated.ReadOnlyNotifiedAt != nil
	})).Return(subscription, nil).Once()

	var readOnly int
	h.notifier.EXPECT().Notify(mock.Anything, mock.Anything, mock.MatchedBy(func(notice domain.Notice) bool {
		if notice.Kind == domain.NoticeReadOnly {
			readOnly++
			require.Contains(t, notice.Reason, "mode baca saja")
			require.Contains(t, notice.Reason, "Tidak ada data yang dihapus")
		}
		return true
	})).Return(nil).Maybe()

	result, err := h.service.Sweep(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, readOnly)
	require.Equal(t, 1, result.ReadOnly)
	require.Zero(t, result.PastDue, "the past due notice was already sent and marked")
}

// TestOneWorkspaceThatCannotBeReconciledDoesNotStopThePass: the rest of the fleet is
// waiting, and a row that cannot be read is retried on the interval rather than in a
// tight loop.
func TestOneWorkspaceThatCannotBeReconciledDoesNotStopThePass(t *testing.T) {
	h := newHarness(t)
	other := uuid.New()

	h.store.EXPECT().DueWorkspaces(mock.Anything, mock.Anything, mock.Anything).
		Return([]uuid.UUID{h.workspace, other}, nil).Once()
	// The first workspace cannot be read at all.
	h.store.EXPECT().Subscription(mock.Anything, mock.Anything).
		Return(domain.Subscription{}, errors.New("the subscription store is down")).Maybe()
	// Both are rescheduled, which is what makes the failure a delay rather than a
	// workspace nobody visits again.
	h.store.EXPECT().TouchDirectory(mock.Anything, mock.Anything, h.workspace, mock.Anything).Return(nil).Once()
	h.store.EXPECT().TouchDirectory(mock.Anything, mock.Anything, other, mock.Anything).Return(nil).Once()

	result, err := h.service.Sweep(t.Context())
	require.NoError(t, err, "one broken workspace is not a failed pass")
	require.Zero(t, result.PeriodsSynced)
}

// TestAPaymentIsAppliedOnceNoMatterHowOftenItArrives is the epic's third gate. A
// provider delivers a notification until somebody acknowledges it, so the same payment
// arrives twice on purpose — and the second one must not buy a second month.
func TestAPaymentIsAppliedOnceNoMatterHowOftenItArrives(t *testing.T) {
	h := newHarness(t)
	plan := h.plan()
	subscription := h.subscription(plan)

	invoice := domain.Invoice{
		ID:                uuid.New(),
		WorkspaceID:       h.workspace,
		SubscriptionID:    subscription.ID,
		Kind:              domain.KindSubscription,
		Status:            domain.StatusPending,
		AmountIDR:         plan.MonthlyPriceIDR,
		PeriodStart:       subscription.CurrentPeriodEnd,
		PeriodEnd:         subscription.CurrentPeriodEnd.AddDate(0, 0, 30),
		Provider:          "duitku",
		ProviderReference: "bolu-invoice-m",
	}

	paid := h.clock.at
	settled := invoice
	settled.Status = domain.StatusPaid
	settled.PaidAt = &paid

	h.gateway.EXPECT().Name().Return("duitku").Maybe()
	h.gateway.EXPECT().Verify(mock.Anything, mock.Anything).
		Return(domain.Payment{Reference: invoice.ProviderReference, AmountIDR: plan.MonthlyPriceIDR, Paid: true, PaidAt: paid}, nil).Once()
	h.store.EXPECT().PaymentRoute(mock.Anything, "duitku", invoice.ProviderReference).
		Return(domain.PaymentRoute{WorkspaceID: invoice.WorkspaceID, InvoiceID: invoice.ID}, nil).Once()
	h.store.EXPECT().Invoice(mock.Anything, mock.Anything, invoice.ID).
		Return(invoice, nil).Once()

	// The first delivery settles it and extends the period.
	h.store.EXPECT().SettleInvoice(mock.Anything, mock.Anything, mock.Anything).
		Return(settled, true, nil).Once()
	h.store.EXPECT().PlanByID(mock.Anything, subscription.PlanID).Return(plan, nil).Maybe()
	h.store.EXPECT().Subscription(mock.Anything, mock.Anything).Return(subscription, nil).Once()
	h.store.EXPECT().SetSubscription(mock.Anything, mock.Anything, mock.MatchedBy(func(updated domain.Subscription) bool {
		return updated.Status == domain.StatusActive &&
			updated.CurrentPeriodStart.Equal(invoice.PeriodStart) &&
			updated.CurrentPeriodEnd.Equal(invoice.PeriodEnd) &&
			updated.PastDueSince == nil
	})).Return(subscription, nil).Once()
	// What the workspace no longer owes is closed: the invoice that was just paid is not
	// among them, and a top-up never is.
	h.store.EXPECT().Outstanding(mock.Anything, mock.Anything).Return([]domain.Invoice{
		settled,
		{ID: uuid.New(), Kind: domain.KindTopup, Status: domain.StatusPending, AmountIDR: 25000},
	}, nil).Once()
	h.store.EXPECT().Period(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(h.period(subscription, plan), nil).Once()
	h.notifier.EXPECT().Notify(mock.Anything, mock.Anything, mock.MatchedBy(func(notice domain.Notice) bool {
		return notice.Kind == domain.NoticePaid
	})).Return(nil).Once()

	require.NoError(t, h.service.HandlePayment(t.Context(), domain.Delivery{Provider: "duitku", Body: []byte("{}")}))

	// The second delivery settles nothing: the invoice is no longer pending, so the
	// period is not extended again and no notice is sent.
	h.gateway.EXPECT().Verify(mock.Anything, mock.Anything).
		Return(domain.Payment{Reference: invoice.ProviderReference, AmountIDR: plan.MonthlyPriceIDR, Paid: true}, nil).Once()
	h.store.EXPECT().PaymentRoute(mock.Anything, "duitku", invoice.ProviderReference).
		Return(domain.PaymentRoute{WorkspaceID: invoice.WorkspaceID, InvoiceID: invoice.ID}, nil).Once()
	h.store.EXPECT().Invoice(mock.Anything, mock.Anything, invoice.ID).
		Return(invoice, nil).Once()
	h.store.EXPECT().SettleInvoice(mock.Anything, mock.Anything, mock.Anything).
		Return(settled, false, nil).Once()
	require.NoError(t, h.service.HandlePayment(t.Context(), domain.Delivery{Provider: "duitku", Body: []byte("{}")}))
}

// TestAPaymentSmallerThanTheInvoiceDoesNotBuyTheMonth: an amount that does not match
// is not a payment of it, and extending the package for less money than it costs would
// be the one mistake a workspace could make on purpose.
func TestAPaymentSmallerThanTheInvoiceDoesNotBuyTheMonth(t *testing.T) {
	h := newHarness(t)
	invoice := domain.Invoice{
		ID:                uuid.New(),
		WorkspaceID:       h.workspace,
		SubscriptionID:    uuid.New(),
		Kind:              domain.KindSubscription,
		Status:            domain.StatusPending,
		AmountIDR:         149000,
		Provider:          "duitku",
		ProviderReference: "bolu-underpaid-m",
	}

	h.gateway.EXPECT().Name().Return("duitku").Maybe()
	h.gateway.EXPECT().Verify(mock.Anything, mock.Anything).
		Return(domain.Payment{Reference: invoice.ProviderReference, AmountIDR: 1000, Paid: true}, nil).Once()
	h.store.EXPECT().PaymentRoute(mock.Anything, "duitku", invoice.ProviderReference).
		Return(domain.PaymentRoute{WorkspaceID: invoice.WorkspaceID, InvoiceID: invoice.ID}, nil).Once()
	h.store.EXPECT().Invoice(mock.Anything, mock.Anything, invoice.ID).
		Return(invoice, nil).Once()
	// Nothing settles and nothing is topped up. No expectation is set for either,
	// and the mock is strict: an unexpected call fails this test.

	err := h.service.HandlePayment(t.Context(), domain.Delivery{Provider: "duitku", Body: []byte("{}")})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
	require.Contains(t, err.Error(), "lebih kecil")
}

// TestATopUpLandsInThePeriodItWasBoughtIn: the tokens are added to the period the
// purchase names, and they do not carry into the next one — a top-up is what a
// workspace buys when it misjudged a month.
func TestATopUpLandsInThePeriodItWasBoughtIn(t *testing.T) {
	h := newHarness(t)
	plan := h.plan()
	subscription := h.subscription(plan)
	period := h.period(subscription, plan, func(p *domain.QuotaPeriod) { p.TokensToppedUp = 200000 })

	invoice := domain.Invoice{
		ID:                uuid.New(),
		WorkspaceID:       h.workspace,
		SubscriptionID:    subscription.ID,
		Kind:              domain.KindTopup,
		Status:            domain.StatusPending,
		AmountIDR:         25000,
		TokenAmount:       200000,
		PeriodStart:       period.PeriodStart,
		PeriodEnd:         period.PeriodEnd,
		Provider:          "duitku",
		ProviderReference: "bolu-invoice-t",
	}

	paid := h.clock.at
	settled := invoice
	settled.Status = domain.StatusPaid
	settled.PaidAt = &paid

	h.gateway.EXPECT().Name().Return("duitku").Maybe()
	h.gateway.EXPECT().Verify(mock.Anything, mock.Anything).
		Return(domain.Payment{Reference: invoice.ProviderReference, AmountIDR: 25000, Paid: true, PaidAt: paid}, nil).Once()
	h.store.EXPECT().PaymentRoute(mock.Anything, "duitku", invoice.ProviderReference).
		Return(domain.PaymentRoute{WorkspaceID: invoice.WorkspaceID, InvoiceID: invoice.ID}, nil).Once()
	h.store.EXPECT().Invoice(mock.Anything, mock.Anything, invoice.ID).
		Return(invoice, nil).Once()
	h.store.EXPECT().SettleInvoice(mock.Anything, mock.Anything, mock.Anything).Return(settled, true, nil).Once()

	h.store.EXPECT().AddTopUp(mock.Anything, mock.Anything, period.PeriodStart, int64(200000)).
		Return(period, nil).Once()

	var reason string
	h.notifier.EXPECT().Notify(mock.Anything, mock.Anything, mock.MatchedBy(func(notice domain.Notice) bool {
		reason = notice.Reason
		return notice.Kind == domain.NoticeTopUp
	})).Return(nil).Once()

	require.NoError(t, h.service.HandlePayment(t.Context(), domain.Delivery{Provider: "duitku", Body: []byte("{}")}))
	require.Contains(t, reason, "200.000")
	require.Contains(t, reason, "masuk")

	// And nothing about the subscription is touched by a top-up: it is not a package
	// change and it does not move the period.
}

// TestAnExpiredLinkIsClosedAndNotAPayment: a link that ran out is a different event
// from a payment that failed, and treating it as a failure would mark an invoice the
// workspace could still pay.
func TestAnExpiredLinkIsClosedAndNotAPayment(t *testing.T) {
	h := newHarness(t)
	invoice := domain.Invoice{
		ID:                uuid.New(),
		WorkspaceID:       h.workspace,
		Kind:              domain.KindSubscription,
		Status:            domain.StatusPending,
		Provider:          "duitku",
		ProviderReference: "bolu-expired-m",
		AmountIDR:         149000,
	}

	h.gateway.EXPECT().Name().Return("duitku").Maybe()
	h.gateway.EXPECT().Verify(mock.Anything, mock.Anything).
		Return(domain.Payment{Reference: invoice.ProviderReference, Expired: true}, nil).Once()
	h.store.EXPECT().PaymentRoute(mock.Anything, "duitku", invoice.ProviderReference).
		Return(domain.PaymentRoute{WorkspaceID: invoice.WorkspaceID, InvoiceID: invoice.ID}, nil).Once()
	h.store.EXPECT().Invoice(mock.Anything, mock.Anything, invoice.ID).
		Return(invoice, nil).Once()
	h.store.EXPECT().ExpireInvoice(mock.Anything, mock.Anything, invoice.ID).Return(invoice, true, nil).Once()

	require.NoError(t, h.service.HandlePayment(t.Context(), domain.Delivery{Provider: "duitku", Body: []byte("{}")}))

	// A declined payment is settled as failed, which keeps the reason on the row.
	h2 := newHarness(t)
	declined := invoice
	h2.gateway.EXPECT().Name().Return("duitku").Maybe()
	h2.gateway.EXPECT().Verify(mock.Anything, mock.Anything).
		Return(domain.Payment{Reference: declined.ProviderReference}, nil).Once()
	h2.store.EXPECT().PaymentRoute(mock.Anything, "duitku", declined.ProviderReference).
		Return(domain.PaymentRoute{WorkspaceID: declined.WorkspaceID, InvoiceID: declined.ID}, nil).Once()
	h2.store.EXPECT().Invoice(mock.Anything, mock.Anything, declined.ID).
		Return(declined, nil).Once()
	h2.store.EXPECT().SettleInvoice(mock.Anything, mock.Anything, mock.MatchedBy(func(settled domain.Invoice) bool {
		return settled.Status == domain.StatusFailed
	})).Return(declined, true, nil).Once()

	require.NoError(t, h2.service.HandlePayment(t.Context(), domain.Delivery{Provider: "duitku", Body: []byte("{}")}))
}

// TestADeliveryForAnotherProviderCannotBeVerified: the path names a provider, and one
// this deployment does not speak cannot be checked — so it is refused rather than
// parsed hopefully.
func TestADeliveryForAnotherProviderCannotBeVerified(t *testing.T) {
	h := newHarness(t)

	h.gateway.EXPECT().Name().Return("duitku").Maybe()

	err := h.service.HandlePayment(t.Context(), domain.Delivery{Provider: "xendit", Body: []byte("amount=1")})
	require.ErrorIs(t, err, domain.ErrNoGateway)
}

// TestADeliveryThatCannotBeVerifiedIsTheCallersPayload: a signature that does not match is
// not this deployment's failure, and the distinction reaches the provider — a gateway told
// "internal error" would keep delivering a body that can never verify.
func TestADeliveryThatCannotBeVerifiedIsTheCallersPayload(t *testing.T) {
	h := newHarness(t)

	h.gateway.EXPECT().Name().Return("duitku").Maybe()
	h.gateway.EXPECT().Verify(mock.Anything, mock.Anything).
		Return(domain.Payment{}, errors.New("duitku: the callback signature does not match")).Once()
	// Nothing is looked up: an unverifiable delivery never reaches the invoice. No
	// expectation is set for the routing read, and the mock is strict.

	err := h.service.HandlePayment(t.Context(), domain.Delivery{Provider: "duitku", Body: []byte("amount=1")})

	require.ErrorIs(t, err, domain.ErrUnverifiable)
	require.Contains(t, err.Error(), "signature does not match", "the provider's own words survive")
}

// TestAProviderThatIsNotConfiguredIsSAidSo: no gateway means the invoice is still
// written and the screen says to transfer; a webhook has nothing to verify with.
func TestAProviderThatIsNotConfiguredIsSAidSo(t *testing.T) {
	h := newHarness(t, func(deps *service.Deps) { deps.Gateway = nil })

	err := h.service.HandlePayment(t.Context(), domain.Delivery{Body: []byte("{}")})
	require.ErrorIs(t, err, domain.ErrNoGateway)
}

func timePointer(at time.Time) *time.Time { return &at }

// TestAnInvoiceThePaymentSupersededIsWithdrawn: a period that ends while an invoice is
// outstanding can be issued a second one — the sweep runs on an interval and every pass
// that finds the period ended asks — so a workspace that has paid must not be shown an
// amount for a month it is now inside. A top-up is never withdrawn: what somebody bought
// on top of a package is not affected by paying for the package.
func TestAnInvoiceThePaymentSupersededIsWithdrawn(t *testing.T) {
	h := newHarness(t)
	plan := h.plan()
	subscription := h.subscription(plan)

	paid := domain.Invoice{
		ID: uuid.New(), WorkspaceID: h.workspace, SubscriptionID: subscription.ID,
		Kind: domain.KindSubscription, Status: domain.StatusPending, AmountIDR: plan.MonthlyPriceIDR,
		PeriodStart: subscription.CurrentPeriodEnd, PeriodEnd: subscription.CurrentPeriodEnd.AddDate(0, 0, 30),
		Provider: "duitku", ProviderReference: "bolu-paid-m",
	}
	stale := paid
	stale.ID = uuid.New()
	stale.PeriodStart = paid.PeriodStart.AddDate(0, 0, 1)
	stale.PeriodEnd = paid.PeriodEnd.AddDate(0, 0, 1)
	topUp := stale
	topUp.ID = uuid.New()
	topUp.Kind = domain.KindTopup
	topUp.TokenAmount = 200000

	settled := paid
	settled.Status = domain.StatusPaid
	at := h.clock.at
	settled.PaidAt = &at

	h.gateway.EXPECT().Name().Return("duitku").Maybe()
	h.gateway.EXPECT().Verify(mock.Anything, mock.Anything).
		Return(domain.Payment{Reference: paid.ProviderReference, AmountIDR: plan.MonthlyPriceIDR, Paid: true, PaidAt: at}, nil).Once()
	h.store.EXPECT().PaymentRoute(mock.Anything, "duitku", paid.ProviderReference).
		Return(domain.PaymentRoute{WorkspaceID: h.workspace, InvoiceID: paid.ID}, nil).Once()
	h.store.EXPECT().Invoice(mock.Anything, mock.Anything, paid.ID).Return(paid, nil).Once()
	h.store.EXPECT().SettleInvoice(mock.Anything, mock.Anything, mock.Anything).Return(settled, true, nil).Once()

	h.store.EXPECT().PlanByID(mock.Anything, subscription.PlanID).Return(plan, nil).Maybe()
	h.store.EXPECT().Subscription(mock.Anything, mock.Anything).Return(subscription, nil).Once()
	h.store.EXPECT().SetSubscription(mock.Anything, mock.Anything, mock.Anything).Return(subscription, nil).Once()
	h.store.EXPECT().Period(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(h.period(subscription, plan), nil).Once()
	h.store.EXPECT().Outstanding(mock.Anything, mock.Anything).
		Return([]domain.Invoice{settled, stale, topUp}, nil).Once()

	// The stale month is closed, and the top-up is left alone.
	h.store.EXPECT().ExpireInvoice(mock.Anything, mock.Anything, stale.ID).Return(stale, true, nil).Once()
	h.notifier.EXPECT().Notify(mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	require.NoError(t, h.service.HandlePayment(t.Context(), domain.Delivery{Provider: "duitku", Body: []byte("{}")}))
}
