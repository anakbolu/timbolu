/**
 * What the dashboard says when handing an instruction to the team fails.
 *
 * Most transport or server failures cannot be fixed by the user on the spot, so
 * they collapse into a single retry prompt instead of leaking raw English API
 * errors (`internal server error`, `invalid_block`, etc.) onto the dashboard.
 *
 * Payment and session refusals are the two exceptions because retrying the button
 * can never fix them: a `402` / `no_budget` / `read_only` refusal means the
 * workspace must settle its billing or top up at `/settings/subscription`, and a
 * `401` after token renewal means the session is over and the user must sign in
 * again.
 */

import type { ApiError } from "@/lib/api";

export const GENERIC_TASK_ERROR_MESSAGE = "Tugas belum bisa dikirim. Coba lagi sebentar.";
export const SESSION_EXPIRED_TASK_ERROR_MESSAGE = "Sesi berakhir. Masuk lagi.";
export const SUBSCRIPTION_SETTINGS_PATH = "/settings/subscription";

const BILLING_CODES = new Set(["no_budget", "read_only", "workspace_read_only"]);

function isBillingRefusal(error: ApiError): boolean {
  if (error.status === 402) return true;
  const code = error.code?.trim().toLowerCase();
  return code !== undefined && BILLING_CODES.has(code);
}

/**
 * formatTaskError maps an API refusal on the dashboard task-send path to the
 * Indonesian copy shown below the instruction bar.
 */
export function formatTaskError(error: ApiError): string {
  if (isBillingRefusal(error)) {
    const reason = error.message.trim();
    const exit = `Perpanjang paket di halaman Paket dan tagihan (${SUBSCRIPTION_SETTINGS_PATH}).`;
    if (!reason) return exit;
    const ended = /[.!?]$/.test(reason) ? reason : `${reason}.`;
    return `${ended} ${exit}`;
  }

  if (error.status === 401) {
    return SESSION_EXPIRED_TASK_ERROR_MESSAGE;
  }

  return GENERIC_TASK_ERROR_MESSAGE;
}
