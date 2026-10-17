/**
 * Routines over HTTP. It talks to the domain service interface only.
 *
 * Ported 1:1 from internal/modules/routine/handler/handler.go. Messages, error
 * codes, and JSON field names are the frontend's contract and are copied
 * verbatim.
 */

import type { Hono } from "hono";
import type { AppContext, AppEnv } from "../../../platform/context.ts";
import { currentUserId, isUuid, workspaceUUID } from "../../../platform/context.ts";
import { AppError } from "../../../platform/errors.ts";
import type { Logger } from "../../../platform/logger.ts";
import { error, ok, okWithMeta, created } from "../../../platform/response.ts";
import {
  ErrInvalidInput,
  ErrNotFound,
  ErrScheduleUnavailable,
  NilUUID,
  is,
  userMessage,
  type Run,
  type Scope,
} from "../domain/routine.ts";
import type {
  Filter,
  PreviewRequest,
  RunFilter,
  Service,
  View,
  WriteRequest,
} from "../domain/service.ts";

/** Serves the routine endpoints. */
export class Handler {
  constructor(
    private readonly service: Service,
    private readonly log: Logger,
  ) {}

  /**
   * The workspace's routines with their next run times.
   *
   * Routines are readable and editable by every member: a routine is the team's
   * work rather than an administrative setting.
   */
  list = async (c: AppContext): Promise<Response> => {
    const agentID = this.parseOptionalID(c.req.query("agent_id"));
    if (agentID === null) {
      return error(c, 400, "agent_id is not a uuid", "invalid_input", null);
    }

    const rawEnabled = (c.req.query("enabled") ?? "").trim();
    const filter: Filter = {
      agentID: agentID || NilUUID,
      enabled: rawEnabled === "" ? null : rawEnabled === "true",
    };

    try {
      const routines = await this.service.routines(scopeOf(c), filter);
      const payload = routines.map(wireRoutine);
      return okWithMeta(c, "ok", payload, { count: payload.length });
    } catch (err) {
      throw this.fail("list routines", err);
    }
  };

  /** One routine. */
  get = async (c: AppContext): Promise<Response> => {
    const id = this.parseID(c.req.param("routineID"));
    if (id === null) {
      return error(c, 400, "routine id is not a uuid", "invalid_input", null);
    }

    try {
      const routine = await this.service.routine(scopeOf(c), id);
      return ok(c, "ok", wireRoutine(routine));
    } catch (err) {
      throw this.fail("get routine", err);
    }
  };

  /** Stores a routine and registers its schedule. */
  create = async (c: AppContext): Promise<Response> => {
    const body = await readBody(c);
    if (!body) {
      return error(c, 400, "the body is not valid JSON", "invalid_request", null);
    }

    const agentID = this.parseID(asString(body.agent_id));
    if (agentID === null) {
      return error(c, 400, "agent_id is not a uuid", "invalid_input", null);
    }

    try {
      const routine = await this.service.create(scopeOf(c), writeRequestOf(body, agentID));
      return created(c, "routine created", wireRoutine(routine));
    } catch (err) {
      throw this.fail("create routine", err);
    }
  };

  /** Changes a routine and re-registers its schedule. */
  update = async (c: AppContext): Promise<Response> => {
    const id = this.parseID(c.req.param("routineID"));
    if (id === null) {
      return error(c, 400, "routine id is not a uuid", "invalid_input", null);
    }

    const body = await readBody(c);
    if (!body) {
      return error(c, 400, "the body is not valid JSON", "invalid_request", null);
    }

    // A body id that does not parse means "leave the Bolu alone", which is what
    // Go's optionalParse does: the stored agent is kept.
    const agentID = isUuid(asString(body.agent_id).trim()) ? asString(body.agent_id).trim() : NilUUID;

    try {
      const routine = await this.service.update(scopeOf(c), id, writeRequestOf(body, agentID));
      return ok(c, "routine updated", wireRoutine(routine));
    } catch (err) {
      throw this.fail("update routine", err);
    }
  };

  /** Removes a routine and its schedule, keeping its history. */
  delete = async (c: AppContext): Promise<Response> => {
    const id = this.parseID(c.req.param("routineID"));
    if (id === null) {
      return error(c, 400, "routine id is not a uuid", "invalid_input", null);
    }

    try {
      await this.service.delete(scopeOf(c), id);
      return ok(c, "routine deleted", null);
    } catch (err) {
      throw this.fail("delete routine", err);
    }
  };

  /** Stops a routine firing without losing its history. */
  pause = async (c: AppContext): Promise<Response> => {
    return this.setEnabled(c, false);
  };

