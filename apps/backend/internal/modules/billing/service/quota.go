package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/billing/domain"
)

// The quota half: what a workspace may spend, and the reconciliation that keeps the
// durable mirror honest.
//
// Two mirrors hold the same number, and they are not interchangeable. The live
// counter in Redis answers the hot path — it is read before every model call — and
// the ledger is the durable record. When they disagree the larger one wins, which is
// the direction that never hands out a fresh allowance: a Redis that lost its keys
// must not let a workspace spend its month again, and a ledger write that lagged must
// not either.

// Check answers whether a workspace may spend right now.
//
// It is asked twice at two boundaries — once by the gateway before every model call,
// and once by the task runtime at a clean round boundary — and the two answers have
// to agree, which is why there is one implementation rather than two.
func (s *Service) Check(ctx context.Context, workspaceID uuid.UUID) (domain.Check, error) {
	subscription, plan, err := s.live(ctx, workspaceID)
	if err != nil {
		return domain.Check{}, err
	}

	now := s.now()
	scope := domain.Scope{WorkspaceID: workspaceID}

	check := domain.Check{
		Allowed:         true,
		Entitlement:     subscription.Entitlement(now, s.grace),
		DailyCostMicros: plan.DailyCostMicros,
	}

	// A workspace past its grace window may not spend or write, and the answer says
	// which of the two it is: "you are read-only" is a different instruction from
	// "you are out of tokens", and a person acts differently on each.
	if check.Entitlement == domain.EntitlementReadOnly {
		check.Allowed = false
		return check, fmt.Errorf("%w: workspace %s", domain.ErrReadOnly, shortID(workspaceID))
	}

	period, err := s.deps.Repository.Period(ctx, scope, subscription, plan, now)
	if err != nil {
		return domain.Check{}, err
	}
	check.Period = period

	// The stored usage is as fresh as the last reconciliation; the live counter is
	// as fresh as the last call. The check reads the live one, because refusing a
	// call that has already been paid for is worse than allowing one that will be
	// accounted for a minute later.
	used, err := s.usedTokens(ctx, workspaceID, period.PeriodStart)
	if err != nil {
		return domain.Check{}, err
	}
	check.Period.TokensUsed = used

	allowance := period.Allowance()
	if allowance > 0 && used >= allowance {
		check.Allowed = false
		return check, fmt.Errorf("%w: %d dari %d token terpakai periode ini; beli tambahan atau tunggu periode berikutnya",
			domain.ErrQuotaExceeded, used, allowance)
	}

	check.SpentTodayMicros = s.dailyCost(ctx, workspaceID)
	if check.DailyExhausted() {
		check.Allowed = false
		return check, fmt.Errorf("%w: batas biaya harian %d mikro-rupiah tercapai; tugas baru bisa jalan lagi besok",
			domain.ErrQuotaExceeded, check.DailyCostMicros)
	}

	return check, nil
}

// usedTokens is the tokens spent in a period: the live counter when it answers, and
// the ledger when it does not.
//
// The two are compared rather than one preferred, because either can be the one
// telling the truth. A flushed Redis under-reports and the ledger corrects it; a
// ledger write that has not landed yet under-reports and the counter corrects it.
// Taking the larger of the two is what makes an outage cost accuracy in the safe
// direction.
func (s *Service) usedTokens(ctx context.Context, workspaceID uuid.UUID, since time.Time) (int64, error) {
	live, liveErr := s.counterTokens(ctx, workspaceID)
	ledger, ledgerErr := s.ledgerTokens(ctx, workspaceID, since)

	switch {
	case liveErr != nil && ledgerErr != nil:
		// Neither answered, so nothing here can answer either. Returning zero would
		// hand the workspace a fresh allowance, which is the one thing this must
		// never do.
		return 0, fmt.Errorf("billing: reading the spent tokens: %w", liveErr)
	case liveErr != nil:
		return ledger, nil //nolint:nilerr // deliberate: the counter is optional and the ledger answered
	case ledgerErr != nil:
		return live, nil //nolint:nilerr // deliberate: the ledger is optional and the counter answered
	}

	if ledger > live {
		return ledger, nil
	}
	return live, nil
}

// counterTokens reads the live counter, or zero when none is configured.
func (s *Service) counterTokens(ctx context.Context, workspaceID uuid.UUID) (int64, error) {
	if s.deps.Counter == nil {
		return 0, errNoCounter
	}
	return s.deps.Counter.PeriodTokens(ctx, workspaceID)
}

