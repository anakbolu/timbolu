/**
 * Provider registry: configuration in, adapter out.
 *
 * Ported from internal/platform/llm/registry.go. The domain contract the
 * adapters implement (Provider, ChatRequest, StreamEvent, Capabilities) is
 * declared here as well, because the Go domain package
 * (internal/modules/llm/domain) is ported by the llm module owner and this
 * package is what the module imports. See the gaps in the port report: when
 * the domain module lands, these declarations should move there and this file
 * should re-export them unchanged.
 */

import { Client as OpenAIClient } from "./openai/client.ts";
import { Client as AnthropicClient } from "./anthropic/client.ts";
import type { Policy } from "./vision/vision.ts";

// ---------------------------------------------------------------- contract

/** Who wrote a message. */
export type Role = "system" | "user" | "assistant" | "tool";

/** Why the model stopped. */
export type FinishReason = "stop" | "tool_calls" | "length" | "content_filter" | "unknown";

/** The kinds of a streaming event. */
export type StreamEventType = "text" | "tool_call" | "usage" | "done" | "error";

/** How firmly the model must use a tool. */
export type ToolChoice = "auto" | "none" | "required";

export type LLMErrorCode =
  | "rate_limited"
  | "provider_unavailable"
  | "invalid_request"
  | "context_too_long"
  | "quota_exceeded"
  | "provider_not_found"
  | "duplicate_provider"
  | "no_provider"
  | "usage_not_recorded"
  | "unsupported";

/** Every adapter reports the same error shape, so the gateway can fall back. */
export class LLMError extends Error {
  constructor(
    readonly code: LLMErrorCode,
    message: string,
  ) {
    super(message);
    this.name = "LLMError";
  }
}

export const ErrRateLimited = new LLMError("rate_limited", "llm: provider rate limited");
export const ErrProviderUnavailable = new LLMError("provider_unavailable", "llm: provider unavailable");
export const ErrInvalidRequest = new LLMError("invalid_request", "llm: invalid request");
export const ErrContextTooLong = new LLMError("context_too_long", "llm: context too long");
export const ErrQuotaExceeded = new LLMError("quota_exceeded", "llm: workspace quota exceeded");
export const ErrProviderNotFound = new LLMError("provider_not_found", "llm: provider not configured");
export const ErrDuplicateProvider = new LLMError(
  "duplicate_provider",
  "llm: provider name already exists",
);
export const ErrNoProvider = new LLMError("no_provider", "llm: no provider is configured");
export const ErrUsageNotRecorded = new LLMError(
  "usage_not_recorded",
  "llm: usage could not be recorded",
);
export const ErrUnsupported = new LLMError("unsupported", "llm: provider does not support this request");

/** Wraps a sentinel with the provider's detail, the way `%w: %s` does in Go. */
export function llmError(sentinel: LLMError, detail: string): LLMError {
  return new LLMError(sentinel.code, detail ? `${sentinel.message}: ${detail}` : sentinel.message);
}

/** `errors.Is` for the LLM error set. */
export function isLLMError(err: unknown, sentinel: LLMError): boolean {
  return err instanceof LLMError && err.code === sentinel.code;
}

/** Reports whether falling back to another provider is worth trying. */
export function retryable(err: unknown): boolean {
  return (
    isLLMError(err, ErrRateLimited) ||
    isLLMError(err, ErrProviderUnavailable) ||
    isLLMError(err, ErrQuotaExceeded)
  );
}

/** A picture sent to a vision model. */
export interface Image {
  mediaType: string;
  /** base64, without a data: prefix. */
  data: string;
}

/** The model asking for a tool to be run. */
export interface ToolCall {
  id: string;
  name: string;
  /** Parsed JSON arguments; Go's json.RawMessage. */
  arguments: unknown;
}

/** The answer to a ToolCall. */
export interface ToolResult {
  callId: string;
  content: string;
  isError: boolean;
}

/** One turn of the conversation in the internal format. */
export interface Message {
  role: Role;
  text: string;
  images: Image[];
  toolCalls: ToolCall[];
  toolResults: ToolResult[];
}

/** A callable capability offered to the model. */
export interface Tool {
  name: string;
  description: string;
  /** Parsed JSON schema; Go's json.RawMessage. */
  schema: unknown;
  label: string;
}

/** Ties a call to the work that caused it. */
export interface RequestMetadata {
  workspaceId: string;
  agentId: string;
  taskId: string;
  purpose: string;
}

/** Purpose values. */
export const PurposeAgent = "agent";
export const PurposeRouting = "routing";
export const PurposeExtraction = "extraction";
export const PurposeEmbedding = "embedding";

/** One completion request. */
export interface ChatRequest {
  model: string;
  system: string;
  retrieved: string;
  messages: Message[];
  tools: Tool[];
  toolChoice: ToolChoice;
  /** when ToolChoice pins one tool */
  toolName: string;
  maxTokens: number;
  temperature: number | null;
  stopSequences: string[];
  metadata: RequestMetadata;
}

/** What a call cost, in tokens. */
export interface Usage {
  inputTokens: number;
  outputTokens: number;
  cacheReadTokens: number;
  cacheWriteTokens: number;
}

/** The billable token count. */
export function usageTotal(u: Usage): number {
  return u.inputTokens + u.outputTokens;
}

/** One completion. */
export interface ChatResponse {
  text: string;
  toolCalls: ToolCall[];
  finishReason: FinishReason;
  usage: Usage;
  model: string;
  provider: string;
  rawFinishReason: string;
}

