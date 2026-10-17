/**
 * The LLM gateway use cases: which provider a workspace calls, in which order,
 * what a call cost, and how much of the token allowance is left.
 *
 * Ported from internal/modules/llm/service/service.go. The Go package splits
 * one receiver across files; the TS port keeps the class here and puts the
 * methods of gateway.go, resolver.go, quota.go and embed.go in the matching
 * files as functions over the instance.
 *
 * Two rules run through the package:
 *   - the secret never leaves it in clear text: it is encrypted before it
 *     reaches the store and decrypted only to build a provider;
 *   - every call that reaches a provider is recorded in the usage ledger.
 */

import type { Logger } from "../../../platform/logger.ts";
import type {
  AgentOverride,
  AgentStore,
  CostTable,
  Factory,
  Gateway,
  ProviderConfig,
  Redacted,
  Resolver,
  Scope,
  SecretBox,
  Store,
  StoredProvider,
  UpsertRequest,
  UsageReporter,
} from "../domain/gateway.ts";
import type {
  AuditEntry,
  Auditor,
  Metrics,
  QuotaChecker,
  QuotaCounter,
  QuotaPolicy,
  UsageDaily,
  UsageReader,
  UsageRecorder,
  UsageReport,
} from "../domain/provider.ts";
import { redactStored } from "../domain/gateway.ts";
import type { Capabilities, ChatRequest, ChatResponse, EmbedRequest, EmbedResponse, Provider, StreamEvent, Usage } from "../domain/provider.ts";
import { ErrInvalidRequest, llmError } from "../domain/provider.ts";
import { chat, record, stream } from "./gateway.ts";
import { embed } from "./embed.ts";
import { check, dayStart } from "./quota.ts";
import { resolve } from "./resolver.ts";
import { chatSpan, embedSpan, metric, type ChatSpan, type Tracer } from "./tracing.ts";

/** Bounds the provider label. */
export const NameMaxLength = 80;

/** Bounds the fallback order. */
export const MaxPriority = 1000;

/** The window the usage report covers when the caller does not choose one. */
export const DefaultUsageDays = 30;

/** Bounds the report window, so one request cannot ask for years of aggregation. */
export const MaxUsageDays = 366;

/** How many providers one request tries before it gives up. */
export const ChainLimit = 3;

/** The service settings. */
export interface Config {
  /**
   * Turns the pre-flight token check on. Local development runs with it off, so
   * a fresh workspace can call a model before any package exists.
   */
  quotaEnabled: boolean;
  /** Caps the number of providers one request tries. Zero means ChainLimit. */
  chainLimit: number;
}

/** The service dependencies. */
export interface Deps {
  store: Store;
  agents: AgentStore;
  factory: Factory;
  /** Encrypts the API keys at rest. Null means this deployment cannot store a secret. */
  box: SecretBox | null;
  /** Writes the usage ledger. Required: a gateway that cannot record is not billable. */
  recorder: UsageRecorder | null;
  /** Reads the aggregated usage and the durable token total. */
  reader: UsageReader | null;
  /** The live token spend; quota is the allowance. */
  counter: QuotaCounter | null;
  quota: QuotaPolicy | null;
  costs: CostTable | null;
  default: ProviderConfig;
  /**
   * The model the platform embeds with. Empty means a provider's own configured
   * model is used, which only works when that model embeds.
   */
  embeddingModel: string;
  /** Records model configuration changes in the workspace's trail. Optional. */
  auditor: Auditor | null;
  /** Reports a trail that could not be written. Optional. */
  logger: Logger | null;
  /** Starts the spans of a model call. Optional. */
  tracer: Tracer | null;
  /** Reports what a call was and how long it took. Optional. */
  metrics: Metrics | null;
  settings: Config;
}

/** What the handler calls: the gateway plus the usage report, which one object answers. */
export interface ServicePort extends Gateway, UsageReporter {}

/** The gateway implementation. It satisfies Gateway, Resolver, QuotaChecker and UsageReporter. */
export class Service implements Gateway, Resolver, QuotaChecker, UsageReporter {
  readonly deps: Deps;
  readonly settings: Config;
  /** The time source, so the usage window and the quota period are testable. */
  private clock: () => Date;

  constructor(deps: Deps) {
    this.deps = deps;
    this.settings = {
      ...deps.settings,
      chainLimit: deps.settings.chainLimit > 0 ? deps.settings.chainLimit : ChainLimit,
    };
    this.clock = () => new Date();
  }

