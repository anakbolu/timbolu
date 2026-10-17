/**
 * The checklist module's data access.
 *
 * Every method runs inside the caller's tenant scope, so Row Level Security
 * filters the statements even where one forgets its workspace filter. Two
 * things are the database's rather than this file's: the
 * `checklists_agent_active_key` index refuses a second open plan for one Bolu,
 * and the trigger on `checklist_items` refuses a task ticked while its subtasks
 * are open. This file translates those refusals into the module's own errors.
 *
 * Ported 1:1 from internal/modules/checklist/repository/postgres.go, with
 * repository/sqlcgen/*.go folded in: each query keeps its name as a comment and
 * its SQL text unchanged.
 */

import type { Database, Tx } from "../../../platform/database/index.ts";
import {
  ErrActiveExists,
  ErrInvalidInput,
  ErrNotFound,
  ErrSubtasksOpen,
  isZeroScope,
  normaliseFilter,
  wrap,
  type Checklist,
  type Filter,
  type Item,
  type NewChecklist,
  type Scope,
} from "../domain/checklist.ts";

/** One row of `checklists`. */
interface ChecklistRow {
  id: string;
  workspace_id: string;
  agent_id: string;
  title: string;
  status: string;
  task_id: string | null;
  created_at: Date;
  updated_at: Date;
  completed_at: Date | null;
}

/** One row of `checklist_items`. */
interface ChecklistItemRow {
  id: string;
  checklist_id: string;
  workspace_id: string;
  parent_id: string | null;
  title: string;
  done: boolean;
  position: number;
  created_at: Date;
  updated_at: Date;
  done_at: Date | null;
}

export class PostgresRepository {
  constructor(private readonly db: Database) {}

  // ---------------------------------------------------------------- the list

  /** The Bolu's open plan. */
  async active(scope: Scope, agentId: string): Promise<Checklist> {
    return this.read(scope, async (tx) => {
      // name: GetActiveChecklist
      const rows = await tx<ChecklistRow[]>`
        SELECT id, workspace_id, agent_id, title, status, task_id, created_at, updated_at, completed_at
        FROM checklists
        WHERE workspace_id = ${scope.workspaceId} AND agent_id = ${agentId} AND status = 'active'
      `;
      if (rows.length === 0) {
        throw wrap(ErrNotFound, `Bolu ${agentId} has no plan in progress`);
      }

      return this.load(scope, rows[0], tx);
    });
  }

  /**
   * Writes a plan and its items in one transaction.
   *
   * The items are inserted in the order they were written and carry that order
   * as their position, so a plan read back is the plan that was meant.
   */
  async create(scope: Scope, input: NewChecklist): Promise<Checklist> {
    return this.write(scope, async (tx) => {
      // name: CreateChecklist
      const headers = await this.translateWrite("create checklist", () =>
        tx<ChecklistRow[]>`
          INSERT INTO checklists (workspace_id, agent_id, title, task_id)
          VALUES (${scope.workspaceId}, ${input.agentId}, ${input.title}, ${optionalId(input.taskId)})
          RETURNING id, workspace_id, agent_id, title, status, task_id, created_at, updated_at, completed_at
        `,
      );
      const header = headers[0];

      for (const [index, task] of input.tasks.entries()) {
        // name: CreateChecklistItem
        const parents = await this.translateWrite("create checklist item", () =>
          tx<ChecklistItemRow[]>`
            INSERT INTO checklist_items (checklist_id, workspace_id, parent_id, title, position)
            VALUES (${header.id}, ${scope.workspaceId}, ${null}, ${task.title}, ${index})
            RETURNING id, checklist_id, workspace_id, parent_id, title, done, position,
                      created_at, updated_at, done_at
          `,
        );
        const parent = parents[0];

        for (const [subIndex, subtask] of task.subtasks.entries()) {
          await this.translateWrite("create checklist subtask", () =>
            tx<ChecklistItemRow[]>`
              INSERT INTO checklist_items (checklist_id, workspace_id, parent_id, title, position)
              VALUES (${header.id}, ${scope.workspaceId}, ${parent.id}, ${subtask}, ${subIndex})
              RETURNING id, checklist_id, workspace_id, parent_id, title, done, position,
                        created_at, updated_at, done_at
            `,
          );
        }
      }

      return this.load(scope, header, tx);
    });
  }

  /**
   * Ticks one item and settles the plan if that was the last thing open.
   *
   * The tick, the count, and the settle are one transaction.
   */
  async markDone(scope: Scope, checklistId: string, itemId: string): Promise<Checklist> {
    return this.change(scope, checklistId, async (tx) => {
      // name: MarkChecklistItemDone
      const rows = await this.translateWrite("mark item done", () =>
        tx<ChecklistItemRow[]>`
          UPDATE checklist_items
          SET done = true, done_at = now(), updated_at = now()
          WHERE id = ${itemId} AND checklist_id = ${checklistId} AND workspace_id = ${scope.workspaceId}
          RETURNING id, checklist_id, workspace_id, parent_id, title, done, position,
                    created_at, updated_at, done_at
        `,
      );
      if (rows.length === 0) {
        throw wrap(ErrNotFound, `item ${itemId}`);
      }
    });
  }

