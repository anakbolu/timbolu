-- EPIC 12 · What the subscription needs that the initial schema only reserved.
--
-- `plans`, `subscriptions`, `billing_invoices`, and `quota_periods` were created
-- with the first migration and seeded with the trial package; nothing wrote to
-- them until now. This migration adds the columns the product needs to actually
-- sell a package and to pace a workspace against it:
--
--   * a plan carries its own daily cost ceiling, so one runaway task cannot spend
--     a month in an afternoon inside a package that is otherwise generous;
--   * an invoice says what it is for — a month, or a top-up — and where to pay it;
--   * a quota period carries the top-up tokens beside the granted ones, because a
--     top-up is spent in the period it was bought in and does not roll over.
--
-- The three paid packages and the top-up packs are seeded here. They are product
-- decisions, and they are written down rather than left to a screen's placeholder:
-- a workspace that reaches its allowance has to be offered something concrete.

-- ---------------------------------------------------------------- plans

-- The day's cost ceiling of a package, in micro-rupiah. Zero means the package
-- does not bound a day, which is the honest default for the trial: a person
-- trying the product should be stopped by the package's tokens, not by a ceiling
-- they did not ask for.
ALTER TABLE plans ADD COLUMN daily_cost_micros BIGINT NOT NULL DEFAULT 0;

-- How many days the package runs for. It is a column rather than a constant
-- because the trial is short and a paid month is not, and "when does this period
-- end" has to be answerable from the plan that was sold.
ALTER TABLE plans ADD COLUMN period_days INT NOT NULL DEFAULT 30
    CONSTRAINT plans_period_days_valid CHECK (period_days BETWEEN 1 AND 366);

-- ---------------------------------------------------------------- subscriptions

-- The day the package was ended by a person rather than by a failed payment.
-- `canceled` rows stay for history and are outside the live-subscription unique
-- index, which is what lets a workspace subscribe again afterwards.
ALTER TABLE subscriptions ADD COLUMN canceled_at TIMESTAMPTZ;

-- The day the workspace was carrying a payment link that had not been paid yet.
-- It is recorded so "why is this invoice outstanding" is answerable from the
-- subscription rather than only from the invoice list.
ALTER TABLE subscriptions ADD COLUMN past_due_since TIMESTAMPTZ;

-- The package a workspace will move to when its current period ends. Nil means it
-- stays on the one it is on.
--
-- The change is deferred rather than immediate because a period was already paid
-- for: taking a bigger package mid-month would either hand out the bigger
-- allowance for free or charge a proration nobody asked for, and deferring it says
-- plainly which month each price belongs to.
ALTER TABLE subscriptions ADD COLUMN pending_plan_id UUID REFERENCES plans (id);

-- When each transition was reported to the workspace. They are what makes a
-- transition notice a once-per-period event: the state itself is derived from the
-- times, so without a marker the sweep would say "you are read-only" every time it
-- ran rather than on the day it became true.
ALTER TABLE subscriptions ADD COLUMN past_due_notified_at TIMESTAMPTZ;
ALTER TABLE subscriptions ADD COLUMN read_only_notified_at TIMESTAMPTZ;

-- ---------------------------------------------------------------- invoices

-- What one invoice is for. A month is created by the period rolling over; a top-up
-- is created when a workspace buys tokens inside a period it already has.
ALTER TABLE billing_invoices ADD COLUMN kind TEXT NOT NULL DEFAULT 'subscription'
    CONSTRAINT billing_invoices_kind_known CHECK (kind IN ('subscription', 'topup'));

-- The tokens a top-up invoice buys. A month's tokens are the plan's, so this is
-- zero for a subscription invoice — and that is deliberate rather than a missing
-- value: the plan is the allowance, and copying it here would be a second copy
-- that can drift.
ALTER TABLE billing_invoices ADD COLUMN token_amount BIGINT NOT NULL DEFAULT 0;

-- What a person acts on to pay: a URL when the provider gives one, or the virtual
-- account and its bank when it gives an account number instead. Empty when the
-- deployment has no provider, which is a real state — the invoice is still created
-- and the screen says to transfer and confirm.
ALTER TABLE billing_invoices ADD COLUMN payment_instructions TEXT NOT NULL DEFAULT '';

