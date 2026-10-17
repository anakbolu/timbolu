/**
 * The gateway ports: what a workspace stores, what the service resolves, and
 * what the handler and the other modules call.
 *
 * Ported from internal/modules/llm/domain/gateway.go.
 */

import type {
  Capabilities,
  ChatRequest,
  ChatResponse,
  EmbedRequest,
  EmbedResponse,
  Provider,
  StreamEvent,
  Usage,
} from "./provider.ts";
import type { UsageDaily, UsageReport } from "./provider.ts";

/** The usage screen's read: a period's totals with the per-Bolu and per-task breakdowns. */
export interface UsageReporter {
  /** What a workspace spent over a window of days, with the same window read three ways. */
  report(
    ctx: AbortSignal | undefined,
    scope: Scope,
    days: number,
    limit: number,
  ): Promise<UsageReport>;
}

export { AdapterAnthropic, AdapterOpenAI, KnownAdapters } from "./provider.ts";

/** One configured route to a model. */
export interface ProviderConfig {
  id: string;
  workspaceId: string;
  /** The label the user sees, e.g. "OpenRouter (utama)". */
  name: string;
  /** Selects the wire format: openai or anthropic. */
  adapter: string;
  /** The endpoint root; the adapter appends its path. */
  baseUrl: string;
  /** The model name the provider expects. */
  model: string;
  /** The decrypted secret. It never leaves the backend. */
  apiKey: string;
  /** Orders the fallback chain: lower goes first. */
  priority: number;
  /** Caps the answer length; zero means the adapter default. */
  maxTokens: number;
  /** Marks the provider a workspace uses when an agent has no override. */
  isDefault: boolean;
  /** The model's context window, used for the pre-flight check. */
  contextTokens: number;
  enabled: boolean;
  createdAt: Date;
  updatedAt: Date;
}

/** A configuration as an API response sees it: the key is replaced by whether one is stored. */
export interface Redacted {
  id: string;
  name: string;
  adapter: string;
  base_url: string;
  model: string;
  priority: number;
  max_tokens: number;
  is_default: boolean;
  context_tokens: number;
  enabled: boolean;
  has_api_key: boolean;
  created_at: Date;
  updated_at: Date;
}

/** Hides the secret: the response says whether a key is stored, never what it is. */
export function redact(c: ProviderConfig): Redacted {
  return {
    id: c.id,
    name: c.name,
    adapter: c.adapter,
    base_url: c.baseUrl,
    model: c.model,
    priority: c.priority,
    max_tokens: c.maxTokens,
    is_default: c.isDefault,
    context_tokens: c.contextTokens,
    enabled: c.enabled,
    has_api_key: c.apiKey !== "",
    created_at: c.createdAt,
    updated_at: c.updatedAt,
  };
}

/** The per-Bolu part of a configuration: which model it uses and which provider it prefers. */
export interface AgentOverride {
  /** Pins the workspace provider this Bolu must use. */
  provider_id?: string;
  /** Adapter and base_url describe a provider that exists only for this Bolu. */
  adapter?: string;
  base_url?: string;
  model?: string;
  /** A per-agent key, stored encrypted in the same column. */
  api_key?: string;
  max_tokens?: number;
}

/** Reports whether the override says nothing, which means "use the workspace configuration". */
export function agentOverrideIsZero(o: AgentOverride): boolean {
  return !o.provider_id && !o.adapter && !o.model;
}

/**
 * Builds a ready-to-call adapter for one configuration. The container supplies
 * it, so this module never learns which adapters exist.
 */
export interface Factory {
  build(cfg: ProviderConfig): Provider;
}

/** Turns a workspace and an agent into the ordered list of providers to try. */
export interface Resolver {
  /** Returns at least one provider, or ErrNoProvider. */
  resolve(ctx: AbortSignal | undefined, scope: Scope): Promise<ProviderConfig[]>;
  /** Builds a ready-to-call adapter for one configuration. */
  provider(cfg: ProviderConfig): Provider;
  /** The platform-level configuration, used when a workspace has none. */
  default(): ProviderConfig;
}

/** The tenant identity a resolution runs as. */
export interface Scope {
  userId: string;
  workspaceId: string;
  /** Narrows the resolution to one Bolu's override. */
  agentId: string;
}

/** Reports whether the scope carries no identity. */
export function scopeIsZero(s: Scope): boolean {
  return !s.workspaceId;
}

