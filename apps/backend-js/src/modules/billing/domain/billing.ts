/**
 * The billing contract: the package a workspace is on, the quota that package
 * grants, the invoices it owes, and the tokens it bought on top.
 *
 * Two rules the whole package exists for:
 *
 *   - a workspace always has exactly one live subscription. Not "usually", and
 *     not "one per plan": a workspace with two live packages would have two
 *     answers to "what may this workspace spend", and the quota check would
 *     pick whichever it read first.
 *   - what a workspace may spend is derived, never remembered. The package's
 *     tokens plus what it bought, minus what the ledger says it used. A stored
 *     "remaining" is a number that can be wrong, and a wrong allowance is either
 *     a workspace billed for tokens it did not use or one served for free.
 *
 * Ported 1:1 from internal/modules/billing/domain/billing.go.
 */

/** The zero uuid, as Go's `uuid.Nil`. */
export const NilUUID = "00000000-0000-0000-0000-000000000000";

/**
 * The tenant identity a query runs as. Row Level Security reads both parts:
 * `workspaceId` is the active tenant and `userId` is the authenticated user.
 */
export interface Scope {
  userId?: string | null;
  workspaceId: string;
}

/** Reports whether the scope carries no workspace, which every read here requires. */
export function isZeroScope(scope: Scope): boolean {
  return !scope.workspaceId || scope.workspaceId === NilUUID;
}

// ---------------------------------------------------------------- constants

/** The package a workspace reaches by signing up. It still counts as live. */
export const StatusTrial = "trial";
/** A paid package inside its period. */
export const StatusActive = "active";
/** A package whose period ended and whose invoice is unpaid; inside the grace window. */
export const StatusPastDue = "past_due";
/** A package a person ended. Outside the live subscription's unique index. */
export const StatusCanceled = "canceled";

/** A month of a package. */
export const KindSubscription = "subscription";
/** A one-off purchase of tokens inside a period. */
export const KindTopup = "topup";

export const StatusPending = "pending";
export const StatusPaid = "paid";
export const StatusFailed = "failed";
export const StatusExpired = "expired";

/** The workspace may spend and may write. */
export const EntitlementActive = "active";
/** The period ended and the invoice is unpaid, and the workspace is inside its grace. */
export const EntitlementGrace = "grace";
/** The grace window is over: reads and exports keep working, writes are refused. */
export const EntitlementReadOnly = "read_only";

/** How long a workspace keeps working after its period ends. */
export const DefaultGraceDays = 7;

/** The two points at which a person can still act. */
export const WarningThreshold = 80;
export const ExhaustedThreshold = 100;

/** The page a client gets when it does not ask for one. */
export const DefaultListLimit = 50;

/** Bounds one page. */
export const MaxListLimit = 200;

// ---------------------------------------------------------------- errors

/** A row the caller named that is not there, or is not theirs. */
export const ErrNotFound = new Error("billing: not found");
export const ErrPlanNotFound = new Error("billing: plan not found");
export const ErrSubscriptionNotFound = new Error("billing: subscription not found");
export const ErrInvoiceNotFound = new Error("billing: invoice not found");
export const ErrTopUpNotFound = new Error("billing: top-up package not found");
export const ErrInvalidInput = new Error("billing: invalid input");
/** The allowance being spent. Its message names both numbers. */
export const ErrQuotaExceeded = new Error("kuota token habis");
/** A workspace past its grace window. */
export const ErrReadOnly = new Error("langganan workspace sudah lewat masa tenggang");
/** A workspace asked for a package while it is already on one. */
export const ErrAlreadySubscribed = new Error("billing: the workspace is already subscribed");
/** No payment provider is configured, which is a deployment fact. */
export const ErrNoGateway = new Error("billing: no payment gateway is configured");
/** A payment notification that cannot be trusted. */
export const ErrUnverifiable = new Error("billing: the payment notification cannot be verified");
/** A payment arrived for an invoice that already has an outcome. */
export const ErrInvoiceSettled = new Error("billing: the invoice has already been settled");

/** An error carrying the sentinel it was wrapped from, the way Go's `%w` does. */
export class WrappedError extends Error {
  readonly sentinel: Error;

  constructor(sentinel: Error, message: string) {
    super(message);
    this.name = "WrappedError";
    this.sentinel = sentinel;
  }
}

/** Wraps `sentinel` with a detail, matching `fmt.Errorf("%w: %s", ...)`. */
export function wrap(sentinel: Error, detail: string): WrappedError {
  return new WrappedError(sentinel, `${sentinel.message}: ${detail}`);
}

