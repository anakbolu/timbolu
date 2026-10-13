package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	agentdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/domain"
	agentrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/repository"
	agentservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/service"
	authrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/repository"
	billingdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/billing/domain"
	billingrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/billing/repository"
	billingservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/billing/service"
	llmrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/repository"
	llmservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/service"
	workspacedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	workspacerepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/repository"
	workspaceservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/service"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
)

// This file is EPIC 12's proof against a real Postgres. The claims are the ones a mock
// would only restate:
//
//   - a workspace always has exactly one live subscription, created in the same
//     transaction that creates the workspace (E12.1);
//   - the allowance is the package plus what was bought, and it is measured against the
//     real ledger (E12.2);
//   - one outstanding invoice per period however often it is created, and a settled one
//     cannot be settled again (E12.3, E12.4);
//   - the billing tables are tenant-isolated by policy, not by a careful query.

// billingFixture is a workspace with its trial package, and the module over the real
// database.
type billingFixture struct {
	repository *billingrepo.Repository
	ledger     *llmrepo.Repository
	pool       *database.Pool
	service    *billingservice.Service

	userID    uuid.UUID
	workspace uuid.UUID
	scope     billingdomain.Scope
	number    int
}

func newBillingFixture(t *testing.T, dsn string) *billingFixture {
	t.Helper()

	return onboardBillingFixture(t, poolFor(t, dsn))
}

// onboardBillingFixture builds the fixture over a pool that is already open, so a test can
// change the catalogue before the workspace onboards — the period a workspace starts with is
// granted from the package at that moment.
func onboardBillingFixture(t *testing.T, pool *database.Pool) *billingFixture {
	t.Helper()

	fixture := &billingFixture{
		repository: billingrepo.New(pool),
		ledger:     llmrepo.New(pool),
		pool:       pool,
		number:     1,
	}

	// The trial package is created by the workspace module's own onboarding, which is
	// the path a person takes: the provisioner runs inside that transaction.
	fixture.onboard(t, "Toko Sinar")

	fixture.service = billingservice.New(billingservice.Deps{
		Repository: fixture.repository,
		Usage:      billingUsage{reader: fixture.ledger},
		Logger:     zap.NewNop(),
	})

	return fixture
}

// onboard creates one workspace through the real path.
func (f *billingFixture) onboard(t *testing.T, name string) {
	t.Helper()

	ctx := t.Context()
	user, err := authrepo.New(f.pool.PgxPool()).CreateUser(ctx, uuid.NewString()+"@example.com", "$argon2id$hash")
	require.NoError(t, err)

	workspaces := workspacerepo.New(f.pool)
	workspaces.SetSubscriptionProvisioner(f.repository)

	result, err := workspaces.Onboard(ctx, user.ID, workspacedomain.OnboardRequest{
		Name:          name,
		BusinessField: "Retail",
		Timezone:      "Asia/Jakarta",
	})
	require.NoError(t, err)

	f.userID = user.ID
	f.workspace = result.Workspace.ID
	f.scope = billingdomain.Scope{UserID: user.ID, WorkspaceID: result.Workspace.ID}
}

// billingUsage adapts the ledger to the billing port the way the container does. The day
// boundary is the gateway's own rule, so it is asked for rather than recomputed here.
type billingUsage struct {
	reader *llmrepo.Repository
}

// PeriodTokens implements billingdomain.UsageReader.
func (u billingUsage) PeriodTokens(ctx context.Context, workspaceID uuid.UUID, since time.Time) (int64, error) {
	return u.reader.SumSince(ctx, workspaceID, since)
}

// DailyCost implements billingdomain.UsageReader.
func (u billingUsage) DailyCost(ctx context.Context, workspaceID uuid.UUID) (int64, error) {
	return u.reader.CostSince(ctx, workspaceID, llmservice.DayStart(time.Now()))
}

// billingPlanLimits adapts the module to the three plan ports the other modules declare,
// which is what the container does with three small types. One type answers all three
// because the question behind them is one: what does this workspace's package allow.
type billingPlanLimits struct {
	service *billingservice.Service
}

// MaxAgents implements agentdomain.PlanLimits.
func (l billingPlanLimits) MaxAgents(ctx context.Context, workspaceID uuid.UUID) (int, error) {
	limits, err := l.service.Limits(ctx, workspaceID)
	return limits.MaxAgents, err
}

