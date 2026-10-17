/**
 * The payment half: what a workspace owes, how it pays, and what a provider's
 * notification does.
 *
 * Nothing here is a mandate. The first customers pay by bank transfer or a
 * virtual account, and the honest model of that is an invoice per period that a
 * person pays when it arrives — not a card on file. "Recurring" therefore means
 * "a new invoice is issued when each period ends", and the sweep issues it.
 *
 * Ported 1:1 from internal/modules/billing/service/payment.go.
 */

import {
  EntitlementActive,
  ErrInvalidInput,
  ErrInvoiceSettled,
  ErrNoGateway,
  ErrUnverifiable,
  KindTopup,
  NilUUID,
  StatusActive,
  StatusFailed,
  StatusPaid,
  StatusPending,
  periodAllowance,
  invoiceOutstanding,
  wrap,
  type Invoice,
  type Scope,
} from "../domain/billing.ts";
import {
  NoticePaid,
  NoticeTopUp,
  type Customer,
  type Delivery,
  type Payment,
  type TopUpRequest,
} from "../domain/service.ts";
import type { BillingService } from "./service.ts";
import { formatDate, formatIdr, formatTokens } from "./format.ts";

/**
 * Asks the provider for a payment link for one invoice.
 *
 * A deployment with no provider configured says so rather than pretending: the
 * invoice exists, and the screen shows what to transfer. Retrying is deliberate
 * — the attempts are counted and the failure is kept.
 */
export async function pay(
  s: BillingService,
  scope: Scope,
  invoiceId: string,
): Promise<Invoice> {
  if (!scope.workspaceId || scope.workspaceId === NilUUID) {
    throw wrap(ErrInvalidInput, "workspace is required");
  }

  const invoice = await s.deps.repository.invoice(scope, invoiceId);
  if (!invoiceOutstanding(invoice)) {
    throw wrap(ErrInvoiceSettled, `tagihan ini sudah ${invoice.status}`);
  }

  return charge(s, scope, invoice);
}

/**
 * Asks the provider for a payment and records what came back.
 *
 * It never fails the caller over the provider: an invoice that could not be
 * given a link is still an invoice a workspace can pay by transfer.
 */
export async function charge(
  s: BillingService,
  scope: Scope,
  invoice: Invoice,
): Promise<Invoice> {
  if (!s.deps.gateway) {
    return invoice;
  }

  let customer: Customer = {
    workspaceId: scope.workspaceId,
    workspace: "",
    email: "",
    name: "",
  };
  if (s.deps.customers) {
    try {
      customer = await s.deps.customers.customer(scope);
      customer.workspaceId = scope.workspaceId;
    } catch (err) {
      s.deps.logger.warn({ err }, "could not read who the invoice is for");
    }
  }

  invoice.provider = s.deps.gateway.name();
  invoice.attempts++;

  let result;
  try {
    result = await s.deps.gateway.charge(invoice, customer);
  } catch (err) {
    invoice.lastError = err instanceof Error ? err.message : String(err);
    const stored = await s.deps.repository.setInvoicePayment(scope, invoice);
    s.deps.logger.warn({ invoice: invoice.id, err }, "could not create a payment link");
    return stored;
  }

  invoice.providerReference = result.reference;
  invoice.instructions = result.instructions;
  invoice.lastError = "";
  if (result.expiresAt && !Number.isNaN(result.expiresAt.getTime())) {
    invoice.expiresAt = result.expiresAt;
  }

  return s.deps.repository.setInvoicePayment(scope, invoice);
}

/**
 * Buys tokens inside the period the workspace is in.
 *
 * The tokens land in the period they were bought in and stay there: carrying the
 * remainder forward would mean a workspace that bought early is served by the
 * purchase instead of by its package.
 */
