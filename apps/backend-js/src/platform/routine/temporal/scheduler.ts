/**
 * Adapts the Temporal schedule API to the routine module's engine port.
 *
 * Ported from apps/backend/internal/platform/routine/temporal/scheduler.go.
 *
 * It lives in the platform layer rather than in the module because the module
 * must stay runnable without a Temporal server: the API answers its reads and
 * its history, and only the writes that need a schedule report that none can be
 * registered. The package knows the SDK; the module knows only the port.
 */

import {
  ScheduleOverlapPolicy,
  ScheduleNotFoundError,
  type CalendarSpec,
  type Range,
  type ScheduleOptions,
  type ScheduleSpec,
} from "@temporalio/client";
import type { ClientHandle } from "../../temporal/temporal.ts";

/**
 * The routine module's cadence and routine, as this adapter reads them.
 *
 * The Go file imports `modules/routine/domain`; the module tree is ported
 * separately, so the fields and the two methods the spec projection needs are
 * declared here. They are structural: a `routine/domain/routine.ts` value
 * satisfies them without an adapter.
 */
export interface Schedule {
  /** `daily`, `hourly_range`, `weekly`, or `trigger`. */
  kind: string;
  minute: number;
  hourStart: number;
  hourEnd: number;
  /** An ISO weekday (1 = Monday … 7 = Sunday), 0 when the kind does not use one. */
  dayOfWeek: number;
  /** Reports whether the cadence has times at all. */
  scheduled(): boolean;
  /** Renders the cadence in the user's words. */
  describe(): string;
}

export interface Routine {
  id: string;
  workspaceID: string;
  name: string;
  schedule(): Schedule;
}

/** `domain.ErrScheduleUnavailable`: no engine can be reached. */
export const ErrScheduleUnavailable = new Error("routine: the scheduler is not configured");

/** `domain.ErrInvalidInput`: the cadence cannot be expressed as a schedule. */
export const ErrInvalidInput = new Error("routine: invalid input");

/**
 * What one routine run is started with.
 *
 * Every field is an id, so a restart re-reads the current state instead of
 * replaying a snapshot. The keys are Go's field names because Go's default
 * converter JSON-encodes the struct without tags — a running deployment's
 * schedule history decodes with exactly these keys.
 */
interface RunInput {
  RoutineID: string;
  WorkspaceID: string;
}

/** Bounds one routine run. Longer than a task's own timeout: a run may wait on a task. */
const SCHEDULE_TIMEOUT_MS = 2 * 60 * 60 * 1000;

/**
 * What every schedule id this deployment creates begins with, so a schedule
 * created for a routine is recognisable in the Temporal UI.
 */
export const Prefix = "routine-";

/** The Sunday-first names Temporal's calendar spec uses. */
const WEEKDAYS = [
  "SUNDAY",
  "MONDAY",
  "TUESDAY",
  "WEDNESDAY",
  "THURSDAY",
  "FRIDAY",
  "SATURDAY",
] as const;

/** Registers the schedules a routine is fired by. */
export class Scheduler {
  private constructor(
    private readonly client: ClientHandle | null,
    private readonly queue: string,
    private readonly workflowName: string,
  ) {}

  /**
   * Builds the adapter. The queue is the agent worker's, which is where the
   * routine workflow is registered.
   */
  static create(client: ClientHandle | null, queue: string, workflowName: string): Scheduler {
    return new Scheduler(client, queue, workflowName);
  }

  /**
   * Creates or replaces one routine's schedule.
   *
   * It is create-or-update against the id derived from the routine, which is
   * what makes it idempotent: registering the same routine twice leaves exactly
   * one schedule. The id is derived rather than random so a routine whose
   * schedule id was lost (a restored database, say) is repaired by registering
   * it again instead of leaving an orphan behind.
   */
  async register(routine: Routine, location: string | null): Promise<string> {
    if (!this.client) {
      throw new Error(`${ErrScheduleUnavailable.message}: temporal is not configured`);
    }

    const spec = specFor(routine.schedule(), location);
    const scheduleID = ScheduleID(routine.id);
    const options: ScheduleOptions = {
      scheduleId: scheduleID,
      spec,
      action: {
        type: "startWorkflow",
        workflowId: scheduleID,
        workflowType: this.workflowName,
        args: [
          {
            RoutineID: routine.id,
            WorkspaceID: routine.workspaceID,
          } satisfies RunInput,
        ],
        taskQueue: this.queue,
        // One run at a time. A routine that is still working when its next slot
        // arrives is skipped rather than piled on.
        workflowExecutionTimeout: SCHEDULE_TIMEOUT_MS,
      },
      policies: {
        overlap: ScheduleOverlapPolicy.SKIP,
        // A short catch-up window, so a restarted server does not replay a day
        // of missed runs at once.
        catchupWindow: 60 * 60 * 1000,
        // A schedule that cannot fire must be visible rather than silently dead.
        pauseOnFailure: false,
      },
      state: { note: routine.name },
    };

    const handle = this.client.client().schedule.getHandle(scheduleID);
    try {
      await handle.update(() => ({
        action: options.action,
        spec: options.spec,
        policies: options.policies ?? {},
        state: { note: routine.name },
      }));
      return scheduleID;
    } catch {
      // The update is how an existing schedule is replaced. A schedule that is
      // not there yet is created below, exactly as the Go adapter does: it
      // attempts the update and treats any failure as "not registered".
    }

    try {
      await this.client.client().schedule.create(options);
    } catch (err) {
      throw new Error(`routine: register schedule: ${(err as Error).message}`);
    }

    return scheduleID;
  }

