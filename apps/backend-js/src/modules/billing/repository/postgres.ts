/**
 * The billing data access.
 *
 * Every method that names a workspace runs inside the caller's scope, so Row
 * Level Security filters the statements even if a query forgets a filter. The
 * three that do not are the ones that cannot: the sweep queue, which exists to
 * say which tenant to open a scope with; the invoice a payment provider names,
 * which the provider cannot scope for us; and the two catalogues, which carry no
 * tenant at all.
 *
 * Ported 1:1 from internal/modules/billing/repository/postgres.go, with
 * repository/sqlcgen/*.go folded in: each query keeps its name as a comment and
 * its SQL text unchanged.
 */

import type { Database, Sql, Tx } from "../../../platform/database/index.ts";
import {
  DefaultGraceDays,
  DefaultListLimit,
  ErrInvalidInput,
  ErrInvoiceNotFound,
  ErrNotFound,
  ErrPlanNotFound,
  ErrSubscriptionNotFound,
  ErrTopUpNotFound,
  MaxListLimit,
  NilUUID,
  StatusTrial,
  isZeroScope,
  wrap,
  type Invoice,
  type Plan,
  type QuotaPeriod,
  type Scope,
  type Subscription,
  type TopUpPackage,
} from "../domain/billing.ts";
import type { InvoiceFilter, Repository } from "../domain/service.ts";
import {
  toInvoice,
  toPeriod,
  toPlan,
  toSubscription,
  toTopUp,
  type InvoiceRow,
  type PlanRow,
  type QuotaPeriodRow,
  type SubscriptionRow,
  type TopUpRow,
} from "./convert.ts";

/** A driver failure, labelled with the operation that produced it. */
function translate(err: unknown, operation: string): Error {
  const message = err instanceof Error ? err.message : String(err);
  return new Error(`billing: ${operation}: ${message}`, { cause: err });
}

/** Applies the module's page ceiling. */
function limitOr(value: number): number {
  if (value <= 0 || value > MaxListLimit) {
    return DefaultListLimit;
  }
  return value;
}

/**
 * Renders an optional id. The subscription column is nullable in the schema, and
 * the zero uuid would name a subscription that is not there rather than the
 * absence of one.
 */
function uuidOrNull(id: string): string | null {
  return !id || id === NilUUID ? null : id;
}

/**
 * The period a moment belongs to.
 *
 * The period is the subscription's own rather than a calendar month: a workspace
 * that subscribed on the 20th is granted its allowance on the 20th, and the
 * row's unique key is its start.
 */
function periodWindow(
  subscription: Subscription,
  plan: Plan,
  at: Date,
): { start: Date; end: Date } {
  let start = subscription.currentPeriodStart;
  let end = subscription.currentPeriodEnd;

  // The window walks forward when the subscription has already been extended
  // past the moment being asked about. It stops at the moment rather than at
  // every period in between: a period nobody spent in has nothing to count.
  const days = plan.periodDays > 0 ? plan.periodDays : 30;
  while (at.getTime() >= end.getTime()) {
    start = end;
    end = new Date(start.getTime());
    end.setUTCDate(end.getUTCDate() + days);
  }
  return { start, end };
}

export class PostgresRepository implements Repository {
  constructor(private readonly db: Database) {}

  /** Runs one query in the caller's scope, read-only. */
  private async read<T>(scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    if (isZeroScope(scope)) {
      throw wrap(ErrInvalidInput, "workspace is required");
    }
    return this.db.inScopeRead({ userId: scope.userId, workspaceId: scope.workspaceId }, fn);
  }

  /** Runs one query in the caller's scope. */
  private async write<T>(scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    if (isZeroScope(scope)) {
      throw wrap(ErrInvalidInput, "workspace is required");
    }
    return this.db.inScope({ userId: scope.userId, workspaceId: scope.workspaceId }, fn);
  }

  private pool(): Sql {
    return this.db.sql;
  }

  // -------------------------------------------------------------- packages

  /** name: ListPlans */
  async plans(): Promise<Plan[]> {
    // name: ListPlans
    const rows = await this.pool()<PlanRow[]>`
      SELECT * FROM plans ORDER BY sort_order, created_at
    `;
    return rows.map(toPlan);
  }