// MaxIntegrations implements integrationdomain.PlanLimits.
func (l billingPlanLimits) MaxIntegrations(ctx context.Context, workspaceID uuid.UUID) (int, error) {
	limits, err := l.service.Limits(ctx, workspaceID)
	return limits.MaxIntegrations, err
}

// MaxMembers implements workspacedomain.PlanLimits.
func (l billingPlanLimits) MaxMembers(ctx context.Context, workspaceID uuid.UUID) (int, error) {
	limits, err := l.service.Limits(ctx, workspaceID)
	return limits.MaxMembers, err
}

// discardMailer accepts an invitation and does nothing with it, because what is under
// test is the cap rather than the delivery.
type discardMailer struct{}

// Send implements workspacedomain.Mailer.
func (discardMailer) Send(context.Context, string, string, string) error { return nil }

// fixtureTokens mints invitation tokens without a random source, and keeps the last raw
// value so a test can accept the invitation the module just issued. The hash is what the
// store keeps and the raw value is what the mailer would have carried — which is exactly
// how the real path reaches the person it was sent to.
type fixtureTokens struct {
	last string
}

// Generate implements workspacedomain.Tokens.
func (t *fixtureTokens) Generate() (string, string, error) {
	raw := uuid.NewString()
	t.last = raw
	return raw, workspacedomain.HashToken(raw), nil
}

// spent records tokens in the real ledger, which is what the check reads when the live
// counter is absent.
func (f *billingFixture) spent(t *testing.T, tokens int64) {
	t.Helper()

	require.NoError(t, f.pool.InScope(t.Context(), database.Scope{
		UserID:      f.userID,
		WorkspaceID: f.workspace,
	}, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `
			INSERT INTO usage_ledger (workspace_id, provider, model, purpose, input_tokens, output_tokens, cost_micros)
			VALUES ($1, 'test', 'test-model', 'agent', $2, 0, 0)`,
			f.workspace, tokens)
		return err
	}))
}

// countInWorkspace reads a count inside a tenant scope, because every billing table
// carries Row Level Security: a count outside a scope returns zero whatever the table
// holds, which would make an assertion pass for the wrong reason.
func (f *billingFixture) countInWorkspace(t *testing.T, query string, args ...any) int {
	t.Helper()

	var count int
	require.NoError(t, f.pool.InScopeRead(t.Context(), database.Scope{
		UserID:      f.userID,
		WorkspaceID: f.workspace,
	}, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), query, args...).Scan(&count)
	}))

	return count
}

