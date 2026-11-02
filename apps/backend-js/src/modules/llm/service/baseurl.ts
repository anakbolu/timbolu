/**
 * Pure base_url validation rules to prevent Server-Side Request Forgery (SSRF)
 * when workspace admins configure custom LLM provider endpoints.
 *
 * Rules:
 *  - Empty string or whitespace-only -> allowed (means "use default deployment endpoint")
 *  - Must be parseable by URL constructor
 *  - Scheme must be https:, or http: when process.env.NODE_ENV !== "production" (or options.allowHttp)
 *  - Max length bounded (e.g. 2048 chars)
 *  - Reject hostnames: localhost, *.localhost, *.local, *.internal,
 *    metadata.google.internal, or dotless hostnames that are not IP literals.
 *  - Reject IP literals (IPv4, IPv6, IPv4-mapped):
 *    - loopback (127.0.0.0/8, ::1)
 *    - RFC 1918 private networks (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16)
 *    - link-local (169.254.0.0/16, fe80::/10)
 *    - CGNAT (100.64.0.0/10)
 *    - ULA (fc00::/7)
 *    - 0.0.0.0/8 (including 0.0.0.0)
 *    - multicast (224.0.0.0/4, ff00::/8)
 *    - IPv4-mapped IPv6 (e.g. ::ffff:127.0.0.1)
 *  - For non-IP hostnames: resolve with dns.promises.lookup(host, { all: true }).
 *    If any resolved IP is private/reserved, reject. If resolution fails, reject.
 *    Lookup is injectable for offline / deterministic testing.
 */

import { promises as dnsPromises } from "node:dns";
import { isIP } from "node:net";

export const MaxBaseUrlLength = 2048;

export type DnsLookupFn = (
  hostname: string,
  options: { all: true },
) => Promise<Array<{ address: string; family: number }>>;

export interface CheckBaseUrlOptions {
  allowHttp?: boolean;
  lookup?: DnsLookupFn;
}

export type CheckBaseUrlResult =
  | { ok: true; value: string }
  | { ok: false; reason: string };

/**
 * Checks whether an IPv4 address (as 4 octets) belongs to private/reserved ranges.
 */
function isPrivateOrReservedIPv4(octets: number[]): boolean {
  const [b0, b1] = octets;

  // 0.0.0.0/8 (Current network)
  if (b0 === 0) return true;

  // 10.0.0.0/8 (RFC 1918)
  if (b0 === 10) return true;

  // 100.64.0.0/10 (Shared Address Space / CGNAT: 100.64.0.0 - 100.127.255.255)
  if (b0 === 100 && b1 >= 64 && b1 <= 127) return true;

  // 127.0.0.0/8 (Loopback)
  if (b0 === 127) return true;

  // 169.254.0.0/16 (Link-Local)
  if (b0 === 169 && b1 === 254) return true;

  // 172.16.0.0/12 (RFC 1918: 172.16.0.0 - 172.31.255.255)
  if (b0 === 172 && b1 >= 16 && b1 <= 31) return true;

  // 192.168.0.0/16 (RFC 1918)
  if (b0 === 192 && b1 === 168) return true;

  // 224.0.0.0/4 (Multicast: 224.0.0.0 - 239.255.255.255)
  if (b0 >= 224 && b0 <= 239) return true;

  // 240.0.0.0/4 (Reserved for future use: 240.0.0.0 - 255.255.255.254) & 255.255.255.255 (Broadcast)
  if (b0 >= 240) return true;

  return false;
}

/**
 * Parses IPv4 string to 4 numbers.
 */
function parseIPv4(ip: string): number[] | null {
  const parts = ip.split(".");
  if (parts.length !== 4) return null;
  const nums: number[] = [];
  for (const part of parts) {
    if (!/^\d+$/.test(part)) return null;
    const n = Number.parseInt(part, 10);
    if (n < 0 || n > 255) return null;
    nums.push(n);
  }
  return nums;
}

/**
 * Normalizes and checks if an IPv6 address string is private, loopback, link-local,
 * multicast, ULA, or IPv4-mapped private.
 */
