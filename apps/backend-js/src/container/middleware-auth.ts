/**
 * The route guards: identity, tenancy, verification, role, platform admin and
 * the stream variants.
 *
 * Ported from apps/backend/internal/container/middleware_auth.go. Every status,
 * message and error code here is the frontend's contract — the literals are
 * copied verbatim, including the Indonesian ones.
 */

import type { MiddlewareHandler } from "hono";
import type { AppEnv, AppContext } from "../platform/context.ts";
import { HeaderWorkspaceID, currentUserId, isUuid, logger, role, scope } from "../platform/context.ts";
import { AppError } from "../platform/errors.ts";
import { error as errorEnvelope, unavailable } from "../platform/response.ts";

/** The minimum of the auth service the guards need. */
export interface AuthGuardPort {
  /** Resolves a bearer access token to an identity, or throws ErrInvalidToken. */
  authenticate(token: string): Promise<{ id: string; email: string }>;
  /** Reads the account, so a platform role is never taken from a stale token. */
  currentUser(userId: string): Promise<{ id: string; email: string; role: string }>;
  /** Throws ErrEmailNotVerified when the account has not confirmed its address. */
  assertVerified(userId: string): Promise<void>;
}

/** The minimum of the workspace service the guards need. */
export interface WorkspaceGuardPort {
  /** Returns the caller's role, or throws ErrNotMember. */
  resolve(workspaceId: string, userId: string): Promise<string>;
}

/** Sentinel identity: raised by the auth service, read by these guards. */
export function isInvalidToken(err: unknown): boolean {
  return (err as any)?.code === "invalid_token";
}

export function isUserNotFound(err: unknown): boolean {
  return (err as any)?.code === "user_not_found";
}

export function isEmailNotVerified(err: unknown): boolean {
  return (err as any)?.code === "email_not_verified";
}

export function isNotMember(err: unknown): boolean {
  return (err as any)?.code === "not_a_member";
}

/** Reads the access token from the Authorization header. */
export function bearerToken(c: AppContext): string {
  const header = (c.req.header("Authorization") ?? "").trim();
  if (!header) return "";
  const prefix = "Bearer ";
  if (header.length <= prefix.length || header.slice(0, prefix.length).toLowerCase() !== prefix.toLowerCase()) {
    return "";
  }
  return header.slice(prefix.length).trim();
}

/**
 * Reads the active workspace from the request header, then from the query
 * string. Anything that is not a UUID is rejected rather than passed to the
 * database.
 */
export function requestedWorkspaceId(c: AppContext): string {
  const raw = (c.req.header(HeaderWorkspaceID) ?? "").trim() || (c.req.query("workspace_id") ?? "").trim();
  if (!raw) throw new AppError(400, "workspace id is required", "invalid_workspace");
  if (!isUuid(raw)) throw new AppError(400, "workspace id is not a uuid", "invalid_workspace");
  return raw;
}

export interface GuardDeps {
  auth: AuthGuardPort | null;
  workspace: WorkspaceGuardPort | null;
  /** The deployment's email-verification switch. */
  requireEmailVerification: boolean;
  /** The platform-admin check, so `users.role` is read through the auth service. */
  isPlatformAdmin: (role: string) => boolean;
}

/**
 * Authenticates the request from the bearer access token and stores the identity
 * on the context.
 *
 * An identity that is already resolved is left alone rather than resolved again.
 * That is what lets the live stream authenticate from the query string first (a
 * browser cannot set a header on a WebSocket) and this middleware then accept the
 * identity it set, instead of rejecting the request for having no header.
 */
export function requireUser(deps: GuardDeps): MiddlewareHandler<AppEnv> {
  return async (c, next) => {
    if (!deps.auth) return unavailable(c as any, "authentication is not configured", "auth_not_configured");
    if (currentUserId(c)) return next();

    const token = bearerToken(c);
    if (!token) {
      return errorEnvelope(c as any, 401, "access token is required", "unauthenticated");
    }

    try {
      const user = await deps.auth.authenticate(token);
      c.set("userId", user.id);
      c.set("userEmail", user.email);
    } catch (err) {
      if (isInvalidToken(err)) {
        return errorEnvelope(c as any, 401, "access token is invalid or expired", "unauthenticated");
      }
      logger(c).error({ err }, "authenticate request failed");
      return errorEnvelope(c as any, 500, "internal server error", "internal_error");
    }

    return next();
  };
}

/**
 * Resolves the active workspace, verifies the caller's membership, and stores the
 * tenant scope the handlers query with.
 *
 * The check is on the stored scope, never on the workspace header: the header is
 * a request value, so treating it as proof of an already-verified tenant would
 * skip the membership check entirely.
 */
