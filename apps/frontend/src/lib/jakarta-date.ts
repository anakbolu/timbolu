/**
 * Jakarta business date and compliance timestamp formatters.
 *
 * Timbolu's financial and compliance deadlines (billing periods, payment
 * due dates, data erasure records) are business dates anchored in Indonesia
 * (WIB / Asia/Jakarta). Formatting them with the browser's local timezone
 * can shift dates by ±1 day depending on where the user or their VPN is located.
 */

export const ASIA_JAKARTA = "Asia/Jakarta";

/**
 * formatBusinessDay formats an ISO timestamp or date string into Indonesian
 * business date format ("1 Desember 2026") locked to Asia/Jakarta.
 * Returns "—" for falsy or invalid values.
 */
export function formatBusinessDay(value: string | null | undefined): string {
  if (!value) {
    return "—";
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return "—";
  }
  return new Intl.DateTimeFormat("id-ID", {
    timeZone: ASIA_JAKARTA,
    day: "numeric",
    month: "long",
    year: "numeric",
  }).format(date);
}

/**
 * formatComplianceTimestamp formats an instant into full Indonesian date and
 * time locked to Asia/Jakarta, including the "WIB" time zone marker.
 * Example: "1 Desember 2026, 14.30.00 WIB"
 */
export function formatComplianceTimestamp(value: string | Date | null | undefined): string {
  if (!value) {
    return "—";
  }
  const date = typeof value === "string" ? new Date(value) : value;
  if (Number.isNaN(date.getTime())) {
    return "—";
  }

  const formatted = new Intl.DateTimeFormat("id-ID", {
    timeZone: ASIA_JAKARTA,
    day: "numeric",
    month: "long",
    year: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  }).format(date);

  return `${formatted} WIB`;
}
