/**
 * The subscription guard: a workspace past its grace window may not write, and
 * it can still read everything it has.
 *
 * The guard is exercised through the real `registerRoutes`, because the bug this
 * pins was in the route table rather than in the rule (#522): the Bun tree that
 * production runs mounted `requireUser` and `requireTenant` on the tenant group
 * and nothing else, so a workspace that had stopped paying could still send a
 * message, approve a draft, and run a task.
 *
 * The two halves matter equally: the writes are refused, and a GET is always
 * answered — the promise is that no data is hidden. The billing routes are the
 * third edge: a workspace that cannot pay would be read-only forever if the one
 * call that fixes it were refused too.
 *
 * Everything runs on stubs: the auth, workspace, billing, device and checklist
 * ports are in-memory, so nothing opens Postgres, Redis, or a socket.
 */

import { describe, expect, test } from "bun:test";
import { Hono } from "hono";

import type { AppEnv } from "../platform/context.ts";
import { nopLogger } from "../platform/logger.ts";
import { requestContext } from "../platform/middleware/request_context.ts";
import type { Container } from "./container.ts";
import { registerRoutes } from "./handler.ts";
import { isBillingPath } from "./middleware-auth.ts";
import { ErrSubscriptionNotFound, wrap, type Invoice } from "../modules/billing/domain/billing.ts";
import type { Service as BillingPort } from "../modules/billing/domain/service.ts";
import { Handler as BillingHandler } from "../modules/billing/handler/handler.ts";
import type { Device, RegisterRequest, Scope as DeviceScope, Service as DevicePort } from "../modules/device/domain/device.ts";
import { Handler as DeviceHandler } from "../modules/device/handler/handler.ts";
import type { ErasureService } from "../modules/retention/domain/erasure.ts";
import { Handler as RetentionHandler } from "../modules/retention/handler/handler.ts";
import type { Service as WorkspacePort, Workspace as WorkspaceRecord } from "../modules/workspace/domain/workspace.ts";
import { Handler as WorkspaceHandler } from "../modules/workspace/handler/handler.ts";

const workspaceId = "11111111-1111-1111-1111-111111111111";
const userId = "22222222-2222-2222-2222-222222222222";
const token = "member-token";

const headers = { Authorization: `Bearer ${token}`, "X-Workspace-Id": workspaceId };

/** The subscription the guard reads, and a top-up it must never be asked for on a read. */
class StubBilling implements BillingPort {
  /** How many times the guard asked, which a read must not do. */
  reads = 0;
  /** How many top-ups the billing route answered, which the exemption must allow. */
  topUps = 0;

  constructor(private readonly answer: () => Promise<boolean>) {}

  async readOnly(): Promise<boolean> {
    this.reads++;
    return this.answer();
  }

  async buyTopUp(): Promise<Invoice> {
    this.topUps++;
    return {
      id: "99999999-9999-9999-9999-999999999999",
      workspaceId,
      subscriptionId: "88888888-8888-8888-8888-888888888888",
      kind: "topup",
      provider: "",
      providerReference: "",
      amountIDR: 50_000,
      tokenAmount: 1_000_000,
      status: "pending",
      periodStart: new Date(0),
      periodEnd: new Date(0),
      instructions: "",
      attempts: 0,
      lastError: "",
      expiresAt: null,
      paidAt: null,
      createdAt: new Date(0),
    };
  }

  async snapshot(): Promise<never> {
    throw new Error("the subscription screen is not under test");
  }
  async plans(): Promise<never> {
    throw new Error("the plan list is not under test");
  }
  async change(): Promise<never> {
    throw new Error("a plan change is not under test");
  }
  async invoices(): Promise<never> {
    throw new Error("the invoice list is not under test");
  }
  async pay(): Promise<never> {
    throw new Error("paying an invoice is not under test");
  }
  async handlePayment(): Promise<never> {
    throw new Error("a payment webhook is not under test");
  }
  async check(): Promise<never> {
    throw new Error("the quota check is not under test");
  }
  async limits(): Promise<never> {
    throw new Error("the plan limits are not under test");
  }
  async sweep(): Promise<never> {
    throw new Error("the reconciliation sweep is not under test");
  }
}

