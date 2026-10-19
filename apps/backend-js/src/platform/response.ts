/**
 * The standard JSON envelope returned by every HTTP handler:
 * `{success, message, data?, meta?, error?}`.
 *
 * Ported 1:1 from apps/backend/internal/platform/response/response.go so the
 * existing Next.js frontend keeps working unchanged.
 */

export interface ErrorPayload {
  code: string;
  details?: unknown;
}

export interface APIResponse<T = unknown> {
  success: boolean;
  message: string;
  data?: T;
  meta?: unknown;
  error?: ErrorPayload;
}

/** Minimal surface of a Hono context this module needs. */
interface JsonContext {
  json: (body: unknown, status?: any) => Response;
}

function body<T>(success: boolean, message: string, data: T, meta: unknown, error?: ErrorPayload): APIResponse<T> {
  const out: APIResponse<T> = { success, message };
  // Go's `json:",omitempty"` on an interface field drops nil. Mirror that:
  // undefined and null both mean "no data", an empty array or object does not.
  if (data !== undefined && data !== null) out.data = data;
  if (meta !== undefined && meta !== null) out.meta = meta;
  if (error !== undefined) out.error = error;
  return out;
}

/** Success envelope with an explicit status code. */
export function success<T>(c: JsonContext, status: number, message: string, data?: T): Response {
  return successWithMeta(c, status, message, data, undefined);
}

/** Success envelope with pagination/aggregate metadata. */
export function successWithMeta<T>(
  c: JsonContext,
  status: number,
  message: string,
  data: T | undefined,
  meta: unknown,
): Response {
  return c.json(body(true, message, data, meta), status);
}

/** Failure envelope with an explicit status code. */
export function error(c: JsonContext, status: number, message: string, code: string, details?: unknown): Response {
  return c.json(body(false, message, undefined, undefined, details === undefined ? { code } : { code, details }), status);
}

/** 200 success envelope. */
export function ok<T>(c: JsonContext, message: string, data?: T): Response {
  return success(c, 200, message, data);
}

/** 200 success envelope with metadata. */
export function okWithMeta<T>(c: JsonContext, message: string, data: T | undefined, meta: unknown): Response {
  return successWithMeta(c, 200, message, data, meta);
}

/** 201 success envelope. */
export function created<T>(c: JsonContext, message: string, data?: T): Response {
  return success(c, 201, message, data);
}

/** 503 envelope, used by readiness probes and unconfigured routes. */
export function unavailable(c: JsonContext, message: string, code: string): Response {
  return error(c, 503, message, code);
}