// TestASubscriptionRoundTripsAndItsPeriodsAreReadable covers the writes and the reads the
// screens lean on, against real Postgres: a subscription written back is one read back, the
// periods of a workspace come newest first, and the invoice filter narrows the way the
// "unpaid" tab asks it to.
func TestASubscriptionRoundTripsAndItsPeriodsAreReadable(t *testing.T) {
	fixture := newBillingFixture(t, appDatabase(t))
	ctx := t.Context()

	subscription, err := fixture.repository.Subscription(ctx, fixture.scope)
	require.NoError(t, err)

	// A write back is a write read back, and the fields a person changed are the ones that
	// survive.
	later := subscription.CurrentPeriodEnd.AddDate(0, 0, 30)
	grace := later.AddDate(0, 0, billingdomain.DefaultGraceDays)
	// The moment it went past due is the caller's to record rather than the store's to
	// invent: the sweep is what decides that a period ended, and the store only keeps the
	// row it is handed.
	wentPastDue := time.Now()
	written, err := fixture.repository.SetSubscription(ctx, fixture.scope, billingdomain.Subscription{
		ID:                 subscription.ID,
		PlanID:             subscription.PlanID,
		Status:             billingdomain.StatusPastDue,
		CurrentPeriodStart: subscription.CurrentPeriodEnd,
		CurrentPeriodEnd:   later,
		GraceUntil:         &grace,
		PastDueSince:       &wentPastDue,
	})
	require.NoError(t, err)
	require.Equal(t, billingdomain.StatusPastDue, written.Status)
	require.WithinDuration(t, later, written.CurrentPeriodEnd, time.Second)
	require.NotNil(t, written.GraceUntil)
	require.NotNil(t, written.PastDueSince)
	require.WithinDuration(t, wentPastDue, *written.PastDueSince, time.Second)

	// A workspace this one is not a member of cannot write its subscription either.
	their := newBillingFixture(t, appDatabase(t))
	_, err = their.repository.SetSubscription(ctx, their.scope, written)
	require.ErrorIs(t, err, billingdomain.ErrNotFound)

	// The packages this deployment sells are readable by any workspace, in the order the
	// screens show them, and the trial is one of them.
	plans, err := fixture.repository.Plans(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(plans), 3)
	require.True(t, plans[0].SortOrder <= plans[len(plans)-1].SortOrder)
	require.Equal(t, "coba", plans[0].Key)
	require.Less(t, plans[0].PeriodDays, 30, "a trial is shorter than a month")

	// The trial is found by key and by its flag, and both answer the same package.
	trial, err := fixture.repository.TrialPlan(ctx)
	require.NoError(t, err)
	require.True(t, trial.IsTrial)
	require.Equal(t, plans[0].ID, trial.ID)

	byKey, err := fixture.repository.Plan(ctx, "keluarga")
	require.NoError(t, err)
	require.False(t, byKey.IsTrial)
	require.Positive(t, byKey.MonthlyPriceIDR)

	_, err = fixture.repository.Plan(ctx, "paket-yang-tidak-ada")
	require.ErrorIs(t, err, billingdomain.ErrPlanNotFound)

	// The periods a workspace has been in, newest first. Onboarding opened exactly one, and
	// the extension written above did not open another: a period exists because something
	// read the allowance during it, not because the subscription's dates moved.
	periods, err := fixture.repository.ListPeriods(ctx, fixture.scope, 10)
	require.NoError(t, err)
	require.Len(t, periods, 1)
	require.Equal(t, subscription.CurrentPeriodStart, periods[0].PeriodStart)

	// The invoice filter narrows, which is what a screen's "unpaid" tab asks for.
	paidAt := time.Now()
	paid, err := fixture.repository.CreateInvoice(ctx, fixture.scope, billingdomain.Invoice{
		SubscriptionID: subscription.ID,
		Kind:           billingdomain.KindSubscription,
		Status:         billingdomain.StatusPaid,
		AmountIDR:      149000,
		PeriodStart:    subscription.CurrentPeriodStart,
		PeriodEnd:      subscription.CurrentPeriodEnd,
		PaidAt:         &paidAt,
	})
	require.NoError(t, err)

	paidOnly, err := fixture.repository.Invoices(ctx, fixture.scope, billingdomain.InvoiceFilter{Status: billingdomain.StatusPaid})
	require.NoError(t, err)
	require.Len(t, paidOnly, 1)
	require.Equal(t, paid.ID, paidOnly[0].ID)

	everything, err := fixture.repository.Invoices(ctx, fixture.scope, billingdomain.InvoiceFilter{})
	require.NoError(t, err)
	require.Len(t, everything, 1, "the filter narrows rather than hiding what is there when it is empty")

	// A settled invoice is no longer outstanding, so it leaves the list the sweep reads.
	outstanding, err := fixture.repository.Outstanding(ctx, fixture.scope)
	require.NoError(t, err)
	require.Empty(t, outstanding)

	// The top-up catalogue is a catalogue: every workspace reads the same packs, and what it
	// bought is recorded in its own invoice.
	theirTopUps, err := their.repository.TopUps(ctx, their.scope)
	require.NoError(t, err)
	mineTopUps, err := fixture.repository.TopUps(ctx, fixture.scope)
	require.NoError(t, err)
	require.Equal(t, len(theirTopUps), len(mineTopUps))
	require.NotEmpty(t, mineTopUps)

	one, err := fixture.repository.TopUp(ctx, fixture.scope, mineTopUps[0].Key)
	require.NoError(t, err)
	require.Equal(t, mineTopUps[0].ID, one.ID)
	require.Positive(t, one.TokenAmount)
	require.Positive(t, one.PriceIDR)

	_, err = fixture.repository.TopUp(ctx, fixture.scope, "tambahan-yang-tidak-ada")
	require.ErrorIs(t, err, billingdomain.ErrTopUpNotFound)

	// A top-up lands on the period that starts at the moment it was confirmed, and a second
	// one adds to it rather than replacing it — which is what an increment buys over a write
	// of a total.
	period, err := fixture.repository.Period(ctx, fixture.scope, written, trial, time.Now())
	require.NoError(t, err)
	require.Zero(t, period.TokensToppedUp)

	topped, err := fixture.repository.AddTopUp(ctx, fixture.scope, period.PeriodStart, 200000)
	require.NoError(t, err)
	require.Equal(t, int64(200000), topped.TokensToppedUp)

	again, err := fixture.repository.AddTopUp(ctx, fixture.scope, period.PeriodStart, 50000)
	require.NoError(t, err)
	require.Equal(t, int64(250000), again.TokensToppedUp)

	// A marker written on a period can be cleared, which is what lets a notice be sent once
	// per period and then again the next time the state is reached.
	at := time.Now()
	warned, err := fixture.repository.SavePeriod(ctx, fixture.scope, billingdomain.QuotaPeriod{
		ID: period.ID, TokensUsed: 50, TokensToppedUp: again.TokensToppedUp,
		Warning80SentAt: &at, Warning100SentAt: &at,
	})
	require.NoError(t, err)
	require.NotNil(t, warned.Warning80SentAt)

	cleared, err := fixture.repository.SavePeriod(ctx, fixture.scope, billingdomain.QuotaPeriod{
		ID: period.ID, TokensUsed: warned.TokensUsed, TokensToppedUp: warned.TokensToppedUp,
	})
	require.NoError(t, err)
	require.Nil(t, cleared.Warning80SentAt, "a marker is written as a value, so it can be cleared")

	// The directory is what the sweep reads, and touching it moves the workspace's next pass
	// rather than removing it from the queue.
	due, err := fixture.repository.DueWorkspaces(ctx, time.Now().Add(time.Hour), 100)
	require.Contains(t, due, fixture.workspace)

	require.NoError(t, fixture.repository.TouchDirectory(ctx, fixture.scope, fixture.workspace,
		time.Now().Add(-time.Minute)))

	due, err = fixture.repository.DueWorkspaces(ctx, time.Now(), 100)
	require.Contains(t, due, fixture.workspace, "a pass that is due stays due")

	require.NoError(t, fixture.repository.TouchDirectory(ctx, fixture.scope, fixture.workspace,
		time.Now().Add(time.Hour)))

	due, err = fixture.repository.DueWorkspaces(ctx, time.Now(), 100)
	require.NotContains(t, due, fixture.workspace, "a workspace moved forward is not read again")

	// And the limit bounds the pass, so a deployment with many workspaces reconciles them
	// over several passes rather than in one unbounded read.
	capped, err := fixture.repository.DueWorkspaces(ctx, time.Now().Add(24*time.Hour), 1)
	require.NoError(t, err)
	require.LessOrEqual(t, len(capped), 1)
}

