/**
 * The grant writes are the one part of the integration module that is not open
 * to every member: a grant decides which Bolu may use a connected account, and
 * a member who can write one can hand the account's send tools to an agent
 * without asking.
 *
 * These tests pin the split in the route table rather than in the handler: the
 * writes are absent from `routes()` and present in `managerRoutes()`.
 */

import { describe, expect, test } from "bun:test";
import { Hono } from "hono";
import type { AppContext, AppEnv } from "../../../platform/context.ts";
import { requireRole } from "../../../container/middleware-auth.ts";
import { Handler, managerRoutes, routes } from "./handler.ts";

/** Answers "ok" without a service: only the guard is under test here. */
// The cast is unchecked on purpose: `Handler` carries private state, so a stub
// that implements just the two grant writes can never match it structurally.
const handlerStub = {
  setGrant: async (c: AppContext) => c.json({ success: true, message: "ok" }, 200),
  deleteGrant: async (c: AppContext) => c.json({ success: true, message: "ok" }, 200),
} as unknown as Handler;

/** Builds an app whose tenant role comes from the `x-test-role` header. */
function appWithRoleGuard(register: (group: Hono<AppEnv>) => void): Hono<AppEnv> {
  const app = new Hono<AppEnv>();
  app.use("*", async (c, next) => {
    // `requireRole` reads this exact context variable.
    c.set("role", c.req.header("x-test-role") ?? "");
    return next();
  });
  const group = new Hono<AppEnv>();
  group.use("*", requireRole("owner", "admin"));
  register(group);
  app.route("/", group);
  return app;
}

const grantPath = "/agents/1b4e28ba-2fa1-11d2-883f-0016d3cca427/grants/1b4e28ba-2fa1-11d2-883f-0016d3cca427";

describe("grant writes require a manager role", () => {
  test("a member may not set a grant", async () => {
    const app = appWithRoleGuard((group) => managerRoutes(group, handlerStub));
    const res = await app.request(grantPath, {
      method: "PUT",
      headers: { "x-test-role": "member", "content-type": "application/json" },
      body: JSON.stringify({ permission: "read_write" }),
    });
    expect(res.status).toBe(403);
  });

  test("a member may not delete a grant", async () => {
    const app = appWithRoleGuard((group) => managerRoutes(group, handlerStub));
    const res = await app.request(grantPath, {
      method: "DELETE",
      headers: { "x-test-role": "member" },
    });
    expect(res.status).toBe(403);
  });

  test("an owner may set a grant", async () => {
    const app = appWithRoleGuard((group) => managerRoutes(group, handlerStub));
    const res = await app.request(grantPath, {
      method: "PUT",
      headers: { "x-test-role": "owner", "content-type": "application/json" },
      body: JSON.stringify({ permission: "read_write" }),
    });
    expect(res.status).not.toBe(403);
    expect(res.status).toBe(200);
  });

  test("the member-facing routes do not register the grant writes", async () => {
    const app = new Hono<AppEnv>();
    const group = new Hono<AppEnv>();
    routes(group, handlerStub);
    app.route("/", group);

    const put = await app.request(grantPath, { method: "PUT" });
    const del = await app.request(grantPath, { method: "DELETE" });
    expect(put.status).toBe(404);
    expect(del.status).toBe(404);
  });
});
