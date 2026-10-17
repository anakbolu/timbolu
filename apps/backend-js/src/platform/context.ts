/**
 * The typed Hono environment every handler shares.
 *
 * Mirrors the Fiber `Locals` contract from
 * apps/backend/internal/platform/middleware/identity.go: the auth middleware
 * writes `userId`/`userEmail`, the tenant middleware writes `scope`/`role`, and
 * handlers read them through the accessors below — never by parsing a header
 * themselves.
 */

import type { Context } from "hono";
import type { Scope } from "./database/index.ts";
import type { Logger } from "./logger.ts";

export interface AppVariables {
  /** Set by the request-context middleware. */
  requestId: string;
  traceId: string;
  workspaceId: string;
  log: Logger;
  /** Set by the auth middleware on authenticated routes. */
  userId?: string;
  userEmail?: string;
  /** Set by the tenant middleware after the membership check. */
  scope?: Scope;
  role?: string;
  /** Set by the auth middleware: whether the account's email is confirmed. */
  emailVerified?: boolean;
  /** Set by the platform-admin middleware. */
  platformRole?: string;
}

export type AppEnv = { Variables: AppVariables };
export type AppContext = Context<AppEnv>;

/** Header names understood by the request context middleware. */
export const HeaderRequestID = "X-Request-Id";
export const HeaderWorkspaceID = "X-Workspace-Id";
export const HeaderTraceID = "X-Trace-Id";

/** The authenticated user, or undefined when the request is anonymous. */
export function currentUserId(c: AppContext): string | undefined {
  return c.get("userId");
}

/** The authenticated user's address, used when accepting an invitation. */
export function currentEmail(c: AppContext): string {
  return c.get("userEmail") ?? "";
}

/** The caller's role in the active workspace, or an empty string. */
export function role(c: AppContext): string {
  return c.get("role") ?? "";
}

/**
 * The tenant scope set by the tenant middleware. A zero scope means the route
 * is not tenant scoped.
 */
export function scope(c: AppContext): Scope {
  return c.get("scope") ?? {};
}

/**
 * The active workspace id. The tenant scope is authoritative because the tenant
 * middleware only sets it after verifying the caller's membership; the header
 * value is the fallback for a request that has not been through it.
 */
export function workspaceUUID(c: AppContext): string | undefined {
  const s = c.get("scope");
  if (s?.workspaceId) return s.workspaceId;
  const header = c.req.header(HeaderWorkspaceID)?.trim();
  return isUuid(header) ? header : undefined;
}

/** The request-scoped logger. Handlers must log through it. */
export function logger(c: AppContext): Logger {
  return c.get("log");
}

/** The request identifier, generating one when the middleware was not installed. */
export function requestId(c: AppContext): string {
  return c.get("requestId") || crypto.randomUUID();
}

/** The correlation id shared with tracing. */
export function traceId(c: AppContext): string {
  return c.get("traceId") || crypto.randomUUID().replace(/-/g, "");
}

const UUID_RE = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

/** True for a canonical UUID string. */
export function isUuid(value: string | null | undefined): value is string {
  return !!value && UUID_RE.test(value);
}

/** True for a UUID-shaped value, the shape the request middleware accepts. */
export function sanitizeId(value: string | null | undefined): string {
  const trimmed = (value ?? "").trim();
  return isUuid(trimmed) ? trimmed : "";
}
