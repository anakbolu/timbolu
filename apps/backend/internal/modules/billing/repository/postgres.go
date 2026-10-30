// Package repository implements the billing data access with pgx and the
// sqlc-generated queries.
//
// Every method that names a workspace runs inside the caller's scope, so Row Level
// Security filters the statements even if a query forgets a filter. The three that
// do not are the ones that cannot: the sweep queue, which exists to say which
// tenant to open a scope with; the invoice a payment provider names, which the
// provider cannot scope for us; and the two catalogues, which carry no tenant at
// all.
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/billing/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/billing/repository/sqlcgen"
	"github.com/anakbolu/timbolu/packages/platform/database"
)

// Repository is the Postgres-backed implementation of domain.Repository.
type Repository struct {
	pool *database.Pool
}

// New builds the repository.
func New(pool *database.Pool) *Repository { return &Repository{pool: pool} }

// Compile-time check that the repository is the interface the module asked for.
var _ domain.Repository = (*Repository)(nil)

// read runs one query in the caller's scope, read-only.
func (r *Repository) read(ctx context.Context, scope domain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}
	if scope.IsZero() {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	return r.pool.InScopeRead(ctx, database.Scope{
		UserID:      scope.UserID,
		WorkspaceID: scope.WorkspaceID,
	}, func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

// write runs one query in the caller's scope.
func (r *Repository) write(ctx context.Context, scope domain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}
	if scope.IsZero() {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	return r.pool.InScope(ctx, database.Scope{
		UserID:      scope.UserID,
		WorkspaceID: scope.WorkspaceID,
	}, func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

// translate turns a driver error into this module's own.
func translate(err error, operation string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", domain.ErrNotFound, operation)
	}
	return fmt.Errorf("billing: %s: %w", operation, err)
}

// limitOr applies the module's page ceiling.
func limitOr(value int) int32 {
	if value <= 0 || value > domain.MaxListLimit {
		return domain.DefaultListLimit
	}
	return int32(value)
}

// uuidOrNull renders an optional id. The subscription column is nullable in the
// schema, and the zero uuid would name a subscription that is not there rather
// than the absence of one.
func uuidOrNull(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// ---------------------------------------------------------------- packages

// Plans lists the packages this deployment sells.
func (r *Repository) Plans(ctx context.Context) ([]domain.Plan, error) {
	if r == nil || r.pool == nil {
		return nil, fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}

	rows, err := sqlcgen.New(r.pool.PgxPool()).ListPlans(ctx)
	if err != nil {
		return nil, fmt.Errorf("billing: list plans: %w", err)
	}

	plans := make([]domain.Plan, 0, len(rows))
	for _, row := range rows {
		plans = append(plans, toPlan(row))
	}
	return plans, nil
}

// Plan reads one package by its key.
func (r *Repository) Plan(ctx context.Context, key string) (domain.Plan, error) {
	if r == nil || r.pool == nil {
		return domain.Plan{}, fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}

	row, err := sqlcgen.New(r.pool.PgxPool()).GetPlanByKey(ctx, key)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Plan{}, fmt.Errorf("%w: %s", domain.ErrPlanNotFound, key)
		}
		return domain.Plan{}, fmt.Errorf("billing: read plan: %w", err)
	}
	return toPlan(row), nil
}

// PlanByID reads the package a subscription names.
func (r *Repository) PlanByID(ctx context.Context, id uuid.UUID) (domain.Plan, error) {
	if r == nil || r.pool == nil {
		return domain.Plan{}, fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}

	row, err := sqlcgen.New(r.pool.PgxPool()).GetPlanByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Plan{}, fmt.Errorf("%w: %s", domain.ErrPlanNotFound, id)
		}
		return domain.Plan{}, fmt.Errorf("billing: read plan: %w", err)
	}
	return toPlan(row), nil
}

