import type { AppContext } from "../../../platform/context.ts";
import { readProductRequest } from "./handler.ts";
import { describe, expect, test } from "bun:test";

describe("order handler product validation", () => {
  describe("readProductRequest", () => {
    test("should return null for non-JSON", async () => {
      const c = { req: { json: () => Promise.reject("error") } } as unknown as AppContext;
      expect(await readProductRequest(c)).toBeNull();
    });
    
    test("should accept valid number for price_idr", async () => {
      const body = { sku: "test", name: "test", price_idr: 1000 };
      const c = { req: { json: () => Promise.resolve(body) } } as unknown as AppContext;
      const req = await readProductRequest(c);
      expect(req?.price_idr).toBe(1000);
    });

    test("should return undefined for missing price_idr", async () => {
      const body = { sku: "test", name: "test" };
      const c = { req: { json: () => Promise.resolve(body) } } as unknown as AppContext;
      const req = await readProductRequest(c);
      expect(req?.price_idr).toBeUndefined();
    });

    test("should return undefined for string price_idr", async () => {
      const body = { sku: "test", name: "test", price_idr: "1000" };
      const c = { req: { json: () => Promise.resolve(body) } } as unknown as AppContext;
      const req = await readProductRequest(c);
      expect(req?.price_idr).toBeUndefined();
    });
    test("should return undefined for negative price_idr", async () => {
      const body = { sku: "test", name: "test", price_idr: -1000 };
      const c = { req: { json: () => Promise.resolve(body) } } as unknown as AppContext;
      const req = await readProductRequest(c);
      expect(req?.price_idr).toBeUndefined();
    });
  });
});
