/**
 * The task runtime data access with postgres.js.
 *
 * Every statement runs inside the caller's tenant scope, so Row Level Security
 * filters it even when a query forgets its workspace filter. The writes are
 * guarded by status in the statement itself: a retried activity cannot overwrite
 * a result the user has already seen.
 *
 * Ported 1:1 from internal/modules/task/repository/postgres.go and the folded
 * sqlc queries in internal/modules/task/repository/queries/task.sql (query
 * names kept as comments above each function).
 */

import type { Database, Tx } from "../../../platform/database/index.ts";
import type { Scope as DatabaseScope } from "../../../platform/database/index.ts";
import {
  DefaultHistoryLimit,
  DefaultPruneLimit,
  ErrInvalidInput,
  ErrTaskNotFound,
  MaxHistoryLimit,
  wrap,
  type Filter,
  type Finish,
  type NewTask,
  type Progress,
  type Scope,
  type Step,
  type StepPrune,
  type Task,
  type TaskRepository,
  type StepRepository,
  type StepPruner,
} from "../domain/task.ts";

/** A row of `tasks`. */
interface TaskRow {
  id: string;
  workspace_id: string;
  agent_id: string;
  parent_task_id: string | null;
  conversation_id: string | null;
  trigger: string;
  title: string;
  status: string;
  workflow_id: string;
  temporal_run_id: string;
  stopped_reason: string;
  summary: string;
  step_count: number;
  input_tokens: number;
  output_tokens: number;
  cost_micros: number;
  started_at: Date | null;
  finished_at: Date | null;
  created_at: Date;
  updated_at: Date;
  depth: number;
  heartbeat_at: Date | null;
  waiting_reason: string;
  waiting_draft_id: string | null;
  reply_message_id: string | null;
}

/** A row of `task_steps`. */
interface StepRow {
  id: string;
  workspace_id: string;
  task_id: string;
  seq: number;
  kind: string;
  tool_name: string;
  tool_label: string;
  input: unknown;
  output: unknown;
  input_tokens: number;
  output_tokens: number;
  pruned_at: Date | null;
  created_at: Date;
}

/** The Postgres-backed implementation of the task ports. */
export class PostgresRepository implements TaskRepository, StepRepository, StepPruner {
  constructor(private readonly db: Database) {}

  /** CreateTask: stores a queued task. */
  async create(_ctx: AbortSignal | undefined, scope: Scope, task: NewTask): Promise<Task> {
    const workspaceId = requireWorkspace(scope);
    return this.write(scope, async (tx) => {
      const rows = await tx<TaskRow[]>`
        INSERT INTO tasks (
            workspace_id, agent_id, parent_task_id, conversation_id, reply_message_id,
            trigger, title, status, depth
        )
        SELECT
            ${workspaceId},
            a.id,
            ${optional(task.parentTaskId)}::uuid,
            ${optional(task.conversationId)}::uuid,
            ${optional(task.replyMessageId)}::uuid,
            ${task.trigger},
            ${task.title},
            'queued',
            ${task.depth}
        FROM agents a
        WHERE a.id = ${task.agentId}
          AND a.workspace_id = ${workspaceId}
          AND a.deleted_at IS NULL
          AND (
              ${optional(task.conversationId)}::uuid IS NULL
              OR EXISTS (
                  SELECT 1 FROM conversations c
                  WHERE c.id = ${optional(task.conversationId)}::uuid
                    AND c.workspace_id = ${workspaceId}
              )
          )
          AND (
              ${optional(task.replyMessageId)}::uuid IS NULL
              OR EXISTS (
                  SELECT 1 FROM messages m
                  WHERE m.id = ${optional(task.replyMessageId)}::uuid
                    AND m.workspace_id = ${workspaceId}
              )
          )
          AND (
              ${optional(task.parentTaskId)}::uuid IS NULL
              OR EXISTS (
                  SELECT 1 FROM tasks p
                  WHERE p.id = ${optional(task.parentTaskId)}::uuid
                    AND p.workspace_id = ${workspaceId}
              )
          )
        RETURNING *
      `;

      const row = rows[0];
      if (!row) {
        throw wrap(
          ErrInvalidInput,
          "the Bolu, the conversation, the message, or the parent task is not in this workspace",
        );
      }
      return toTask(row);
    }).catch((err: unknown) => {
      throw translateWriteError(err, "create task");
    });
  }

  /** GetTask: returns one task. */
  async get(_ctx: AbortSignal | undefined, scope: Scope, id: string): Promise<Task> {
    const workspaceId = requireWorkspace(scope);
    return this.read(scope, async (tx) => {
      const rows = await tx<TaskRow[]>`
        SELECT * FROM tasks WHERE id = ${id} AND workspace_id = ${workspaceId}
      `;
      const row = rows[0];
      if (!row) throw ErrTaskNotFound;
      return toTask(row);
    });
  }

