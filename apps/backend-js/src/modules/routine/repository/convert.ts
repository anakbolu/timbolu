/**
 * The conversions from the generated rows to the domain types.
 *
 * Every routine query selects the same column set, so one `routineRow` shape
 * serves all of them; the run rows are read the same way. Reading a run's
 * nullable link through `derefID` is what keeps a run whose routine was deleted
 * from reading as a routine with the zero uuid.
 *
 * Ported 1:1 from internal/modules/routine/repository/convert.go.
 */

import { NilUUID, makeRoutine, type Routine, type Run } from "../domain/routine.ts";

/** The column set every routine query returns. */
export interface RoutineRow {
  id: string;
  workspace_id: string;
  agent_id: string;
  name: string;
  kind: string;
  minute: number;
  hour_start: number | null;
  hour_end: number | null;
  day_of_week: number | null;
  trigger_source: string;
  instruction: string;
  enabled: boolean;
  paused_reason: string;
  schedule_id: string;
  created_at: Date;
  updated_at: Date;
}

/** The column set every run query returns. */
export interface RunRow {
  id: string;
  workspace_id: string;
  routine_id: string | null;
  routine_name: string;
  task_id: string | null;
  status: string;
  summary: string;
  started_at: Date;
  finished_at: Date | null;
}

/**
 * Renders one stored routine.
 *
 * A null hour is read as zero rather than as "no hour": the columns are
 * nullable because the bootstrap migration allowed it, and every cadence the
 * product can produce states one. The day is the opposite — null there means a
 * cadence that does not use a weekday, which is what zero means in the domain.
 */
export function toRoutine(row: RoutineRow): Routine {
  return makeRoutine({
    id: row.id,
    workspaceID: row.workspace_id,
    agentID: row.agent_id,
    name: row.name,
    kind: row.kind,
    minute: row.minute,
    hourStart: row.hour_start ?? 0,
    hourEnd: row.hour_end ?? 0,
    dayOfWeek: row.day_of_week ?? 0,
    triggerSource: row.trigger_source,
    instruction: row.instruction,
    enabled: row.enabled,
    pausedReason: row.paused_reason,
    scheduleID: row.schedule_id,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  });
}

/** Renders one stored run. */
export function toRun(row: RunRow): Run {
  return {
    id: row.id,
    workspaceID: row.workspace_id,
    routineID: row.routine_id ?? NilUUID,
    routineName: row.routine_name,
    taskID: row.task_id ?? NilUUID,
    status: row.status,
    summary: row.summary,
    startedAt: row.started_at,
    finishedAt: row.finished_at,
  };
}
