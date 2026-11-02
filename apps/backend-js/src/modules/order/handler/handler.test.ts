import { describe, expect, test } from "bun:test";
import type { AppContext } from "../../../platform/context.ts";
import type { Database, Tx } from "../../../platform/database/index.ts";
import { nopLogger } from "../../../platform/logger.ts";
import type { NewOrder, NewProduct, Order, Product, ProductChange, Scope, Service } from "../domain/order.ts";
import { orderCodeFrom, PostgresRepository } from "../repository/postgres.ts";
import { Handler } from "./handler.ts";

function createFakeProduct(overrides: Partial<Product> = {}): Product {
  return {
    id: "11111111-1111-1111-1111-111111111111",
    sku: "SKU-1",
    name: "Produk Contoh",
    unitPriceMicros: 50000 * 1_000_000,
    unit: "pcs",
    active: true,
    createdAt: new Date(),
    updatedAt: new Date(),
    ...overrides,
  };
}

function makeContext(body: unknown, paramId = "11111111-1111-1111-1111-111111111111"): AppContext {
  return {
    req: {
      json: async () => body,
      param: (_name?: string) => paramId,
      header: (_name?: string) => undefined,
      query: (_name?: string) => undefined,
    },
    get: (_key: string) => undefined,
    json: (b: unknown, s?: number) => new Response(JSON.stringify(b), { status: s }),
  } as unknown as AppContext;
}

describe("Handler product price validation", () => {
  test("createProduct: body tanpa price_idr -> 400 dan service tidak dipanggil", async () => {
    let called = false;
    const fakeService = {
      createProduct: async () => {
        called = true;
        return createFakeProduct();
      },
    } as unknown as Service;

    const handler = new Handler(fakeService, nopLogger);
    const c = makeContext({ sku: "A", name: "Produk A" });

    const res = await handler.createProduct(c);
    expect(res.status).toBe(400);
    expect(called).toBe(false);
  });

  test("createProduct: body price_idr string -> 400 dan service tidak dipanggil", async () => {
    let called = false;
    const fakeService = {
      createProduct: async () => {
        called = true;
        return createFakeProduct();
      },
    } as unknown as Service;

    const handler = new Handler(fakeService, nopLogger);
    const c = makeContext({ sku: "A", name: "Produk A", price_idr: "50000" });

    const res = await handler.createProduct(c);
    expect(res.status).toBe(400);
    expect(called).toBe(false);
  });

  test("createProduct: body price_idr -1 -> 400 dan service tidak dipanggil", async () => {
    let called = false;
    const fakeService = {
      createProduct: async () => {
        called = true;
        return createFakeProduct();
      },
    } as unknown as Service;

    const handler = new Handler(fakeService, nopLogger);
    const c = makeContext({ sku: "A", name: "Produk A", price_idr: -1 });

    const res = await handler.createProduct(c);
    expect(res.status).toBe(400);
    expect(called).toBe(false);
  });

  test("createProduct: body price_idr 50000 -> 201 dan unitPriceMicros diterima service = 50000 * 1_000_000", async () => {
    const received: { arg?: Omit<NewProduct, "active"> } = {};
    const fakeService = {
      createProduct: async (_ctx: AppContext, _scope: Scope, arg: Omit<NewProduct, "active">) => {
        received.arg = arg;
        return createFakeProduct({ unitPriceMicros: arg.unitPriceMicros });
      },
    } as unknown as Service;

    const handler = new Handler(fakeService, nopLogger);
    const c = makeContext({ sku: "A", name: "Produk A", price_idr: 50000 });

    const res = await handler.createProduct(c);
    expect(res.status).toBe(201);
    expect(received.arg).toBeDefined();
    expect(received.arg?.unitPriceMicros).toBe(50000 * 1_000_000);
  });

  test("updateProduct: body price_idr string -> 400; body tanpa price_idr -> 200 dan unitPriceMicros undefined", async () => {
    const updateCalls: ProductChange[] = [];
    const fakeService = {
      updateProduct: async (_ctx: AppContext, _scope: Scope, _id: string, arg: ProductChange) => {
        updateCalls.push(arg);
        return createFakeProduct();
      },
    } as unknown as Service;

    const handler = new Handler(fakeService, nopLogger);

    // 1. price_idr string -> 400, service tidak dipanggil
    const cInvalid = makeContext({ name: "Produk Baru", price_idr: "50000" });
    const resInvalid = await handler.updateProduct(cInvalid);
    expect(resInvalid.status).toBe(400);
    expect(updateCalls.length).toBe(0);

    // 2. body tanpa price_idr -> sukses (200), unitPriceMicros undefined
    const cValid = makeContext({ name: "Produk Baru" });
    const resValid = await handler.updateProduct(cValid);
    expect(resValid.status).toBe(200);
    expect(updateCalls.length).toBe(1);
    expect(updateCalls[0].unitPriceMicros).toBeUndefined();
  });
});

