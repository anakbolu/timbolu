/**
 * Exposes the plan a Bolu keeps for itself as tools the hub can run.
 *
 * It is an adapter in the hub's sense and, like the order module's, it is not a
 * connected account: a plan belongs to the Bolu, there is no provider, no
 * token, and nothing outside this deployment to reach. `needsConnection` says
 * so, and it is what the tool filter reads to offer these tools to a Bolu whose
 * profile asks for them without demanding a grant.
 *
 * The adapter is deliberately thin: it turns a tool call's JSON into a service
 * call and the answer back into JSON, and decides nothing of its own.
 *
 * Ported 1:1 from internal/modules/checklist/adapter/adapter.go.
 */

import {
  ErrNotFound,
  StatusActive,
  counts,
  is,
  type Checklist,
  type Item,
  type NewChecklist,
  type Scope,
} from "../domain/checklist.ts";
import {
  ErrSchemaViolation,
  ErrUnknownTool,
  type Adapter,
  type Call,
  type Result,
  type ToolSpec,
  type TriggerSpec,
} from "../../integration/domain/adapter.ts";
import { LabelRead, LabelWriteInternal } from "../../integration/domain/integration.ts";

/** The application key this adapter answers for. */
export const App = "checklist";

/** Writes the plan down. `write_internal`: nobody is asked before it runs. */
export const ToolCreate = "checklist.create";
/** Ticks one task or subtask. */
export const ToolCheck = "checklist.check";
/** Drops one task from the plan. */
export const ToolRemove = "checklist.remove";
/** Throws the whole plan away. */
export const ToolClear = "checklist.clear";
/** Answers what the plan is. */
export const ToolRead = "checklist.read";

/** The part of the module this adapter uses. */
export interface Service {
  read(ctx: unknown, scope: Scope, agentId: string): Promise<Checklist>;
  create(ctx: unknown, scope: Scope, plan: NewChecklist): Promise<Checklist>;
  check(ctx: unknown, scope: Scope, agentId: string, itemId: string): Promise<Checklist>;
  remove(ctx: unknown, scope: Scope, agentId: string, itemId: string): Promise<Checklist>;
  clear(ctx: unknown, scope: Scope, agentId: string): Promise<void>;
}

/** The plan integration. */
export class ChecklistAdapter implements Adapter {
  constructor(private readonly plans: Service) {}

  /** The application key. */
  app(): string {
    return App;
  }

  /**
   * There is no token, because there is no account. The Bolu is the account.
   */
  needsToken(): boolean {
    return false;
  }

  /**
   * False is the whole point of this adapter: a plan is the Bolu's own, so a
   * call needs no connected account and no grant.
   */
  needsConnection(): boolean {
    return false;
  }

  /** A plan announces nothing. */
  triggers(): TriggerSpec[] {
    return [];
  }

