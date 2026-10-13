package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Repository is what the billing module stores: the packages it sells, the
// subscription a workspace is on, the invoices it owes, the periods its allowance
// is counted over, and the top-up packs it may buy.
type Repository interface {
	// Plans lists the packages this deployment sells, in the order the screens
	// show them.
	Plans(ctx context.Context) ([]Plan, error)
	// Plan reads one package by its key, which is what a person chooses.
	Plan(ctx context.Context, key string) (Plan, error)
	// PlanByID reads the package a subscription names.
	PlanByID(ctx context.Context, id uuid.UUID) (Plan, error)
	// TrialPlan reads the package a new workspace starts on. It is the row the
	// migration seeds, so a deployment that renamed it still finds it by key.
	TrialPlan(ctx context.Context) (Plan, error)

	// Subscription reads the workspace's live package. A workspace always has
	// one, so a read that finds none is a broken invariant rather than a state.
	Subscription(ctx context.Context, scope Scope) (Subscription, error)
	// SetSubscription writes a subscription back: its status, its period, and its
	// grace window.
	SetSubscription(ctx context.Context, scope Scope, subscription Subscription) (Subscription, error)
	// DueWorkspaces lists the workspaces the sweep still has to look at, across
	// every workspace, oldest first.
	//
	// It is an unscoped read, and the reason is the one the fleet directory has:
	// the sweep runs on behalf of the deployment rather than of a workspace, and
	// there is no tenant to open a scope with until a row says which one. It reads
	// `billing_directory`, which carries the workspace id and a moment and nothing
	// else; everything it then does runs inside that workspace's scope.
	DueWorkspaces(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error)
	// TouchDirectory moves a workspace's next pass forward.
	TouchDirectory(ctx context.Context, scope Scope, workspaceID uuid.UUID, nextSyncAt time.Time) error

	// Period reads the period a subscription is in at a moment, opening it from the
	// subscription's own times when it does not exist yet.
	//
	// Opening it here rather than in the caller is what makes a period turn exactly
	// once: the insert is the row's own unique key, so two callers at the boundary
	// write one period between them.
	Period(ctx context.Context, scope Scope, subscription Subscription, plan Plan, at time.Time) (QuotaPeriod, error)
	// SavePeriod writes a period back: its usage, its top-up, and which warnings
	// were sent.
	SavePeriod(ctx context.Context, scope Scope, period QuotaPeriod) (QuotaPeriod, error)
	// AddTopUp adds tokens to the period that starts at a moment.
	AddTopUp(ctx context.Context, scope Scope, periodStart time.Time, tokens int64) (QuotaPeriod, error)
	// ExpireInvoice closes an invoice whose payment window has passed.
	ExpireInvoice(ctx context.Context, scope Scope, id uuid.UUID) (Invoice, bool, error)

	// Invoice reads one invoice.
	Invoice(ctx context.Context, scope Scope, id uuid.UUID) (Invoice, error)
	// PaymentRoute answers which workspace and which invoice a provider's own
	// reference names.
	//
	// It is the one unscoped read a payment needs, and it has to be: a provider does
	// not know which workspace it is talking about, and every table it would have to
	// read is protected by Row Level Security — so a read with no tenant returns
	// nothing. The routing row carries the two ids and nothing else, and everything
	// it points at is read inside that workspace's scope.
	PaymentRoute(ctx context.Context, provider, reference string) (PaymentRoute, error)
	// InvoiceForPeriod reads the invoice of one period and kind, which is what makes
	// creating one idempotent: the unique index answers, and the caller reads what is
	// there instead of writing a second.
	InvoiceForPeriod(ctx context.Context, scope Scope, subscriptionID uuid.UUID, kind string, periodStart time.Time) (Invoice, error)
	// Invoices lists a workspace's invoices, newest first.
	Invoices(ctx context.Context, scope Scope, filter InvoiceFilter) ([]Invoice, error)
	// Outstanding lists what a workspace still owes, oldest first, for the sweep
	// and the screen.
	Outstanding(ctx context.Context, scope Scope) ([]Invoice, error)
	// CreateInvoice writes one invoice, idempotently against the period it names.
	CreateInvoice(ctx context.Context, scope Scope, invoice Invoice) (Invoice, error)
	// SettleInvoice records an invoice's outcome and whether the payment was new.
	//
	// It answers false when the invoice already had that outcome, which is what
	// makes a replayed webhook harmless: the caller only extends a period when the
	// settlement was the first one.
	SettleInvoice(ctx context.Context, scope Scope, invoice Invoice) (Invoice, bool, error)
	// SetInvoicePayment records the payment link a provider returned, or why it
	// could not be created.
	SetInvoicePayment(ctx context.Context, scope Scope, invoice Invoice) (Invoice, error)

	// TopUps lists the packs a workspace may buy.
	TopUps(ctx context.Context, scope Scope) ([]TopUpPackage, error)
	// TopUp reads one pack by its key.
	TopUp(ctx context.Context, scope Scope, key string) (TopUpPackage, error)
}

