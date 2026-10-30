/**
 * The hub's assembly, checked at the seam only the container can see.
 *
 * The tool registry and the tool executor are two separate bridges into the same
 * hub, and neither can prove on its own that a Bolu's tool call reaches the order
 * module: the registry has to be populated with an adapter, the executor has to
 * be handed a hub whose `registry()` answers that same registry, and the tool the
 * Bolu holds has to be looked up through it. When any of those links is missing
 * the executor refuses the call with `task: tool is not available` — which is the
 * correct answer for a deployment with no hub, and the bug when there is one.
 *
 * Everything runs on stubs: the hub, the Bolu's grants, and the order module.
 * Nothing here needs Postgres, Temporal, or a network.
 */

import { describe, expect, test } from "bun:test";

import { taskToolExecutor, taskToolRegistry } from "./adapters.ts";
import { newContainer } from "./container.ts";
import { loadFromFile } from "../platform/config/init.ts";
import type { Database } from "../platform/database/index.ts";
import { newRegistry } from "../modules/integration/service/registry.ts";
import type { Tool as AgentToolRecord } from "../modules/agent/domain/agent.ts";
import type {
  Adapter as IntegrationAdapter,
  Registry as IntegrationRegistry,
  Result as IntegrationResult,
} from "../modules/integration/domain/adapter.ts";
import type {
  Integration as IntegrationRecord,
  Scope as IntegrationScope,
} from "../modules/integration/domain/integration.ts";
import {
  Adapter as OrderAdapter,
  App as OrderApp,
  ToolFindProduct,
  ToolReadOrders,
  ToolRecordOrder,
  type Service as OrderService,
} from "../modules/order/adapter/adapter.ts";
import {
  StatusRecorded,
  type Lookup,
  type NewOrder,
  type Order,
  type OrderFilter,
  type Scope as OrderScope,
} from "../modules/order/domain/order.ts";
import {
  ErrToolNotAvailable,
  LabelRead,
  is,
  type Scope as TaskScope,
  type ToolCall as TaskToolCall,
} from "../modules/task/domain/task.ts";

const workspaceId = "11111111-1111-1111-1111-111111111111";
const userId = "22222222-2222-2222-2222-222222222222";
const agentId = "33333333-3333-3333-3333-333333333333";
const taskId = "44444444-4444-4444-4444-444444444444";

/** One order the read tool answers with, so the module is visibly the source. */
const recordedOrder: Order = {
  id: "55555555-5555-5555-5555-555555555555",
  code: "ORD-7",
  status: StatusRecorded,
  note: "",
  agentId,
  taskId,
  customer: {
    id: "66666666-6666-6666-6666-666666666666",
    name: "Ani",
    phone: "0812",
    email: "",
    note: "",
    orders: 1,
    createdAt: new Date(0),
    updatedAt: new Date(0),
  },
  totalMicros: 30_000_000,
  lines: [
    {
      id: "77777777-7777-7777-7777-777777777777",
      orderId: "55555555-5555-5555-5555-555555555555",
      productId: "88888888-8888-8888-8888-888888888888",
      description: "Kopi Arabika",
      unit: "kg",
      quantity: 2,
      unitPriceMicros: 15_000_000,
      lineTotalMicros: 30_000_000,
    },
  ],
  createdAt: new Date("2026-10-30T03:00:00Z"),
  updatedAt: new Date("2026-10-30T03:00:00Z"),
};

/** The order module, reduced to what the adapter reads. */
class StubOrderService implements OrderService {
  /** Every filter the read tool asked with, in order. */
  readonly reads: OrderFilter[] = [];

  async lookup(): Promise<Lookup> {
    throw new Error("the lookup tool is not under test");
  }

  async record(): Promise<Order> {
    throw new Error("the recording tool is not under test");
  }

  async orders(_ctx: unknown, _scope: OrderScope, filter: OrderFilter): Promise<Order[]> {
    this.reads.push(filter);
    return [recordedOrder];
  }
}

/** The four reads the executor makes of the hub. */
interface HubPorts {
  registry(): IntegrationRegistry | null;
  list(ctx: unknown, scope: IntegrationScope): Promise<IntegrationRecord[]>;
  allowedPermissions(
    ctx: unknown,
    scope: IntegrationScope,
    agentId: string,
  ): Promise<Map<string, string>>;
  execute(
    ctx: unknown,
    scope: IntegrationScope,
    integrationId: string,
    tool: string,
    args: string,
  ): Promise<IntegrationResult>;
}

/**
 * The hub as the executor reads it.
 *
 * `execute` is the one method that has to do something: it runs the adapter the
 * registry names, which is what `IntegrationService.execute` does for a
 * connectionless tool. The connection reads answer nothing because the order
 * tools need no account — and a call that asked for one would fail loudly here
 * rather than pass silently.
 */
function hubOver(registry: IntegrationRegistry | null): HubPorts {
  return {
    registry: () => registry,
    list: async () => [],
    allowedPermissions: async () => new Map(),
    async execute(_ctx, scope, integrationId, tool, args) {
      const found = registry?.lookup(tool) ?? null;
      if (!found) {
        throw new Error(`integration: no adapter answers ${tool}`);
      }
      return found.adapter.execute(undefined, {
        scope,
        integrationId,
        tool,
        arguments: args,
        token: { accessToken: "", refreshToken: "", expiresAt: null, scopes: [] },
      });
    },
  };
}

