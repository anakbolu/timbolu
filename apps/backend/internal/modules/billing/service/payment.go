package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/billing/domain"
)

// The payment half: what a workspace owes, how it pays, and what a provider's
// notification does.
//
// Nothing here is a mandate. The first customers pay by bank transfer or a virtual
// account, and the honest model of that is an invoice per period that a person pays
// when it arrives — not a card on file. "Recurring" therefore means "a new invoice is
// issued when each period ends", and the sweep is what issues it.

// Pay asks the provider for a payment link for one invoice.
//
// A deployment with no provider configured says so rather than pretending: the
// invoice exists, and the screen shows what to transfer. Retrying is deliberate —
// the attempts are counted and the failure is kept, so a provider that was
// unreachable when the invoice was issued can be tried again from the screen rather
// than by issuing a second invoice for the same month.
func (s *Service) Pay(ctx context.Context, scope domain.Scope, invoiceID uuid.UUID) (domain.Invoice, error) {
	if scope.IsZero() {
		return domain.Invoice{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	invoice, err := s.deps.Repository.Invoice(ctx, scope, invoiceID)
	if err != nil {
		return domain.Invoice{}, err
	}
	if !invoice.Outstanding() {
		return domain.Invoice{}, fmt.Errorf("%w: tagihan ini sudah %s", domain.ErrInvoiceSettled, invoice.Status)
	}

	return s.charge(ctx, scope, invoice)
}

// charge asks the provider for a payment and records what came back.
//
// It never fails the caller over the provider: an invoice that could not be given a
// link is still an invoice a workspace can pay by transfer, and the screen shows the
// reason it has no link rather than showing nothing.
func (s *Service) charge(ctx context.Context, scope domain.Scope, invoice domain.Invoice) (domain.Invoice, error) {
	if s.deps.Gateway == nil {
		return invoice, nil
	}

	customer := domain.Customer{WorkspaceID: scope.WorkspaceID}
	if s.deps.Customers != nil {
		resolved, err := s.deps.Customers.Customer(ctx, scope)
		if err != nil {
			s.deps.Logger.Warn("could not read who the invoice is for", zap.Error(err))
		} else {
			customer = resolved
			customer.WorkspaceID = scope.WorkspaceID
		}
	}

	invoice.Provider = s.deps.Gateway.Name()
	invoice.Attempts++

	result, err := s.deps.Gateway.Charge(ctx, invoice, customer)
	if err != nil {
		invoice.LastError = err.Error()
		stored, saveErr := s.deps.Repository.SetInvoicePayment(ctx, scope, invoice)
		if saveErr != nil {
			return domain.Invoice{}, saveErr
		}
		s.deps.Logger.Warn("could not create a payment link",
			zap.String("invoice", invoice.ID.String()), zap.Error(err))
		return stored, nil
	}

	invoice.ProviderReference = result.Reference
	invoice.Instructions = result.Instructions
	invoice.LastError = ""
	if !result.ExpiresAt.IsZero() {
		expires := result.ExpiresAt
		invoice.ExpiresAt = &expires
	}

	return s.deps.Repository.SetInvoicePayment(ctx, scope, invoice)
}

// BuyTopUp buys tokens inside the period the workspace is in.
//
// The tokens land in the period they were bought in and stay there: a top-up is what
// a workspace buys when it misjudged a month, and carrying the remainder forward
// would mean a workspace that bought early is served by the purchase instead of by
// its package.
func (s *Service) BuyTopUp(ctx context.Context, scope domain.Scope, request domain.TopUpRequest) (domain.Invoice, error) {
	if scope.IsZero() {
		return domain.Invoice{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	key := request.PackageKey
	if key == "" {
		return domain.Invoice{}, fmt.Errorf("%w: pilih paket tambahan yang dituju", domain.ErrInvalidInput)
	}

	pack, err := s.deps.Repository.TopUp(ctx, scope, key)
	if err != nil {
		return domain.Invoice{}, err
	}

	subscription, plan, err := s.live(ctx, scope.WorkspaceID)
	if err != nil {
		return domain.Invoice{}, err
	}

	now := s.now()
	period, err := s.deps.Repository.Period(ctx, scope, subscription, plan, now)
	if err != nil {
		return domain.Invoice{}, err
	}

	invoice, err := s.deps.Repository.CreateInvoice(ctx, scope, domain.Invoice{
		SubscriptionID: subscription.ID,
		Kind:           domain.KindTopup,
		Provider:       s.gatewayName(),
		AmountIDR:      pack.PriceIDR,
		TokenAmount:    pack.TokenAmount,
		Status:         domain.StatusPending,
		PeriodStart:    period.PeriodStart,
		PeriodEnd:      period.PeriodEnd,
	})
	if err != nil {
		return domain.Invoice{}, err
	}

	return s.charge(ctx, scope, invoice)
}

// HandlePayment applies a verified payment notification.
//
// It is the only place a period is extended, and it is guarded twice: the delivery
// is verified by the provider's own signature, and the settlement is written once by
// the invoice's own `status = 'pending'` guard. A webhook that arrives twice — which
// providers do on purpose, because a notification nobody acknowledged is a
// notification that may not have landed — therefore extends exactly one period.
func (s *Service) HandlePayment(ctx context.Context, delivery domain.Delivery) error {
	if s.deps.Gateway == nil {
		return fmt.Errorf("%w: no payment provider is configured", domain.ErrNoGateway)
	}
	if delivery.Provider != "" && delivery.Provider != s.deps.Gateway.Name() {
		// The path named a provider this deployment does not speak, so nothing can
		// verify the payload. Refusing is the only safe answer.
		return fmt.Errorf("%w: %s", domain.ErrNoGateway, delivery.Provider)
	}

	payment, err := s.deps.Gateway.Verify(ctx, delivery)
	if err != nil {
		// A delivery that cannot be trusted is the caller's payload, not this
		// deployment's failure — and the distinction reaches the provider, which must not
		// be told to retry something that can never verify.
		return fmt.Errorf("%w: %w", domain.ErrUnverifiable, err)
	}

	// A provider names one reference and nothing else, so the routing row is what says
	// whose invoice it is. That read is unscoped because it has to be — every table it
	// would otherwise read is protected by Row Level Security — and it carries the two
	// ids and nothing else. Everything after it runs inside that workspace's scope.
	route, err := s.deps.Repository.PaymentRoute(ctx, s.deps.Gateway.Name(), payment.Reference)
	if err != nil {
		return err
	}

	scope := domain.Scope{WorkspaceID: route.WorkspaceID}
	invoice, err := s.deps.Repository.Invoice(ctx, scope, route.InvoiceID)
	if err != nil {
		return err
	}

	switch {
	case payment.Paid:
		return s.applyPayment(ctx, scope, invoice, payment)
	case payment.Expired:
		_, _, err := s.deps.Repository.ExpireInvoice(ctx, scope, invoice.ID)
		return err
	default:
		settled := invoice
		settled.Status = domain.StatusFailed
		settled.PaidAt = nil
		_, _, err := s.deps.Repository.SettleInvoice(ctx, scope, settled)
		return err
	}
}

// applyPayment settles an invoice and carries out what it bought.
func (s *Service) applyPayment(ctx context.Context, scope domain.Scope, invoice domain.Invoice, payment domain.Payment) error {
	if payment.AmountIDR > 0 && payment.AmountIDR < invoice.AmountIDR {
		// Paying less than the invoice asks for is not a payment of it. The invoice
		// is left outstanding and the reason is recorded, so a person sees an
		// amount that did not match rather than a package that was extended for
		// less money than it costs.
		s.deps.Logger.Warn("a payment was smaller than its invoice",
			zap.String("invoice", invoice.ID.String()),
			zap.Int64("paid", payment.AmountIDR),
			zap.Int64("expected", invoice.AmountIDR),
		)
		return fmt.Errorf("%w: pembayaran %s lebih kecil dari tagihan %s",
			domain.ErrInvalidInput, formatIDR(payment.AmountIDR), formatIDR(invoice.AmountIDR))
	}

	settled := invoice
	settled.Status = domain.StatusPaid
	paidAt := payment.PaidAt
	if paidAt.IsZero() {
		paidAt = s.now()
	}
	settled.PaidAt = &paidAt

	stored, fresh, err := s.deps.Repository.SettleInvoice(ctx, scope, settled)
	if err != nil {
		return err
	}
	if !fresh {
		// Already settled: a replayed delivery, and the period was extended the
		// first time. This is the answer that makes a webhook idempotent rather
		// than a second month.
		s.deps.Logger.Info("a payment arrived for an invoice that was already settled",
			zap.String("invoice", invoice.ID.String()))
		return nil
	}

	switch stored.Kind {
	case domain.KindTopup:
		return s.applyTopUp(ctx, scope, stored)
	default:
		return s.applySubscriptionPayment(ctx, scope, stored)
	}
}

// applyTopUp puts bought tokens into the period they were bought in.
func (s *Service) applyTopUp(ctx context.Context, scope domain.Scope, invoice domain.Invoice) error {
	period, err := s.deps.Repository.AddTopUp(ctx, scope, invoice.PeriodStart, invoice.TokenAmount)
	if err != nil {
		return err
	}

	s.notify(ctx, scope, domain.Notice{
		Kind:      domain.NoticeTopUp,
		Used:      period.TokensUsed,
		Allowance: period.Allowance(),
		Reason: fmt.Sprintf("Paket tambahan %s token sudah masuk. Kuota periode ini sekarang %s dari %s token.",
			formatTokens(invoice.TokenAmount), formatTokens(period.TokensUsed), formatTokens(period.Allowance())),
	})

	return nil
}

// applySubscriptionPayment extends the period the invoice was for.
//
// The new period is the invoice's own window rather than "now plus a month", and the
// difference matters: a payment that arrives during the grace days must not push the
// boundary forward, or a workspace that paid late would keep drifting its billing
// anniversary. The package a person chose while the invoice was outstanding is
// applied here, because this is the moment the period it belongs to begins.
func (s *Service) applySubscriptionPayment(ctx context.Context, scope domain.Scope, invoice domain.Invoice) error {
	subscription, plan, err := s.live(ctx, scope.WorkspaceID)
	if err != nil {
		return err
	}

	if subscription.PendingPlanID != uuid.Nil {
		if pending, err := s.deps.Repository.PlanByID(ctx, subscription.PendingPlanID); err == nil {
			subscription.PlanID = pending.ID
			plan = pending
		} else {
			s.deps.Logger.Warn("could not apply the pending package", zap.Error(err))
		}
		subscription.PendingPlanID = uuid.Nil
	}

	subscription.Status = domain.StatusActive
	subscription.CurrentPeriodStart = invoice.PeriodStart
	subscription.CurrentPeriodEnd = invoice.PeriodEnd
	grace := invoice.PeriodEnd.AddDate(0, 0, s.cfg.GraceDays)
	subscription.GraceUntil = &grace
	// The workspace paid, so what it was told about not paying is no longer true —
	// and the next period's notices have to be able to fire.
	subscription.PastDueSince = nil
	subscription.PastDueNotifiedAt = nil
	subscription.ReadOnlyNotifiedAt = nil

	if _, err := s.deps.Repository.SetSubscription(ctx, scope, subscription); err != nil {
		return err
	}

	// The period the workspace is now in has to exist before anything reads its
	// allowance, so it is opened here rather than left for the next check.
	if _, err := s.deps.Repository.Period(ctx, scope, subscription, plan, invoice.PeriodStart); err != nil {
		return err
	}

	// And what the workspace no longer owes is closed. A period that ended while the
	// invoice was outstanding may have been issued a second one — the sweep runs on an
	// interval and each pass that finds the period ended asks for an invoice — and a
	// person who has paid should not be shown an amount for a month they are now inside.
	s.withdrawStaleInvoices(ctx, scope, invoice.ID)

	s.notify(ctx, scope, domain.Notice{
		Kind:        domain.NoticePaid,
		PlanName:    plan.Name,
		Entitlement: domain.EntitlementActive,
		Reason: fmt.Sprintf("Pembayaran diterima. Paket %s aktif sampai %s.",
			plan.Name, formatDate(invoice.PeriodEnd)),
	})

	return nil
}

// withdrawStaleInvoices expires the subscription invoices a workspace no longer owes.
//
// It runs after a payment and never touches the one that was just settled, nor a
// top-up: what a person bought on top of a package is not affected by paying for the
// package. An expired row stays as history, which is what makes "we asked twice and the
// second one was withdrawn" answerable.
func (s *Service) withdrawStaleInvoices(ctx context.Context, scope domain.Scope, paidID uuid.UUID) {
	outstanding, err := s.deps.Repository.Outstanding(ctx, scope)
	if err != nil {
		s.deps.Logger.Warn("could not read the outstanding invoices after a payment", zap.Error(err))
		return
	}

	for _, invoice := range outstanding {
		if invoice.ID == paidID || invoice.Kind != domain.KindSubscription {
			continue
		}
		if _, _, err := s.deps.Repository.ExpireInvoice(ctx, scope, invoice.ID); err != nil {
			s.deps.Logger.Warn("could not withdraw an invoice the payment superseded",
				zap.String("invoice", invoice.ID.String()), zap.Error(err))
		}
	}
}

// gatewayName is the provider invoices record, or an empty string when none is
// configured.
func (s *Service) gatewayName() string {
	if s.deps.Gateway == nil {
		return ""
	}
	return s.deps.Gateway.Name()
}
