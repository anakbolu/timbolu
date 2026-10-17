/**
 * The routine service contract: what the handler calls, the ports the service
 * needs, and the shapes that travel between them.
 *
 * Ported 1:1 from internal/modules/routine/domain/service.go.
 */

import type { Routine, Run, Scope } from "./routine.ts";

/** A routine with the times it will next run at. */
export interface View extends Routine {
  nextRuns: Date[];
}

/** How the task a run opened ended. */
export interface Outcome {
  /** The task's terminal status: succeeded, failed, or canceled. */
  status: string;
  /** The task's own one line, which is what the history shows. */
  summary: string;
  /** Why a task stopped without succeeding, in the user's words. */
  reason: string;
}

/** Narrows a routine read. */
export interface Filter {
  agentID: string;
  enabled: boolean | null;
}

/** Narrows a run read. */
export interface RunFilter {
  routineID: string;
  agentID: string;
  status: string;
  limit: number;
}

/** One routine to store. */
export interface WriteRequest {
  agentID: string;
  name: string;
  kind: string;
  minute: number;
  hourStart: number;
  hourEnd: number;
  dayOfWeek: number;
  triggerSource: string;
  instruction: string;
  enabled: boolean;
}

/** A cadence a form is asking about. */
export interface PreviewRequest {
  kind: string;
  minute: number;
  hourStart: number;
  hourEnd: number;
  dayOfWeek: number;
}

/** How many next runs a preview answers with. */
export const Count = 5;

/**
 * What one routine run is started with.
 *
 * Every field is an id, so a restart re-reads the current state instead of
 * replaying a snapshot. The keys are Go's field names because the Go struct has
 * no JSON tags and Go's default converter encodes field names as they are — a
 * running deployment's schedule history decodes with exactly these keys.
 */
export interface RunInput {
  RoutineID: string;
  WorkspaceID: string;
}

/** Persists routines and their runs. */
export interface Repository {
  create(scope: Scope, routine: Routine): Promise<Routine>;
  get(scope: Scope, id: string): Promise<Routine>;
  list(scope: Scope, filter: Filter): Promise<Routine[]>;
  /** Finds the routines watching one event. */
  listByTrigger(scope: Scope, triggerSource: string): Promise<Routine[]>;
  /** Every enabled routine of the workspace, which is what a reschedule walks. */
  listEnabled(scope: Scope): Promise<Routine[]>;
  update(scope: Scope, routine: Routine): Promise<Routine>;
  /** Records the engine's id for a routine. */
  setScheduleID(scope: Scope, id: string, scheduleID: string): Promise<void>;
  /** Removes the routine. Its runs survive: the link is dropped. */
  delete(scope: Scope, id: string): Promise<void>;

  startRun(scope: Scope, run: Run): Promise<Run>;
  /** Records how a run ended, idempotently. */
  finishRun(scope: Scope, finish: FinishRun): Promise<Run>;
  /** The newest run of a routine. */
  latestRun(scope: Scope, routineID: string): Promise<Run>;
  /** Records the task a run opened. */
  attachTask(scope: Scope, runID: string, taskID: string): Promise<Run>;
  /** The run a task belongs to. */
  runByTask(scope: Scope, taskID: string): Promise<Run>;
  listRuns(scope: Scope, filter: RunFilter): Promise<Run[]>;
  /** The Bolu of the workspace, with the name a routine is labelled with. */
  agents(scope: Scope): Promise<AgentInfo[]>;
}

/** One run's outcome. */
export interface FinishRun {
  runID: string;
  status: string;
  summary: string;
  taskID: string;
  finishedAt: Date;
}

/** The per-Bolu part a routine's label needs: the name and the rest switch. */
export interface AgentInfo {
  id: string;
  name: string;
  resting: boolean;
}

/**
 * Registers, updates, pauses, and removes the schedules a routine is fired by.
 *
 * A port rather than a direct Temporal call, so the module stays runnable
 * without a Temporal server: only the writes that need a schedule report that
 * none can be registered.
 */
