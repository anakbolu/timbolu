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
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

import { AttentionRow } from "@/components/dashboard/usage-view";
import type { AttentionTask, Task } from "@/lib/api";
import { buildStats, buildWeek, countPending, expressionOf, STATE_EXPRESSION } from "./dashboard-view";

const DASHBOARD_DIR = path.resolve(import.meta.dirname, "../components/dashboard");

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

test("buildWeek counts only succeeded tasks for a given day", () => {
  // Friday in the week of Monday 2026-10-26 .. Sunday 2026-11-01
  const fridayIso = new Date(2026, 9, 30, 10, 0, 0).toISOString();
  const tasks: Task[] = [
    task({ task_id: "task-succeeded-1", status: "succeeded", finished_at: fridayIso }),
    task({ task_id: "task-succeeded-2", status: "succeeded", finished_at: fridayIso }),
    task({ task_id: "task-failed", status: "failed", health: "done", finished_at: fridayIso }),
    task({ task_id: "task-canceled", status: "canceled", health: "done", finished_at: fridayIso }),
  ];

  const week = buildWeek(tasks, now);
  const fridayBar = week.find((bar) => bar.day === "Jum");
  assert.ok(fridayBar, "Friday bar not found in week chart");
  assert.equal(fridayBar.label, "2");
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

test("countPending counts only drafts with pending status", () => {
  const drafts = [
    { status: "pending" },
    { status: "approved" },
    { status: "pending" },
    { status: "rejected" },
  ] as Parameters<typeof countPending>[0];
  assert.equal(countPending(drafts), 2);
});

test("AttentionRow in Perlu perhatian renders href /approval/<id> for waiting_draft_id and UsageView reloads via load(workspace, days)", () => {
  const attentionTask: AttentionTask = {
    ...task({
      task_id: "task-wait-1",
      status: "waiting_approval",
      health: "waiting",
      waiting_draft_id: "draft-999",
      waiting_reason: "menunggu persetujuan kirim email",
    }),
    attention: "waiting",
  };

  const html = renderToStaticMarkup(createElement(AttentionRow, { task: attentionTask }));
  assert.ok(
    html.includes('href="/approval/draft-999"'),
    `Expected rendered AttentionRow to link to /approval/draft-999, got: ${html}`,
  );
  assert.ok(
    !html.includes('href="/dashboard"'),
    "Rendered AttentionRow must not link Lihat draf to /dashboard",
  );

  const content = fs.readFileSync(path.join(DASHBOARD_DIR, "usage-view.tsx"), "utf-8");
  assert.ok(
    !content.includes("reloadTasks"),
    "Muat ulang on Perlu perhatian must not call reloadTasks()",
  );
  assert.ok(
    content.includes("onClick={() => void reload()}") && content.includes("await load(workspace, days)"),
    "Muat ulang must re-invoke load(workspace, days)",
  );
});

test("dash-view Buka obrolan links to /chat?conversation=<id> or falls back to /chat?agent=<id>", () => {
  const content = fs.readFileSync(path.join(DASHBOARD_DIR, "dash-view.tsx"), "utf-8");

  assert.ok(
    content.includes("`/chat?conversation=${row.draft.conversation_id}`"),
    "Buka obrolan must link to /chat?conversation=${row.draft.conversation_id} when conversation_id is present",
  );
  assert.ok(
    content.includes("`/chat?agent=${row.draft.agent_id}`"),
    "Buka obrolan must fall back to /chat?agent=${row.draft.agent_id}",
  );
  assert.ok(!content.includes('href="/chat"'), "Buka obrolan must not link to bare /chat");
});

test("routines-view links run.task_id to /tasks/<id> as Lihat tugas and preserves agent fallback", () => {
  const content = fs.readFileSync(path.join(DASHBOARD_DIR, "routines-view.tsx"), "utf-8");

  assert.ok(
    content.includes("href={`/tasks/${run.task_id}`}") && content.includes("Lihat tugas"),
    "RoutineRun with task_id must link to /tasks/${run.task_id} with label Lihat tugas",
  );
  assert.ok(
    content.includes("href={`/chat?agent=${agentId}`}") && content.includes("Buka obrolan"),
    "RoutineRun without task_id must preserve /chat?agent=${agentId} fallback",
  );
  assert.ok(
    !content.includes("there is no task\n                      screen"),
    "Outdated comment claiming there is no task screen must be removed",
  );
});
