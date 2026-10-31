/**
 * Readiness rules for the realtime component.
 *
 * A broken live path must be visible in the report without taking the process
 * out of rotation: an orchestrator that restarts an API which still serves
 * requests turns a silent realtime outage into a restart loop. These cases pin
 * that split, with fakes so they need neither Redis nor Postgres.
 */

import { describe, expect, test } from "bun:test";

import type { Component, RealtimeProbe, Report } from "../domain/health.ts";
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

function component(report: Report, name: string): Component | undefined {
  return report.components?.find((c) => c.name === name);
}

describe("readiness", () => {
  test("reports realtime ok when the round trip succeeds", async () => {
    const probe = new FakeProbe();
    const report = await new Service(null, null, probe).readiness();

    expect(probe.calls).toBe(1);
    expect(component(report, "realtime")).toEqual({ name: "realtime", status: "ok" });
    expect(report.status).toBe("ok");
  });

  test("reports a failed probe without failing readiness", async () => {
    const report = await new Service(null, null, new FakeProbe(new Error("pubsub: probe timed out"))).readiness();

    expect(component(report, "realtime")).toEqual({
      name: "realtime",
      status: "unavailable",
      error: "pubsub: probe timed out",
    });
    // The API still serves requests, so the overall status stays ok.
    expect(report.status).toBe("ok");
  });

  test("reports realtime not_configured without a probe", async () => {
    const report = await new Service(null, null, null).readiness();

    expect(component(report, "realtime")).toEqual({ name: "realtime", status: "not_configured" });
    expect(report.status).toBe("ok");
  });
});