  /** name: GetPlanByKey */
  async plan(key: string): Promise<Plan> {
    const rows = await this.pool()<PlanRow[]>`SELECT * FROM plans WHERE key = ${key}`;
    const row = rows[0];
    if (!row) throw wrap(ErrPlanNotFound, key);
    return toPlan(row);
  }

  /** name: GetPlanByID */
  async planById(id: string): Promise<Plan> {
    const rows = await this.pool()<PlanRow[]>`SELECT * FROM plans WHERE id = ${id}`;
    const row = rows[0];
    if (!row) throw wrap(ErrPlanNotFound, id);
    return toPlan(row);
  }

  /** name: GetTrialPlan */
  async trialPlan(): Promise<Plan> {
    const rows = await this.pool()<PlanRow[]>`
      SELECT * FROM plans WHERE is_trial ORDER BY sort_order LIMIT 1
    `;
    const row = rows[0];
    if (!row) {
      // A deployment whose packages were all removed has no trial to start on,
      // which is a deployment problem rather than a request problem.
      throw wrap(ErrPlanNotFound, "no trial plan is seeded");
    }
    return toPlan(row);
  }

  // -------------------------------------------------------------- subscription

  /** name: GetLiveSubscription */
  async subscription(scope: Scope): Promise<Subscription> {
    return this.read(scope, async (tx) => {
      const rows = await tx<SubscriptionRow[]>`
        SELECT * FROM subscriptions
        WHERE workspace_id = ${scope.workspaceId} AND status <> 'canceled'
        ORDER BY created_at DESC
        LIMIT 1
      `;
      const row = rows[0];
      if (!row) {
        // A workspace with no live subscription is the invariant the migration
        // backfilled away, so it is a broken row rather than a state.
        throw wrap(ErrSubscriptionNotFound, "the workspace has no live subscription");
      }
      return toSubscription(row);
    });
  }

  /** name: UpdateSubscription */
  async setSubscription(scope: Scope, subscription: Subscription): Promise<Subscription> {
    return this.write(scope, async (tx) => {
      const rows = await tx<SubscriptionRow[]>`
        UPDATE subscriptions
        SET plan_id = ${subscription.planId},
            status = ${subscription.status},
            current_period_start = ${subscription.currentPeriodStart},
            current_period_end = ${subscription.currentPeriodEnd},
            grace_until = ${subscription.graceUntil},
            past_due_since = ${subscription.pastDueSince},
            pending_plan_id = ${uuidOrNull(subscription.pendingPlanId)},
            past_due_notified_at = ${subscription.pastDueNotifiedAt},
            read_only_notified_at = ${subscription.readOnlyNotifiedAt},
            canceled_at = ${subscription.canceledAt},
            updated_at = now()
        WHERE id = ${subscription.id} AND workspace_id = ${scope.workspaceId}
        RETURNING *
      `;
      const row = rows[0];
      if (!row) throw wrap(ErrNotFound, "update subscription");
      return toSubscription(row);
    });
  }

  /**
   * Creates the live subscription a new workspace begins on, the quota period it
   * grants, and the queue row the sweep finds it by — all inside the caller's
   * transaction.
   */
  async startTrial(tx: Tx, workspaceId: string): Promise<void> {
    if (!workspaceId || workspaceId === NilUUID) {
      throw wrap(ErrInvalidInput, "workspace is required");
    }

    const planRows = await tx<PlanRow[]>`
      SELECT * FROM plans WHERE is_trial ORDER BY sort_order LIMIT 1
    `;
    const planRow = planRows[0];
    if (!planRow) throw wrap(ErrPlanNotFound, "no trial plan is seeded");
    const plan = toPlan(planRow);

    const now = new Date();
    const start = now;
    const end = new Date(now.getTime());
    end.setUTCDate(end.getUTCDate() + plan.periodDays);
    const grace = new Date(end.getTime());
    grace.setUTCDate(grace.getUTCDate() + DefaultGraceDays);

    // name: CreateSubscription
    const subscriptionRows = await tx<SubscriptionRow[]>`
      INSERT INTO subscriptions (workspace_id, plan_id, status, current_period_start, current_period_end, grace_until)
      VALUES (
          ${workspaceId},
          ${plan.id},
          ${StatusTrial},
          ${start},
          ${end},
          ${grace}
      )
      RETURNING *
    `;
    const subscription = subscriptionRows[0];
    if (!subscription) throw new Error("billing: start trial: no subscription was created");

    // name: CreateQuotaPeriod
    await tx`
      INSERT INTO quota_periods (workspace_id, period_start, period_end, tokens_granted, tokens_topped_up, tokens_used)
      VALUES (
          ${workspaceId},
          ${subscription.current_period_start},
          ${subscription.current_period_end},
          ${plan.tokenQuota},
          ${0},
          ${0}
      )
      ON CONFLICT (workspace_id, period_start) DO UPDATE
      SET period_end = EXCLUDED.period_end,
          updated_at = now()
    `;

    // name: EnsureBillingDirectory
    await tx`
      INSERT INTO billing_directory (workspace_id, next_sync_at)
      VALUES (${workspaceId}, now())
      ON CONFLICT (workspace_id) DO NOTHING
    `;
  }