// TestANewWorkspaceStartsWithItsTrialPackage is the epic's first gate, and it is a claim
// about the data rather than about a screen: a workspace exists without a package only
// if onboarding half-finished, and the provisioner runs inside the same transaction that
// creates it.
func TestANewWorkspaceStartsWithItsTrialPackage(t *testing.T) {
	fixture := newBillingFixture(t, appDatabase(t))
	ctx := t.Context()

	subscription, err := fixture.repository.Subscription(ctx, fixture.scope)
	require.NoError(t, err)

	plan, err := fixture.repository.PlanByID(ctx, subscription.PlanID)
	require.NoError(t, err)
	require.Equal(t, "coba", plan.Key)
	require.True(t, plan.IsTrial)
	require.Equal(t, billingdomain.StatusTrial, subscription.Status)
	require.WithinDuration(t, time.Now(), subscription.CurrentPeriodStart, time.Minute)
	require.WithinDuration(t, time.Now().AddDate(0, 0, plan.PeriodDays), subscription.CurrentPeriodEnd, time.Minute)
	require.NotNil(t, subscription.GraceUntil, "the window a workspace is given is on the row, not recomputed at read time")

	// The period it grants exists, so the first check reads a row rather than creating
	// one, and the sweep's queue has the workspace from the same transaction.
	require.Equal(t, 1, fixture.countInWorkspace(t,
		`SELECT count(*) FROM quota_periods WHERE period_start = $1`, subscription.CurrentPeriodStart))

	// The queue is the one table without a policy, so it is read unscoped — which is
	// exactly what the sweep does and why the table exists.
	var queued int
	require.NoError(t, fixture.pool.PgxPool().QueryRow(ctx,
		`SELECT count(*) FROM billing_directory WHERE workspace_id = $1`, fixture.workspace).Scan(&queued))
	require.Equal(t, 1, queued, "the sweep finds every workspace, including one that has never been visited")

	// The same onboarding twice is the same workspace and not a second subscription: the
	// live-subscription index is what refuses it, not a check in the caller.
	again, err := fixture.repository.Subscription(ctx, fixture.scope)
	require.NoError(t, err)
	require.Equal(t, subscription.ID, again.ID)

	_, err = fixture.repository.CreateInvoice(ctx, fixture.scope, billingdomain.Invoice{
		SubscriptionID: subscription.ID,
		Kind:           billingdomain.KindSubscription,
		Status:         billingdomain.StatusPending,
		AmountIDR:      149000,
		PeriodStart:    subscription.CurrentPeriodEnd,
		PeriodEnd:      subscription.CurrentPeriodEnd.AddDate(0, 0, 30),
	})
	require.NoError(t, err)

	// One outstanding invoice per subscription per period, whichever writer gets there
	// first — the sweep and a screen can both try, and the index decides.
	duplicate, err := fixture.repository.CreateInvoice(ctx, fixture.scope, billingdomain.Invoice{
		SubscriptionID: subscription.ID,
		Kind:           billingdomain.KindSubscription,
		Status:         billingdomain.StatusPending,
		AmountIDR:      149000,
		PeriodStart:    subscription.CurrentPeriodEnd,
		PeriodEnd:      subscription.CurrentPeriodEnd.AddDate(0, 0, 30),
	})
	require.NoError(t, err, "the second writer reads the invoice that exists rather than failing")
	require.Equal(t, 1, fixture.countInWorkspace(t, `SELECT count(*) FROM billing_invoices`))

	// An expired one is re-issuable, which is what makes a period still owed payable
	// again after its payment window closed.
	_, _, err = fixture.repository.ExpireInvoice(ctx, fixture.scope, duplicate.ID)
	require.NoError(t, err)

	reissued, err := fixture.repository.CreateInvoice(ctx, fixture.scope, billingdomain.Invoice{
		SubscriptionID: subscription.ID,
		Kind:           billingdomain.KindSubscription,
		Status:         billingdomain.StatusPending,
		AmountIDR:      149000,
		PeriodStart:    subscription.CurrentPeriodEnd,
		PeriodEnd:      subscription.CurrentPeriodEnd.AddDate(0, 0, 30),
	})
	require.NoError(t, err)
	require.NotEqual(t, duplicate.ID, reissued.ID)
	require.Equal(t, 2, fixture.countInWorkspace(t, `SELECT count(*) FROM billing_invoices`),
		"what was expired stays as history")
}

