/**
 * The routine use cases: what a routine is, when it runs, what one run did, and
 * how the engine is kept in step with the rows.
 *
 * Three rules run through the package:
 *
 *   - a routine that exists always has a schedule. Creating one registers the
 *     schedule before it is reported as created, so a routine that could not be
 *     scheduled is a failed write rather than a silent one that never runs.
 *   - the times a screen shows and the times the engine fires come from one
 *     translation (`Schedule`). A second calculation anywhere is how a form
 *     comes to promise a time the engine never fires.
 *   - a run that finds nothing to do is `skipped` and costs no tokens. That is
 *     the product's largest saving, so it is a first-class outcome.
 *
 * Ported 1:1 from internal/modules/routine/service/service.go.
 */

import { nopLogger, type Logger } from "../../../platform/logger.ts";
import {
  ErrInvalidInput,
  ErrScheduleUnavailable,
  KindDaily,
  KindHourlyRange,
  KindTrigger,
  KindWeekly,
  Kinds,
  NilUUID,
  RunFailed,
  RunRunning,
  RunSkipped,
  RunSucceeded,
  Schedule,
  isZeroScope,
  makeRoutine,
  wrap,
  type Routine,
  type Run,
  type Scope,
} from "../domain/routine.ts";
import { Count } from "../domain/service.ts";
import type {
  AgentInfo,
  AuditEntry,
  Auditor,
  Engine,
  Event,
  Filter,
  Outcome,
  PreviewRequest,
  Repository,
  RunFilter,
  Service,
  TaskStarter,
  View,
  WorkQueue,
  WorkspaceClock,
  WriteRequest,
} from "../domain/service.ts";

/** Bounds a routine's name, which is shown in a list and in a history line. */
const nameMaxRunes = 120;

/** Bounds what a Bolu is told to do. */
const instructionMaxRunes = 2000;

/** Bounds how many events one run acts on. */
const pendingLimit = 20;

/** The service dependencies. */
export interface Deps {
  repository: Repository;
  engine?: Engine | null;
  tasks?: TaskStarter | null;
  queue?: WorkQueue | null;
  auditor?: Auditor | null;
  clock?: WorkspaceClock | null;
  logger?: Logger | null;
  /** The time source, so a preview is testable without waiting. */
  now?: () => Date;
}

/** What the cheap check consumed. */
interface Claimed {
  events: Event[];
  claimed: number;
}

/** The routine implementation. */
export class RoutineService implements Service {
  private readonly deps: Deps;
  private readonly now: () => Date;

  constructor(deps: Deps) {
    this.deps = deps;
    this.now = deps.now ?? (() => new Date());
  }

  private get log(): Logger {
    return this.deps.logger ?? nopLogger;
  }

  /**
   * The workspace's routines, each with its next run times.
   *
   * The workspace's zone is read once for the whole list rather than per row.
   */
  async routines(scope: Scope, filter: Filter): Promise<View[]> {
    this.requireScope(scope);

    const routines = await this.deps.repository.list(scope, filter);
    const location = await this.location(scope);

    return routines.map((routine) => this.view(routine, location));
  }

  /** One routine with its next run times. */
  async routine(scope: Scope, id: string): Promise<View> {
    this.requireScope(scope);

    const routine = await this.deps.repository.get(scope, id);
    const location = await this.location(scope);

    return this.view(routine, location);
  }

  /** The enabled routines watching one event. */
  async routinesFor(scope: Scope, triggerSource: string): Promise<Routine[]> {
    this.requireScope(scope);
    const trimmed = triggerSource.trim();
    if (trimmed === "") {
      throw wrap(ErrInvalidInput, "trigger source is required");
    }

    return this.deps.repository.listByTrigger(scope, trimmed);
  }

  /** The run history. */
  async runs(scope: Scope, filter: RunFilter): Promise<Run[]> {
    this.requireScope(scope);
    return this.deps.repository.listRuns(scope, filter);
  }

