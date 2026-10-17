/**
 * Process and request logging.
 *
 * Ported from packages/platform/logger. Every line carries request_id,
 * trace_id and workspace_id, and secrets are redacted before they are written.
 */

import { pino, type Logger as PinoLogger } from "pino";

export type Logger = PinoLogger;

const SECRET_QUERY_KEYS = [
  "token",
  "access_token",
  "refresh_token",
  "api_key",
  "apikey",
  "key",
  "secret",
  "password",
  "signature",
  "code",
  "state",
];

const SECRET_HEADERS = [
  "authorization",
  "cookie",
  "set-cookie",
  "x-api-key",
  "proxy-authorization",
  "x-workspace-token",
];

/** Replaces the value of a secret query parameter with `redacted`. */
export function redactQuery(raw: string): string {
  if (!raw) return "";
  const params = new URLSearchParams(raw);
  let touched = false;
  for (const key of Array.from(params.keys())) {
    if (SECRET_QUERY_KEYS.includes(key.toLowerCase())) {
      params.set(key, "redacted");
      touched = true;
    }
  }
  return touched ? params.toString() : raw;
}

/** Replaces the value of a secret header with `redacted`. */
export function redactHeaders(headers: Record<string, string | string[] | undefined>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [key, value] of Object.entries(headers)) {
    if (value === undefined) continue;
    const joined = Array.isArray(value) ? value.join(", ") : String(value);
    out[key] = SECRET_HEADERS.includes(key.toLowerCase()) ? "redacted" : joined;
  }
  return out;
}

export interface LoggerConfig {
  level?: string;
  format?: "json" | "console";
}

/** Builds the process logger. */
export function createLogger(cfg: LoggerConfig = {}): Logger {
  const level = (cfg.level || "info").toLowerCase();
  // `console` formatting needs pino-pretty, which is not a dependency of this
  // project. Rather than fail to start over a log format, both values emit JSON:
  // a deployment that wants colour pipes the output through `bunx pino-pretty`.
  return pino({ level, base: undefined, timestamp: pino.stdTimeFunctions.isoTime });
}

/** A logger that writes nothing, for tests and unconfigured modules. */
export const nopLogger: Logger = pino({ level: "silent" });

/** The request-scoped logger, carrying the correlation identifiers. */
export function requestLogger(
  base: Logger,
  fields: { requestId: string; traceId: string; workspaceId?: string },
): Logger {
  const bound = base.child({ request_id: fields.requestId, trace_id: fields.traceId });
  return fields.workspaceId ? bound.child({ workspace_id: fields.workspaceId }) : bound;
}
