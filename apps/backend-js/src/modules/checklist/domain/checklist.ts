/**
 * The checklist module's contract: the plan a Bolu writes for itself, the tasks
 * and subtasks it is made of, and the ports the service needs to keep it.
 *
 * Three rules are the whole reason it is more than a table definition:
 *
 *   - **One plan at a time.** A Bolu may hold exactly one active checklist.
 *   - **Completion is derived.** A list is finished when every top-level task is
 *     ticked, and a task with an unfinished subtask cannot be ticked at all.
 *   - **A subtask is one level deep.**
 *
 * Ported 1:1 from internal/modules/checklist/domain/checklist.go.
 */

/** The plan the Bolu is working on. There is at most one per Bolu. */
export const StatusActive = "active";
/** A plan whose every task is ticked. */
export const StatusDone = "done";

/** A request that cannot be honoured. */
export const ErrInvalidInput = new Error("checklist: invalid input");
/** A checklist, or an item, that is not in this workspace. */
export const ErrNotFound = new Error("checklist: not found");
/** A create while the Bolu still has a plan to finish. */
export const ErrActiveExists = new Error("checklist: the Bolu still has a plan to finish");
/** A tick on a task whose subtasks are not all done. */
export const ErrSubtasksOpen = new Error("checklist: a task with unfinished subtasks cannot be marked done");

/** How many top-level tasks one plan may hold. */
export const MaxTasks = 20;
/** How many subtasks one task may hold. */
export const MaxSubtasks = 10;
/** Bounds one title. */
export const MaxTitleRunes = 200;
/** Bounds a list read when the caller does not say how many. */
export const DefaultLimit = 50;
/** Bounds one page. */
export const MaxLimit = 200;

/** The zero uuid, as Go's `uuid.Nil`. */
export const NilUUID = "00000000-0000-0000-0000-000000000000";

/** The tenant a call runs as. */
export interface Scope {
  userId?: string;
  workspaceId: string;
}

/** One line of a plan: a task, or a subtask of one. */
export interface Item {
  id: string;
  checklistId: string;
  /** The task this item is a subtask of, or Nil when it is a task. */
  parentId: string;
  title: string;
  done: boolean;
  /** The order the Bolu wrote the items in. */
  position: number;
  /** Filled on a read of the tree, empty when the item is read on its own. */
  subtasks: Item[];
  createdAt: Date;
  updatedAt: Date;
  doneAt: Date | null;
}

/** One plan a Bolu wrote for itself. */
export interface Checklist {
  id: string;
  agentId: string;
  taskId: string;
  title: string;
  status: string;
  /** The top-level tasks, in the order they were written, each with its subtasks. */
  items: Item[];
  createdAt: Date;
  updatedAt: Date;
  /** When the last task was ticked. Null while the plan is open. */
  completedAt: Date | null;
}

/** One task to write down, with the subtasks it starts with. */
export interface NewItem {
  title: string;
  subtasks: string[];
}

/** A plan to write. */
export interface NewChecklist {
  agentId: string;
  /** The task the Bolu was working on when it wrote the plan down. */
  taskId: string;
  title: string;
  tasks: NewItem[];
}

/** Narrows a list read. */
export interface Filter {
  agentId: string;
  status: string;
  limit: number;
}

/** Returns the filter with its bounds applied. */
export function normaliseFilter(filter: Filter): Filter {
  let limit = filter.limit;
  if (limit <= 0) {
    limit = DefaultLimit;
  } else if (limit > MaxLimit) {
    limit = MaxLimit;
  }
  return { ...filter, limit };
}

/** Reports whether the scope names no workspace. */
export function isZeroScope(scope: Scope): boolean {
  return !scope.workspaceId || scope.workspaceId === NilUUID;
}

/** Reports whether this item belongs to a task rather than being one. */
export function isSubtask(item: Item): boolean {
  return item.parentId !== NilUUID && item.parentId !== "";
}

/** The subtasks that are not ticked yet. */
export function openSubtasks(item: Item): Item[] {
  return item.subtasks.filter((subtask) => !subtask.done);
}

/** Reports whether this is no plan at all. */
export function isZeroChecklist(plan: Checklist): boolean {
  return plan.id === NilUUID || plan.id === "";
}

/** Reports whether this plan is the one the Bolu is working on. */
export function isActive(plan: Checklist): boolean {
  return plan.status === StatusActive;
}

/** How much of a plan is done, counted over tasks and subtasks together. */
export function counts(plan: Checklist): { done: number; total: number } {
  let done = 0;
  let total = 0;
  for (const item of plan.items) {
    total++;
    if (item.done) {
      done++;
    }
    for (const subtask of item.subtasks) {
      total++;
      if (subtask.done) {
        done++;
      }
    }
  }
  return { done, total };
}

/** Finds the item with this id, task or subtask. */
export function findItem(plan: Checklist, id: string): Item | null {
  for (const item of plan.items) {
    if (item.id === id) {
      return item;
    }
    for (const subtask of item.subtasks) {
      if (subtask.id === id) {
        return subtask;
      }
    }
  }
  return null;
}

/** The persistence contract. */
export interface Repository {
  /** The Bolu's open plan, or ErrNotFound when it has none. */
  active(scope: Scope, agentId: string): Promise<Checklist>;
  /** Writes a plan and its items in one transaction. */
  create(scope: Scope, plan: NewChecklist): Promise<Checklist>;
  /** Ticks one item and answers with the plan as it now is. */
  markDone(scope: Scope, checklistId: string, itemId: string): Promise<Checklist>;
  /** Deletes one item, subtasks and all. */
  removeItem(scope: Scope, checklistId: string, itemId: string): Promise<Checklist>;
  /** Deletes a plan and everything in it. */
  clear(scope: Scope, checklistId: string): Promise<void>;
  /** Plans, newest first, filtered by the caller. */
  list(scope: Scope, filter: Filter): Promise<Checklist[]>;
}

/** What the tool adapter and the HTTP handler call. */
export interface Service {
  read(ctx: unknown, scope: Scope, agentId: string): Promise<Checklist>;
  create(ctx: unknown, scope: Scope, plan: NewChecklist): Promise<Checklist>;
  check(ctx: unknown, scope: Scope, agentId: string, itemId: string): Promise<Checklist>;
  remove(ctx: unknown, scope: Scope, agentId: string, itemId: string): Promise<Checklist>;
  clear(ctx: unknown, scope: Scope, agentId: string): Promise<void>;
  list(ctx: unknown, scope: Scope, filter: Filter): Promise<Checklist[]>;
}

/**
 * An error carrying the sentinel a caller switches on, so `is` reproduces Go's
 * `errors.Is` for the messages `fmt.Errorf("%w: ...")` produced.
 */
export class WrappedError extends Error {
  constructor(
    readonly sentinel: Error,
    message: string,
  ) {
    super(message);
    this.name = "WrappedError";
  }
}

/** Wraps a sentinel with a detail, rendering the message as Go's `%w: detail`. */
export function wrap(sentinel: Error, detail: string): WrappedError {
  return new WrappedError(sentinel, `${sentinel.message}: ${detail}`);
}

/** True when `err` is the sentinel or wraps it. */
export function is(err: unknown, sentinel: Error): boolean {
  let current: unknown = err;
  for (let guard = 0; guard < 32; guard++) {
    if (current === sentinel) {
      return true;
    }
    if (!(current instanceof Error)) {
      return false;
    }
    if (current instanceof WrappedError) {
      current = current.sentinel;
      continue;
    }
    current = "cause" in current ? current.cause : undefined;
  }
  return false;
}

/** The message of an unknown thrown value. */
export function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
