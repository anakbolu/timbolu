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
import type { MiddlewareHandler } from "hono";
import type { AppEnv } from "../platform/context.ts";
import { error as errorEnvelope } from "../platform/response.ts";
import type { Container } from "./container.ts";
import { requireActiveSubscription, requirePlatformAdmin, requireRole, requireStreamAuth, requireTenant, requireUser, requireVerified, type GuardDeps } from "./middleware-auth.ts";
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
import { routes as chatRoutes, managerRoutes as chatManager, streamRoutes as chatStream, contentRoutes as chatContent } from "../modules/chat/handler/handler.ts";
import { routes as integrationRoutes, callbackRoutes as integrationCallback } from "../modules/integration/handler/handler.ts";
import { routes as checklistRoutes } from "../modules/checklist/handler/handler.ts";
import { routes as orderRoutes, managerRoutes as orderManager } from "../modules/order/handler/handler.ts";
import { routes as billingRoutes, webhookRoutes as billingWebhook } from "../modules/billing/handler/handler.ts";
import { routes as approvalRoutes, managerRoutes as approvalManager } from "../modules/approval/handler/handler.ts";
import { ownerRoutes as retentionOwner } from "../modules/retention/handler/handler.ts";
import { routes as memoryRoutes } from "../modules/memory/handler/handler.ts";
import { routes as deviceRoutes } from "../modules/device/handler/handler.ts";
import { routes as routineRoutes } from "../modules/routine/handler/handler.ts";
import { routes as whatsappRoutes, linkStreamRoutes as whatsappLinkStream } from "../modules/whatsapp/handler/handler.ts";
import { routes as taskRoutes } from "../modules/task/handler/handler.ts";

/** A route that answers 503 rather than 404, for a module this deployment lacks. */
function unavailable(message: string, code: string) {
  return (c: any) => errorEnvelope(c, 503, message, code);
}

/** Builds the guard dependencies from the container. */
function guards(c: Container): GuardDeps {
  return {
    auth: c.authService as any,
    workspace: c.workspaceService as any,
    billing: c.billingService,
    requireEmailVerification: c.config.auth.requireEmailVerification,
    isPlatformAdmin: isAdminRole,
  };
}


/**
 * Builds a route group whose middleware is attached to the paths it registers,
 * not to the whole prefix.
 *
 * `group.use("*", mw)` on a sub-app mounted at "/" runs for every request that
 * reaches the sub-app — including one that matches no route at all — so a typo'd
 * URL answered 401 "access token is required" instead of 404, which a client reads
 * as an expired session. Fiber's empty-path groups do not behave that way, so this
 * reproduces the original contract.
 *
 * The routes are registered twice: once on a throwaway app to learn their paths,
 * then on the real one with a guard per path. Registration is pure, so doing it
 * twice is safe.
 */
