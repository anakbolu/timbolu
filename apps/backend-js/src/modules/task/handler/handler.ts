/**
 * Exposes the task runtime over HTTP: the history, one task with its recorded
 * rounds, the children a task handed work to, and the cancel.
 *
 * It talks to the domain service interface only. Opening a task from a browser
 * is deliberately absent: a task is opened by chat, by a routine, by a webhook,
 * or by another task, and each of those has its own entry point. A client that
 * could open one directly could claim a trigger that never happened.
 *
 * Ported 1:1 from internal/modules/task/handler/handler.go.
 */

import type { Hono } from "hono";
import type { AppContext, AppEnv } from "../../../platform/context.ts";
import { isUuid, scope as requestScope } from "../../../platform/context.ts";
import { badRequest, conflict, internal, notFound } from "../../../platform/errors.ts";
import type { Logger } from "../../../platform/logger.ts";
import { nopLogger } from "../../../platform/logger.ts";
import { ok, okWithMeta } from "../../../platform/response.ts";
import type { Service } from "../domain/service.ts";
import type { Filter, Scope, Task } from "../domain/task.ts";
import {
  ErrCostLimitReached,
  ErrDepthLimitReached,
  ErrInvalidInput,
  ErrStepLimitReached,
  ErrTaskNotFound,
  ErrTokenLimitReached,
  ErrUnknownTrigger,
  is,
} from "../domain/task.ts";
import { attention as attentionList } from "./attention.ts";
import { Payload, StepWire } from "../service/wire.ts";

/** Serves the task endpoints. */
export class Handler {
  /**
   * Where this deployment's Temporal UI is reachable, and the namespace its
   * links are filed under. Both are deployment facts the container installs; an
   * empty base means no link is offered.
   */
  temporalUI = "";
  temporalNamespace = "";

  constructor(
    readonly service: Service,
    readonly log: Logger = nopLogger,
  ) {}

  /**
   * Registers the endpoints every member of the workspace may read.
   *
   * The attention list is registered before the by-id route, and that order is
   * load-bearing rather than cosmetic: Hono matches in registration order, so
   * `/tasks/:taskID` first would claim `/tasks/attention` and try to parse
   * "attention" as a uuid.
   */
  routes(group: Hono<AppEnv>): void {
    group.get("/tasks", this.history);
    group.get("/tasks/attention", this.attention);
    group.get("/tasks/:taskID", this.get);
    group.get("/tasks/:taskID/steps", this.steps);
    group.get("/tasks/:taskID/children", this.children);
    group.post("/tasks/:taskID/cancel", this.cancel);
  }

  /** The tasks of the workspace, newest first. */
  history = async (c: AppContext): Promise<Response> => {
    const agentId = c.req.query("agent_id") ?? "";
    if (agentId !== "" && !isUuid(agentId)) {
      throw badRequest("agent_id is not a uuid", "invalid_input");
    }

    const parentTaskId = c.req.query("parent_task_id") ?? "";
    if (parentTaskId !== "" && !isUuid(parentTaskId)) {
      throw badRequest("parent_task_id is not a uuid", "invalid_input");
    }

    const filter: Filter = {
      status: c.req.query("status") ?? "",
      live: queryBool(c.req.query("live")),
      limit: queryInt(c.req.query("limit"), 0),
      agentId: agentId === "" ? "" : agentId,
      parentTaskId: parentTaskId === "" ? "" : parentTaskId,
    };

    let tasks: Task[];
    try {
      tasks = await this.service.history(undefined, this.scope(c), filter);
    } catch (err) {
      throw this.fail("list tasks", err);
    }

    const payload = tasks.map((task) => Payload(task, this.now()));
    return okWithMeta(c, "ok", payload, { count: payload.length });
  };

  /** Returns one task. */
  get = async (c: AppContext): Promise<Response> => {
    const id = parseId(c.req.param("taskID"));

    let task: Task;
    try {
      task = await this.service.get(undefined, this.scope(c), id);
    } catch (err) {
      throw this.fail("get task", err);
    }

    return ok(c, "ok", Payload(task, this.now()));
  };

