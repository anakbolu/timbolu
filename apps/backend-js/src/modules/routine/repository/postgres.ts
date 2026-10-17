/**
 * The routine data access.
 *
 * Every statement runs inside the caller's tenant scope, so Row Level Security
 * filters it even when a query forgets its workspace filter.
 *
 * Ported 1:1 from internal/modules/routine/repository/postgres.go, with
 * repository/sqlcgen/*.go folded in: each query keeps its name as a comment and
 * its SQL text unchanged.
 */

import type { Database, Tx } from "../../../platform/database/index.ts";
import {
  ErrInvalidInput,
  ErrNotFound,
  NilUUID,
  wrap,
  type Routine,
  type Run,
  type Scope,
} from "../domain/routine.ts";
import type {
  AgentInfo,
  Filter,
  FinishRun,
  Repository,
  RunFilter,
} from "../domain/service.ts";
import { toRoutine, toRun, type RoutineRow, type RunRow } from "./convert.ts";

/** The ceiling of a list read, so one request cannot pull every routine. */
const listLimit = 200;

/** The default page of a run history when the caller does not say. */
const defaultRunLimit = 50;

export class PostgresRepository implements Repository {
  constructor(private readonly db: Database) {}

  // -------------------------------------------------------------- routines

  /** name: CreateRoutine */
  async create(scope: Scope, routine: Routine): Promise<Routine> {
    return this.write(scope, async (tx) => {
      const rows = await tx<RoutineRow[]>`
        INSERT INTO routines (
            workspace_id, agent_id, name, kind, minute, hour_start, hour_end,
            day_of_week, trigger_source, instruction, enabled
        )
        SELECT
            ${scope.workspaceId},
            a.id,
            ${routine.name},
            ${routine.kind},
            ${routine.minute},
            ${routine.hourStart},
            ${routine.hourEnd},
            ${routine.dayOfWeek === 0 ? null : routine.dayOfWeek},
            ${routine.triggerSource},
            ${routine.instruction},
            ${routine.enabled}
        FROM agents a
        WHERE a.id = ${routine.agentID}
          AND a.workspace_id = ${scope.workspaceId}
          AND a.deleted_at IS NULL
        RETURNING
            id, workspace_id, agent_id, name, kind, minute, hour_start, hour_end,
            day_of_week, trigger_source, instruction, enabled, paused_reason,
            schedule_id, created_at, updated_at
      `;
      if (rows.length === 0) {
        // The insert is a SELECT, so a Bolu outside this workspace comes back
        // as no rows rather than as a constraint violation.
        throw wrap(ErrInvalidInput, "the bolu is not in this workspace");
      }
      return toRoutine(rows[0]);
    });
  }

  /** name: GetRoutine */
  async get(scope: Scope, id: string): Promise<Routine> {
    return this.read(scope, async (tx) => {
      const rows = await tx<RoutineRow[]>`
        SELECT
            id, workspace_id, agent_id, name, kind, minute, hour_start, hour_end,
            day_of_week, trigger_source, instruction, enabled, paused_reason,
            schedule_id, created_at, updated_at
        FROM routines
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
      `;
      if (rows.length === 0) throw wrap(ErrNotFound, "get routine");
      return toRoutine(rows[0]);
    });
  }

  /** name: ListRoutines */
  async list(scope: Scope, filter: Filter): Promise<Routine[]> {
    return this.read(scope, async (tx) => {
      const agentID = filter.agentID && filter.agentID !== NilUUID ? filter.agentID : null;
      const rows = await tx<RoutineRow[]>`
        SELECT
            id, workspace_id, agent_id, name, kind, minute, hour_start, hour_end,
            day_of_week, trigger_source, instruction, enabled, paused_reason,
            schedule_id, created_at, updated_at
        FROM routines
        WHERE workspace_id = ${scope.workspaceId}
          AND (${agentID}::uuid IS NULL OR agent_id = ${agentID}::uuid)
          AND (${filter.enabled}::boolean IS NULL OR enabled = ${filter.enabled}::boolean)
        ORDER BY created_at DESC
        LIMIT ${listLimit}
      `;
      return rows.map(toRoutine);
    });
  }

  /** name: ListRoutinesByTrigger */
  async listByTrigger(scope: Scope, triggerSource: string): Promise<Routine[]> {
    return this.read(scope, async (tx) => {
      const rows = await tx<RoutineRow[]>`
        SELECT
            id, workspace_id, agent_id, name, kind, minute, hour_start, hour_end,
            day_of_week, trigger_source, instruction, enabled, paused_reason,
            schedule_id, created_at, updated_at
        FROM routines
        WHERE workspace_id = ${scope.workspaceId}
          AND trigger_source = ${triggerSource}
          AND enabled
        ORDER BY created_at
      `;
      return rows.map(toRoutine);
    });
  }