  /** ListTasks: the history, newest first. */
  async list(_ctx: AbortSignal | undefined, scope: Scope, filter: Filter): Promise<Task[]> {
    const workspaceId = requireWorkspace(scope);
    const limit =
      filter.limit <= 0 ? DefaultHistoryLimit : filter.limit > MaxHistoryLimit ? MaxHistoryLimit : filter.limit;

    return this.read(scope, async (tx) => {
      const rows = await tx<TaskRow[]>`
        SELECT * FROM tasks
        WHERE workspace_id = ${workspaceId}
          AND (${optional(filter.agentId)}::uuid IS NULL OR agent_id = ${optional(filter.agentId)}::uuid)
          AND (
              ${optional(filter.parentTaskId)}::uuid IS NULL
              OR parent_task_id = ${optional(filter.parentTaskId)}::uuid
          )
          AND (${optional(filter.status)}::text IS NULL OR status = ${optional(filter.status)}::text)
          AND (
              ${filter.live}::boolean = false
              OR status IN ('queued', 'running', 'waiting_approval')
          )
        ORDER BY created_at DESC, id DESC
        LIMIT ${limit}
      `;
      return rows.map(toTask);
    });
  }

  /** ListChildTasks: the tasks this one handed work to. */
  async children(_ctx: AbortSignal | undefined, scope: Scope, taskId: string): Promise<Task[]> {
    const workspaceId = requireWorkspace(scope);
    return this.read(scope, async (tx) => {
      const rows = await tx<TaskRow[]>`
        SELECT * FROM tasks
        WHERE workspace_id = ${workspaceId} AND parent_task_id = ${taskId}
        ORDER BY created_at
      `;
      return rows.map(toTask);
    });
  }

  /** StartTask: records the workflow that owns the task and marks it running. */
  async start(
    _ctx: AbortSignal | undefined,
    scope: Scope,
    id: string,
    workflowId: string,
    runId: string,
  ): Promise<Task> {
    const workspaceId = requireWorkspace(scope);
    return this.write(scope, async (tx) => {
      const rows = await tx<TaskRow[]>`
        UPDATE tasks
        SET status = 'running',
            workflow_id = ${workflowId},
            temporal_run_id = ${runId},
            started_at = COALESCE(started_at, now()),
            heartbeat_at = now(),
            updated_at = now()
        WHERE id = ${id} AND workspace_id = ${workspaceId}
          AND status IN ('queued', 'running')
        RETURNING *
      `;
      const row = rows[0];
      if (!row) throw ErrTaskNotFound;
      return toTask(row);
    });
  }

  /** ProgressTask: records a heartbeat with the running totals. */
  async progress(_ctx: AbortSignal | undefined, scope: Scope, progress: Progress): Promise<void> {
    const workspaceId = requireWorkspace(scope);
    await this.write(scope, async (tx) => {
      const rows = await tx<TaskRow[]>`
        UPDATE tasks
        SET heartbeat_at = now(),
            input_tokens = ${progress.inputTokens},
            output_tokens = ${progress.outputTokens},
            cost_micros = ${progress.costMicros},
            step_count = ${progress.stepCount},
            updated_at = now()
        WHERE id = ${progress.taskId} AND workspace_id = ${workspaceId}
          AND status IN ('queued', 'running')
        RETURNING *
      `;
      if (!rows[0]) {
        // The task finished while this heartbeat was in flight, which is not a
        // failure: there is nothing left to report on.
        throw ErrTaskNotFound;
      }
    });
  }

  /** ParkTask: records that the task is waiting for a human decision. */
  async park(
    _ctx: AbortSignal | undefined,
    scope: Scope,
    id: string,
    reason: string,
    draftId: string,
  ): Promise<Task> {
    const workspaceId = requireWorkspace(scope);
    return this.write(scope, async (tx) => {
      const rows = await tx<TaskRow[]>`
        UPDATE tasks
        SET status = 'waiting_approval',
            waiting_reason = ${reason},
            waiting_draft_id = ${optional(draftId)}::uuid,
            heartbeat_at = now(),
            updated_at = now()
        WHERE id = ${id} AND workspace_id = ${workspaceId}
          AND status IN ('queued', 'running')
        RETURNING *
      `;
      const row = rows[0];
      if (!row) throw ErrTaskNotFound;
      return toTask(row);
    });
  }