/** Reports whether `err` is `sentinel` or wraps it, matching `errors.Is`. */
export function is(err: unknown, sentinel: Error): boolean {
  let current: unknown = err;
  for (let guard = 0; guard < 32; guard++) {
    if (current === sentinel) return true;
    if (!(current instanceof Error)) return false;
    if (current instanceof WrappedError) {
      current = current.sentinel;
      continue;
    }
    current = "cause" in current ? current.cause : undefined;
  }
  return false;
}

/**
 * Strips this module's own prefix from a refusal, so what a person reads is
 * about what they asked for rather than about which package noticed.
 */
export function userMessage(err: unknown, prefix: Error): string {
  const raw = err instanceof Error ? err.message : String(err);
  let message = raw.startsWith(prefix.message) ? raw.slice(prefix.message.length) : raw;
  if (message.startsWith(":")) message = message.slice(1);
  message = message.trim();
  return message === "" ? prefix.message : message;
}

// ---------------------------------------------------------------- time

/**
 * Go's zero `time.Time` has no `Date` equivalent, so an epoch `Date` is the
 * sentinel. Every stored column this module reads is a real moment, but a
 * subscription read from a partly-written row can still carry one.
 */
export function isZeroTime(at: Date | null | undefined): boolean {
  return !at || Number.isNaN(at.getTime()) || at.getTime() === 0;
}

/** `t.AddDate(0, 0, days)`, in UTC. */
export function addDays(at: Date, days: number): Date {
  const out = new Date(at.getTime());
  out.setUTCDate(out.getUTCDate() + days);
  return out;
}

// ---------------------------------------------------------------- types

/** A package: what it costs, what it grants, and what it bounds. */
export interface Plan {
  id: string;
  key: string;
  name: string;
  /** What one period costs, in rupiah. */
  monthlyPriceIdr: number;
  /** The tokens one period grants. Zero means unlimited. */
  tokenQuota: number;
  /** One day's spend ceiling, in micro-rupiah. Zero means the package does not bound a day. */
  dailyCostMicros: number;
  /** How long one period runs. */
  periodDays: number;
  /** The caps the package enforces. Zero means unlimited. */
  maxAgents: number;
  maxIntegrations: number;
  maxMembers: number;
  isTrial: boolean;
  sortOrder: number;
}

/** One workspace's live package. */
export interface Subscription {
  id: string;
  workspaceId: string;
  planId: string;
  status: string;
  /** The period the workspace is in; the allowance is granted per period. */
  currentPeriodStart: Date;
  currentPeriodEnd: Date;
  /** The last moment the workspace keeps working after its period ends. */
  graceUntil: Date | null;
  /** When the outstanding invoice was created. Null means nothing is owed. */
  pastDueSince: Date | null;
  /** The package the workspace moves to when its current period ends. */
  pendingPlanId: string;
  /** When each transition was reported, which makes a notice once-per-period. */
  pastDueNotifiedAt: Date | null;
  readOnlyNotifiedAt: Date | null;
  canceledAt: Date | null;
  createdAt: Date;
  updatedAt: Date;
}

/**
 * What the subscription allows right now.
 *
 * The order is the whole rule: inside its period works; past it works while its
 * grace lasts; past both is read-only. A canceled subscription is read-only
 * immediately, because a person who ended the package did not ask for a grace
 * window.
 */
export function entitlement(s: Subscription, now: Date, graceMs: number): string {
  if (s.status === StatusCanceled) {
    return EntitlementReadOnly;
  }
  if (isZeroTime(s.currentPeriodEnd) || now.getTime() < s.currentPeriodEnd.getTime()) {
    return EntitlementActive;
  }
  let grace = graceMs;
  if (grace < 0) grace = 0;
  let until = new Date(s.currentPeriodEnd.getTime() + grace);
  if (s.graceUntil && s.graceUntil.getTime() > until.getTime()) {
    until = s.graceUntil;
  }
  if (now.getTime() < until.getTime()) {
    return EntitlementGrace;
  }
  return EntitlementReadOnly;
}

/** Where a provider's reference points: the workspace whose invoice it is, and the invoice. */
export interface PaymentRoute {
  workspaceId: string;
  invoiceId: string;
}

/** One amount a workspace owes, or owed. */
export interface Invoice {
  id: string;
  workspaceId: string;
  subscriptionId: string;
  kind: string;
  provider: string;
  /** The provider's own id for the payment, which is what the webhook names. */
  providerReference: string;
  amountIdr: number;
  /** What a top-up buys. Zero on a month. */
  tokenAmount: number;
  status: string;
  periodStart: Date;
  periodEnd: Date;
  instructions: string;
  attempts: number;
  lastError: string;
  expiresAt: Date | null;
  paidAt: Date | null;
  createdAt: Date;
}

