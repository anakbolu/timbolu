/**
 * How a task reads on a phone: its state, its rounds, and its moments.
 *
 * The labels are the web's (`apps/frontend/src/lib/dashboard-view.ts`
 * `taskState`/`taskStepLabel`), so a task is one state in both clients. They are
 * copied rather than imported because the dashboard module lives outside this
 * app's root, the same reason `src/lib/bolu.ts` copies its palette.
 */

import type { Task } from "@/lib/api";

/** The tone a state paints with. The screen owns the colour, not this module. */
export type TaskState = {
  label: string;
  tone: "ok" | "warn" | "bad" | "muted" | "live";
  /** True while the task still occupies its Bolu: queued, running, or parked. */
  live: boolean;
};

/**
 * The state of one task.
 *
 * `health` is read alongside `status` and neither alone is enough: the status is
 * what the runtime stored, while health is derived from the heartbeat — so a
 * task whose worker died still stores `running` and must read as stuck, not as
 * work in progress.
 */
export function taskState(task: Pick<Task, "status" | "health">): TaskState {
  if (task.status === "succeeded") return { label: "Selesai", tone: "ok", live: false };
  if (task.status === "failed") return { label: "Gagal", tone: "bad", live: false };
  if (task.status === "canceled") return { label: "Dibatalkan", tone: "muted", live: false };
  if (task.status === "waiting_approval" || task.health === "waiting") {
    return { label: "Menunggu keputusanmu", tone: "warn", live: true };
  }
  if (task.health === "stuck") return { label: "Terhenti", tone: "bad", live: true };
  if (task.status === "queued") return { label: "Antre", tone: "muted", live: true };
  return { label: "Berjalan", tone: "live", live: true };
}

/** The colour a state paints with. */
export const TASK_TONE = {
  ok: { color: "#2E7D4F" },
  warn: { color: "#B4531E" },
  bad: { color: "#B3261E" },
  muted: { color: "#5A5670" },
  live: { color: "#B4531E" },
} as const;

/** Which round a step is, in the app's language. An unknown kind keeps its id. */
export function stepLabel(kind: string): string {
  switch (kind) {
    case "think":
      return "Berpikir";
    case "tool_call":
      return "Memanggil alat";
    case "tool_result":
      return "Hasil alat";
    case "draft":
      return "Draf";
    case "final":
      return "Jawaban";
    default:
      return kind;
  }
}

/** One moment, in the app's language: a date and a time, not an ISO string. */
export function momentText(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return iso;
  const day = date.toLocaleDateString("id-ID", { day: "numeric", month: "long", year: "numeric" });
  const time = `${String(date.getHours()).padStart(2, "0")}.${String(date.getMinutes()).padStart(2, "0")}`;
  return `${day}, ${time}`;
}

/** A moment as an age, coarse on purpose: the list answers "how long ago". */
export function relativeAge(iso: string, now = Date.now()): string {
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "";
  const minutes = Math.floor((now - then) / 60_000);
  if (minutes < 1) return "baru saja";
  if (minutes < 60) return `${minutes} menit lalu`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} jam lalu`;
  const days = Math.floor(hours / 24);
  if (days === 1) return "kemarin";
  if (days < 30) return `${days} hari lalu`;
  return `${Math.floor(days / 30)} bulan lalu`;
}

/**
 * The readable text of one round's payload.
 *
 * The web dashboard prints the payload as JSON, and so does this — with two
 * concessions to a phone: a model's answer is stored as `{text}` and is read as
 * its text rather than as an object, and the output is cut at `limit` characters
 * so one 100 KB tool result cannot make the screen unreadable.
 */
export function stepPayloadText(value: unknown, limit = 600): string {
  // `JSON.stringify` returns `undefined` for one, and a step can carry `null`
  // for the half it did not record; neither is a payload worth printing.
  if (value === undefined || value === null) return "";
  let text: string;
  if (typeof value === "string") {
    text = value;
  } else if (value !== null && typeof value === "object" && "text" in value) {
    // `in` narrows the property to `unknown`; the `typeof` is the check.
    text = typeof value.text === "string" ? value.text : JSON.stringify(value, null, 2);
  } else {
    text = JSON.stringify(value, null, 2);
  }
  return text.length > limit ? `${text.slice(0, limit)}…` : text;
}

/**
 * The artifact references a task's rounds carry.
 *
 * A task's own payload has no artifact field — `/tasks` and `/steps` publish the
 * task's counters and its raw rounds, and only a message's `html` block carries a
 * reference (`docs/api.md` § The block contract). A round's payload is whatever a
 * tool answered, so this walks it for `content_ref` instead of assuming one
 * wrapper: a block stored at the top of a result and one nested under `blocks`
 * are the same fact.
 *
 * The walk is depth-bounded and cycle-safe by the depth alone: payloads come from
 * `JSON.stringify` on the server, so they are trees.
 */
export function contentRefs(...payloads: unknown[]): string[] {
  const found = new Set<string>();

  const walk = (value: unknown, depth: number): void => {
    if (depth > 6 || value === null || typeof value !== "object") return;
    if (Array.isArray(value)) {
      for (const item of value) walk(item, depth + 1);
      return;
    }
    // A JSON object has string keys, and its values stay `unknown`; each read
    // below re-checks its type, so this asserts the key type and nothing about
    // the payload's shape.
    const record = value as Record<string, unknown>;
    for (const [key, child] of Object.entries(record)) {
      // `content_ref` is the field name the API's html block uses; a task's
      // rounds are tool JSON, so the key is matched, not the block's shape.
      if (key === "content_ref" && typeof child === "string" && child !== "") {
        found.add(child);
        continue;
      }
      walk(child, depth + 1);
    }
  };

  for (const payload of payloads) walk(payload, 0);
  return [...found];
}
