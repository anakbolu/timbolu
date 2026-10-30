/**
 * The operator surface is gated by the route table, not only by its own service.
 *
 * The guard used to be registered with `api.use("/admin", ...)`, and Hono matches
 * that path exactly. Every admin route lives one segment below it, so the
 * middleware never ran: an account with `role=user` read the deployment's account
 * list with a plain bearer token (#485). The first case is that request.
 *
 * The others pin the two edges of the fix. A request with no token is refused
 * before any route handler runs, and a path with no route stays a 404 — a guard
 * registered over the whole `/admin` prefix would have answered 401 there and a
 * client would read that as an expired session (see `withGuards`).
 *
 * The app is the real `registerRoutes` over a partial container: the auth service
 * and the admin service are stubs, so nothing opens Postgres, Redis or a socket.
 */

import { describe, expect, test } from "bun:test";
import { Hono } from "hono";

import type { AppEnv } from "../platform/context.ts";
import { nopLogger } from "../platform/logger.ts";
import type { Container } from "./container.ts";
import { registerRoutes } from "./handler.ts";
import type {
  Account,
  ChangeRoleRequest,
  Filter,
  Service as AdminService,
  Stats,
} from "../modules/admin/domain/admin.ts";
import { Handler as AdminHandler } from "../modules/admin/handler/handler.ts";

const userId = "22222222-2222-2222-2222-222222222222";
const adminId = "33333333-3333-3333-3333-333333333333";
const targetId = "44444444-4444-4444-4444-444444444444";

function account(id: string, role: string): Account {
  return {
    id,
    email: `${role}@example.com`,
    role,
    emailVerified: true,
    onboarded: true,
    hasGoogle: false,
    hasPassword: true,
    createdAt: new Date(0),
    workspaces: 1,
  };
}

/** The operator surface's use cases, with no database behind them. */
class StubAdminService implements AdminService {
  async overview(): Promise<Stats> {
    return {
      total: 2,
      admins: 1,
      users: 1,
      verified: 2,
      onboarded: 2,
      googleAccounts: 0,
      newLast7Days: 2,
    };
  }

  async list(_filter: Filter): Promise<[Account[], number]> {
    return [[account(userId, "user")], 1];
  }

  async changeRole(req: ChangeRoleRequest): Promise<Account> {
    return account(req.targetID, req.role);
  }
}

/** Every token the fake auth service accepts, and the account it names. */
const sessions: Record<string, Account> = {
  "user-token": account(userId, "user"),
  "admin-token": account(adminId, "admin"),
};

const authPort = {
  async authenticate(token: string): Promise<{ id: string; email: string }> {
    const session = sessions[token];
    if (!session) throw { code: "invalid_token" };
    return { id: session.id, email: session.email };
  },
  async currentUser(id: string): Promise<{ id: string; email: string; role: string }> {
    const session = Object.values(sessions).find((entry) => entry.id === id);
    if (!session) throw { code: "user_not_found" };
    return { id: session.id, email: session.email, role: session.role };
  },
  async assertVerified(): Promise<void> {
    // The operator surface never asks; the guard deps only carry the port.
  },
};

/** The route table over a container whose only built module is the operator surface. */
function newApp(): Hono<AppEnv> {
  const container = {
    config: {
      http: { apiPrefix: "/api" },
      auth: { requireEmailVerification: false },
    },
    log: nopLogger,
    auth: null,
    authService: authPort,
    workspaceService: null,
    admin: new AdminHandler(new StubAdminService(), nopLogger),
  } as unknown as Container;

  const app = new Hono<AppEnv>();
  registerRoutes(app, container);
  return app;
}

interface Envelope {
  success: boolean;
  message: string;
  data?: Record<string, unknown>;
  error?: { code: string };
}

describe("the operator surface's guard", () => {
  test("refuses a plain account on every admin route", async () => {
    const app = newApp();

    const overview = await app.request("/api/admin/overview", { headers: { Authorization: "Bearer user-token" } });
    expect(overview.status).toBe(403);
    expect(((await overview.json()) as Envelope).error?.code).toBe("forbidden");

    const users = await app.request("/api/admin/users", { headers: { Authorization: "Bearer user-token" } });
    expect(users.status).toBe(403);
    expect(((await users.json()) as Envelope).error?.code).toBe("forbidden");

    // 403, not the handler's 400 for a malformed body: the guard runs first.
    const change = await app.request(`/api/admin/users/${targetId}/role`, {
      method: "POST",
      headers: { Authorization: "Bearer user-token", "Content-Type": "application/json" },
      body: JSON.stringify({ role: "admin" }),
    });
    expect(change.status).toBe(403);
    expect(((await change.json()) as Envelope).error?.code).toBe("forbidden");
  });

  test("asks an anonymous request for a token before any route runs", async () => {
    const app = newApp();

    const res = await app.request("/api/admin/overview");
    expect(res.status).toBe(401);
    expect(((await res.json()) as Envelope).error?.code).toBe("unauthenticated");
  });

  test("admits a platform admin to every admin route", async () => {
    const app = newApp();

    const overview = await app.request("/api/admin/overview", { headers: { Authorization: "Bearer admin-token" } });
    expect(overview.status).toBe(200);
    const overviewBody = (await overview.json()) as Envelope;
    expect(overviewBody.success).toBe(true);
    expect(overviewBody.data?.total).toBe(2);

    const users = await app.request("/api/admin/users", { headers: { Authorization: "Bearer admin-token" } });
    expect(users.status).toBe(200);

    const change = await app.request(`/api/admin/users/${targetId}/role`, {
      method: "POST",
      headers: { Authorization: "Bearer admin-token", "Content-Type": "application/json" },
      body: JSON.stringify({ role: "admin" }),
    });
    expect(change.status).toBe(200);
    expect(((await change.json()) as Envelope).data?.role).toBe("admin");
  });

  test("leaves paths without a route as 404, not as a guard refusal", async () => {
    const app = newApp();

    const exact = await app.request("/api/admin", { headers: { Authorization: "Bearer admin-token" } });
    expect(exact.status).toBe(404);

    const typo = await app.request("/api/admin/overveiw", { headers: { Authorization: "Bearer admin-token" } });
    expect(typo.status).toBe(404);

    // A typo carries no route, so it is not a token problem even without one.
    const anonymous = await app.request("/api/admin/overveiw");
    expect(anonymous.status).toBe(404);
  });
});