  /** What a Bolu can do with its plan. */
  tools(): ToolSpec[] {
    return [
      {
        name: ToolCreate,
        label: LabelWriteInternal,
        description:
          "Tulis rencana kerja kamu sendiri: daftar tugas, masing-masing boleh punya " +
          "sub-tugas. Pakai ini kalau pekerjaannya butuh beberapa langkah, supaya langkahnya tidak " +
          "hilang di tengah jalan. Satu Bolu hanya boleh punya satu rencana aktif; selesaikan dulu " +
          "rencana sebelumnya sebelum menulis yang baru.",
        schema: `{
				"type": "object",
				"properties": {
					"title": {
						"type": "string",
						"description": "Judul rencananya, misalnya 'Tagih pelanggan bulan ini'"
					},
					"tasks": {
						"type": "array",
						"description": "Tugas-tugasnya, urut sesuai yang kamu tulis",
						"items": {
							"type": "object",
							"properties": {
								"title": {"type": "string", "description": "Satu tugas"},
								"subtasks": {
									"type": "array",
									"description": "Langkah-langkah kecil di dalam tugas ini, kalau ada",
									"items": {"type": "string"}
								}
							},
							"required": ["title"]
						}
					}
				},
				"required": ["tasks"]
			}`,
      },
      {
        name: ToolCheck,
        label: LabelWriteInternal,
        description:
          "Tandai satu tugas atau sub-tugas di rencanamu sudah selesai. Pakai id yang " +
          "diberikan checklist.create atau checklist.read. Tugas induk baru bisa ditandai selesai " +
          "kalau semua sub-tugasnya sudah selesai.",
        schema: `{
				"type": "object",
				"properties": {
					"id": {"type": "string", "description": "id tugas atau sub-tugas yang sudah selesai"}
				},
				"required": ["id"]
			}`,
      },
      {
        name: ToolRemove,
        label: LabelWriteInternal,
        description:
          "Hapus satu tugas dari rencana yang sedang aktif, kalau tugas itu memang tidak " +
          "perlu dikerjakan lagi. Sub-tugasnya ikut terhapus.",
        schema: `{
				"type": "object",
				"properties": {
					"id": {"type": "string", "description": "id tugas atau sub-tugas yang mau dihapus"}
				},
				"required": ["id"]
			}`,
      },
      {
        name: ToolClear,
        label: LabelWriteInternal,
        description:
          "Buang seluruh rencana yang sedang aktif, kalau rencananya sudah tidak dipakai " +
          "lagi. Setelah ini kamu bebas menulis rencana baru. Pakai ini hanya kalau rencananya " +
          "memang salah atau batal, bukan untuk menghindari menyelesaikan pekerjaan.",
        schema: `{
				"type": "object",
				"properties": {}
			}`,
      },
      {
        name: ToolRead,
        label: LabelRead,
        description:
          "Baca rencana yang sedang aktif: tugas dan sub-tugasnya beserta id dan mana yang " +
          "sudah selesai. Pakai ini kalau kamu perlu tahu apa yang masih tersisa.",
        schema: `{
				"type": "object",
				"properties": {}
			}`,
      },
    ];
  }

  /** Runs one tool. */
  async execute(ctx: unknown, call: Call): Promise<Result> {
    const scope: Scope = {
      userId: call.scope.userId,
      workspaceId: call.scope.workspaceId,
    };

    switch (call.tool) {
      case ToolCreate:
        return this.create(ctx, scope, call);
      case ToolCheck:
        return this.check(ctx, scope, call);
      case ToolRemove:
        return this.remove(ctx, scope, call);
      case ToolClear:
        return this.clear(ctx, scope, call);
      case ToolRead:
        return this.read(ctx, scope, call);
      default:
        throw new Error(`${ErrUnknownTool.message}: ${call.tool}`);
    }
  }

  // ------------------------------------------------------------- the tools

  /** Writes the plan down and answers with it, ids and all. */
  private async create(ctx: unknown, scope: Scope, call: Call): Promise<Result> {
    let args: { title?: string; tasks?: { title?: string; subtasks?: string[] }[] };
    try {
      args = JSON.parse(call.arguments || "{}");
    } catch (err) {
      throw new Error(`${ErrSchemaViolation.message}: ${message(err)}`);
    }

    const plan = await this.plans.create(ctx, scope, {
      agentId: call.scope.agentId ?? NilId,
      taskId: call.scope.taskId ?? NilId,
      title: args.title ?? "",
      tasks: (args.tasks ?? []).map((task) => ({
        title: task.title ?? "",
        subtasks: task.subtasks ?? [],
      })),
    });

    const { total } = counts(plan);
    return planResult(
      plan,
      `Rencana ${JSON.stringify(plan.title)} dibuat: ${plan.items.length} tugas, ${total} langkah`,
    );
  }

  /** Ticks one item and answers with the plan as it now is. */
  private async check(ctx: unknown, scope: Scope, call: Call): Promise<Result> {
    const itemId = itemIdOf(call);
    const plan = await this.plans.check(ctx, scope, call.scope.agentId ?? NilId, itemId);

    const { done, total } = counts(plan);
    const summary =
      plan.status === StatusActive
        ? `Satu langkah selesai (${done}/${total})`
        : `Rencana ${JSON.stringify(plan.title)} selesai: ${total} langkah`;

    return planResult(plan, summary);
  }