  /** Puts a paused routine back. */
  resume = async (c: AppContext): Promise<Response> => {
    return this.setEnabled(c, true);
  };

  /** One routine's history. */
  runs = async (c: AppContext): Promise<Response> => {
    const id = this.parseID(c.req.param("routineID"));
    if (id === null) {
      return error(c, 400, "routine id is not a uuid", "invalid_input", null);
    }

    // Reading the routine first confirms the caller may see it: another
    // workspace's id answers "not found" rather than an empty list.
    try {
      await this.service.routine(scopeOf(c), id);
    } catch (err) {
      throw this.fail("get routine", err);
    }

    try {
      const runs = await this.service.runs(scopeOf(c), {
        routineID: id,
        agentID: NilUUID,
        status: "",
        limit: queryInt(c, "limit", 0),
      });
      return okWithMeta(c, "ok", runs.map(wireRun), { count: runs.length });
    } catch (err) {
      throw this.fail("list runs", err);
    }
  };

  /** The workspace's history, so the screen can filter it by Bolu. */
  allRuns = async (c: AppContext): Promise<Response> => {
    const agentID = this.parseOptionalID(c.req.query("agent_id"));
    if (agentID === null) {
      return error(c, 400, "agent_id is not a uuid", "invalid_input", null);
    }

    const routineID = this.parseOptionalID(c.req.query("routine_id"));
    if (routineID === null) {
      return error(c, 400, "routine_id is not a uuid", "invalid_input", null);
    }

    const filter: RunFilter = {
      status: (c.req.query("status") ?? "").trim(),
      limit: queryInt(c, "limit", 0),
      agentID: agentID || NilUUID,
      routineID: routineID || NilUUID,
    };

    try {
      const runs = await this.service.runs(scopeOf(c), filter);
      return okWithMeta(c, "ok", runs.map(wireRun), { count: runs.length });
    } catch (err) {
      throw this.fail("list runs", err);
    }
  };

  /** The next run times for a cadence a form is filling in. */
  preview = async (c: AppContext): Promise<Response> => {
    const body = await readBody(c);
    if (!body) {
      return error(c, 400, "the body is not valid JSON", "invalid_request", null);
    }

    const request: PreviewRequest = {
      kind: asString(body.kind),
      minute: asNumber(body.minute),
      hourStart: asNumber(body.hour_start),
      hourEnd: asNumber(body.hour_end),
      dayOfWeek: asNumber(body.day_of_week),
    };

    try {
      const times = await this.service.preview(scopeOf(c), request);
      return ok(c, "ok", { next_runs: times.map(rfc3339) });
    } catch (err) {
      throw this.fail("preview schedule", err);
    }
  };

  private async setEnabled(c: AppContext, enabled: boolean): Promise<Response> {
    const id = this.parseID(c.req.param("routineID"));
    if (id === null) {
      return error(c, 400, "routine id is not a uuid", "invalid_input", null);
    }

    try {
      const routine = await this.service.setEnabled(scopeOf(c), id, enabled);
      return ok(c, enabled ? "rutinitas dilanjutkan" : "rutinitas dijeda", wireRoutine(routine));
    } catch (err) {
      throw this.fail("set routine enabled", err);
    }
  }

  /** Parses a required id, or returns null when it is not a uuid. */
  private parseID(value: string | undefined): string | null {
    return isUuid(value) ? value : null;
  }

  /** Parses an id that may be absent; null means malformed, "" means absent. */
  private parseOptionalID(value: string | undefined): string | null {
    const trimmed = (value ?? "").trim();
    if (trimmed === "") return "";
    return isUuid(trimmed) ? trimmed : null;
  }

  /** Maps the module's errors onto the response envelope's status and code. */
  private fail(operation: string, err: unknown): AppError {
    if (is(err, ErrNotFound)) {
      return new AppError(404, "routine not found", "routine_not_found");
    }
    if (is(err, ErrScheduleUnavailable)) {
      // A deployment whose engine is unreachable cannot schedule anything. That
      // is the platform, not the request, so it is a 503.
      return new AppError(503, err instanceof Error ? err.message : String(err), "routine_unavailable");
    }
    if (is(err, ErrInvalidInput)) {
      return new AppError(400, userMessage(err, ErrInvalidInput), "invalid_input");
    }

    this.log.error({ operation, err }, "routine request failed");
    return new AppError(500, "internal server error", "internal_error");
  }
}

