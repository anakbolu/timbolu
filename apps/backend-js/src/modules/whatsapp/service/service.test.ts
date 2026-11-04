/**
 * Unlinking a WhatsApp number: what the service clears, and how the HTTP surface
 * answers when a number is unlinked or absent.
 *
 * Everything runs against a fake repository and a fake session store, with no
 * Postgres, Redis, or gateway process.
 */

import { describe, expect, test } from "bun:test";
import { Hono } from "hono";

import type { AppEnv } from "../../../platform/context.ts";
import { nopLogger } from "../../../platform/logger.ts";
import {
  ErrNotConnected,
  ErrNotFound,
  NilUUID,
  StatusConnected,
  StatusDisconnected,
  wrap,
  type Inbound,
  type Link,
  type Media,
  type MessageFilter,
  type Number,
  type Outbox,
  type OutboxFilter,
  type Scope,
  type SessionForgetter,
  type Snapshot,
  type Store,
  type ToSend,
  type Transport,
} from "../domain/whatsapp.ts";
import { Handler, routes } from "../handler/handler.ts";
import { Service } from "./service.ts";

const userId = "22222222-2222-2222-2222-222222222222";
const workspaceId = "11111111-1111-1111-1111-111111111111";
const numberId = "33333333-3333-3333-3333-333333333333";
const missingNumberId = "44444444-4444-4444-4444-444444444444";
const agentId = "55555555-5555-5555-5555-555555555555";
const integrationId = "66666666-6666-6666-6666-666666666666";

function connectedNumber(): Number {
  const at = new Date("2026-10-30T09:00:00Z");
  return {
    id: numberId,
    workspaceID: workspaceId,
    integrationID: integrationId,
    phone: "628123456789",
    label: "WhatsApp ••••6789",
    agentID: agentId,
    status: StatusConnected,
    statusReason: "",
    tosAcceptedAt: at,
    hourlyLimit: 40,
    connectedAt: at,
    lastSeenAt: at,
    createdAt: at,
    updatedAt: at,
  };
}

/** An in-memory repository and session store for the unlink path. */
class FakeStore implements Store, SessionForgetter {
  readonly rows = new Map<string, Number>();
  readonly forgotten: Array<{ scope: Scope; numberID: string }> = [];
  readonly published: Array<{ scope: Scope; numberID: string; enabled: boolean }> = [];
  readonly statusUpdates: Array<{ scope: Scope; id: string; status: string; reason: string }> = [];

  constructor(initial: Number[] = [connectedNumber()]) {
    for (const row of initial) {
      this.rows.set(row.id, { ...row });
    }
  }

  async get(scope: Scope, id: string): Promise<Number> {
    const row = this.rows.get(id);
    if (!row || row.workspaceID !== scope.workspaceId) {
      throw wrap(ErrNotFound, "get number");
    }
    return { ...row };
  }

  async setStatus(scope: Scope, id: string, status: string, reason: string): Promise<Number> {
    this.statusUpdates.push({ scope, id, status, reason });
    const row = await this.get(scope, id);
    const updated: Number = { ...row, status, statusReason: reason };
    this.rows.set(id, updated);
    return { ...updated };
  }

  async publish(scope: Scope, numberID: string, enabled: boolean): Promise<void> {
    this.published.push({ scope, numberID, enabled });
  }

  async forgetSession(scope: Scope, numberID: string): Promise<void> {
    this.forgotten.push({ scope, numberID });
  }

  async create(): Promise<Number> {
    throw new Error("create is not under test");
  }
  async byPhone(): Promise<Number> {
    throw new Error("byPhone is not under test");
  }
  async byIntegration(): Promise<Number> {
    throw new Error("byIntegration is not under test");
  }
  async list(): Promise<Number[]> {
    throw new Error("list is not under test");
  }
  async update(): Promise<Number> {
    throw new Error("update is not under test");
  }
  async setIntegration(): Promise<void> {
    throw new Error("setIntegration is not under test");
  }
  async setPhone(): Promise<void> {
    throw new Error("setPhone is not under test");
  }
  async acceptTerms(): Promise<Number> {
    throw new Error("acceptTerms is not under test");
  }
  async touch(): Promise<void> {
    throw new Error("touch is not under test");
  }
  async delete(): Promise<void> {
    throw new Error("delete is not under test");
  }
  async record(): Promise<{ message: Inbound; created: boolean }> {
    throw new Error("record is not under test");
  }
  async messages(_scope: Scope, _filter: MessageFilter): Promise<Inbound[]> {
    throw new Error("messages is not under test");
  }
  async byWaID(): Promise<Inbound> {
    throw new Error("byWaID is not under test");
  }
  async messageByID(): Promise<Inbound> {
    throw new Error("messageByID is not under test");
  }
  async pendingMedia(): Promise<Inbound[]> {
    throw new Error("pendingMedia is not under test");
  }
  async setMedia(_scope: Scope, _id: string, _media: Media): Promise<void> {
    throw new Error("setMedia is not under test");
  }
  async attachTask(): Promise<void> {
    throw new Error("attachTask is not under test");
  }
  async enqueue(): Promise<Outbox> {
    throw new Error("enqueue is not under test");
  }
  async due(): Promise<Outbox[]> {
    throw new Error("due is not under test");
  }
  async markSent(): Promise<void> {
    throw new Error("markSent is not under test");
  }
  async markHeld(): Promise<void> {
    throw new Error("markHeld is not under test");
  }
  async markRetry(): Promise<void> {
    throw new Error("markRetry is not under test");
  }
  async markFailed(): Promise<void> {
    throw new Error("markFailed is not under test");
  }
  async sentSince(): Promise<number> {
    throw new Error("sentSince is not under test");
  }
  async outbox(_scope: Scope, _filter: OutboxFilter): Promise<Outbox[]> {
    throw new Error("outbox is not under test");
  }
  async reclaim(): Promise<number> {
    throw new Error("reclaim is not under test");
  }
}