export function requireTenant(deps: GuardDeps): MiddlewareHandler<AppEnv> {
  return async (c, next) => {
    if (!deps.workspace) return unavailable(c as any, "workspaces are not configured", "workspace_not_configured");

    const existing = scope(c);
    if (existing.userId || existing.workspaceId) return next();

    let workspaceId: string;
    try {
      workspaceId = requestedWorkspaceId(c);
    } catch (err) {
      const e = err as AppError;
      return errorEnvelope(c as any, 400, e.message, "invalid_workspace");
    }

    const userId = currentUserId(c)!;
    try {
      const callerRole = await deps.workspace.resolve(workspaceId, userId);
      c.set("scope", { userId, workspaceId });
      c.set("role", callerRole);
    } catch (err) {
      if (isNotMember(err)) {
        return errorEnvelope(c as any, 403, "you are not a member of this workspace", "not_a_member");
      }
      logger(c).error({ err }, "resolve membership failed");
      return errorEnvelope(c as any, 500, "internal server error", "internal_error");
    }

    return next();
  };
}

/**
 * Authenticates the live-stream endpoints: `requireUser` and `requireTenant` with
 * one difference — the token and the workspace may come from the query string.
 */
export function requireStreamAuth(deps: GuardDeps, notConfiguredCode: string): MiddlewareHandler<AppEnv> {
  return async (c, next) => {
    if (!deps.auth || !deps.workspace) {
      return unavailable(c as any, "the live stream is not configured", notConfiguredCode);
    }

    let token = bearerToken(c) || (c.req.query("token") ?? "").trim();
    if (!token) {
      return errorEnvelope(c as any, 401, "access token is required", "unauthenticated");
    }

    try {
      const user = await deps.auth.authenticate(token);
      c.set("userId", user.id);
      c.set("userEmail", user.email);
    } catch (err) {
      if (isInvalidToken(err)) {
        return errorEnvelope(c as any, 401, "access token is invalid or expired", "unauthenticated");
      }
      logger(c).error({ err }, "authenticate stream failed");
      return errorEnvelope(c as any, 500, "internal server error", "internal_error");
    }

    let workspaceId: string;
    try {
      workspaceId = requestedWorkspaceId(c);
    } catch (err) {
      const e = err as AppError;
      return errorEnvelope(c as any, 400, e.message, "invalid_workspace");
    }

    try {
      const callerRole = await deps.workspace.resolve(workspaceId, c.get("userId")!);
      c.set("scope", { userId: c.get("userId")!, workspaceId });
      c.set("role", callerRole);
    } catch (err) {
      if (isNotMember(err)) {
        return errorEnvelope(c as any, 403, "you are not a member of this workspace", "not_a_member");
      }
      logger(c).error({ err }, "resolve membership for stream failed");
      return errorEnvelope(c as any, 500, "internal server error", "internal_error");
    }

    return next();
  };
}

/**
 * Rejects the request unless the authenticated account may reach the operator
 * surface.
 *
 * Not `requireRole`: that reads a role inside the active workspace, which
 * somebody else's workspace owner granted. This reads the account's own platform
 * role, and it needs no workspace at all. The role is read from the account on
 * every request rather than carried in the access token, so a demotion takes
 * effect on the next call instead of on the next sign-in.
 */
export function requirePlatformAdmin(deps: GuardDeps): MiddlewareHandler<AppEnv> {
  return async (c, next) => {
    if (!deps.auth) return unavailable(c as any, "authentication is not configured", "auth_not_configured");

    const userId = currentUserId(c);
    if (!userId) return errorEnvelope(c as any, 401, "access token is required", "unauthenticated");

    try {
      const user = await deps.auth.currentUser(userId);
      if (!deps.isPlatformAdmin(user.role)) {
        return errorEnvelope(c as any, 403, "hanya admin platform yang boleh membuka halaman ini", "forbidden");
      }
      c.set("platformRole", user.role);
    } catch (err) {
      if (isUserNotFound(err)) {
        return errorEnvelope(c as any, 401, "access token is invalid or expired", "unauthenticated");
      }
      logger(c).error({ err }, "resolve platform role failed");
      return errorEnvelope(c as any, 500, "internal server error", "internal_error");
    }

    return next();
  };
}

/** Rejects the request unless the caller's role in the active workspace is allowed. */
export function requireRole(...roles: string[]): MiddlewareHandler<AppEnv> {
  const allowed = new Set(roles);
  return async (c, next) => {
    if (!allowed.has(role(c))) {
      return errorEnvelope(c as any, 403, "your role does not allow this action", "forbidden");
    }
    return next();
  };
}

/**
 * Enforces a verified email. A deployment that has turned verification off admits
 * the account as soon as it exists: the gate is registering, not confirming an
 * address.
 */
export function requireVerified(deps: GuardDeps): MiddlewareHandler<AppEnv> {
  return async (c, next) => {
    if (!deps.auth) return unavailable(c as any, "authentication is not configured", "auth_not_configured");
    if (!deps.requireEmailVerification) return next();

    try {
      await deps.auth.assertVerified(currentUserId(c)!);
    } catch (err) {
      if (isEmailNotVerified(err)) {
        return errorEnvelope(c as any, 403, "verify your email first", "email_not_verified");
      }
      logger(c).error({ err }, "verify account failed");
      return errorEnvelope(c as any, 500, "internal server error", "internal_error");
    }

    return next();
  };
}