// UsageReader reads the token and cost totals a workspace recorded in the ledger.
//
// It is a port for the reason the quota policy is one: the billing module owns
// what a workspace may spend and the ledger owns what it did spend, and neither
// imports the other. The container hands over the model gateway's own reader.
//
// The day boundary is not a parameter. It belongs to whoever owns the ledger, and
// passing it in would put the same Jakarta midnight in two modules — which is the
// shape of a bug where one of them is changed and the other is not.
type UsageReader interface {
	// PeriodTokens sums the tokens recorded since a moment, which is the durable
	// fallback when the live counter is unavailable.
	PeriodTokens(ctx context.Context, workspaceID uuid.UUID, since time.Time) (int64, error)
	// DailyCost is the cost recorded today, in micro-rupiah.
	DailyCost(ctx context.Context, workspaceID uuid.UUID) (int64, error)
}

// SpendCounter is the live token and cost total of a workspace, which is the fast
// path the quota check reads.
//
// It is a port rather than a direct Redis call so the module can run without
// Redis: the counter answers nil and the ledger answers instead, which is slower
// and correct rather than faster and wrong.
type SpendCounter interface {
	// PeriodTokens is the tokens recorded for the current period.
	PeriodTokens(ctx context.Context, workspaceID uuid.UUID) (int64, error)
	// DailyCost is the cost recorded for the current day, in micro-rupiah.
	DailyCost(ctx context.Context, workspaceID uuid.UUID) (int64, error)
}

// Gateway creates a payment and verifies that a payment happened.
//
// It is the seam a provider is replaceable behind: the module knows what an
// invoice is, and the adapter knows what Midtrans calls one. A deployment with no
// gateway configured gets nil, and an invoice is then created without a link —
// which is a real state the product supports, because the first customers are paid
// by bank transfer and confirmed by hand.
type Gateway interface {
	// Charge asks the provider for a payment for one invoice and returns what a
	// person acts on to pay it, the provider's own reference, and when it stops
	// working.
	Charge(ctx context.Context, invoice Invoice, customer Customer) (Charge, error)
	// Verify checks a delivery's signature and parses it.
	//
	// It answers the reference and the outcome, so the caller decides what to do
	// with them: the provider says a payment happened, and whether that extends a
	// period is the module's business.
	//
	// A delivery it cannot trust is refused with ErrUnverifiable wrapped, because the
	// distinction reaches the provider: a payload that can never verify must not be
	// retried, and the status this becomes is what tells it so.
	Verify(ctx context.Context, delivery Delivery) (Payment, error)
	// Name is the provider's key, which is what an invoice records and what
	// routes a webhook back to the adapter that can verify it.
	Name() string
}

// Customer is what a provider needs to address an invoice to somebody.
type Customer struct {
	WorkspaceID uuid.UUID
	Workspace   string
	Email       string
	Name        string
}

// Charge is what one successful payment creation answers.
type Charge struct {
	// Reference is the provider's own id for the payment, which is what the
	// webhook names and what the invoice records.
	Reference string
	// Instructions is what a person acts on to pay: a URL when the provider gives
	// one, or the virtual account and its bank when it gives an account number
	// instead. The product's first customers pay by bank transfer, so a field that
	// only held a link would be empty for the flow the product actually sells.
	Instructions string
	// ExpiresAt is when the instructions stop working.
	ExpiresAt time.Time
}

// Delivery is one payment notification, as it arrived.
type Delivery struct {
	// Provider is which provider the notification is addressed to, taken from the
	// path it arrived on. A delivery naming a provider this deployment does not
	// speak cannot be verified, so it is refused rather than parsed hopefully.
	Provider string
	// Body is the raw payload.
	Body []byte
	// Headers are the notification's headers, which is where most providers put
	// the signature.
	Headers map[string]string
}

// Payment is what a verified delivery says happened.
type Payment struct {
	Reference string
	AmountIDR int64
	Paid      bool
	// Expired reports a link that ran out rather than a payment that failed.
	Expired bool
	// PaidAt is when the provider says the money landed.
	PaidAt time.Time
}

