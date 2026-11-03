/**
 * The billing role guards (#661): endpoints that spend workspace money
 * (POST /billing/subscription and POST /billing/topups) require a manager role
 * (owner or admin). Endpoints that read subscription info or pay an invoice
 * remain accessible to any workspace member.
 */

import { describe, expect, test } from "bun:test";
import { Hono } from "hono";

import type { AppEnv } from "../platform/context.ts";
import { nopLogger } from "../platform/logger.ts";
import { requestContext } from "../platform/middleware/request_context.ts";
import type { Container } from "./container.ts";
import { registerRoutes } from "./handler.ts";
import type { Invoice, Plan, Snapshot } from "../modules/billing/domain/billing.ts";
import type { Delivery, Service as BillingPort } from "../modules/billing/domain/service.ts";
import { Handler as BillingHandler, WebhookMaxBytes } from "../modules/billing/handler/handler.ts";

const workspaceId = "11111111-1111-1111-1111-111111111111";
const userId = "22222222-2222-2222-2222-222222222222";
const token = "test-token";

class StubBilling implements BillingPort {
  async readOnly(): Promise<boolean> {
    return false;
  }

  async snapshot(): Promise<Snapshot> {
    const now = new Date(0);
    return {
      plan: {
        id: "plan-free",
        key: "free",
        name: "Free",
        monthlyPriceIDR: 0,
        tokenQuota: 100_000,
        dailyCostMicros: 10_000,
        periodDays: 30,
        maxAgents: 1,
        maxIntegrations: 1,
        maxMembers: 1,
        isTrial: false,
        sortOrder: 1,
      },
      subscription: {
        id: "sub-1",
        workspaceId,
        planId: "plan-free",
        status: "active",
        currentPeriodStart: now,
        currentPeriodEnd: now,
        graceUntil: null,
        pastDueSince: null,
        pendingPlanId: "",
        pastDueNotifiedAt: null,
        readOnlyNotifiedAt: null,
        canceledAt: null,
        createdAt: now,
        updatedAt: now,
      },
      pendingPlanKey: "",
      pendingPlanName: "",
      entitlement: "free",
      graceDays: 3,
      period: {
        id: "period-1",
        workspaceId,
        periodStart: now,
        periodEnd: now,
        tokensGranted: 100_000,
        tokensToppedUp: 0,
        tokensUsed: 0,
        warning80SentAt: null,
        warning100SentAt: null,
        createdAt: now,
        updatedAt: now,
      },
      outstanding: [],
      invoices: [],
      topUps: [],
      usageToday: {
        tokens: 0,
        costMicros: 0,
      },
      limits: {
        maxAgents: 1,
        maxIntegrations: 1,
        maxMembers: 1,
      },
      readOnly: false,
    };
  }

  async plans(): Promise<Plan[]> {
    return [];
  }

  async change(): Promise<Snapshot> {
    return this.snapshot();
  }
  async invoices(): Promise<Invoice[]> {
    return [];
  }

  async pay(): Promise<Invoice> {
    return {
      id: "99999999-9999-9999-9999-999999999999",
      workspaceId,
      subscriptionId: "88888888-8888-8888-8888-888888888888",
      kind: "subscription",
      provider: "xendit",
      providerReference: "inv-1",
      amountIDR: 100_000,
      tokenAmount: 0,
      status: "paid",
      periodStart: new Date(0),
      periodEnd: new Date(0),
      instructions: "",
      attempts: 0,
      lastError: "",
      expiresAt: null,
      paidAt: new Date(0),
      createdAt: new Date(0),
    };
  }