/** The embedding half of the gateway. */
export interface Embedder {
  /** Turns text into vectors, walking the fallback chain the same way a completion does. */
  embed(
    ctx: AbortSignal | undefined,
    scope: Scope,
    req: EmbedRequest,
  ): Promise<EmbedResponse>;
}

/** What the agent worker calls. */
export interface Gateway extends Embedder {
  /** Runs one completion, falling back when a provider fails in a way another might survive. */
  chat(
    ctx: AbortSignal | undefined,
    scope: Scope,
    req: ChatRequest,
  ): Promise<ChatResponse>;
  /** Runs one completion and reports progress, with fallback before the first byte. */
  stream(
    ctx: AbortSignal | undefined,
    scope: Scope,
    req: ChatRequest,
  ): Promise<AsyncIterable<StreamEvent>>;
  /** Lists the workspace configuration, redacted. */
  providers(ctx: AbortSignal | undefined, scope: Scope): Promise<Redacted[]>;
  /** Stores one provider configuration. */
  upsert(
    ctx: AbortSignal | undefined,
    scope: Scope,
    req: UpsertRequest,
  ): Promise<Redacted>;
  /** Removes one configuration. */
  delete(ctx: AbortSignal | undefined, scope: Scope, id: string): Promise<void>;
  /** Calls the provider with a one-line prompt and reports its capabilities. */
  test(
    ctx: AbortSignal | undefined,
    scope: Scope,
    id: string,
  ): Promise<Capabilities>;
  /** Stores a Bolu's override. */
  setAgentModel(
    ctx: AbortSignal | undefined,
    scope: Scope,
    override: AgentOverride,
  ): Promise<void>;
  /** Reads a Bolu's override. */
  agentModel(ctx: AbortSignal | undefined, scope: Scope): Promise<AgentOverride>;
  /** The aggregated spend of the workspace. */
  usage(ctx: AbortSignal | undefined, scope: Scope, days: number): Promise<UsageDaily[]>;
}

/** A configuration as it lives in the database: the key is ciphertext. */
export interface StoredProvider extends ProviderConfig {
  /**
   * The sealed API key. null after a read means no key is stored; null on an
   * update means "keep the stored key", so a rename cannot wipe the secret.
   * An empty array clears it.
   */
  encryptedKey: Uint8Array | null;
}

/** Renders a stored configuration for an API response. */
export function redactStored(s: StoredProvider): Redacted {
  const out = redact(s);
  out.has_api_key = !!s.encryptedKey && s.encryptedKey.length > 0;
  return out;
}

/** One provider write. */
export interface UpsertRequest extends ProviderConfig {
  /**
   * Removes the stored API key. An empty apiKey means "keep the stored secret",
   * because the client never receives it and therefore cannot send it back.
   */
  clearKey?: boolean;
}

/** Persists provider configurations. It never sees a plaintext key. */
export interface Store {
  list(ctx: AbortSignal | undefined, workspaceId: string): Promise<StoredProvider[]>;
  get(
    ctx: AbortSignal | undefined,
    workspaceId: string,
    id: string,
  ): Promise<StoredProvider>;
  /** Inserts a new configuration, clearing the previous default in the same transaction. */
  create(
    ctx: AbortSignal | undefined,
    stored: StoredProvider,
  ): Promise<StoredProvider>;
  /** Replaces an existing configuration. */
  update(
    ctx: AbortSignal | undefined,
    stored: StoredProvider,
  ): Promise<StoredProvider>;
  delete(ctx: AbortSignal | undefined, workspaceId: string, id: string): Promise<void>;
}

/** Reads and writes one Bolu's model override. */
export interface AgentStore {
  agentModel(
    ctx: AbortSignal | undefined,
    workspaceId: string,
    agentId: string,
  ): Promise<AgentOverride>;
  setAgentModel(
    ctx: AbortSignal | undefined,
    workspaceId: string,
    agentId: string,
    override: AgentOverride,
  ): Promise<void>;
}

/** Encrypts and decrypts API keys at rest. */
export interface SecretBox {
  encrypt(plaintext: string): Uint8Array;
  decrypt(ciphertext: Uint8Array): string;
}

/** Turns token counts into a cost, in micro-rupiah. */
export interface CostTable {
  cost(model: string, usage: Usage): number;
}