// Notifier tells a workspace's people that something about their package needs
// them: an allowance running out, a payment not arriving, a workspace gone
// read-only.
//
// It is one port for one decision rather than two, because the email and the line
// in the stream are the same fact reaching two places — and only the email reaches
// somebody who is not looking at the screen.
type Notifier interface {
	Notify(ctx context.Context, scope Scope, notice Notice) error
}

// Notice is one thing a person needs to know about the package.
type Notice struct {
	// Kind is one of the Notice* constants.
	Kind string
	// Reason is what happened, in the user's words.
	Reason string
	// PlanName is the package it concerns.
	PlanName string
	// Threshold is the percentage for a warning notice, or zero.
	Threshold int
	// Used and Allowance are the numbers the notice quotes, or zero.
	Used      int64
	Allowance int64
	// Entitlement is the derived state, so a notice about a read-only workspace
	// and one about an allowance carry the same fact.
	Entitlement string
}

// Activity records what happened to a workspace's package in its durable stream,
// which is what puts it in front of a person without them opening the screen.
type Activity interface {
	Emit(ctx context.Context, event Event) error
}

// Event is one line in the workspace's stream.
type Event struct {
	WorkspaceID uuid.UUID
	Type        string
	Payload     any
}

// Notice kinds.
const (
	// NoticeWarning is an allowance crossing 80% or 100%.
	NoticeWarning = "billing.quota_warning"
	// NoticePastDue is a period ending with an unpaid invoice.
	NoticePastDue = "billing.past_due"
	// NoticeReadOnly is a workspace that can no longer write.
	NoticeReadOnly = "billing.read_only"
	// NoticePaid is a payment that landed, which is worth saying because it
	// changed something a person asked for.
	NoticePaid = "billing.paid"
	// NoticeTopUp is tokens bought.
	NoticeTopUp = "billing.top_up"
)

// Clock reports the current time, so a period boundary and a grace window are
// testable without waiting.
type Clock func() time.Time

// InvoiceFilter narrows an invoice read.
type InvoiceFilter struct {
	// Status narrows to one status, or every status when empty.
	Status string
	Limit  int
}

// ChangeRequest is one package change a person asked for.
type ChangeRequest struct {
	// PlanKey is the package to move to.
	PlanKey string
}

// TopUpRequest is one purchase of tokens.
type TopUpRequest struct {
	// PackageKey is the top-up pack to buy.
	PackageKey string
}

// SweepResult is what one pass of the billing sweep did, reported so a person can
// see that it ran rather than inferring it from the absence of a problem.
type SweepResult struct {
	// Periods synced from the live counter into `quota_periods`.
	PeriodsSynced int
	// Warnings sent, by threshold.
	Warnings80  int
	Warnings100 int
	// Subscriptions moved past due, and those moved read-only.
	PastDue  int
	ReadOnly int
	// Invoices created for periods that turned.
	InvoicesCreated int
	// Invoices expired because their payment window closed.
	InvoicesExpired int
}

// Service is the billing use case contract.
type Service interface {
	// Snapshot is everything the subscription screen shows, in one read.
	Snapshot(ctx context.Context, scope Scope) (Snapshot, error)
	// Plans lists the packages a workspace may move to.
	Plans(ctx context.Context, scope Scope) ([]Plan, error)
	// Change moves the workspace to another package.
	Change(ctx context.Context, scope Scope, request ChangeRequest) (Snapshot, error)
	// Invoices lists what a workspace was charged.
	Invoices(ctx context.Context, scope Scope, filter InvoiceFilter) ([]Invoice, error)
	// Pay asks the provider for a payment link for one invoice.
	Pay(ctx context.Context, scope Scope, invoiceID uuid.UUID) (Invoice, error)
	// BuyTopUp buys tokens inside the period the workspace is in.
	BuyTopUp(ctx context.Context, scope Scope, request TopUpRequest) (Invoice, error)
	// HandlePayment applies a verified payment notification.
	HandlePayment(ctx context.Context, delivery Delivery) error

	// Check answers whether a workspace may spend right now, which is what the
	// gateway and the task runtime both ask.
	Check(ctx context.Context, workspaceID uuid.UUID) (Check, error)
	// Limits reports what the workspace's package allows it to create.
	Limits(ctx context.Context, workspaceID uuid.UUID) (Limits, error)
	// ReadOnly reports whether the workspace is past its grace window.
	ReadOnly(ctx context.Context, workspaceID uuid.UUID) (bool, error)

	// Sweep runs one reconciliation pass over every workspace.
	Sweep(ctx context.Context) (SweepResult, error)
}