-- How many times the provider was asked to create the payment. It is what a retry
-- reads, so a provider that was unreachable at invoice time is tried again rather
-- than leaving a permanent dead link.
ALTER TABLE billing_invoices ADD COLUMN attempts INT NOT NULL DEFAULT 0;

-- Why creating the payment failed, in the provider's own words.
ALTER TABLE billing_invoices ADD COLUMN last_error TEXT NOT NULL DEFAULT '';

-- When the payment link stops being usable. The provider's own expiry is copied
-- here so the screen shows a date rather than recomputing one.
ALTER TABLE billing_invoices ADD COLUMN expires_at TIMESTAMPTZ;

-- One *outstanding* invoice per subscription per period.
--
-- This is the idempotency the webhook and the sweep both rest on: a period that
-- rolls over twice — a retried sweep, a double delivery — writes one row instead
-- of two, so a workspace cannot be charged twice for the same month.
--
-- It is partial on `pending` so that an invoice that expired is re-issuable: a
-- workspace whose payment window closed must be able to be sent a new one, and a
-- total index would refuse it a second row for a month it still owes. What was
-- expired stays as history, which is what makes "we asked twice" answerable.
-- Top-ups are outside it, because a workspace may buy several in one period.
CREATE UNIQUE INDEX billing_invoices_subscription_period_kind_key
    ON billing_invoices (subscription_id, period_start, kind)
    WHERE subscription_id IS NOT NULL AND kind = 'subscription' AND status = 'pending';

-- The outstanding invoices of one workspace, which is what the sweep reads to
-- expire them and the screen reads to show what is owed.
CREATE INDEX billing_invoices_outstanding_idx
    ON billing_invoices (workspace_id, created_at DESC)
    WHERE status = 'pending';

-- ---------------------------------------------------------------- quota periods

-- Tokens bought on top of the package, spent inside the period they were bought
-- in. They are a separate column from `tokens_granted` because they are a
-- different promise: the grant is the package, the top-up is a purchase that
-- expires with the period it was made in.
ALTER TABLE quota_periods ADD COLUMN tokens_topped_up BIGINT NOT NULL DEFAULT 0;

-- ---------------------------------------------------------------- top-up packs

