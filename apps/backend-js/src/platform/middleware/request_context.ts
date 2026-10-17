/**
 * The request context middleware: request_id, workspace_id, trace_id and the
 * request-scoped logger.
 *
 * Ported from apps/backend/internal/platform/middleware/request_context.go. It
 * attaches the three identifiers to one bound logger, stores them on the
 * context, echoes the two the caller can quote, and writes exactly one line per
 * request when it finishes.
 *
 * The header names and the `traceparent` parsing are the Go ones: a value that
 * is not UUID-shaped is discarded rather than logged, so a caller cannot push
 * arbitrary content into the log stream or the response headers.
 */

import type { MiddlewareHandler } from "hono";
import { getConnInfo } from "hono/bun";

import type { AppEnv, AppContext } from "../context.ts";
import { HeaderRequestID, HeaderTraceID, HeaderWorkspaceID, sanitizeId } from "../context.ts";
import { redactHeaders, redactQuery, requestLogger, type Logger } from "../logger.ts";
import { AppError } from "../errors.ts";
import { spanFrom } from "./tracing.ts";

/** The W3C header a caller's trace is propagated in. */
const HeaderTraceparent = "traceparent";

/**
 * The format a `traceparent` must have for its trace id to be adopted. Lowercase
 * hex only, exactly as the Go regexp: version, 32-hex trace id, 16-hex span id,
 * flags.
 */
const TRACEPARENT_FORMAT = /^[0-9a-f]{2}-([0-9a-f]{32})-([0-9a-f]{16})-[0-9a-f]{2}$/;

/**
 * Attaches request_id, workspace_id and trace_id to a request logger and stores
 * both on the context. Downstream handlers read them with the accessors in
 * `context.ts`, so no handler builds its own logger.
 */
export function requestContext(base: Logger): MiddlewareHandler<AppEnv> {
  return async (c, next) => {
    const requestID = sanitizeId(c.req.header(HeaderRequestID)) || crypto.randomUUID();
    const traceID = traceIDFrom(c);
    const workspaceID = sanitizeId(c.req.header(HeaderWorkspaceID));

    const reqLog = requestLogger(base, {
      requestId: requestID,
      traceId: traceID,
      ...(workspaceID ? { workspaceId: workspaceID } : {}),
    });

    c.set("requestId", requestID);
    c.set("traceId", traceID);
    c.set("workspaceId", workspaceID);
    c.set("log", reqLog);

    // The response echoes the identifiers so a user can quote them when
    // reporting a problem.
    c.header(HeaderRequestID, requestID);
    c.header(HeaderTraceID, traceID);

    const start = performance.now();

    let failure: unknown = null;
    try {
      await next();
    } catch (err) {
      failure = err;
    }

    const fields: Record<string, unknown> = {
      method: c.req.method,
      path: c.req.path,
      query: redactQuery(new URL(c.req.url).search.replace(/^\?/, "")),
      status: failure === null ? c.res.status : statusFromError(failure),
      latency: Math.round((performance.now() - start) * 1000) / 1000,
      ip: clientIP(c),
      user_agent: c.req.header("user-agent") ?? "",
    };

    if (base.isLevelEnabled("debug")) {
      fields.headers = redactHeaders(Object.fromEntries(c.req.raw.headers));
    }

    if (failure !== null) {
      // The error handler runs after this middleware returns, so the response
      // status is still the default: take the status from the error itself.
      fields.err = failure instanceof Error ? failure.message : String(failure);
      reqLog.error(fields, "request failed");
      throw failure;
    }

    reqLog.info(fields, "request");
  };
}

/**
 * Reports the status code an error will produce: an AppError carries its own
 * code, anything else becomes a 500.
 */
function statusFromError(err: unknown): number {
  return err instanceof AppError ? err.status : 500;
}

/**
 * Reads the request's own span first, which is the identifier the trace in the
 * collector is filed under — the two must be the same string or the log line
 * and the trace it names are different requests. It falls back to a W3C
 * traceparent header, then X-Trace-Id, and generates an id when none is
 * present, so a trace id is always available.
 */
function traceIDFrom(c: AppContext): string {
  const span = spanFrom(c);
  if (span?.traceId) {
    return span.traceId;
  }

  const matches = TRACEPARENT_FORMAT.exec((c.req.header(HeaderTraceparent) ?? "").toLowerCase());
  if (matches) {
    return matches[1];
  }

  const header = sanitizeId(c.req.header(HeaderTraceID));
  if (header) {
    return header.replace(/-/g, "");
  }

  return crypto.randomUUID().replace(/-/g, "");
}

/**
 * The caller's address.
 *
 * Fiber's `c.IP()` reads the proxy header when one is configured and the
 * connection otherwise. Here the proxy header is preferred — a deployment
 * behind a load balancer would otherwise log the balancer for every request —
 * and the Bun connection address is the fallback.
 */
function clientIP(c: AppContext): string {
  const forwarded = c.req.header("x-forwarded-for");
  if (forwarded) {
    const first = forwarded.split(",")[0].trim();
    if (first) return first;
  }
  const real = c.req.header("x-real-ip")?.trim();
  if (real) return real;

  try {
    return getConnInfo(c).remote.address ?? "";
  } catch {
    // Serving without the Bun server object (a unit test, an adapter that does
    // not expose it) leaves the address unknown rather than failing the request.
    return "";
  }
}
