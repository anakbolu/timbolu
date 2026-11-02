import { describe, expect, test } from "bun:test";
import { Handler } from "./handler.ts";
import { nopLogger } from "../../../platform/logger.ts";
import type { Service } from "../domain/service.ts";

describe("Content Origin Policy", () => {
  test("withContent sets frame-ancestors correctly", () => {
    const service = {} as Service;
    const handler = new Handler(service, nopLogger);
    handler.withContent({ frameAncestors: ["https://app.example.com"] });
    
    const policy = handler.contentPolicy();
    expect(policy).toContain("frame-ancestors https://app.example.com");
    expect(policy).not.toContain("frame-ancestors 'self'");
  });

  test("withContent default is 'self'", () => {
    const service = {} as Service;
    const handler = new Handler(service, nopLogger);
    const policy = handler.contentPolicy();
    expect(policy).toContain("frame-ancestors 'self'");
  });
});