  /** name: ListEnabledRoutines */
  async listEnabled(scope: Scope): Promise<Routine[]> {
    return this.read(scope, async (tx) => {
      const rows = await tx<RoutineRow[]>`
        SELECT
            id, workspace_id, agent_id, name, kind, minute, hour_start, hour_end,
            day_of_week, trigger_source, instruction, enabled, paused_reason,
            schedule_id, created_at, updated_at
        FROM routines
        WHERE workspace_id = ${scope.workspaceId} AND enabled
        ORDER BY created_at
      `;
      return rows.map(toRoutine);
    });
  }

  /** name: UpdateRoutine */
  async update(scope: Scope, routine: Routine): Promise<Routine> {
    return this.write(scope, async (tx) => {
      const rows = await tx<RoutineRow[]>`
        UPDATE routines SET
            agent_id = ${routine.agentID},
            name = ${routine.name},
            kind = ${routine.kind},
            minute = ${routine.minute},
            hour_start = ${routine.hourStart},
            hour_end = ${routine.hourEnd},
            day_of_week = ${routine.dayOfWeek === 0 ? null : routine.dayOfWeek},
            trigger_source = ${routine.triggerSource},
            instruction = ${routine.instruction},
            enabled = ${routine.enabled},
            paused_reason = ${routine.pausedReason},
            updated_at = now()
        WHERE id = ${routine.id} AND workspace_id = ${scope.workspaceId}
        RETURNING
            id, workspace_id, agent_id, name, kind, minute, hour_start, hour_end,
            day_of_week, trigger_source, instruction, enabled, paused_reason,
            schedule_id, created_at, updated_at
      `;
      if (rows.length === 0) throw wrap(ErrNotFound, "update routine");
      return toRoutine(rows[0]);
    });
  }

  /** name: SetRoutineScheduleID */
  async setScheduleID(scope: Scope, id: string, scheduleID: string): Promise<void> {
    await this.write(scope, async (tx) => {
      await tx`
        UPDATE routines SET schedule_id = ${scheduleID}, updated_at = now()
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
      `;
    });
  }

  /** name: DeleteRoutine */
  async delete(scope: Scope, id: string): Promise<void> {
    await this.write(scope, async (tx) => {
      const rows = await tx`
        DELETE FROM routines
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
      `;
      if (rows.count === 0) throw ErrNotFound;
    });
  }

  // ------------------------------------------------------------------ runs

  /** name: StartRoutineRun */
  async startRun(scope: Scope, run: Run): Promise<Run> {
    return this.write(scope, async (tx) => {
      const rows = await tx<RunRow[]>`
        INSERT INTO routine_runs (workspace_id, routine_id, routine_name, status)
        VALUES (
            ${scope.workspaceId},
            ${optionalID(run.routineID)},
            ${run.routineName},
            ${run.status}
        )
        RETURNING id, workspace_id, routine_id, routine_name, task_id, status, summary, started_at, finished_at
      `;
      if (rows.length === 0) throw wrap(ErrNotFound, "start run");
      return toRun(rows[0]);
    });
  }

  /**
   * name: FinishRoutineRun
   *
   * The update only matches a run that has not finished, so a retried activity
   * does not overwrite the first outcome. When nothing matched, the row is read
   * back and returned: "this run already has an outcome" is the answer the
   * caller asked for.
   */
  async finishRun(scope: Scope, finish: FinishRun): Promise<Run> {
    return this.write(scope, async (tx) => {
      const rows = await tx<RunRow[]>`
        UPDATE routine_runs SET
            status = ${finish.status},
            summary = ${finish.summary},
            task_id = ${optionalID(finish.taskID)},
            finished_at = ${finish.finishedAt}
        WHERE id = ${finish.runID}
          AND workspace_id = ${scope.workspaceId}
          AND finished_at IS NULL
        RETURNING id, workspace_id, routine_id, routine_name, task_id, status, summary, started_at, finished_at
      `;
      if (rows.length > 0) return toRun(rows[0]);

      // name: GetRoutineRun
      const existing = await tx<RunRow[]>`
        SELECT id, workspace_id, routine_id, routine_name, task_id, status, summary, started_at, finished_at
        FROM routine_runs
        WHERE id = ${finish.runID} AND workspace_id = ${scope.workspaceId}
      `;
      if (existing.length === 0) throw wrap(ErrNotFound, "finish run");
      return toRun(existing[0]);
    });
  }