  async buyTopUp(): Promise<Invoice> {
    return {
      id: "99999999-9999-9999-9999-999999999999",
      workspaceId,
      subscriptionId: "88888888-8888-8888-8888-888888888888",
      kind: "topup",
      provider: "xendit",
      providerReference: "topup-1",
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

  lastDelivery: Delivery | null = null;
  handlePaymentCalls = 0;

  async handlePayment(delivery: Delivery): Promise<void> {
    this.handlePaymentCalls++;
    this.lastDelivery = delivery;
  }

  async check(): Promise<never> {
    throw new Error("quota check not under test");
  }

  async limits(): Promise<never> {
    throw new Error("limits not under test");
  }

  async sweep(): Promise<never> {
    throw new Error("sweep not under test");
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

function newAppWithBilling(resolvedRole: string): { app: Hono<AppEnv>; billing: StubBilling } {
  const billing = new StubBilling();
  const workspacePort = {
    async resolve(): Promise<string> {
      return resolvedRole;
    },
  };

  const container = {
    config: {
      http: { apiPrefix: "/api" },
      auth: { requireEmailVerification: false },
    },
    log: nopLogger,
    auth: null,
    authService: authPort,
    workspaceService: workspacePort,
    billing: new BillingHandler(billing, nopLogger),
    billingService: billing,
  } as unknown as Container;

  const app = new Hono<AppEnv>();
  app.use("*", requestContext(nopLogger));
  registerRoutes(app, container);
  return { app, billing };
}

function newApp(resolvedRole: string): Hono<AppEnv> {
  return newAppWithBilling(resolvedRole).app;
}

const headers = {
  Authorization: `Bearer ${token}`,
  "X-Workspace-Id": workspaceId,
  "Content-Type": "application/json",
};

describe("billing role guards (#661)", () => {
  test("member -> 403 on POST /billing/subscription", async () => {
    const app = newApp("member");
    const res = await app.request("/api/billing/subscription", {
      method: "POST",
      headers,
      body: JSON.stringify({ tier: "pro" }),
    });
    expect(res.status).toBe(403);
  });

  test("member -> 403 on POST /billing/topups", async () => {
    const app = newApp("member");
    const res = await app.request("/api/billing/topups", {
      method: "POST",
      headers,
      body: JSON.stringify({ package_key: "topup-small" }),
    });
    expect(res.status).toBe(403);
  });

  test("member -> not 403 on a read or a payment (#661)", async () => {
    const headers = { authorization: `Bearer ${token}`, "x-workspace-id": workspaceId };
    const app = newApp("member");

    const snapshotRes = await app.request("/api/billing/subscription", { headers });
    expect(snapshotRes.status).toBe(200);

    // A zero uuid is a real invoice id the stub answers, so this reaches the handler
    // instead of stopping at input validation. Paying is open to every member: a
    // workspace made read-only still has to be able to settle its invoice (#661).
    const payRes = await app.request(
      "/api/billing/invoices/00000000-0000-0000-0000-000000000000/pay",
      { method: "POST", headers },
    );
    expect(payRes.status).not.toBe(403);
    expect(payRes.status).not.toBe(401);
  });
});
describe("billing webhook body limit (#653)", () => {
  test("Content-Length > 64 KiB -> 413 dan service.handlePayment tidak dipanggil", async () => {
    const { app, billing } = newAppWithBilling("member");
    const res = await app.request("/api/billing/webhooks/duitku", {
      method: "POST",
      headers: {
        "Content-Type": "application/x-www-form-urlencoded",
        "Content-Length": String(WebhookMaxBytes + 1),
      },
      body: "merchantCode=D1234&amount=10000",
    });

    expect(res.status).toBe(413);
    const body = (await res.json()) as { success: boolean; error?: { code: string } };
    expect(body.success).toBe(false);
    expect(body.error?.code).toBe("payload_too_large");
    expect(billing.handlePaymentCalls).toBe(0);
  });

  test("streamed body tanpa Content-Length > 64 KiB -> 413 dan service.handlePayment tidak dipanggil", async () => {
    const { app, billing } = newAppWithBilling("member");
    const oversized = new Uint8Array(WebhookMaxBytes + 10);
    oversized.fill(65);

    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(oversized);
        controller.close();
      },
    });

    const res = await app.request("/api/billing/webhooks/duitku", {
      method: "POST",
      headers: {
        "Content-Type": "application/x-www-form-urlencoded",
      },
      body: stream,
      duplex: "half",
    });

    expect(res.status).toBe(413);
    const body = (await res.json()) as { success: boolean; error?: { code: string } };
    expect(body.success).toBe(false);
    expect(body.error?.code).toBe("payload_too_large");
    expect(billing.handlePaymentCalls).toBe(0);
  });

  test("body normal <= 64 KiB -> 200 dan byte yang diterima service identik", async () => {
    const { app, billing } = newAppWithBilling("member");
    const rawPayload = "merchantCode=D1234&amount=50000&merchantOrderId=INV-1&resultCode=00";
    const expectedBytes = new TextEncoder().encode(rawPayload);

    const res = await app.request("/api/billing/webhooks/duitku", {
      method: "POST",
      headers: {
        "Content-Type": "application/x-www-form-urlencoded",
      },
      body: rawPayload,
    });

    expect(res.status).toBe(200);
    const body = (await res.json()) as { success: boolean };
    expect(body.success).toBe(true);
    expect(billing.handlePaymentCalls).toBe(1);
    expect(billing.lastDelivery).not.toBeNull();
    expect(billing.lastDelivery?.provider).toBe("duitku");
    expect(billing.lastDelivery?.body).toEqual(expectedBytes);
  });
});