  // -------------------------------------------------------------- directory

  /** name: ListDueBillingWorkspaces */
  async dueWorkspaces(now: Date, limit: number): Promise<string[]> {
    // Unscoped on purpose: the table carries a workspace id and a moment and
    // nothing else, and this read is what tells the sweep which tenant to open.
    const rows = await this.pool()<{ workspace_id: string }[]>`
      SELECT workspace_id FROM billing_directory
      WHERE next_sync_at <= ${now}
      ORDER BY next_sync_at
      LIMIT ${limitOr(limit)}
    `;
    return rows.map((row) => row.workspace_id);
  }

  /** name: TouchBillingDirectory */
  async touchDirectory(scope: Scope, workspaceId: string, nextSyncAt: Date): Promise<void> {
    await this.write(scope, async (tx) => {
      await tx`
        UPDATE billing_directory
        SET next_sync_at = ${nextSyncAt}, updated_at = now()
        WHERE workspace_id = ${workspaceId}
      `;
    });
  }

  // -------------------------------------------------------------- quota periods

  /** name: CreateQuotaPeriod */
  async period(
    scope: Scope,
    subscription: Subscription,
    plan: Plan,
    at: Date,
  ): Promise<QuotaPeriod> {
    const { start, end } = periodWindow(subscription, plan, at);

    return this.write(scope, async (tx) => {
      const rows = await tx<QuotaPeriodRow[]>`
        INSERT INTO quota_periods (workspace_id, period_start, period_end, tokens_granted, tokens_topped_up, tokens_used)
        VALUES (
            ${scope.workspaceId},
            ${start},
            ${end},
            ${plan.tokenQuota},
            ${0},
            ${0}
        )
        ON CONFLICT (workspace_id, period_start) DO UPDATE
        SET period_end = EXCLUDED.period_end,
            updated_at = now()
        RETURNING *
      `;
      const row = rows[0];
      if (!row) throw wrap(ErrNotFound, "open quota period");
      return toPeriod(row);
    });
  }

  /** name: UpdateQuotaPeriod */
  async savePeriod(scope: Scope, period: QuotaPeriod): Promise<QuotaPeriod> {
    return this.write(scope, async (tx) => {
      const rows = await tx<QuotaPeriodRow[]>`
        UPDATE quota_periods
        SET tokens_used = ${period.tokensUsed},
            tokens_topped_up = ${period.tokensToppedUp},
            warning_80_sent_at = ${period.warning80SentAt},
            warning_100_sent_at = ${period.warning100SentAt},
            updated_at = now()
        WHERE id = ${period.id} AND workspace_id = ${scope.workspaceId}
        RETURNING *
      `;
      const row = rows[0];
      if (!row) throw wrap(ErrNotFound, "save quota period");
      return toPeriod(row);
    });
  }

  /** name: AddQuotaTopUp */
  async addTopUp(scope: Scope, periodStart: Date, tokens: number): Promise<QuotaPeriod> {
    return this.write(scope, async (tx) => {
      const rows = await tx<QuotaPeriodRow[]>`
        UPDATE quota_periods
        SET tokens_topped_up = tokens_topped_up + ${tokens},
            updated_at = now()
        WHERE workspace_id = ${scope.workspaceId} AND period_start = ${periodStart}
        RETURNING *
      `;
      const row = rows[0];
      if (!row) throw wrap(ErrNotFound, "add quota top-up");
      return toPeriod(row);
    });
  }

  // -------------------------------------------------------------- invoices

