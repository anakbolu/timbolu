// Package service implements the billing use cases: what a workspace is on, what it
// may spend, what it owes, and what happens when it does not pay.
//
// The shape of the module follows from one decision: **what a workspace may spend
// is derived, never remembered.** A percentage that is stored can be wrong, and a
// wrong allowance is either a workspace charged for tokens it never used or one
// served for free. So the entitlement is computed from the subscription's times on
// every read, the allowance is the package plus what was bought, and the usage is
// whichever of the two mirrors — the live counter, then the ledger — is telling the
// truth.
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/billing/domain"
)

// Config holds the settings a deployment decides.
type Config struct {
	// GraceDays is how long a workspace keeps working after its period ends.
	// Zero takes the product default.
	GraceDays int
	// SyncInterval is how often one workspace is reconciled. It bounds how stale
	// the durable mirror can be, which is what the screen shows.
	SyncInterval time.Duration
	// SweepLimit bounds one pass, so a deployment with many workspaces reconciles
	// them over several passes rather than in one unbounded read.
	SweepLimit int
}

// DefaultSyncInterval is how often a workspace is reconciled when nothing says
// otherwise. Fifteen minutes keeps the durable mirror close enough to the live
// counter that a screen is never far behind, without a write per model call.
const DefaultSyncInterval = 15 * time.Minute

// DefaultSweepLimit bounds one pass.
const DefaultSweepLimit = 200

// Deps are the service dependencies.
type Deps struct {
	Repository domain.Repository
	// Usage reads the ledger, which is the durable truth about what was spent.
	// Optional: without it the live counter answers alone.
	Usage domain.UsageReader
	// Counter is the live token and cost total. Optional: without it every check
	// reads the ledger, which is slower and correct.
	Counter domain.SpendCounter
	// Gateway creates a payment. Nil means this deployment takes no online
	// payment, which is a real deployment: the invoice is still written and the
	// screen says how to pay it.
	Gateway domain.Gateway
	// Notifier tells the workspace's people what changed. Optional.
	Notifier domain.Notifier
	// Customers is who a payment is addressed to. Optional: a provider that does
	// not need it is not given one.
	Customers Customers
	Logger    *zap.Logger
	Clock     domain.Clock
	Config    Config
}

// Customers is who a payment is for, which is what a provider addresses an invoice
// to. It is a port for the reason the notifier is: the workspace module owns the
// name and the address, and this module may not import it.
type Customers interface {
	Customer(ctx context.Context, scope domain.Scope) (domain.Customer, error)
}

// Service is the billing module.
type Service struct {
	deps  Deps
	now   domain.Clock
	grace time.Duration
	cfg   Config
}

// New builds the service.
func New(deps Deps) *Service {
	log := deps.Logger
	if log == nil {
		log = zap.NewNop()
	}
	clock := deps.Clock
	if clock == nil {
		clock = time.Now
	}

	graceDays := deps.Config.GraceDays
	if graceDays <= 0 {
		graceDays = domain.DefaultGraceDays
	}
	sync := deps.Config.SyncInterval
	if sync <= 0 {
		sync = DefaultSyncInterval
	}
	limit := deps.Config.SweepLimit
	if limit <= 0 {
		limit = DefaultSweepLimit
	}

	return &Service{
		deps: Deps{
			Repository: deps.Repository,
			Usage:      deps.Usage,
			Counter:    deps.Counter,
			Gateway:    deps.Gateway,
			Notifier:   deps.Notifier,
			Customers:  deps.Customers,
			Logger:     log,
			Clock:      clock,
		},
		now:   clock,
		grace: time.Duration(graceDays) * 24 * time.Hour,
		cfg: Config{
			GraceDays:    graceDays,
			SyncInterval: sync,
			SweepLimit:   limit,
		},
	}
}