export async function buyTopUp(
  s: BillingService,
  scope: Scope,
  request: TopUpRequest,
): Promise<Invoice> {
  if (!scope.workspaceId || scope.workspaceId === NilUUID) {
    throw wrap(ErrInvalidInput, "workspace is required");
  }

  const key = request.packageKey ?? "";
  if (key === "") {
    throw wrap(ErrInvalidInput, "pilih paket tambahan yang dituju");
  }

  const pack = await s.deps.repository.topUp(scope, key);
  const { subscription, plan } = await s.live(scope.workspaceId);

  const now = s.now();
  const period = await s.deps.repository.period(scope, subscription, plan, now);

  const invoice = await s.deps.repository.createInvoice(scope, {
    id: "",
    workspaceId: scope.workspaceId,
    subscriptionId: subscription.id,
    kind: KindTopup,
    provider: s.gatewayName(),
    providerReference: "",
    amountIdr: pack.priceIdr,
    tokenAmount: pack.tokenAmount,
    status: StatusPending,
    periodStart: period.periodStart,
    periodEnd: period.periodEnd,
    instructions: "",
    attempts: 0,
    lastError: "",
    expiresAt: null,
    paidAt: null,
    createdAt: now,
  });

  return charge(s, scope, invoice);
}

/**
 * Applies a verified payment notification.
 *
 * It is the only place a period is extended, and it is guarded twice: the
 * delivery is verified by the provider's own signature, and the settlement is
 * written once by the invoice's own `status = 'pending'` guard. A webhook that
 * arrives twice therefore extends exactly one period.
 */
export async function handlePayment(s: BillingService, delivery: Delivery): Promise<void> {
  if (!s.deps.gateway) {
    throw wrap(ErrNoGateway, "no payment provider is configured");
  }
  if (delivery.provider !== "" && delivery.provider !== s.deps.gateway.name()) {
    // The path named a provider this deployment does not speak, so nothing can
    // verify the payload. Refusing is the only safe answer.
    throw wrap(ErrNoGateway, delivery.provider);
  }

  let payment: Payment;
  try {
    payment = await s.deps.gateway.verify(delivery);
  } catch (err) {
    // A delivery that cannot be trusted is the caller's payload, not this
    // deployment's failure — and the provider must not be told to retry
    // something that can never verify.
    throw wrap(ErrUnverifiable, err instanceof Error ? err.message : String(err));
  }

  // A provider names one reference and nothing else, so the routing row is what
  // says whose invoice it is. That read is unscoped because it has to be;
  // everything after it runs inside that workspace's scope.
  const route = await s.deps.repository.paymentRoute(s.deps.gateway.name(), payment.reference);

  const scope: Scope = { workspaceId: route.workspaceId };
  const invoice = await s.deps.repository.invoice(scope, route.invoiceId);

  if (payment.paid) {
    return applyPayment(s, scope, invoice, payment);
  }
  if (payment.expired) {
    await s.deps.repository.expireInvoice(scope, invoice.id);
    return;
  }

  const settled: Invoice = { ...invoice, status: StatusFailed, paidAt: null };
  await s.deps.repository.settleInvoice(scope, settled);
}

/** Settles an invoice and carries out what it bought. */
async function applyPayment(
  s: BillingService,
  scope: Scope,
  invoice: Invoice,
  payment: Payment,
): Promise<void> {
  if (payment.amountIdr > 0 && payment.amountIdr < invoice.amountIdr) {
    // Paying less than the invoice asks for is not a payment of it. The invoice
    // is left outstanding and the reason is recorded.
    s.deps.logger.warn(
      { invoice: invoice.id, paid: payment.amountIdr, expected: invoice.amountIdr },
      "a payment was smaller than its invoice",
    );
    throw wrap(
      ErrInvalidInput,
      `pembayaran ${formatIdr(payment.amountIdr)} lebih kecil dari tagihan ${formatIdr(invoice.amountIdr)}`,
    );
  }

  const settled: Invoice = {
    ...invoice,
    status: StatusPaid,
    paidAt: payment.paidAt ?? s.now(),
  };

  const stored = await s.deps.repository.settleInvoice(scope, settled);
  if (!stored.fresh) {
    // Already settled: a replayed delivery, and the period was extended the
    // first time. This is the answer that makes a webhook idempotent.
    s.deps.logger.info(
      { invoice: invoice.id },
      "a payment arrived for an invoice that was already settled",
    );
    return;
  }

  if (stored.invoice.kind === KindTopup) {
    return applyTopUp(s, scope, stored.invoice);
  }
  return applySubscriptionPayment(s, scope, stored.invoice);
}

