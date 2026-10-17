/**
 * The billing use cases: what a workspace is on, what it may spend, what it
 * owes, and what happens when it does not pay.
 *
 * The shape of the module follows from one decision: **what a workspace may
 * spend is derived, never remembered.** So the entitlement is computed from the
 * subscription's times on every read, the allowance is the package plus what was
 * bought, and the usage is whichever of the two mirrors — the live counter, then
 * the ledger — is telling the truth.
 *
 * Ported 1:1 from internal/modules/billing/service/service.go. The methods Go
 * declares in `quota.go` and `payment.go` are implemented in those files as
 * functions over the service, because TypeScript cannot spread one class across
 * files; the class methods here delegate to them.
 */

import { nopLogger, type Logger } from "../../../platform/logger.ts";
import {
  DefaultGraceDays,
  DefaultListLimit,
  EntitlementReadOnly,
  ErrInvalidInput,
  NilUUID,
  StatusTrial,
  periodAllowance,
  entitlement,
  isZeroScope,
  wrap,
  type Check,
  type Invoice,
  type Limits,
  type Plan,
  type Scope,
  type Snapshot,
  type Subscription,
} from "../domain/billing.ts";
import {
  type ChangeRequest,
  type Clock,
  type Customer,
  type Delivery,
  type Gateway,
  type InvoiceFilter,
  type Notice,
  type Notifier,
  type Repository,
  type Service,
  type SpendCounter,
  type SweepResult,
  type TopUpRequest,
  type UsageReader,
} from "../domain/service.ts";
import { check as checkQuota, dailyCost, sweep as sweepBilling } from "./quota.ts";
import { buyTopUp, handlePayment, pay } from "./payment.ts";

/** How often a workspace is reconciled when nothing says otherwise. */
export const DefaultSyncInterval = 15 * 60 * 1000;

/** Bounds one pass. */
export const DefaultSweepLimit = 200;

/** The settings a deployment decides. */
export interface Config {
  /** How long a workspace keeps working after its period ends. Zero: the product default. */
  graceDays: number;
  /** How often one workspace is reconciled, in milliseconds. */
  syncInterval: number;
  /** Bounds one pass, so a large fleet reconciles over several passes. */
  sweepLimit: number;
}

/**
 * Who a payment is for, which is what a provider addresses an invoice to. It is
 * a port for the reason the notifier is: the workspace module owns the name and
 * the address, and this module may not import it.
 */
export interface Customers {
  customer(scope: Scope): Promise<Customer>;
}

/** The service dependencies. */
export interface Deps {
  repository: Repository;
  /** Reads the ledger, the durable truth about what was spent. Optional. */
  usage?: UsageReader | null;
  /** The live token and cost total. Optional: without it every check reads the ledger. */
  counter?: SpendCounter | null;
  /** Creates a payment. Null means this deployment takes no online payment. */
  gateway?: Gateway | null;
  /** Tells the workspace's people what changed. Optional. */
  notifier?: Notifier | null;
  /** Who a payment is addressed to. Optional. */
  customers?: Customers | null;
  logger?: Logger | null;
  clock?: Clock | null;
  config?: Partial<Config> | null;
}

/** The resolved dependencies: what the service reads after construction. */
export interface ResolvedDeps {
  repository: Repository;
  usage: UsageReader | null;
  counter: SpendCounter | null;
  gateway: Gateway | null;
  notifier: Notifier | null;
  customers: Customers | null;
  logger: Logger;
  clock: Clock;
}

/** The billing module. */
export class BillingService implements Service {
  readonly deps: ResolvedDeps;
  /** The time source, so a period boundary and a grace window are testable. */
  readonly now: Clock;
  /** How long a workspace keeps working after its period ends, in milliseconds. */
  readonly grace: number;
  readonly cfg: Config;

  constructor(deps: Deps) {
    const clock = deps.clock ?? (() => new Date());

    const graceDays =
      deps.config?.graceDays && deps.config.graceDays > 0
        ? deps.config.graceDays
        : DefaultGraceDays;
    const syncInterval =
      deps.config?.syncInterval && deps.config.syncInterval > 0
        ? deps.config.syncInterval
        : DefaultSyncInterval;
    const sweepLimit =
      deps.config?.sweepLimit && deps.config.sweepLimit > 0
        ? deps.config.sweepLimit
        : DefaultSweepLimit;

    this.deps = {
      repository: deps.repository,
      usage: deps.usage ?? null,
      counter: deps.counter ?? null,
      gateway: deps.gateway ?? null,
      notifier: deps.notifier ?? null,
      customers: deps.customers ?? null,
      logger: deps.logger ?? nopLogger,
      clock,
    };
    this.now = clock;
    this.grace = graceDays * 24 * 60 * 60 * 1000;
    this.cfg = { graceDays, syncInterval, sweepLimit };
  }