/** A fake transport that records unlink calls. */
class FakeTransport implements Transport {
  readonly unlinked: Snapshot[] = [];
  unlinkError: Error | null = null;

  async unlink(snapshot: Snapshot): Promise<void> {
    this.unlinked.push(snapshot);
    if (this.unlinkError) throw this.unlinkError;
  }

  async link(): Promise<Link> {
    throw new Error("link is not under test");
  }
  async linkState(): Promise<Link> {
    throw new Error("linkState is not under test");
  }
  async pairCode(): Promise<Link> {
    throw new Error("pairCode is not under test");
  }
  async cancel(): Promise<void> {
    throw new Error("cancel is not under test");
  }
  async send(_snapshot: Snapshot, _message: ToSend): Promise<string> {
    throw new Error("send is not under test");
  }
  async typing(): Promise<void> {
    throw new Error("typing is not under test");
  }
  async online(): Promise<boolean> {
    throw new Error("online is not under test");
  }
}

/** Mounts the WhatsApp routes behind the identity the container's guards set. */
function newApp(service: Service): Hono<AppEnv> {
  const app = new Hono<AppEnv>();
  const api = new Hono<AppEnv>();
  api.use("*", async (c, next) => {
    c.set("userId", userId);
    c.set("scope", { userId, workspaceId });
    await next();
  });
  routes(api, new Handler(service, nopLogger));
  app.route("/api", api);
  return app;
}

interface Envelope {
  success: boolean;
  message: string;
  data?: Record<string, unknown>;
  error?: { code: string };
}


describe("DELETE /api/whatsapp/numbers/:numberID", () => {
  test("unlinking a number that does not exist answers 404", async () => {
    const store = new FakeStore([]);
    const service = new Service({ repository: store, sessions: store });
    const app = newApp(service);

    const res = await app.request(`/api/whatsapp/numbers/${missingNumberId}`, {
      method: "DELETE",
    });

    expect(res.status).toBe(404);
    const body = (await res.json()) as Envelope;
    expect(body.success).toBe(false);
    expect(body.error?.code).toBe("whatsapp_not_found");
    expect(store.forgotten).toHaveLength(0);
    expect(store.published).toHaveLength(0);
  });

  test("unlinking a linked number answers 200 with status disconnected", async () => {
    const store = new FakeStore();
    const transport = new FakeTransport();
    const disconnectedIntegrations: Array<{ scope: Scope; id: string }> = [];
    const service = new Service({
      repository: store,
      sessions: store,
      transport,
      connections: {
        link: async () => NilUUID,
        disconnect: async (scope, id) => {
          disconnectedIntegrations.push({ scope, id });
        },
      },
    });
    const app = newApp(service);

    const res = await app.request(`/api/whatsapp/numbers/${numberId}`, {
      method: "DELETE",
    });

    expect(res.status).toBe(200);
    const body = (await res.json()) as Envelope;
    expect(body.success).toBe(true);
    expect(body.message).toBe("number unlinked");
    expect(body.data?.id).toBe(numberId);
    expect(body.data?.status).toBe(StatusDisconnected);
    expect(body.data?.reason).toBe("dinonaktifkan");
    expect(body.data?.connected).toBe(false);

    expect(transport.unlinked).toEqual([
      {
        numberID: numberId,
        workspaceID: workspaceId,
        phone: "628123456789",
        session: null,
      },
    ]);
    expect(store.forgotten).toEqual([{ scope: { userId, workspaceId }, numberID: numberId }]);
    expect(store.published).toEqual([
      { scope: { userId, workspaceId }, numberID: numberId, enabled: false },
    ]);
    expect(disconnectedIntegrations).toEqual([
      { scope: { userId, workspaceId }, id: integrationId },
    ]);
  });

  test("unlinking succeeds with 200 even when no gateway transport is configured or online", async () => {
    const store = new FakeStore();
    const serviceWithoutTransport = new Service({
      repository: store,
      sessions: store,
      transport: null,
    });
    const appWithoutTransport = newApp(serviceWithoutTransport);

    const res = await appWithoutTransport.request(`/api/whatsapp/numbers/${numberId}`, {
      method: "DELETE",
    });

    expect(res.status).toBe(200);
    const body = (await res.json()) as Envelope;
    expect(body.success).toBe(true);
    expect(body.data?.status).toBe(StatusDisconnected);
    expect(store.forgotten).toEqual([{ scope: { userId, workspaceId }, numberID: numberId }]);
    expect(store.published).toEqual([
      { scope: { userId, workspaceId }, numberID: numberId, enabled: false },
    ]);

    // Also when a transport is present but reports the number is already offline.
    const offlineStore = new FakeStore();
    const offlineTransport = new FakeTransport();
    offlineTransport.unlinkError = ErrNotConnected;
    const serviceWithOfflineTransport = new Service({
      repository: offlineStore,
      sessions: offlineStore,
      transport: offlineTransport,
    });
    const appWithOfflineTransport = newApp(serviceWithOfflineTransport);

    const offlineRes = await appWithOfflineTransport.request(
      `/api/whatsapp/numbers/${numberId}`,
      { method: "DELETE" },
    );

    expect(offlineRes.status).toBe(200);
    const offlineBody = (await offlineRes.json()) as Envelope;
    expect(offlineBody.success).toBe(true);
    expect(offlineBody.data?.status).toBe(StatusDisconnected);
  });
});