/** The push-address registry, reduced to counting registrations. */
class StubDevices implements DevicePort {
  registrations = 0;

  async register(_scope: DeviceScope, request: RegisterRequest): Promise<Device> {
    this.registrations++;
    return {
      id: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
      userId,
      workspaceId,
      platform: request.platform,
      token: request.token,
      appVersion: request.appVersion,
      lastSeenAt: new Date(0),
      createdAt: new Date(0),
    };
  }

  async remove(): Promise<void> {
    throw new Error("unregistering is not under test");
  }
}

/** The workspace module, reduced to the read the guard lets through. */
class StubWorkspaces implements WorkspacePort {
  async current(): Promise<WorkspaceRecord> {
    return {
      id: workspaceId,
      name: "Toko Ani",
      businessField: "retail",
      timezone: "Asia/Jakarta",
      language: "id",
      workHoursStart: "08:00",
      workHoursEnd: "17:00",
      workDays: [1, 2, 3, 4, 5],
      role: "owner",
      createdAt: new Date(0),
    };
  }
  async resolve(): Promise<string> {
    return "owner";
  }
  async onboard(): Promise<never> {
    throw new Error("onboarding is not under test");
  }
  async workspaces(): Promise<never> {
    throw new Error("the workspace list is not under test");
  }
  async update(): Promise<never> {
    throw new Error("a workspace update is not under test");
  }
  async teams(): Promise<never> {
    throw new Error("the team list is not under test");
  }
  async members(): Promise<never> {
    throw new Error("the member list is not under test");
  }
  async setRole(): Promise<never> {
    throw new Error("a role change is not under test");
  }
  async removeMember(): Promise<never> {
    throw new Error("removing a member is not under test");
  }
  async invite(): Promise<never> {
    throw new Error("an invitation is not under test");
  }
  async invitations(): Promise<never> {
    throw new Error("the invitation list is not under test");
  }
  async revokeInvitation(): Promise<never> {
    throw new Error("revoking an invitation is not under test");
  }
  async acceptInvitation(): Promise<never> {
    throw new Error("accepting an invitation is not under test");
  }
}

/** The erasure endpoints, which are the owner-group write in this suite. */
class StubErasure implements ErasureService {
  async erase(): Promise<never> {
    throw new Error("erasing is not under test");
  }
  async findErasure(): Promise<never> {
    throw new Error("reading an erasure is not under test");
  }
  async erasures(): Promise<never> {
    throw new Error("listing erasures is not under test");
  }
}

const authPort = {
  async authenticate(): Promise<{ id: string; email: string }> {
    return { id: userId, email: "member@example.com" };
  },
  async currentUser(): Promise<{ id: string; email: string; role: string }> {
    return { id: userId, email: "member@example.com", role: "user" };
  },
  async assertVerified(): Promise<void> {},
};

const workspacePort = {
  async resolve(): Promise<string> {
    return "owner";
  },
};

interface Harness {
  app: Hono<AppEnv>;
  billing: StubBilling | null;
  devices: StubDevices;
}

/**
 * The route table over a container whose modules are stubs. `subscription` is
 * what the guard reads; `null` means the deployment has no billing wired.
 */
function newApp(subscription: (() => Promise<boolean>) | null): Harness {
  const billing = subscription === null ? null : new StubBilling(subscription);
  const devices = new StubDevices();

  const container = {
    config: {
      http: { apiPrefix: "/api" },
      auth: { requireEmailVerification: false },
    },
    log: nopLogger,
    auth: null,
    authService: authPort,
    workspaceService: workspacePort,
    workspace: new WorkspaceHandler(new StubWorkspaces(), nopLogger),
    billing: billing === null ? null : new BillingHandler(billing, nopLogger),
    billingService: billing,
    device: new DeviceHandler(devices, nopLogger),
    retention: new RetentionHandler(new StubErasure(), nopLogger),
  } as unknown as Container;

  const app = new Hono<AppEnv>();
  // Production installs this first; the guard logs through the logger it binds.
  app.use("*", requestContext(nopLogger));
  registerRoutes(app, container);
  return { app, billing, devices };
}