function isPrivateOrReservedIPv6(ip: string): boolean {
  const cleaned = ip.toLowerCase();

  // Loopback ::1
  if (cleaned === "::1" || cleaned === "0000:0000:0000:0000:0000:0000:0000:0001") {
    return true;
  }
  // Unspecified ::
  if (cleaned === "::" || cleaned === "0000:0000:0000:0000:0000:0000:0000:0000") {
    return true;
  }

  // IPv4-mapped IPv6: ::ffff:127.0.0.1 or ::ffff:7f00:1
  const ipv4MappedMatch = cleaned.match(/^::ffff:(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})$/);
  if (ipv4MappedMatch) {
    const octets = parseIPv4(ipv4MappedMatch[1]);
    if (octets && isPrivateOrReservedIPv4(octets)) return true;
  }

  // Expand IPv6 to 8 16-bit hextets
  const hextets = expandIPv6(cleaned);
  if (!hextets) return true; // Malformed -> consider unsafe

  const [h0] = hextets;

  // ::ffff:x.x.x.x in pure hex format
  if (
    hextets[0] === 0 &&
    hextets[1] === 0 &&
    hextets[2] === 0 &&
    hextets[3] === 0 &&
    hextets[4] === 0 &&
    hextets[5] === 0xffff
  ) {
    const b0 = (hextets[6] >> 8) & 0xff;
    const b1 = hextets[6] & 0xff;
    const b2 = (hextets[7] >> 8) & 0xff;
    const b3 = hextets[7] & 0xff;
    if (isPrivateOrReservedIPv4([b0, b1, b2, b3])) return true;
  }

  // Loopback check again on expanded hextets
  if (
    hextets[0] === 0 &&
    hextets[1] === 0 &&
    hextets[2] === 0 &&
    hextets[3] === 0 &&
    hextets[4] === 0 &&
    hextets[5] === 0 &&
    hextets[6] === 0 &&
    hextets[7] === 1
  ) {
    return true;
  }

  // Unspecified ::
  if (hextets.every((h) => h === 0)) return true;

  // ULA: fc00::/7 (fc00:: - fdff:: -> top 7 bits 1111110 -> 0xfc or 0xfd)
  if ((h0 & 0xfe00) === 0xfc00) return true;

  // Link-Local: fe80::/10 (fe80:: - febf:: -> top 10 bits 1111111010 -> 0xfe80)
  if ((h0 & 0xffc0) === 0xfe80) return true;

  // Multicast: ff00::/8 (ff00:: - ffff::)
  if ((h0 & 0xff00) === 0xff00) return true;

  return false;
}

/**
 * Expands an IPv6 address string into an array of 8 16-bit integer hextets.
 */
function expandIPv6(ip: string): number[] | null {
  // Check for IPv4 embedded at the end
  let partsStr = ip;
  let embeddedV4: number[] | null = null;
  const lastColon = ip.lastIndexOf(":");
  if (lastColon !== -1) {
    const tail = ip.slice(lastColon + 1);
    if (tail.includes(".")) {
      embeddedV4 = parseIPv4(tail);
      if (!embeddedV4) return null;
      partsStr = ip.slice(0, lastColon);
    }
  }

  const parts = partsStr.split("::");
  if (parts.length > 2) return null;

  const left = parts[0] ? parts[0].split(":").filter(Boolean) : [];
  const right = parts.length === 2 && parts[1] ? parts[1].split(":").filter(Boolean) : [];

  const neededHextets = 8 - (embeddedV4 ? 2 : 0);
  const totalGiven = left.length + right.length;

  if (parts.length === 1 && totalGiven !== neededHextets) return null;
  if (parts.length === 2 && totalGiven >= neededHextets) return null;

  const missing = neededHextets - totalGiven;
  const zeros = Array(missing).fill("0");

  const fullHextetStrs = parts.length === 2 ? [...left, ...zeros, ...right] : left;

  const hextets: number[] = [];
  for (const str of fullHextetStrs) {
    const val = Number.parseInt(str, 16);
    if (Number.isNaN(val) || val < 0 || val > 0xffff) return null;
    hextets.push(val);
  }

  if (embeddedV4) {
    hextets.push((embeddedV4[0] << 8) | embeddedV4[1]);
    hextets.push((embeddedV4[2] << 8) | embeddedV4[3]);
  }

  return hextets.length === 8 ? hextets : null;
}