  /** name: GetInvoice */
  async invoice(scope: Scope, id: string): Promise<Invoice> {
    return this.read(scope, async (tx) => {
      const rows = await tx<InvoiceRow[]>`
        SELECT * FROM billing_invoices
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
      `;
      const row = rows[0];
      if (!row) throw wrap(ErrNotFound, "read invoice");
      return toInvoice(row);
    });
  }

  /** name: GetPaymentRoute */
  async paymentRoute(provider: string, reference: string): Promise<{ workspaceId: string; invoiceId: string }> {
    // Unscoped, and it has to be: a provider does not know which workspace it is
    // talking about. The row it finds carries the two ids and nothing else.
    const rows = await this.pool()<{ workspace_id: string; invoice_id: string }[]>`
      SELECT workspace_id, invoice_id FROM billing_payment_references
      WHERE provider = ${provider} AND reference = ${reference}
    `;
    const row = rows[0];
    if (!row) {
      throw wrap(ErrInvoiceNotFound, `${provider}/${reference}`);
    }
    return { workspaceId: row.workspace_id, invoiceId: row.invoice_id };
  }

  /** name: GetActiveInvoiceForPeriod */
  async invoiceForPeriod(
    scope: Scope,
    subscriptionId: string,
    kind: string,
    periodStart: Date,
  ): Promise<Invoice> {
    return this.read(scope, async (tx) => {
      const rows = await tx<InvoiceRow[]>`
        SELECT * FROM billing_invoices
        WHERE workspace_id = ${scope.workspaceId}
          AND subscription_id = ${uuidOrNull(subscriptionId)}
          AND kind = ${kind}
          AND period_start = ${periodStart}
          AND status IN ('pending', 'paid')
      `;
      const row = rows[0];
      if (!row) throw wrap(ErrNotFound, "read invoice for period");
      return toInvoice(row);
    });
  }

  /** name: ListInvoices */
  async invoices(scope: Scope, filter: InvoiceFilter): Promise<Invoice[]> {
    return this.read(scope, async (tx) => {
      const rows = await tx<InvoiceRow[]>`
        SELECT * FROM billing_invoices
        WHERE workspace_id = ${scope.workspaceId}
          AND (${filter.status}::text = '' OR status = ${filter.status})
        ORDER BY created_at DESC
        LIMIT ${limitOr(filter.limit)}
      `;
      return rows.map(toInvoice);
    });
  }

  /** name: ListOutstandingInvoices */
  async outstanding(scope: Scope): Promise<Invoice[]> {
    return this.read(scope, async (tx) => {
      const rows = await tx<InvoiceRow[]>`
        SELECT * FROM billing_invoices
        WHERE workspace_id = ${scope.workspaceId} AND status = 'pending'
        ORDER BY period_start, created_at
      `;
      return rows.map(toInvoice);
    });
  }

  /**
   * name: CreateInvoice
   *
   * `ON CONFLICT DO NOTHING` rather than letting the unique index raise: a
   * conflict means another writer created this period's invoice a moment ago,
   * and that is an answer to read rather than a failure.
   */
  async createInvoice(scope: Scope, invoice: Invoice): Promise<Invoice> {
    return this.write(scope, async (tx) => {
      const rows = await tx<InvoiceRow[]>`
        INSERT INTO billing_invoices (
            workspace_id, subscription_id, provider, provider_reference, amount_idr,
            token_amount, status, kind, period_start, period_end, payment_instructions, expires_at
        )
        VALUES (
            ${scope.workspaceId},
            ${uuidOrNull(invoice.subscriptionId)},
            ${invoice.provider},
            ${invoice.providerReference},
            ${invoice.amountIdr},
            ${invoice.tokenAmount},
            ${invoice.status},
            ${invoice.kind},
            ${invoice.periodStart},
            ${invoice.periodEnd},
            ${invoice.instructions},
            ${invoice.expiresAt}
        )
        ON CONFLICT DO NOTHING
        RETURNING *
      `;
      const row = rows[0];
      if (row) return toInvoice(row);

      // No rows: a second writer created this period's invoice a moment ago —
      // two sweep passes, or a sweep and a screen. The row's own index is what
      // decides, and the answer is the invoice that exists.
      const existing = await tx<InvoiceRow[]>`
        SELECT * FROM billing_invoices
        WHERE workspace_id = ${scope.workspaceId}
          AND subscription_id = ${uuidOrNull(invoice.subscriptionId)}
          AND kind = ${invoice.kind}
          AND period_start = ${invoice.periodStart}
          AND status IN ('pending', 'paid')
      `;
      const found = existing[0];
      if (!found) throw wrap(ErrNotFound, "read the invoice that was created first");
      return toInvoice(found);
    });
  }