  /** Replaces the time source, which the tests use to cross a day or a month boundary. */
  setClock(clock: () => Date): void {
    this.clock = clock;
  }

  now(): Date {
    return this.clock ? this.clock() : new Date();
  }

  // ------------------------------------------------------------ configuration

  /** Lists the workspace configuration, redacted. */
  async providers(ctx: AbortSignal | undefined, scope: Scope): Promise<Redacted[]> {
    requireWorkspace(scope);
    const stored = await this.deps.store.list(ctx, scope.workspaceId);
    return stored.map(redactStored);
  }

  /**
   * Stores one provider configuration. The first provider of a workspace
   * becomes its default, so a workspace is never left without one.
   */
  async upsert(
    ctx: AbortSignal | undefined,
    scope: Scope,
    req: UpsertRequest,
  ): Promise<Redacted> {
    requireWorkspace(scope);

    const cfg: ProviderConfig = {
      ...req,
      workspaceId: scope.workspaceId,
      name: req.name.trim(),
      adapter: req.adapter.trim(),
      baseUrl: req.baseUrl.trim(),
      model: req.model.trim(),
    };

    this.validate(cfg);

    const stored = this.buildStored(cfg, req.clearKey ?? false);

    let saved: StoredProvider;
    if (!cfg.id) {
      // The first provider of a workspace is its default: without one the
      // fallback chain would have no head.
      if (!stored.isDefault) {
        const existing = await this.deps.store.list(ctx, scope.workspaceId);
        stored.isDefault = existing.length === 0;
      }
      saved = await this.deps.store.create(ctx, stored);
    } else {
      saved = await this.deps.store.update(ctx, stored);
    }

    const redacted = redactStored(saved);
    const action = cfg.id ? "provider.updated" : "provider.created";
    // The line is built from the redacted shape, so the key cannot reach it
    // even by accident: what is recorded is what a screen shows.
    this.audit(ctx, scope, {
      action,
      objectType: "provider",
      objectId: redacted.id,
      actorUserId: scope.userId,
      metadata: {
        name: redacted.name,
        adapter: redacted.adapter,
        model: redacted.model,
        has_api_key: redacted.has_api_key,
        is_default: redacted.is_default,
      },
    });

    return redacted;
  }

  /**
   * Records one entry. A trail that could not be written is reported rather
   * than failing the change: the configuration already changed.
   */
  audit(
    ctx: AbortSignal | undefined,
    scope: Scope,
    entry: AuditEntry,
  ): void {
    if (!this.deps.auditor) return;
    this.deps.auditor.record(ctx, scope, entry).catch((err: unknown) => {
      this.deps.logger?.warn(
        { action: entry.action, err },
        "could not record a model configuration audit entry",
      );
    });
  }

  /** Removes one configuration. */
  async delete(ctx: AbortSignal | undefined, scope: Scope, id: string): Promise<void> {
    requireWorkspace(scope);
    if (!id) throw llmError(ErrInvalidRequest, "provider id is required");

    // The name is read before the row goes, because after it the trail can only
    // say that a provider was removed — not which one. Read only when something
    // will record it.
    let name = "";
    if (this.deps.auditor) {
      const providers = await this.deps.store.list(ctx, scope.workspaceId).catch(() => []);
      name = providers.find((provider) => provider.id === id)?.name ?? "";
    }

    await this.deps.store.delete(ctx, scope.workspaceId, id);

    this.audit(ctx, scope, {
      action: "provider.deleted",
      objectType: "provider",
      objectId: id,
      actorUserId: scope.userId,
      metadata: { name },
    });
  }

  /**
   * Calls the provider once and reports what it can do, which is how the
   * settings screen proves that a key works before a Bolu depends on it.
   */
  async test(
    ctx: AbortSignal | undefined,
    scope: Scope,
    id: string,
  ): Promise<Capabilities> {
    requireWorkspace(scope);

    const stored = await this.deps.store.get(ctx, scope.workspaceId, id);
    const cfg = this.open(stored);
    const provider = this.deps.factory.build(cfg);

    // The prompt is the smallest one that proves the round trip: one short
    // answer, no tools. The call is billed by the provider, so it is recorded
    // like any other.
    const response = await provider.chat(ctx, {
      model: "",
      system: "",
      retrieved: "",
      messages: [
        { role: "user", text: "Balas satu kata: siap", images: [], toolCalls: [], toolResults: [] },
      ],
      tools: [],
      toolChoice: "auto",
      toolName: "",
      maxTokens: 16,
      temperature: null,
      stopSequences: [],
      metadata: { workspaceId: scope.workspaceId, agentId: "", taskId: "", purpose: "routing" },
    });

    await record(this, ctx, scope, cfg, response, {
      workspaceId: scope.workspaceId,
      agentId: "",
      taskId: "",
      purpose: "routing",
    });

    return provider.capabilities();
  }