  /**
   * Reads the workspace's subscription and the package it is on.
   *
   * Deliberately not cached: a package change, a payment, and a period boundary
   * all have to be visible to the next read, and a cached plan would be a
   * workspace served for days on an allowance it no longer has.
   */
  async live(workspaceId: string): Promise<{ subscription: Subscription; plan: Plan }> {
    if (!workspaceId || workspaceId === NilUUID) {
      throw wrap(ErrInvalidInput, "workspace is required");
    }

    const scope: Scope = { workspaceId };
    const subscription = await this.deps.repository.subscription(scope);
    const plan = await this.deps.repository.planById(subscription.planId);
    return { subscription, plan };
  }

  /** Everything the subscription screen shows, in one read. */
  async snapshot(scope: Scope): Promise<Snapshot> {
    if (isZeroScope(scope)) {
      throw wrap(ErrInvalidInput, "workspace is required");
    }

    const { subscription, plan } = await this.live(scope.workspaceId);

    const now = this.now();
    const period = await this.deps.repository.period(scope, subscription, plan, now);
    const outstanding = await this.deps.repository.outstanding(scope);
    const invoices = await this.deps.repository.invoices(scope, {
      status: "",
      limit: DefaultListLimit,
    });
    const topUps = await this.deps.repository.topUps(scope);

    const state = entitlement(subscription, now, this.grace);

    let pendingPlanKey = "";
    let pendingPlanName = "";
    if (subscription.pendingPlanId !== NilUUID) {
      try {
        const pending = await this.deps.repository.planById(subscription.pendingPlanId);
        pendingPlanKey = pending.key;
        pendingPlanName = pending.name;
      } catch {
        // A pending package that no longer exists is not a reason to fail the
        // whole screen; the field stays empty and the current package shows.
      }
    }

    return {
      plan,
      subscription,
      pendingPlanKey,
      pendingPlanName,
      entitlement: state,
      graceDays: this.cfg.graceDays,
      period,
      outstanding,
      invoices,
      topUps,
      usageToday: { tokens: 0, costMicros: await this.dailyCost(scope.workspaceId) },
      limits: {
        maxAgents: plan.maxAgents,
        maxIntegrations: plan.maxIntegrations,
        maxMembers: plan.maxMembers,
      },
      readOnly: state === EntitlementReadOnly,
    };
  }

  /** The packages a workspace may move to. */
  async plans(scope: Scope): Promise<Plan[]> {
    if (isZeroScope(scope)) {
      throw wrap(ErrInvalidInput, "workspace is required");
    }
    return this.deps.repository.plans();
  }

  /**
   * Moves the workspace to another package.
   *
   * The change is deferred to the next period whenever the workspace is inside a
   * period it already has: taking a bigger package mid-month would either hand
   * out the bigger allowance for free or charge a proration nobody asked for. A
   * workspace on its trial has no period to protect, so the change applies at
   * once.
   *
   * An outstanding invoice for the next period is withdrawn when the price
   * changes, because the amount it asked for is no longer the amount owed.
   */
  async change(scope: Scope, request: ChangeRequest): Promise<Snapshot> {
    if (isZeroScope(scope)) {
      throw wrap(ErrInvalidInput, "workspace is required");
    }

    const key = (request.planKey ?? "").trim();
    if (key === "") {
      throw wrap(ErrInvalidInput, "pilih paket yang dituju");
    }

    const { subscription, plan } = await this.live(scope.workspaceId);

    const target = await this.deps.repository.plan(key);
    if (target.id === plan.id && subscription.pendingPlanId === NilUUID) {
      throw wrap(ErrInvalidInput, `workspace sudah memakai paket ${target.name}`);
    }

    // A trial has nothing to protect: no money has changed hands for the period
    // it is in, so the chosen package applies to it immediately.
    if (subscription.status === StatusTrial && subscription.pendingPlanId === NilUUID) {
      subscription.planId = target.id;
    } else {
      subscription.pendingPlanId = target.id;
    }

    await this.deps.repository.setSubscription(scope, subscription);

    await this.withdrawUpcomingInvoice(scope, subscription, target.name);
    await this.notify(scope, {
      kind: NoticePlanChanged,
      planName: target.name,
      reason: `Paket workspace diubah ke ${target.name}. Perubahannya berlaku saat periode berikutnya dimulai.`,
      threshold: 0,
      used: 0,
      allowance: 0,
      entitlement: "",
    });

    return this.snapshot(scope);
  }

