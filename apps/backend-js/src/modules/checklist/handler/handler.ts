/**
 * Exposes the plans Bolus keep over HTTP.
 *
 * It is read-only, and that is deliberate rather than unfinished. A plan
 * belongs to the Bolu that is working from it: the tools are how one is
 * written, ticked, and thrown away, and a client that could tick a line could
 * make a Bolu believe work was done that nobody did. A screen shows the list;
 * the Bolu is the only thing that moves it.
 *
 * Ported 1:1 from internal/modules/checklist/handler/handler.go.
 */

import type { Hono } from "hono";
import { currentUserId, isUuid, workspaceUUID, type AppContext, type AppEnv } from "../../../platform/context.ts";
import type { Logger } from "../../../platform/logger.ts";
import { error, okWithMeta } from "../../../platform/response.ts";
import {
  ErrInvalidInput,
  counts,
  is,
  type Checklist,
  type Filter,
  type Item,
  type Scope,
  type Service,
} from "../domain/checklist.ts";

/** Serves the plans. */
export class Handler {
  constructor(
    private readonly service: Service,
    private readonly log: Logger,
  ) {}

  /**
   * Returns the workspace's plans, newest first.
   *
   * A plan is the team's business rather than a private one: the person who
   * notices a Bolu going round in circles is not always the owner.
   */
  list = async (c: AppContext): Promise<Response> => {
    const filter: Filter = {
      agentId: "",
      status: c.req.query("status") ?? "",
      limit: parseLimit(c.req.query("limit")),
    };

    // An agent id that is not a uuid is a client bug rather than an empty
    // result, so it is refused rather than passed down as a nil filter — which
    // would quietly answer with every Bolu's plan.
    const raw = c.req.query("agent_id");
    if (raw) {
      if (!isUuid(raw)) {
        return error(c, 400, "agent_id bukan uuid", "invalid_input");
      }
      filter.agentId = raw;
    }

    try {
      const plans = await this.service.list(c, requestScopeOf(c), filter);
      const payload = plans.map(toPayload);
      return okWithMeta(c, "ok", payload, { count: payload.length });
    } catch (err) {
      return this.fail(c, "list checklists", err);
    }
  };

  /** Maps the module's errors onto responses. */
  private fail(c: AppContext, operation: string, err: unknown): Response {
    if (is(err, ErrInvalidInput)) {
      return error(c, 400, err instanceof Error ? err.message : String(err), "invalid_input");
    }

    this.log.error({ operation, err }, "checklist request failed");
    return error(c, 500, "permintaan gagal", "internal_error");
  }
}

/** Registers the endpoints every member of the workspace may read. */
export function routes(group: Hono<AppEnv>, h: Handler): void {
  group.get("/checklists", h.list);
}

/** The tenant the request runs as. */
function requestScopeOf(c: AppContext): Scope {
  return { userId: currentUserId(c), workspaceId: workspaceUUID(c) ?? "" };
}

/** Reads `limit`, treating an unreadable value as absent, as `QueryInt` does. */
function parseLimit(raw: string | undefined): number {
  if (!raw || !/^[+-]?\d+$/.test(raw)) {
    return 0;
  }
  return Number.parseInt(raw, 10);
}

// ---------------------------------------------------------------- the wire

interface ChecklistPayload {
  checklist_id: string;
  agent_id: string;
  task_id?: string;
  title: string;
  status: string;
  done_count: number;
  total_count: number;
  items: ItemPayload[];
  created_at: string;
  completed_at?: string;
}

interface ItemPayload {
  item_id: string;
  title: string;
  done: boolean;
  subtasks?: ItemPayload[];
}

/** Renders a plan. */
function toPayload(plan: Checklist): ChecklistPayload {
  const { done, total } = counts(plan);

  const payload: ChecklistPayload = {
    checklist_id: plan.id,
    agent_id: plan.agentId,
    title: plan.title,
    status: plan.status,
    done_count: done,
    total_count: total,
    items: plan.items.map(renderItem),
    created_at: rfc3339(plan.createdAt),
  };
  if (plan.taskId && plan.taskId !== "00000000-0000-0000-0000-000000000000") {
    payload.task_id = plan.taskId;
  }
  if (plan.completedAt) {
    payload.completed_at = rfc3339(plan.completedAt);
  }

  return payload;
}

/** Renders one task, with its subtasks. */
function renderItem(item: Item): ItemPayload {
  const rendered: ItemPayload = { item_id: item.id, title: item.title, done: item.done };
  if (item.subtasks.length > 0) {
    rendered.subtasks = item.subtasks.map((subtask) => ({
      item_id: subtask.id,
      title: subtask.title,
      done: subtask.done,
    }));
  }
  return rendered;
}

/** Formats a time the way Go's `Format("2006-01-02T15:04:05Z07:00")` does. */
function rfc3339(at: Date): string {
  return at.toISOString().replace(/\.\d{3}Z$/, "Z");
}
