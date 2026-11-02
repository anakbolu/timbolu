import { describe, expect, test } from "bun:test";
import type { AppContext } from "../../../platform/context.ts";
import { nopLogger } from "../../../platform/logger.ts";
import type { NewProduct, Product, ProductChange, Scope, Service } from "../domain/order.ts";
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