// TrialPlan reads the package a new workspace starts on.
func (r *Repository) TrialPlan(ctx context.Context) (domain.Plan, error) {
	if r == nil || r.pool == nil {
		return domain.Plan{}, fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}

	row, err := sqlcgen.New(r.pool.PgxPool()).GetTrialPlan(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// A deployment whose packages were all removed has no trial to start
			// on, which is a deployment problem rather than a request problem.
			return domain.Plan{}, fmt.Errorf("%w: no trial plan is seeded", domain.ErrPlanNotFound)
		}
		return domain.Plan{}, fmt.Errorf("billing: read trial plan: %w", err)
	}
	return toPlan(row), nil
}

// ---------------------------------------------------------------- subscription

// Subscription reads the workspace's live package.
func (r *Repository) Subscription(ctx context.Context, scope domain.Scope) (domain.Subscription, error) {
	var subscription domain.Subscription

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetLiveSubscription(ctx, scope.WorkspaceID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// A workspace with no live subscription is the invariant the
				// migration backfilled away, so it is a broken row rather than a
				// state a screen should render.
				return fmt.Errorf("%w: the workspace has no live subscription", domain.ErrSubscriptionNotFound)
			}
			return fmt.Errorf("billing: read subscription: %w", err)
		}
		subscription = toSubscription(row)
		return nil
	})
	if err != nil {
		return domain.Subscription{}, err
	}

	return subscription, nil
}

// SetSubscription writes a subscription back.
func (r *Repository) SetSubscription(ctx context.Context, scope domain.Scope, subscription domain.Subscription) (domain.Subscription, error) {
	var stored domain.Subscription

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.UpdateSubscription(ctx, sqlcgen.UpdateSubscriptionParams{
			ID:                 subscription.ID,
			WorkspaceID:        scope.WorkspaceID,
			PlanID:             subscription.PlanID,
			Status:             subscription.Status,
			CurrentPeriodStart: subscription.CurrentPeriodStart,
			CurrentPeriodEnd:   subscription.CurrentPeriodEnd,
			GraceUntil:         subscription.GraceUntil,
			PastDueSince:       subscription.PastDueSince,
			PendingPlanID:      uuidOrNull(subscription.PendingPlanID),
			PastDueNotifiedAt:  subscription.PastDueNotifiedAt,
			ReadOnlyNotifiedAt: subscription.ReadOnlyNotifiedAt,
			CanceledAt:         subscription.CanceledAt,
		})
		if err != nil {
			return translate(err, "update subscription")
		}
		stored = toSubscription(row)
		return nil
	})
	if err != nil {
		return domain.Subscription{}, err
	}

	return stored, nil
}

// StartTrial creates the live subscription a new workspace begins on, the quota
// period it grants, and the queue row the sweep finds it by — all inside the
// caller's transaction.
//
// It is the workspace module's provisioner: onboarding hands over its transaction
// so a workspace never exists without a package. Writing the directory row in the
// same transaction is what keeps the sweep from missing a workspace a crash caught
// halfway.
func (r *Repository) StartTrial(ctx context.Context, tx pgx.Tx, workspaceID uuid.UUID) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}
	if workspaceID == uuid.Nil {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	queries := sqlcgen.New(tx)

	plan, err := queries.GetTrialPlan(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no trial plan is seeded", domain.ErrPlanNotFound)
		}
		return fmt.Errorf("billing: read trial plan: %w", err)
	}

	now := time.Now()
	start := now
	end := now.AddDate(0, 0, int(plan.PeriodDays))
	grace := end.AddDate(0, 0, domain.DefaultGraceDays)

	subscription, err := queries.CreateSubscription(ctx, sqlcgen.CreateSubscriptionParams{
		WorkspaceID:        workspaceID,
		PlanID:             plan.ID,
		Status:             domain.StatusTrial,
		CurrentPeriodStart: start,
		CurrentPeriodEnd:   end,
		GraceUntil:         &grace,
	})
	if err != nil {
		return fmt.Errorf("billing: start trial: %w", err)
	}

	if _, err := queries.CreateQuotaPeriod(ctx, sqlcgen.CreateQuotaPeriodParams{
		WorkspaceID:    workspaceID,
		PeriodStart:    subscription.CurrentPeriodStart,
		PeriodEnd:      subscription.CurrentPeriodEnd,
		TokensGranted:  plan.TokenQuota,
		TokensToppedUp: 0,
		TokensUsed:     0,
	}); err != nil {
		return fmt.Errorf("billing: create quota period: %w", err)
	}

	if err := queries.EnsureBillingDirectory(ctx, workspaceID); err != nil {
		return fmt.Errorf("billing: queue workspace for the sweep: %w", err)
	}

	return nil
}

