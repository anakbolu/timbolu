/**
 * The routine contract: the cadence a routine runs on, the run it produces, and
 * the value that translates a cadence into the next occurrences.
 *
 * Two things are deliberately pure here and both are load-bearing:
 *
 *   - `Schedule` is the cadence, translated from a routine's columns. The
 *     preview a form shows and the Temporal Schedule the engine fires come from
 *     the *same* value, so "the next run is 06:00" and "the engine will fire at
 *     06:00" cannot disagree.
 *   - `Schedule.nextRuns` computes the next occurrences without a Temporal
 *     server or a database. It is what the API answers a preview with.
 *
 * Ported 1:1 from internal/modules/routine/domain/routine.go.
 */

/** Runs once a day at a fixed time, e.g. "06:00 cek dokumen baru". */
export const KindDaily = "daily";
/** Runs every hour inside a window, e.g. "08:00–21:00". */
export const KindHourlyRange = "hourly_range";
/** Runs once a week on one weekday at a fixed time. */
export const KindWeekly = "weekly";
/** Runs when an event arrives rather than at a time. It gets no schedule. */
export const KindTrigger = "trigger";

/** The kinds a write may use, in the order a form offers them. */
export const Kinds: string[] = [KindDaily, KindHourlyRange, KindWeekly, KindTrigger];

/** A run whose task is still in flight. */
export const RunRunning = "running";
/** A run whose work finished. */
export const RunSucceeded = "succeeded";
/** A run whose task failed. Recorded, and the schedule keeps firing. */
export const RunFailed = "failed";
/** A run that found nothing to do and never called a model. */
export const RunSkipped = "skipped";

/** The zero uuid, as Go's `uuid.Nil`. */
export const NilUUID = "00000000-0000-0000-0000-000000000000";

/** `routine: not found`. */
export const ErrNotFound = new Error("routine: not found");
/** `routine: invalid input`. */
export const ErrInvalidInput = new Error("routine: invalid input");
/** `routine: the scheduler is not configured`. */
export const ErrScheduleUnavailable = new Error("routine: the scheduler is not configured");

/** An error carrying the sentinel it was wrapped from, the way Go's `%w` does. */
export class WrappedError extends Error {
  readonly sentinel: Error;

  constructor(sentinel: Error, message: string) {
    super(message);
    this.name = "WrappedError";
    this.sentinel = sentinel;
  }
}

/** Wraps `sentinel` with a detail, matching `fmt.Errorf("%w: %s", ...)`. */
export function wrap(sentinel: Error, detail: string): WrappedError {
  return new WrappedError(sentinel, `${sentinel.message}: ${detail}`);
}

/** Reports whether `err` is `sentinel` or wraps it, matching `errors.Is`. */
export function is(err: unknown, sentinel: Error): boolean {
  let current: unknown = err;
  for (let guard = 0; guard < 32; guard++) {
    if (current === sentinel) return true;
    if (!(current instanceof Error)) return false;
    if (current instanceof WrappedError) {
      current = current.sentinel;
      continue;
    }
    current = "cause" in current ? current.cause : undefined;
  }
  return false;
}

/** Renders an error for a person: the detail after the sentinel's own text. */
export function userMessage(err: unknown, sentinel: Error): string {
  const raw = err instanceof Error ? err.message : String(err);
  const detail = raw.startsWith(sentinel.message) ? raw.slice(sentinel.message.length) : raw;
  return detail.trim().replace(/^:/, "").trim();
}

/** The tenant identity a query runs as. */
export interface Scope {
  userId?: string | null;
  workspaceId: string;
}

/** Reports whether the scope carries no workspace at all. */
export function isZeroScope(scope: Scope): boolean {
  return !scope.workspaceId || scope.workspaceId === NilUUID;
}

/** One thing a Bolu does on its own. */
export interface RoutineFields {
  id: string;
  workspaceID: string;
  agentID: string;
  name: string;
  kind: string;
  /**
   * Minute, hourStart, and hourEnd are wall-clock times in the workspace's own
   * zone: a daily routine uses hourStart, an hourly range uses both ends
   * inclusive, and a weekly one adds dayOfWeek.
   */
  minute: number;
  hourStart: number;
  hourEnd: number;
  /** An ISO weekday (1 = Monday … 7 = Sunday), 0 when the kind does not use one. */
  dayOfWeek: number;
  /** The event this routine watches, as `app.thing`; empty for a clock cadence. */
  triggerSource: string;
  instruction: string;
  enabled: boolean;
  /** Explains a routine the engine stopped, in the user's words. */
  pausedReason: string;
  /** The engine's id for this routine's schedule. */
  scheduleID: string;
  createdAt: Date;
  updatedAt: Date;
}