// ledgerTokens reads the ledger, or zero when no reader is configured.
func (s *Service) ledgerTokens(ctx context.Context, workspaceID uuid.UUID, since time.Time) (int64, error) {
	if s.deps.Usage == nil {
		return 0, errNoLedger
	}
	return s.deps.Usage.PeriodTokens(ctx, workspaceID, since)
}

// dailyCost is the cost spent today, from whichever source answers. A failure is
// reported as zero here rather than as an error, and that is deliberate: the day's
// ceiling is the second bound, and a workspace must not be refused every call
// because the day's total could not be read — the period's allowance still holds.
func (s *Service) dailyCost(ctx context.Context, workspaceID uuid.UUID) int64 {
	if s.deps.Counter != nil {
		if cost, err := s.deps.Counter.DailyCost(ctx, workspaceID); err == nil {
			return cost
		}
	}
	if s.deps.Usage != nil {
		if cost, err := s.deps.Usage.DailyCost(ctx, workspaceID); err == nil {
			// The live counter wins when it answers, because it is the one the day's
			// bound is enforced against; the ledger is what stops an outage from
			// erasing the day.
			return cost
		}
	}
	return 0
}

// Sweep runs one reconciliation pass over every workspace that is due.
//
// This is the periodic half of the design, and the reason it exists at all: the hot
// path can only read, so something has to write. One pass does four things —
//
//   - syncs the live counter into `quota_periods`, which is what the screen shows;
//   - sends the 80% and 100% warnings, once per period per threshold;
//   - issues the invoice for the next period when the current one ends, and moves the
//     subscription past due;
//   - expires the payment links that ran out.
//
// It is a loop rather than a durable workflow because nothing in it waits: every
// state it computes is derived from the times, so a pass that never ran is corrected
// by the next one rather than leaving a workspace in a state nobody will revisit.
// `billing_directory.next_sync_at` is what makes that true — a missed pass is a pass
// that is still due.
func (s *Service) Sweep(ctx context.Context) (domain.SweepResult, error) {
	var result domain.SweepResult

	if s.deps.Repository == nil {
		return result, errNoRepository
	}

	now := s.now()
	due, err := s.deps.Repository.DueWorkspaces(ctx, now, s.cfg.SweepLimit)
	if err != nil {
		return result, err
	}

	for _, workspaceID := range due {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if err := s.sweepWorkspace(ctx, workspaceID, now, &result); err != nil {
			// One workspace that cannot be reconciled must not stop the pass: the
			// rest of the fleet is waiting, and this one will be due again in an
			// interval.
			s.deps.Logger.Warn("could not reconcile a workspace's billing",
				zap.String("workspace", workspaceID.String()), zap.Error(err))
		}

		// The next pass is scheduled whether or not this one worked, so a workspace
		// that failed is retried on the interval rather than in a tight loop.
		scope := domain.Scope{WorkspaceID: workspaceID}
		if err := s.deps.Repository.TouchDirectory(ctx, scope, workspaceID, now.Add(s.cfg.SyncInterval)); err != nil {
			s.deps.Logger.Warn("could not schedule a workspace's next reconciliation",
				zap.String("workspace", workspaceID.String()), zap.Error(err))
		}
	}

	return result, nil
}

// sweepWorkspace reconciles one workspace.
func (s *Service) sweepWorkspace(ctx context.Context, workspaceID uuid.UUID, now time.Time, result *domain.SweepResult) error {
	scope := domain.Scope{WorkspaceID: workspaceID}

	subscription, plan, err := s.live(ctx, workspaceID)
	if err != nil {
		return err
	}

	period, err := s.deps.Repository.Period(ctx, scope, subscription, plan, now)
	if err != nil {
		return err
	}

	// 1. The durable mirror catches up with the ledger, and the thresholds that
	// catch-up crossed are marked in the same write.
	//
	// One write rather than two: a pass that synced and then marked would write the
	// same row twice, and a crash between them would leave the usage recorded and the
	// warning unmarked — which is a warning that repeats.
	used, err := s.usedTokens(ctx, workspaceID, period.PeriodStart)
	if err != nil {
		return err
	}

	synced := used != period.TokensUsed
	period.TokensUsed = used

	crossed := domain.WarningsAt(period).Claim()
	markedAt := s.now()
	for _, threshold := range crossed {
		switch threshold {
		case domain.WarningThreshold:
			period.Warning80SentAt = &markedAt
		case domain.ExhaustedThreshold:
			period.Warning100SentAt = &markedAt
		}
	}

	if synced || len(crossed) > 0 {
		if period, err = s.deps.Repository.SavePeriod(ctx, scope, period); err != nil {
			return err
		}
		if synced {
			result.PeriodsSynced++
		}
	}

	// 2. The thresholds are reported, and counted, once the marks are down. Sending
	// first and marking after would repeat a warning on every pass until one got
	// through; marking first and sending after repeats it only if the notifier is
	// down, which is the direction that loses less.
	s.warn(ctx, scope, subscription, plan, period, crossed, result)

	// 3. The invoices: what ran out is closed, and what is owed is issued.
	if err := s.settleInvoices(ctx, scope, subscription, plan, now, result); err != nil {
		return err
	}

	// 4. The transitions, which are what tell a person their workspace stopped.
	return s.reportTransitions(ctx, scope, subscription, plan, now, result)
}