  /**
   * Pauses or resumes a schedule without removing it.
   *
   * A schedule that is not there is not an error on resume: the routine's row
   * is what the caller is changing, and the next registration recreates the
   * schedule.
   */
  async setPaused(scheduleID: string, paused: boolean, reason: string): Promise<void> {
    if (scheduleID === "") {
      // There is no schedule to pause. It is the same answer as one that was
      // already gone, and it is checked before the client so a routine that was
      // never registered can still be paused.
      return;
    }
    if (!this.client) {
      throw new Error(`${ErrScheduleUnavailable.message}: temporal is not configured`);
    }

    const handle = this.client.client().schedule.getHandle(scheduleID);
    if (paused) {
      try {
        await handle.pause(reason);
      } catch (err) {
        throw new Error(`routine: pause schedule: ${(err as Error).message}`);
      }
      return;
    }

    try {
      await handle.unpause(reason);
    } catch (err) {
      throw new Error(`routine: resume schedule: ${(err as Error).message}`);
    }
  }

  /** Deletes a schedule. One that is already gone is not an error. */
  async remove(scheduleID: string): Promise<void> {
    if (scheduleID === "") {
      // Nothing was ever registered, so there is nothing to delete.
      return;
    }
    if (!this.client) {
      throw new Error(`${ErrScheduleUnavailable.message}: temporal is not configured`);
    }

    try {
      await this.client.client().schedule.getHandle(scheduleID).delete();
    } catch (err) {
      // Deleting a schedule that does not exist is the caller's goal already
      // met. Reporting it would make a routine whose schedule was lost
      // undeletable.
      if (isNotFound(err)) return;
      throw new Error(`routine: delete schedule: ${(err as Error).message}`);
    }
  }

  /** Reports whether a schedule id is present. */
  async exists(scheduleID: string): Promise<boolean> {
    if (!this.client) {
      throw new Error(`${ErrScheduleUnavailable.message}: temporal is not configured`);
    }

    try {
      await this.client.client().schedule.getHandle(scheduleID).describe();
    } catch (err) {
      if (isNotFound(err)) return false;
      throw err;
    }
    return true;
  }

  /**
   * Reads the next time the engine will fire a schedule, or `null` when it has
   * no next time (the Go zero time).
   */
  async nextRun(scheduleID: string): Promise<Date | null> {
    if (!this.client) {
      throw new Error(`${ErrScheduleUnavailable.message}: temporal is not configured`);
    }

    const description = await this.client.client().schedule.getHandle(scheduleID).describe();
    return description.info.nextActionTimes[0] ?? null;
  }
}

/**
 * The deterministic id of one routine's schedule.
 *
 * Derived from the routine rather than random, so registering a routine whose
 * schedule id was lost repairs it instead of leaving an orphan schedule behind.
 */
export function ScheduleID(routineID: string): string {
  // Go's `uuid.String()` is the canonical lowercase form, so an id that
  // arrives uppercase still names the schedule the running deployment created.
  return Prefix + routineID.toLowerCase();
}

/**
 * Renders a cadence as the Temporal schedule spec.
 *
 * The translation lives here rather than in the module for one reason: the spec
 * type is the SDK's, and the module is not allowed to import it. The decisions
 * it encodes are the module's — `Schedule` already says which fields a kind uses
 * — so this function only projects them.
 */
export function specFor(schedule: Schedule, location: string | null): ScheduleSpec {
  if (!schedule.scheduled()) {
    throw new Error(`${ErrInvalidInput.message}: a trigger routine has no schedule`);
  }

  // The zone is the workspace's own. The engine fires "06:00 in Jakarta", so
  // changing the workspace's timezone moves every future run without touching
  // the routine.
  const spec: ScheduleSpec = { timezone: location ?? "UTC" };
  const minute: Range<number>[] = [{ start: schedule.minute, end: schedule.minute, step: 1 }];

  switch (schedule.kind) {
    case "daily":
      spec.calendars = [
        {
          minute,
          hour: [{ start: schedule.hourStart, end: schedule.hourStart, step: 1 }],
          comment: schedule.describe(),
        },
      ];
      return spec;

    case "hourly_range": {
      const hours: Range<number>[] = [];
      for (let hour = schedule.hourStart; hour <= schedule.hourEnd; hour++) {
        hours.push({ start: hour, end: hour, step: 1 });
      }
      spec.calendars = [{ minute, hour: hours, comment: schedule.describe() }];
      return spec;
    }

    case "weekly": {
      // Temporal numbers the week from Sunday; the domain numbers it from
      // Monday, matching the database and the form.
      const day = WEEKDAYS[temporalWeekday(schedule.dayOfWeek)];
      const calendar: CalendarSpec = {
        minute,
        hour: [{ start: schedule.hourStart, end: schedule.hourStart, step: 1 }],
        dayOfWeek: [day],
        comment: schedule.describe(),
      };
      spec.calendars = [calendar];
      return spec;
    }

    default:
      throw new Error(`${ErrInvalidInput.message}: unknown kind ${JSON.stringify(schedule.kind)}`);
  }
}

/**
 * Converts an ISO weekday (1 = Monday … 7 = Sunday) to the Sunday-first index
 * the Temporal calendar spec uses (0 = Sunday … 6 = Saturday).
 */
function temporalWeekday(iso: number): number {
  return iso === 7 ? 0 : iso;
}

/**
 * Reports whether a schedule is simply not there, which for describe and delete
 * is the caller's goal already met rather than a failure.
 */
function isNotFound(err: unknown): boolean {
  return err instanceof ScheduleNotFoundError;
}