/** A routine with the cadence translation the engine and the preview share. */
export interface Routine extends RoutineFields {
  schedule(): Schedule;
}

/** Renders the cadence a routine's columns describe. */
export function scheduleOf(routine: RoutineFields): Schedule {
  return new Schedule(routine.kind, routine.minute, routine.hourStart, routine.hourEnd, routine.dayOfWeek);
}

/** Builds a routine value. */
export function makeRoutine(fields: RoutineFields): Routine {
  return { ...fields, schedule: () => scheduleOf(fields) };
}

/** One execution of a routine. */
export interface Run {
  id: string;
  workspaceID: string;
  /** Zero once the routine that produced this run has been deleted. */
  routineID: string;
  /** The routine's name at the time it ran, so a deleted routine's history reads. */
  routineName: string;
  taskID: string;
  status: string;
  summary: string;
  startedAt: Date;
  finishedAt: Date | null;
}

/**
 * A routine's cadence, on its own.
 *
 * The same value serves the preview the API answers, the Temporal spec the
 * engine fires, and the tests between them.
 */
export class Schedule {
  constructor(
    readonly kind: string,
    readonly minute: number,
    readonly hourStart: number,
    readonly hourEnd: number,
    readonly dayOfWeek: number,
  ) {}

  /**
   * Reports whether the cadence has times at all. A trigger routine does not:
   * it is started by an event, so the engine is given no schedule for it.
   */
  scheduled(): boolean {
    return this.kind !== KindTrigger;
  }

  /** Renders the cadence in the user's words. */
  describe(): string {
    switch (this.kind) {
      case KindDaily:
        return `setiap hari ${String(this.hourStart).padStart(2, "0")}:${String(this.minute).padStart(2, "0")}`;
      case KindHourlyRange:
        return `tiap jam ${String(this.hourStart).padStart(2, "0")}:${String(this.minute).padStart(2, "0")}–${String(this.hourEnd).padStart(2, "0")}:${String(this.minute).padStart(2, "0")}`;
      case KindWeekly:
        return `setiap ${weekdayName(this.dayOfWeek)} ${String(this.hourStart).padStart(2, "0")}:${String(this.minute).padStart(2, "0")}`;
      case KindTrigger:
        return "saat ada kejadian";
      default:
        return this.kind;
    }
  }

  /**
   * Returns the next occurrences after `from`, in order.
   *
   * The computation is in the workspace's own location: "06:00" means six in
   * the morning where the business is, not in UTC. Calendar arithmetic — not
   * adding 24 hours — keeps the wall-clock time across a daylight-saving change.
   *
   * A trigger routine has no times, so the answer is empty rather than an error.
   */
  nextRuns(location: string | null, from: Date, count: number): Date[] {
    if (count <= 0 || !this.scheduled()) {
      return [];
    }

    const zone = location || "UTC";
    const local = localParts(from, zone);
    const runs: Date[] = [];

    switch (this.kind) {
      case KindDaily: {
        let day = { year: local.year, month: local.month, day: local.day };
        while (runs.length < count) {
          const candidate = zonedTimeToUtc(day.year, day.month, day.day, this.hourStart, this.minute, zone);
          if (candidate.getTime() > from.getTime()) {
            runs.push(candidate);
          }
          day = addLocalDays(day.year, day.month, day.day, 1);
        }
        break;
      }

      case KindHourlyRange: {
        // Two days are enough for any window: the last hour of one day and the
        // first of the next are both reachable within it.
        for (let offset = 0; offset < 2; offset++) {
          const day = addLocalDays(local.year, local.month, local.day, offset);
          for (let hour = this.hourStart; hour <= this.hourEnd && runs.length < count; hour++) {
            const candidate = zonedTimeToUtc(day.year, day.month, day.day, hour, this.minute, zone);
            if (candidate.getTime() > from.getTime()) {
              runs.push(candidate);
            }
          }
          if (runs.length >= count) break;
        }
        break;
      }

      case KindWeekly: {
        // The wanted weekday may be today, and today may already be past its
        // hour, so the search walks forward to the first matching day and then
        // steps a week at a time.
        let day = { year: local.year, month: local.month, day: local.day };
        for (let missed = 0; missed < 7; missed++) {
          if (isoWeekday(day.year, day.month, day.day) === this.dayOfWeek) {
            const candidate = zonedTimeToUtc(day.year, day.month, day.day, this.hourStart, this.minute, zone);
            if (candidate.getTime() > from.getTime()) {
              break;
            }
          }
          day = addLocalDays(day.year, day.month, day.day, 1);
        }

        while (runs.length < count) {
          runs.push(zonedTimeToUtc(day.year, day.month, day.day, this.hourStart, this.minute, zone));
          day = addLocalDays(day.year, day.month, day.day, 7);
        }
        break;
      }
    }

    return runs;
  }

