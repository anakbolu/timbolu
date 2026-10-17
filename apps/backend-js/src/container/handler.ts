/**
 * The route table: the only place that knows the URL layout, and the only place
 * that decides which middleware guards which group.
 *
 * Ported from apps/backend/internal/container/handler.go:
 *
 *   /api                          public: health and the auth entry points
 *   /api  + requireUser           account endpoints, workspace list, invites
 *   /api  + requireTenant         everything scoped to the active workspace
 *
 * The order is the guard. Hono, like Fiber, applies a middleware to the routes
 * registered after it, so every read a plain member may make is registered above
 * the role groups.
 */

import { Hono } from "hono";
import type { AppEnv } from "../platform/context.ts";
import { error as errorEnvelope } from "../platform/response.ts";
import type { Container } from "./container.ts";
import { requirePlatformAdmin, requireRole, requireTenant, requireUser, requireVerified, type GuardDeps } from "./middleware-auth.ts";
import { isAdminRole } from "../modules/admin/domain/admin.ts";

import { routes as healthRoutes } from "../modules/health/handler/handler.ts";
import { routes as authRoutes, protectedRoutes as authProtected } from "../modules/auth/handler/handler.ts";
import {
  managerRoutes as workspaceManager,
  ownerRoutes as workspaceOwner,
  tenantRoutes as workspaceTenant,
  userRoutes as workspaceUser,
  verifiedRoutes as workspaceVerified,
} from "../modules/workspace/handler/handler.ts";
import {
  catalogRoutes as agentCatalog,
  managerRoutes as agentManager,
  routes as agentRoutes,
} from "../modules/agent/handler/handler.ts";
import { managerRoutes as llmManager, routes as llmRoutes } from "../modules/llm/handler/handler.ts";
import { routes as adminRoutes } from "../modules/admin/handler/handler.ts";

/** A route that answers 503 rather than 404, for a module this deployment lacks. */
function unavailable(message: string, code: string) {
  return (c: any) => errorEnvelope(c, 503, message, code);
}

/** Builds the guard dependencies from the container. */
function guards(c: Container): GuardDeps {
  return {
    auth: c.authService as any,
    workspace: c.workspaceService as any,
    requireEmailVerification: c.config.auth.requireEmailVerification,
    isPlatformAdmin: isAdminRole,
  };
}

/** Mounts every module router. */
export function registerRoutes(app: Hono<AppEnv>, c: Container): void {
  const api = new Hono<AppEnv>();
  const deps = guards(c);
  const prefix = c.config.http.apiPrefix || "/api";

  // Health sits at the root, not under the prefix — a probe does not know the
  // API's layout.
  if (c.health) healthRoutes(app, c.health, prefix);
  else {
    app.get("/health", unavailable("health is not configured", "health_not_configured"));
    app.get("/ready", unavailable("health is not configured", "health_not_configured"));
  }

  // --- public ---------------------------------------------------------------
  if (c.auth) authRoutes(api, c.auth);
  else api.all("/auth/*", unavailable("authentication is not configured", "auth_not_configured"));

  // --- authenticated, no active workspace -----------------------------------
  const user = new Hono<AppEnv>();
  user.use("*", requireUser(deps));
  if (c.auth) authProtected(user, c.auth);

  // The operator surface: an authenticated account and nothing else. It is
  // registered before the tenant groups because those are empty-path groups, and
  // a platform-role check registered after them would be asked for a workspace.
  if (c.admin) {
    api.use("/admin", requireUser(deps), requirePlatformAdmin(deps));
    adminRoutes(api, c.admin);
  } else {
    api.all("/admin*", unavailable("the operator surface is not configured", "admin_not_configured"));
  }

  // The reads that describe the deployment rather than a workspace.
  if (c.agent) agentCatalog(user, c.agent);

  if (c.workspace) {
    workspaceUser(user, c.workspace);
  } else {
    user.all("/workspaces*", unavailable("workspaces are not configured", "workspace_not_configured"));
  }

  // Verified accounts only: onboarding and accepting an invitation create or join
  // tenancy, so the address must be confirmed first.
  const verified = new Hono<AppEnv>();
  verified.use("*", requireUser(deps), requireVerified(deps));
  if (c.workspace) workspaceVerified(verified, c.workspace);

  // --- active workspace required --------------------------------------------
  const tenant = new Hono<AppEnv>();
  tenant.use("*", requireUser(deps), requireTenant(deps));

  if (c.workspace) workspaceTenant(tenant, c.workspace);
  if (c.agent) agentRoutes(tenant, c.agent);
  if (c.llm) llmRoutes(tenant, c.llm);

  // Modules not yet wired answer 503 on their documented paths rather than 404,
  // so a screen that asks for one says "not configured" instead of looking like a
  // typo. Kept in the same order as the Go container's registerUnavailableModules.
  const pending: Array<[string, string, string]> = [
    ["/conversations*", "chat is not configured", "chat_not_configured"],
    ["/tasks*", "tasks are not configured", "task_not_configured"],
    ["/memories*", "memory is not configured", "memory_not_configured"],
    ["/knowledge*", "memory is not configured", "memory_not_configured"],
    ["/routines*", "routines are not configured", "routine_not_configured"],
    ["/routine-runs*", "routines are not configured", "routine_not_configured"],
    ["/whatsapp*", "whatsapp is not configured", "whatsapp_not_configured"],
    ["/billing*", "billing is not configured", "billing_not_configured"],
    ["/drafts*", "approvals are not configured", "approval_not_configured"],
    ["/approval-policies*", "approvals are not configured", "approval_not_configured"],
    ["/integrations*", "integrations are not configured", "integration_not_configured"],
    ["/orders*", "orders are not configured", "order_not_configured"],
    ["/checklists*", "checklists are not configured", "checklist_not_configured"],
  ];
  for (const [path, message, code] of pending) tenant.all(path, unavailable(message, code));

  // --- role groups, registered last -----------------------------------------
  const managers = new Hono<AppEnv>();
  managers.use("*", requireUser(deps), requireTenant(deps), requireRole("owner", "admin"));
  if (c.workspace) workspaceManager(managers, c.workspace);
  if (c.agent) agentManager(managers, c.agent);
  if (c.llm) llmManager(managers, c.llm);

  const owners = new Hono<AppEnv>();
  owners.use("*", requireUser(deps), requireTenant(deps), requireRole("owner"));
  if (c.workspace) workspaceOwner(owners, c.workspace);

  // Everything assembled under the API prefix.
  api.route("/", user);
  api.route("/", verified);
  api.route("/", tenant);
  api.route("/", managers);
  api.route("/", owners);
  app.route(prefix, api);
}
