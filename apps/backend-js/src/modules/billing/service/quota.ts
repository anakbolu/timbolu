/**
 * The quota half: what a workspace may spend, and the reconciliation that keeps
 * the durable mirror honest.
 *
 * Two mirrors hold the same number, and they are not interchangeable. The live
 * counter in Redis answers the hot path — it is read before every model call —
 * and the ledger is the durable record. When they disagree the larger one wins,
 * which is the direction that never hands out a fresh allowance.
 *
 * Ported 1:1 from internal/modules/billing/service/quota.go.
 */

import {
  EntitlementGrace,
  EntitlementReadOnly,
  ErrQuotaExceeded,
  ErrReadOnly,
  KindSubscription,
  NilUUID,
  StatusPastDue,
  StatusPending,
  WarningThreshold,
  ExhaustedThreshold,
  periodAllowance,
  claimWarnings,
  dailyExhausted,
  entitlement,
  periodUsed,
  warningsAt,
  wrap,
  type Check,
  type Invoice,
  type Plan,
  type QuotaPeriod,
  type Scope,
  type Subscription,
} from "../domain/billing.ts";
import {
  NoticePastDue,
  NoticeReadOnly,
  NoticeWarning,
  type SweepResult,
} from "../domain/service.ts";
import type { BillingService } from "./service.ts";
import { formatIdr } from "./format.ts";
import { charge } from "./payment.ts";

/** "No source is configured", handled rather than reported. */
export const errNoCounter = new Error("billing: no live counter is configured");
export const errNoLedger = new Error("billing: no usage ledger is configured");

/** Renders an id for a message. */
function shortId(id: string): string {
  return id.length < 8 ? id : id.slice(0, 8);
}

/**
 * Whether a workspace may spend right now.
 *
 * It is asked twice at two boundaries — once by the gateway before every model
 * call, and once by the task runtime at a clean round boundary — and the two
 * answers have to agree, which is why there is one implementation.
 */
export async function check(s: BillingService, workspaceId: string): Promise<Check> {
  const { subscription, plan } = await s.live(workspaceId);

  const now = s.now();
  const scope: Scope = { workspaceId };

  const result: Check = {
    allowed: true,
    entitlement: entitlement(subscription, now, s.grace),
    period: {
      id: "",
      workspaceId,
      periodStart: now,
      periodEnd: now,
      tokensGranted: 0,
      tokensToppedUp: 0,
      tokensUsed: 0,
      warning80SentAt: null,
      warning100SentAt: null,
      createdAt: now,
      updatedAt: now,
    },
    dailyCostMicros: plan.dailyCostMicros,
    spentTodayMicros: 0,
  };

  // A workspace past its grace window may not spend or write, and the answer
  // says which of the two it is: "you are read-only" is a different instruction
  // from "you are out of tokens".
  if (result.entitlement === EntitlementReadOnly) {
    result.allowed = false;
    throw wrap(ErrReadOnly, `workspace ${shortId(workspaceId)}`);
  }

  const period = await s.deps.repository.period(scope, subscription, plan, now);
  result.period = period;

  // The stored usage is as fresh as the last reconciliation; the live counter is
  // as fresh as the last call. The check reads the live one, because refusing a
  // call that has already been paid for is worse than allowing one that will be
  // accounted for a minute later.
  const used = await usedTokens(s, workspaceId, period.periodStart);
  result.period.tokensUsed = used;

  const allowance = periodAllowance(period);
  if (allowance > 0 && used >= allowance) {
    result.allowed = false;
    throw wrap(
      ErrQuotaExceeded,
      `${used} dari ${allowance} token terpakai periode ini; beli tambahan atau tunggu periode berikutnya`,
    );
  }

  result.spentTodayMicros = await dailyCost(s, workspaceId);
  if (dailyExhausted(result)) {
    result.allowed = false;
    throw wrap(
      ErrQuotaExceeded,
      `batas biaya harian ${result.dailyCostMicros} mikro-rupiah tercapai; tugas baru bisa jalan lagi besok`,
    );
  }

  return result;
}

