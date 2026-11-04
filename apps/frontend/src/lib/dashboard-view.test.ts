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

import type { Agent, Draft, Task } from "@/lib/api";
import {
  applySectionResult,
  buildPending,
  buildStats,
  buildWeek,
  canOpenWorkspaceStream,
  countPending,
  EMPTY_SECTION_ERRORS,
  expressionOf,
  STATE_EXPRESSION,
  streamRetryDelayMs,
  summarizeSectionErrors,
  taskState,
} from "./dashboard-view";

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

test("canOpenWorkspaceStream opens when ready, workspaceID, and accessToken are present despite section errors", () => {
  assert.equal(
    canOpenWorkspaceStream({
      ready: true,
      workspaceID: "ws-1",
      accessToken: "tok-1",
      error: "500 internal error",
      sectionErrors: { tasks: "timeout", drafts: "network error" },
    }),
    true,
  );

  assert.equal(
    canOpenWorkspaceStream({ ready: false, workspaceID: "ws-1", accessToken: "tok-1" }),
    false,
  );
  assert.equal(
    canOpenWorkspaceStream({ ready: true, workspaceID: null, accessToken: "tok-1" }),
    false,
  );
  assert.equal(
    canOpenWorkspaceStream({ ready: true, workspaceID: "", accessToken: "tok-1" }),
    false,
  );
  assert.equal(
    canOpenWorkspaceStream({ ready: true, workspaceID: "ws-1", accessToken: null }),
    false,
  );
  assert.equal(
    canOpenWorkspaceStream({ ready: true, workspaceID: "ws-1", accessToken: "" }),
    false,
  );
});

test("applySectionResult preserves prior data on failure and clears only that section on recovery", () => {
  const initialTasks = [task({ task_id: "t-old" })];
  const baseErrors = { ...EMPTY_SECTION_ERRORS, agents: "agents down" };

  const failed = applySectionResult("tasks", initialTasks, baseErrors, {
    ok: false,
    message: "tasks 503",
  });
  assert.equal(failed.data, initialTasks);
  assert.deepEqual(failed.errors, {
    ...EMPTY_SECTION_ERRORS,
    agents: "agents down",
    tasks: "tasks 503",
  });

  const nextTasks = [task({ task_id: "t-new" })];
  const recovered = applySectionResult("tasks", failed.data, failed.errors, {
    ok: true,
    data: nextTasks,
  });
  assert.equal(recovered.data, nextTasks);
  assert.deepEqual(recovered.errors, {
    ...EMPTY_SECTION_ERRORS,
    agents: "agents down",
    tasks: null,
  });
});

test("summarizeSectionErrors returns null when clean and follows fixed section order when multiple fail", () => {
  assert.equal(summarizeSectionErrors(EMPTY_SECTION_ERRORS), null);

  assert.equal(
    summarizeSectionErrors({
      drafts: "err-drafts",
      tasks: "err-tasks",
      agents: "err-agents",
      conversations: "err-conversations",
    }),
    "err-drafts",
  );
  assert.equal(
    summarizeSectionErrors({
      drafts: null,
      tasks: "err-tasks",
      agents: "err-agents",
      conversations: "err-conversations",
    }),
    "err-tasks",
  );
  assert.equal(
    summarizeSectionErrors({
      drafts: null,
      tasks: null,
      agents: "err-agents",
      conversations: "err-conversations",
    }),
    "err-agents",
  );
  assert.equal(
    summarizeSectionErrors({
      drafts: null,
      tasks: null,
      agents: null,
      conversations: "err-conversations",
    }),
    "err-conversations",
  );
});

test("streamRetryDelayMs backs off exponentially and caps at 30000ms", () => {
  assert.equal(streamRetryDelayMs(0), 1_000);
  assert.equal(streamRetryDelayMs(1), 2_000);
  assert.equal(streamRetryDelayMs(2), 4_000);
  assert.equal(streamRetryDelayMs(3), 8_000);
  assert.equal(streamRetryDelayMs(4), 16_000);
  assert.equal(streamRetryDelayMs(5), 30_000);
  assert.equal(streamRetryDelayMs(6), 30_000);
  assert.equal(streamRetryDelayMs(100), 30_000);
});

test("taskState reports a running task with stuck heartbeat as Terhenti rather than Berjalan", () => {
  const state = taskState({ status: "running", health: "stuck" });
  assert.equal(state.label, "Terhenti");
  assert.notEqual(state.label, "Berjalan");
  assert.equal(state.live, true);
});

test("buildPending preserves the full draft rupiah amount without scaling", () => {
  const agents: Agent[] = [
    {
      id: "agent-1",
      team_id: "team-1",
      team_name: "Operasional",
      team_kind: "default",
      name: "Oren",
      role: "Kasir",
      persona: "Teliti",
      tone: "Ramah",
      shape: "round",
      color: "#F97316",
      status: "active",
      display_status: "waiting",
      position: 0,
      tools: [],
      default_model: {},
      memory_extraction_enabled: true,
      created_at: new Date(2026, 9, 30, 8, 0, 0).toISOString(),
    },
  ];
  const drafts: Draft[] = [
    {
      draft_id: "draft-1",
      task_id: "task-1",
      agent_id: "agent-1",
      action_kind: "kirim_invoice",
      title: "Tagihan Oktober PT Maju",
      status: "pending",
      revision: 1,
      payload: { amount: 1_124_000 },
      created_at: new Date(2026, 9, 30, 11, 0, 0).toISOString(),
    },
  ];

  const rows = buildPending(drafts, agents);
  assert.equal(rows.length, 1);
  assert.match(rows[0].meta, /Rp 1\.124\.000\b/);
  assert.doesNotMatch(rows[0].meta, /Rp 1\.124(?!\.)/);
});