function withGuards(
  register: (g: Hono<AppEnv>) => void,
  ...middleware: MiddlewareHandler<AppEnv>[]
): Hono<AppEnv> {
  const probe = new Hono<AppEnv>();
  register(probe);

  const group = new Hono<AppEnv>();
  for (const path of new Set(probe.routes.map((r) => r.path))) {
    group.use(path, ...middleware);
  }
  register(group);
  return group;
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

  // The endpoints a provider reaches directly. Where they are registered is what
  // makes them public: an empty-path group below applies its middleware to the
  // whole prefix, so a route added after it would be asked for a session it
  // cannot have. What makes them safe is what each carries — a signed,
  // single-use state for the redirect, and the adapter's own signature for a
  // webhook.
  if (c.integration) integrationCallback(api, c.integration);
  if (c.billing) billingWebhook(api, c.billing);

  // --- authenticated, no active workspace -----------------------------------
  const user = withGuards((g) => {
    if (c.auth) authProtected(g, c.auth);
    // The reads that describe the deployment rather than a workspace.
    if (c.agent) agentCatalog(g, c.agent);
    if (c.workspace) workspaceUser(g, c.workspace);
    else g.all("/workspaces*", unavailable("workspaces are not configured", "workspace_not_configured"));
  }, requireUser(deps));

  // The operator surface: an authenticated account and nothing else. It is
  // registered before the tenant groups because those are empty-path groups, and
  // a platform-role check registered after them would be asked for a workspace.
  const admin = c.admin;
  if (admin) {
    // The guard is attached to the paths the module registers, not to "/admin",
    // which Hono matches exactly and which no admin route has (#485). A wildcard
    // would cover those paths too, but it would also answer 401 on "/admin" and
    // on a typo below it, where the contract is 404 — so this uses the same
    // per-path mechanism as the other guarded groups.
    api.route("/", withGuards((g) => adminRoutes(g, admin), requireUser(deps), requirePlatformAdmin(deps)));
  } else {
    api.all("/admin*", unavailable("the operator surface is not configured", "admin_not_configured"));
  }

  // Verified accounts only: onboarding and accepting an invitation create or join
  // tenancy, so the address must be confirmed first.
  //
  // The guard is registered on the two paths rather than on an empty-path group.
  // An empty-path group applies its middleware to every route registered after
  // it, whichever group registers that route — mounted at "/" it would put
  // requireVerified on every tenant read as well, and a member whose address is
  // unconfirmed would be told to verify before they could see their own chat.
  api.use("/workspaces/onboard", requireUser(deps), requireVerified(deps));
  api.use("/invitations/accept", requireUser(deps), requireVerified(deps));
  if (c.workspace) workspaceVerified(api, c.workspace);

  // --- active workspace required --------------------------------------------
  const pending: Array<[string, string, string]> = [];
  if (!c.chat) pending.push(["/conversations*", "chat is not configured", "chat_not_configured"], ["/events*", "chat is not configured", "chat_not_configured"]);
  if (!c.task) pending.push(["/tasks*", "tasks are not configured", "task_not_configured"]);
  if (!c.memory) pending.push(["/memories*", "memory is not configured", "memory_not_configured"], ["/knowledge*", "memory is not configured", "memory_not_configured"]);
  if (!c.routine) pending.push(["/routines*", "routines are not configured", "routine_not_configured"], ["/routine-runs*", "routines are not configured", "routine_not_configured"]);
  if (!c.whatsapp) pending.push(["/whatsapp*", "whatsapp is not configured", "whatsapp_not_configured"]);
  if (!c.billing) pending.push(["/billing*", "billing is not configured", "billing_not_configured"]);
  if (!c.approval) pending.push(["/drafts*", "approvals are not configured", "approval_not_configured"], ["/approval-policies*", "approvals are not configured", "approval_not_configured"]);
  if (!c.integration) pending.push(["/integrations*", "integrations are not configured", "integration_not_configured"]);
  if (!c.order) pending.push(["/orders*", "orders are not configured", "order_not_configured"]);
  if (!c.checklist) pending.push(["/checklists*", "checklists are not configured", "checklist_not_configured"]);
  if (!c.device) pending.push(["/devices*", "devices are not configured", "device_not_configured"]);

  // Active workspace required. The subscription guard runs here rather than
  // inside each service, because "everything that writes stops" is one decision
  // about the workspace rather than thirty about its endpoints — and thirty
  // copies would be thirty places for a new endpoint to forget. The billing
  // routes stay reachable because `requireActiveSubscription` exempts them by
  // path; see its comment for why a workspace that cannot pay must be able to
  // reach the call that fixes it.
  const tenant = withGuards((g) => {
    if (c.workspace) workspaceTenant(g, c.workspace);
    if (c.agent) agentRoutes(g, c.agent);
    if (c.llm) llmRoutes(g, c.llm);
    if (c.chat) chatRoutes(g, c.chat);
    if (c.task) taskRoutes(g, c.task);
    if (c.memory) memoryRoutes(g, c.memory);
    if (c.routine) routineRoutes(g, c.routine);
    if (c.whatsapp) whatsappRoutes(g, c.whatsapp);
    if (c.billing) billingRoutes(g, c.billing);
    if (c.approval) approvalRoutes(g, c.approval);
    if (c.integration) integrationRoutes(g, c.integration);
    if (c.order) orderRoutes(g, c.order);
    if (c.checklist) checklistRoutes(g, c.checklist);
    if (c.device) deviceRoutes(g, c.device);
    // Modules not wired answer 503 on their documented paths rather than 404, so a
    // screen that asks for one says "not configured" instead of looking like a typo.
    for (const [path, message, code] of pending) g.all(path, unavailable(message, code));
  }, requireUser(deps), requireTenant(deps), requireActiveSubscription(deps, prefix));

  // --- role groups, registered last -----------------------------------------
  const managers = withGuards((g) => {
    if (c.workspace) workspaceManager(g, c.workspace);
    if (c.agent) agentManager(g, c.agent);
    if (c.llm) llmManager(g, c.llm);
    if (c.chat) chatManager(g, c.chat);
    if (c.order) orderManager(g, c.order);
    if (c.approval) approvalManager(g, c.approval);
  }, requireUser(deps), requireTenant(deps), requireActiveSubscription(deps, prefix), requireRole("owner", "admin"));

  const owners = withGuards((g) => {
    if (c.workspace) workspaceOwner(g, c.workspace);
    if (c.retention) retentionOwner(g, c.retention);
  }, requireUser(deps), requireTenant(deps), requireActiveSubscription(deps, prefix), requireRole("owner"));

  // The live stream carries its own authentication because a browser cannot set
  // a header on a WebSocket or an EventSource: the token and the workspace travel
  // in the query string. The middleware is registered on the stream paths only, so
  // a public route is never asked for a token.
  if (c.chat) {
    app.use(`${prefix}/events/stream`, requireStreamAuth(deps, "chat_not_configured"));
    app.use(`${prefix}/events/socket`, requireStreamAuth(deps, "chat_not_configured"));
    chatStream(api, c.chat);
    // The content origin is mounted on the app root and is deliberately
    // unauthenticated: an iframe without allow-same-origin sends no cookie, so the
    // reference in the URL is the only capability and it names one document.
    chatContent(app, c.chat);
  }
  if (c.whatsapp) {
    app.use(`${prefix}/whatsapp/links/:linkID/socket`, requireStreamAuth(deps, "whatsapp_not_configured"));
    whatsappLinkStream(api, c.whatsapp);
  }

  // Everything assembled under the API prefix.
  api.route("/", user);
  api.route("/", tenant);
  api.route("/", managers);
  api.route("/", owners);
  app.route(prefix, api);
}
