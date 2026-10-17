/**
 * Exposes memory and knowledge over HTTP. It talks to the domain service
 * interface only.
 *
 * The payloads are the module's own types: a memory has no secret in it — the
 * sensitive filter runs before anything is stored — and the screen needs every
 * field it carries, from the kind to the message a fact came from.
 *
 * Ported 1:1 from internal/modules/memory/handler/handler.go.
 */

import type { Hono } from "hono";
import { currentUserId, isUuid, workspaceUUID, type AppContext, type AppEnv } from "../../../platform/context.ts";
import type { Logger } from "../../../platform/logger.ts";
import { created, error, ok, okWithMeta } from "../../../platform/response.ts";
import {
  ErrInvalidInput,
  ErrNoEmbedding,
  ErrNoModel,
  ErrNotFound,
  ErrSensitive,
  ErrVectorWidth,
  is,
  knowledgeJSON,
  memoryJSON,
  type KnowledgeFilter,
  type MemoryFilter,
  type Scope,
} from "../domain/memory.ts";
import type { Service } from "../domain/service.ts";
import { trimSpace } from "../service/chunk.ts";

/** Serves the memory endpoints. */
export class Handler {
  constructor(
    private readonly service: Service,
    private readonly log: Logger,
  ) {}

  /**
   * Returns the memories of the workspace.
   *
   * Memory is not restricted to owners: what a Bolu remembers is what it does,
   * and the person who notices a wrong fact is not always the one who pays for
   * the workspace.
   */
  list = async (c: AppContext): Promise<Response> => {
    const filter: MemoryFilter = {
      agentId: "",
      main: false,
      query: trimSpace(c.req.query("q") ?? ""),
      includeSuperseded: queryBool(c, "history", false),
      limit: queryInt(c, "limit", 0),
    };

    const layer = trimSpace(c.req.query("layer") ?? "");
    let agentId: string;
    try {
      agentId = parseOptionalID(c.req.query("agent_id") ?? "");
    } catch {
      return error(c, 400, "agent_id is not a uuid", "invalid_input");
    }
    if (
      layer === "main" ||
      trimSpace(c.req.query("agent_id") ?? "").toLowerCase() === "main"
    ) {
      filter.main = true;
      agentId = "";
    }
    if (!filter.main && agentId !== "") {
      filter.agentId = agentId;
    }

    try {
      const memories = await this.service.memories(requestScopeOf(c), filter);
      const payload = memories.map(memoryJSON);
      return okWithMeta(c, "ok", payload, { count: payload.length });
    } catch (err) {
      return this.fail(c, "list memories", err);
    }
  };

  /** Stores one memory a person wrote. */
  add = async (c: AppContext): Promise<Response> => {
    const body = await readBody<{
      content?: string;
      kind?: string;
      agent_id?: string;
      pinned?: boolean;
    }>(c);
    if (!body) {
      return error(c, 400, "the body is not valid JSON", "invalid_request");
    }

    let agentId: string;
    try {
      agentId = parseLayerID(body.agent_id ?? "");
    } catch {
      return error(c, 400, "agent_id is not a uuid", "invalid_input");
    }

    try {
      const memory = await this.service.add(requestScopeOf(c), {
        content: body.content ?? "",
        kind: body.kind ?? "",
        agentId,
        pinned: body.pinned ?? false,
      });
      return created(c, "stored", memoryJSON(memory));
    } catch (err) {
      return this.fail(c, "add memory", err);
    }
  };

  /** Changes one memory: its text, its kind, its pin, or its layer. */
  edit = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("memoryID");
    if (!isUuid(id)) {
      return error(c, 400, "memory id is not a uuid", "invalid_input");
    }

    const body = await readBody<{
      content?: string | null;
      kind?: string | null;
      pinned?: boolean | null;
      agent_id?: string | null;
      move_to_main?: boolean | null;
    }>(c);
    if (!body) {
      return error(c, 400, "the body is not valid JSON", "invalid_request");
    }

    const request = {
      id,
      content: body.content ?? null,
      kind: body.kind ?? null,
      pinned: body.pinned ?? null,
      agentId: null as string | null,
      moveToMain: body.move_to_main === true,
    };
    if (body.agent_id != null) {
      let agentId: string;
      let main: boolean;
      try {
        [agentId, main] = parseMove(body.agent_id);
      } catch {
        return error(c, 400, "agent_id is not a uuid", "invalid_input");
      }
      if (main) {
        request.moveToMain = true;
      } else {
        request.agentId = agentId;
      }
    }