/** Whether the invoice still expects a payment. */
export function invoiceOutstanding(i: Invoice): boolean {
  return i.status === StatusPending;
}

/** One billing period's allowance and use. */
export interface QuotaPeriod {
  id: string;
  workspaceId: string;
  periodStart: Date;
  periodEnd: Date;
  /** The package's allowance for this period. */
  tokensGranted: number;
  /** What was bought inside this period; it expires with the period. */
  tokensToppedUp: number;
  /** The tokens the ledger recorded inside this period. */
  tokensUsed: number;
  /** When each threshold was reported, which makes a warning once-per-period. */
  warning80SentAt: Date | null;
  warning100SentAt: Date | null;
  createdAt: Date;
  updatedAt: Date;
}

/** What the period grants: the package plus what was bought. */
export function periodAllowance(p: QuotaPeriod): number {
  return p.tokensGranted + p.tokensToppedUp;
}

/**
 * Whether the allowance is spent. A zero allowance is unlimited rather than
 * exhausted: it is what a package that does not cap tokens looks like.
 */
export function periodExhausted(p: QuotaPeriod): boolean {
  const allowance = periodAllowance(p);
  return allowance > 0 && p.tokensUsed >= allowance;
}

/** The usage as a percentage of the allowance, or zero when unlimited. */
export function periodUsed(p: QuotaPeriod): number {
  const allowance = periodAllowance(p);
  if (allowance <= 0) return 0;
  // Integer division, exactly as Go's `int(p.TokensUsed * 100 / allowance)`.
  return Math.trunc((p.tokensUsed * 100) / allowance);
}

/** What is left, or -1 when the allowance is unlimited. */
export function periodRemaining(p: QuotaPeriod): number {
  const allowance = periodAllowance(p);
  if (allowance <= 0) return -1;
  return allowance - p.tokensUsed;
}

/** A one-off purchase of tokens. */
export interface TopUpPackage {
  id: string;
  key: string;
  name: string;
  tokenAmount: number;
  priceIdr: number;
  sortOrder: number;
  active: boolean;
}

/** The token and cost total of one period, read from the ledger. */
export interface Usage {
  tokens: number;
  /** What the period cost, in micro-rupiah. */
  costMicros: number;
}

/** What one workspace may create, read from its live package. Zero means unlimited. */
export interface Limits {
  maxAgents: number;
  maxIntegrations: number;
  maxMembers: number;
}

/** Everything the subscription screen shows, in one read. */
export interface Snapshot {
  plan: Plan;
  subscription: Subscription;
  /** The package the next period will be billed at, when a person chose one. */
  pendingPlanKey: string;
  pendingPlanName: string;
  entitlement: string;
  /** How long the workspace has after the period ends. */
  graceDays: number;
  period: QuotaPeriod;
  /** What the workspace owes, oldest first. */
  outstanding: Invoice[];
  /** The recent history, newest first. */
  invoices: Invoice[];
  /** The top-up packs this workspace may buy. */
  topUps: TopUpPackage[];
  /** What the workspace spent today, the other bound the quota asks. */
  usageToday: Usage;
  limits: Limits;
  /** The derived state the middleware and the screens both read. */
  readOnly: boolean;
}

/** What the quota check answers. */
export interface Check {
  allowed: boolean;
  entitlement: string;
  period: QuotaPeriod;
  dailyCostMicros: number;
  spentTodayMicros: number;
}

/** Whether the day's ceiling is reached. */
export function dailyExhausted(c: Check): boolean {
  return c.dailyCostMicros > 0 && c.spentTodayMicros >= c.dailyCostMicros;
}

/** The thresholds crossed by a period, which is what the sweep sends. */
export interface Warnings {
  at80: boolean;
  at100: boolean;
}

/**
 * The thresholds this read crosses and has not reported, so the caller marks
 * exactly what it is about to send. A period that jumped from 70% to 105%
 * reports both, in order.
 */
export function claimWarnings(w: Warnings): number[] {
  const thresholds: number[] = [];
  if (w.at80) thresholds.push(WarningThreshold);
  if (w.at100) thresholds.push(ExhaustedThreshold);
  return thresholds;
}

/** Which thresholds a period has crossed and not yet reported. */
export function warningsAt(period: QuotaPeriod): Warnings {
  if (periodAllowance(period) <= 0) {
    // Unlimited: there is nothing to warn about, and a percentage of infinity
    // is not a number.
    return { at80: false, at100: false };
  }

  return {
    at80:
      period.tokensUsed * 100 >= periodAllowance(period) * WarningThreshold &&
      period.warning80SentAt === null,
    at100:
      period.tokensUsed * 100 >= periodAllowance(period) * ExhaustedThreshold &&
      period.warning100SentAt === null,
  };
}
