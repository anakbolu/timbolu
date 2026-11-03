/**
 * Readiness rules for health components.
 *
 * The API process cannot serve traffic without its primary store, so a missing or
 * unreachable database drops the overall readiness status to unavailable.
 * Redis remains optional (not_configured does not fail readiness, but unavailable does).
 * Realtime is informative only: a broken live path is visible without failing readiness
 * to avoid an orchestrator restart loop for an API that still serves HTTP requests.
 */

import { describe, expect, test } from "bun:test";

import type { Checker, Component, RealtimeProbe, Report, Repository } from "../domain/health.ts";
import { Service } from "./service.ts";

/** Answers with a prepared outcome and counts the calls. */
class FakeProbe implements RealtimeProbe {
  calls = 0;

  constructor(private readonly failure: Error | null = null) {}

  async probe(): Promise<void> {
    this.calls++;
    if (this.failure) throw this.failure;
  }
}

class FakeChecker implements Checker {
  calls = 0;

  constructor(private readonly failure: Error | null = null) {}

  async ping(): Promise<void> {
    this.calls++;
    if (this.failure) throw this.failure;
  }
}

class FakeRepository implements Repository {
  calls = 0;

  constructor(private readonly failure: Error | null = null) {}

  async ping(): Promise<void> {
    this.calls++;
    if (this.failure) throw this.failure;
  }
}

function component(report: Report, name: string): Component | undefined {
  return report.components?.find((c) => c.name === name);
}

describe("readiness", () => {
  test("reports unavailable when all dependencies are null", async () => {
    const report = await new Service(null, null, null).readiness();

    expect(component(report, "database")).toEqual({ name: "database", status: "not_configured" });
    expect(component(report, "redis")).toEqual({ name: "redis", status: "not_configured" });
    expect(component(report, "realtime")).toEqual({ name: "realtime", status: "not_configured" });
    expect(report.status).toBe("unavailable");
  });

  test("reports unavailable when database is not_configured and redis/realtime are not_configured", async () => {
    const report = await new Service(null, null, null).readiness();

    expect(component(report, "database")).toEqual({ name: "database", status: "not_configured" });
    expect(report.status).toBe("unavailable");
  });

  test("reports ok when database is ok and redis/realtime are null", async () => {
    const db = new FakeRepository();
    const report = await new Service(db, null, null).readiness();

    expect(db.calls).toBe(1);
    expect(component(report, "database")).toEqual({ name: "database", status: "ok" });
    expect(component(report, "redis")).toEqual({ name: "redis", status: "not_configured" });
    expect(component(report, "realtime")).toEqual({ name: "realtime", status: "not_configured" });
    expect(report.status).toBe("ok");
  });

  test("reports realtime ok when database is ok and realtime probe succeeds", async () => {
    const db = new FakeRepository();
    const probe = new FakeProbe();
    const report = await new Service(db, null, probe).readiness();

    expect(db.calls).toBe(1);
    expect(probe.calls).toBe(1);
    expect(component(report, "database")).toEqual({ name: "database", status: "ok" });
    expect(component(report, "realtime")).toEqual({ name: "realtime", status: "ok" });
    expect(report.status).toBe("ok");
  });

  test("reports a failed realtime probe without failing readiness when database is ok", async () => {
    const db = new FakeRepository();
    const report = await new Service(db, null, new FakeProbe(new Error("pubsub: probe timed out"))).readiness();

    expect(db.calls).toBe(1);
    expect(component(report, "database")).toEqual({ name: "database", status: "ok" });
    expect(component(report, "realtime")).toEqual({
      name: "realtime",
      status: "unavailable",
      error: "pubsub: probe timed out",
    });
    // The API still serves requests, so the overall status stays ok.
    expect(report.status).toBe("ok");
  });

  test("reports unavailable when database ping fails", async () => {
    const db = new FakeRepository(new Error("connection refused"));
    const report = await new Service(db, null, null).readiness();

    expect(component(report, "database")).toEqual({
      name: "database",
      status: "unavailable",
      error: "connection refused",
    });
    expect(report.status).toBe("unavailable");
  });

  test("reports unavailable when redis ping fails even if database is ok", async () => {
    const db = new FakeRepository();
    const redis = new FakeChecker(new Error("redis down"));
    const report = await new Service(db, redis, null).readiness();

    expect(component(report, "database")).toEqual({ name: "database", status: "ok" });
    expect(component(report, "redis")).toEqual({
      name: "redis",
      status: "unavailable",
      error: "redis down",
    });
    expect(report.status).toBe("unavailable");
  });
});
