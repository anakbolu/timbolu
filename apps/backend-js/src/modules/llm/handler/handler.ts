/**
 * The model configuration over HTTP: the provider list, the fallback order, the
 * per-Bolu override, and the usage report.
 *
 * Ported from internal/modules/llm/handler/handler.go. It talks to the domain
 * gateway interface only, and the secret never travels back: a response says
 * whether a key is stored, never what it is.
 */

import type { Hono } from "hono";
import type { AppContext, AppEnv } from "../../../platform/context.ts";
import { currentUserId, isUuid, workspaceUUID } from "../../../platform/context.ts";
import { AppError, badRequest } from "../../../platform/errors.ts";
import type { Logger } from "../../../platform/logger.ts";
import { error, ok, success } from "../../../platform/response.ts";
import type {
  AgentOverride,
  Gateway,
  Scope,
  UsageReporter,
} from "../domain/gateway.ts";
import type { Capabilities, UsageDaily } from "../domain/provider.ts";
import {
  ErrContextTooLong,
  ErrDuplicateProvider,
  ErrInvalidRequest,
  ErrNoProvider,
  ErrProviderNotFound,
  ErrProviderUnavailable,
  ErrQuotaExceeded,
  ErrRateLimited,
  ErrUnsupported,
  ErrUsageNotRecorded,
  KnownAdapters,
} from "../domain/provider.ts";
import { report } from "./report.ts";

/** Serves the model endpoints. */
export class Handler {
  readonly service: Gateway;
  /** The usage screen's read. A second field because the two have different consumers. */
  reporter: UsageReporter | null;
  readonly log: Logger;

  constructor(service: Gateway, log: Logger) {
    this.service = service;
    this.log = log;
    // The same object answers both when the deployment wired it.
    this.reporter = isUsageReporter(service) ? service : null;
  }

  /** Installs the usage reporter, which the container calls after the service is built. */
  withReporter(reporter: UsageReporter): Handler {
    this.reporter = reporter;
    return this;
  }

  /** The provider configuration in fallback order. */
  list = async (c: AppContext): Promise<Response> => {
    try {
      const providers = await this.service.providers(undefined, scope(c));
      return ok(c, "ok", providers);
    } catch (err) {
      return this.fail(c, "list providers", err);
    }
  };

  /** The adapter names the backend can serve. */
  adapters = async (c: AppContext): Promise<Response> => {
    return ok(c, "ok", { adapters: KnownAdapters });
  };

  /** The aggregated spend of the workspace, most recent day first. */
  usage = async (c: AppContext): Promise<Response> => {
    const days = queryInt(c, "days", 0);

    let usage: UsageDaily[];
    try {
      usage = await this.service.usage(undefined, scope(c), days);
    } catch (err) {
      return this.fail(c, "read usage", err);
    }

    const payload = usage.map((day) => ({
      day: day.day.toISOString().slice(0, 10),
      provider: day.provider,
      model: day.model,
      purpose: day.purpose,
      calls: day.calls,
      input_tokens: day.inputTokens,
      output_tokens: day.outputTokens,
      cache_read_tokens: day.cacheReadTokens,
      cache_write_tokens: day.cacheWriteTokens,
      total_tokens: day.inputTokens + day.outputTokens,
      cost_micros: day.costMicros,
    }));

    return ok(c, "ok", payload);
  };

  /** Adds a provider. */
  create = async (c: AppContext): Promise<Response> => {
    const req = await readJson<UpsertProviderRequest>(c);

    const enabled = req.enabled ?? true;

    try {
      const provider = await this.service.upsert(undefined, scope(c), {
        id: "",
        workspaceId: "",
        name: req.name ?? "",
        adapter: req.adapter ?? "",
        baseUrl: req.base_url ?? "",
        model: req.model ?? "",
        apiKey: req.api_key ?? "",
        priority: req.priority ?? 0,
        maxTokens: req.max_tokens ?? 0,
        isDefault: req.is_default ?? false,
        contextTokens: req.context_tokens ?? 0,
        enabled,
        createdAt: new Date(0),
        updatedAt: new Date(0),
      });
      return success(c, 201, "provider created", provider);
    } catch (err) {
      return this.fail(c, "create provider", err);
    }
  };

  /** Replaces a provider. An empty api_key keeps the stored secret. */
  update = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("providerID");
    if (!isUuid(id)) return error(c, 400, "invalid provider id", "invalid_request");

    const req = await readJson<UpsertProviderRequest>(c);

    let enabled: boolean;
    try {
      enabled = await this.resolveEnabled(c, id, req.enabled);
    } catch (err) {
      return this.fail(c, "resolve provider state", err);
    }

