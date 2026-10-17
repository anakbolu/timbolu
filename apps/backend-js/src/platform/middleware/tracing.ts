/**
 * The tracing middleware: one span per request, and the trace id its logs carry.
 *
 * Ported from apps/backend/internal/platform/middleware/tracing.go, with one
 * deliberate reduction. The Go deployment runs the OpenTelemetry SDK and
 * exports spans; this port has no OTel SDK installed (it is not a dependency,
 * and adding one is not allowed). So the span here is a local record — it is
 * never exported — and what it is kept for is the identifier: a valid 32-hex
 * trace id that is generated once per request, stored on the context, and
 * therefore stable for every log line the request writes.
 *
 * What the span records is deliberately thin, matching Go: the method, the
 * matched *route* rather than the path, the status, and the workspace once the
 * tenant is known. **The URL and the query string are never recorded**, because
 * the event stream authenticates from the query string and a span carrying the
 * URL would carry a token into a second system.
 *
 * It is installed before the request-context middleware so the two agree: the
 * request logger's `trace_id` is read off the span this middleware started,
 * which is what makes a log line and a trace the same request rather than two
 * identifiers for it.
 */

import type { MiddlewareHandler } from "hono";
import type { AppContext, AppEnv } from "../context.ts";
import { HeaderTraceID, sanitizeId } from "../context.ts";
import { AppError } from "../errors.ts";

/** The key the request's span is stored under, mirroring the Go Locals key. */
export const SpanContextKey = "otel_span";

/**
 * The span a request is running under.
 *
 * It is a value object rather than an OTel span: with no exporter installed,
 * the only observable part is the trace id, plus the attributes a debugger or a
 * test can read. `end` freezes it.
 */
export interface Span {
  readonly traceId: string;
  readonly spanId: string;
  name: string;
  attributes: Record<string, string | number>;
  /** The status the request finished with, once it has. */
  status: number;
  /** The error recorded for a failed request, if any. */
  error: Error | null;
  ended: boolean;
}

/**
 * Hono contexts are short-lived and keyed by the request, so a WeakMap holds
 * the span without a variable on the context type — which would mean editing
 * `context.ts`, a file this module does not own.
 */
const spans = new WeakMap<AppContext, Span>();

/** Returns the span a request is running under, or null when tracing is absent. */
export function spanFrom(c: AppContext): Span | null {
  return spans.get(c) ?? null;
}

/**
 * Starts one span per request and carries it into the handler chain.
 *
 * A caller's `traceparent` is honoured, which is what stitches this
 * deployment's spans to whatever is upstream of it. A deployment with no
 * exporter still runs this: the span's trace id is what the log lines carry.
 */
export function tracing(): MiddlewareHandler<AppEnv> {
  return async (c, next) => {
    const span: Span = {
      traceId: traceIdFrom(c),
      spanId: randomHex(8),
      name: spanName(c, ""),
      attributes: { "http.request.method": c.req.method },
      status: 0,
      error: null,
      ended: false,
    };

    // The span travels on the context, which is how a later middleware reads
    // the trace id without knowing anything about tracing.
    spans.set(c, span);
    c.set("traceId", span.traceId);

    let failure: unknown = null;
    try {
      await next();
    } catch (err) {
      failure = err;
    }

    // The route is only known once the chain has matched, so the name is
    // finished here rather than at the start. A route that never matched keeps
    // the path it was asked for, which is exactly what a 404 wants to say.
    const route = c.req.routePath;
    if (route && route !== "/*" && route !== "*") {
      span.name = spanName(c, route);
      span.attributes["http.route"] = route;
    }

    const workspace = c.get("workspaceId");
    if (workspace) {
      span.attributes.workspace_id = workspace;
    }

    let status = c.res.status;
    if (failure !== null) {
      status = failure instanceof AppError ? failure.status : 500;
      span.error = failure instanceof Error ? failure : new Error(String(failure));
    }

    span.attributes["http.response.status_code"] = status;
    span.status = status;
    span.ended = true;

    if (failure !== null) throw failure;
  };
}

/** The trace id for a request: the span's own, a W3C traceparent, or a fresh one. */
function traceIdFrom(c: AppContext): string {
  const traceparent = c.req.header("traceparent")?.toLowerCase() ?? "";
  const match = /^[0-9a-f]{2}-([0-9a-f]{32})-([0-9a-f]{16})-[0-9a-f]{2}$/.exec(traceparent);
  if (match) {
    return match[1];
  }

  const header = sanitizeId(c.req.header(HeaderTraceID));
  if (header) {
    return header.replace(/-/g, "");
  }

  return randomHex(16);
}

/** The span's name: the method and the route, or the path until the route is known. */
function spanName(c: AppContext, route: string): string {
  const target = route || new URL(c.req.url).pathname;
  return `${c.req.method} ${target}`;
}

/** `bytes` of crypto-random bytes as lowercase hex — 16 bytes is a 32-hex trace id. */
function randomHex(bytes: number): string {
  const buffer = crypto.getRandomValues(new Uint8Array(bytes));
  let out = "";
  for (const byte of buffer) {
    out += byte.toString(16).padStart(2, "0");
  }
  return out;
}
