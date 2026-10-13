// Package domain defines the billing contract: the package a workspace is on, the
// quota that package grants, the invoices it owes, and the tokens it bought on top.
//
// Two rules the whole package exists for:
//
//   - a workspace always has exactly one live subscription. Not "usually", and not
//     "one per plan": a workspace with two live packages would have two answers to
//     "what may this workspace spend", and the quota check would pick whichever it
//     read first.
//   - what a workspace may spend is derived, never remembered. The package's tokens
//     plus what it bought, minus what the ledger says it used. A stored "remaining"
//     is a number that can be wrong, and a wrong allowance is either a workspace
//     billed for tokens it did not use or one served for free.
package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Scope is the tenant identity a query runs as. Row Level Security reads both
// parts: WorkspaceID is the active tenant and UserID is the authenticated user.
type Scope struct {
	UserID      uuid.UUID
	WorkspaceID uuid.UUID
}

// IsZero reports whether the scope carries no workspace, which every read here
// requires.
func (s Scope) IsZero() bool { return s.WorkspaceID == uuid.Nil }

// Subscription statuses, matching the check constraint on `subscriptions.status`.
const (
	// StatusTrial is the package a workspace reaches by signing up. It still
	// counts as live: a workspace on it may spend, and its period ends.
	StatusTrial = "trial"
	// StatusActive is a paid package inside its period.
	StatusActive = "active"
	// StatusPastDue is a package whose period ended and whose invoice is unpaid.
	// The workspace is inside its grace window and still works.
	StatusPastDue = "past_due"
	// StatusCanceled is a package a person ended. It is outside the live
	// subscription's unique index, so the workspace may subscribe again.
	StatusCanceled = "canceled"
)

// Invoice kinds, matching the check constraint on `billing_invoices.kind`.
const (
	// KindSubscription is a month of a package.
	KindSubscription = "subscription"
	// KindTopup is a one-off purchase of tokens inside a period.
	KindTopup = "topup"
)

// Invoice statuses, matching the check constraint on `billing_invoices.status`.
const (
	StatusPending = "pending"
	StatusPaid    = "paid"
	StatusFailed  = "failed"
	StatusExpired = "expired"
)

// Entitlement states. They are derived from the subscription's times rather than
// stored, for the reason the task runtime derives "stuck" from a heartbeat: a
// stored flag is a second answer that a missed write can make wrong.
const (
	// EntitlementActive means the workspace may spend and may write.
	EntitlementActive = "active"
	// EntitlementGrace means the period ended and the invoice is unpaid, and the
	// workspace is inside the days it was given to pay.
	EntitlementGrace = "grace"
	// EntitlementReadOnly means the grace window is over. Reads and exports keep
	// working; writes and new work are refused. Nothing is deleted.
	EntitlementReadOnly = "read_only"
)

// DefaultGraceDays is how long a workspace keeps working after its period ends.
//
// A week is chosen rather than a day because the product is paid by bank transfer
// and a virtual account: the money takes a day to land, and a workspace that
// stopped working the hour its period ended would be stopped while its payment was
// in flight.
const DefaultGraceDays = 7

// Usage thresholds at which a workspace is warned, as a percentage of what it may
// spend. They are the two points where a person can still act: at 80% there is
// time to buy a top-up, and at 100% the answer is a top-up or waiting for the
// period to turn.
const (
	WarningThreshold   = 80
	ExhaustedThreshold = 100
)

// DefaultListLimit is the page a client gets when it does not ask for one.
const DefaultListLimit = 50

// MaxListLimit bounds one page.
const MaxListLimit = 200