  /** Reads the model override of one Bolu. */
  async agentModel(ctx: AbortSignal | undefined, scope: Scope): Promise<AgentOverride> {
    requireAgent(scope);
    return this.deps.agents.agentModel(ctx, scope.workspaceId, scope.agentId);
  }

  /**
   * Stores the model override of one Bolu. The provider it points at must exist
   * in the same workspace, so a stale id cannot silently fall back.
   */
  async setAgentModel(
    ctx: AbortSignal | undefined,
    scope: Scope,
    override: AgentOverride,
  ): Promise<void> {
    requireAgent(scope);

    override.adapter = override.adapter?.trim();
    override.base_url = override.base_url?.trim();
    override.model = override.model?.trim();

    if (override.adapter) {
      // Validate the pair the same way a stored provider is validated, so an
      // unknown adapter is rejected here rather than at the first task.
      this.deps.factory.build({
        id: "",
        workspaceId: "",
        name: "",
        adapter: override.adapter,
        baseUrl: override.base_url ?? "",
        model: override.model ?? "",
        apiKey: override.api_key ?? "",
        priority: 0,
        maxTokens: 0,
        isDefault: false,
        contextTokens: 0,
        enabled: true,
        createdAt: new Date(0),
        updatedAt: new Date(0),
      });
    }
    if (override.provider_id) {
      await this.deps.store.get(ctx, scope.workspaceId, override.provider_id);
    }

    await this.deps.agents.setAgentModel(ctx, scope.workspaceId, scope.agentId, override);

    // Which model a Bolu thinks with is a cost decision, so the change is
    // recorded. The entry names the shape of the override rather than a key.
    this.audit(ctx, scope, {
      action: "agent_model.changed",
      objectType: "agent",
      objectId: scope.agentId,
      actorUserId: scope.userId,
      metadata: {
        adapter: override.adapter ?? "",
        model: override.model ?? "",
        provider_id: override.provider_id ?? "",
        has_own_key: !!override.api_key,
        max_tokens: override.max_tokens ?? 0,
      },
    });
  }

  /** The aggregated spend of the workspace, most recent day first. */
  async usage(
    ctx: AbortSignal | undefined,
    scope: Scope,
    days: number,
  ): Promise<UsageDaily[]> {
    requireWorkspace(scope);
    if (!this.deps.reader) throw new Error("llm: no usage reader is configured");
    return this.deps.reader.daily(ctx, scope.workspaceId, this.windowStart(days));
  }

  /**
   * What the workspace spent over a window, read three ways. The window is
   * resolved in Jakarta, exactly like the daily report, so the two screens
   * cannot disagree about what "the last seven days" means.
   */
  async report(
    ctx: AbortSignal | undefined,
    scope: Scope,
    days: number,
    limit: number,
  ): Promise<UsageReport> {
    requireWorkspace(scope);
    if (!this.deps.reader) throw new Error("llm: no usage reader is configured");

    const since = this.windowStart(days);
    const report = await this.deps.reader.report(ctx, scope.workspaceId, since, limit);
    // The moment is set here rather than in the repository: the window is this
    // module's answer to "the last seven days".
    report.periodStartedAt = since;
    return report;
  }

  /** The first moment of the window, in Jakarta days. */
  private windowStart(days: number): Date {
    const window = days <= 0 ? DefaultUsageDays : Math.min(days, MaxUsageDays);
    const start = dayStart(this.now());
    start.setUTCDate(start.getUTCDate() - (window - 1));
    return start;
  }