// warn sends the threshold warnings a period crossed, in order.
//
// The order matters: a period can jump from well under the allowance to over it
// between two passes, and the 80% message is the one that gives a person time to act.
// Sending only the last threshold crossed would tell them their workspace has already
// stopped.
func (s *Service) warn(ctx context.Context, scope domain.Scope, subscription domain.Subscription, plan domain.Plan, period domain.QuotaPeriod, crossed []int, result *domain.SweepResult) {
	for _, threshold := range crossed {
		s.notify(ctx, scope, domain.Notice{
			Kind:        domain.NoticeWarning,
			Threshold:   threshold,
			PlanName:    plan.Name,
			Used:        period.TokensUsed,
			Allowance:   period.Allowance(),
			Entitlement: subscription.Entitlement(s.now(), s.grace),
			Reason:      reasonForThreshold(threshold, period),
		})

		switch threshold {
		case domain.WarningThreshold:
			result.Warnings80++
		case domain.ExhaustedThreshold:
			result.Warnings100++
		}
	}
}

// reasonForThreshold is what a person reads, and it says what to do next.
//
// "You are at 80%" is a number; "and here is what happens at 100%" is an instruction.
// The two notices differ because the two moments do.
func reasonForThreshold(threshold int, period domain.QuotaPeriod) string {
	switch threshold {
	case domain.WarningThreshold:
		return fmt.Sprintf("Pemakaian token workspace sudah %d%% (%d dari %d). "+
			"Setelah kuota habis, tugas baru ditahan sampai paket tambahan dibeli atau periode berganti.",
			period.Used(), period.TokensUsed, period.Allowance())
	default:
		return fmt.Sprintf("Kuota token workspace sudah habis (%d dari %d). "+
			"Tugas baru ditahan; tugas yang sedang berjalan diselesaikan. "+
			"Beli paket tambahan atau tunggu periode berikutnya.",
			period.TokensUsed, period.Allowance())
	}
}

// settleInvoices closes what ran out and issues what is owed.
func (s *Service) settleInvoices(ctx context.Context, scope domain.Scope, subscription domain.Subscription, plan domain.Plan, now time.Time, result *domain.SweepResult) error {
	outstanding, err := s.deps.Repository.Outstanding(ctx, scope)
	if err != nil {
		return err
	}

	for _, invoice := range outstanding {
		if invoice.ExpiresAt == nil || invoice.ExpiresAt.After(now) {
			continue
		}
		if _, fresh, err := s.deps.Repository.ExpireInvoice(ctx, scope, invoice.ID); err != nil {
			return err
		} else if fresh {
			result.InvoicesExpired++
		}
	}

	// The invoice for the period after this one is issued when this one ends. Before
	// that there is nothing to pay, which is what keeps a workspace from being asked
	// for money for a month it is already inside.
	if now.Before(subscription.CurrentPeriodEnd) {
		return nil
	}

	_, err = s.ensureUpcomingInvoice(ctx, scope, subscription, plan, result)
	return err
}