/** One normalised streaming event. */
export interface StreamEvent {
  type: StreamEventType;
  text: string;
  toolCall: ToolCall | null;
  usage: Usage | null;
  finishReason: FinishReason;
  err: unknown;
}

/** What a provider can do. */
export interface Capabilities {
  tools: boolean;
  vision: boolean;
  streaming: boolean;
  promptCaching: boolean;
  parallelToolCalls: boolean;
  embeddings: boolean;
  maxContextTokens: number;
}

/** One embedding call. */
export interface EmbedRequest {
  model: string;
  inputs: string[];
  metadata: RequestMetadata;
}

/** What an embedding call answered. */
export interface EmbedResponse {
  vectors: number[][];
  model: string;
  provider: string;
  usage: Usage;
}

/** The width of the vectors this product stores. */
export const EmbeddingDimensions = 1536;

/** One configured way of reaching a model. */
export interface Provider {
  /** The adapter name, e.g. "openai" or "anthropic". */
  name(): string;
  /** The model this provider is configured with. */
  model(): string;
  capabilities(): Capabilities;
  chat(ctx: AbortSignal | undefined, req: ChatRequest): Promise<ChatResponse>;
  /**
   * The iterable ends with EventDone or EventError; it is closed when the
   * stream ends.
   */
  stream(ctx: AbortSignal | undefined, req: ChatRequest): Promise<AsyncIterable<StreamEvent>>;
  embed(ctx: AbortSignal | undefined, req: EmbedRequest): Promise<EmbedResponse>;
}

/** Reports why a request cannot be served, or null when it can. */
export function supports(c: Capabilities, req: ChatRequest): LLMError | null {
  if (req.tools.length > 0 && !c.tools) return ErrUnsupported;
  if (req.toolChoice === "required" || req.toolName !== "") {
    if (!c.tools) return ErrUnsupported;
  }
  for (const message of req.messages) {
    if (message.images.length > 0 && !c.vision) return ErrUnsupported;
  }
  if (c.maxContextTokens > 0 && estimateTokens(req) > c.maxContextTokens) {
    return ErrContextTooLong;
  }
  return null;
}

/** Roughly four characters per token, the provider-independent size estimate. */
export function estimateTokens(req: ChatRequest): number {
  let characters = req.system.length;
  for (const message of req.messages) {
    characters += message.text.length;
    for (const call of message.toolCalls) {
      characters += call.name.length + jsonLength(call.arguments);
    }
    for (const result of message.toolResults) {
      characters += result.content.length;
    }
  }
  for (const tool of req.tools) {
    characters += tool.name.length + tool.description.length + jsonLength(tool.schema);
  }
  return Math.floor(characters / 4);
}

function jsonLength(value: unknown): number {
  if (value === null || value === undefined) return 0;
  if (typeof value === "string") return value.length;
  try {
    return JSON.stringify(value).length;
  } catch {
    return 0;
  }
}

// ---------------------------------------------------------------- config

/** Adapter names, used in configuration and in the usage ledger. */
export const AdapterOpenAI = "openai";
export const AdapterAnthropic = "anthropic";

/** The adapters this gateway understands. */
export const KnownAdapters = [AdapterOpenAI, AdapterAnthropic] as const;

/** One configured route to a model. */
export interface ProviderConfig {
  id: string;
  workspaceId: string;
  name: string;
  /** Selects the wire format: openai or anthropic. */
  adapter: string;
  baseUrl: string;
  model: string;
  apiKey: string;
  priority: number;
  maxTokens: number;
  isDefault: boolean;
  contextTokens: number;
  enabled: boolean;
  createdAt: Date;
  updatedAt: Date;
}

/** Builds a ready-to-call adapter for one configuration. */
export interface LLMFactory {
  build(cfg: ProviderConfig): Provider;
}

// ---------------------------------------------------------------- factory

/**
 * Turns a stored configuration into a ready-to-call adapter.
 *
 * Vision is the deployment's list of models that cannot read a picture. A null
 * policy keeps the wire format's own default, which is that a model sees.
 */
export class Factory implements LLMFactory {
  readonly vision: Policy | null;

  constructor(vision: Policy | null = null) {
    this.vision = vision;
  }

  build(cfg: ProviderConfig): Provider {
    return build(cfg, this.vision ? this.vision.sees(cfg.model) : true);
  }
}

/**
 * Assembles the adapter for one configuration. Whether the model sees is
 * decided by the caller, because it is a fact about the model and not about the
 * wire format the adapter speaks.
 */
export function build(cfg: ProviderConfig, sees: boolean): Provider {
  switch (cfg.adapter) {
    case AdapterOpenAI:
      // One adapter serves every OpenAI-compatible endpoint because they share
      // the wire format. The base URL decides which one is called.
      return new OpenAIClient({
        baseUrl: cfg.baseUrl,
        apiKey: cfg.apiKey,
        model: cfg.model,
        maxTokens: cfg.maxTokens,
        contextTokens: cfg.contextTokens,
        supportsTools: true,
        supportsVision: sees,
        parallelToolCalls: true,
        supportsEmbeddings: true,
      });

    case AdapterAnthropic:
      return new AnthropicClient({
        baseUrl: cfg.baseUrl,
        apiKey: cfg.apiKey,
        model: cfg.model,
        maxTokens: cfg.maxTokens,
        contextTokens: cfg.contextTokens,
        supportsTools: true,
        supportsVision: sees,
        promptCaching: true,
      });

    default:
      throw llmError(ErrInvalidRequest, `unknown adapter ${JSON.stringify(cfg.adapter)}`);
  }
}