describe("orderCodeFrom (#670)", () => {
  test("menghasilkan P- diikuti 10 karakter hex kapital", () => {
    const sampleUuid = "12345678-abcd-ef01-2345-6789abcdef01";
    const code = orderCodeFrom(sampleUuid);

    expect(code).toBe("P-12345678AB");
    expect(code).toMatch(/^P-[0-9A-F]{10}$/);
    expect(code.length).toBe(12); // 'P-' (2) + 10 hex
  });
});

describe("PostgresRepository.record retry unik (#670)", () => {
  test("percobaan pertama throw unique violation orders_workspace_code_key, percobaan kedua berhasil", async () => {
    let insertOrderAttempts = 0;

    const fakeOrderRow = {
      id: "99999999-9999-9999-9999-999999999999",
      workspace_id: "ws-1",
      customer_id: "c-1",
      customer_phone: "6281234567890",
      customer_name: "Budi",
      agent_id: null,
      task_id: null,
      code: "P-1234567890",
      note: "",
      status: "recorded",
      total_micros: 50000000000,
      created_at: new Date(),
      updated_at: new Date(),
    };

    // Tagged template mock untuk tx
    const txMock = ((strings: TemplateStringsArray, ...values: unknown[]) => {
      const raw = strings.join(" ");
      if (raw.includes("INSERT INTO customers")) {
        return Promise.resolve([
          {
            id: "c-1",
            workspace_id: "ws-1",
            name: "Budi",
            phone: "6281234567890",
            email: "",
            note: "",
            created_at: new Date(),
            updated_at: new Date(),
          },
        ]);
      }
      if (raw.includes("INSERT INTO orders")) {
        insertOrderAttempts++;
        if (insertOrderAttempts === 1) {
          const err = Object.assign(
            new Error('duplicate key value violates unique constraint "orders_workspace_code_key"'),
            { code: "23505", constraint_name: "orders_workspace_code_key" },
          );
          return Promise.reject(err);
        }
        return Promise.resolve([fakeOrderRow]);
      }
      if (raw.includes("SELECT * FROM products")) {
        return Promise.resolve([
          {
            id: "p-1",
            workspace_id: "ws-1",
            sku: "SKU-1",
            name: "Kopi",
            unit_price_micros: 50000000000,
            unit: "cup",
            active: true,
            created_at: new Date(),
            updated_at: new Date(),
          },
        ]);
      }
      if (raw.includes("INSERT INTO order_items")) {
        return Promise.resolve([]);
      }
      if (raw.includes("UPDATE orders")) {
        return Promise.resolve([]);
      }
      if (raw.includes("SELECT * FROM orders WHERE id")) {
        return Promise.resolve([fakeOrderRow]);
      }
      if (raw.includes("SELECT * FROM order_items WHERE order_id")) {
        return Promise.resolve([
          {
            id: "item-1",
            workspace_id: "ws-1",
            order_id: fakeOrderRow.id,
            product_id: "p-1",
            description: "Kopi",
            unit: "cup",
            quantity: "1.000",
            unit_price_micros: 50000000000,
            line_total_micros: 50000000000,
          },
        ]);
      }
      return Promise.resolve([]);
    }) as unknown as Tx;

    const fakeDb = {
      inScope: async <T>(_scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> => {
        return fn(txMock);
      },
      inScopeRead: async <T>(_scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> => {
        return fn(txMock);
      },
    } as unknown as Database;

    const repo = new PostgresRepository(fakeDb);
    const newOrder: NewOrder = {
      customerPhone: "081234567890",
      customerName: "Budi",
      agentId: "",
      taskId: "",
      items: [{ product: "Kopi", quantity: 1, description: "", note: "" }],
      note: "",
    };

    const result = await repo.record({ workspaceId: "ws-1" }, newOrder);
    expect(insertOrderAttempts).toBe(2);
    expect(result.id).toBe(fakeOrderRow.id);
    expect(result.code).toBe(fakeOrderRow.code);
  });
});