  /**
   * The next run times for a cadence that has not been saved yet.
   *
   * It reads the workspace's own timezone, because that is what the times mean.
   */
  async preview(scope: Scope, request: PreviewRequest): Promise<Date[]> {
    this.requireScope(scope);

    const location = await this.location(scope);
    const schedule = this.validate({
      agentID: NilUUID,
      name: "",
      kind: request.kind,
      minute: request.minute,
      hourStart: request.hourStart,
      hourEnd: request.hourEnd,
      dayOfWeek: request.dayOfWeek,
      triggerSource: "",
      instruction: "",
      enabled: true,
    });

    return schedule.nextRuns(location, this.now(), Count);
  }

  // ---------------------------------------------------------------- writes

  /** Stores a routine and registers its schedule. */
  async create(scope: Scope, request: WriteRequest): Promise<View> {
    this.requireScope(scope);
    if (!request.agentID || request.agentID === NilUUID) {
      throw wrap(ErrInvalidInput, "a routine belongs to a bolu");
    }

    const schedule = this.validate(request);
    const name = this.acceptName(request.name);
    const instruction = this.acceptInstruction(request.instruction);
    const location = await this.location(scope);

    const stored = await this.deps.repository.create(
      scope,
      makeRoutine({
        id: NilUUID,
        workspaceID: scope.workspaceId,
        agentID: request.agentID,
        name,
        kind: schedule.kind,
        minute: schedule.minute,
        hourStart: schedule.hourStart,
        hourEnd: schedule.hourEnd,
        dayOfWeek: schedule.dayOfWeek,
        triggerSource: request.triggerSource.trim(),
        instruction,
        enabled: request.enabled,
        pausedReason: "",
        scheduleID: "",
        createdAt: new Date(0),
        updatedAt: new Date(0),
      }),
    );

    const registered = await this.register(scope, stored, location);

    await this.audit(scope, {
      action: "routine.created",
      objectType: "routine",
      objectID: registered.id,
      actorUserID: scope.userId ?? NilUUID,
      metadata: {
        kind: registered.kind,
        schedule: registered.schedule().describe(),
        scheduled: registered.schedule().scheduled(),
      },
    });

    return this.view(registered, location);
  }

  /** Stores a change and re-registers the schedule. */
  async update(scope: Scope, id: string, request: WriteRequest): Promise<View> {
    this.requireScope(scope);

    const stored = await this.deps.repository.get(scope, id);
    const schedule = this.validate(request);
    const name = this.acceptName(request.name);
    const instruction = this.acceptInstruction(request.instruction);
    const agentID = !request.agentID || request.agentID === NilUUID ? stored.agentID : request.agentID;
    const location = await this.location(scope);

    const updated = await this.deps.repository.update(
      scope,
      makeRoutine({
        id: stored.id,
        workspaceID: stored.workspaceID,
        agentID,
        name,
        kind: schedule.kind,
        minute: schedule.minute,
        hourStart: schedule.hourStart,
        hourEnd: schedule.hourEnd,
        dayOfWeek: schedule.dayOfWeek,
        // Paused reason is cleared by an edit: the change is the answer to it.
        pausedReason: "",
        triggerSource: request.triggerSource.trim(),
        instruction,
        enabled: request.enabled,
        scheduleID: stored.scheduleID,
        createdAt: stored.createdAt,
        updatedAt: stored.updatedAt,
      }),
    );

    const registered = await this.register(scope, updated, location);

    await this.audit(scope, {
      action: "routine.updated",
      objectType: "routine",
      objectID: registered.id,
      actorUserID: scope.userId ?? NilUUID,
      metadata: {
        kind: registered.kind,
        schedule: registered.schedule().describe(),
      },
    });

    return this.view(registered, location);
  }

