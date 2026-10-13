/**
 * The dashboard's derived view models.
 *
 * The design's mock data was invented in the browser; every value here is read
 * from the workspace instead, and a value the API does not have is absent rather
 * than filled in. What stays exactly as the design drew it is the shape of each
 * row — a label, a value, and the note under it — because that is the UI.
 */

import type { ActivityEvent, Agent, Draft, Task } from "@/lib/api";
import { ALL_BOTS, type BoluExpressionName, type CrewMember } from "@/lib/crew";

/**
 * TIMEZONES are the zones a workspace may be set to.
 *
 * Scheduled work is registered in the workspace's own zone, so this list is what
 * both the settings screen and a routine's form offer: one list, so the zone a
 * routine reads beside its times is one a person can actually choose.
 */
export const TIMEZONES = [
  { value: "Asia/Jakarta", label: "WIB (GMT+7)" },
  { value: "Asia/Makassar", label: "WITA (GMT+8)" },
  { value: "Asia/Jayapura", label: "WIT (GMT+9)" },
];

/** ACCENT is the primary blue: the "today" bar and the primary buttons. */
export const ACCENT = "#1F7FD6";

/**
 * The look each derived state is posed with.
 *
 * These are the generator's own expressions, so a Bolu reading as "waiting"
 * wears the same face the character sheet drew for it rather than a one-off.
 */
export const STATE_EXPRESSION: Record<string, BoluExpressionName> = {
  working: "senang",
  thinking: "mikir",
  waiting: "iseng",
  idle: "senang",
  resting: "capek",
};

/** The word a person reads for each derived Bolu state. */
export const STATE_LABEL: Record<string, string> = {
  working: "Bekerja",
  thinking: "Berpikir",
  waiting: "Menunggu kamu",
  idle: "Santai",
  resting: "Istirahat",
};

/** The sentence under a Bolu's name in the roster. */
export const STATE_LINE: Record<string, string> = {
  working: "Sedang bekerja",
  thinking: "Sedang berpikir",
  waiting: "Draf menunggu kamu",
  idle: "Lagi santai",
  resting: "Istirahat",
};

/** The dot beside a Bolu's face, per derived state. */
export const STATE_DOT: Record<string, string> = {
  working: "#1E7A43",
  thinking: "#2FA65A",
  waiting: "#E2602B",
  idle: "#4A86E8",
  resting: "#8B88A0",
};

/** Where each state sits in the office room. */
export const STATE_SPOT: Record<string, "desk" | "queue" | "sofa" | "bean"> = {
  working: "desk",
  thinking: "desk",
  waiting: "queue",
  idle: "sofa",
  resting: "bean",
};

/** A stat card: a label, the number, and what the number counts. */
export type StatItem = { label: string; value: string; note: string };

/** One bar of the week chart. */
export type WeekBar = {
  day: string;
  label: string;
  /** CSS height, computed the way the design computed it. */
  h: string;
  bg: string;
  weight: number;
};

/** One row of the live activity feed. */
export type FeedRow = {
  key: string;
  who: string;
  text: string;
  time: string;
  /** The Bolu that acted, or null when a person did. */
  agent: Agent | null;
};

/** One draft waiting for a person, as the dashboard lists it. */
export type PendingRow = {
  draft: Draft;
  agent: Agent | null;
  kind: string;
  title: string;
  meta: string;
};

/** One Bolu in the sidebar roster. */
export type RosterItem = {
  agent: Agent;
  line: string;
  dot: string;
  waiting: number;
};

/** The state a Bolu is drawn in, which is what the generator is posed with. */
export function stateOf(agent: Agent): string {
  return agent.display_status || agent.status || "idle";
}

/** expressionOf is the face a state is posed with. */
export function expressionOf(state: string): BoluExpressionName {
  return STATE_EXPRESSION[state] ?? "senang";
}

/**
 * crewFor is the seeded crew member an agent is drawn as.
 *
 * The generator needs a body and a colour, and the API stores the colour on the
 * agent while the body may be blank on an older row. Both the six seeded names
 * and any Bolu the workspace made resolve here: a seeded name takes its own
 * member, and anything else takes a member chosen from the name so the same Bolu
 * keeps the same body across reloads.
 */
export function crewFor(agent: Agent): CrewMember {
  const seeded = ALL_BOTS.find((member) => member.name === agent.name);
  if (seeded) return seeded;

  let hash = 0;
  for (let index = 0; index < agent.name.length; index++) {
    hash = (hash * 31 + agent.name.charCodeAt(index)) % 1000003;
  }
  return ALL_BOTS[hash % ALL_BOTS.length];
}

/** The shape a date is formatted in, kept in one place so every screen agrees. */
const DAYS = ["Minggu", "Senin", "Selasa", "Rabu", "Kamis", "Jumat", "Sabtu"];
const MONTHS = [
  "Januari",
  "Februari",
  "Maret",
  "April",
  "Mei",
  "Juni",
  "Juli",
  "Agustus",
  "September",
  "Oktober",
  "November",
  "Desember",
];
const SHORT_DAYS = ["Sen", "Sel", "Rab", "Kam", "Jum", "Sab", "Min"];

