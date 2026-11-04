/**
 * Shared currency and token formatters for workspace billing and usage screens.
 *
 * Billing and metered usage intentionally use two distinct rupiah formatters
 * (`formatRupiah` and `formatRupiahCost`) rather than one configurable function
 * because they belong to different financial domains with different precision rules.
 */

/**
 * formatRupiah renders a whole-rupiah IDR amount for billing screens.
 *
 * Plan prices, invoice amounts, `outstanding_idr`, and daily cost limits are
 * billed in whole rupiah, so `maximumFractionDigits: 0` avoids noisy `,00` suffixes.
 */
export function formatRupiah(amount: number): string {
  return new Intl.NumberFormat("id-ID", {
    style: "currency",
    currency: "IDR",
    maximumFractionDigits: 0,
  }).format(amount);
}

/**
 * formatRupiahCost renders a metered usage cost in IDR.
 *
 * Per-agent, per-task, and unrouted usage figures (`cost_idr` and micro-rupiah
 * conversions) can be fractional or below Rp 1.000 (e.g. Rp 812,4 or Rp 0,42).
 * Keeping up to 2 fraction digits below 1.000 prevents small non-zero costs
 * from rounding down to a misleading "Rp 0".
 */
export function formatRupiahCost(amount: number): string {
  return new Intl.NumberFormat("id-ID", {
    style: "currency",
    currency: "IDR",
    maximumFractionDigits: amount < 1000 ? 2 : 0,
  }).format(amount);
}

/** formatTokens renders a token count with Indonesian thousands separators. */
export function formatTokens(tokens: number): string {
  return new Intl.NumberFormat("id-ID").format(tokens);
}
