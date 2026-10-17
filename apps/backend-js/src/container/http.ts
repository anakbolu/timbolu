/**
 * The HTTP app: middleware chain, CORS and the global error handler.
 *
 * Ported from apps/backend/internal/container/http.go. The order of the chain
 * is the design: tracing first so the request's trace id comes from its own
 * span, then the request context whose logger is the same request, then CORS.
 */

import { Hono } from "hono";
import { cors } from "hono/cors";
import type { AppEnv, AppContext } from "../platform/context.ts";
import { HeaderRequestID, HeaderTraceID } from "../platform/context.ts";
import { AppError, isAppError } from "../platform/errors.ts";
import { error as errorEnvelope } from "../platform/response.ts";
import type { Logger } from "../platform/logger.ts";

export interface CorsConfig {
  origins: string[];
}

/**
 * Lets the web app call the API from the browser.
 *
 * The client sends the refresh cookie, so the response must name the caller's
 * origin exactly: browsers reject `Access-Control-Allow-Origin: *` on a
 * credentialed request. The configured origins are allowed; with none configured
 * (local development) the request origin is reflected, which is what makes
 * `localhost:3000` and the docker frontend both work.
 */
export function corsMiddleware(cfg: CorsConfig) {
  const allowed = new Set(cfg.origins.filter(Boolean));

  return cors({
    origin: (origin) => {
      if (!origin) return null;
      // No configured list means local development: reflect the caller.
      if (allowed.size === 0) return origin;
      return allowed.has(origin) ? origin : null;
    },
    allowMethods: ["GET", "POST", "HEAD", "PUT", "DELETE", "PATCH", "OPTIONS"],
    // An explicit list, not "*": on a credentialed request the browser reads the
    // wildcard literally instead of as a wildcard, so the preflight would fail
    // for every real header.
    allowHeaders: [
      "Content-Type",
      "Authorization",
      "X-Workspace-Id",
      "X-Request-Id",
      "X-Trace-Id",
      "Accept",
      "Origin",
      "Last-Event-ID",
    ],
    exposeHeaders: ["X-Request-Id", "X-Trace-Id", "Content-Disposition"],
    // Required for the refresh cookie to be sent and stored.
    credentials: true,
  });
}

/**
 * Turns an unhandled error into the standard JSON envelope and logs it with the
 * request context. A thrown AppError carries its own status and code; anything
 * else becomes a 500 `internal_error`, which is what the Go error handler does.
 */
export function errorHandler(log: Logger) {
  return (err: Error, c: AppContext): Response => {
    const appErr = isAppError(err) ? err : null;
    const status = appErr?.status ?? 500;
    const message = appErr?.message ?? "internal server error";
    const code = appErr?.code ?? "internal_error";

    const reqLog = c.get("log") ?? log;
    reqLog.error(
      { err: { message: err.message, stack: err.stack }, status, path: c.req.path },
      "unhandled error",
    );

    const res = errorEnvelope(c as any, status, message, code, appErr?.details);
    // The envelope builder cannot see the request ids; echo them here so a user
    // can quote them when reporting a problem.
    const requestId = c.get("requestId");
    const traceId = c.get("traceId");
    if (requestId) res.headers.set(HeaderRequestID, requestId);
    if (traceId) res.headers.set(HeaderTraceID, traceId);
    return res;
  };
}

/**
 * Builds the app with the middleware chain installed and no routes yet.
 * `registerRoutes` mounts the modules on top of this.
 */
export function newApp(opts: { log: Logger; cors: CorsConfig; tracing: () => any; requestContext: () => any }): Hono<AppEnv> {
  const app = new Hono<AppEnv>();

  // Tracing goes on first so the request's trace id comes from its own span,
  // and the logger the next middleware builds is the same request as the trace.
  app.use("*", opts.tracing());
  app.use("*", opts.requestContext());
  app.use("*", corsMiddleware(opts.cors));

  app.onError(errorHandler(opts.log));
  app.notFound((c) =>
    errorEnvelope(c as any, 404, `route not found: ${c.req.method} ${c.req.path}`, "not_found"),
  );

  return app;
}

/** A route that answers 503 rather than 404, for a module this deployment lacks. */
export function unavailableModule(message: string, code: string) {
  return (c: AppContext) => errorEnvelope(c as any, 503, message, code);
}

export { AppError };