// ---------------------------------------------------------------- directory

// DueWorkspaces lists the workspaces the sweep still has to look at.
func (r *Repository) DueWorkspaces(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error) {
	if r == nil || r.pool == nil {
		return nil, fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}

	// Unscoped on purpose: the table carries a workspace id and a moment and
	// nothing else, and this read is what tells the sweep which tenant to open.
	rows, err := sqlcgen.New(r.pool.PgxPool()).ListDueBillingWorkspaces(ctx, sqlcgen.ListDueBillingWorkspacesParams{
		Now:        now,
		LimitCount: limitOr(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("billing: list due workspaces: %w", err)
	}

	return rows, nil
}

// TouchDirectory moves a workspace's next pass forward.
func (r *Repository) TouchDirectory(ctx context.Context, scope domain.Scope, workspaceID uuid.UUID, nextSyncAt time.Time) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		if err := queries.TouchBillingDirectory(ctx, sqlcgen.TouchBillingDirectoryParams{
			WorkspaceID: workspaceID,
			NextSyncAt:  nextSyncAt,
		}); err != nil {
			return fmt.Errorf("billing: touch directory: %w", err)
		}
		return nil
	})
}

// ---------------------------------------------------------------- quota periods

// Period reads the period a subscription is in at a moment, opening it when it
// does not exist yet.
func (r *Repository) Period(ctx context.Context, scope domain.Scope, subscription domain.Subscription, plan domain.Plan, at time.Time) (domain.QuotaPeriod, error) {
	var period domain.QuotaPeriod

	start, end := periodWindow(subscription, plan, at)

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.CreateQuotaPeriod(ctx, sqlcgen.CreateQuotaPeriodParams{
			WorkspaceID:    scope.WorkspaceID,
			PeriodStart:    start,
			PeriodEnd:      end,
			TokensGranted:  plan.TokenQuota,
			TokensToppedUp: 0,
			TokensUsed:     0,
		})
		if err != nil {
			return translate(err, "open quota period")
		}
		period = toPeriod(row)
		return nil
	})
	if err != nil {
		return domain.QuotaPeriod{}, err
	}

	return period, nil
}

// periodWindow is the period a moment belongs to.
//
// The period is the subscription's own rather than a calendar month: a workspace
// that subscribed on the 20th is granted its allowance on the 20th, and the row's
// unique key is its start, so the window has to be the subscription's.
func periodWindow(subscription domain.Subscription, plan domain.Plan, at time.Time) (time.Time, time.Time) {
	start := subscription.CurrentPeriodStart
	end := subscription.CurrentPeriodEnd

	// The window walks forward when the subscription has already been extended past
	// the moment being asked about — a sweep that ran after a payment, say. It stops
	// at the moment rather than at every period in between: a period nobody spent in
	// has nothing to count.
	days := plan.PeriodDays
	if days <= 0 {
		days = 30
	}
	for !at.Before(end) {
		start = end
		end = start.AddDate(0, 0, days)
	}
	return start, end
}

// SavePeriod writes a period back.
func (r *Repository) SavePeriod(ctx context.Context, scope domain.Scope, period domain.QuotaPeriod) (domain.QuotaPeriod, error) {
	var stored domain.QuotaPeriod

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.UpdateQuotaPeriod(ctx, sqlcgen.UpdateQuotaPeriodParams{
			ID:               period.ID,
			WorkspaceID:      scope.WorkspaceID,
			TokensUsed:       period.TokensUsed,
			TokensToppedUp:   period.TokensToppedUp,
			Warning80SentAt:  period.Warning80SentAt,
			Warning100SentAt: period.Warning100SentAt,
		})
		if err != nil {
			return translate(err, "save quota period")
		}
		stored = toPeriod(row)
		return nil
	})
	if err != nil {
		return domain.QuotaPeriod{}, err
	}

	return stored, nil
}

