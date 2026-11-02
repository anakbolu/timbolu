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

function parseIPv4(ip: string): number[] | null {
  const parts = ip.split(".");
  if (parts.length !== 4) return null;
  const nums = parts.map((p) => (/^\d+$/.test(p) ? Number.parseInt(p, 10) : -1));
  return nums.every((n) => n >= 0 && n <= 255) ? nums : null;
}

function isPrivateOrReservedIPv4(octets: number[]): boolean {
  const [b0, b1] = octets;
  return (
    b0 === 0 || // Current network
    b0 === 10 || // RFC 1918 private
    (b0 === 100 && b1 >= 64 && b1 <= 127) || // CGNAT (100.64.0.0/10)
    b0 === 127 || // Loopback
    (b0 === 169 && b1 === 254) || // Link-local / Cloud metadata (169.254.0.0/16)
    (b0 === 172 && b1 >= 16 && b1 <= 31) || // RFC 1918 private
    (b0 === 192 && b1 === 168) || // RFC 1918 private
    b0 >= 224 // Multicast & reserved (224.0.0.0/4, 240.0.0.0/4)
  );
}

function isPrivateOrReservedIPv6(ip: string): boolean {
  const lower = ip.toLowerCase();
  if (lower === "::1" || lower === "::") return true;
  if (lower.startsWith("::ffff:")) {
    const tail = lower.slice(7);
    if (/^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(tail)) {
      const v4 = parseIPv4(tail);
      return v4 ? isPrivateOrReservedIPv4(v4) : true;
    }
    const hexParts = tail.split(":");
    if (hexParts.length === 2) {
      const h0 = Number.parseInt(hexParts[0], 16);
      const h1 = Number.parseInt(hexParts[1], 16);
      if (!Number.isNaN(h0) && !Number.isNaN(h1)) {
        return isPrivateOrReservedIPv4([(h0 >> 8) & 0xff, h0 & 0xff, (h1 >> 8) & 0xff, h1 & 0xff]);
      }
    }
    return true;
  }
  const normalized = lower.replace(/^0+/, "");
  if (/^f[cd][0-9a-f]{2}:/i.test(normalized) || /^f[cd]::/i.test(normalized)) return true; // ULA fc00::/7
  if (/^fe[89ab][0-9a-f]:/i.test(normalized) || /^fe[89ab]::/i.test(normalized)) return true; // Link-local fe80::/10
  if (/^ff[0-9a-f]{2}:/i.test(normalized) || /^ff::/i.test(normalized)) return true; // Multicast ff00::/8
  if (normalized.startsWith("0:") || normalized.includes("::")) {
    const segments = lower.split(":").filter(Boolean);
    const last = segments[segments.length - 1];
    if (last === "1" && segments.slice(0, -1).every((s) => /^0+$/.test(s))) return true;
    if (segments.every((s) => /^0+$/.test(s))) return true;
  }
  return false;
}
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

function isProhibitedHostname(hostname: string): boolean {
  const host = hostname.toLowerCase().replace(/\.$/, "");
  if (host === "localhost" || host.endsWith(".localhost")) return true;
  if (host === "local" || host.endsWith(".local")) return true;
  if (host === "internal" || host.endsWith(".internal")) return true;
  // Dotless hostnames that are not IP literals (e.g. internal-svc)
  return !host.includes(".") && isIP(host) === 0;
}

export async function checkBaseUrl(
  raw: string,
  options?: CheckBaseUrlOptions,
): Promise<CheckBaseUrlResult> {
  const trimmed = raw.trim();
  if (trimmed === "") return { ok: true, value: "" };
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

  if (parsed.protocol === "http:") {
    if (!allowHttp) return { ok: false, reason: "http: scheme is not allowed in production" };
  } else if (parsed.protocol !== "https:") {
    return { ok: false, reason: `unsupported scheme ${parsed.protocol} (only https: is allowed)` };
  }

  const rawHost =
    parsed.hostname.startsWith("[") && parsed.hostname.endsWith("]")
      ? parsed.hostname.slice(1, -1)
      : parsed.hostname;

  if (isProhibitedHostname(rawHost)) {
    return { ok: false, reason: `host '${parsed.hostname}' is a reserved internal or private name` };
  }

  if (isIP(rawHost) !== 0) {
    if (isPrivateOrReservedIP(rawHost)) {
      return { ok: false, reason: `ip address '${rawHost}' is in a private or reserved range` };
    }
    return { ok: true, value: trimmed };
  }

  const lookupFn: DnsLookupFn = options?.lookup ?? ((h, o) => dnsPromises.lookup(h, o));
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