  /** ResumeTask: clears the waiting state when the decision arrives. */
  async resume(_ctx: AbortSignal | undefined, scope: Scope, id: string): Promise<Task> {
    const workspaceId = requireWorkspace(scope);
    return this.write(scope, async (tx) => {
      const rows = await tx<TaskRow[]>`
        UPDATE tasks
        SET status = 'running',
            waiting_reason = '',
            waiting_draft_id = NULL,
            heartbeat_at = now(),
            updated_at = now()
        WHERE id = ${id} AND workspace_id = ${workspaceId}
          AND status = 'waiting_approval'
        RETURNING *
      `;
      const row = rows[0];
      if (!row) throw ErrTaskNotFound;
      return toTask(row);
    });
  }

  /** FinishTask: records the terminal state. */
  async finish(_ctx: AbortSignal | undefined, scope: Scope, finish: Finish): Promise<Task> {
    const workspaceId = requireWorkspace(scope);
    return this.write(scope, async (tx) => {
      const rows = await tx<TaskRow[]>`
        UPDATE tasks
        SET status = ${finish.status},
            summary = ${finish.summary},
            stopped_reason = ${finish.stoppedReason},
            input_tokens = ${finish.inputTokens},
            output_tokens = ${finish.outputTokens},
            cost_micros = ${finish.costMicros},
            step_count = ${finish.stepCount},
            waiting_reason = '',
            waiting_draft_id = NULL,
            finished_at = now(),
            heartbeat_at = now(),
            updated_at = now()
        WHERE id = ${finish.taskId} AND workspace_id = ${workspaceId}
          AND status IN ('queued', 'running', 'waiting_approval')
        RETURNING *
      `;
      const row = rows[0];
      if (!row) {
        // A task that already finished stays finished. Reporting not found is
        // honest: there was nothing to write.
        throw ErrTaskNotFound;
      }
      return toTask(row);
    });
  }

  /** CancelTask: stops a task that has not finished. */
  async cancel(
    _ctx: AbortSignal | undefined,
    scope: Scope,
    id: string,
    reason: string,
  ): Promise<Task> {
    const workspaceId = requireWorkspace(scope);
    return this.write(scope, async (tx) => {
      const rows = await tx<TaskRow[]>`
        UPDATE tasks
        SET status = 'canceled',
            stopped_reason = ${reason},
            finished_at = now(),
            updated_at = now()
        WHERE id = ${id} AND workspace_id = ${workspaceId}
          AND status IN ('queued', 'running', 'waiting_approval')
        RETURNING *
      `;
      const row = rows[0];
      if (!row) throw ErrTaskNotFound;
      return toTask(row);
    });
  }

  /** CountLiveTasksForAgent: how many tasks a Bolu has in flight. */
  async countLiveTasksForAgent(
    _ctx: AbortSignal | undefined,
    scope: Scope,
    agentId: string,
  ): Promise<number> {
    const workspaceId = requireWorkspace(scope);
    return this.read(scope, async (tx) => {
      const rows = await tx<{ count: number }[]>`
        SELECT count(*)::bigint AS count FROM tasks
        WHERE workspace_id = ${workspaceId} AND agent_id = ${agentId}
          AND status IN ('queued', 'running', 'waiting_approval')
      `;
      return Number(rows[0]?.count ?? 0);
    });
  }

  /** AppendTaskStep: writes one step, assigning its sequence number in the statement. */
  async append(_ctx: AbortSignal | undefined, scope: Scope, step: Step): Promise<Step> {
    const workspaceId = requireWorkspace(scope);
    return this.write(scope, async (tx) => {
      const rows = await tx<StepRow[]>`
        INSERT INTO task_steps (
            workspace_id, task_id, seq, kind, tool_name, tool_label,
            input, output, input_tokens, output_tokens
        )
        VALUES (
            ${workspaceId}, ${step.taskId},
            COALESCE((SELECT max(seq) FROM task_steps WHERE task_id = ${step.taskId}), 0) + 1,
            ${step.kind}, ${step.toolName}, ${step.toolLabel},
            ${tx.json((step.input ?? {}) as any)},
            ${tx.json((step.output ?? {}) as any)},
            ${step.inputTokens}, ${step.outputTokens}
        )
        RETURNING *
      `;
      const row = rows[0];
      if (!row) throw wrap(ErrInvalidInput, "referenced row is not in this workspace");
      return toStep(row);
    }).catch((err: unknown) => {
      throw translateWriteError(err, "append step");
    });
  }

  /** ListTaskSteps: a task's steps in order. */
  async listSteps(_ctx: AbortSignal | undefined, scope: Scope, taskId: string): Promise<Step[]> {
    const workspaceId = requireWorkspace(scope);
    return this.read(scope, async (tx) => {
      const rows = await tx<StepRow[]>`
        SELECT * FROM task_steps
        WHERE workspace_id = ${workspaceId} AND task_id = ${taskId}
        ORDER BY seq
      `;
      return rows.map(toStep);
    });
  }

