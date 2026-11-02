/**
 * CORS fail-closed and origin reflection tests (#631).
 *
 * Verifies that:
 * 1. Production-like config with an empty origin list + evil origin fails closed
 *    (no Access-Control-Allow-Origin header).
 * 2. Production-like config with allowed origins returns the matching origin,
 *    and refuses unlisted origins.
 * 3. Local environment with an empty origin list reflects the caller's origin.
 * 4. newApp plumbs CORS configuration and environment through the middleware stack.
 */

import { describe, expect, test } from "bun:test";
import { Hono, type MiddlewareHandler } from "hono";

import type { AppEnv } from "../platform/context.ts";
import { nopLogger } from "../platform/logger.ts";
import { corsMiddleware, newApp } from "./http.ts";

const passthrough: () => MiddlewareHandler<AppEnv> = () => async (_c, next) => next();

describe("CORS (#631)", () => {
  test("production config with empty origin list fails closed", async () => {
    const app = new Hono<AppEnv>();
    app.use("*", corsMiddleware({ origins: [], environment: "production" }, nopLogger));
    app.get("/", (c) => c.text("ok"));

    const res = await app.request("/", {
      headers: { Origin: "https://evil.example" },
    });

    expect(res.status).toBe(200);
    expect(res.headers.get("access-control-allow-origin")).toBeNull();
  });

  test("production config with explicit origins allows listed and refuses unlisted", async () => {
    const app = new Hono<AppEnv>();
    app.use(
      "*",
      corsMiddleware(
        { origins: ["https://timbolu.com"], environment: "production" },
        nopLogger,
      ),
    );
    app.get("/", (c) => c.text("ok"));

    const allowedRes = await app.request("/", {
      headers: { Origin: "https://timbolu.com" },
    });
    expect(allowedRes.status).toBe(200);
    expect(allowedRes.headers.get("access-control-allow-origin")).toBe(
      "https://timbolu.com",
    );

    const evilRes = await app.request("/", {
      headers: { Origin: "https://evil.example" },
    });
    expect(evilRes.status).toBe(200);
    expect(evilRes.headers.get("access-control-allow-origin")).toBeNull();
  });

  test("local environment with empty origin list reflects caller origin", async () => {
    const app = new Hono<AppEnv>();
    app.use("*", corsMiddleware({ origins: [], environment: "local" }, nopLogger));
    app.get("/", (c) => c.text("ok"));

    const res = await app.request("/", {
      headers: { Origin: "http://localhost:3000" },
    });

    expect(res.status).toBe(200);
    expect(res.headers.get("access-control-allow-origin")).toBe(
      "http://localhost:3000",
    );
  });

  test("newApp plumbs CORS environment and fails closed on empty origins in production", async () => {
    const app = newApp({
      log: nopLogger,
      cors: {
        origins: [],
        environment: "production",
      },
      tracing: passthrough,
      requestContext: passthrough,
    });
    app.get("/", (c) => c.text("ok"));

    const res = await app.request("/", {
      headers: { Origin: "https://evil.example" },
    });

    expect(res.status).toBe(200);
    expect(res.headers.get("access-control-allow-origin")).toBeNull();
  });
});
