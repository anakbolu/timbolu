/**
 * The retention module's own storage: the queue of workspaces due a pass, and
 * the record of what each pass did.
 *
 * Every statement here runs outside a tenant scope, and that is not an
 * oversight — it is what these two tables are. The queue exists to tell the
 * sweep which workspace to open a scope with, so a policy keyed on
 * `app_current_workspace_id()` would make it match nothing; and the run log
 * carries counts and a moment and no workspace, task or content at all.
 * Everything the job then does to a task step or an attachment runs inside that
 * workspace's own scope, in the module that owns the table.
 *
 * Ported from internal/modules/retention/repository/postgres.go and the sqlc
 * queries in repository/sqlcgen (retention.sql.go, models.go).
 */

import type { Database } from "../../../platform/database/index.ts";
import {
  DefaultLimit,
  ErrInvalidInput,
  NilUUID,
  wrap,
  type Repository,
  type Run,
  type RunResult,
  type Scope,
} from "../domain/retention.ts";
import {
  ErrErasureNotFound,
  type Erasure,
  type ErasureRepository,
  type ErasureRequest,
  type ErasureResult,
} from "../domain/erasure.ts";

/** The row shape of `data_erasures`. */
export interface DataErasureRow {
  id: number;
  workspace_id: string;
  workspace_name: string;
  requested_by: string;
  requested_by_email: string;
  requested_at: Date;
  completed_at: Date | null;
  objects_deleted: number;
  rows_deleted: number;
  error: string;
}

/** The row shape of `retention_runs`. */
export interface RetentionRunRow {
  id: number;
  started_at: Date;
  finished_at: Date | null;
  steps_pruned: number;
  attachments_pruned: number;
  bytes_freed: number;
  error: string;
}

/**
 * The Postgres-backed implementation of `Repository` and `ErasureRepository`.
 *
 * The erasure evidence lives here too because the Go module declares it on the
 * same struct; the service receives it through its own port.
 */
export class PostgresRepository implements Repository, ErasureRepository {
  constructor(private readonly db: Database) {}

  // ---------------------------------------------------------------- the queue

  /** name: ListDueRetentionTargets */
  async dueTargets(now: Date, limit: number): Promise<string[]> {
    if (!this.db) throw wrap(ErrInvalidInput, "database is not configured");

    const rows = await this.db.sql<{ workspace_id: string }[]>`
      SELECT workspace_id FROM retention_targets
      WHERE next_prune_at <= ${now}
      ORDER BY next_prune_at
      LIMIT ${limitOr(limit)}
    `;
    return rows.map((row) => row.workspace_id);
  }

  /** name: TouchRetentionTarget */
  async touchTarget(scope: Scope, next: Date): Promise<void> {
    if (!this.db) throw wrap(ErrInvalidInput, "database is not configured");
    if (!scope.workspaceId || scope.workspaceId === NilUUID) {
      throw wrap(ErrInvalidInput, "workspace is required");
    }

    // It runs at the owner's level rather than in the workspace's scope,
    // because the queue row is what a scope is opened *from*: an update through
    // the workspace policy would need the scope this update exists to schedule.
    // The statement names its workspace, so it still cannot move a row it was
    // not asked to.
    await this.db.sql`
      UPDATE retention_targets
      SET next_prune_at = ${next}, updated_at = now()
      WHERE workspace_id = ${scope.workspaceId}
    `;
  }

  // ---------------------------------------------------------------- the run log

  /** name: StartRetentionRun */
  async startRun(): Promise<number> {
    if (!this.db) throw wrap(ErrInvalidInput, "database is not configured");

    const rows = await this.db.sql<{ id: number }[]>`
      INSERT INTO retention_runs (started_at) VALUES (now())
      RETURNING id
    `;
    return rows[0].id;
  }

