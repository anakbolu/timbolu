import type { BillingPlan, TopUpPackage } from "@/lib/api";

/** formatRupiah renders the rupiah the API reports. */
export function formatRupiah(amount: number): string {
  const safeAmount = Number.isFinite(amount) ? amount : 0;
  return new Intl.NumberFormat("id-ID", {
    style: "currency",
    currency: "IDR",
    maximumFractionDigits: 0,
  }).format(safeAmount);
}

/** formatTokens renders a token count with thousands separators. */
export function formatTokens(tokens: number): string {
  const safeTokens = Number.isFinite(tokens) ? tokens : 0;
  return new Intl.NumberFormat("id-ID").format(safeTokens);
}

export type TopUpConfirmation = {
  kind: "topup";
  title: string;
  packageName: string;
  tokenAmount: number;
  tokenAmountFormatted: string;
  priceIdr: number;
  priceFormatted: string;
  periodDescription: string;
  description: string;
  actionLabel: string;
  cancelLabel: string;
};

export type PlanConfirmation = {
  kind: "plan";
  title: string;
  planName: string;
  priceIdr: number;
  priceFormatted: string;
  periodDays: number;
  periodFormatted: string;
  effectiveDescription: string;
  description: string;
  actionLabel: string;
  cancelLabel: string;
};

export type BillingConfirmation = TopUpConfirmation | PlanConfirmation;

export type PendingBillingAction =
  | { kind: "topup"; pack: TopUpPackage }
  | { kind: "plan"; plan: BillingPlan };

/**
 * buildTopUpConfirmation formats the confirmation details before buying a top-up pack.
 *
 * Rules:
 * - actionLabel uses a verb and formatted price (e.g. "Beli 500 ribu token · Rp 75.000")
 * - token count is explicitly formatted and mentioned
 * - period is described (current period only, does not roll over)
 * - labels and descriptions are never empty
 */
export function buildTopUpConfirmation(pack: Partial<TopUpPackage>): TopUpConfirmation {
  const rawTokens = typeof pack.token_amount === "number" && Number.isFinite(pack.token_amount) ? pack.token_amount : 0;
  const rawPrice = typeof pack.price_idr === "number" && Number.isFinite(pack.price_idr) ? pack.price_idr : 0;
  const tokenAmountFormatted = `${formatTokens(rawTokens)} token`;
  const priceFormatted = formatRupiah(rawPrice);

  const rawName = pack.name?.trim();
  const packageName = rawName || (rawTokens > 0 ? tokenAmountFormatted : "Paket tambahan");

  const title = "Konfirmasi Pembelian Top-Up";
  const periodDescription = "Periode berjalan (tidak terbawa ke periode berikutnya)";
  const description = `Beli ${packageName} (${tokenAmountFormatted}) seharga ${priceFormatted}. Token tambahan dipakai di periode ini dan tidak terbawa ke periode berikutnya.`;
  const actionLabel = `Beli ${packageName} · ${priceFormatted}`;
  const cancelLabel = "Batal";

  return {
    kind: "topup",
    title,
    packageName,
    tokenAmount: rawTokens,
    tokenAmountFormatted,
    priceIdr: rawPrice,
    priceFormatted,
    periodDescription,
    description,
    actionLabel,
    cancelLabel,
  };
}

/**
 * buildPlanConfirmation formats the confirmation details before switching billing plans.
 *
 * Rules:
 * - actionLabel uses a verb and formatted price (e.g. "Pilih Paket Keluarga · Rp 149.000")
 * - plan period length is stated
 * - effect on the current period is stated consistently with choose() notices:
 *   active immediately during trial, or effective next period after the invoice is paid
 * - labels and descriptions are never empty
 */
export function buildPlanConfirmation(
  plan: Partial<BillingPlan>,
  subscriptionStatus?: string,
): PlanConfirmation {
  const rawPrice = typeof plan.monthly_price_idr === "number" && Number.isFinite(plan.monthly_price_idr)
    ? plan.monthly_price_idr
    : 0;
  const rawPeriodDays = typeof plan.period_days === "number" && Number.isFinite(plan.period_days) && plan.period_days > 0
    ? plan.period_days
    : 30;

  const rawName = plan.name?.trim();
  const planName = rawName || "Paket langganan";
  const priceFormatted = formatRupiah(rawPrice);
  const periodFormatted = `${rawPeriodDays} hari`;

  const isTrial = subscriptionStatus === "trial";
  const effectiveDescription = isTrial
    ? "Paket langsung berlaku. Masa coba berjalan dengan paket baru sampai periodenya habis."
    : "Paket berlaku mulai periode berikutnya, setelah tagihan dibayar.";

  const title = "Konfirmasi Pilih Paket";
  const description = `Pilih ${planName} seharga ${priceFormatted} per ${periodFormatted}. ${effectiveDescription}`;
  const actionLabel = `Pilih ${planName} · ${priceFormatted}`;
  const cancelLabel = "Batal";

  return {
    kind: "plan",
    title,
    planName,
    priceIdr: rawPrice,
    priceFormatted,
    periodDays: rawPeriodDays,
    periodFormatted,
    effectiveDescription,
    description,
    actionLabel,
    cancelLabel,
  };
}

/**
 * buildBillingConfirmation resolves either a top-up or plan confirmation from a pending action.
 */
export function buildBillingConfirmation(
  pending: PendingBillingAction,
  subscriptionStatus?: string,
): BillingConfirmation {
  if (pending.kind === "topup") {
    return buildTopUpConfirmation(pending.pack);
  }
  return buildPlanConfirmation(pending.plan, subscriptionStatus);
}
