/**
 * The ports and the service contract the billing module implements.
 *
 * A module may import another module's `domain` and nothing deeper, so every
 * cross-module need is an interface here: the ledger (`UsageReader`), the live
 * counter (`SpendCounter`), the payment provider (`Gateway`), the notifier, the
 * stream and the customer directory.
 *
 * Ported 1:1 from internal/modules/billing/domain/service.go.
 */

import type {
  Check,
  Invoice,
  Limits,
  PaymentRoute,
  Plan,
  QuotaPeriod,
  Scope,
  Snapshot,
  Subscription,
  TopUpPackage,
} from "./billing.ts";

/**
 * What the billing module stores: the packages it sells, the subscription a
 * workspace is on, the invoices it owes, the periods its allowance is counted
 * over, and the top-up packs it may buy.
 *
 * A read the Go code runs inside an existing transaction takes the transaction
 * (`startTrial`); everything else opens its own scope.
 */
export interface Repository {
  /** The packages this deployment sells, in the order the screens show them. */
  plans(): Promise<Plan[]>;
  /** One package by its key, which is what a person chooses. */
  plan(key: string): Promise<Plan>;
  /** The package a subscription names. */
  planById(id: string): Promise<Plan>;
  /** The package a new workspace starts on. */
  trialPlan(): Promise<Plan>;

  /** The workspace's live package. A workspace always has one. */
  subscription(scope: Scope): Promise<Subscription>;
  /** Writes a subscription back: its status, its period, and its grace window. */
  setSubscription(scope: Scope, subscription: Subscription): Promise<Subscription>;
  /**
   * The workspaces the sweep still has to look at, across every workspace,
   * oldest first. Unscoped on purpose: it is what tells the sweep which tenant
   * to open a scope with.
   */
  dueWorkspaces(now: Date, limit: number): Promise<string[]>;
  /** Moves a workspace's next pass forward. */
  touchDirectory(scope: Scope, workspaceId: string, nextSyncAt: Date): Promise<void>;

  /**
   * The period a subscription is in at a moment, opening it from the
   * subscription's own times when it does not exist yet. The insert is the
   * row's own unique key, so two callers at the boundary write one period.
   */
  period(scope: Scope, subscription: Subscription, plan: Plan, at: Date): Promise<QuotaPeriod>;
  /** Writes a period back: its usage, its top-up, and which warnings were sent. */
  savePeriod(scope: Scope, period: QuotaPeriod): Promise<QuotaPeriod>;
  /** Adds tokens to the period that starts at a moment. */
  addTopUp(scope: Scope, periodStart: Date, tokens: number): Promise<QuotaPeriod>;
  /** Closes an invoice whose payment window has passed; `fresh` says whether it changed. */
  expireInvoice(scope: Scope, id: string): Promise<{ invoice: Invoice | null; fresh: boolean }>;

  /** One invoice. */
  invoice(scope: Scope, id: string): Promise<Invoice>;
  /**
   * Which workspace and which invoice a provider's own reference names. The one
   * unscoped read a payment needs: a provider does not know the tenant.
   */
  paymentRoute(provider: string, reference: string): Promise<PaymentRoute>;
  /**
   * The invoice of one period and kind that still stands, which is what makes
   * creating one idempotent.
   */
  invoiceForPeriod(
    scope: Scope,
    subscriptionId: string,
    kind: string,
    periodStart: Date,
  ): Promise<Invoice>;
  /** A workspace's invoices, newest first. */
  invoices(scope: Scope, filter: InvoiceFilter): Promise<Invoice[]>;
  /** What a workspace still owes, oldest first. */
  outstanding(scope: Scope): Promise<Invoice[]>;
  /** Writes one invoice, idempotently against the period it names. */
  createInvoice(scope: Scope, invoice: Invoice): Promise<Invoice>;
  /**
   * Records an invoice's outcome and whether the payment was new. `fresh` is
   * false when the invoice already had that outcome, which makes a replayed
   * webhook harmless.
   */
  settleInvoice(scope: Scope, invoice: Invoice): Promise<{ invoice: Invoice; fresh: boolean }>;
  /** Records the payment link a provider returned, or why it could not be created. */
  setInvoicePayment(scope: Scope, invoice: Invoice): Promise<Invoice>;

  /** The packs a workspace may buy. */
  topUps(scope: Scope): Promise<TopUpPackage[]>;
  /** One pack by its key. */
  topUp(scope: Scope, key: string): Promise<TopUpPackage>;

  /**
   * Creates the live subscription a new workspace begins on, the quota period it
   * grants, and the queue row the sweep finds it by — inside the caller's
   * transaction. It is the workspace module's trial provisioner.
   */
  startTrial(tx: unknown, workspaceId: string): Promise<void>;
}

/**
 * Reads the token and cost totals a workspace recorded in the ledger.
 *
 * The day boundary is not a parameter: it belongs to whoever owns the ledger.
 */
export interface UsageReader {
  /** Sums the tokens recorded since a moment. */
  periodTokens(workspaceId: string, since: Date): Promise<number>;
  /** The cost recorded today, in micro-rupiah. */
  dailyCost(workspaceId: string): Promise<number>;
}

/**
 * The live token and cost total of a workspace, which is the fast path the quota
 * check reads. Optional: without it every check reads the ledger.
 */