  /**
   * name: FinishRetentionRun
   *
   * The counts and the reason are written as values rather than coalesced,
   * because a run that finished with nothing to do is a different fact from one
   * that has not finished.
   */
  async finishRun(id: number, result: RunResult, failure: Error | null): Promise<void> {
    if (!this.db) throw wrap(ErrInvalidInput, "database is not configured");

    await this.db.sql`
      UPDATE retention_runs
      SET finished_at = now(),
          steps_pruned = ${result.steps_pruned},
          attachments_pruned = ${result.attachments_pruned},
          bytes_freed = ${result.bytes_freed},
          error = ${failure ? failure.message : ""}
      WHERE id = ${id}
    `;
  }

  /** name: ListRetentionRuns */
  async runs(limit: number): Promise<Run[]> {
    if (!this.db) throw wrap(ErrInvalidInput, "database is not configured");

    const rows = await this.db.sql<RetentionRunRow[]>`
      SELECT * FROM retention_runs
      ORDER BY started_at DESC
      LIMIT ${limitOr(limit)}
    `;
    return rows.map(toRun);
  }

  // ---------------------------------------------------------------- the erasure evidence

  /**
   * name: StartErasure
   *
   * Unscoped like the run log and for the same reason: the row records that a
   * tenant was removed, so it cannot be written inside that tenant's scope —
   * and it carries an id, a name, a moment, and counts rather than anything the
   * customer said.
   */
  async start(
    request: ErasureRequest,
    workspaceName: string,
    requesterEmail: string,
  ): Promise<number> {
    if (!this.db) throw wrap(ErrInvalidInput, "database is not configured");

    const rows = await this.db.sql<{ id: number }[]>`
      INSERT INTO data_erasures (
        workspace_id, workspace_name, requested_by, requested_by_email
      ) VALUES (
        ${request.workspaceId}, ${workspaceName},
        ${request.requestedBy}, ${requesterEmail}
      )
      RETURNING id
    `;
    return rows[0].id;
  }

  /** name: FinishErasure */
  async finish(id: number, result: ErasureResult, failure: Error | null): Promise<void> {
    if (!this.db) throw wrap(ErrInvalidInput, "database is not configured");

    await this.db.sql`
      UPDATE data_erasures
      SET completed_at = now(),
          objects_deleted = ${result.objects},
          rows_deleted = ${result.rows},
          error = ${failure ? failure.message : ""}
      WHERE id = ${id}
    `;
  }

  /** name: ListErasures */
  async list(limit: number): Promise<Erasure[]> {
    if (!this.db) throw wrap(ErrInvalidInput, "database is not configured");

    const rows = await this.db.sql<DataErasureRow[]>`
      SELECT * FROM data_erasures
      ORDER BY requested_at DESC
      LIMIT ${limitOr(limit)}
    `;
    return rows.map(toErasure);
  }

  /** name: FindErasure */
  async find(workspaceId: string): Promise<Erasure> {
    if (!this.db) throw wrap(ErrInvalidInput, "database is not configured");

    const rows = await this.db.sql<DataErasureRow[]>`
      SELECT * FROM data_erasures
      WHERE workspace_id = ${workspaceId} AND completed_at IS NOT NULL
      ORDER BY requested_at DESC
      LIMIT 1
    `;
    if (rows.length === 0) throw wrap(ErrErasureNotFound, workspaceId);
    return toErasure(rows[0]);
  }
}

/** Maps a row, turning the absent completion back into `null`. */
function toErasure(row: DataErasureRow): Erasure {
  return {
    id: row.id,
    workspaceId: row.workspace_id,
    workspaceName: row.workspace_name,
    requestedBy: row.requested_by,
    requestedByMail: row.requested_by_email,
    requestedAt: row.requested_at,
    completedAt: row.completed_at ?? null,
    objects: row.objects_deleted,
    rows: row.rows_deleted,
    error: row.error,
  };
}

/** Maps a row, turning the absent finish time back into `null`. */
function toRun(row: RetentionRunRow): Run {
  return {
    id: row.id,
    startedAt: row.started_at,
    finishedAt: row.finished_at ?? null,
    stepsPruned: row.steps_pruned,
    attachments: row.attachments_pruned,
    bytesFreed: row.bytes_freed,
    error: row.error,
  };
}

/** Takes the deployment's bound or the module's default. */
function limitOr(limit: number): number {
  if (limit <= 0) return DefaultLimit;
  return limit;
}