    try {
      const provider = await this.service.upsert(undefined, scope(c), {
        id,
        workspaceId: "",
        name: req.name ?? "",
        adapter: req.adapter ?? "",
        baseUrl: req.base_url ?? "",
        model: req.model ?? "",
        apiKey: req.api_key ?? "",
        priority: req.priority ?? 0,
        maxTokens: req.max_tokens ?? 0,
        isDefault: req.is_default ?? false,
        contextTokens: req.context_tokens ?? 0,
        enabled,
        createdAt: new Date(0),
        updatedAt: new Date(0),
        clearKey: req.clear_api_key ?? false,
      });
      return ok(c, "provider updated", provider);
    } catch (err) {
      return this.fail(c, "update provider", err);
    }
  };

  /** Removes a provider. */
  delete = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("providerID");
    if (!isUuid(id)) return error(c, 400, "invalid provider id", "invalid_request");

    try {
      await this.service.delete(undefined, scope(c), id);
    } catch (err) {
      return this.fail(c, "delete provider", err);
    }

    return ok(c, "provider deleted");
  };

  /** Calls the provider once, which is how the screen proves a key works. */
  test = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("providerID");
    if (!isUuid(id)) return error(c, 400, "invalid provider id", "invalid_request");

    let capabilities: Capabilities;
    try {
      capabilities = await this.service.test(undefined, scope(c), id);
    } catch (err) {
      return this.fail(c, "test provider", err);
    }

    return ok(c, "provider answered", {
      tools: capabilities.tools,
      vision: capabilities.vision,
      streaming: capabilities.streaming,
      prompt_caching: capabilities.promptCaching,
      parallel_tool_calls: capabilities.parallelToolCalls,
      max_context_tokens: capabilities.maxContextTokens,
    });
  };

  /** The model override of one Bolu. */
  agentModel = async (c: AppContext): Promise<Response> => {
    let agentScope: Scope;
    try {
      agentScope = this.agentScope(c);
    } catch (err) {
      return this.fail(c, "resolve agent scope", err);
    }

    try {
      const override = await this.service.agentModel(undefined, agentScope);
      return ok(c, "ok", modelPayload(override));
    } catch (err) {
      return this.fail(c, "read agent model", err);
    }
  };

  /** Stores the model override of one Bolu. */
  setAgentModel = async (c: AppContext): Promise<Response> => {
    let agentScope: Scope;
    try {
      agentScope = this.agentScope(c);
    } catch (err) {
      return this.fail(c, "resolve agent scope", err);
    }

    const req = await readJson<SetModelRequest>(c);

    let providerId: string;
    try {
      providerId = parseOptionalId(req.provider_id ?? "", "provider id");
    } catch (err) {
      return error(c, 400, messageOf(err), "invalid_request");
    }

    const override: AgentOverride = {
      provider_id: providerId || undefined,
      adapter: req.adapter ?? "",
      base_url: req.base_url ?? "",
      model: req.model ?? "",
      api_key: req.api_key ?? "",
      max_tokens: req.max_tokens ?? 0,
    };

    try {
      await this.service.setAgentModel(undefined, agentScope, override);
    } catch (err) {
      return this.fail(c, "write agent model", err);
    }

    return ok(c, "model updated", modelPayload(override));
  };

  /**
   * Keeps the stored value when the request omits the field, so a partial
   * update cannot disable a provider by accident.
   */
  private async resolveEnabled(
    c: AppContext,
    id: string,
    requested: boolean | undefined,
  ): Promise<boolean> {
    if (requested !== undefined) return requested;

    if (!isUuid(id)) throw new RequestError(400, "invalid_request", "invalid provider id");

    const providers = await this.service.providers(undefined, scope(c));
    const found = providers.find((provider) => provider.id === id);
    if (!found) throw ErrProviderNotFound;

    return found.enabled;
  }

  private agentScope(c: AppContext): Scope {
    const agentId = c.req.param("agentID");
    if (!isUuid(agentId)) {
      throw new RequestError(400, "invalid_request", "invalid agent id");
    }

    return {
      userId: currentUserId(c) ?? "",
      workspaceId: workspaceUUID(c) ?? "",
      agentId,
    };
  }

  fail(c: AppContext, operation: string, err: unknown): Response {
    if (err instanceof RequestError) {
      return error(c, err.status, err.message, err.code);
    }

    if (isCode(err, ErrProviderNotFound)) {
      return error(c, 404, "provider not found", "provider_not_found");
    }
    if (isCode(err, ErrDuplicateProvider)) {
      return error(c, 409, "a provider with that name already exists", "provider_exists");
    }
    if (isCode(err, ErrNoProvider)) {
      return error(c, 409, messageOf(err), "no_provider");
    }
    if (isCode(err, ErrQuotaExceeded)) {
      return error(c, 429, messageOf(err), "quota_exceeded");
    }
    if (isCode(err, ErrUnsupported)) {
      return error(c, 400, messageOf(err), "unsupported");
    }
    if (isCode(err, ErrContextTooLong)) {
      return error(c, 400, messageOf(err), "context_too_long");
    }
    if (isCode(err, ErrInvalidRequest)) {
      return error(c, 400, messageOf(err), "invalid_request");
    }
    if (isCode(err, ErrRateLimited)) {
      return error(c, 429, messageOf(err), "rate_limited");
    }
    if (isCode(err, ErrProviderUnavailable)) {
      return error(c, 502, messageOf(err), "provider_unavailable");
    }
    if (isCode(err, ErrUsageNotRecorded)) {
      return error(c, 500, messageOf(err), "usage_not_recorded");
    }

    this.log.error({ operation, err }, "llm request failed");
    return error(c, 500, "internal server error", "internal_error");
  }
}