// Errors the service and the handlers map onto responses.
var (
	// ErrNotFound is a row the caller named that is not there, or is not theirs.
	// The repository translates a missing row into it, and the handlers map it
	// onto 404.
	ErrNotFound             = errors.New("billing: not found")
	ErrPlanNotFound         = errors.New("billing: plan not found")
	ErrSubscriptionNotFound = errors.New("billing: subscription not found")
	ErrInvoiceNotFound      = errors.New("billing: invoice not found")
	ErrTopUpNotFound        = errors.New("billing: top-up package not found")
	ErrInvalidInput         = errors.New("billing: invalid input")
	// ErrQuotaExceeded is the allowance being spent. It is what the gateway and
	// the task runtime report to a person, so its message names both numbers.
	ErrQuotaExceeded = errors.New("kuota token habis")
	// ErrReadOnly is a workspace past its grace window. Writes and new work are
	// refused and nothing is deleted.
	ErrReadOnly = errors.New("langganan workspace sudah lewat masa tenggang")
	// ErrAlreadySubscribed means a workspace asked for a package while it is
	// already on one. Changing a package is a change, not a second subscription.
	ErrAlreadySubscribed = errors.New("billing: the workspace is already subscribed")
	// ErrNoGateway means no payment provider is configured, which is a
	// deployment fact rather than a failure to retry.
	ErrNoGateway = errors.New("billing: no payment gateway is configured")
	// ErrUnverifiable is a payment notification that cannot be trusted: a signature that
	// does not match, a body that is not the shape the provider sends, or a payload for
	// another merchant. It is the caller's payload rather than this deployment's failure,
	// and the status matters — a provider that keeps retrying something that can never
	// verify is a log nobody can read.
	ErrUnverifiable = errors.New("billing: the payment notification cannot be verified")
	// ErrInvoiceSettled means a payment arrived for an invoice that already has
	// an outcome. It is what makes a replayed webhook harmless.
	ErrInvoiceSettled = errors.New("billing: the invoice has already been settled")
)

// Plan is a package: what it costs, what it grants, and what it bounds.
type Plan struct {
	ID   uuid.UUID
	Key  string
	Name string
	// MonthlyPriceIDR is what one period costs, in rupiah.
	MonthlyPriceIDR int64
	// TokenQuota is the tokens one period grants. Zero means unlimited, which is
	// how a self-hosted deployment runs without packages at all.
	TokenQuota int64
	// DailyCostMicros bounds one day's spend, in micro-rupiah. Zero means the
	// package does not bound a day.
	DailyCostMicros int64
	// PeriodDays is how long one period runs.
	PeriodDays int
	// MaxAgents, MaxIntegrations, and MaxMembers are the caps the package
	// enforces. Zero means unlimited.
	MaxAgents       int
	MaxIntegrations int
	MaxMembers      int
	IsTrial         bool
	SortOrder       int
}

// Subscription is one workspace's live package.
type Subscription struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	PlanID      uuid.UUID
	Status      string
	// CurrentPeriodStart and CurrentPeriodEnd bound the period the workspace is
	// in. The allowance is granted per period, so these are what "this month"
	// means when the quota is asked.
	CurrentPeriodStart time.Time
	CurrentPeriodEnd   time.Time
	// GraceUntil is the last moment the workspace keeps working after its period
	// ends. Nil means the workspace is inside its period.
	GraceUntil *time.Time
	// PastDueSince is when the outstanding invoice was created. Nil means nothing
	// is owed.
	PastDueSince *time.Time
	// PendingPlanID is the package the workspace moves to when its current period
	// ends. Nil means it stays on the one it is on. See the migration for why the
	// change is deferred rather than immediate.
	PendingPlanID uuid.UUID
	// PastDueNotifiedAt and ReadOnlyNotifiedAt are when each transition was
	// reported. They are what makes a transition notice a once-per-period event:
	// the state is derived from the times, so without a marker the sweep would
	// repeat itself every pass.
	PastDueNotifiedAt  *time.Time
	ReadOnlyNotifiedAt *time.Time
	CanceledAt         *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Entitlement reports what the subscription allows right now.
