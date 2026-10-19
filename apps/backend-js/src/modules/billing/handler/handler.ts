/**
 * The billing endpoints. It talks to the domain service interface only.
 *
 * Ported 1:1 from internal/modules/billing/handler/handler.go.
 */

import type { Hono } from "hono";
import type { AppContext, AppEnv } from "../../../platform/context.ts";
import { currentUserId, isUuid, workspaceUUID } from "../../../platform/context.ts";
import { nopLogger, type Logger } from "../../../platform/logger.ts";
import { created, error, ok, okWithMeta } from "../../../platform/response.ts";
import {
  ErrInvalidInput,
  ErrInvoiceNotFound,
  ErrInvoiceSettled,
  ErrNoGateway,
  ErrNotFound,
  ErrPlanNotFound,
  ErrSubscriptionNotFound,
  ErrTopUpNotFound,
  ErrUnverifiable,
  invoiceOutstanding,
  is,
  periodRemaining,
  periodUsed,
  userMessage,
  type Invoice,
  type Plan,
  type Scope,
  type Snapshot,
  type Subscription,
  type TopUpPackage,
} from "../domain/billing.ts";
import type { Delivery, Service } from "../domain/service.ts";

/** Serves the billing endpoints. */
export class Handler {
  readonly service: Service;
  readonly log: Logger;

  constructor(service: Service, log?: Logger | null) {
    this.service = service;
    this.log = log ?? nopLogger;
  }

  /** Returns the workspace's package, its usage, and what it owes. */
  snapshot = async (c: AppContext): Promise<Response> => {
    try {
      const snapshot = await this.service.snapshot(scope(c));
      return ok(c, "ok", wireSnapshot(snapshot));
    } catch (err) {
      return this.fail(c, "read subscription", err);
    }
  };

  /** Returns the packages a workspace may move to. */
  plans = async (c: AppContext): Promise<Response> => {
    try {
      const plans = await this.service.plans(scope(c));
      const payload = plans.map(wirePlan);
      return okWithMeta(c, "ok", payload, { count: payload.length });
    } catch (err) {
      return this.fail(c, "list plans", err);
    }
  };

  /** Moves the workspace to another package. */
  change = async (c: AppContext): Promise<Response> => {
    const body = await readBody(c);
    if (!body) {
      return error(c, 400, "the body is not valid JSON", "invalid_request");
    }

    try {
      const snapshot = await this.service.change(scope(c), {
        planKey: typeof body.plan_key === "string" ? body.plan_key : "",
      });
      return ok(c, "plan changed", wireSnapshot(snapshot));
    } catch (err) {
      return this.fail(c, "change plan", err);
    }
  };

  /** Lists what the workspace was charged. */
  invoices = async (c: AppContext): Promise<Response> => {
    try {
      const invoices = await this.service.invoices(scope(c), {
        status: (c.req.query("status") ?? "").trim(),
        limit: queryInt(c, "limit", 0),
      });
      const payload = invoices.map(wireInvoice);
      return okWithMeta(c, "ok", payload, { count: payload.length });
    } catch (err) {
      return this.fail(c, "list invoices", err);
    }
  };

  /** Asks for a payment link for one invoice. */
  pay = async (c: AppContext): Promise<Response> => {
    const invoiceId = c.req.param("invoiceID") ?? "";
    if (!isUuid(invoiceId)) {
      return error(c, 400, "invoice id is not a uuid", "invalid_input");
    }

    try {
      const invoice = await this.service.pay(scope(c), invoiceId);
      return ok(c, "invoice payment", wireInvoice(invoice));
    } catch (err) {
      return this.fail(c, "pay invoice", err);
    }
  };

  /** Buys tokens inside the current period. */
  buyTopUp = async (c: AppContext): Promise<Response> => {
    const body = await readBody(c);
    if (!body) {
      return error(c, 400, "the body is not valid JSON", "invalid_request");
    }

    try {
      const invoice = await this.service.buyTopUp(scope(c), {
        packageKey: typeof body.package_key === "string" ? body.package_key : "",
      });
      return created(c, "top-up invoice", wireInvoice(invoice));
    } catch (err) {
      return this.fail(c, "buy top-up", err);
    }
  };

  /**
   * Applies one payment notification.
   *
   * It answers 200 for anything the provider should not retry — a refusal it
   * cannot fix by sending again — and 4xx for what it can.
   */
  webhook = async (c: AppContext): Promise<Response> => {
    const body = new Uint8Array(await c.req.arrayBuffer());

    const headers: Record<string, string> = {};
    c.req.raw.headers.forEach((value, key) => {
      headers[key] = value;
    });

    const delivery: Delivery = {
      provider: (c.req.param("provider") ?? "").trim(),
      body,
      headers,
    };

    try {
      await this.service.handlePayment(delivery);
    } catch (err) {
      return this.fail(c, "payment webhook", err);
    }

    return ok(c, "payment accepted", null);
  };