/** greeting is what the header says at this hour. */
export function greeting(now = new Date()): string {
  const hour = now.getHours();
  if (hour < 11) return "Selamat pagi";
  if (hour < 15) return "Selamat siang";
  if (hour < 19) return "Selamat sore";
  return "Selamat malam";
}

/** longDate is the design's "Rabu, 7 Oktober". */
export function longDate(now = new Date()): string {
  return `${DAYS[now.getDay()]}, ${now.getDate()} ${MONTHS[now.getMonth()]}`;
}

/**
 * relativeAge turns a timestamp into the phrase a person would say.
 *
 * It is deliberately coarse: the dashboard tells you whether something just
 * happened or has been waiting, and a second-by-second age is noise there.
 */
export function relativeAge(iso: string | undefined, now = new Date()): string {
  if (!iso) return "";
  const then = new Date(iso);
  if (Number.isNaN(then.getTime())) return "";
  const minutes = Math.floor((now.getTime() - then.getTime()) / 60000);
  if (minutes < 1) return "baru saja";
  if (minutes < 60) return `${minutes} menit lalu`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} jam lalu`;
  const days = Math.floor(hours / 24);
  if (days === 1) return "kemarin";
  if (days < 30) return `${days} hari lalu`;
  return `${Math.floor(days / 30)} bulan lalu`;
}

/** clockTime is the feed's own stamp: 24-hour, minutes only for today. */
export function clockTime(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "";
  const hours = String(date.getHours()).padStart(2, "0");
  const minutes = String(date.getMinutes()).padStart(2, "0");
  return `${hours}.${minutes}`;
}

/** isSameDay compares two instants by calendar day. */
function isSameDay(iso: string | undefined, day: Date): boolean {
  if (!iso) return false;
  const date = new Date(iso);
  return (
    date.getFullYear() === day.getFullYear() &&
    date.getMonth() === day.getMonth() &&
    date.getDate() === day.getDate()
  );
}

/** startOfWeek is Monday 00:00 of the week `now` falls in. */
export function startOfWeek(now: Date): Date {
  const start = new Date(now);
  // getDay() is Sunday-based; the design's week runs Sen–Min.
  const offset = (start.getDay() + 6) % 7;
  start.setDate(start.getDate() - offset);
  start.setHours(0, 0, 0, 0);
  return start;
}

/** finishedOn returns the tasks that finished on `day`. */
function finishedOn(tasks: Task[], day: Date): Task[] {
  return tasks.filter((task) => isSameDay(task.finished_at, day));
}

/**
 * buildStats counts what the workspace has actually done.
 *
 * The design's four cards are reproduced in shape, and filled with the counts the
 * API can answer: what is waiting, what finished today, what is running, and how
 * many rounds the Bolu have executed this week. The design's "time saved" and
 * "unpaid invoices" cards have no endpoint behind them, and a made-up number on a
 * dashboard is worse than a missing one.
 */
export function buildStats(input: {
  drafts: Draft[];
  tasks: Task[];
  agents: Agent[];
  now?: Date;
}): StatItem[] {
  const now = input.now ?? new Date();
  const pending = input.drafts.filter((draft) => draft.status === "pending");
  const oldest = pending.reduce<string | undefined>((oldestIso, draft) => {
    if (!oldestIso) return draft.created_at;
    return new Date(draft.created_at) < new Date(oldestIso) ? draft.created_at : oldestIso;
  }, undefined);

  const doneToday = input.tasks.filter(
    (task) => isSameDay(task.finished_at, now) && task.status === "done",
  );
  const running = input.tasks.filter(
    (task) => task.health === "running" || task.health === "waiting",
  );
  const stepsThisWeek = input.tasks
    .filter((task) => new Date(task.created_at) >= startOfWeek(now))
    .reduce((total, task) => total + task.step_count, 0);
  const workers = new Set(doneToday.map((task) => task.agent_id));

  return [
    {
      label: "Menunggu persetujuanmu",
      value: String(pending.length),
      note: oldest ? `paling lama ${relativeAge(oldest, now)}` : "tidak ada yang menunggu",
    },
    {
      label: "Tugas selesai hari ini",
      value: String(doneToday.length),
      note:
        workers.size === 0
          ? `dari ${input.agents.length} anggota tim`
          : `oleh ${workers.size} anggota tim`,
    },
    {
      label: "Sedang dikerjakan",
      value: String(running.length),
      note: running.length === 0 ? "semua Bolu menganggur" : "tersambung ke kantor",
    },
    {
      label: "Langkah dijalankan minggu ini",
      value: String(stepsThisWeek),
      note: "tanpa kamu sentuh",
    },
  ];
}

/**
 * buildWeek counts finished tasks per weekday of the current week.
 *
 * A day that has not happened yet is drawn as the design drew it: a flat stub, so
 * the week reads as a week rather than as a chart that stops.
 */
export function buildWeek(tasks: Task[], now = new Date()): WeekBar[] {
  const start = startOfWeek(now);
  const days = SHORT_DAYS.map((label, index) => {
    const day = new Date(start);
    day.setDate(start.getDate() + index);
    return { label, day, count: finishedOn(tasks, day).length };
  });

  const peak = Math.max(...days.map((day) => day.count), 1);
  const todayIndex = (now.getDay() + 6) % 7;

  return days.map((day, index) => {
    const isToday = index === todayIndex;
    const isFuture = day.day > now && !isToday;
    return {
      day: day.label,
      label: day.count === 0 ? "" : String(day.count),
      h: isFuture ? "6px" : `${Math.round(Math.min(1, day.count / peak) * 140)}px`,
      bg: isFuture ? "#E3E5EF" : isToday ? ACCENT : "#1E1B2E",
      weight: isToday ? 700 : 500,
    };
  });
}

/** One sentence per event type, so the feed reads as work rather than as ids. */
const EVENT_TEXT: Record<string, (payload: Record<string, unknown>) => string> = {
  "task.started": () => "mulai mengerjakan tugas baru",
  "task.finished": (payload) =>
    typeof payload.title === "string" && payload.title
      ? `selesai: ${payload.title}`
      : "menyelesaikan tugas",
  "task.waiting_approval": () => "menyiapkan draf dan menunggu kamu",
  "task.running": () => "masih mengerjakan tugasnya",
  "draft.created": (payload) =>
    typeof payload.title === "string" && payload.title
      ? `menyiapkan draf: ${payload.title}`
      : "menyiapkan draf baru",
  "draft.decided": () => "drafnya sudah kamu putuskan",
  "message.new": () => "mengirim pesan baru",
  "router.decision": () => "dipilih untuk menjawab grup",
  "routine.run": () => "menjalankan rutinitas",
};

/**
 * buildFeed turns the activity stream into the dashboard's feed rows.
 *
 * The stream is the same one the chat and the office read, so the feed cannot
 * disagree with them about what happened. An event that names no Bolu — a person
 * deciding a draft — is drawn as the person.
 */
export function buildFeed(events: ActivityEvent[], agents: Agent[], limit = 7): FeedRow[] {
  return events
    .map<FeedRow | null>((event) => {
      const agent = agents.find((item) => item.id === event.agent_id) ?? null;
      const name = agent?.name ?? "Kamu";
      const describe = EVENT_TEXT[event.type];
      if (!describe) return null;
      const text = describe(event.payload ?? {});
      if (!text) return null;
      return {
        key: String(event.id),
        who: name,
        text,
        time: clockTime(event.created_at),
        agent,
      };
    })
    .filter((row): row is FeedRow => row !== null)
    .slice(0, limit);
}

/**
 * buildPending lists the drafts a person has to answer.
 *
 * The meta line is the design's "Rp 1.124.000 · jatuh tempo 21 Okt" reduced to
 * what a draft actually carries: its kind, its value when the payload has one,
 * and how long it has waited.
 */
export function buildPending(drafts: Draft[], agents: Agent[]): PendingRow[] {
  return drafts
    .filter((draft) => draft.status === "pending")
    .map((draft) => {
      const agent = agents.find((item) => item.id === draft.agent_id);
      const amount = draft.payload?.amount;
      const parts = [draft.action_kind.replace(/_/g, " ")];
      if (typeof amount === "number") {
        parts.push(`Rp ${amount.toLocaleString("id-ID")}`);
      }
      parts.push(`menunggu ${relativeAge(draft.created_at)}`);
      return {
        draft,
        agent: agent ?? null,
        kind: draft.action_kind.replace(/_/g, " "),
        title: draft.title,
        meta: parts.join(" · "),
      };
    });
}

/** buildRoster turns the registry into the sidebar's Bolu rows. */
export function buildRoster(agents: Agent[], drafts: Draft[]): RosterItem[] {
  return agents.map((agent) => {
    const waiting = drafts.filter(
      (draft) => draft.agent_id === agent.id && draft.status === "pending",
    ).length;
    const state = agent.display_status || agent.status || "idle";
    const line =
      waiting > 0
        ? waiting === 1
          ? "Draf menunggu kamu"
          : `${waiting} draf menunggu kamu`
        : STATE_LINE[state] ?? "Lagi santai";
    return {
      agent,
      line,
      dot: STATE_DOT[state] ?? STATE_DOT.idle,
      waiting,
    };
  });
}

/** countPending is what the sidebar badge and the nav badge show. */
export function countPending(drafts: Draft[]): number {
  return drafts.filter((draft) => draft.status === "pending").length;
}