  /** Drops one item and answers with what is left. */
  private async remove(ctx: unknown, scope: Scope, call: Call): Promise<Result> {
    const itemId = itemIdOf(call);
    const plan = await this.plans.remove(ctx, scope, call.scope.agentId ?? NilId, itemId);

    const { done, total } = counts(plan);
    const summary =
      plan.status === StatusActive
        ? `Satu tugas dihapus dari rencana (${done}/${total} selesai)`
        : `Rencana ${JSON.stringify(plan.title)} selesai: ${total} langkah`;

    return planResult(plan, summary);
  }

  /** Throws the plan away. */
  private async clear(ctx: unknown, scope: Scope, call: Call): Promise<Result> {
    await this.plans.clear(ctx, scope, call.scope.agentId ?? NilId);

    return {
      content: JSON.stringify({
        dibuang: true,
        catatan: "Rencana yang aktif sudah dibuang. Kamu bebas menulis rencana baru dengan checklist.create.",
      }),
      summary: "Rencana aktif dibuang",
    };
  }

  /**
   * Answers what the plan is.
   *
   * A Bolu with no plan is a successful answer rather than a failure: "what am
   * I working on" has the answer "nothing".
   */
  private async read(ctx: unknown, scope: Scope, call: Call): Promise<Result> {
    let plan: Checklist;
    try {
      plan = await this.plans.read(ctx, scope, call.scope.agentId ?? NilId);
    } catch (err) {
      if (is(err, ErrNotFound)) {
        return {
          content: JSON.stringify({
            rencana: null,
            catatan:
              "Belum ada rencana aktif. Tulis satu dengan checklist.create kalau pekerjaannya butuh beberapa langkah.",
          }),
          summary: "Belum ada rencana aktif",
        };
      }
      throw err;
    }

    return planResult(plan, `Rencana ${JSON.stringify(plan.title)} dibaca`);
  }
}

// ---------------------------------------------------------------- rendering

/**
 * Reads the id a check or remove call names.
 *
 * A malformed id is refused here rather than at the database, because the model
 * is the one that has to fix it.
 */
function itemIdOf(call: Call): string {
  let args: { id?: string };
  try {
    args = JSON.parse(call.arguments || "{}");
  } catch (err) {
    throw new Error(`${ErrSchemaViolation.message}: ${message(err)}`);
  }

  const id = (args.id ?? "").trim();
  if (!isUuid(id)) {
    throw new Error(`${ErrSchemaViolation.message}: id tugasnya harus uuid, bukan ${JSON.stringify(args.id ?? "")}`);
  }
  return id;
}

/**
 * Renders a plan as the document the model reads.
 *
 * The ids travel with every answer, which is what makes `check` and `remove`
 * callable without a read in between.
 */
function planResult(plan: Checklist, summary: string): Result {
  const { done, total } = counts(plan);

  return {
    content: JSON.stringify({
      rencana: {
        id: plan.id,
        judul: plan.title,
        status: plan.status,
        selesai: done,
        total,
        tugas: renderItems(plan.items),
      },
    }),
    summary,
  };
}

/** Renders the tasks, each with its subtasks. */
function renderItems(items: Item[]): Record<string, unknown>[] {
  return items.map((item) => {
    const rendered: Record<string, unknown> = {
      id: item.id,
      judul: item.title,
      selesai: item.done,
    };

    if (item.subtasks.length > 0) {
      rendered["sub_tugas"] = item.subtasks.map((subtask) => ({
        id: subtask.id,
        judul: subtask.title,
        selesai: subtask.done,
      }));
    }

    return rendered;
  });
}

function isUuid(value: string): boolean {
  return /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value);
}

function message(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

const NilId = "00000000-0000-0000-0000-000000000000";
