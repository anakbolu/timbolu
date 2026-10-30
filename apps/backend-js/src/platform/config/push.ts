/**
 * The push kill switch.
 *
 * A push leaves the process for a third party on behalf of every account with a
 * registered phone, so it is a decision a deployment makes deliberately rather
 * than one that follows from a row existing in `device_tokens`. Only the
 * literal `true` — after trimming and ignoring case — turns it on; `1`, `yes`,
 * `on`, an empty value and an unset variable all keep the sender away. The
 * strictness is on purpose: a value nobody documented must fail closed.
 */
export function parsePushEnabled(raw: string | null | undefined): boolean {
  return (raw ?? "").trim().toLowerCase() === "true";
}