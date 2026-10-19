/**
 * Rupiah and token figures are rendered here rather than in a screen, because
 * they appear in the notices the backend sends: an email cannot call a
 * formatter, and two formatters would be two answers to "how much".
 *
 * Ported 1:1 from internal/modules/billing/service/format.go.
 */

/** Renders rupiah the way an Indonesian reader writes it. */
export function formatIdr(amount: number): string {
  return "Rp " + group(String(Math.trunc(amount)), 3, ".");
}

/** Renders a token count with thousands separators. */
export function formatTokens(tokens: number): string {
  return group(String(Math.trunc(tokens)), 3, ".");
}

/** Renders a moment as a date in Jakarta, which is the calendar day the customers read. */
export function formatDate(at: Date): string {
  const parts = new Intl.DateTimeFormat("en-US", {
    timeZone: "Asia/Jakarta",
    day: "numeric",
    month: "long",
    year: "numeric",
  }).formatToParts(at);

  const get = (type: Intl.DateTimeFormatPartTypes): string =>
    parts.find((part) => part.type === type)?.value ?? "";

  // Go's "2 January 2006": the day without a leading zero, the full month name.
  return `${get("day")} ${get("month")} ${get("year")}`;
}

/** Inserts a separator every `size` digits, from the right. */
export function group(digits: string, size: number, separator: string): string {
  let sign = "";
  if (digits.startsWith("-")) {
    sign = "-";
    digits = digits.slice(1);
  }

  let out = "";
  for (let index = 0; index < digits.length; index++) {
    if (index > 0 && (digits.length - index) % size === 0) {
      out += separator;
    }
    out += digits[index];
  }

  if (out === "") {
    return sign + "0";
  }
  return sign + out;
}