interface Envelope {
  success: boolean;
  message: string;
  data?: Record<string, unknown>;
  error?: { code: string };
}

const registerDevice = { method: "POST", headers, body: JSON.stringify({ token: "push-1", platform: "android" }) };

describe("the subscription guard", () => {
  test("answers a read from a lapsed workspace and never asks the subscription about it", async () => {
    const { app, billing } = newApp(async () => true);

    const res = await app.request("/api/workspaces/current", { headers });
    expect(res.status).toBe(200);
    expect(((await res.json()) as Envelope).success).toBe(true);

    // A read is not a write, so the guard does not even reach the store.
    expect(billing!.reads).toBe(0);
  });

  test("refuses a write from a lapsed workspace with the read-only answer", async () => {
    const { app, devices } = newApp(async () => true);

    const res = await app.request("/api/devices", registerDevice);
    expect(res.status).toBe(402);

    const body = (await res.json()) as Envelope;
    expect(body.success).toBe(false);
    expect(body.message).toContain("mode baca saja");
    expect(body.error?.code).toBe("http_error");

    // The handler never ran, which is what "refused" means here.
    expect(devices.registrations).toBe(0);
  });

  test("refuses an owner-only write from a lapsed workspace too", async () => {
    const { app } = newApp(async () => true);

    // The role groups are separate groups, so the guard has to be mounted on
    // each of them; this is the one that would silently keep writing.
    const res = await app.request("/api/workspaces/current/erasure", { method: "POST", headers });
    expect(res.status).toBe(402);
    expect(((await res.json()) as Envelope).message).toContain("mode baca saja");
  });

  test("keeps the billing routes open so a workspace can pay its way out", async () => {
    const { app, billing } = newApp(async () => true);

    const res = await app.request("/api/billing/topups", {
      method: "POST",
      headers: { ...headers, "Content-Type": "application/json" },
      body: JSON.stringify({ package_key: "topup-small" }),
    });
    expect(res.status).toBe(201);
    expect(billing!.topUps).toBe(1);
    // The exemption is decided by path, before the store is asked.
    expect(billing!.reads).toBe(0);
  });

  test("lets a workspace inside its period write", async () => {
    const { app, devices } = newApp(async () => false);

    const res = await app.request("/api/devices", registerDevice);
    expect(res.status).toBe(201);
    expect(devices.registrations).toBe(1);
  });

  test("refuses a write whose subscription could not be read", async () => {
    const { app, devices } = newApp(async () => {
      throw new Error("the subscription store is down");
    });

    // Nothing here knows whether the workspace lapsed, so the write is refused
    // rather than opened on a guess.
    const res = await app.request("/api/devices", registerDevice);
    expect(res.status).toBe(402);
    expect(devices.registrations).toBe(0);
  });

  test("allows a write when the workspace has no subscription row at all", async () => {
    const { app, devices } = newApp(async () => {
      throw wrap(ErrSubscriptionNotFound, "the workspace has no live subscription");
    });

    // A workspace with no package has not lapsed: there is nothing it stopped
    // paying for. The row is what needs fixing, and it is logged rather than
    // acted on.
    const res = await app.request("/api/devices", registerDevice);
    expect(res.status).toBe(201);
    expect(devices.registrations).toBe(1);
  });

  test("never refuses a write when billing is not wired", async () => {
    const { app, devices } = newApp(null);

    const res = await app.request("/api/devices", registerDevice);
    expect(res.status).toBe(201);
    expect(devices.registrations).toBe(1);
  });
});

describe("isBillingPath", () => {
  test("recognises the billing prefix and nothing that merely starts with it", () => {
    expect(isBillingPath("/billing")).toBe(true);
    expect(isBillingPath("/billing/subscription")).toBe(true);
    expect(isBillingPath("/billingfoo")).toBe(false);
    expect(isBillingPath("/bill")).toBe(false);
    expect(isBillingPath("/agents")).toBe(false);
  });
});