// AddTopUp adds tokens to the period that starts at a moment.
func (r *Repository) AddTopUp(ctx context.Context, scope domain.Scope, periodStart time.Time, tokens int64) (domain.QuotaPeriod, error) {
	var stored domain.QuotaPeriod

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.AddQuotaTopUp(ctx, sqlcgen.AddQuotaTopUpParams{
			WorkspaceID: scope.WorkspaceID,
			PeriodStart: periodStart,
			Tokens:      tokens,
		})
		if err != nil {
			return translate(err, "add quota top-up")
		}
		stored = toPeriod(row)
		return nil
	})
	if err != nil {
		return domain.QuotaPeriod{}, err
	}

	return stored, nil
}

// ListPeriods lists a workspace's periods, newest first.
func (r *Repository) ListPeriods(ctx context.Context, scope domain.Scope, limit int) ([]domain.QuotaPeriod, error) {
	periods := []domain.QuotaPeriod{}

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListQuotaPeriods(ctx, sqlcgen.ListQuotaPeriodsParams{
			WorkspaceID: scope.WorkspaceID,
			LimitCount:  limitOr(limit),
		})
		if err != nil {
			return fmt.Errorf("billing: list quota periods: %w", err)
		}
		for _, row := range rows {
			periods = append(periods, toPeriod(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return periods, nil
}

// ---------------------------------------------------------------- invoices

// Invoice reads one invoice.
func (r *Repository) Invoice(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Invoice, error) {
	var invoice domain.Invoice

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetInvoice(ctx, sqlcgen.GetInvoiceParams{ID: id, WorkspaceID: scope.WorkspaceID})
		if err != nil {
			return translate(err, "read invoice")
		}
		invoice = toInvoice(row)
		return nil
	})
	if err != nil {
		return domain.Invoice{}, err
	}

	return invoice, nil
}

// PaymentRoute answers where a provider's reference points.
func (r *Repository) PaymentRoute(ctx context.Context, provider, reference string) (domain.PaymentRoute, error) {
	if r == nil || r.pool == nil {
		return domain.PaymentRoute{}, fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}

	// Unscoped, and it has to be: a provider does not know which workspace it is
	// talking about, and every table it would have to read is protected by Row Level
	// Security. The row it finds carries the two ids and nothing else.
	row, err := sqlcgen.New(r.pool.PgxPool()).GetPaymentRoute(ctx, sqlcgen.GetPaymentRouteParams{
		Provider:  provider,
		Reference: reference,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.PaymentRoute{}, fmt.Errorf("%w: %s/%s", domain.ErrInvoiceNotFound, provider, reference)
		}
		return domain.PaymentRoute{}, fmt.Errorf("billing: read payment route: %w", err)
	}

	return domain.PaymentRoute{WorkspaceID: row.WorkspaceID, InvoiceID: row.InvoiceID}, nil
}

// InvoiceForPeriod reads the invoice of one period and kind that still stands.
func (r *Repository) InvoiceForPeriod(ctx context.Context, scope domain.Scope, subscriptionID uuid.UUID, kind string, periodStart time.Time) (domain.Invoice, error) {
	var invoice domain.Invoice

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetActiveInvoiceForPeriod(ctx, sqlcgen.GetActiveInvoiceForPeriodParams{
			WorkspaceID:    scope.WorkspaceID,
			SubscriptionID: uuidOrNull(subscriptionID),
			Kind:           kind,
			PeriodStart:    periodStart,
		})
		if err != nil {
			return translate(err, "read invoice for period")
		}
		invoice = toInvoice(row)
		return nil
	})
	if err != nil {
		return domain.Invoice{}, err
	}

	return invoice, nil
}