  /** Returns a task's recorded rounds, in order. */
  steps = async (c: AppContext): Promise<Response> => {
    const id = parseId(c.req.param("taskID"));

    let steps;
    try {
      steps = await this.service.steps(undefined, this.scope(c), id);
    } catch (err) {
      throw this.fail("list steps", err);
    }

    const payload = steps.map(StepWire);
    return okWithMeta(c, "ok", payload, { count: payload.length });
  };

  /**
   * Returns the tasks this one handed work to, which is how a handoff chain is
   * read from its root.
   */
  children = async (c: AppContext): Promise<Response> => {
    const id = parseId(c.req.param("taskID"));

    let children: Task[];
    try {
      children = await this.service.children(undefined, this.scope(c), id);
    } catch (err) {
      throw this.fail("list children", err);
    }

    const payload = children.map((child) => Payload(child, this.now()));
    return okWithMeta(c, "ok", payload, { count: payload.length });
  };

  /** Stops a task that has not finished. */
  cancel = async (c: AppContext): Promise<Response> => {
    const id = parseId(c.req.param("taskID"));

    let task: Task;
    try {
      task = await this.service.cancel(undefined, this.scope(c), id);
    } catch (err) {
      throw this.fail("cancel task", err);
    }

    return ok(c, "task canceled", Payload(task, this.now()));
  };

  /** The attention list: the work that needs a person. */
  attention = async (c: AppContext): Promise<Response> => attentionList(this, c);

  /**
   * The clock the health mark is derived from. The service owns the real clock;
   * this reads the wall clock for a display field, which is why it is not
   * injectable.
   */
  now(): Date {
    return new Date();
  }

  /** The module scope from the identity the tenant middleware stored. */
  scope(c: AppContext): Scope {
    const s = requestScope(c);
    return { userId: s.userId ?? null, workspaceId: s.workspaceId ?? null };
  }

  /**
   * Installs the deployment's Temporal UI, which is what a stuck task's link
   * points at.
   */
  withTemporalUI(baseUrl: string, namespace: string): Handler {
    this.temporalUI = baseUrl;
    this.temporalNamespace = namespace.trim() === "" ? "default" : namespace;
    return this;
  }

  /** Maps a service error onto the envelope the frontend branches on. */
  fail(operation: string, err: unknown): Error {
    if (is(err, ErrTaskNotFound)) return notFound("task not found", "task_not_found");
    if (is(err, ErrInvalidInput)) return badRequest(messageOf(err), "invalid_input");
    if (is(err, ErrUnknownTrigger)) return badRequest(messageOf(err), "unknown_trigger");
    if (
      is(err, ErrStepLimitReached) ||
      is(err, ErrTokenLimitReached) ||
      is(err, ErrCostLimitReached) ||
      is(err, ErrDepthLimitReached)
    ) {
      return conflict(messageOf(err), "task_limit_reached");
    }

    this.log.error({ operation, err: messageOf(err) }, "task request failed");
    return internal("internal server error", "internal_error");
  }
}

/** Mirrors Go `func Routes(group fiber.Router, h *Handler)`. */
export function routes(group: Hono<AppEnv>, h: Handler): void {
  h.routes(group);
}

/** Reads a path parameter that must be a uuid, answering the Go 400 when it is not. */
function parseId(value: string | undefined): string {
  const id = (value ?? "").trim();
  if (!isUuid(id)) {
    throw badRequest("task id is not a uuid", "invalid_input");
  }
  return id;
}

/** Fiber's `c.QueryBool`: a missing value is false, and only strconv's spellings are true. */
function queryBool(value: string | undefined): boolean {
  switch (value) {
    case "1":
    case "t":
    case "T":
    case "TRUE":
    case "true":
    case "True":
      return true;
    default:
      return false;
  }
}

/** Fiber's `c.QueryInt`: an absent or unparsable value falls back to the default. */
function queryInt(value: string | undefined, fallback: number): number {
  if (value === undefined || value.trim() === "") return fallback;
  const parsed = Number(value);
  return Number.isInteger(parsed) ? parsed : fallback;
}

function messageOf(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