  /**
   * Pauses or resumes a routine's schedule.
   *
   * Pausing keeps the schedule and the history: the routine is still in the
   * list, with its runs, and resuming puts it back without a new one.
   */
  async setEnabled(scope: Scope, id: string, enabled: boolean): Promise<View> {
    this.requireScope(scope);

    const stored = await this.deps.repository.get(scope, id);
    const location = await this.location(scope);

    const updated = await this.deps.repository.update(
      scope,
      makeRoutine({
        id: stored.id,
        workspaceID: stored.workspaceID,
        agentID: stored.agentID,
        name: stored.name,
        kind: stored.kind,
        minute: stored.minute,
        hourStart: stored.hourStart,
        hourEnd: stored.hourEnd,
        dayOfWeek: stored.dayOfWeek,
        triggerSource: stored.triggerSource,
        instruction: stored.instruction,
        enabled,
        pausedReason: "",
        scheduleID: stored.scheduleID,
        createdAt: stored.createdAt,
        updatedAt: stored.updatedAt,
      }),
    );

    await this.sync(scope, updated, location);

    await this.audit(scope, {
      action: enabled ? "routine.resumed" : "routine.paused",
      objectType: "routine",
      objectID: updated.id,
      actorUserID: scope.userId ?? NilUUID,
      metadata: { kind: updated.kind },
    });

    return this.view(updated, location);
  }

  /** Removes a routine and its schedule, keeping its runs. */
  async delete(scope: Scope, id: string): Promise<void> {
    this.requireScope(scope);

    const stored = await this.deps.repository.get(scope, id);

    // The schedule goes first. A row deleted while its schedule still fires
    // would leave the engine starting workflows for a routine nothing can look
    // up, which is a run that fails forever.
    await this.remove(stored.scheduleID);
    await this.deps.repository.delete(scope, id);

    await this.audit(scope, {
      action: "routine.deleted",
      objectType: "routine",
      objectID: id,
      actorUserID: scope.userId ?? NilUUID,
      metadata: { kind: stored.kind, name: stored.name },
    });
  }

  /**
   * Re-registers every enabled routine of the workspace.
   *
   * It is what a timezone or working-hours change calls: the engine is told,
   * rather than left to fire at the old wall-clock time.
   */
  async reschedule(scope: Scope): Promise<void> {
    this.requireScope(scope);
    if (!this.deps.engine) return;

    const routines = await this.deps.repository.listEnabled(scope);
    const location = await this.location(scope);

    const failures: string[] = [];
    for (const routine of routines) {
      try {
        await this.register(scope, routine, location);
      } catch (err) {
        failures.push(`${routine.name}: ${err instanceof Error ? err.message : String(err)}`);
      }
    }

    if (failures.length > 0) {
      throw wrap(ErrScheduleUnavailable, failures.join("; "));
    }
  }

  // ------------------------------------------------------------------ runs

  /**
   * Starts the routines watching an event, and reports how many started.
   *
   * Every routine watching this event is attempted, and one that fails does not
   * stop the others. The first failure is rethrown once the loop is done, which
   * is what Go's `(int, error)` return means to the callers: they log the
   * failure and ignore the count.
   */
  async fire(scope: Scope, triggerSource: string): Promise<number> {
    const routines = await this.routinesFor(scope, triggerSource);

    let started = 0;
    let failure: Error | null = null;

    for (const routine of routines) {
      let run: Run;
      try {
        run = await this.runOnce(scope, routine.id);
      } catch (err) {
        if (!failure) {
          const detail = err instanceof Error ? err.message : String(err);
          failure = new Error(`${routine.name}: ${detail}`, { cause: err });
        }
        continue;
      }
      // Only a run that actually opened its task counts as started.
      if (run.status === RunRunning) started++;
    }

    if (failure) throw failure;
    return started;
  }