/**
 * Checks whether an IP string (v4 or v6) is private or reserved.
 */
export function isPrivateOrReservedIP(ip: string): boolean {
  const family = isIP(ip);
  if (family === 4) {
    const octets = parseIPv4(ip);
    return octets ? isPrivateOrReservedIPv4(octets) : true;
  }
  if (family === 6) {
    return isPrivateOrReservedIPv6(ip);
  }
  return false;
}

/**
 * Checks whether a hostname is prohibited without needing DNS resolution.
 */
function isProhibitedHostname(hostname: string): boolean {
  const host = hostname.toLowerCase().replace(/\.$/, ""); // strip trailing dot if any

  if (host === "localhost" || host.endsWith(".localhost")) return true;
  if (host === "local" || host.endsWith(".local")) return true;
  if (host === "internal" || host.endsWith(".internal")) return true;
  if (host === "metadata.google.internal") return true;

  // Dotless hostnames that are not IP literals (e.g. "internal-svc", "myhost")
  if (!host.includes(".") && isIP(host) === 0) return true;

  return false;
}


/**
 * Validates a base_url string.
 * Returns { ok: true, value } where value is trimmed, or { ok: false, reason }.
 */
export async function checkBaseUrl(
  raw: string,
  options?: CheckBaseUrlOptions,
): Promise<CheckBaseUrlResult> {
  const trimmed = raw.trim();
  if (trimmed === "") {
    return { ok: true, value: "" };
  }

  if (trimmed.length > MaxBaseUrlLength) {
    return { ok: false, reason: `base_url exceeds max length of ${MaxBaseUrlLength} characters` };
  }

  let parsed: URL;
  try {
    parsed = new URL(trimmed);
  } catch {
    return { ok: false, reason: "base_url is not a valid URL" };
  }

  const allowHttp =
    options?.allowHttp ??
    (typeof process !== "undefined" && process.env?.NODE_ENV !== "production");

  if (parsed.protocol === "https:") {
    // ok
  } else if (parsed.protocol === "http:") {
    if (!allowHttp) {
      return { ok: false, reason: "http: scheme is not allowed in production" };
    }
  } else {
    return { ok: false, reason: `unsupported scheme ${parsed.protocol} (only https: is allowed)` };
  }

  const rawHost =
    parsed.hostname.startsWith("[") && parsed.hostname.endsWith("]")
      ? parsed.hostname.slice(1, -1)
      : parsed.hostname;

  if (isProhibitedHostname(rawHost)) {
    return { ok: false, reason: `host '${parsed.hostname}' is a reserved internal or private name` };
  }

  const ipFamily = isIP(rawHost);
  if (ipFamily !== 0) {
    if (isPrivateOrReservedIP(rawHost)) {
      return { ok: false, reason: `ip address '${rawHost}' is in a private or reserved range` };
    }
    return { ok: true, value: trimmed };
  }

  // Non-IP hostname: resolve with dns
  const lookupFn: DnsLookupFn =
    options?.lookup ??
    ((host, opts) => dnsPromises.lookup(host, opts));

  let resolved: Array<{ address: string; family: number }>;
  try {
    resolved = await lookupFn(rawHost, { all: true });
    if (!resolved || resolved.length === 0) {
      return { ok: false, reason: `could not resolve host '${rawHost}'` };
    }
  } catch {
    return { ok: false, reason: `could not resolve host '${rawHost}'` };
  }

  for (const record of resolved) {
    if (isPrivateOrReservedIP(record.address)) {
      return {
        ok: false,
        reason: `host '${rawHost}' resolved to private or reserved IP address '${record.address}'`,
      };
    }
  }

  return { ok: true, value: trimmed };
}

/**
 * Asserts that a base_url is safe, throwing an Error with message if invalid.
 */
export async function assertSafeBaseUrl(
  raw: string,
  options?: CheckBaseUrlOptions,
): Promise<string> {
  const result = await checkBaseUrl(raw, options);
  if (!result.ok) {
    throw new Error(result.reason);
  }
  return result.value;
}