/**
 * The tokens spent in a period: the live counter when it answers, and the ledger
 * when it does not. The larger of the two wins, so an outage costs accuracy in
 * the safe direction.
 */
export async function usedTokens(
  s: BillingService,
  workspaceId: string,
  since: Date,
): Promise<number> {
  let live: number | null = null;
  let liveErr: unknown = null;
  let ledger: number | null = null;
  let ledgerErr: unknown = null;

  if (s.deps.counter) {
    try {
      live = await s.deps.counter.periodTokens(workspaceId);
    } catch (err) {
      liveErr = err;
    }
  } else {
    liveErr = errNoCounter;
  }

  if (s.deps.usage) {
    try {
      ledger = await s.deps.usage.periodTokens(workspaceId, since);
    } catch (err) {
      ledgerErr = err;
    }
  } else {
    ledgerErr = errNoLedger;
  }

  if (liveErr !== null && ledgerErr !== null) {
    // Neither answered, so nothing here can answer either. Returning zero would
    // hand the workspace a fresh allowance, which is the one thing this must
    // never do.
    const message = liveErr instanceof Error ? liveErr.message : String(liveErr);
    throw new Error(`billing: reading the spent tokens: ${message}`, { cause: liveErr });
  }
  if (liveErr !== null) return ledger as number;
  if (ledgerErr !== null) return live as number;

  return Math.max(live as number, ledger as number);
}

/**
 * The cost spent today, from whichever source answers. A failure is reported as
 * zero rather than as an error: the day's ceiling is the second bound, and a
 * workspace must not be refused every call because the day's total could not be
 * read — the period's allowance still holds.
 */
export async function dailyCost(s: BillingService, workspaceId: string): Promise<number> {
  if (s.deps.counter) {
    try {
      return await s.deps.counter.dailyCost(workspaceId);
    } catch {
      // Fall through to the ledger.
    }
  }
  if (s.deps.usage) {
    try {
      return await s.deps.usage.dailyCost(workspaceId);
    } catch {
      // Neither answered; zero is the answer that refuses nothing.
    }
  }
  return 0;
}

/**
 * One reconciliation pass over every workspace that is due.
 *
 * One pass does four things: syncs the live counter into `quota_periods`; sends
 * the 80% and 100% warnings, once per period per threshold; issues the invoice
 * for the next period when the current one ends and moves the subscription past
 * due; and expires the payment links that ran out.
 */
export async function sweep(s: BillingService): Promise<SweepResult> {
  const result: SweepResult = {
    periodsSynced: 0,
    warnings80: 0,
    warnings100: 0,
    pastDue: 0,
    readOnly: 0,
    invoicesCreated: 0,
    invoicesExpired: 0,
  };

  const now = s.now();
  const due = await s.deps.repository.dueWorkspaces(now, s.cfg.sweepLimit);

  for (const workspaceId of due) {
    try {
      await sweepWorkspace(s, workspaceId, now, result);
    } catch (err) {
      // One workspace that cannot be reconciled must not stop the pass: the
      // rest of the fleet is waiting, and this one will be due again.
      s.deps.logger.warn({ workspace: workspaceId, err }, "could not reconcile a workspace's billing");
    }

    // The next pass is scheduled whether or not this one worked, so a workspace
    // that failed is retried on the interval rather than in a tight loop.
    const scope: Scope = { workspaceId };
    try {
      await s.deps.repository.touchDirectory(
        scope,
        workspaceId,
        new Date(now.getTime() + s.cfg.syncInterval),
      );
    } catch (err) {
      s.deps.logger.warn(
        { workspace: workspaceId, err },
        "could not schedule a workspace's next reconciliation",
      );
    }
  }

  return result;
}