  /** name: LatestRoutineRun */
  async latestRun(scope: Scope, routineID: string): Promise<Run> {
    return this.read(scope, async (tx) => {
      const rows = await tx<RunRow[]>`
        SELECT id, workspace_id, routine_id, routine_name, task_id, status, summary, started_at, finished_at
        FROM routine_runs
        WHERE workspace_id = ${scope.workspaceId}
          AND routine_id = ${routineID}
        ORDER BY started_at DESC, id DESC
        LIMIT 1
      `;
      if (rows.length === 0) throw wrap(ErrNotFound, "latest run");
      return toRun(rows[0]);
    });
  }

  /** name: AttachRoutineRunTask */
  async attachTask(scope: Scope, runID: string, taskID: string): Promise<Run> {
    return this.write(scope, async (tx) => {
      const rows = await tx<RunRow[]>`
        UPDATE routine_runs SET task_id = ${optionalID(taskID)}
        WHERE id = ${runID}
          AND workspace_id = ${scope.workspaceId}
          AND finished_at IS NULL
        RETURNING id, workspace_id, routine_id, routine_name, task_id, status, summary, started_at, finished_at
      `;
      if (rows.length === 0) throw wrap(ErrNotFound, "attach task");
      return toRun(rows[0]);
    });
  }

  /** name: GetRoutineRunByTask */
  async runByTask(scope: Scope, taskID: string): Promise<Run> {
    return this.read(scope, async (tx) => {
      const rows = await tx<RunRow[]>`
        SELECT id, workspace_id, routine_id, routine_name, task_id, status, summary, started_at, finished_at
        FROM routine_runs
        WHERE workspace_id = ${scope.workspaceId} AND task_id = ${taskID}
        ORDER BY started_at DESC
        LIMIT 1
      `;
      if (rows.length === 0) throw wrap(ErrNotFound, "run by task");
      return toRun(rows[0]);
    });
  }

  /** name: ListRoutineRuns */
  async listRuns(scope: Scope, filter: RunFilter): Promise<Run[]> {
    let limit = filter.limit;
    if (limit <= 0) limit = defaultRunLimit;
    if (limit > listLimit) limit = listLimit;

    return this.read(scope, async (tx) => {
      const routineID = optionalID(filter.routineID);
      const agentID = optionalID(filter.agentID);
      const status = filter.status === "" ? null : filter.status;

      const rows = await tx<RunRow[]>`
        SELECT
            run.id, run.workspace_id, run.routine_id, run.routine_name, run.task_id,
            run.status, run.summary, run.started_at, run.finished_at
        FROM routine_runs run
        LEFT JOIN routines r ON r.id = run.routine_id
        WHERE run.workspace_id = ${scope.workspaceId}
          AND (
                ${routineID}::uuid IS NULL
                OR run.routine_id = ${routineID}::uuid
              )
          AND (${agentID}::uuid IS NULL OR r.agent_id = ${agentID}::uuid)
          AND (${status}::text IS NULL OR run.status = ${status}::text)
        ORDER BY run.started_at DESC, run.id DESC
        LIMIT ${limit}
      `;
      return rows.map(toRun);
    });
  }

  // ---------------------------------------------------------------- agents

  /** name: ListRoutineAgents */
  async agents(scope: Scope): Promise<AgentInfo[]> {
    return this.read(scope, async (tx) => {
      const rows = await tx<{ id: string; name: string; resting: boolean }[]>`
        SELECT id, name, (status = 'resting') AS resting FROM agents
        WHERE workspace_id = ${scope.workspaceId} AND deleted_at IS NULL
        ORDER BY name
      `;
      return rows.map((row) => ({ id: row.id, name: row.name, resting: row.resting }));
    });
  }

  // --------------------------------------------------------------- helpers

  private async read<T>(scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    return this.db.inScopeRead(
      { userId: scope.userId ?? null, workspaceId: scope.workspaceId },
      fn,
    );
  }

  private async write<T>(scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    return this.db.inScope(
      { userId: scope.userId ?? null, workspaceId: scope.workspaceId },
      fn,
    );
  }
}

/** Writes an id that may be absent; a zero uuid would reference a row nobody has. */
function optionalID(id: string): string | null {
  return !id || id === NilUUID ? null : id;
}
