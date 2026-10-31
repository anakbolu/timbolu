/**
 * How a routine reads on a phone: whether it is on, when it runs next, and how
 * its runs ended.
 *
 * The labels are the task tab's vocabulary (`src/lib/task-view.ts`): a run that
 * failed says "Gagal" in both places, and the colours are that module's palette
 * rather than a second one.
 */

import type { Routine, RoutineRun } from "@/lib/api";
import { TASK_TONE } from "@/lib/task-view";

/** The tone a routine paints with; the screens own the colour, not this module. */
export type RoutineTone = keyof typeof TASK_TONE;

export type RoutineStatus = { label: string; tone: RoutineTone };

/** Whether the schedule is live. A paused routine stays listed, with its history. */
export function routineStatus(routine: Pick<Routine, "enabled">): RoutineStatus {
  if (routine.enabled) return { label: "Aktif", tone: "ok" };
  return { label: "Dijeda", tone: "muted" };
}

/** One run's outcome, in the same words the task tab uses. */
export function runStatus(run: Pick<RoutineRun, "status">): RoutineStatus {
  switch (run.status) {
    case "succeeded":
      return { label: "Selesai", tone: "ok" };
    case "failed":
      return { label: "Gagal", tone: "bad" };
    // A skipped run found nothing to do and never called a model, so it is
    // neither a success nor a failure; painting it red would misread it.
    case "skipped":
      return { label: "Dilewati", tone: "muted" };
    case "running":
      return { label: "Berjalan", tone: "live" };
    default:
      return { label: run.status, tone: "muted" };
  }
}

/** The local calendar day of one instant, as a number, so two days can be compared. */
function localDayStart(at: Date): number {
  return new Date(at.getFullYear(), at.getMonth(), at.getDate()).getTime();
}

/**
 * When the routine runs next, in the phone's local time.
 *
 * `next_runs` is an instant, and the API's own `schedule` sentence is a wall-clock
 * time in the *workspace's* zone. Showing the sentence alone would tell a person
 * in Jakarta what a workspace in Makassar will do, without saying so; the day
 * name ("hari ini", "besok") plus the local time is the answer to "when should I
 * expect this". A trigger routine has no clock at all, and a paused one has no
 * next run — both are stated instead of left blank.
 */
export function nextRunText(
  routine: Pick<Routine, "enabled" | "scheduled" | "schedule" | "next_runs">,
  now = Date.now(),
): string {
  // Paused wins over the cadence's own wording: a paused trigger routine also
  // does not run, and saying "saat kejadiannya datang" beside a "Dijeda" badge
  // would promise it does.
  if (!routine.enabled) return "Dijeda — tidak akan berjalan";
  if (!routine.scheduled) return "Saat kejadiannya datang";

  const next = routine.next_runs[0];
  if (!next) return routine.schedule;
  const at = new Date(next);
  if (Number.isNaN(at.getTime())) return routine.schedule;

  // Rounded, not truncated: a day boundary that shifted by an hour (daylight
  // saving) is still one day, and the clock time below is what the person reads.
  const days = Math.round((localDayStart(at) - localDayStart(new Date(now))) / 86_400_000);
  const time = `${String(at.getHours()).padStart(2, "0")}.${String(at.getMinutes()).padStart(2, "0")}`;
  if (days === 0) return `Berikutnya hari ini ${time}`;
  if (days === 1) return `Berikutnya besok ${time}`;
  const day = at.toLocaleDateString("id-ID", { day: "numeric", month: "long" });
  return `Berikutnya ${day}, ${time}`;
}

/**
 * Why a skipped run did nothing, in a sentence a person can read.
 *
 * The server records the reason in `summary`; a run that carries none keeps the
 * generic explanation, because "skipped" without a reason is still not a failure.
 */
export function runDetail(run: Pick<RoutineRun, "status" | "summary">): string {
  if (run.summary !== "") return run.summary;
  if (run.status === "skipped") return "Tidak ada yang perlu dikerjakan; model tidak dipanggil.";
  if (run.status === "running") return "Masih berjalan.";
  return "";
}
