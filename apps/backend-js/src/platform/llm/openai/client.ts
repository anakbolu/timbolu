/**
 * The [OI]-compatible chat completions wire format:
 * POST {base_url}/v1/chat/completions.
 *
 * Ported from internal/platform/llm/openai/client.go. The same adapter serves
 * every server that copies the format (OpenRouter, Groq, DeepSeek, vLLM,
 * Ollama); the base URL decides which one is called.
 */

import type {
  Capabilities,
  ChatRequest,
  ChatResponse,
  EmbedRequest,
  EmbedResponse,
  FinishReason,
  Provider,
  StreamEvent,
  ToolCall,
  Usage,
} from "../registry.ts";
import {
  ErrInvalidRequest,
  ErrProviderUnavailable,
  ErrRateLimited,
  ErrUnsupported,
  llmError,
} from "../registry.ts";
import { Reader, isDone } from "../sse/sse.ts";
import { refused } from "../vision/vision.ts";
import { embed as embedCall } from "./embed.ts";
import {
  decodeChoice,
  encodeRequest,
  mapFinishReason,
  type chatCompletion,
  type streamChunk,
  type streamToolCall,
  type wireError,
  wireErrorToError,
  wireUsageToUsage,
} from "./wire.ts";

/** Used when a configuration does not name a base URL. */
export const DefaultBaseURL = "https://api.openai.com";

/** The fallback context window when the configuration does not state one. */
export const DefaultContextTokens = 128000;

/** One connection to an [OI]-compatible server. */
export interface Config {
  baseUrl: string;
  apiKey: string;
  model: string;
  /** Caps the answer length when a request does not set one. */
  maxTokens: number;
  /** The model's window, used for the pre-flight check. */
  contextTokens: number;
  supportsTools: boolean;
  supportsVision: boolean;
  parallelToolCalls: boolean;
  /** Whether this server implements the embeddings route. */
  supportsEmbeddings: boolean;
  /** Extra headers some gateways need. */
  headers?: Record<string, string>;
  /** Bounds one call. Streaming uses the caller's signal instead. */
  timeout?: number;
}

/** The adapter. */
export class Client implements Provider {
  private readonly cfg: Required<Omit<Config, "headers">> & { headers: Record<string, string> };

  constructor(cfg: Config) {
    if (cfg.model.trim() === "") {
      throw llmError(ErrInvalidRequest, "model is required");
    }
    const baseUrl = (cfg.baseUrl === "" ? DefaultBaseURL : cfg.baseUrl).replace(/\/+$/, "");
    this.cfg = {
      baseUrl,
      apiKey: cfg.apiKey,
      model: cfg.model,
      maxTokens: cfg.maxTokens,
      contextTokens: cfg.contextTokens <= 0 ? DefaultContextTokens : cfg.contextTokens,
      supportsTools: cfg.supportsTools,
      supportsVision: cfg.supportsVision,
      parallelToolCalls: cfg.parallelToolCalls,
      supportsEmbeddings: cfg.supportsEmbeddings,
      headers: cfg.headers ?? {},
      timeout: cfg.timeout !== undefined && cfg.timeout > 0 ? cfg.timeout : 5 * 60_000,
    };
  }

  /** The adapter name. */
  name(): string {
    return "openai";
  }

  /** The configured model. */
  model(): string {
    return this.cfg.model;
  }

  /** The configured answer cap, which a request may override. */
  configuredMaxTokens(): number {
    return this.cfg.maxTokens;
  }

  /** What this configuration can do. */
  capabilities(): Capabilities {
    return {
      tools: this.cfg.supportsTools,
      vision: this.cfg.supportsVision,
      streaming: true,
      promptCaching: false,
      parallelToolCalls: this.cfg.parallelToolCalls,
      embeddings: this.cfg.supportsEmbeddings,
      maxContextTokens: this.cfg.contextTokens,
    };
  }

  /** One completion. */
  async chat(ctx: AbortSignal | undefined, req: ChatRequest): Promise<ChatResponse> {
    const body = JSON.stringify(encodeRequest(this, req, false));

    const resp = await this.do(ctx, body, false);
    const raw = await readLimited(resp, 32 << 20);

    let payload: chatCompletion;
    try {
      payload = JSON.parse(raw) as chatCompletion;
    } catch (err) {
      throw llmError(ErrProviderUnavailable, `decode response: ${errorText(err)}`);
    }
    if (payload.error) throw wireErrorToError(payload.error);
    if (!payload.choices || payload.choices.length === 0) {
      throw llmError(ErrProviderUnavailable, "provider returned no choices");
    }

    const decoded = decodeChoice(this, payload);
    return {
      text: decoded.text,
      toolCalls: decoded.toolCalls,
      finishReason: decoded.finishReason,
      rawFinishReason: decoded.rawFinishReason,
      model: decoded.model,
      provider: "openai",
      usage: decoded.usage ?? { inputTokens: 0, outputTokens: 0, cacheReadTokens: 0, cacheWriteTokens: 0 },
    };
  }