  /**
   * name: SetInvoiceSettlement
   *
   * The guard is the statement's own `status = 'pending'`, so a replayed webhook
   * updates nothing and this answers with the invoice as it already stands.
   */
  async settleInvoice(
    scope: Scope,
    invoice: Invoice,
  ): Promise<{ invoice: Invoice; fresh: boolean }> {
    return this.write(scope, async (tx) => {
      const rows = await tx<InvoiceRow[]>`
        UPDATE billing_invoices
        SET status = ${invoice.status},
            paid_at = ${invoice.paidAt},
            updated_at = now()
        WHERE id = ${invoice.id}
          AND workspace_id = ${scope.workspaceId}
          AND status = 'pending'
        RETURNING *
      `;
      const row = rows[0];
      if (row) return { invoice: toInvoice(row), fresh: true };

      // No rows: the invoice is no longer pending, so the answer is what it
      // already says rather than a failure.
      const current = await tx<InvoiceRow[]>`
        SELECT * FROM billing_invoices
        WHERE id = ${invoice.id} AND workspace_id = ${scope.workspaceId}
      `;
      const found = current[0];
      if (!found) throw wrap(ErrNotFound, "read settled invoice");
      return { invoice: toInvoice(found), fresh: false };
    });
  }

  /**
   * name: SetInvoicePayment
   *
   * The routing row is written with the link, in one transaction: a payment link
   * that exists is a payment link a notification can find its way back from.
   */
  async setInvoicePayment(scope: Scope, invoice: Invoice): Promise<Invoice> {
    return this.write(scope, async (tx) => {
      // name: RememberPaymentRoute
      await tx`
        INSERT INTO billing_payment_references (provider, reference, workspace_id, invoice_id)
        VALUES (${invoice.provider}, ${invoice.providerReference}, ${scope.workspaceId}, ${invoice.id})
        ON CONFLICT (provider, reference) DO NOTHING
      `;

      const rows = await tx<InvoiceRow[]>`
        UPDATE billing_invoices
        SET provider = ${invoice.provider},
            provider_reference = ${invoice.providerReference},
            payment_instructions = ${invoice.instructions},
            expires_at = ${invoice.expiresAt},
            attempts = ${invoice.attempts},
            last_error = ${invoice.lastError},
            updated_at = now()
        WHERE id = ${invoice.id} AND workspace_id = ${scope.workspaceId}
        RETURNING *
      `;
      const row = rows[0];
      if (!row) throw wrap(ErrNotFound, "record invoice payment");
      return toInvoice(row);
    });
  }

  /** name: ExpireInvoice */
  async expireInvoice(
    scope: Scope,
    id: string,
  ): Promise<{ invoice: Invoice | null; fresh: boolean }> {
    return this.write(scope, async (tx) => {
      const rows = await tx<InvoiceRow[]>`
        UPDATE billing_invoices
        SET status = 'expired', updated_at = now()
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId} AND status = 'pending'
        RETURNING *
      `;
      const row = rows[0];
      if (!row) {
        // Already settled or already expired: nothing to change, and not a failure.
        return { invoice: null, fresh: false };
      }
      return { invoice: toInvoice(row), fresh: true };
    });
  }

  // -------------------------------------------------------------- top-ups

  /** name: ListTopUpPackages */
  async topUps(_scope: Scope): Promise<TopUpPackage[]> {
    // A catalogue rather than tenant data: every workspace sees the same packs.
    const rows = await this.pool()<TopUpRow[]>`
      SELECT * FROM topup_packages WHERE active ORDER BY sort_order
    `;
    return rows.map(toTopUp);
  }

  /** name: GetTopUpPackage */
  async topUp(_scope: Scope, key: string): Promise<TopUpPackage> {
    const rows = await this.pool()<TopUpRow[]>`
      SELECT * FROM topup_packages WHERE key = ${key} AND active
    `;
    const row = rows[0];
    if (!row) throw wrap(ErrTopUpNotFound, key);
    return toTopUp(row);
  }
}