// reportTransitions tells the workspace when it fell past due and when it stopped.
//
// The state itself is derived from the times, so these notices are the only thing
// that needs remembering — and they are remembered with a marker rather than by
// comparing two reads, because the sweep has no memory between passes.
func (s *Service) reportTransitions(ctx context.Context, scope domain.Scope, subscription domain.Subscription, plan domain.Plan, now time.Time, result *domain.SweepResult) error {
	entitlement := subscription.Entitlement(now, s.grace)
	changed := false

	switch entitlement {
	case domain.EntitlementGrace:
		if subscription.PastDueSince == nil {
			end := subscription.CurrentPeriodEnd
			subscription.PastDueSince = &end
			subscription.Status = domain.StatusPastDue
			changed = true
		}
		if subscription.PastDueNotifiedAt == nil {
			at := now
			subscription.PastDueNotifiedAt = &at
			changed = true
			result.PastDue++
			s.notify(ctx, scope, domain.Notice{
				Kind:        domain.NoticePastDue,
				PlanName:    plan.Name,
				Entitlement: entitlement,
				Reason: fmt.Sprintf("Masa langganan %s berakhir dan tagihannya belum dibayar. "+
					"Workspace masih bisa bekerja sampai %d hari setelahnya, lalu masuk mode baca saja.",
					plan.Name, s.cfg.GraceDays),
			})
		}
	case domain.EntitlementReadOnly:
		if subscription.ReadOnlyNotifiedAt == nil {
			at := now
			subscription.ReadOnlyNotifiedAt = &at
			changed = true
			result.ReadOnly++
			s.notify(ctx, scope, domain.Notice{
				Kind:        domain.NoticeReadOnly,
				PlanName:    plan.Name,
				Entitlement: entitlement,
				Reason: "Masa tenggang langganan sudah lewat, jadi workspace sekarang mode baca saja: " +
					"membaca dan mengekspor data tetap jalan, tapi perubahan dan tugas baru ditolak " +
					"sampai tagihannya dibayar. Tidak ada data yang dihapus.",
			})
		}
	}

	if !changed {
		return nil
	}

	_, err := s.deps.Repository.SetSubscription(ctx, scope, subscription)
	return err
}

// ensureUpcomingInvoice issues the invoice for the period that starts when the
// current one ends, if there is not one already.
//
// A trial is not renewable: it has no price, and re-issuing it would be a free month
// every month. So a workspace whose package in force is still the trial gets no
// invoice at all — it is told to choose a package, which is the only honest answer
// when nobody has decided what it should pay.
func (s *Service) ensureUpcomingInvoice(ctx context.Context, scope domain.Scope, subscription domain.Subscription, plan domain.Plan, result *domain.SweepResult) (domain.Invoice, error) {
	next, err := planForNextPeriod(ctx, s.deps.Repository, subscription, plan)
	if err != nil {
		return domain.Invoice{}, err
	}

	start := subscription.CurrentPeriodEnd
	if invoice, err := s.deps.Repository.InvoiceForPeriod(ctx, scope, subscription.ID, domain.KindSubscription, start); err == nil {
		return invoice, nil
	}

	if next.IsTrial || next.MonthlyPriceIDR <= 0 {
		// Nothing to sell yet. The subscription still moves past due — the period
		// did end — and the notice above says what to do about it.
		return domain.Invoice{}, nil
	}

	days := next.PeriodDays
	if days <= 0 {
		days = 30
	}

	invoice, err := s.deps.Repository.CreateInvoice(ctx, scope, domain.Invoice{
		SubscriptionID: subscription.ID,
		Kind:           domain.KindSubscription,
		Provider:       s.gatewayName(),
		AmountIDR:      next.MonthlyPriceIDR,
		Status:         domain.StatusPending,
		PeriodStart:    start,
		PeriodEnd:      start.AddDate(0, 0, days),
	})
	if err != nil {
		return domain.Invoice{}, err
	}

	result.InvoicesCreated++
	s.notify(ctx, scope, domain.Notice{
		Kind:     domain.NoticePastDue,
		PlanName: next.Name,
		Reason: fmt.Sprintf("Tagihan %s untuk periode berikutnya sudah terbit: %s.",
			next.Name, formatIDR(next.MonthlyPriceIDR)),
	})

	return s.charge(ctx, scope, invoice)
}

// Sentinels for "no source is configured", which are handled rather than reported:
// a deployment without Redis runs on the ledger alone, and one with neither still
// has a usable module — the period's stored usage is what it reads.
var (
	errNoCounter = errors.New("billing: no live counter is configured")
	errNoLedger  = errors.New("billing: no usage ledger is configured")
)

// shortID renders an id for a message.
func shortID(id uuid.UUID) string {
	text := id.String()
	if len(text) < 8 {
		return text
	}
	return text[:8]
}