  /** Maps the module's errors onto responses. */
  fail(c: AppContext, operation: string, err: unknown): Response {
    if (
      is(err, ErrNotFound) ||
      is(err, ErrInvoiceNotFound) ||
      is(err, ErrSubscriptionNotFound) ||
      is(err, ErrPlanNotFound) ||
      is(err, ErrTopUpNotFound)
    ) {
      return error(c, 404, userMessage(err, ErrNotFound), "billing_not_found");
    }
    if (is(err, ErrNoGateway)) {
      return error(
        c,
        503,
        "pembayaran online belum dikonfigurasi di deployment ini",
        "billing_unavailable",
      );
    }
    if (is(err, ErrInvoiceSettled)) {
      return error(c, 409, userMessage(err, ErrInvoiceSettled), "invoice_settled");
    }
    if (is(err, ErrUnverifiable)) {
      // 400 rather than 500: the payload is wrong and retrying it cannot help.
      return error(c, 400, "pemberitahuan pembayaran tidak bisa diverifikasi", "unverifiable_payment");
    }
    if (is(err, ErrInvalidInput)) {
      return error(c, 400, userMessage(err, ErrInvalidInput), "invalid_input");
    }

    // A failure nobody classified is the deployment's business, so it is logged
    // with its own words and answered with one sentence.
    this.log.error({ operation, err }, "a billing request failed");
    return error(c, 500, "internal error", "internal_error");
  }
}

/** Registers the subscription endpoints. */
export function routes(group: Hono<AppEnv>, h: Handler): void {
  group.get("/billing/subscription", h.snapshot);
  group.get("/billing/plans", h.plans);
  group.post("/billing/subscription", h.change);
  group.get("/billing/invoices", h.invoices);
  group.post("/billing/invoices/:invoiceID/pay", h.pay);
  group.post("/billing/topups", h.buyTopUp);
}

/**
 * Registers a provider's payment notification, on the router a provider can
 * reach without a session. What makes it safe is the adapter's own signature
 * check, which is the only thing standing between a stranger and a paid package.
 */
export function webhookRoutes(group: Hono<AppEnv>, h: Handler): void {
  group.post("/billing/webhooks/:provider", h.webhook);
}

/** The tenant the request runs in, put there by the tenant middleware. */
function scope(c: AppContext): Scope {
  return {
    userId: currentUserId(c) ?? "",
    workspaceId: workspaceUUID(c) ?? "",
  };
}

/** `c.QueryInt` with Go's default: a missing or malformed value answers the default. */
function queryInt(c: AppContext, key: string, fallback: number): number {
  const raw = c.req.query(key);
  if (raw === undefined) return fallback;
  const parsed = Number.parseInt(raw, 10);
  return Number.isFinite(parsed) ? parsed : fallback;
}

/** `c.BodyParser` for a JSON object; null when the body is not one. */
async function readBody(c: AppContext): Promise<Record<string, unknown> | null> {
  const body: unknown = await c.req.json().catch(() => null);
  if (body === null || typeof body !== "object" || Array.isArray(body)) return null;
  return body as Record<string, unknown>;
}

// ---------------------------------------------------------------- wire types

interface PlanPayload {
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
}

interface InvoicePayload {
  id: string;
  kind: string;
  status: string;
  amount_idr: number;
  token_amount?: number;
  period_start: string;
  period_end: string;
  payment_instructions?: string;
  provider?: string;
  attempts: number;
  last_error?: string;
  expires_at?: string;
  paid_at?: string;
  created_at: string;
  outstanding: boolean;
}

interface SubscriptionPayload {
  id: string;
  plan_key: string;
  plan_name: string;
  pending_plan_key?: string;
  pending_plan_name?: string;
  status: string;
  entitlement: string;
  current_period_start: string;
  current_period_end: string;
  grace_until?: string;
  grace_days: number;
  read_only: boolean;
  tokens_granted: number;
  tokens_topped_up: number;
  tokens_used: number;
  tokens_remaining: number;
  used_percent: number;
  daily_cost_micros: number;
  spent_today_micros: number;
  outstanding_idr: number;
  max_agents: number;
  max_integrations: number;
  max_members: number;
}