/** Puts bought tokens into the period they were bought in. */
async function applyTopUp(s: BillingService, scope: Scope, invoice: Invoice): Promise<void> {
  const period = await s.deps.repository.addTopUp(scope, invoice.periodStart, invoice.tokenAmount);

  await s.notify(scope, {
    kind: NoticeTopUp,
    used: period.tokensUsed,
    allowance: periodAllowance(period),
    reason:
      `Paket tambahan ${formatTokens(invoice.tokenAmount)} token sudah masuk. ` +
      `Kuota periode ini sekarang ${formatTokens(period.tokensUsed)} dari ${formatTokens(periodAllowance(period))} token.`,
    planName: "",
    threshold: 0,
    entitlement: "",
  });
}

/**
 * Extends the period the invoice was for.
 *
 * The new period is the invoice's own window rather than "now plus a month": a
 * payment that arrives during the grace days must not push the boundary forward,
 * or a workspace that paid late would keep drifting its billing anniversary. The
 * package a person chose while the invoice was outstanding is applied here.
 */
async function applySubscriptionPayment(
  s: BillingService,
  scope: Scope,
  invoice: Invoice,
): Promise<void> {
  const { subscription, plan: currentPlan } = await s.live(scope.workspaceId);
  let plan = currentPlan;

  if (subscription.pendingPlanId !== NilUUID) {
    try {
      const pending = await s.deps.repository.planById(subscription.pendingPlanId);
      subscription.planId = pending.id;
      plan = pending;
    } catch (err) {
      s.deps.logger.warn({ err }, "could not apply the pending package");
    }
    subscription.pendingPlanId = NilUUID;
  }

  subscription.status = StatusActive;
  subscription.currentPeriodStart = invoice.periodStart;
  subscription.currentPeriodEnd = invoice.periodEnd;
  const grace = new Date(invoice.periodEnd.getTime());
  grace.setUTCDate(grace.getUTCDate() + s.cfg.graceDays);
  subscription.graceUntil = grace;
  // The workspace paid, so what it was told about not paying is no longer true —
  // and the next period's notices have to be able to fire.
  subscription.pastDueSince = null;
  subscription.pastDueNotifiedAt = null;
  subscription.readOnlyNotifiedAt = null;

  await s.deps.repository.setSubscription(scope, subscription);

  // The period the workspace is now in has to exist before anything reads its
  // allowance, so it is opened here rather than left for the next check.
  await s.deps.repository.period(scope, subscription, plan, invoice.periodStart);

  // And what the workspace no longer owes is closed.
  await withdrawStaleInvoices(s, scope, invoice.id);

  await s.notify(scope, {
    kind: NoticePaid,
    planName: plan.name,
    entitlement: EntitlementActive,
    reason: `Pembayaran diterima. Paket ${plan.name} aktif sampai ${formatDate(invoice.periodEnd)}.`,
    threshold: 0,
    used: 0,
    allowance: 0,
  });
}

/**
 * Expires the subscription invoices a workspace no longer owes.
 *
 * It runs after a payment and never touches the one that was just settled, nor a
 * top-up: what a person bought on top of a package is not affected by paying for
 * the package.
 */
async function withdrawStaleInvoices(
  s: BillingService,
  scope: Scope,
  paidId: string,
): Promise<void> {
  let outstanding: Invoice[];
  try {
    outstanding = await s.deps.repository.outstanding(scope);
  } catch (err) {
    s.deps.logger.warn({ err }, "could not read the outstanding invoices after a payment");
    return;
  }

  for (const invoice of outstanding) {
    if (invoice.id === paidId || invoice.kind !== "subscription") {
      continue;
    }
    try {
      await s.deps.repository.expireInvoice(scope, invoice.id);
    } catch (err) {
      s.deps.logger.warn(
        { invoice: invoice.id, err },
        "could not withdraw an invoice the payment superseded",
      );
    }
  }
}