  /** PruneTaskSteps + CountUnprunedSteps: empties step payloads past their window. */
  async pruneContents(
    _ctx: AbortSignal | undefined,
    scope: Scope,
    olderThan: Date,
    limit: number,
  ): Promise<StepPrune> {
    const workspaceId = requireWorkspace(scope);
    const bounded = limit <= 0 ? DefaultPruneLimit : limit;

    return this.write(scope, async (tx) => {
      const pruned = await tx<{ id: string }[]>`
        UPDATE task_steps
        SET input = '{}'::jsonb,
            output = '{}'::jsonb,
            pruned_at = now()
        WHERE task_steps.workspace_id = ${workspaceId}
          AND task_steps.id IN (
            SELECT candidate.id FROM task_steps AS candidate
            WHERE candidate.workspace_id = ${workspaceId}
              AND candidate.pruned_at IS NULL
              AND candidate.created_at < ${olderThan}
            ORDER BY candidate.created_at
            LIMIT ${bounded}
          )
        RETURNING id
      `;

      const remaining = await tx<{ count: number }[]>`
        SELECT count(*) AS count FROM task_steps
        WHERE workspace_id = ${workspaceId}
          AND pruned_at IS NULL
          AND created_at < ${olderThan}
      `;

      return { pruned: pruned.length, remaining: Number(remaining[0]?.count ?? 0) };
    });
  }

  private read<T>(scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    return this.db.inScopeRead(databaseScope(scope), fn);
  }

  private write<T>(scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    return this.db.inScope(databaseScope(scope), fn);
  }
}

/** The platform scope the repository hands to the pool. */
function databaseScope(scope: Scope): DatabaseScope {
  return { userId: scope.userId ?? null, workspaceId: scope.workspaceId ?? null };
}

/** The workspace a statement runs in; an unscoped read is a bug, not a query. */
function requireWorkspace(scope: Scope): string {
  if (!scope.workspaceId) {
    throw wrap(ErrInvalidInput, "workspace is required");
  }
  return scope.workspaceId;
}

/** An id or NULL, the way sqlc's `narg` renders an absent uuid. */
function optional(id: string | null | undefined): string | null {
  if (!id || id === "00000000-0000-0000-0000-000000000000") return null;
  return id;
}

/** Reads an absent uuid back as the empty string. */
function deref(id: string | null): string {
  return id ?? "";
}

function toTask(row: TaskRow): Task {
  return {
    id: row.id,
    workspaceId: row.workspace_id,
    agentId: row.agent_id,
    parentTaskId: deref(row.parent_task_id),
    conversationId: deref(row.conversation_id),
    trigger: row.trigger,
    title: row.title,
    status: row.status,
    replyMessageId: deref(row.reply_message_id),
    workflowId: row.workflow_id,
    temporalRunId: row.temporal_run_id,
    depth: row.depth,
    summary: row.summary,
    stoppedReason: row.stopped_reason,
    waitingReason: row.waiting_reason,
    waitingDraftId: deref(row.waiting_draft_id),
    stepCount: row.step_count,
    inputTokens: Number(row.input_tokens),
    outputTokens: Number(row.output_tokens),
    costMicros: Number(row.cost_micros),
    heartbeatAt: row.heartbeat_at,
    startedAt: row.started_at,
    finishedAt: row.finished_at,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

function toStep(row: StepRow): Step {
  return {
    id: row.id,
    taskId: row.task_id,
    seq: row.seq,
    kind: row.kind,
    toolName: row.tool_name,
    toolLabel: row.tool_label,
    input: row.input,
    output: row.output,
    inputTokens: row.input_tokens,
    outputTokens: row.output_tokens,
    pruned: row.pruned_at !== null,
    createdAt: row.created_at,
  };
}

/**
 * Turns the constraint violations the runtime can actually hit into the errors
 * the service reports, and leaves the rest wrapped.
 */
function translateWriteError(err: unknown, action: string): unknown {
  if (err && typeof err === "object" && "code" in err) {
    const code = err.code;
    switch (code) {
      case "23503":
        // A foreign key failure means the Bolu, the conversation, or the parent
        // task is not in this workspace, which RLS also refuses.
        return wrap(ErrInvalidInput, "referenced row is not in this workspace");
      case "23514":
        // The status, trigger, kind, or depth check constraint.
        return wrap(ErrInvalidInput, "value is outside the allowed set");
    }
  }
  return err instanceof Error
    ? new Error(`task: ${action}: ${err.message}`, { cause: err })
    : err;
}