  /** Turns text into vectors; the route lives in embed.ts. */
  embed(ctx: AbortSignal | undefined, req: EmbedRequest): Promise<EmbedResponse> {
    return embedCall(this, ctx, req);
  }

  /** One completion, reporting its progress. */
  async stream(ctx: AbortSignal | undefined, req: ChatRequest): Promise<AsyncIterable<StreamEvent>> {
    const body = JSON.stringify(encodeRequest(this, req, true));
    const resp = await this.do(ctx, body, true);
    return this.readStream(ctx, resp);
  }

  /**
   * Turns the provider's events into the normalised ones. The iterable closes
   * when the stream ends; the last event is `done` or `error`.
   */
  private async *readStream(ctx: AbortSignal | undefined, resp: Response): AsyncIterable<StreamEvent> {
    if (!resp.body) {
      yield errorEvent(llmError(ErrProviderUnavailable, "read stream: empty body"));
      return;
    }
    const reader = new Reader(resp.body);

    // Tool-call arguments arrive in fragments; the call is only reported once
    // it is complete, which keeps the consumer from having to reassemble JSON.
    let pending: (ToolCall | null)[] = [];

    try {
      for (;;) {
        if (ctx?.aborted) {
          yield errorEvent(ctx.reason ?? new Error("aborted"));
          return;
        }

        let event;
        try {
          event = await reader.next();
        } catch (err) {
          yield errorEvent(llmError(ErrProviderUnavailable, `read stream: ${errorText(err)}`));
          return;
        }

        if (event === null) {
          // The provider closed the stream. Anything still being assembled is
          // reported before the terminal event, so a truncated tool call is
          // never silently dropped.
          yield* flushToolCalls(pending);
          yield doneEvent("stop");
          return;
        }

        if (isDone(event.data)) {
          yield* flushToolCalls(pending);
          yield doneEvent("stop");
          return;
        }
        if (event.data === "") continue;

        let chunk: streamChunk;
        try {
          chunk = JSON.parse(event.data) as streamChunk;
        } catch (err) {
          // A provider that sends something unparsable is a provider failure,
          // not a reason to hang.
          yield errorEvent(llmError(ErrProviderUnavailable, `decode stream event: ${errorText(err)}`));
          return;
        }
        if (chunk.error) {
          yield errorEvent(wireErrorToError(chunk.error));
          return;
        }

        if (chunk.usage) {
          yield { ...emptyEvent(), type: "usage", usage: wireUsageToUsage(chunk.usage) };
        }

        for (const choice of chunk.choices ?? []) {
          if (choice.delta.content !== "") {
            yield { ...emptyEvent(), type: "text", text: choice.delta.content };
          }
          for (const call of choice.delta.tool_calls ?? []) {
            pending = mergeToolCall(pending, call);
          }
          if (choice.finish_reason !== "") {
            const reason = mapFinishReason(choice.finish_reason);
            yield* flushToolCalls(pending);
            yield doneEvent(reason);
            return;
          }
        }
      }
    } finally {
      await resp.body.cancel().catch(() => undefined);
    }
  }

  /** Performs the HTTP call and turns a failure into the shared error set. */
  private do(ctx: AbortSignal | undefined, body: string, stream: boolean): Promise<Response> {
    return this.post(ctx, "/v1/chat/completions", body, stream);
  }

  /**
   * Sends one body to one route of the provider's API. The path is a parameter
   * because this wire format serves completions and embeddings from the same
   * host with the same headers.
   */
  async post(ctx: AbortSignal | undefined, path: string, body: string, stream: boolean): Promise<Response> {
    const url = this.cfg.baseUrl + path;
    const headers: Record<string, string> = {
      "Content-Type": "application/json",
      Accept: stream ? "text/event-stream" : "application/json",
    };
    if (this.cfg.apiKey !== "") headers.Authorization = "Bearer " + this.cfg.apiKey;
    for (const [key, value] of Object.entries(this.cfg.headers)) headers[key] = value;

    let resp: Response;
    try {
      resp = await fetch(url, {
        method: "POST",
        headers,
        body,
        signal: combineSignals(ctx, this.cfg.timeout),
      });
    } catch (err) {
      throw llmError(ErrProviderUnavailable, errorText(err));
    }

    if (resp.status >= 200 && resp.status < 300) return resp;

    if (resp.status === 429) {
      throw await rateLimitError(resp);
    }
    if (resp.status === 413 || resp.status === 400) {
      const message = await errorBody(resp);
      // A provider that names the picture is not complaining about the request:
      // it is saying this model cannot read one, which is a capability the
      // chain lacks. A 413 is a bound, and stays a malformed request.
      if (resp.status === 400 && refused(message)) {
        throw statusError(resp, message, ErrUnsupported);
      }
      throw statusError(resp, message, ErrInvalidRequest);
    }
    if (resp.status >= 500) {
      throw statusError(resp, await errorBody(resp), ErrProviderUnavailable);
    }
    throw statusError(resp, await errorBody(resp), ErrProviderUnavailable);
  }
}

