/**
 * Tests for routine preview state resolution (#697).
 *
 * Verifies that the preview helper distinguishes between in-flight requests,
 * server failures, empty run schedules, and trigger routines, preventing
 * failed requests from being rendered as "menghitung waktu berikutnya…".
 *
 * Run with Bun:
 *   cd apps/frontend && bun test src/lib/routine-preview.test.ts
 */

import assert from "node:assert/strict";
import test from "node:test";

import type { ApiResult } from "@/lib/api";
import { resolveRoutinePreview } from "./routine-preview";

test("resolveRoutinePreview maps kind:'trigger' to idle regardless of result", () => {
  const result: ApiResult<{ next_runs: string[] }> = {
    ok: true,
    status: 200,
    data: { next_runs: ["2026-11-03T09:00:00.000Z"] },
  };

  const state = resolveRoutinePreview({
    kind: "trigger",
    busy: false,
    result,
  });

  assert.deepEqual(state, { state: "idle" });
});

test("resolveRoutinePreview maps in-flight request to busy", () => {
  const state = resolveRoutinePreview({
    kind: "daily",
    busy: true,
    result: null,
  });

  assert.deepEqual(state, { state: "busy" });
});

test("resolveRoutinePreview maps { ok: false, message } to failed with that reason (never busy)", () => {
  const state = resolveRoutinePreview({
    kind: "daily",
    busy: false,
    result: {
      ok: false,
      status: 402,
      message: "Ruang kerja hanya bisa dibaca (read-only) karena langganan berakhir.",
    },
  });

  assert.deepEqual(state, {
    state: "failed",
    reason: "Ruang kerja hanya bisa dibaca (read-only) karena langganan berakhir.",
  });
  // Must NOT be busy or idle so the component never renders "menghitung waktu berikutnya…"
  assert.notEqual(state.state, "busy");
  assert.notEqual(state.state, "idle");
});

test("resolveRoutinePreview maps { ok: true, next_runs: [...] } to ready with runs", () => {
  const runs = [
    "2026-11-03T09:00:00.000Z",
    "2026-11-04T09:00:00.000Z",
  ];

  const state = resolveRoutinePreview({
    kind: "daily",
    busy: false,
    result: {
      ok: true,
      status: 200,
      data: { next_runs: runs },
    },
  });

  assert.deepEqual(state, {
    state: "ready",
    runs,
  });
});

test("resolveRoutinePreview maps { ok: true, next_runs: [] } to empty-but-settled (not busy)", () => {
  const state = resolveRoutinePreview({
    kind: "hourly_range",
    busy: false,
    result: {
      ok: true,
      status: 200,
      data: { next_runs: [] },
    },
  });

  assert.deepEqual(state, { state: "empty" });
  assert.notEqual(state.state, "busy");
});

test("resolveRoutinePreview maps null result when not busy to idle", () => {
  const state = resolveRoutinePreview({
    kind: "daily",
    busy: false,
    result: null,
  });

  assert.deepEqual(state, { state: "idle" });
});