export interface Engine {
  /** Creates or replaces one routine's schedule, idempotently. */
  register(routine: Routine, location: string | null): Promise<string>;
  /** Pauses or resumes a schedule without removing it. */
  setPaused(scheduleID: string, paused: boolean, reason: string): Promise<void>;
  /** Deletes a schedule. One already gone is not an error. */
  remove(scheduleID: string): Promise<void>;
}

/** Opens the task a run performs. */
export interface TaskStarter {
  start(scope: Scope, request: StartTask): Promise<string>;
}

/** One run's work. */
export interface StartTask {
  agentID: string;
  title: string;
  prompt: string;
}

/**
 * Reports the unconsumed events a routine watches.
 *
 * This is the cheap check the product's economics rest on: a routine started
 * with nothing waiting finishes without calling a model.
 */
export interface WorkQueue {
  /** Returns the oldest unclaimed events of one kind, and claims them. */
  pending(scope: Scope, triggerSource: string, limit: number): Promise<Event[]>;
}

/** One queued delivery a routine acts on. */
export interface Event {
  id: string;
  app: string;
  event: string;
  payload: Uint8Array;
}

/** Records what a person did to a routine. */
export interface Auditor {
  record(scope: Scope, entry: AuditEntry): Promise<void>;
}

/** One thing that happened, for the record. */
export interface AuditEntry {
  action: string;
  objectType: string;
  objectID: string;
  actorUserID: string;
  metadata: Record<string, unknown>;
}

/** Audit actions this module records. */
export const ActionCreated = "routine.created";
export const ActionUpdated = "routine.updated";
export const ActionPaused = "routine.paused";
export const ActionResumed = "routine.resumed";
export const ActionDeleted = "routine.deleted";

/**
 * Reads the zone a routine is scheduled in. It is the zone and nothing else: a
 * routine carries its own hours.
 */
export interface WorkspaceClock {
  /** Returns the workspace's timezone name, or null for UTC. */
  location(workspaceID: string): Promise<string | null>;
}

/** What the HTTP handlers, the webhook path, and the worker call. */
export interface Service {
  /** The workspace's routines, newest first, each with its next run times. */
  routines(scope: Scope, filter: Filter): Promise<View[]>;
  /** One routine with the times it will next run at. */
  routine(scope: Scope, id: string): Promise<View>;
  /** The enabled routines watching one event, which a delivery is dispatched to. */
  routinesFor(scope: Scope, triggerSource: string): Promise<Routine[]>;

  /** Stores a routine and registers its schedule in one step. */
  create(scope: Scope, request: WriteRequest): Promise<View>;
  /** Stores a change and re-registers the schedule. */
  update(scope: Scope, id: string, request: WriteRequest): Promise<View>;
  /** Pauses or resumes a routine's schedule, keeping its history. */
  setEnabled(scope: Scope, id: string, enabled: boolean): Promise<View>;
  /** Removes the routine and its schedule, and keeps its run history. */
  delete(scope: Scope, id: string): Promise<void>;

  /** The run history, newest first, optionally for one routine. */
  runs(scope: Scope, filter: RunFilter): Promise<Run[]>;

  /** The next run times for a cadence that has not been saved yet. */
  preview(scope: Scope, request: PreviewRequest): Promise<Date[]>;

  /** Re-registers every enabled routine of the workspace. */
  reschedule(scope: Scope): Promise<void>;

  /** Starts the routines watching an event, and reports how many started. */
  fire(scope: Scope, triggerSource: string): Promise<number>;

  /** Opens one run of one routine and hands its work to the task runtime. */
  runOnce(scope: Scope, routineID: string): Promise<Run>;

  /** Finishes the run a task belongs to. */
  recordTaskOutcome(scope: Scope, taskID: string, outcome: Outcome): Promise<void>;
}