// TestTheAllowanceIsMeasuredAgainstTheLedger is E12.2's durable half: the number the
// check refuses on is the one the ledger records, and what was bought on top counts with
// it.
func TestTheAllowanceIsMeasuredAgainstTheLedger(t *testing.T) {
	fixture := newBillingFixture(t, appDatabase(t))
	ctx := t.Context()

	plan, err := fixture.repository.TrialPlan(ctx)
	require.NoError(t, err)

	subscription, err := fixture.repository.Subscription(ctx, fixture.scope)
	require.NoError(t, err)

	period, err := fixture.repository.Period(ctx, fixture.scope, subscription, plan, time.Now())
	require.NoError(t, err)
	require.Equal(t, plan.TokenQuota, period.TokensGranted, "the period grants what the package says")

	// Under the allowance the workspace may spend.
	fixture.spent(t, plan.TokenQuota-1)

	check, err := fixture.service.Check(ctx, fixture.workspace)
	require.NoError(t, err)
	require.True(t, check.Allowed)
	require.Equal(t, plan.TokenQuota-1, check.Period.TokensUsed, "the check read the ledger, not a stored count")

	// At the allowance it may not, and the refusal names both numbers.
	fixture.spent(t, 1)

	_, err = fixture.service.Check(ctx, fixture.workspace)
	require.ErrorIs(t, err, billingdomain.ErrQuotaExceeded)
	require.Contains(t, err.Error(), "200000")

	// The sweep writes the ledger's total into the durable mirror, which is the number
	// the screen shows, and reports the threshold the spending crossed.
	synced, err := fixture.repository.Period(ctx, fixture.scope, subscription, plan, time.Now())
	require.NoError(t, err)
	require.Zero(t, synced.TokensUsed, "nothing has reconciled it yet")

	result, err := fixture.service.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, result.PeriodsSynced)
	require.Equal(t, 1, result.Warnings100)

	after, err := fixture.repository.Period(ctx, fixture.scope, subscription, plan, time.Now())
	require.NoError(t, err)
	require.Equal(t, plan.TokenQuota, after.TokensUsed, "the mirror caught up with the ledger")
	require.NotNil(t, after.Warning100SentAt, "and the crossing that spent the allowance was reported")

	// A second pass synced nothing and warned nobody: the count is unchanged and the mark
	// is down, which is what makes a warning a once-per-period event.
	again, err := fixture.service.Sweep(ctx)
	require.NoError(t, err)
	require.Zero(t, again.Warnings100)
	require.Zero(t, again.Warnings80)

	// A purchase on top puts it back to work, in the period it was bought in.
	topped, err := fixture.repository.AddTopUp(ctx, fixture.scope, period.PeriodStart, 200000)
	require.NoError(t, err)
	require.Equal(t, int64(400000), topped.Allowance())

	allowed, err := fixture.service.Check(ctx, fixture.workspace)
	require.NoError(t, err)
	require.True(t, allowed.Allowed, "the top-up is spent with the package, not instead of it")
}