/** Registers the endpoints every member of the workspace may use. */
export function routes(group: Hono<AppEnv>, h: Handler): void {
  group.get("/routines", h.list);
  group.post("/routines", h.create);
  group.get("/routines/:routineID", h.get);
  group.patch("/routines/:routineID", h.update);
  group.delete("/routines/:routineID", h.delete);
  group.post("/routines/:routineID/pause", h.pause);
  group.post("/routines/:routineID/resume", h.resume);
  group.get("/routines/:routineID/runs", h.runs);
  group.get("/routine-runs", h.allRuns);
  group.post("/routines/preview", h.preview);
}

/** A routine as a client reads it. */
interface RoutinePayload {
  id: string;
  agent_id: string;
  name: string;
  kind: string;
  minute: number;
  hour_start: number;
  hour_end: number;
  day_of_week?: number;
  trigger_source?: string;
  instruction: string;
  enabled: boolean;
  paused_reason?: string;
  schedule: string;
  scheduled: boolean;
  next_runs: string[];
  created_at: string;
}

/** One run as a client reads it. */
interface RunPayload {
  run_id: string;
  routine_id?: string;
  routine_name: string;
  task_id?: string;
  status: string;
  summary: string;
  started_at: string;
  finished_at?: string;
}

/**
 * Renders one routine with the times it will next run at.
 *
 * `omitempty` is reproduced by omission: Go drops a zero `day_of_week`, an empty
 * `trigger_source`, and an empty `paused_reason`.
 */
function wireRoutine(view: View): RoutinePayload {
  const schedule = view.schedule();
  const payload: RoutinePayload = {
    id: view.id,
    agent_id: view.agentID,
    name: view.name,
    kind: view.kind,
    minute: view.minute,
    hour_start: view.hourStart,
    hour_end: view.hourEnd,
    instruction: view.instruction,
    enabled: view.enabled,
    schedule: schedule.describe(),
    scheduled: schedule.scheduled(),
    next_runs: view.nextRuns.map(rfc3339),
    created_at: rfc3339(view.createdAt),
  };
  if (view.dayOfWeek !== 0) payload.day_of_week = view.dayOfWeek;
  if (view.triggerSource !== "") payload.trigger_source = view.triggerSource;
  if (view.pausedReason !== "") payload.paused_reason = view.pausedReason;
  return payload;
}

/**
 * Renders one run.
 *
 * The ids travel as empty strings when absent rather than as the zero uuid,
 * because a client would read `00000000-…` as a real routine.
 */
function wireRun(run: Run): RunPayload {
  const payload: RunPayload = {
    run_id: run.id,
    routine_name: run.routineName,
    status: run.status,
    summary: run.summary,
    started_at: rfc3339(run.startedAt),
  };
  if (run.routineID !== NilUUID) payload.routine_id = run.routineID;
  if (run.taskID !== NilUUID) payload.task_id = run.taskID;
  if (run.finishedAt !== null) payload.finished_at = rfc3339(run.finishedAt);
  return payload;
}

/** Renders an instant the way Go's `Format(time.RFC3339)` does: UTC, no fraction. */
function rfc3339(at: Date): string {
  return at.toISOString().replace(/\.\d{3}Z$/, "Z");
}

/** The tenant scope of the request. */
function scopeOf(c: AppContext): Scope {
  return {
    userId: currentUserId(c) ?? null,
    workspaceId: workspaceUUID(c) ?? NilUUID,
  };
}

/** Reads a JSON body, or null when it is not valid JSON. */
async function readBody(c: AppContext): Promise<Record<string, unknown> | null> {
  const parsed: unknown = await c.req.json().catch(() => null);
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return null;
  return parsed as Record<string, unknown>;
}

function asString(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function asNumber(value: unknown): number {
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

/** Reads a query integer, answering the fallback when it is absent or malformed. */
function queryInt(c: AppContext, name: string, fallback: number): number {
  const raw = c.req.query(name);
  if (raw === undefined || raw.trim() === "") return fallback;
  const parsed = Number.parseInt(raw, 10);
  return Number.isNaN(parsed) ? fallback : parsed;
}

/** The write a body describes, with the id the caller resolved. */
function writeRequestOf(body: Record<string, unknown>, agentID: string): WriteRequest {
  return {
    agentID,
    name: asString(body.name),
    kind: asString(body.kind),
    minute: asNumber(body.minute),
    hourStart: asNumber(body.hour_start),
    hourEnd: asNumber(body.hour_end),
    dayOfWeek: asNumber(body.day_of_week),
    triggerSource: asString(body.trigger_source),
    instruction: asString(body.instruction),
    // An absent switch on a create means "on", because a routine somebody just
    // wrote is one they want to run.
    enabled: typeof body.enabled === "boolean" ? body.enabled : true,
  };
}