//
// The order is the whole rule: a workspace inside its period works; one past it
// works while its grace lasts; one past both is read-only. A canceled
// subscription is read-only immediately, because a person who ended the package
// did not ask for a grace window.
func (s Subscription) Entitlement(now time.Time, grace time.Duration) string {
	if s.Status == StatusCanceled {
		return EntitlementReadOnly
	}
	if s.CurrentPeriodEnd.IsZero() || now.Before(s.CurrentPeriodEnd) {
		return EntitlementActive
	}
	if grace < 0 {
		grace = 0
	}
	until := s.CurrentPeriodEnd.Add(grace)
	if s.GraceUntil != nil && s.GraceUntil.After(until) {
		until = *s.GraceUntil
	}
	if now.Before(until) {
		return EntitlementGrace
	}
	return EntitlementReadOnly
}

// Live reports whether the subscription counts as the workspace's one live
// package, which is the same set the unique index on the table covers.
func (s Subscription) Live() bool { return s.Status != StatusCanceled }

// PaymentRoute is where a provider's reference points: the workspace whose invoice it is,
// and the invoice itself.
type PaymentRoute struct {
	WorkspaceID uuid.UUID
	InvoiceID   uuid.UUID
}

// Invoice is one amount a workspace owes, or owed.
type Invoice struct {
	ID             uuid.UUID
	WorkspaceID    uuid.UUID
	SubscriptionID uuid.UUID
	Kind           string
	Provider       string
	// ProviderReference is the provider's own id for the payment, which is what
	// the webhook names.
	ProviderReference string
	AmountIDR         int64
	// TokenAmount is what a top-up buys. Zero on a month, because the month's
	// tokens are the plan's.
	TokenAmount  int64
	Status       string
	PeriodStart  time.Time
	PeriodEnd    time.Time
	Instructions string
	Attempts     int
	LastError    string
	ExpiresAt    *time.Time
	PaidAt       *time.Time
	CreatedAt    time.Time
}

// Outstanding reports whether the invoice still expects a payment.
func (i Invoice) Outstanding() bool { return i.Status == StatusPending }

// QuotaPeriod is one billing period's allowance and use.
//
// It is the durable mirror of the live counter in Redis. The counter answers the
// hot path and this answers everything else: what the screen shows, what a restart
// recovers from, and whether a warning was already sent.
type QuotaPeriod struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	PeriodStart time.Time
	PeriodEnd   time.Time
	// TokensGranted is the package's allowance for this period.
	TokensGranted int64
	// TokensToppedUp is what was bought inside this period. It expires with the
	// period, which is why it is a separate column from the grant.
	TokensToppedUp int64
	// TokensUsed is the tokens the ledger recorded inside this period.
	TokensUsed int64
	// Warning80SentAt and Warning100SentAt are when each threshold was reported.
	// They are what makes a warning a once-per-period event rather than a
	// message per model call.
	Warning80SentAt  *time.Time
	Warning100SentAt *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Allowance is what the period grants: the package plus what was bought.
func (p QuotaPeriod) Allowance() int64 { return p.TokensGranted + p.TokensToppedUp }

// Exhausted reports whether the allowance is spent.
//
// A zero allowance is unlimited rather than exhausted: it is what a package that
// does not cap tokens looks like, and reading it as zero would refuse every call.
func (p QuotaPeriod) Exhausted() bool {
	allowance := p.Allowance()
	return allowance > 0 && p.TokensUsed >= allowance
}

// Used reports the usage as a percentage of the allowance, or zero when the
// allowance is unlimited.
func (p QuotaPeriod) Used() int {
	allowance := p.Allowance()
	if allowance <= 0 {
		return 0
	}
	return int(p.TokensUsed * 100 / allowance)
}

// Remaining is what is left, or a negative number when the allowance is
// unlimited. The caller decides how to render "no limit" rather than receiving a
// number that looks like a real bound.
func (p QuotaPeriod) Remaining() int64 {
	allowance := p.Allowance()
	if allowance <= 0 {
		return -1
	}
	return allowance - p.TokensUsed
}

// TopUpPackage is a one-off purchase of tokens.
type TopUpPackage struct {
	ID          uuid.UUID
	Key         string
	Name        string
	TokenAmount int64
	PriceIDR    int64
	SortOrder   int
	Active      bool
}

