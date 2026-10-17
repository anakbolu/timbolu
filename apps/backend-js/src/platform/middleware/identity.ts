/**
 * The writer side of the request identity.
 *
 * Ported from apps/backend/internal/platform/middleware/identity.go. The Go
 * file holds both the setters (used by the container's auth and tenant
 * middleware) and the accessors (used by module handlers). In this port the
 * accessors already live in `src/platform/context.ts` and are not duplicated
 * here: this file only writes the variables those accessors read, so there is
 * exactly one place a value can come from.
 *
 * Only the auth and tenant middleware may call these; a handler that sets them
 * would be granting itself a scope it was never given.
 */

import type { AppContext } from "../context.ts";
import type { Scope } from "../database/index.ts";

/**
 * Stores the authenticated user on the request. The email is kept as well
 * because accepting an invitation must match it against the invited address,
 * and no module may read another module's repository.
 */
export function setIdentity(c: AppContext, userId: string, email: string): void {
  c.set("userId", userId);
  c.set("userEmail", email);
}

/**
 * Stores the tenant scope the request may query with. Only the tenant
 * middleware sets it, and only after the caller's membership was verified.
 */
export function setScope(c: AppContext, scope: Scope): void {
  c.set("scope", scope);
}

/** Stores the caller's role inside the active workspace. */
export function setRole(c: AppContext, role: string): void {
  c.set("role", role);
}