    try {
      const memory = await this.service.edit(requestScopeOf(c), request);
      return ok(c, "updated", memoryJSON(memory));
    } catch (err) {
      return this.fail(c, "edit memory", err);
    }
  };

  /** Deletes one memory and its vector. */
  forget = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("memoryID");
    if (!isUuid(id)) {
      return error(c, 400, "memory id is not a uuid", "invalid_input");
    }

    try {
      await this.service.forget(requestScopeOf(c), id);
      return ok(c, "deleted");
    } catch (err) {
      return this.fail(c, "delete memory", err);
    }
  };

  /** Returns the workspace's document chunks, including the ones whose embedding failed. */
  knowledge = async (c: AppContext): Promise<Response> => {
    const filter: KnowledgeFilter = {
      agentId: "",
      allAgents: false,
      status: trimSpace(c.req.query("status") ?? ""),
      query: trimSpace(c.req.query("q") ?? ""),
      source: trimSpace(c.req.query("source") ?? ""),
      limit: queryInt(c, "limit", 0),
      withVector: false,
    };

    try {
      filter.agentId = parseOptionalID(c.req.query("agent_id") ?? "");
    } catch {
      return error(c, 400, "agent_id is not a uuid", "invalid_input");
    }

    try {
      const items = await this.service.knowledge(requestScopeOf(c), filter);
      const payload = items.map(knowledgeJSON);
      return okWithMeta(c, "ok", payload, { count: payload.length });
    } catch (err) {
      return this.fail(c, "list knowledge", err);
    }
  };

  /** Chunks one document, embeds it, and stores the chunks. */
  ingest = async (c: AppContext): Promise<Response> => {
    const body = await readBody<{
      source?: string;
      text?: string;
      agent_id?: string;
    }>(c);
    if (!body) {
      return error(c, 400, "the body is not valid JSON", "invalid_request");
    }

    let agentId: string;
    try {
      agentId = parseLayerID(body.agent_id ?? "");
    } catch {
      return error(c, 400, "agent_id is not a uuid", "invalid_input");
    }

    try {
      const items = await this.service.ingest(requestScopeOf(c), {
        agentId,
        source: body.source ?? "",
        text: body.text ?? "",
      });
      return created(c, "stored", items.map(knowledgeJSON));
    } catch (err) {
      return this.fail(c, "ingest document", err);
    }
  };

  /** Deletes one chunk and its vector. */
  forgetKnowledge = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("knowledgeID");
    if (!isUuid(id)) {
      return error(c, 400, "knowledge id is not a uuid", "invalid_input");
    }

    try {
      await this.service.forgetKnowledge(requestScopeOf(c), id);
      return ok(c, "deleted");
    } catch (err) {
      return this.fail(c, "delete knowledge", err);
    }
  };

  /** Maps the module's errors onto responses. */
  private fail(c: AppContext, operation: string, err: unknown): Response {
    if (is(err, ErrNotFound)) {
      return error(c, 404, "not found", "memory_not_found");
    }
    if (is(err, ErrSensitive)) {
      // The message is the service's own sentence, which names what was found.
      // Stripping the error's prefix keeps "memory: " out of what a person
      // reads.
      return error(c, 422, userMessage(err, ErrSensitive), "sensitive_content");
    }
    if (is(err, ErrInvalidInput) || is(err, ErrVectorWidth)) {
      return error(c, 400, messageOf(err), "invalid_input");
    }
    if (is(err, ErrNoEmbedding) || is(err, ErrNoModel)) {
      // A deployment without an embedding provider cannot store a memory. That
      // is a 503 rather than a 400: the request was fine, the platform is not
      // ready for it.
      return error(c, 503, messageOf(err), "memory_unavailable");
    }

    this.log.error({ operation, err: messageOf(err) }, "memory request failed");
    return error(c, 500, "internal server error", "internal_error");
  }
}

/** Registers the endpoints every member of the workspace may use. */
export function routes(group: Hono<AppEnv>, h: Handler): void {
  group.get("/memories", h.list);
  group.post("/memories", h.add);
  group.patch("/memories/:memoryID", h.edit);
  group.delete("/memories/:memoryID", h.forget);

  group.get("/knowledge", h.knowledge);
  group.post("/knowledge", h.ingest);
  group.delete("/knowledge/:knowledgeID", h.forgetKnowledge);
}

function requestScopeOf(c: AppContext): Scope {
  return {
    userId: currentUserId(c),
    workspaceId: workspaceUUID(c) ?? "",
  };
}

/** Parses an id that may be absent, which "main" also expresses. */
function parseOptionalID(value: string): string {
  const trimmed = trimSpace(value);
  if (trimmed === "" || trimmed.toLowerCase() === "main") {
    return "";
  }
  if (!isUuid(trimmed)) {
    throw new Error("not a uuid");
  }
  return trimmed;
}

/** Reads the layer an add or an ingest names. An empty value and "main" both mean the workspace-wide layer. */
function parseLayerID(value: string): string {
  return parseOptionalID(value);
}

/** Reads a move request and reports whether it asks for the main layer. */
function parseMove(value: string): [string, boolean] {
  const trimmed = trimSpace(value);
  if (trimmed === "" || trimmed.toLowerCase() === "main") {
    return ["", true];
  }
  if (!isUuid(trimmed)) {
    throw new Error("not a uuid");
  }
  return [trimmed, false];
}

/** Reads a JSON body, answering null when it is absent or malformed. */
async function readBody<T>(c: AppContext): Promise<T | null> {
  try {
    const parsed: unknown = await c.req.json();
    if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
      return null;
    }
    return parsed as T;
  } catch {
    return null;
  }
}

/** Fiber's `c.QueryBool`, whose unparsable value is the default. */
function queryBool(c: AppContext, key: string, fallback: boolean): boolean {
  const raw = c.req.query(key);
  if (raw === undefined) {
    return fallback;
  }
  switch (raw.trim()) {
    case "1":
    case "t":
    case "T":
    case "true":
    case "TRUE":
    case "True":
      return true;
    case "0":
    case "f":
    case "F":
    case "false":
    case "FALSE":
    case "False":
      return false;
    default:
      return fallback;
  }
}

/** Fiber's `c.QueryInt`, whose unparsable value is the default. */
function queryInt(c: AppContext, key: string, fallback: number): number {
  const raw = c.req.query(key);
  if (raw === undefined) {
    return fallback;
  }
  const trimmed = raw.trim();
  if (!/^[+-]?\d+$/.test(trimmed)) {
    return fallback;
  }
  const value = Number(trimmed);
  return Number.isSafeInteger(value) ? value : fallback;
}

function messageOf(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/** Renders an error for a person: the detail after the sentinel's own text. */
function userMessage(err: unknown, sentinel: Error): string {
  const detail = trimSpace(messageOf(err).replace(sentinel.message, ""));
  return detail.startsWith(":") ? detail.slice(1) : detail;
}