  /**
   * Expires the outstanding invoice for a period that has not started, so a
   * re-priced one can be issued.
   */
  async withdrawUpcomingInvoice(
    scope: Scope,
    subscription: Subscription,
    planName: string,
  ): Promise<void> {
    let invoices: Invoice[];
    try {
      invoices = await this.deps.repository.outstanding(scope);
    } catch (err) {
      this.deps.logger.warn({ err }, "could not read the outstanding invoices");
      return;
    }

    for (const invoice of invoices) {
      if (invoice.periodStart.getTime() <= subscription.currentPeriodStart.getTime()) {
        continue;
      }
      try {
        await this.deps.repository.expireInvoice(scope, invoice.id);
      } catch (err) {
        this.deps.logger.warn({ err, plan: planName }, "could not withdraw an upcoming invoice");
      }
    }
  }

  /** What a workspace was charged. */
  async invoices(scope: Scope, filter: InvoiceFilter): Promise<Invoice[]> {
    if (isZeroScope(scope)) {
      throw wrap(ErrInvalidInput, "workspace is required");
    }
    return this.deps.repository.invoices(scope, filter);
  }

  /** Asks the provider for a payment link for one invoice. */
  async pay(scope: Scope, invoiceId: string): Promise<Invoice> {
    return pay(this, scope, invoiceId);
  }

  /** Buys tokens inside the period the workspace is in. */
  async buyTopUp(scope: Scope, request: TopUpRequest): Promise<Invoice> {
    return buyTopUp(this, scope, request);
  }

  /** Applies a verified payment notification. */
  async handlePayment(delivery: Delivery): Promise<void> {
    return handlePayment(this, delivery);
  }

  /**
   * Whether a workspace may spend right now, which the gateway's pre-flight
   * check and the task runtime's round check both ask.
   */
  async check(workspaceId: string): Promise<Check> {
    return checkQuota(this, workspaceId);
  }

  /**
   * What the workspace's package allows it to create. It reads the package in
   * force rather than the one a person is moving to: a cap that changed the
   * moment somebody clicked would let a workspace outgrow a package it has not
   * paid for.
   */
  async limits(workspaceId: string): Promise<Limits> {
    const { plan } = await this.live(workspaceId);
    return {
      maxAgents: plan.maxAgents,
      maxIntegrations: plan.maxIntegrations,
      maxMembers: plan.maxMembers,
    };
  }

  /** Whether the workspace is past its grace window. */
  async readOnly(workspaceId: string): Promise<boolean> {
    const { subscription } = await this.live(workspaceId);
    return entitlement(subscription, this.now(), this.grace) === EntitlementReadOnly;
  }

  /**
   * The model gateway's quota port: the period's token allowance.
   *
   * A workspace past its grace window is not reported as an allowance of zero,
   * because zero means unlimited in that port and would read as "spend freely".
   * `check` refuses instead, with the reason a person can act on.
   */
  async allowance(workspaceId: string): Promise<number> {
    const check = await this.check(workspaceId);
    return periodAllowance(check.period);
  }

  /** The model gateway's quota port: the day's ceiling, in micro-rupiah. */
  async dailyCostAllowance(workspaceId: string): Promise<number> {
    const check = await this.check(workspaceId);
    return check.dailyCostMicros;
  }

  /** The cost spent today, from whichever source answers. */
  async dailyCost(workspaceId: string): Promise<number> {
    return dailyCost(this, workspaceId);
  }

  /** One reconciliation pass over every workspace that is due. */
  async sweep(): Promise<SweepResult> {
    return sweepBilling(this);
  }

  /**
   * Sends one notice, logging a failure rather than failing the caller's work.
   *
   * A notice that cannot be sent must not undo what it is about: an allowance
   * that was spent is spent whether or not the email arrived.
   */
  async notify(scope: Scope, notice: Notice): Promise<void> {
    if (!this.deps.notifier) return;
    try {
      await this.deps.notifier.notify(scope, notice);
    } catch (err) {
      this.deps.logger.warn(
        { kind: notice.kind, err },
        "could not tell a workspace about its package",
      );
    }
  }

  /** The provider invoices record, or an empty string when none is configured. */
  gatewayName(): string {
    return this.deps.gateway ? this.deps.gateway.name() : "";
  }
}

/** The line a package change writes into the workspace's stream. */
export const NoticePlanChanged = "billing.plan_changed";

/** The answer when the module was built without its store. */
export const errNoRepository = new Error("billing: the repository is not configured");