  /**
   * Executes one routine now, which is what the engine calls on each tick.
   *
   * The cheap check comes first: a routine that watches events looks at the
   * queue before anything else happens, and a queue with nothing waiting
   * finishes the run as `skipped` without a model call. A resting Bolu is the
   * same saving.
   *
   * The run stays `running` after this returns: its task runs out of process,
   * and the summary the history shows is the task's own.
   */
  async runOnce(scope: Scope, routineID: string): Promise<Run> {
    this.requireScope(scope);

    const routine = await this.deps.repository.get(scope, routineID);
    if (!routine.enabled) {
      throw wrap(ErrInvalidInput, "the routine is paused");
    }

    const pending = await this.claimPending(scope, routine);

    // Why a run did nothing, when it did nothing. Each is a real saving: no
    // event arrived, or the Bolu is not taking work, and either way no model is
    // called and no token is spent.
    if (routine.schedule().kind === KindTrigger && pending.claimed === 0) {
      return this.recordSkip(scope, routine, "tidak ada kejadian baru");
    }

    const agents = await this.agentNames(scope);
    const info = agents.get(routine.agentID);
    if (info?.resting) {
      return this.recordSkip(scope, routine, "Bolu sedang istirahat");
    }

    const run = await this.deps.repository.startRun(scope, {
      id: NilUUID,
      workspaceID: scope.workspaceId,
      routineID: routine.id,
      routineName: routine.name,
      taskID: NilUUID,
      status: RunRunning,
      summary: "",
      startedAt: new Date(0),
      finishedAt: null,
    });

    if (!this.deps.tasks) {
      return this.failRun(scope, run, new Error("the task runtime is not configured"));
    }

    let taskID: string;
    try {
      taskID = await this.deps.tasks.start(scope, {
        agentID: routine.agentID,
        title: routine.name,
        prompt: this.prompt(routine, pending),
      });
    } catch (err) {
      return this.failRun(scope, run, err instanceof Error ? err : new Error(String(err)));
    }

    // The link is written before the run is reported as dispatched, so a task
    // that finished immediately still finds the run it belongs to.
    return this.deps.repository.attachTask(scope, run.id, taskID);
  }

  /**
   * Finishes the run a task belongs to.
   *
   * A task no routine opened answers not-found, which the caller ignores: most
   * tasks in the product were opened by chat.
   */
  async recordTaskOutcome(scope: Scope, taskID: string, outcome: Outcome): Promise<void> {
    this.requireScope(scope);

    const run = await this.deps.repository.runByTask(scope, taskID);
    if (run.finishedAt !== null) {
      // Already recorded, which a retried activity does on purpose.
      return;
    }

    const runStatus = outcome.status !== "" && outcome.status !== RunSucceeded ? RunFailed : RunSucceeded;

    // A run that failed reads as the task's own reason rather than as its step
    // count: "batas langkah terlampaui" is what a person needs.
    let summary = outcome.summary;
    if (runStatus === RunFailed && outcome.reason.trim() !== "") {
      summary = outcome.reason;
    }

    await this.deps.repository.finishRun(scope, {
      runID: run.id,
      status: runStatus,
      summary,
      taskID,
      finishedAt: this.now(),
    });
  }

  /** Writes a run that found nothing to do, without opening a task. */
  private async recordSkip(scope: Scope, routine: Routine, reason: string): Promise<Run> {
    const run = await this.deps.repository.startRun(scope, {
      id: NilUUID,
      workspaceID: scope.workspaceId,
      routineID: routine.id,
      routineName: routine.name,
      taskID: NilUUID,
      status: RunSkipped,
      summary: "",
      startedAt: new Date(0),
      finishedAt: null,
    });

    return this.deps.repository.finishRun(scope, {
      runID: run.id,
      status: RunSkipped,
      summary: reason,
      taskID: NilUUID,
      finishedAt: this.now(),
    });
  }

  /**
   * Records a run whose task could not be opened. The schedule keeps firing:
   * one bad run must not stop a routine forever.
   */
  private async failRun(scope: Scope, run: Run, cause: Error): Promise<Run> {
    const finished = await this.deps.repository.finishRun(scope, {
      runID: run.id,
      status: RunFailed,
      summary: cause.message,
      taskID: NilUUID,
      finishedAt: this.now(),
    });

    this.log.warn({ routine_id: run.routineID, err: cause }, "a routine run could not open its task");
    return finished;
  }

