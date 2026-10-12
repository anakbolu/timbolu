-- Billing queries: the packages sold, the subscription a workspace is on, the
-- invoices it owes, the periods its allowance is counted over, and the packs it
-- may buy on top.
--
-- Two of these are deliberately unscoped — the directory queue the sweep reads and
-- the invoice a webhook names — and both say why where they are written. Every
-- other statement carries `workspace_id`, so Row Level Security and the filter
-- agree rather than one depending on the other.

-- ---------------------------------------------------------------- packages

-- name: ListPlans :many
SELECT * FROM plans ORDER BY sort_order, created_at;

-- name: GetPlanByKey :one
SELECT * FROM plans WHERE key = $1;

-- name: GetTrialPlan :one
SELECT * FROM plans WHERE is_trial ORDER BY sort_order LIMIT 1;

-- ---------------------------------------------------------------- subscription

-- name: GetLiveSubscription :one
SELECT * FROM subscriptions
WHERE workspace_id = sqlc.arg('workspace_id') AND status <> 'canceled'
ORDER BY created_at DESC
LIMIT 1;

-- name: GetSubscriptionByID :one
SELECT * FROM subscriptions
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id');

-- name: CreateSubscription :one
-- The live-subscription unique index is what makes this safe to call from the
-- onboarding transaction and from nothing else: a second live row for one
-- workspace is refused by the database rather than by the caller remembering.
INSERT INTO subscriptions (workspace_id, plan_id, status, current_period_start, current_period_end, grace_until)
VALUES (
    sqlc.arg('workspace_id'),
    sqlc.arg('plan_id'),
    sqlc.arg('status'),
    sqlc.arg('current_period_start'),
    sqlc.arg('current_period_end'),
    sqlc.arg('grace_until')
)
RETURNING *;

-- name: UpdateSubscription :one
UPDATE subscriptions
SET plan_id = sqlc.arg('plan_id'),
    status = sqlc.arg('status'),
    current_period_start = sqlc.arg('current_period_start'),
    current_period_end = sqlc.arg('current_period_end'),
    grace_until = sqlc.arg('grace_until'),
    past_due_since = sqlc.arg('past_due_since'),
    pending_plan_id = sqlc.arg('pending_plan_id'),
    past_due_notified_at = sqlc.arg('past_due_notified_at'),
    read_only_notified_at = sqlc.arg('read_only_notified_at'),
    canceled_at = sqlc.arg('canceled_at'),
    updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
RETURNING *;

-- ---------------------------------------------------------------- quota periods

-- name: GetQuotaPeriodCovering :one
-- The period that covers a moment. A period is bounded by its start rather than
-- by a range, because the row's own key is (workspace, start) and a range
-- comparison would make two rows able to cover the same instant.
SELECT * FROM quota_periods
WHERE workspace_id = sqlc.arg('workspace_id') AND period_start = sqlc.arg('period_start');

-- name: ListQuotaPeriods :many
SELECT * FROM quota_periods
WHERE workspace_id = sqlc.arg('workspace_id')
ORDER BY period_start DESC
LIMIT sqlc.arg('limit_count');

-- name: CreateQuotaPeriod :one
INSERT INTO quota_periods (workspace_id, period_start, period_end, tokens_granted, tokens_topped_up, tokens_used)
VALUES (
    sqlc.arg('workspace_id'),
    sqlc.arg('period_start'),
    sqlc.arg('period_end'),
    sqlc.arg('tokens_granted'),
    sqlc.arg('tokens_topped_up'),
    sqlc.arg('tokens_used')
)
ON CONFLICT (workspace_id, period_start) DO UPDATE
SET period_end = EXCLUDED.period_end,
    updated_at = now()
RETURNING *;

-- name: UpdateQuotaPeriod :one
-- The warnings are written as a fixed value rather than as a coalesce, because
-- "already sent" has to survive a pass that decided not to send: a coalesce would
-- make it impossible to ever clear one.
UPDATE quota_periods
SET tokens_used = sqlc.arg('tokens_used'),
    tokens_topped_up = sqlc.arg('tokens_topped_up'),
    warning_80_sent_at = sqlc.arg('warning_80_sent_at'),
    warning_100_sent_at = sqlc.arg('warning_100_sent_at'),
    updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
RETURNING *;

-- name: AddQuotaTopUp :one
-- A top-up lands on the period that covers the moment it was confirmed. It is an
-- increment rather than a write of a total, so two purchases confirmed at once
-- add up instead of one overwriting the other.
UPDATE quota_periods
SET tokens_topped_up = tokens_topped_up + sqlc.arg('tokens'),
    updated_at = now()
WHERE workspace_id = sqlc.arg('workspace_id') AND period_start = sqlc.arg('period_start')
RETURNING *;

-- ---------------------------------------------------------------- invoices

-- name: GetInvoice :one
SELECT * FROM billing_invoices
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id');

