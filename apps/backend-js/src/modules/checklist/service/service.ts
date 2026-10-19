/**
 * The checklist module's use cases: the plan a Bolu writes for itself, the
 * ticks it makes, and the two ways it throws one away.
 *
 * The refusals are written for the reader they have, which is a model in the
 * middle of a job. Each one says what was asked for and what is in the way,
 * because a tool result is the whole of what the model gets.
 *
 * Ported 1:1 from internal/modules/checklist/service/service.go.
 */

import { nopLogger, type Logger } from "../../../platform/logger.ts";
import {
  ErrActiveExists,
  ErrInvalidInput,
  ErrNotFound,
  ErrSubtasksOpen,
  MaxSubtasks,
  MaxTasks,
  MaxTitleRunes,
  StatusActive,
  StatusDone,
  counts,
  findItem,
  is,
  isZeroScope,
  openSubtasks,
  wrap,
  type Checklist,
  type Filter,
  type Item,
  type NewChecklist,
  type NewItem,
  type Repository,
  type Scope,
  type Service,
} from "../domain/checklist.ts";

/** The service dependencies. */
export interface Deps {
  repository: Repository;
  logger?: Logger | null;
}

/** The checklist module's implementation. */
export class ChecklistService implements Service {
  private readonly repository: Repository;
  private readonly log: Logger;

  constructor(deps: Deps) {
    this.repository = deps.repository;
    this.log = deps.logger ?? nopLogger;
  }

  /**
   * Returns the Bolu's open plan.
   *
   * ErrNotFound is the honest answer for a Bolu that has not written one, and
   * the tool turns it into an empty list rather than an error.
   */
  async read(_ctx: unknown, scope: Scope, agentId: string): Promise<Checklist> {
    requireScope(scope);
    if (isNil(agentId)) {
      throw wrap(ErrInvalidInput, "Bolu tidak dikenal");
    }

    return this.repository.active(scope, agentId);
  }

  /**
   * Writes the Bolu's next plan.
   *
   * It is refused while one is still open, and the refusal carries what is
   * still open rather than only the fact of the refusal.
   */
  async create(_ctx: unknown, scope: Scope, plan: NewChecklist): Promise<Checklist> {
    requireScope(scope);
    if (isNil(plan.agentId)) {
      throw wrap(ErrInvalidInput, "Bolu tidak dikenal");
    }

    const title = planTitle(plan.title);
    const tasks = planTasks(plan.tasks);

    // The read is what turns the database's constraint into a sentence. The
    // constraint still holds — it is what makes the rule true under two writers
    // — but the model gets the list instead of a unique-violation.
    let current: Checklist | null = null;
    try {
      current = await this.repository.active(scope, plan.agentId);
    } catch (err) {
      if (!is(err, ErrNotFound)) {
        throw err;
      }
      // Nothing open: the next plan may be written.
    }
    if (current) {
      throw wrap(ErrActiveExists, openWork(current));
    }

    return this.repository.create(scope, { ...plan, title, tasks });
  }

  /**
   * Ticks one item of the open plan.
   *
   * An item that is already ticked is not an error; a task with subtasks still
   * open is refused and the refusal names them; the last open task settles the
   * plan.
   */
  async check(_ctx: unknown, scope: Scope, agentId: string, itemId: string): Promise<Checklist> {
    const { plan, item } = await this.locate(scope, agentId, itemId);
    if (item.done) {
      return plan;
    }
    const open = openSubtasks(item);
    if (open.length > 0) {
      throw wrap(ErrSubtasksOpen, openSubtasksMessage(open));
    }

    return this.repository.markDone(scope, plan.id, itemId);
  }

  /** Deletes one item of the open plan, subtasks and all. */
  async remove(_ctx: unknown, scope: Scope, agentId: string, itemId: string): Promise<Checklist> {
    const { plan } = await this.locate(scope, agentId, itemId);
    return this.repository.removeItem(scope, plan.id, itemId);
  }

  /**
   * Throws the open plan away, which is what frees the Bolu to write another.
   */
  async clear(_ctx: unknown, scope: Scope, agentId: string): Promise<void> {
    let plan: Checklist;
    try {
      plan = await this.read(null, scope, agentId);
    } catch (err) {
      if (is(err, ErrNotFound)) {
        throw wrap(ErrNotFound, "tidak ada rencana aktif untuk dibuang");
      }
      throw err;
    }

    return this.repository.clear(scope, plan.id);
  }

  /** Returns plans for a screen. */
  async list(_ctx: unknown, scope: Scope, filter: Filter): Promise<Checklist[]> {
    requireScope(scope);
    if (filter.status !== "" && filter.status !== StatusActive && filter.status !== StatusDone) {
      throw wrap(ErrInvalidInput, `status ${JSON.stringify(filter.status)} tidak dikenal`);
    }

    return this.repository.list(scope, filter);
  }