  /**
   * Deletes one item, and with it the subtasks underneath it, and settles the
   * plan the same way a tick does.
   */
  async removeItem(scope: Scope, checklistId: string, itemId: string): Promise<Checklist> {
    return this.change(scope, checklistId, async (tx) => {
      // name: DeleteChecklistItem
      const rows = await this.translateWrite("delete checklist item", () =>
        tx<{ id: string }[]>`
          DELETE FROM checklist_items
          WHERE id = ${itemId} AND checklist_id = ${checklistId} AND workspace_id = ${scope.workspaceId}
          RETURNING id
        `,
      );
      if (rows.length === 0) {
        throw wrap(ErrNotFound, `item ${itemId}`);
      }
    });
  }

  /**
   * Runs one write, settles the plan if nothing is left open, and answers with
   * the plan as it now is.
   */
  private async change(
    scope: Scope,
    checklistId: string,
    apply: (tx: Tx) => Promise<void>,
  ): Promise<Checklist> {
    return this.write(scope, async (tx) => {
      await apply(tx);

      // name: CountOpenTasks
      const counts = await tx<{ count: number }[]>`
        SELECT count(*) FROM checklist_items
        WHERE checklist_id = ${checklistId} AND workspace_id = ${scope.workspaceId}
          AND parent_id IS NULL AND NOT done
      `;
      const open = Number(counts[0]?.count ?? 0);

      // Nothing left open settles the plan; anything else reads it as it
      // stands.
      let row: ChecklistRow;
      if (open === 0) {
        // name: CompleteChecklist
        const rows = await this.translateWrite("complete checklist", () =>
          tx<ChecklistRow[]>`
            UPDATE checklists
            SET status = 'done', completed_at = now(), updated_at = now()
            WHERE id = ${checklistId} AND workspace_id = ${scope.workspaceId}
            RETURNING id, workspace_id, agent_id, title, status, task_id, created_at, updated_at, completed_at
          `,
        );
        if (rows.length === 0) {
          throw wrap(ErrNotFound, `checklist ${checklistId}`);
        }
        row = rows[0];
      } else {
        // name: GetChecklist
        const rows = await tx<ChecklistRow[]>`
          SELECT id, workspace_id, agent_id, title, status, task_id, created_at, updated_at, completed_at
          FROM checklists
          WHERE id = ${checklistId} AND workspace_id = ${scope.workspaceId}
        `;
        if (rows.length === 0) {
          throw wrap(ErrNotFound, `checklist ${checklistId}`);
        }
        row = rows[0];
      }

      return this.load(scope, row, tx);
    });
  }

  /** Deletes a plan and everything in it. */
  async clear(scope: Scope, checklistId: string): Promise<void> {
    await this.write(scope, async (tx) => {
      // name: DeleteChecklist
      const rows = await this.translateWrite("delete checklist", () =>
        tx<{ id: string }[]>`
          DELETE FROM checklists WHERE id = ${checklistId} AND workspace_id = ${scope.workspaceId}
          RETURNING id
        `,
      );
      if (rows.length === 0) {
        throw wrap(ErrNotFound, `checklist ${checklistId}`);
      }
    });
  }

  /** Plans, newest first, each with its items. */
  async list(scope: Scope, filter: Filter): Promise<Checklist[]> {
    const bounded = normaliseFilter(filter);

    return this.read(scope, async (tx) => {
      // name: ListChecklists
      const rows = await tx<ChecklistRow[]>`
        SELECT id, workspace_id, agent_id, title, status, task_id, created_at, updated_at, completed_at
        FROM checklists
        WHERE workspace_id = ${scope.workspaceId}
          AND (${optionalId(bounded.agentId)}::uuid IS NULL OR agent_id = ${optionalId(bounded.agentId)}::uuid)
          AND (${bounded.status}::text = '' OR status = ${bounded.status}::text)
        ORDER BY created_at DESC
        LIMIT ${bounded.limit}
      `;

      if (rows.length === 0) {
        return [];
      }

      // name: ListItemsForChecklists
      const ids = rows.map((row) => row.id);
      const items = await tx<ChecklistItemRow[]>`
        SELECT id, checklist_id, workspace_id, parent_id, title, done, position, created_at, updated_at, done_at
        FROM checklist_items
        WHERE workspace_id = ${scope.workspaceId} AND checklist_id = ANY(${ids}::uuid[])
        ORDER BY position, created_at
      `;

      const grouped = new Map<string, ChecklistItemRow[]>();
      for (const item of items) {
        const list = grouped.get(item.checklist_id) ?? [];
        list.push(item);
        grouped.set(item.checklist_id, list);
      }

      return rows.map((row) => toChecklist(row, grouped.get(row.id) ?? []));
    });
  }