-- name: GetPaymentRoute :one
-- The one read a payment notification can make: the routing row that says whose invoice
-- the provider named. It carries the two ids and nothing else.
SELECT workspace_id, invoice_id FROM billing_payment_references
WHERE provider = sqlc.arg('provider') AND reference = sqlc.arg('reference');

-- name: RememberPaymentRoute :exec
INSERT INTO billing_payment_references (provider, reference, workspace_id, invoice_id)
VALUES (sqlc.arg('provider'), sqlc.arg('reference'), sqlc.arg('workspace_id'), sqlc.arg('invoice_id'))
ON CONFLICT (provider, reference) DO NOTHING;

-- name: GetActiveInvoiceForPeriod :one
-- An invoice that still stands for a period: one waiting to be paid, or one that
-- was paid. An expired or failed one is deliberately outside this, which is what
-- makes a month still owed re-issuable.
SELECT * FROM billing_invoices
WHERE workspace_id = sqlc.arg('workspace_id')
  AND subscription_id = sqlc.arg('subscription_id')
  AND kind = sqlc.arg('kind')
  AND period_start = sqlc.arg('period_start')
  AND status IN ('pending', 'paid');

-- name: ListInvoices :many
SELECT * FROM billing_invoices
WHERE workspace_id = sqlc.arg('workspace_id')
  AND (sqlc.arg('status')::text = '' OR status = sqlc.arg('status'))
ORDER BY created_at DESC
LIMIT sqlc.arg('limit_count');

-- name: ListOutstandingInvoices :many
SELECT * FROM billing_invoices
WHERE workspace_id = sqlc.arg('workspace_id') AND status = 'pending'
ORDER BY period_start, created_at;

-- name: CreateInvoice :one
-- `ON CONFLICT DO NOTHING` rather than letting the unique index raise: a conflict means
-- another writer created this period's invoice a moment ago, and that is an answer to
-- read rather than a failure. It also matters that the statement cannot raise — a failed
-- statement aborts the transaction, and the read that recovers the answer would then be
-- refused too.
INSERT INTO billing_invoices (
    workspace_id, subscription_id, provider, provider_reference, amount_idr,
    token_amount, status, kind, period_start, period_end, payment_instructions, expires_at
)
VALUES (
    sqlc.arg('workspace_id'),
    sqlc.arg('subscription_id'),
    sqlc.arg('provider'),
    sqlc.arg('provider_reference'),
    sqlc.arg('amount_idr'),
    sqlc.arg('token_amount'),
    sqlc.arg('status'),
    sqlc.arg('kind'),
    sqlc.arg('period_start'),
    sqlc.arg('period_end'),
    sqlc.arg('payment_instructions'),
    sqlc.arg('expires_at')
)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: SetInvoiceSettlement :one
-- The outcome is written once. The `status = 'pending'` guard is the whole
-- idempotency of a replayed webhook: a second delivery updates nothing and the
-- caller reads zero rows, so a period cannot be extended twice for one payment.
UPDATE billing_invoices
SET status = sqlc.arg('status'),
    paid_at = sqlc.arg('paid_at'),
    updated_at = now()
WHERE id = sqlc.arg('id')
  AND workspace_id = sqlc.arg('workspace_id')
  AND status = 'pending'
RETURNING *;

-- name: SetInvoicePayment :one
-- The routing row is written in the same statement's transaction by the repository, so a
-- payment link that exists is a payment link the notification can find.
UPDATE billing_invoices
SET provider = sqlc.arg('provider'),
    provider_reference = sqlc.arg('provider_reference'),
    payment_instructions = sqlc.arg('payment_instructions'),
    expires_at = sqlc.arg('expires_at'),
    attempts = sqlc.arg('attempts'),
    last_error = sqlc.arg('last_error'),
    updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
RETURNING *;

-- name: ExpireInvoice :one
UPDATE billing_invoices
SET status = 'expired', updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id') AND status = 'pending'
RETURNING *;

-- ---------------------------------------------------------------- top-ups

-- name: ListTopUpPackages :many
SELECT * FROM topup_packages WHERE active ORDER BY sort_order;

-- name: GetTopUpPackage :one
SELECT * FROM topup_packages WHERE key = $1 AND active;

-- ---------------------------------------------------------------- directory

-- name: ListDueBillingWorkspaces :many
-- The one read with no tenant scope, and it carries the workspace id and a moment
-- and nothing else. The sweep cannot open a scope until a row tells it which one.
SELECT workspace_id FROM billing_directory
WHERE next_sync_at <= sqlc.arg('now')
ORDER BY next_sync_at
LIMIT sqlc.arg('limit_count');

-- name: TouchBillingDirectory :exec
UPDATE billing_directory
SET next_sync_at = sqlc.arg('next_sync_at'), updated_at = now()
WHERE workspace_id = sqlc.arg('workspace_id');

-- name: EnsureBillingDirectory :exec
INSERT INTO billing_directory (workspace_id, next_sync_at)
VALUES (sqlc.arg('workspace_id'), now())
ON CONFLICT (workspace_id) DO NOTHING;

-- name: GetPlanByID :one
SELECT * FROM plans WHERE id = $1;