  /**
   * Finds one item inside the Bolu's open plan.
   *
   * Both of the writes that name an item go through it, so "which plan is this
   * item in" and "is the Bolu's plan open at all" are answered once.
   */
  private async locate(
    scope: Scope,
    agentId: string,
    itemId: string,
  ): Promise<{ plan: Checklist; item: Item }> {
    requireScope(scope);
    if (isNil(agentId)) {
      throw wrap(ErrInvalidInput, "Bolu tidak dikenal");
    }
    if (isNil(itemId)) {
      throw wrap(ErrInvalidInput, "tugasnya belum disebut");
    }

    let plan: Checklist;
    try {
      plan = await this.repository.active(scope, agentId);
    } catch (err) {
      if (is(err, ErrNotFound)) {
        // Rewritten for the model, which has to know what to do about it.
        throw wrap(ErrNotFound, "tidak ada rencana aktif; tulis rencananya dulu dengan checklist.create");
      }
      throw err;
    }

    const item = findItem(plan, itemId);
    if (!item) {
      throw wrap(ErrNotFound, `tugas ${itemId} tidak ada di rencana yang sedang aktif`);
    }

    return { plan, item };
  }
}

// ---------------------------------------------------------------- validation

/**
 * Refuses a call that names no workspace, which would otherwise be answered by
 * Row Level Security as an empty result and read as "you have no plan".
 */
function requireScope(scope: Scope): void {
  if (isZeroScope(scope)) {
    throw wrap(ErrInvalidInput, "workspace is required");
  }
}

/** Normalises the plan's own title, which is optional. */
function planTitle(title: string): string {
  const trimmed = title.trim();
  if (runeLength(trimmed) > MaxTitleRunes) {
    throw wrap(ErrInvalidInput, `judul rencana lebih dari ${MaxTitleRunes} karakter`);
  }
  return trimmed;
}

/**
 * Validates the tasks and their subtasks, and trims every title.
 *
 * The bounds are the product's: a plan is a list a person reads, and a model
 * given no ceiling writes a hundred lines and calls it thorough.
 */
function planTasks(tasks: NewItem[]): NewItem[] {
  if (tasks.length === 0) {
    throw wrap(ErrInvalidInput, "rencananya belum berisi tugas apa pun");
  }
  if (tasks.length > MaxTasks) {
    throw wrap(ErrInvalidInput, `satu rencana paling banyak ${MaxTasks} tugas (diminta ${tasks.length})`);
  }

  return tasks.map((task, index) => {
    const title = taskTitle(task.title, index + 1);
    if (task.subtasks.length > MaxSubtasks) {
      throw wrap(ErrInvalidInput, `tugas ${JSON.stringify(title)} punya lebih dari ${MaxSubtasks} sub-tugas`);
    }

    const subtasks = task.subtasks.map((subtask, subIndex) => subtaskTitle(subtask, title, subIndex + 1));

    return { title, subtasks };
  });
}

/** Validates one task's title, naming its place in the list. */
function taskTitle(title: string, position: number): string {
  const trimmed = title.trim();
  if (trimmed === "") {
    throw wrap(ErrInvalidInput, `tugas ke-${position} belum punya judul`);
  }
  if (runeLength(trimmed) > MaxTitleRunes) {
    throw wrap(ErrInvalidInput, `judul tugas ke-${position} lebih dari ${MaxTitleRunes} karakter`);
  }
  return trimmed;
}

/** Validates one subtask's title against its task. */
function subtaskTitle(title: string, task: string, position: number): string {
  const trimmed = title.trim();
  if (trimmed === "") {
    throw wrap(ErrInvalidInput, `sub-tugas ke-${position} dari ${JSON.stringify(task)} belum punya judul`);
  }
  if (runeLength(trimmed) > MaxTitleRunes) {
    throw wrap(
      ErrInvalidInput,
      `judul sub-tugas ke-${position} dari ${JSON.stringify(task)} lebih dari ${MaxTitleRunes} karakter`,
    );
  }
  return trimmed;
}

// ---------------------------------------------------------------- messages

/**
 * Renders what is still open in a plan, which is what a refused create answers
 * with.
 */
function openWork(plan: Checklist): string {
  const { done, total } = counts(plan);

  const open: string[] = [];
  for (const item of plan.items) {
    if (item.done) {
      continue;
    }

    let line = item.title;
    const unfinished = openSubtasks(item);
    if (unfinished.length > 0) {
      line += ` (belum: ${unfinished.map((subtask) => subtask.title).join(", ")})`;
    }

    open.push(line);
  }

  if (open.length === 0) {
    return `rencana ${JSON.stringify(plan.title)} masih aktif walau semua tugasnya sudah ditandai selesai (${done}/${total})`;
  }

  return `rencana ${JSON.stringify(plan.title)} masih berjalan (${done} dari ${total} selesai). Selesaikan dulu, atau buang rencananya dengan checklist.clear. Yang belum selesai: ${open.join("; ")}`;
}

/** Renders the subtasks that stop a task from being ticked. */
function openSubtasksMessage(items: Item[]): string {
  return `sub-tugas ini belum selesai: ${items.map((item) => item.title).join(", ")}. Tandai dulu satu per satu, baru tugas induknya.`;
}

/** Counts code points the way Go's `len([]rune(s))` does. */
function runeLength(value: string): number {
  return [...value].length;
}

function isNil(id: string): boolean {
  return !id || id === "00000000-0000-0000-0000-000000000000";
}