/** Reconciles one workspace. */
async function sweepWorkspace(
  s: BillingService,
  workspaceId: string,
  now: Date,
  result: SweepResult,
): Promise<void> {
  const scope: Scope = { workspaceId };

  const { subscription, plan } = await s.live(workspaceId);
  let period = await s.deps.repository.period(scope, subscription, plan, now);

  // 1. The durable mirror catches up with the ledger, and the thresholds that
  // catch-up crossed are marked in the same write.
  const used = await usedTokens(s, workspaceId, period.periodStart);
  const synced = used !== period.tokensUsed;
  period.tokensUsed = used;

  const crossed = claimWarnings(warningsAt(period));
  const markedAt = s.now();
  for (const threshold of crossed) {
    if (threshold === WarningThreshold) {
      period.warning80SentAt = markedAt;
    } else if (threshold === ExhaustedThreshold) {
      period.warning100SentAt = markedAt;
    }
  }

  if (synced || crossed.length > 0) {
    period = await s.deps.repository.savePeriod(scope, period);
    if (synced) result.periodsSynced++;
  }

  // 2. The thresholds are reported, and counted, once the marks are down.
  await warn(s, scope, subscription, plan, period, crossed, result);

  // 3. The invoices: what ran out is closed, and what is owed is issued.
  await settleInvoices(s, scope, subscription, plan, now, result);

  // 4. The transitions, which are what tell a person their workspace stopped.
  await reportTransitions(s, scope, subscription, plan, now, result);
}

/**
 * Sends the threshold warnings a period crossed, in order. A period can jump
 * from well under the allowance to over it between two passes, and the 80%
 * message is the one that gives a person time to act.
 */
async function warn(
  s: BillingService,
  scope: Scope,
  subscription: Subscription,
  plan: Plan,
  period: QuotaPeriod,
  crossed: number[],
  result: SweepResult,
): Promise<void> {
  for (const threshold of crossed) {
    await s.notify(scope, {
      kind: NoticeWarning,
      threshold,
      planName: plan.name,
      used: period.tokensUsed,
      allowance: periodAllowance(period),
      entitlement: entitlement(subscription, s.now(), s.grace),
      reason: reasonForThreshold(threshold, period),
    });

    if (threshold === WarningThreshold) {
      result.warnings80++;
    } else if (threshold === ExhaustedThreshold) {
      result.warnings100++;
    }
  }
}

/**
 * What a person reads, and it says what to do next: "you are at 80%" is a
 * number; "and here is what happens at 100%" is an instruction.
 */
function reasonForThreshold(threshold: number, period: QuotaPeriod): string {
  if (threshold === WarningThreshold) {
    return (
      `Pemakaian token workspace sudah ${periodUsed(period)}% ` +
      `(${period.tokensUsed} dari ${periodAllowance(period)}). ` +
      "Setelah kuota habis, tugas baru ditahan sampai paket tambahan dibeli atau periode berganti."
    );
  }
  return (
    `Kuota token workspace sudah habis (${period.tokensUsed} dari ${periodAllowance(period)}). ` +
    "Tugas baru ditahan; tugas yang sedang berjalan diselesaikan. " +
    "Beli paket tambahan atau tunggu periode berikutnya."
  );
}

/** Closes what ran out and issues what is owed. */
async function settleInvoices(
  s: BillingService,
  scope: Scope,
  subscription: Subscription,
  plan: Plan,
  now: Date,
  result: SweepResult,
): Promise<void> {
  const outstanding = await s.deps.repository.outstanding(scope);

  for (const invoice of outstanding) {
    if (!invoice.expiresAt || invoice.expiresAt.getTime() > now.getTime()) {
      continue;
    }
    const expired = await s.deps.repository.expireInvoice(scope, invoice.id);
    if (expired.fresh) result.invoicesExpired++;
  }

  // The invoice for the period after this one is issued when this one ends.
  if (now.getTime() < subscription.currentPeriodEnd.getTime()) {
    return;
  }

  await ensureUpcomingInvoice(s, scope, subscription, plan, result);
}

/**
 * Tells the workspace when it fell past due and when it stopped. The state is
 * derived from the times, so these notices are the only thing that needs
 * remembering — with a marker rather than by comparing two reads.
 */