// Invoices lists a workspace's invoices.
func (r *Repository) Invoices(ctx context.Context, scope domain.Scope, filter domain.InvoiceFilter) ([]domain.Invoice, error) {
	invoices := []domain.Invoice{}

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListInvoices(ctx, sqlcgen.ListInvoicesParams{
			WorkspaceID: scope.WorkspaceID,
			Status:      filter.Status,
			LimitCount:  limitOr(filter.Limit),
		})
		if err != nil {
			return fmt.Errorf("billing: list invoices: %w", err)
		}
		for _, row := range rows {
			invoices = append(invoices, toInvoice(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return invoices, nil
}

// Outstanding lists what a workspace still owes, oldest first.
func (r *Repository) Outstanding(ctx context.Context, scope domain.Scope) ([]domain.Invoice, error) {
	invoices := []domain.Invoice{}

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListOutstandingInvoices(ctx, scope.WorkspaceID)
		if err != nil {
			return fmt.Errorf("billing: list outstanding invoices: %w", err)
		}
		for _, row := range rows {
			invoices = append(invoices, toInvoice(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return invoices, nil
}

// CreateInvoice writes one invoice.
func (r *Repository) CreateInvoice(ctx context.Context, scope domain.Scope, invoice domain.Invoice) (domain.Invoice, error) {
	var stored domain.Invoice

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.CreateInvoice(ctx, sqlcgen.CreateInvoiceParams{
			WorkspaceID:         scope.WorkspaceID,
			SubscriptionID:      uuidOrNull(invoice.SubscriptionID),
			Provider:            invoice.Provider,
			ProviderReference:   invoice.ProviderReference,
			AmountIdr:           invoice.AmountIDR,
			TokenAmount:         invoice.TokenAmount,
			Status:              invoice.Status,
			Kind:                invoice.Kind,
			PeriodStart:         invoice.PeriodStart,
			PeriodEnd:           invoice.PeriodEnd,
			PaymentInstructions: invoice.Instructions,
			ExpiresAt:           invoice.ExpiresAt,
		})
		if err == nil {
			stored = toInvoice(row)
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return translate(err, "create invoice")
		}

		// No rows: a second writer created this period's invoice a moment ago — two
		// sweep passes, or a sweep and a screen. The row's own index is what decides,
		// and the answer is the invoice that exists rather than a failure, which is
		// what makes an outstanding period have exactly one invoice without a lock.
		existing, readErr := queries.GetActiveInvoiceForPeriod(ctx, sqlcgen.GetActiveInvoiceForPeriodParams{
			WorkspaceID:    scope.WorkspaceID,
			SubscriptionID: uuidOrNull(invoice.SubscriptionID),
			Kind:           invoice.Kind,
			PeriodStart:    invoice.PeriodStart,
		})
		if readErr != nil {
			return translate(readErr, "read the invoice that was created first")
		}
		stored = toInvoice(existing)
		return nil
	})
	if err != nil {
		return domain.Invoice{}, err
	}

	return stored, nil
}

// SettleInvoice records an invoice's outcome, answering whether it was the first
// time.
//
// The guard is the statement's own `status = 'pending'`, so a replayed webhook
// updates nothing and this answers with the invoice as it already stands. That is
// what keeps one payment from extending a period twice.
func (r *Repository) SettleInvoice(ctx context.Context, scope domain.Scope, invoice domain.Invoice) (domain.Invoice, bool, error) {
	var (
		stored domain.Invoice
		fresh  bool
	)

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.SetInvoiceSettlement(ctx, sqlcgen.SetInvoiceSettlementParams{
			ID:          invoice.ID,
			WorkspaceID: scope.WorkspaceID,
			Status:      invoice.Status,
			PaidAt:      invoice.PaidAt,
		})
		if err == nil {
			stored = toInvoice(row)
			fresh = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return translate(err, "settle invoice")
		}

		// No rows: the invoice is no longer pending, so the answer is what it
		// already says rather than a failure. A second delivery is exactly the case
		// this exists for.
		current, readErr := queries.GetInvoice(ctx, sqlcgen.GetInvoiceParams{ID: invoice.ID, WorkspaceID: scope.WorkspaceID})
		if readErr != nil {
			return translate(readErr, "read settled invoice")
		}
		stored = toInvoice(current)
		fresh = false
		return nil
	})
	if err != nil {
		return domain.Invoice{}, false, err
	}

	return stored, fresh, nil
}

// SetInvoicePayment records the payment link a provider returned, or why it could
// not be created.
func (r *Repository) SetInvoicePayment(ctx context.Context, scope domain.Scope, invoice domain.Invoice) (domain.Invoice, error) {
	var stored domain.Invoice

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		// The routing row is written with the link, in one transaction: a payment link
		// that exists is a payment link a notification can find its way back from.
		if err := queries.RememberPaymentRoute(ctx, sqlcgen.RememberPaymentRouteParams{
			Provider:    invoice.Provider,
			Reference:   invoice.ProviderReference,
			WorkspaceID: scope.WorkspaceID,
			InvoiceID:   invoice.ID,
		}); err != nil {
			return fmt.Errorf("billing: remember payment route: %w", err)
		}

		row, err := queries.SetInvoicePayment(ctx, sqlcgen.SetInvoicePaymentParams{
			ID:                  invoice.ID,
			WorkspaceID:         scope.WorkspaceID,
			Provider:            invoice.Provider,
			ProviderReference:   invoice.ProviderReference,
			PaymentInstructions: invoice.Instructions,
			ExpiresAt:           invoice.ExpiresAt,
			Attempts:            int32(invoice.Attempts),
			LastError:           invoice.LastError,
		})
		if err != nil {
			return translate(err, "record invoice payment")
		}
		stored = toInvoice(row)
		return nil
	})
	if err != nil {
		return domain.Invoice{}, err
	}

	return stored, nil
}