  private validate(cfg: ProviderConfig): void {
    if (!cfg.model) throw llmError(ErrInvalidRequest, "model is required");
    if (!cfg.name) throw llmError(ErrInvalidRequest, "name is required");
    if (Array.from(cfg.name).length > NameMaxLength) {
      throw llmError(
        ErrInvalidRequest,
        `name is longer than ${NameMaxLength} characters`,
      );
    }
    if (cfg.priority < 0 || cfg.priority > MaxPriority) {
      throw llmError(
        ErrInvalidRequest,
        `priority must be between 0 and ${MaxPriority}`,
      );
    }
    if (cfg.maxTokens < 0 || cfg.contextTokens < 0) {
      throw llmError(ErrInvalidRequest, "token limits cannot be negative");
    }
    if (cfg.apiKey && !this.deps.box) {
      // Refusing is the only safe answer: storing the key in clear text is
      // worse than asking the operator to configure a key.
      throw llmError(
        ErrInvalidRequest,
        "no encryption key is configured, so an API key cannot be stored",
      );
    }

    // Building the adapter is the check that the adapter name and the model are
    // ones this deployment can serve. It costs a struct, no network call.
    this.deps.factory.build(cfg);
  }

  /**
   * Encrypts the key and shapes the row to write. The three states of the key
   * travel through encryptedKey: null keeps the stored secret, an empty array
   * clears it, and a sealed value replaces it.
   */
  private buildStored(cfg: ProviderConfig, clearKey: boolean): StoredProvider {
    const stored: StoredProvider = { ...cfg, encryptedKey: null };

    if (clearKey) {
      stored.encryptedKey = new Uint8Array(0);
    } else if (cfg.apiKey) {
      if (!this.deps.box) {
        throw llmError(ErrInvalidRequest, "no encryption key is configured");
      }
      stored.encryptedKey = this.deps.box.encrypt(cfg.apiKey);
    }

    // The plaintext key is never part of the row.
    stored.apiKey = "";
    return stored;
  }

  /** Decrypts a stored provider into a usable configuration. */
  open(stored: StoredProvider): ProviderConfig {
    const cfg: ProviderConfig = { ...stored };
    if (!stored.encryptedKey || stored.encryptedKey.length === 0) return cfg;
    if (!this.deps.box) {
      throw llmError(ErrInvalidRequest, "no encryption key is configured");
    }
    cfg.apiKey = this.deps.box.decrypt(stored.encryptedKey);
    return cfg;
  }

  // ------------------------------------------------------------ the other files

  chat(ctx: AbortSignal | undefined, scope: Scope, req: ChatRequest): Promise<ChatResponse> {
    return chat(this, ctx, scope, req);
  }

  stream(ctx: AbortSignal | undefined, scope: Scope, req: ChatRequest): Promise<AsyncIterable<StreamEvent>> {
    return stream(this, ctx, scope, req);
  }

  embed(ctx: AbortSignal | undefined, scope: Scope, req: EmbedRequest): Promise<EmbedResponse> {
    return embed(this, ctx, scope, req);
  }

  resolve(ctx: AbortSignal | undefined, scope: Scope): Promise<ProviderConfig[]> {
    return resolve(this, ctx, scope);
  }

  default(): ProviderConfig {
    return this.deps.default;
  }

  provider(cfg: ProviderConfig): Provider {
    return this.deps.factory.build(cfg);
  }

  check(ctx: AbortSignal | undefined, workspaceId: string): Promise<void> {
    return check(this, ctx, workspaceId);
  }

  /** Writes one usage row and moves the live counter. */
  record(
    ctx: AbortSignal | undefined,
    scope: Scope,
    cfg: ProviderConfig,
    response: ChatResponse,
    meta: ChatRequest["metadata"],
  ): Promise<void> {
    return record(this, ctx, scope, cfg, response, meta);
  }

  cost(model: string, usage: Usage): number {
    return this.deps.costs ? this.deps.costs.cost(model, usage) : 0;
  }

  // ------------------------------------------------------------ tracing helpers

  chatSpan(name: string, scope: Scope, req: ChatRequest): ChatSpan {
    return chatSpan(this.deps.tracer, name, scope, req);
  }

  embedSpan(scope: Scope, req: EmbedRequest): ChatSpan {
    return embedSpan(this.deps.tracer, scope, req);
  }

  metric(
    ctx: AbortSignal | undefined,
    provider: string,
    meta: ChatRequest["metadata"],
    attempts: number,
    elapsedMs: number,
  ): void {
    metric(this.deps.metrics, ctx, provider, meta, attempts, elapsedMs);
  }
}

export function requireWorkspace(scope: Scope): void {
  if (!scope.workspaceId) throw llmError(ErrInvalidRequest, "workspace is required");
}

export function requireAgent(scope: Scope): void {
  requireWorkspace(scope);
  if (!scope.agentId) throw llmError(ErrInvalidRequest, "agent is required");
}