-- What a workspace can buy when its allowance runs out.
--
-- It is a separate table from `plans` on purpose: a package is a recurring
-- commitment that also bounds Bolu, integrations, and members, and a top-up is a
-- one-off purchase of tokens that bounds nothing else. One table with a nullable
-- flag for each would make "which of these is a package" a question every query
-- has to ask.
CREATE TABLE topup_packages (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    key TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    token_amount BIGINT NOT NULL CONSTRAINT topup_packages_tokens_valid CHECK (token_amount > 0),
    price_idr BIGINT NOT NULL CONSTRAINT topup_packages_price_valid CHECK (price_idr > 0),
    sort_order INT NOT NULL DEFAULT 0,
    active BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------- sweep queue

-- Which workspaces the billing sweep still has to look at, and when.
--
-- It carries the workspace id and a moment and nothing else, for the reason
-- `whatsapp_directory` carries only a routing pair: the sweep runs on behalf of
-- the deployment rather than of a workspace, so it cannot open a tenant scope
-- until a row tells it which one — and every table it then reads is protected by
-- Row Level Security. The authoritative state stays in `subscriptions` and
-- `quota_periods`; this is a queue, not a copy.
--
-- It has no policy on purpose. A policy keyed on `app_current_workspace_id()`
-- would match nothing in a read that has no tenant, which is exactly the read this
-- exists for.
CREATE TABLE billing_directory (
    workspace_id UUID PRIMARY KEY REFERENCES workspaces (id) ON DELETE CASCADE,
    -- When this workspace is next due a pass. The sweep moves it forward after
    -- each pass, so a workspace that is up to date is not read again — and a pass
    -- that never ran is caught by the next one rather than lost.
    next_sync_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX billing_directory_due_idx ON billing_directory (next_sync_at);

-- ---------------------------------------------------------------- seeds

-- The three packages, in the order the pricing page shows them.
--
-- The trial is 14 days rather than a month: it is the package a person reaches by
-- signing up, and its point is to be long enough to see the product work and
-- short enough that a workspace which never pays is not served forever.
-- The trial's member cap is raised from the single seat the first migration seeded.
--
-- One seat is the owner, so a workspace on its trial could never invite anybody — and
-- every new workspace is on its trial, which would have made the invitation flow dead
-- until somebody paid. A package whose point is to be tried has to be usable by the team
-- that would try it.
UPDATE plans SET period_days = 14, daily_cost_micros = 20000000, max_members = 3 WHERE key = 'coba';

INSERT INTO plans (key, name, monthly_price_idr, token_quota, max_agents, max_integrations, max_members, is_trial, sort_order, daily_cost_micros, period_days)
VALUES
    ('keluarga', 'Paket Keluarga', 149000, 2000000, 6, 3, 6, false, 2, 60000000, 30),
    ('rombongan', 'Paket Rombongan', 449000, 8000000, 20, 10, 25, false, 3, 200000000, 30)
ON CONFLICT (key) DO NOTHING;

-- Top-up packs. They cost more per token than a package does, which is the point:
-- a top-up is what a workspace buys when it misjudged the month, and pricing it
-- below the package would make the package pointless.
INSERT INTO topup_packages (key, name, token_amount, price_idr, sort_order)
VALUES
    ('tambahan-200k', 'Tambahan 200 ribu token', 200000, 25000, 1),
    ('tambahan-1jt', 'Tambahan 1 juta token', 1000000, 99000, 2)
ON CONFLICT (key) DO NOTHING;

-- ------------------------------------------------- payment routing

-- Where a payment notification finds its workspace.
--
-- A provider names one reference and nothing else: it does not know which workspace it
-- is talking about, and `billing_invoices` is protected by Row Level Security, so a read
-- with no tenant returns nothing. This table is the routing row that answers the
-- question, and it carries the two ids and nothing else — no amount, no status, no
-- token count — for the reason the fleet directory carries only a routing pair: it is a
-- signpost, and everything it points at is read inside the tenant's own scope.
--
-- It has no policy on purpose: a policy keyed on `app_current_workspace_id()` would
-- match nothing in the read that exists to learn which workspace to open.
CREATE TABLE billing_payment_references (
    provider     TEXT NOT NULL,
    reference    TEXT NOT NULL,
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    invoice_id   UUID NOT NULL REFERENCES billing_invoices (id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, reference)
);

CREATE INDEX billing_payment_references_invoice_idx ON billing_payment_references (invoice_id);

-- ---------------------------------------------------------------- backfill

-- Every workspace that already exists gets the package a new one would.
--
-- The alternative was to heal a missing subscription lazily on the first billing
-- read, and it is worse: "exactly one live subscription" would then be a rule the
-- product keeps rather than one the data has, and every read that forgot to heal
-- would answer with a workspace that has no allowance at all.
INSERT INTO subscriptions (workspace_id, plan_id, status, current_period_start, current_period_end, grace_until)
SELECT w.id,
       p.id,
       'trial',
       now(),
       now() + make_interval(days => p.period_days),
       now() + make_interval(days => p.period_days) + make_interval(days => 7)
FROM workspaces w
CROSS JOIN plans p
WHERE p.is_trial
  AND NOT EXISTS (SELECT 1 FROM subscriptions s WHERE s.workspace_id = w.id);

-- And the quota period each of those subscriptions grants, so the first check
-- reads a row rather than creating one.
INSERT INTO quota_periods (workspace_id, period_start, period_end, tokens_granted)
SELECT s.workspace_id, s.current_period_start, s.current_period_end, p.token_quota
FROM subscriptions s
JOIN plans p ON p.id = s.plan_id
ON CONFLICT (workspace_id, period_start) DO NOTHING;

-- And the sweep queue, so an existing workspace is reconciled like a new one.
INSERT INTO billing_directory (workspace_id)
SELECT id FROM workspaces
ON CONFLICT (workspace_id) DO NOTHING;

-- ------------------------------------------------------------- row level security

-- `topup_packages` is a catalogue read by every workspace, so it carries no
-- tenant column and no policy: unlike `plans`, which is also shared, the row a
-- workspace buys is recorded in its own invoice rather than on the package.