  // ---------------------------------------------------------------- loading

  /** Reads one plan and its items, assembling the tree the domain talks about. */
  private async load(scope: Scope, row: ChecklistRow, tx: Tx): Promise<Checklist> {
    // name: ListChecklistItems
    const items = await tx<ChecklistItemRow[]>`
      SELECT id, checklist_id, workspace_id, parent_id, title, done, position, created_at, updated_at, done_at
      FROM checklist_items
      WHERE checklist_id = ${row.id} AND workspace_id = ${scope.workspaceId}
      ORDER BY position, created_at
    `;

    return toChecklist(row, items);
  }

  // ---------------------------------------------------------------- helpers

  private read<T>(scope: Scope, fn: (tx: Tx) => PromiseLike<T>): Promise<T> {
    if (isZeroScope(scope)) {
      return Promise.reject(wrap(ErrInvalidInput, "workspace is required"));
    }
    return this.db.inScopeRead({ userId: scope.userId ?? null, workspaceId: scope.workspaceId }, async (tx) => fn(tx));
  }

  private write<T>(scope: Scope, fn: (tx: Tx) => PromiseLike<T>): Promise<T> {
    if (isZeroScope(scope)) {
      return Promise.reject(wrap(ErrInvalidInput, "workspace is required"));
    }
    return this.db.inScope({ userId: scope.userId ?? null, workspaceId: scope.workspaceId }, async (tx) => fn(tx));
  }

  /**
   * Turns a driver error into this module's own.
   *
   * The two constraint names are the module's two rules, and they are named
   * rather than inferred.
   */
  private async translateWrite<T>(operation: string, run: () => PromiseLike<T>): Promise<T> {
    try {
      return await run();
    } catch (err) {
      const failure = postgresFailure(err);
      if (failure) {
        if (failure.code === "23505") {
          if (failure.constraint === "checklists_agent_active_key") {
            throw wrap(ErrActiveExists, failure.detail);
          }
          throw wrap(ErrInvalidInput, failure.detail);
        }
        if (failure.code === "23514") {
          if (failure.message === "checklist: a task with unfinished subtasks cannot be marked done") {
            throw wrap(ErrSubtasksOpen, failure.message);
          }
          throw wrap(ErrInvalidInput, failure.message);
        }
        if (failure.code === "23503" || failure.code === "22P02" || failure.code === "22003") {
          throw wrap(ErrInvalidInput, failure.message);
        }
      }
      throw new Error(`checklist: ${operation}: ${err instanceof Error ? err.message : String(err)}`);
    }
  }
}

/** Maps a row and its items onto the domain type, nesting subtasks under their task. */
function toChecklist(row: ChecklistRow, items: ChecklistItemRow[]): Checklist {
  const plan: Checklist = {
    id: row.id,
    agentId: row.agent_id,
    taskId: row.task_id ?? NilId,
    title: row.title,
    status: row.status,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
    completedAt: row.completed_at,
    items: [],
  };

  // Two passes: the tasks first, then the subtasks attached to them.
  const index = new Map<string, number>();
  for (const item of items) {
    if (item.parent_id !== null) {
      continue;
    }
    index.set(item.id, plan.items.length);
    plan.items.push(toItem(item));
  }

  for (const item of items) {
    if (item.parent_id === null) {
      continue;
    }
    const position = index.get(item.parent_id);
    if (position === undefined) {
      // An item whose parent is not in this slice is kept as a task rather than
      // dropped.
      plan.items.push(toItem(item));
      continue;
    }
    plan.items[position].subtasks.push(toItem(item));
  }

  return plan;
}

/** Maps one row. */
function toItem(row: ChecklistItemRow): Item {
  return {
    id: row.id,
    checklistId: row.checklist_id,
    parentId: row.parent_id ?? NilId,
    title: row.title,
    done: row.done,
    position: Number(row.position),
    subtasks: [],
    createdAt: row.created_at,
    updatedAt: row.updated_at,
    doneAt: row.done_at,
  };
}

/** Renders a uuid that may be absent as NULL rather than as the zero uuid. */
function optionalId(id: string): string | null {
  return !id || id === NilId ? null : id;
}

/** The shape of a driver error this module translates. */
function postgresFailure(err: unknown): { code: string; constraint: string; message: string; detail: string } | null {
  if (!err || typeof err !== "object" || !("code" in err) || typeof err.code !== "string") {
    return null;
  }
  return {
    code: err.code,
    constraint: "constraint_name" in err && typeof err.constraint_name === "string" ? err.constraint_name : "",
    message: "message" in err && typeof err.message === "string" ? err.message : "",
    detail: "detail" in err && typeof err.detail === "string" ? err.detail : "",
  };
}

const NilId = "00000000-0000-0000-0000-000000000000";