interface SnapshotPayload {
  plan: PlanPayload;
  subscription: SubscriptionPayload;
  invoices: InvoicePayload[];
  outstanding: InvoicePayload[];
  top_ups: { key: string; name: string; token_amount: number; price_idr: number }[];
  entitlement: string;
  read_only: boolean;
  grace_days: number;
}

function wirePlan(plan: Plan): PlanPayload {
  return {
    key: plan.key,
    name: plan.name,
    monthly_price_idr: plan.monthlyPriceIDR,
    token_quota: plan.tokenQuota,
    daily_cost_micros: plan.dailyCostMicros,
    period_days: plan.periodDays,
    max_agents: plan.maxAgents,
    max_integrations: plan.maxIntegrations,
    max_members: plan.maxMembers,
    is_trial: plan.isTrial,
  };
}

function wireInvoice(invoice: Invoice): InvoicePayload {
  const payload: InvoicePayload = {
    id: invoice.id,
    kind: invoice.kind,
    status: invoice.status,
    amount_idr: invoice.amountIDR,
    period_start: timestamp(invoice.periodStart),
    period_end: timestamp(invoice.periodEnd),
    attempts: invoice.attempts,
    created_at: timestamp(invoice.createdAt),
    outstanding: invoiceOutstanding(invoice),
  };
  // Go's `omitempty`: a zero value is not sent.
  if (invoice.tokenAmount !== 0) payload.token_amount = invoice.tokenAmount;
  if (invoice.instructions !== "") payload.payment_instructions = invoice.instructions;
  if (invoice.provider !== "") payload.provider = invoice.provider;
  if (invoice.lastError !== "") payload.last_error = invoice.lastError;
  if (invoice.expiresAt) payload.expires_at = timestamp(invoice.expiresAt);
  if (invoice.paidAt) payload.paid_at = timestamp(invoice.paidAt);
  return payload;
}

function wireSubscription(snapshot: Snapshot): SubscriptionPayload {
  const s: Subscription = snapshot.subscription;
  const payload: SubscriptionPayload = {
    id: s.id,
    plan_key: snapshot.plan.key,
    plan_name: snapshot.plan.name,
    status: s.status,
    entitlement: snapshot.entitlement,
    current_period_start: timestamp(s.currentPeriodStart),
    current_period_end: timestamp(s.currentPeriodEnd),
    grace_days: snapshot.graceDays,
    read_only: snapshot.readOnly,
    tokens_granted: snapshot.period.tokensGranted,
    tokens_topped_up: snapshot.period.tokensToppedUp,
    tokens_used: snapshot.period.tokensUsed,
    tokens_remaining: periodRemaining(snapshot.period),
    used_percent: periodUsed(snapshot.period),
    daily_cost_micros: snapshot.plan.dailyCostMicros,
    spent_today_micros: snapshot.usageToday.costMicros,
    outstanding_idr: owedIdr(snapshot.outstanding),
    max_agents: snapshot.limits.maxAgents,
    max_integrations: snapshot.limits.maxIntegrations,
    max_members: snapshot.limits.maxMembers,
  };
  if (snapshot.pendingPlanKey !== "") payload.pending_plan_key = snapshot.pendingPlanKey;
  if (snapshot.pendingPlanName !== "") payload.pending_plan_name = snapshot.pendingPlanName;
  if (s.graceUntil) payload.grace_until = timestamp(s.graceUntil);
  return payload;
}

function wireSnapshot(snapshot: Snapshot): SnapshotPayload {
  return {
    plan: wirePlan(snapshot.plan),
    subscription: wireSubscription(snapshot),
    invoices: snapshot.invoices.map(wireInvoice),
    outstanding: snapshot.outstanding.map(wireInvoice),
    top_ups: snapshot.topUps.map(wireTopUp),
    entitlement: snapshot.entitlement,
    read_only: snapshot.readOnly,
    grace_days: snapshot.graceDays,
  };
}

function wireTopUp(pack: TopUpPackage): {
  key: string;
  name: string;
  token_amount: number;
  price_idr: number;
} {
  return {
    key: pack.key,
    name: pack.name,
    token_amount: pack.tokenAmount,
    price_idr: pack.priceIDR,
  };
}

/** What the outstanding invoices add up to, which the screen shows beside "bayar". */
function owedIdr(invoices: Invoice[]): number {
  let total = 0;
  for (const invoice of invoices) {
    total += invoice.amountIDR;
  }
  return total;
}

/**
 * Renders a moment as RFC 3339 in UTC, which is what a client parses. A zero
 * time renders as the empty string, because a zero time is not a date.
 */
function timestamp(at: Date): string {
  if (!at || Number.isNaN(at.getTime()) || at.getTime() === 0) return "";
  return at.toISOString();
}
