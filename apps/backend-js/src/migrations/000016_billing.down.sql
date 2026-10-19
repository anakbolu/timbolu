-- The reverse of 000016: the billing columns it added, the table it created, and
-- the two packages it seeded.
--
-- What survives is what the first migration created and nothing else. Subscriptions,
-- invoices, and quota periods are untouched: a deployment that rolls this back keeps
-- its workspaces, its allowances, and what it was charged. What it loses is the
-- ability to have sold a top-up or to name a daily ceiling, so a workspace sitting at
-- one is no longer bounded by it.

DROP TABLE IF EXISTS billing_payment_references;
DROP TABLE IF EXISTS billing_directory;
DROP TABLE IF EXISTS topup_packages;

ALTER TABLE quota_periods DROP COLUMN IF EXISTS tokens_topped_up;

DROP INDEX IF EXISTS billing_invoices_outstanding_idx;
DROP INDEX IF EXISTS billing_invoices_subscription_period_kind_key;

ALTER TABLE billing_invoices DROP COLUMN IF EXISTS expires_at;
ALTER TABLE billing_invoices DROP COLUMN IF EXISTS last_error;
ALTER TABLE billing_invoices DROP COLUMN IF EXISTS attempts;
ALTER TABLE billing_invoices DROP COLUMN IF EXISTS payment_instructions;
ALTER TABLE billing_invoices DROP COLUMN IF EXISTS token_amount;
ALTER TABLE billing_invoices DROP COLUMN IF EXISTS kind;

ALTER TABLE subscriptions DROP COLUMN IF EXISTS read_only_notified_at;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS past_due_notified_at;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS pending_plan_id;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS past_due_since;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS canceled_at;

ALTER TABLE plans DROP COLUMN IF EXISTS period_days;
ALTER TABLE plans DROP COLUMN IF EXISTS daily_cost_micros;

-- The seeded packages go with the columns that described them. A workspace
-- subscribed to one keeps its subscription row, which now points at a plan that is
-- no longer offered; the trial row the first migration seeded stays, so a fresh
-- database still has the package a person reaches by signing up.
DELETE FROM plans WHERE key IN ('keluarga', 'rombongan');