/** The Bolu's grant: one tool, and nothing else. */
const grantedOrderReader = {
  allowedTools: async (): Promise<AgentToolRecord[]> => [
    {
      name: ToolReadOrders,
      integration: OrderApp,
      label: LabelRead,
      description: "Baca pesanan terakhir workspace ini.",
      needsConnection: false,
    },
  ],
};

/** The scope a task's tool call runs under. */
const taskScope: TaskScope = { userId, workspaceId };

/** One call the model makes, naming the Bolu and the task it belongs to. */
const readOrdersCall: TaskToolCall = {
  taskId,
  agentId,
  callId: "call-1",
  name: ToolReadOrders,
  arguments: { limit: 5 },
  conversationId: "99999999-9999-9999-9999-999999999999",
};

/** Builds the registry the way the container does for the order module. */
function orderRegistry(orders: OrderService): IntegrationRegistry {
  const registry = newRegistry();
  registry.register(new OrderAdapter(orders));
  return registry;
}

describe("integration wiring", () => {
  test("the registry carries the order module's tools", () => {
    const orders = new StubOrderService();
    const adapter: IntegrationAdapter = new OrderAdapter(orders);
    const registry = newRegistry();
    registry.register(adapter);

    expect(registry.apps()).toContain(OrderApp);

    const found = registry.lookup(ToolReadOrders);
    expect(found).not.toBeNull();
    expect(found?.spec.name).toBe(ToolReadOrders);
    expect(found?.spec.label).toBe(LabelRead);
    expect(found?.adapter).toBe(adapter);

    // Every tool the adapter declares is registered, not just the one asked for.
    expect(registry.tools().map((spec) => spec.name)).toEqual([
      ToolFindProduct,
      ToolReadOrders,
      ToolRecordOrder,
    ]);
  });

  test("the executor runs a granted order tool instead of refusing it", async () => {
    const orders = new StubOrderService();
    const registry = orderRegistry(orders);
    const hub = hubOver(registry);

    const tools = taskToolRegistry(grantedOrderReader, hub);
    const executor = taskToolExecutor(hub, tools);
    expect(tools).not.toBeNull();
    expect(executor).not.toBeNull();

    // The offered schema is the adapter's own, which is what having a hub buys:
    // a deployment without one offers the open schema instead.
    const offered = await tools!.tools(undefined, taskScope, agentId);
    expect(offered).toHaveLength(1);
    expect(offered[0].Name).toBe(ToolReadOrders);
    expect(offered[0].Schema).toEqual(JSON.parse(registry.lookup(ToolReadOrders)!.spec.schema));

    const result = await executor!.execute(undefined, taskScope, readOrdersCall);

    expect(result.isError).toBe(false);
    expect(result.content).toEqual({
      pesanan: [
        {
          kode: recordedOrder.code,
          status: recordedOrder.status,
          total_rupiah: 30,
          dibuat: "2026-10-30",
          pelanggan: recordedOrder.customer.name,
          baris: [
            {
              produk: "Kopi Arabika",
              jumlah: 2,
              satuan: "kg",
              harga_satuan_rupiah: 15,
              total_rupiah: 30,
            },
          ],
        },
      ],
      jumlah: 1,
    });

    // The module behind the adapter really ran, with the filter the call named.
    expect(orders.reads).toEqual([{ customerPhone: "", limit: 5 }]);
  });

  test("the executor refuses the same call when no hub registry is assembled", async () => {
    const orders = new StubOrderService();
    // The hub exists but carries no registry: the shape a container that never
    // registered an adapter hands over, and the bug this wiring fixes.
    const hub = hubOver(null);

    const tools = taskToolRegistry(grantedOrderReader, hub);
    const executor = taskToolExecutor(hub, tools);
    expect(executor).not.toBeNull();

    const error = await executor!.execute(undefined, taskScope, readOrdersCall).then(
      () => null,
      (err: unknown) => err,
    );

    expect(is(error, ErrToolNotAvailable)).toBe(true);
    expect(orders.reads).toHaveLength(0);
  });

  test("the container hands the hub a registry that knows the order tools", async () => {
    // The bug #435 was here and nowhere else: the hub was built without a
    // registry, so every call was refused as unavailable. The pool is a stub
    // because assembling the container only stores it — no statement runs.
    const config = loadFromFile("");
    config.logging.level = "silent";
    config.database.url = "";
    config.redis.url = "";
    config.temporal.hostPort = "";
    config.storage.localRoot = "";
    config.kms.masterKey = "";
    config.llm.secretEncryptionKey = "";
    // No OAuth application is registered, so the adapters are the two that need
    // no account, and the assertion does not depend on the machine's env.
    config.integrations.gmailClientId = "";
    config.integrations.slackClientId = "";
    config.integrations.gitHubClientId = "";

    const container = await newContainer({ config, db: {} as unknown as Database });
    const hub = container.integrationService;
    if (!hub) throw new Error("the container built no integration service");

    const registry = hub.registry();
    if (!registry) throw new Error("the container's hub carries no registry");
    expect(registry.apps()).toContain(OrderApp);
    expect(registry.lookup(ToolReadOrders)?.spec.label).toBe(LabelRead);
  });
});