// ExpireInvoice closes an invoice whose payment window has passed.
func (r *Repository) ExpireInvoice(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Invoice, bool, error) {
	var (
		stored domain.Invoice
		fresh  bool
	)

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.ExpireInvoice(ctx, sqlcgen.ExpireInvoiceParams{ID: id, WorkspaceID: scope.WorkspaceID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Already settled or already expired: nothing to change, and not a
				// failure.
				return nil
			}
			return translate(err, "expire invoice")
		}
		stored = toInvoice(row)
		fresh = true
		return nil
	})
	if err != nil {
		return domain.Invoice{}, false, err
	}

	return stored, fresh, nil
}

// ---------------------------------------------------------------- top-ups

// TopUps lists the packs a workspace may buy.
func (r *Repository) TopUps(ctx context.Context, _ domain.Scope) ([]domain.TopUpPackage, error) {
	if r == nil || r.pool == nil {
		return nil, fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}

	// A catalogue rather than tenant data: every workspace sees the same packs, and
	// what one bought is recorded in its own invoice.
	rows, err := sqlcgen.New(r.pool.PgxPool()).ListTopUpPackages(ctx)
	if err != nil {
		return nil, fmt.Errorf("billing: list top-up packages: %w", err)
	}

	packages := make([]domain.TopUpPackage, 0, len(rows))
	for _, row := range rows {
		packages = append(packages, toTopUp(row))
	}
	return packages, nil
}

// TopUp reads one pack by its key.
func (r *Repository) TopUp(ctx context.Context, _ domain.Scope, key string) (domain.TopUpPackage, error) {
	if r == nil || r.pool == nil {
		return domain.TopUpPackage{}, fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}

	row, err := sqlcgen.New(r.pool.PgxPool()).GetTopUpPackage(ctx, key)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.TopUpPackage{}, fmt.Errorf("%w: %s", domain.ErrTopUpNotFound, key)
		}
		return domain.TopUpPackage{}, fmt.Errorf("billing: read top-up package: %w", err)
	}
	return toTopUp(row), nil
}