  /** Returns the first occurrence, or null (the Go zero time). */
  nextRun(location: string | null, from: Date): Date | null {
    return this.nextRuns(location, from, 1)[0] ?? null;
  }
}

// --------------------------------------------------------------- time helpers

interface LocalParts {
  year: number;
  month: number;
  day: number;
  hour: number;
  minute: number;
  second: number;
}

const formatters = new Map<string, Intl.DateTimeFormat>();

function formatterFor(zone: string): Intl.DateTimeFormat {
  let formatter = formatters.get(zone);
  if (!formatter) {
    formatter = new Intl.DateTimeFormat("en-US", {
      timeZone: zone,
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
      hourCycle: "h23",
    });
    formatters.set(zone, formatter);
  }
  return formatter;
}

/** The wall-clock parts of an instant in `zone`. */
function localParts(at: Date, zone: string): LocalParts {
  const parts = formatterFor(zone).formatToParts(at);
  const read = (type: Intl.DateTimeFormatPartTypes): number => {
    const found = parts.find((part) => part.type === type);
    return found ? Number(found.value) : 0;
  };
  return {
    year: read("year"),
    month: read("month"),
    day: read("day"),
    hour: read("hour"),
    minute: read("minute"),
    second: read("second"),
  };
}

/** The zone's offset from UTC at one instant, in milliseconds. */
function zoneOffsetMs(at: number, zone: string): number {
  const local = localParts(new Date(at), zone);
  const asUtc = Date.UTC(local.year, local.month - 1, local.day, local.hour, local.minute, local.second, 0);
  return asUtc - at;
}

/** Builds the instant whose wall-clock time in `zone` is the given one. */
function zonedTimeToUtc(year: number, month: number, day: number, hour: number, minute: number, zone: string): Date {
  const guess = Date.UTC(year, month - 1, day, hour, minute, 0, 0);
  let at = guess - zoneOffsetMs(guess, zone);
  // The offset at the guessed instant may differ across a DST boundary; one
  // correction pass settles it, the same way Go's time.Date resolves the zone.
  const offset = zoneOffsetMs(at, zone);
  at = guess - offset;
  return new Date(at);
}

/** Adds calendar days to a local date, without touching the wall-clock time. */
function addLocalDays(year: number, month: number, day: number, days: number): { year: number; month: number; day: number } {
  const shifted = new Date(Date.UTC(year, month - 1, day + days));
  return {
    year: shifted.getUTCFullYear(),
    month: shifted.getUTCMonth() + 1,
    day: shifted.getUTCDate(),
  };
}

/** The ISO weekday (1 = Monday … 7 = Sunday) of a local date. */
function isoWeekday(year: number, month: number, day: number): number {
  const weekday = new Date(Date.UTC(year, month - 1, day)).getUTCDay();
  return weekday === 0 ? 7 : weekday;
}

function weekdayName(iso: number): string {
  switch (iso) {
    case 1:
      return "Senin";
    case 2:
      return "Selasa";
    case 3:
      return "Rabu";
    case 4:
      return "Kamis";
    case 5:
      return "Jumat";
    case 6:
      return "Sabtu";
    case 7:
      return "Minggu";
    default:
      return "hari yang tidak dikenal";
  }
}