// Usage is the token and cost total of one period, read from the ledger.
type Usage struct {
	Tokens int64
	// CostMicros is what the period cost, in micro-rupiah.
	CostMicros int64
}

// Limits is what one workspace may create, read from its live package.
//
// Zero on any field means unlimited, which is how a deployment without packages
// keeps working: nothing is capped that nobody sold a cap for.
type Limits struct {
	MaxAgents       int
	MaxIntegrations int
	MaxMembers      int
}

// Snapshot is everything the subscription screen shows, in one read.
//
// It is one struct rather than four calls because the four answers have to agree:
// a screen that showed a package, a usage figure, and an invoice list read at
// three different moments could show a workspace that is over its allowance beside
// a period that says it is not.
type Snapshot struct {
	Plan         Plan
	Subscription Subscription
	// PendingPlanKey and PendingPlanName are the package the next period will be
	// billed at, when a person chose one. Empty means the current package
	// continues, which is what the screen says instead of an empty field.
	PendingPlanKey  string
	PendingPlanName string
	Entitlement     string
	// GraceDays is how long the workspace has after the period ends, so the
	// screen can say it without inventing one.
	GraceDays int
	Period    QuotaPeriod
	// Outstanding is what the workspace owes, oldest first.
	Outstanding []Invoice
	// Invoices is the recent history, newest first.
	Invoices []Invoice
	// TopUps is the top-up packs this workspace may buy.
	TopUps []TopUpPackage
	// UsageToday is what the workspace spent today, which is the other bound the
	// quota asks.
	UsageToday Usage
	// Limits is what the package allows.
	Limits Limits
	// ReadOnly is the derived state the middleware and the screens both read.
	ReadOnly bool
}

// Check is what the quota check answers. It is a struct rather than an error
// because three callers need three different parts of it: the gateway needs
// "refuse or not", the sweep needs the thresholds, and the screen needs what is
// left.
type Check struct {
	// Allowed is false when the workspace may not spend.
	Allowed bool
	// Entitlement is one of the Entitlement* constants.
	Entitlement string
	// Period is the current period with its usage.
	Period QuotaPeriod
	// DailyCostMicros and SpentTodayMicros are the other bound. A zero ceiling
	// means the package does not bound a day.
	DailyCostMicros  int64
	SpentTodayMicros int64
}

// DailyExhausted reports whether the day's ceiling is reached.
func (c Check) DailyExhausted() bool {
	return c.DailyCostMicros > 0 && c.SpentTodayMicros >= c.DailyCostMicros
}

// Warnings is the thresholds crossed by a period, which is what the sweep sends
// and what keeps a warning a once-per-period event.
type Warnings struct {
	// At80 and At100 report the threshold that was crossed and not yet reported.
	At80  bool
	At100 bool
}

// Claim returns the thresholds this read crosses and has not reported, so the
// caller marks exactly what it is about to send. A period that jumped from 70% to
// 105% in one interval reports both, in order, rather than only the last: the 80%
// message is the one that gives a person time to act.
func (w Warnings) Claim() []int {
	thresholds := make([]int, 0, 2)
	if w.At80 {
		thresholds = append(thresholds, WarningThreshold)
	}
	if w.At100 {
		thresholds = append(thresholds, ExhaustedThreshold)
	}
	return thresholds
}

// WarningsAt reports which thresholds a period has crossed and not yet reported.
func WarningsAt(period QuotaPeriod) Warnings {
	if period.Allowance() <= 0 {
		// Unlimited: there is nothing to warn about, and a percentage of
		// infinity is not a number.
		return Warnings{}
	}

	return Warnings{
		At80:  period.TokensUsed*100 >= period.Allowance()*WarningThreshold && period.Warning80SentAt == nil,
		At100: period.TokensUsed*100 >= period.Allowance()*ExhaustedThreshold && period.Warning100SentAt == nil,
	}
}
