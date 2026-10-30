/**
 * The device wire: the status and code a client branches on, and the payload it
 * reads back.
 *
 * The rules are the service's and are tested there. Here the service is a fake
 * that records what the handler asked it, so a handler that built its own answer
 * instead of calling the service fails these cases — which is the bug this
 * module's wiring is checked against.
 */

import { describe, expect, test } from "bun:test";
import { Hono } from "hono";

import type { AppEnv } from "../../../platform/context.ts";
import { nopLogger } from "../../../platform/logger.ts";
import {
  ErrInvalidInput,
  ErrNotFound,
  wrap,
  type Device,
  type RegisterRequest,
  type Scope,
  type Service,
} from "../domain/device.ts";
import { Handler, routes } from "./handler.ts";

const userId = "22222222-2222-2222-2222-222222222222";
const workspaceId = "11111111-1111-1111-1111-111111111111";

const stored: Device = {
  id: "33333333-3333-3333-3333-333333333333",
  userId,
  workspaceId,
  platform: "ios",
  token: "ExponentPushToken[abc123]",
  appVersion: "1.0.0",
  lastSeenAt: new Date("2026-10-30T09:00:00Z"),
  createdAt: new Date("2026-10-30T09:00:00Z"),
};

/** Answers with `stored`, or throws whatever the case installed. */
class FakeService implements Service {
  readonly registered: Array<{ scope: Scope; request: RegisterRequest }> = [];
  readonly removed: Array<{ scope: Scope; token: string }> = [];
  registerError: Error | null = null;
  removeError: Error | null = null;

  async register(scope: Scope, request: RegisterRequest): Promise<Device> {
    this.registered.push({ scope, request });
    if (this.registerError) throw this.registerError;
    return {
      ...stored,
      platform: request.platform,
      token: request.token,
      appVersion: request.appVersion,
    };
  }

  async remove(scope: Scope, token: string): Promise<void> {
    this.removed.push({ scope, token });
    if (this.removeError) throw this.removeError;
  }
}

/** Mounts the routes behind the identity the container's guards would have set. */
function newApp(service: Service): Hono<AppEnv> {
  const app = new Hono<AppEnv>();
  app.use("*", async (c, next) => {
    c.set("userId", userId);
    c.set("scope", { userId, workspaceId });
    await next();
  });
  routes(app, new Handler(service, nopLogger));
  return app;
}

interface Envelope {
  success: boolean;
  message: string;
  data?: Record<string, unknown>;
  error?: { code: string };
}

async function decode(res: Response): Promise<Envelope> {
  return (await res.json()) as Envelope;
}

describe("POST /devices", () => {
  test("answers 201 with the stored device", async () => {
    const service = new FakeService();
    const res = await newApp(service).request("/devices", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ token: "ExponentPushToken[abc123]", platform: "ios", app_version: "1.0.0" }),
    });

    expect(res.status).toBe(201);
    expect(service.registered).toEqual([
      {
        scope: { userId, workspaceId },
        request: { token: "ExponentPushToken[abc123]", platform: "ios", appVersion: "1.0.0" },
      },
    ]);

    const body = await decode(res);
    expect(body.success).toBe(true);
    expect(body.message).toBe("perangkat terdaftar");
    expect(body.data).toEqual({
      device_id: stored.id,
      workspace_id: workspaceId,
      platform: "ios",
      token: "ExponentPushToken[abc123]",
      app_version: "1.0.0",
      last_seen_at: "2026-10-30T09:00:00Z",
      created_at: "2026-10-30T09:00:00Z",
    });
  });

  test("leaves out the values a registration did not carry", async () => {
    const service = new FakeService();
    const res = await newApp(service).request("/devices", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ token: "ExponentPushToken[abc123]", platform: "ios" }),
    });

    const body = await decode(res);
    expect(body.data?.app_version).toBeUndefined();
  });

  test("answers 422 validation_failed with the service's Indonesian sentence", async () => {
    const service = new FakeService();
    service.registerError = wrap(ErrInvalidInput, "platform harus ios atau android");

    const res = await newApp(service).request("/devices", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ token: "ExponentPushToken[abc123]", platform: "web" }),
    });

    expect(res.status).toBe(422);
    const body = await decode(res);
    expect(body.success).toBe(false);
    expect(body.message).toBe("platform harus ios atau android");
    expect(body.error?.code).toBe("validation_failed");
  });

  test("answers 400 invalid_request for a body that is not JSON", async () => {
    const service = new FakeService();

    const res = await newApp(service).request("/devices", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{",
    });

    expect(res.status).toBe(400);
    expect((await decode(res)).error?.code).toBe("invalid_request");
    expect(service.registered).toHaveLength(0);
  });
});

describe("DELETE /devices/:token", () => {
  test("answers 204 with no body", async () => {
    const service = new FakeService();
    const res = await newApp(service).request("/devices/ExponentPushToken%5Babc123%5D", { method: "DELETE" });

    expect(res.status).toBe(204);
    expect(await res.text()).toBe("");
    expect(service.removed).toEqual([
      { scope: { userId, workspaceId }, token: "ExponentPushToken[abc123]" },
    ]);
  });

  test("answers 404 not_found for a token that is not the caller's", async () => {
    const service = new FakeService();
    service.removeError = ErrNotFound;

    const res = await newApp(service).request("/devices/ExponentPushToken%5Babc123%5D", { method: "DELETE" });

    expect(res.status).toBe(404);
    const body = await decode(res);
    expect(body.message).toBe("perangkat tidak ditemukan");
    expect(body.error?.code).toBe("not_found");
  });
});