/** Appends a streamed fragment to the call it belongs to. */
function mergeToolCall(pending: (ToolCall | null)[], fragment: streamToolCall): (ToolCall | null)[] {
  while (pending.length <= fragment.index) pending.push(null);

  let call = pending[fragment.index];
  if (call === null || call === undefined) {
    call = { id: "", name: "", arguments: "" };
    pending[fragment.index] = call;
  }

  if (fragment.id !== "") call.id = fragment.id;
  if (fragment.function.name !== "") call.name = fragment.function.name;
  if (fragment.function.arguments !== "") {
    call.arguments = (typeof call.arguments === "string" ? call.arguments : "") + fragment.function.arguments;
  }

  return pending;
}

/** Reports every assembled call, dropping the ones with no name. */
function* flushToolCalls(pending: (ToolCall | null)[]): Iterable<StreamEvent> {
  for (const call of pending) {
    if (call === null || call === undefined || call.name === "") continue;
    let args: unknown = {};
    const raw = typeof call.arguments === "string" ? call.arguments : "";
    if (raw !== "") {
      try {
        args = JSON.parse(raw);
      } catch {
        args = raw;
      }
    }
    yield { ...emptyEvent(), type: "tool_call", toolCall: { ...call, arguments: args } };
  }
}

function emptyEvent(): StreamEvent {
  return { type: "text", text: "", toolCall: null, usage: null, finishReason: "unknown", err: null };
}

function doneEvent(reason: FinishReason): StreamEvent {
  return { ...emptyEvent(), type: "done", finishReason: reason };
}

function errorEvent(err: unknown): StreamEvent {
  return { ...emptyEvent(), type: "error", err };
}

/** Carries the provider's retry hint when it sends one. */
async function rateLimitError(resp: Response): Promise<Error> {
  const message = await errorBody(resp);
  const seconds = resp.headers.get("Retry-After");
  if (seconds !== null && seconds !== "" && /^\d+$/.test(seconds)) {
    return llmError(ErrRateLimited, `${message} (retry after ${seconds}s)`);
  }
  return llmError(ErrRateLimited, message);
}

function statusError(resp: Response, message: string, sentinel: typeof ErrInvalidRequest): Error {
  const host = safeHost(resp.url);
  return llmError(sentinel, `${host} returned ${resp.status}: ${message}`);
}

/** Reads the provider's message, which is what says whether a refusal was about the picture. */
async function errorBody(resp: Response): Promise<string> {
  let body: string;
  try {
    body = await readLimited(resp, 4 << 10);
  } catch {
    return "unreadable response";
  }

  try {
    const payload = JSON.parse(body) as { error?: { message?: string; code?: string } | null };
    if (payload.error) {
      const code = payload.error.code ?? "";
      const message = payload.error.message ?? "";
      if (code !== "") return code + ": " + message;
      return message;
    }
  } catch {
    // Not JSON: fall through to the raw text.
  }

  const text = body.trim();
  return text.length > 300 ? text.slice(0, 300) + "..." : text;
}

/** Reads at most `limit` bytes of a response as text. */
export async function readLimited(resp: Response, limit: number): Promise<string> {
  if (!resp.body) return "";
  const reader = resp.body.getReader();
  const decoder = new TextDecoder();
  let out = "";
  let total = 0;
  try {
    for (;;) {
      const step = await reader.read();
      if (step.done) break;
      const size = step.value.byteLength;
      total += size;
      if (total > limit) {
        out += decoder.decode(step.value.subarray(0, size - (total - limit)), { stream: true });
        break;
      }
      out += decoder.decode(step.value, { stream: true });
    }
  } finally {
    await reader.cancel().catch(() => undefined);
  }
  return out + decoder.decode();
}

function safeHost(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

/** Combines the caller's cancellation with a timeout, as http.Client.Timeout does. */
export function combineSignals(ctx: AbortSignal | undefined, timeoutMillis: number): AbortSignal {
  const timeout = AbortSignal.timeout(timeoutMillis);
  return ctx ? AbortSignal.any([ctx, timeout]) : timeout;
}

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export type { wireError, Usage };