  /**
   * Reads and claims the events waiting for one routine.
   *
   * One step rather than two: the queue's read marks the events as taken, so
   * two routines watching one event cannot both act on it.
   */
  private async claimPending(scope: Scope, routine: Routine): Promise<Claimed> {
    if (!this.deps.queue || routine.triggerSource === "") {
      return { events: [], claimed: 0 };
    }

    try {
      const events = await this.deps.queue.pending(scope, routine.triggerSource, pendingLimit);
      return { events, claimed: events.length };
    } catch (err) {
      this.log.warn(
        { routine_id: routine.id, err },
        "could not read the events a routine watches; it will run without them",
      );
      return { events: [], claimed: 0 };
    }
  }

  /**
   * Renders what the Bolu is asked to do.
   *
   * The instruction comes first and the event bodies after it, marked as data:
   * an email's text is something the Bolu reads, not something it is told.
   */
  private prompt(routine: Routine, pending: Claimed): string {
    const parts: string[] = [routine.instruction];
    if (pending.events.length > 0) {
      parts.push(`Ada ${pending.events.length} kejadian menunggu:`);
      for (const event of pending.events) {
        const payload = new TextDecoder().decode(event.payload);
        parts.push(`<data sumber=${JSON.stringify(event.event)}>\n${payload}\n</data>`);
      }
    }
    parts.push("Kerjakan rutinitas ini dan simpulkan hasilnya dalam satu kalimat.");

    return parts.join("\n");
  }

  // --------------------------------------------------------------- helpers

  /** Renders one routine with its next run times. */
  private view(routine: Routine, location: string | null): View {
    return {
      ...routine,
      nextRuns: routine.schedule().nextRuns(location, this.now(), Count),
    };
  }

  /**
   * Makes the engine's schedules match one routine, and stores the id.
   *
   * Create-or-update rather than create: the same routine registered twice is
   * one schedule. A routine whose cadence has no times gets no schedule at all
   * and its stored id is cleared, so a trigger routine that used to be daily
   * does not leave a daily schedule behind.
   */
  private async register(scope: Scope, routine: Routine, location: string | null): Promise<Routine> {
    if (!routine.enabled || !routine.schedule().scheduled()) {
      await this.remove(routine.scheduleID);
      await this.deps.repository.setScheduleID(scope, routine.id, "");
      return makeRoutine({ ...routine, scheduleID: "" });
    }

    if (!this.deps.engine) {
      throw wrap(ErrScheduleUnavailable, "no engine is configured");
    }

    let scheduleID: string;
    try {
      scheduleID = await this.deps.engine.register(routine, location);
    } catch (err) {
      throw new Error(
        `${ErrScheduleUnavailable.message}: ${err instanceof Error ? err.message : String(err)}`,
      );
    }

    if (scheduleID !== routine.scheduleID) {
      await this.deps.repository.setScheduleID(scope, routine.id, scheduleID);
      return makeRoutine({ ...routine, scheduleID });
    }

    return routine;
  }

  /** Makes one routine's schedule match its enabled state, for the pause path. */
  private async sync(scope: Scope, routine: Routine, location: string | null): Promise<void> {
    if (!this.deps.engine) {
      throw wrap(ErrScheduleUnavailable, "no engine is configured");
    }

    if (!routine.schedule().scheduled()) {
      // A trigger routine has nothing to pause: its cadence is the event.
      return;
    }

    if (routine.scheduleID === "") {
      // Never registered, which happens when the routine was created before an
      // engine existed. Registering it now is what the user's click asked for.
      if (!routine.enabled) return;
      await this.register(scope, routine, location);
      return;
    }

    await this.deps.engine.setPaused(routine.scheduleID, !routine.enabled, "dijeda dari layar Rutinitas");
  }

  private async remove(scheduleID: string): Promise<void> {
    if (!this.deps.engine || scheduleID === "") return;
    await this.deps.engine.remove(scheduleID);
  }