async function reportTransitions(
  s: BillingService,
  scope: Scope,
  subscription: Subscription,
  plan: Plan,
  now: Date,
  result: SweepResult,
): Promise<void> {
  const state = entitlement(subscription, now, s.grace);
  let changed = false;

  if (state === EntitlementGrace) {
    if (subscription.pastDueSince === null) {
      subscription.pastDueSince = subscription.currentPeriodEnd;
      subscription.status = StatusPastDue;
      changed = true;
    }
    if (subscription.pastDueNotifiedAt === null) {
      subscription.pastDueNotifiedAt = now;
      changed = true;
      result.pastDue++;
      await s.notify(scope, {
        kind: NoticePastDue,
        planName: plan.name,
        entitlement: state,
        reason:
          `Masa langganan ${plan.name} berakhir dan tagihannya belum dibayar. ` +
          `Workspace masih bisa bekerja sampai ${s.cfg.graceDays} hari setelahnya, lalu masuk mode baca saja.`,
        threshold: 0,
        used: 0,
        allowance: 0,
      });
    }
  } else if (state === EntitlementReadOnly) {
    if (subscription.readOnlyNotifiedAt === null) {
      subscription.readOnlyNotifiedAt = now;
      changed = true;
      result.readOnly++;
      await s.notify(scope, {
        kind: NoticeReadOnly,
        planName: plan.name,
        entitlement: state,
        reason:
          "Masa tenggang langganan sudah lewat, jadi workspace sekarang mode baca saja: " +
          "membaca dan mengekspor data tetap jalan, tapi perubahan dan tugas baru ditolak " +
          "sampai tagihannya dibayar. Tidak ada data yang dihapus.",
        threshold: 0,
        used: 0,
        allowance: 0,
      });
    }
  }

  if (!changed) return;
  await s.deps.repository.setSubscription(scope, subscription);
}

/**
 * Issues the invoice for the period that starts when the current one ends, if
 * there is not one already.
 *
 * A trial is not renewable: it has no price, and re-issuing it would be a free
 * month every month. So a workspace whose package in force is still the trial
 * gets no invoice at all.
 */
async function ensureUpcomingInvoice(
  s: BillingService,
  scope: Scope,
  subscription: Subscription,
  plan: Plan,
  result: SweepResult,
): Promise<Invoice | null> {
  const next = await planForNextPeriod(s, subscription, plan);

  const start = subscription.currentPeriodEnd;
  try {
    return await s.deps.repository.invoiceForPeriod(
      scope,
      subscription.id,
      KindSubscription,
      start,
    );
  } catch {
    // No invoice stands for the period, so one is issued below.
  }

  if (next.isTrial || next.monthlyPriceIDR <= 0) {
    // Nothing to sell yet. The subscription still moves past due — the period
    // did end — and the notice above says what to do about it.
    return null;
  }

  const days = next.periodDays > 0 ? next.periodDays : 30;
  const end = new Date(start.getTime());
  end.setUTCDate(end.getUTCDate() + days);

  const invoice = await s.deps.repository.createInvoice(scope, {
    id: "",
    workspaceId: scope.workspaceId,
    subscriptionId: subscription.id,
    kind: KindSubscription,
    provider: s.gatewayName(),
    providerReference: "",
    amountIDR: next.monthlyPriceIDR,
    tokenAmount: 0,
    status: StatusPending,
    periodStart: start,
    periodEnd: end,
    instructions: "",
    attempts: 0,
    lastError: "",
    expiresAt: null,
    paidAt: null,
    createdAt: s.now(),
  });

  result.invoicesCreated++;
  await s.notify(scope, {
    kind: NoticePastDue,
    planName: next.name,
    reason: `Tagihan ${next.name} untuk periode berikutnya sudah terbit: ${formatIdr(next.monthlyPriceIDR)}.`,
    threshold: 0,
    used: 0,
    allowance: 0,
    entitlement: "",
  });

  return charge(s, scope, invoice);
}

/** The package the *next* period is billed at: the one a person chose, or the one in force. */
async function planForNextPeriod(
  s: BillingService,
  subscription: Subscription,
  plan: Plan,
): Promise<Plan> {
  if (subscription.pendingPlanId === NilUUID) {
    return plan;
  }
  return s.deps.repository.planById(subscription.pendingPlanId);
}