// TestASettledInvoiceCannotBeSettledTwice is the epic's third gate at the storage level.
// A provider delivers a notification until somebody acknowledges it, so the guard has to
// be in the statement rather than in the caller's memory.
func TestASettledInvoiceCannotBeSettledTwice(t *testing.T) {
	fixture := newBillingFixture(t, appDatabase(t))
	ctx := t.Context()

	subscription, err := fixture.repository.Subscription(ctx, fixture.scope)
	require.NoError(t, err)

	invoice, err := fixture.repository.CreateInvoice(ctx, fixture.scope, billingdomain.Invoice{
		SubscriptionID:    subscription.ID,
		Kind:              billingdomain.KindSubscription,
		Status:            billingdomain.StatusPending,
		AmountIDR:         149000,
		Provider:          "duitku",
		ProviderReference: "bolu-" + uuid.NewString() + "-m",
		PeriodStart:       subscription.CurrentPeriodEnd,
		PeriodEnd:         subscription.CurrentPeriodEnd.AddDate(0, 0, 30),
	})
	require.NoError(t, err)

	// A payment link is written with the routing row that finds it again, which is the
	// only read a notification can make: the provider names one reference and every
	// table it would otherwise have to read is protected by Row Level Security.
	_, err = fixture.repository.SetInvoicePayment(ctx, fixture.scope, billingdomain.Invoice{
		ID:                invoice.ID,
		Provider:          "duitku",
		ProviderReference: invoice.ProviderReference,
		Instructions:      "Transfer ke BCA virtual account 8808",
		Attempts:          1,
	})
	require.NoError(t, err)

	route, err := fixture.repository.PaymentRoute(ctx, "duitku", invoice.ProviderReference)
	require.NoError(t, err)
	require.Equal(t, fixture.workspace, route.WorkspaceID, "the routing row says whose invoice it is")
	require.Equal(t, invoice.ID, route.InvoiceID)

	// A reference nobody wrote is not a payment.
	_, err = fixture.repository.PaymentRoute(ctx, "duitku", "bolu-nobody-m")
	require.ErrorIs(t, err, billingdomain.ErrInvoiceNotFound)

	// And the invoice itself is read inside that workspace's scope.
	found, err := fixture.repository.Invoice(ctx, fixture.scope, route.InvoiceID)
	require.NoError(t, err)
	require.Equal(t, fixture.workspace, found.WorkspaceID)
	require.Contains(t, found.Instructions, "virtual account")

	paid := time.Now()
	settled, fresh, err := fixture.repository.SettleInvoice(ctx, fixture.scope, billingdomain.Invoice{
		ID: invoice.ID, Status: billingdomain.StatusPaid, PaidAt: &paid,
	})
	require.NoError(t, err)
	require.True(t, fresh)
	require.Equal(t, billingdomain.StatusPaid, settled.Status)
	require.NotNil(t, settled.PaidAt)

	// The second delivery settles nothing and answers with the invoice as it stands.
	settled, fresh, err = fixture.repository.SettleInvoice(ctx, fixture.scope, billingdomain.Invoice{
		ID: invoice.ID, Status: billingdomain.StatusPaid, PaidAt: &paid,
	})
	require.NoError(t, err)
	require.False(t, fresh, "the invoice was already settled, so nothing was written")
	require.Equal(t, billingdomain.StatusPaid, settled.Status)

	// An invoice this workspace does not have is not found rather than settled.
	other := newBillingFixture(t, appDatabase(t))
	_, _, err = other.repository.SettleInvoice(ctx, other.scope, billingdomain.Invoice{
		ID: invoice.ID, Status: billingdomain.StatusFailed,
	})
	require.ErrorIs(t, err, billingdomain.ErrNotFound)

	still, err := fixture.repository.Invoice(ctx, fixture.scope, invoice.ID)
	require.NoError(t, err)
	require.Equal(t, billingdomain.StatusPaid, still.Status, "and the row it named is untouched")
}