  /** Turns a write into a cadence, refusing one the engine could not fire. */
  private validate(request: WriteRequest): Schedule {
    const schedule = new Schedule(
      request.kind,
      request.minute,
      request.hourStart,
      request.hourEnd,
      request.dayOfWeek,
    );

    if (!Kinds.includes(schedule.kind)) {
      throw wrap(ErrInvalidInput, `unknown kind ${JSON.stringify(schedule.kind)}`);
    }
    if (schedule.minute < 0 || schedule.minute > 59) {
      throw wrap(ErrInvalidInput, `minute ${schedule.minute} is not a minute of the hour`);
    }

    switch (schedule.kind) {
      case KindDaily:
      case KindWeekly:
        if (schedule.hourStart < 0 || schedule.hourStart > 23) {
          throw wrap(ErrInvalidInput, `hour ${schedule.hourStart} is not an hour of the day`);
        }
        break;
      case KindHourlyRange:
        if (schedule.hourStart < 0 || schedule.hourStart > 23 || schedule.hourEnd < 0 || schedule.hourEnd > 23) {
          throw wrap(ErrInvalidInput, "the window must be between 00 and 23");
        }
        if (schedule.hourEnd < schedule.hourStart) {
          // A window that ends before it starts is a typo rather than a night
          // shift, so it is refused instead of being read as spanning midnight.
          throw wrap(ErrInvalidInput, "the window ends before it starts");
        }
        break;
    }

    if (schedule.kind === KindWeekly && (schedule.dayOfWeek < 1 || schedule.dayOfWeek > 7)) {
      throw wrap(ErrInvalidInput, "a weekly routine needs a weekday");
    }
    if (schedule.kind === KindTrigger && request.triggerSource.trim() === "") {
      throw wrap(ErrInvalidInput, "a trigger routine needs the event it watches");
    }
    if (schedule.kind !== KindTrigger && request.triggerSource.trim() !== "") {
      throw wrap(ErrInvalidInput, "only a trigger routine watches an event");
    }

    return schedule;
  }

  private acceptName(value: string): string {
    const trimmed = value.trim();
    if (trimmed === "") {
      throw wrap(ErrInvalidInput, "the routine needs a name");
    }
    if (Array.from(trimmed).length > nameMaxRunes) {
      throw wrap(ErrInvalidInput, `the name is longer than ${nameMaxRunes} characters`);
    }
    return trimmed;
  }

  private acceptInstruction(value: string): string {
    const trimmed = value.trim();
    if (trimmed === "") {
      throw wrap(ErrInvalidInput, "the routine needs an instruction");
    }
    if (Array.from(trimmed).length > instructionMaxRunes) {
      throw wrap(ErrInvalidInput, `the instruction is longer than ${instructionMaxRunes} characters`);
    }
    return trimmed;
  }

  /**
   * Reads the workspace's timezone, falling back to UTC when the module that
   * owns the column is not wired.
   */
  private async location(scope: Scope): Promise<string | null> {
    if (!this.deps.clock) return "UTC";

    const location = await this.deps.clock.location(scope.workspaceId);
    return location || "UTC";
  }

  /** Maps the Bolu of a workspace to their names and switches. */
  private async agentNames(scope: Scope): Promise<Map<string, AgentInfo>> {
    const byID = new Map<string, AgentInfo>();
    let agents: AgentInfo[];
    try {
      agents = await this.deps.repository.agents(scope);
    } catch {
      return byID;
    }

    for (const agent of agents) {
      byID.set(agent.id, agent);
    }
    return byID;
  }

  private async audit(scope: Scope, entry: AuditEntry): Promise<void> {
    if (!this.deps.auditor) return;

    entry.actorUserID = scope.userId ?? NilUUID;
    try {
      await this.deps.auditor.record(scope, entry);
    } catch (err) {
      this.log.warn({ action: entry.action, err }, "could not record a routine change in the audit trail");
    }
  }

  private requireScope(scope: Scope): void {
    if (isZeroScope(scope)) {
      throw wrap(ErrInvalidInput, "workspace is required");
    }
  }
}
