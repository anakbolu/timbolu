/**
 * The rows the queries produce are turned into domain values here and nowhere
 * else, so a column added to the schema is a compile error rather than a field
 * a screen silently loses.
 *
 * Ported 1:1 from internal/modules/billing/repository/convert.go, with the row
 * shapes that `sqlcgen/models.go` generated.
 */

import type { Invoice, Plan, QuotaPeriod, Subscription, TopUpPackage } from "../domain/billing.ts";

/** `plans` as `SELECT *` returns it. */
export interface PlanRow {
  id: string;
  key: string;
  name: string;
  monthly_price_idr: number;
  token_quota: number;
  daily_cost_micros: number;
  period_days: number;
  max_agents: number;
  max_integrations: number;
  max_members: number;
  is_trial: boolean;
  sort_order: number;
  created_at: Date;
}

/** `subscriptions` as `SELECT *` returns it. */
export interface SubscriptionRow {
  id: string;
  workspace_id: string;
  plan_id: string;
  status: string;
  current_period_start: Date;
  current_period_end: Date;
  grace_until: Date | null;
  past_due_since: Date | null;
  pending_plan_id: string | null;
  past_due_notified_at: Date | null;
  read_only_notified_at: Date | null;
  canceled_at: Date | null;
  created_at: Date;
  updated_at: Date;
}

/** `quota_periods` as `SELECT *` returns it. */
export interface QuotaPeriodRow {
  id: string;
  workspace_id: string;
  period_start: Date;
  period_end: Date;
  tokens_granted: number;
  tokens_topped_up: number;
  tokens_used: number;
  warning_80_sent_at: Date | null;
  warning_100_sent_at: Date | null;
  created_at: Date;
  updated_at: Date;
}

/** `billing_invoices` as `SELECT *` returns it. */
export interface InvoiceRow {
  id: string;
  workspace_id: string;
  subscription_id: string | null;
  provider: string;
  provider_reference: string;
  amount_idr: number;
  token_amount: number;
  status: string;
  kind: string;
  period_start: Date;
  period_end: Date;
  payment_instructions: string;
  attempts: number;
  last_error: string;
  expires_at: Date | null;
  paid_at: Date | null;
  created_at: Date;
  updated_at: Date;
}

/** `topup_packages` as `SELECT *` returns it. */
export interface TopUpRow {
  id: string;
  key: string;
  name: string;
  token_amount: number;
  price_idr: number;
  sort_order: number;
  active: boolean;
  created_at: Date;
}

export function toPlan(row: PlanRow): Plan {
  return {
    id: row.id,
    key: row.key,
    name: row.name,
    monthlyPriceIDR: row.monthly_price_idr,
    tokenQuota: row.token_quota,
    dailyCostMicros: row.daily_cost_micros,
    periodDays: row.period_days,
    maxAgents: row.max_agents,
    maxIntegrations: row.max_integrations,
    maxMembers: row.max_members,
    isTrial: row.is_trial,
    sortOrder: row.sort_order,
  };
}

export function toSubscription(row: SubscriptionRow): Subscription {
  return {
    id: row.id,
    workspaceId: row.workspace_id,
    planId: row.plan_id,
    status: row.status,
    currentPeriodStart: row.current_period_start,
    currentPeriodEnd: row.current_period_end,
    graceUntil: row.grace_until,
    pastDueSince: row.past_due_since,
    // The pending plan is nullable, and a null stays the nil id rather than
    // becoming the zero uuid — which would name a package that is not there.
    pendingPlanId: row.pending_plan_id ?? "00000000-0000-0000-0000-000000000000",
    pastDueNotifiedAt: row.past_due_notified_at,
    readOnlyNotifiedAt: row.read_only_notified_at,
    canceledAt: row.canceled_at,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

export function toPeriod(row: QuotaPeriodRow): QuotaPeriod {
  return {
    id: row.id,
    workspaceId: row.workspace_id,
    periodStart: row.period_start,
    periodEnd: row.period_end,
    tokensGranted: row.tokens_granted,
    tokensToppedUp: row.tokens_topped_up,
    tokensUsed: row.tokens_used,
    warning80SentAt: row.warning_80_sent_at,
    warning100SentAt: row.warning_100_sent_at,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

export function toInvoice(row: InvoiceRow): Invoice {
  return {
    id: row.id,
    workspaceId: row.workspace_id,
    // The subscription is nullable on the row; a null stays the nil id rather
    // than becoming the zero uuid, which would name a subscription that is not there.
    subscriptionId: row.subscription_id ?? "00000000-0000-0000-0000-000000000000",
    kind: row.kind,
    provider: row.provider,
    providerReference: row.provider_reference,
    amountIDR: row.amount_idr,
    tokenAmount: row.token_amount,
    status: row.status,
    periodStart: row.period_start,
    periodEnd: row.period_end,
    instructions: row.payment_instructions,
    attempts: row.attempts,
    lastError: row.last_error,
    expiresAt: row.expires_at,
    paidAt: row.paid_at,
    createdAt: row.created_at,
  };
}

export function toTopUp(row: TopUpRow): TopUpPackage {
  return {
    id: row.id,
    key: row.key,
    name: row.name,
    tokenAmount: row.token_amount,
    priceIDR: row.price_idr,
    sortOrder: row.sort_order,
    active: row.active,
  };
}
