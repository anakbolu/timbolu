/**
 * The dashboard's "Tugas selesai hari ini" card.
 *
 * The card used to filter on `status === "done"`, a value the runtime never
 * writes: `done` is a `health`, derived from the status, so the filter could
 * never match and the card stayed at 0 even for a task that succeeded today
 * (#434). These cases pin the count to the stored status plus the day the task
 * finished, and pin that a failed run and yesterday's success stay out of it.
 *
 * `node:test` is what the repo's other TypeScript tests use; run this one with
 * Bun, because the module under test imports through the app's `@/` alias:
 *
 *   cd apps/frontend && bun test src/lib/dashboard-view.test.ts
 */

import assert from "node:assert/strict";
import test from "node:test";

import type { Task } from "@/lib/api";
import { buildStats, expressionOf, STATE_EXPRESSION } from "./dashboard-view";

const now = new Date(2026, 9, 30, 12, 0, 0);

function task(overrides: Partial<Task> = {}): Task {
  return {
    task_id: "task-1",
    agent_id: "agent-1",
    trigger: "chat",
    title: "cek pesanan",
    status: "succeeded",
    health: "done",
    depth: 0,
    step_count: 3,
    input_tokens: 0,
    output_tokens: 0,
    cost_micros: 0,
    created_at: new Date(2026, 9, 30, 9, 0, 0).toISOString(),
    finished_at: new Date(2026, 9, 30, 9, 0, 30).toISOString(),
    ...overrides,
  };
}

/** doneToday is the card's own number, read the way the dashboard reads it. */
function doneToday(tasks: Task[]): string {
  const stats = buildStats({ drafts: [], tasks, agents: [], status: "live", now });
  const card = stats.find((item) => item.label === "Tugas selesai hari ini");
  assert.ok(card, "the dashboard has no done-today card");
  return card.value;
}

test("buildStats counts a task that succeeded today", () => {
  assert.equal(doneToday([task()]), "1");
});

test("buildStats does not count a task that failed today", () => {
  // `health` is "done" on a failed run too, which is why the card must read the
  // status rather than the derived health.
  assert.equal(doneToday([task({ status: "failed", health: "done" })]), "0");
});

test("buildStats does not count a task that succeeded yesterday", () => {
  assert.equal(
    doneToday([task({ finished_at: new Date(2026, 9, 29, 9, 0, 30).toISOString() })]),
    "0",
  );
});

test("expressionOf draws an unknown state as idle", () => {
  // The server may publish a state before this client knows it. A smile would
  // tell a person the Bolu is fine on a state the screen cannot classify, so
  // the quiet face is the honest answer.
  for (const state of ["", "unknown", "WORKING"]) {
    assert.equal(
      expressionOf(state),
      STATE_EXPRESSION.idle,
      `state ${JSON.stringify(state)} should not be drawn as a known face`,
    );
  }
});

test("expressionOf keeps the face each known state already has", () => {
  assert.equal(expressionOf("working"), "senang");
  assert.equal(expressionOf("thinking"), "mikir");
  assert.equal(expressionOf("waiting"), "kaget");
  assert.equal(expressionOf("idle"), "bosan");
  assert.equal(expressionOf("resting"), "capek");
});