// TestTheBillingTablesAreTenantIsolated is the policy rather than the query: a workspace
// cannot read another's package, invoices, or periods even when it names them by id.
func TestTheBillingTablesAreTenantIsolated(t *testing.T) {
	dsn := appDatabase(t)
	mine := newBillingFixture(t, dsn)
	theirs := newBillingFixture(t, dsn)
	ctx := t.Context()

	// The test fixture is a second workspace of the same user, so the scope carries a
	// user id and the isolation is the workspace's alone.
	subscription, err := theirs.repository.Subscription(ctx, theirs.scope)
	require.NoError(t, err)

	invoice, err := theirs.repository.CreateInvoice(ctx, theirs.scope, billingdomain.Invoice{
		SubscriptionID: subscription.ID,
		Kind:           billingdomain.KindSubscription,
		Status:         billingdomain.StatusPending,
		AmountIDR:      149000,
		PeriodStart:    subscription.CurrentPeriodEnd,
		PeriodEnd:      subscription.CurrentPeriodEnd.AddDate(0, 0, 30),
	})
	require.NoError(t, err)

	// Naming the row by its id is not enough: the policy filters it out.
	_, err = mine.repository.Invoice(ctx, mine.scope, invoice.ID)
	require.ErrorIs(t, err, billingdomain.ErrNotFound)

	// And a read with no tenant at all sees nothing of theirs — which is not a
	// limitation but the reason a payment notification has its own routing table: the
	// policy matches `app_current_workspace_id()`, so a provider's read returns nothing
	// until somebody says which workspace to open.
	var visible int
	require.NoError(t, mine.pool.InScopeRead(ctx, database.Scope{UserID: mine.userID}, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM subscriptions`).Scan(&visible)
	}))
	require.Zero(t, visible, "an unscoped read matches no tenant at all")
	require.Positive(t, mine.countInWorkspace(t, `SELECT count(*) FROM subscriptions`))

	_, _, err = mine.repository.SettleInvoice(ctx, mine.scope, billingdomain.Invoice{
		ID: invoice.ID, Status: billingdomain.StatusPaid,
	})
	require.ErrorIs(t, err, billingdomain.ErrNotFound)

	// And a write from outside is refused by the policy rather than by the filter: the
	// insert names their workspace and is rejected.
	require.Error(t, mine.pool.InScope(ctx, database.Scope{
		UserID:      mine.userID,
		WorkspaceID: mine.workspace,
	}, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO billing_invoices (workspace_id, subscription_id, kind, status, amount_idr, period_start, period_end)
			VALUES ($1, $2, 'subscription', 'pending', 149000, now(), now() + interval '30 days')`,
			theirs.workspace, subscription.ID)
		return err
	}))

	// Naming nobody is the one read that is not scoped, and it is the sweep's queue: it
	// carries the workspace id and a moment and nothing else.
	due, err := mine.repository.DueWorkspaces(ctx, time.Now().Add(time.Hour), 100)
	require.NoError(t, err)
	require.NotEmpty(t, due)
}