export interface SpendCounter {
  /** The tokens recorded for the current period. */
  periodTokens(workspaceId: string): Promise<number>;
  /** The cost recorded for the current day, in micro-rupiah. */
  dailyCost(workspaceId: string): Promise<number>;
}

/**
 * Creates a payment and verifies that a payment happened. A deployment with no
 * provider configured gets null, and an invoice is then created without a link.
 */
export interface Gateway {
  /** Asks the provider for a payment for one invoice. */
  charge(invoice: Invoice, customer: Customer): Promise<Charge>;
  /**
   * Checks a delivery's signature and parses it. A delivery it cannot trust is
   * refused with `ErrUnverifiable` wrapped.
   */
  verify(delivery: Delivery): Promise<Payment>;
  /** The provider's key, which is what an invoice records and routes a webhook. */
  name(): string;
}

/** What a provider needs to address an invoice to somebody. */
export interface Customer {
  workspaceId: string;
  workspace: string;
  email: string;
  name: string;
}

/** What one successful payment creation answers. */
export interface Charge {
  /** The provider's own id for the payment. */
  reference: string;
  /** What a person acts on to pay: a URL, or the virtual account and its bank. */
  instructions: string;
  /** When the instructions stop working. */
  expiresAt: Date;
}

/** One payment notification, as it arrived. */
export interface Delivery {
  /** Which provider the notification is addressed to, taken from the path. */
  provider: string;
  /** The raw payload. */
  body: Uint8Array;
  /** The notification's headers, which is where most providers put the signature. */
  headers: Record<string, string>;
}

/** What a verified delivery says happened. */
export interface Payment {
  reference: string;
  amountIDR: number;
  paid: boolean;
  /** A link that ran out rather than a payment that failed. */
  expired: boolean;
  /** When the provider says the money landed. The Go zero time is `null`. */
  paidAt: Date | null;
}

/** Tells a workspace's people that something about their package needs them. */
export interface Notifier {
  notify(scope: Scope, notice: Notice): Promise<void>;
}

/** One thing a person needs to know about the package. */
export interface Notice {
  /** One of the `Notice*` constants. */
  kind: string;
  /** What happened, in the user's words. */
  reason: string;
  /** The package it concerns. */
  planName: string;
  /** The percentage for a warning notice, or zero. */
  threshold: number;
  /** The numbers the notice quotes, or zero. */
  used: number;
  allowance: number;
  /** The derived state, so both kinds of notice carry the same fact. */
  entitlement: string;
}

/** Records what happened to a workspace's package in its durable stream. */
export interface Activity {
  emit(event: Event): Promise<void>;
}

/** One line in the workspace's stream. */
export interface Event {
  workspaceId: string;
  type: string;
  payload: unknown;
}

/** An allowance crossing 80% or 100%. */
export const NoticeWarning = "billing.quota_warning";
/** A period ending with an unpaid invoice. */
export const NoticePastDue = "billing.past_due";
/** A workspace that can no longer write. */
export const NoticeReadOnly = "billing.read_only";
/** A payment that landed. */
export const NoticePaid = "billing.paid";
/** Tokens bought. */
export const NoticeTopUp = "billing.top_up";

/** Reports the current time, so a period boundary and a grace window are testable. */
export type Clock = () => Date;

/** Narrows an invoice read. */
export interface InvoiceFilter {
  /** One status, or every status when empty. */
  status: string;
  limit: number;
}

/** One package change a person asked for. */
export interface ChangeRequest {
  /** The package to move to. */
  planKey: string;
}

/** One purchase of tokens. */
export interface TopUpRequest {
  /** The top-up pack to buy. */
  packageKey: string;
}

/** What one pass of the billing sweep did. */
export interface SweepResult {
  /** Periods synced from the live counter into `quota_periods`. */
  periodsSynced: number;
  /** Warnings sent, by threshold. */
  warnings80: number;
  warnings100: number;
  /** Subscriptions moved past due, and those moved read-only. */
  pastDue: number;
  readOnly: number;
  /** Invoices created for periods that turned. */
  invoicesCreated: number;
  /** Invoices expired because their payment window closed. */
  invoicesExpired: number;
}

/** The billing use case contract. */
export interface Service {
  /** Everything the subscription screen shows, in one read. */
  snapshot(scope: Scope): Promise<Snapshot>;
  /** The packages a workspace may move to. */
  plans(scope: Scope): Promise<Plan[]>;
  /** Moves the workspace to another package. */
  change(scope: Scope, request: ChangeRequest): Promise<Snapshot>;
  /** What a workspace was charged. */
  invoices(scope: Scope, filter: InvoiceFilter): Promise<Invoice[]>;
  /** Asks the provider for a payment link for one invoice. */
  pay(scope: Scope, invoiceId: string): Promise<Invoice>;
  /** Buys tokens inside the period the workspace is in. */
  buyTopUp(scope: Scope, request: TopUpRequest): Promise<Invoice>;
  /** Applies a verified payment notification. */
  handlePayment(delivery: Delivery): Promise<void>;

  /** Whether a workspace may spend right now, which the gateway and the task runtime ask. */
  check(workspaceId: string): Promise<Check>;
  /** What the workspace's package allows it to create. */
  limits(workspaceId: string): Promise<Limits>;
  /** Whether the workspace is past its grace window. */
  readOnly(workspaceId: string): Promise<boolean>;

  /** One reconciliation pass over every workspace. */
  sweep(): Promise<SweepResult>;
}