// live reads the workspace's subscription and the package it is on.
//
// It is the first step of almost everything here, and it is deliberately not
// cached: a package change, a payment, and a period boundary all have to be
// visible to the next read, and a cached plan would be a workspace served for days
// on an allowance it no longer has.
func (s *Service) live(ctx context.Context, workspaceID uuid.UUID) (domain.Subscription, domain.Plan, error) {
	if s.deps.Repository == nil {
		return domain.Subscription{}, domain.Plan{}, fmt.Errorf("%w: no repository is configured", domain.ErrInvalidInput)
	}
	if workspaceID == uuid.Nil {
		return domain.Subscription{}, domain.Plan{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	scope := domain.Scope{WorkspaceID: workspaceID}
	subscription, err := s.deps.Repository.Subscription(ctx, scope)
	if err != nil {
		return domain.Subscription{}, domain.Plan{}, err
	}

	plan, err := s.deps.Repository.PlanByID(ctx, subscription.PlanID)
	if err != nil {
		return domain.Subscription{}, domain.Plan{}, err
	}

	return subscription, plan, nil
}

// planForNextPeriod is the package the *next* period is billed at: the one a person
// chose, or the one in force when nobody chose.
func planForNextPeriod(ctx context.Context, repository domain.Repository, subscription domain.Subscription, plan domain.Plan) (domain.Plan, error) {
	if subscription.PendingPlanID == uuid.Nil {
		return plan, nil
	}

	pending, err := repository.PlanByID(ctx, subscription.PendingPlanID)
	if err != nil {
		return domain.Plan{}, err
	}
	return pending, nil
}

// Snapshot is everything the subscription screen shows, in one read.
func (s *Service) Snapshot(ctx context.Context, scope domain.Scope) (domain.Snapshot, error) {
	if scope.IsZero() {
		return domain.Snapshot{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	subscription, plan, err := s.live(ctx, scope.WorkspaceID)
	if err != nil {
		return domain.Snapshot{}, err
	}

	now := s.now()
	period, err := s.deps.Repository.Period(ctx, scope, subscription, plan, now)
	if err != nil {
		return domain.Snapshot{}, err
	}

	outstanding, err := s.deps.Repository.Outstanding(ctx, scope)
	if err != nil {
		return domain.Snapshot{}, err
	}

	invoices, err := s.deps.Repository.Invoices(ctx, scope, domain.InvoiceFilter{Limit: domain.DefaultListLimit})
	if err != nil {
		return domain.Snapshot{}, err
	}

	topUps, err := s.deps.Repository.TopUps(ctx, scope)
	if err != nil {
		return domain.Snapshot{}, err
	}

	entitlement := subscription.Entitlement(now, s.grace)

	pendingKey, pendingName := "", ""
	if subscription.PendingPlanID != uuid.Nil {
		if pending, err := s.deps.Repository.PlanByID(ctx, subscription.PendingPlanID); err == nil {
			pendingKey, pendingName = pending.Key, pending.Name
		}
	}

	return domain.Snapshot{
		Plan:            plan,
		Subscription:    subscription,
		PendingPlanKey:  pendingKey,
		PendingPlanName: pendingName,
		Entitlement:     entitlement,
		GraceDays:       s.cfg.GraceDays,
		Period:          period,
		Outstanding:     outstanding,
		Invoices:        invoices,
		TopUps:          topUps,
		UsageToday:      domain.Usage{CostMicros: s.dailyCost(ctx, scope.WorkspaceID)},
		Limits: domain.Limits{
			MaxAgents:       plan.MaxAgents,
			MaxIntegrations: plan.MaxIntegrations,
			MaxMembers:      plan.MaxMembers,
		},
		ReadOnly: entitlement == domain.EntitlementReadOnly,
	}, nil
}

// Plans lists the packages a workspace may move to.
func (s *Service) Plans(ctx context.Context, scope domain.Scope) ([]domain.Plan, error) {
	if scope.IsZero() {
		return nil, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	return s.deps.Repository.Plans(ctx)
}

// Change moves the workspace to another package.
//
// The change is deferred to the next period whenever the workspace is inside a
// period it already has: taking a bigger package mid-month would either hand out
// the bigger allowance for free or charge a proration nobody asked for. A
// workspace on its trial has no period to protect, so the change applies at once.
//
// An outstanding invoice for the next period is withdrawn when the price changes,
// because the amount it asked for is no longer the amount owed — the sweep issues
// the new one, and the old one is left as history rather than edited.
func (s *Service) Change(ctx context.Context, scope domain.Scope, request domain.ChangeRequest) (domain.Snapshot, error) {
	if scope.IsZero() {
		return domain.Snapshot{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	key := strings.TrimSpace(request.PlanKey)
	if key == "" {
		return domain.Snapshot{}, fmt.Errorf("%w: pilih paket yang dituju", domain.ErrInvalidInput)
	}

	subscription, plan, err := s.live(ctx, scope.WorkspaceID)
	if err != nil {
		return domain.Snapshot{}, err
	}

	target, err := s.deps.Repository.Plan(ctx, key)
	if err != nil {
		return domain.Snapshot{}, err
	}
	if target.ID == plan.ID && subscription.PendingPlanID == uuid.Nil {
		return domain.Snapshot{}, fmt.Errorf("%w: workspace sudah memakai paket %s", domain.ErrInvalidInput, target.Name)
	}

	// A trial has nothing to protect: no money has changed hands for the period it
	// is in, so the chosen package applies to it immediately.
	if subscription.Status == domain.StatusTrial && subscription.PendingPlanID == uuid.Nil {
		subscription.PlanID = target.ID
	} else {
		subscription.PendingPlanID = target.ID
	}

	if _, err := s.deps.Repository.SetSubscription(ctx, scope, subscription); err != nil {
		return domain.Snapshot{}, err
	}

	s.withdrawUpcomingInvoice(ctx, scope, subscription, target.Name)
	s.notify(ctx, scope, domain.Notice{
		Kind:     NoticePlanChanged,
		PlanName: target.Name,
		Reason:   "Paket workspace diubah ke " + target.Name + ". Perubahannya berlaku saat periode berikutnya dimulai.",
	})

	return s.Snapshot(ctx, scope)
}

// withdrawUpcomingInvoice expires the outstanding invoice for a period that has not
// started, so a re-priced one can be issued.
func (s *Service) withdrawUpcomingInvoice(ctx context.Context, scope domain.Scope, subscription domain.Subscription, planName string) {
	invoices, err := s.deps.Repository.Outstanding(ctx, scope)
	if err != nil {
		s.deps.Logger.Warn("could not read the outstanding invoices", zap.Error(err))
		return
	}

	for _, invoice := range invoices {
		if !invoice.PeriodStart.After(subscription.CurrentPeriodStart) {
			continue
		}
		if _, _, err := s.deps.Repository.ExpireInvoice(ctx, scope, invoice.ID); err != nil {
			s.deps.Logger.Warn("could not withdraw an upcoming invoice", zap.Error(err))
		}
	}
}

// Invoices lists what a workspace was charged.
func (s *Service) Invoices(ctx context.Context, scope domain.Scope, filter domain.InvoiceFilter) ([]domain.Invoice, error) {
	if scope.IsZero() {
		return nil, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	return s.deps.Repository.Invoices(ctx, scope, filter)
}

// Limits reports what the workspace's package allows it to create.
//
// It reads the package in force rather than the one a person is moving to: a cap
// that changed the moment somebody clicked would let a workspace outgrow a package
// it has not paid for.
func (s *Service) Limits(ctx context.Context, workspaceID uuid.UUID) (domain.Limits, error) {
	_, plan, err := s.live(ctx, workspaceID)
	if err != nil {
		return domain.Limits{}, err
	}

	return domain.Limits{
		MaxAgents:       plan.MaxAgents,
		MaxIntegrations: plan.MaxIntegrations,
		MaxMembers:      plan.MaxMembers,
	}, nil
}

// ReadOnly reports whether the workspace is past its grace window.
func (s *Service) ReadOnly(ctx context.Context, workspaceID uuid.UUID) (bool, error) {
	subscription, _, err := s.live(ctx, workspaceID)
	if err != nil {
		return false, err
	}

	return subscription.Entitlement(s.now(), s.grace) == domain.EntitlementReadOnly, nil
}

// notify sends one notice, logging a failure rather than failing the caller's work.
//
// A notice that cannot be sent must not undo what it is about: an allowance that was
// spent is spent whether or not the email arrived.
func (s *Service) notify(ctx context.Context, scope domain.Scope, notice domain.Notice) {
	if s.deps.Notifier == nil {
		return
	}
	if err := s.deps.Notifier.Notify(ctx, scope, notice); err != nil && ctx.Err() == nil {
		s.deps.Logger.Warn("could not tell a workspace about its package",
			zap.String("kind", notice.Kind), zap.Error(err))
	}
}

// NoticePlanChanged is the line a package change writes into the workspace's stream.
// It is declared here rather than in the domain because it is not a notice the sweep
// sends: it is the record of something a person did.
const NoticePlanChanged = "billing.plan_changed"

// errNoRepository is the answer when the module was built without its store.
var errNoRepository = errors.New("billing: the repository is not configured")