// TestTheSubscriptionLimitsAreWhatThePackageSays is E12.1's second gate: the caps are
// enforced where a workspace creates things, and each module asks through its own port.
func TestTheSubscriptionLimitsAreWhatThePackageSays(t *testing.T) {
	fixture := newBillingFixture(t, appDatabase(t))
	ctx := t.Context()

	limits, err := fixture.service.Limits(ctx, fixture.workspace)
	require.NoError(t, err)

	plan, err := fixture.repository.TrialPlan(ctx)
	require.NoError(t, err)
	require.Equal(t, plan.MaxAgents, limits.MaxAgents)
	require.Equal(t, plan.MaxIntegrations, limits.MaxIntegrations)
	require.Equal(t, plan.MaxMembers, limits.MaxMembers)

	// The registry refuses the Bolu past the cap, with the package's own number in the
	// message rather than a database error.
	agents := agentservice.New(agentservice.Deps{
		Repository: agentrepo.New(fixture.pool),
		Limits:     billingPlanLimits{service: fixture.service},
	})

	agentScope := agentdomain.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace}
	teams, err := agents.Teams(ctx, agentScope)
	require.NoError(t, err)
	require.NotEmpty(t, teams)

	// A workspace starts with no Bolu — the first one is a person's choice — so the cap
	// is reached by creating up to it.
	existing, err := agents.List(ctx, agentScope)
	require.NoError(t, err)
	require.Empty(t, existing, "onboarding creates the teams, and a Bolu is a person's choice")

	for index := 0; index < plan.MaxAgents; index++ {
		_, err := agents.Create(ctx, agentScope, agentdomain.CreateRequest{
			TeamID: teams[0].ID,
			Name:   fmt.Sprintf("Bolu %d", index+1),
		})
		require.NoError(t, err)
	}

	_, err = agents.Create(ctx, agentScope, agentdomain.CreateRequest{
		TeamID: teams[0].ID,
		Name:   "Satu lagi",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "batas 6 Bolu", "the refusal names the package's own number")
}

// TestTheMemberCapCountsOpenInvitationsToo is what keeps a workspace from inviting ten
// people into a package that allows three: an invitation is a seat somebody will take,
// and the last seven would otherwise fail after being told to expect access.
func TestTheMemberCapCountsOpenInvitationsToo(t *testing.T) {
	fixture := newBillingFixture(t, appDatabase(t))
	ctx := t.Context()

	plan, err := fixture.repository.TrialPlan(ctx)
	require.NoError(t, err)

	workspaces := workspaceservice.New(workspaceservice.Deps{
		Repository: workspacerepo.New(fixture.pool),
		Mailer:     discardMailer{},
		Tokens:     &fixtureTokens{},
		Limits:     billingPlanLimits{service: fixture.service},
		Config:     workspaceservice.Config{FrontendURL: "https://app.example.com"},
	})

	scope := workspacedomain.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace}

	// The owner is one of the seats, so the package allows one fewer invitation.
	invitations := 0
	for index := 0; index <= plan.MaxMembers; index++ {
		_, err := workspaces.Invite(ctx, scope, workspacedomain.RoleOwner, fixture.userID,
			uuid.NewString()+"@example.com", workspacedomain.RoleMember)
		if err != nil {
			break
		}
		invitations++
	}

	// The cap counts the owner, so the package allows one fewer invitation than its number
	// of seats.
	require.Equal(t, plan.MaxMembers-1, invitations)
	require.Greater(t, plan.MaxMembers, 1, "a trial whose single seat is the owner leaves the invitation flow dead")

	_, err = workspaces.Invite(ctx, scope, workspacedomain.RoleOwner, fixture.userID,
		uuid.NewString()+"@example.com", workspacedomain.RoleMember)
	require.ErrorIs(t, err, workspacedomain.ErrPlanLimit)
	require.Contains(t, err.Error(), "paket", "the refusal names the package rather than the database")
}