/** Registers the endpoints every member of the workspace may read. */
export function routes(group: Hono<AppEnv>, h: Handler): void {
  group.get("/llm/providers", h.list);
  group.get("/llm/adapters", h.adapters);
  group.get("/llm/usage", h.usage);
  // The usage screen's read, and the export an accountant takes away. Both are
  // reads, so both sit behind the same guard as the rest of this group.
  group.get("/llm/usage/report", (c) => report(h, c));
  group.get("/agents/:agentID/model", h.agentModel);
}

/**
 * Registers the endpoints that change the configuration. The container applies
 * the tenant middleware and a managing-role check.
 */
export function managerRoutes(group: Hono<AppEnv>, h: Handler): void {
  group.post("/llm/providers", h.create);
  group.put("/llm/providers/:providerID", h.update);
  group.delete("/llm/providers/:providerID", h.delete);
  group.post("/llm/providers/:providerID/test", h.test);
  group.put("/agents/:agentID/model", h.setAgentModel);
}

/** A failure the handler itself detected, carrying the status and the code the client sees. */
export class RequestError extends AppError {
  constructor(status: number, code: string, message: string) {
    super(status, message, code);
  }
}

/** The scope, from the identity the tenant middleware stored. */
export function scope(c: AppContext): Scope {
  return {
    userId: currentUserId(c) ?? "",
    workspaceId: workspaceUUID(c) ?? "",
    agentId: "",
  };
}

/** Parses an optional UUID, answering the empty string for an absent one. */
export function parseOptionalId(value: string, field: string): string {
  if (!value) return "";
  if (!isUuid(value)) throw new Error(`invalid ${field}`);
  return value;
}

/** The model payload, with the override's fields omitted when they say nothing. */
export function modelPayload(override: AgentOverride): Record<string, unknown> {
  const out: Record<string, unknown> = { has_api_key: !!override.api_key };
  if (override.provider_id) out.provider_id = override.provider_id;
  if (override.adapter) out.adapter = override.adapter;
  if (override.base_url) out.base_url = override.base_url;
  if (override.model) out.model = override.model;
  if (override.max_tokens) out.max_tokens = override.max_tokens;
  return out;
}

/** Reads a JSON body, refusing one that is not an object the way BodyParser does. */
export async function readJson<T>(c: AppContext): Promise<Partial<T>> {
  const body: unknown = await c.req.json().catch(() => null);
  if (body === null || typeof body !== "object" || Array.isArray(body)) {
    throw badRequest("invalid request body", "invalid_request");
  }
  return body as Partial<T>;
}

/** `c.QueryInt` with Go's default: a missing or malformed value answers the default. */
export function queryInt(c: AppContext, key: string, fallback: number): number {
  const raw = c.req.query(key);
  if (raw === undefined || raw === "") return fallback;
  const parsed = Number.parseInt(raw, 10);
  return Number.isNaN(parsed) ? fallback : parsed;
}

function isUsageReporter(service: Gateway): service is Gateway & UsageReporter {
  return typeof (service as Partial<UsageReporter>).report === "function";
}

function isCode(err: unknown, sentinel: { code: string }): boolean {
  return (
    err instanceof Error && "code" in err && typeof err.code === "string" && err.code === sentinel.code
  );
}

function messageOf(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

interface UpsertProviderRequest {
  name: string;
  adapter: string;
  base_url: string;
  model: string;
  api_key: string;
  clear_api_key: boolean;
  priority: number;
  max_tokens: number;
  context_tokens: number;
  is_default: boolean;
  /**
   * Optional: omitting it on create means enabled, and omitting it on update
   * keeps the stored value rather than silently disabling a provider.
   */
  enabled?: boolean;
}

interface SetModelRequest {
  provider_id: string;
  adapter: string;
  base_url: string;
  model: string;
  api_key: string;
  max_tokens: number;
}
