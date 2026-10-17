/**
 * The Messages API wire format: POST {base_url}/v1/messages.
 *
 * Ported from internal/platform/llm/anthropic/client.go. Its shape differs from
 * the [OI]-compatible one in the ways that matter: the system prompt is a
 * top-level field, content is a list of typed blocks, tool calls are `tool_use`
 * blocks, and tool answers are `tool_result` blocks inside a user message.
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
} from "../registry.ts";
import {
  ErrInvalidRequest,
  ErrProviderUnavailable,
  ErrRateLimited,
  ErrUnsupported,
  llmError,
} from "../registry.ts";
import { Reader } from "../sse/sse.ts";
import { refused } from "../vision/vision.ts";
import { embedUnsupported } from "./embed.ts";
import {
  decodeContent,
  encodeRequest,
  mapStopReason,
  type messagesResponse,
  type streamEvent,
  type wireError,
  wireErrorToError,
  wireUsageToUsage,
} from "./wire.ts";

/** The public endpoint. */
export const DefaultBaseURL = "https://api.anthropic.com";

/** The header value Anthropic requires. */
export const APIVersion = "2023-06-01";

/** The fallback context window. */
export const DefaultContextTokens = 200000;

/** Required by the API; a request without one is rejected. */
export const DefaultMaxTokens = 4096;

/** One connection to an Anthropic-compatible server. */
export interface Config {
  baseUrl: string;
  apiKey: string;
  model: string;
  maxTokens: number;
  contextTokens: number;
  /** Enables cache breakpoints on the stable prefix. */
  promptCaching: boolean;
  supportsTools: boolean;
  supportsVision: boolean;
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
      maxTokens: cfg.maxTokens <= 0 ? DefaultMaxTokens : cfg.maxTokens,
      contextTokens: cfg.contextTokens <= 0 ? DefaultContextTokens : cfg.contextTokens,
      promptCaching: cfg.promptCaching,
      supportsTools: cfg.supportsTools,
      supportsVision: cfg.supportsVision,
      headers: cfg.headers ?? {},
      timeout: cfg.timeout !== undefined && cfg.timeout > 0 ? cfg.timeout : 5 * 60_000,
    };
  }

  /** The adapter name. */
  name(): string {
    return "anthropic";
  }

  /** The configured model. */
  model(): string {
    return this.cfg.model;
  }

  /** The configured answer cap, which a request may override. */
  configuredMaxTokens(): number {
    return this.cfg.maxTokens;
  }

  /** Whether cache breakpoints are emitted. */
  cachingPrompt(): boolean {
    return this.cfg.promptCaching;
  }

  /** What this configuration can do. */
  capabilities(): Capabilities {
    return {
      tools: this.cfg.supportsTools,
      vision: this.cfg.supportsVision,
      streaming: true,
      promptCaching: this.cfg.promptCaching,
      parallelToolCalls: true,
      // No embedding endpoint exists in this API at all.
      embeddings: false,
      maxContextTokens: this.cfg.contextTokens,
    };
  }

  /** One completion. */
  async chat(ctx: AbortSignal | undefined, req: ChatRequest): Promise<ChatResponse> {
    const body = JSON.stringify(encodeRequest(this, req, false));

    const resp = await this.do(ctx, body, false);
    const raw = await readLimited(resp, 32 << 20);

    let payload: messagesResponse;
    try {
      payload = JSON.parse(raw) as messagesResponse;
    } catch (err) {
      throw llmError(ErrProviderUnavailable, `decode response: ${errorText(err)}`);
    }
    if (payload.error) throw wireErrorToError(payload.error);

    const decoded = decodeContent(this, payload);
    return {
      text: decoded.text,
      toolCalls: decoded.toolCalls,
      finishReason: decoded.finishReason,
      rawFinishReason: decoded.rawFinishReason,
      model: decoded.model,
      provider: "anthropic",
      usage: decoded.usage ?? { inputTokens: 0, outputTokens: 0, cacheReadTokens: 0, cacheWriteTokens: 0 },
    };
  }

  /** One completion, reporting its progress. */
  async stream(ctx: AbortSignal | undefined, req: ChatRequest): Promise<AsyncIterable<StreamEvent>> {
    const body = JSON.stringify(encodeRequest(this, req, true));
    const resp = await this.do(ctx, body, true);
    return this.readStream(ctx, resp);
  }

  /** Reports that this API has no embedding endpoint. */
  embed(_ctx: AbortSignal | undefined, _req: EmbedRequest): Promise<EmbedResponse> {
    return embedUnsupported(this);
  }

  /** Normalises the Messages API events. */
  private async *readStream(ctx: AbortSignal | undefined, resp: Response): AsyncIterable<StreamEvent> {
    if (!resp.body) {
      yield errorEvent(llmError(ErrProviderUnavailable, "read stream: empty body"));
      return;
    }
    const reader = new Reader(resp.body);

    // The API streams a tool call in two parts: a content_block_start carrying
    // the name and id, then input_json_delta fragments carrying the arguments.
    // The call is only reported once the block closes.
    let openCall: ToolCall | null = null;
    let openIndex = 0;
    let open = false;

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
          if (open) {
            const flushed = flushCall(openCall);
            if (flushed) yield flushed;
          }
          yield doneEvent("stop");
          return;
        }
        if (event.data === "") continue;

        let payload: streamEvent;
        try {
          payload = JSON.parse(event.data) as streamEvent;
        } catch (err) {
          yield errorEvent(llmError(ErrProviderUnavailable, `decode stream event: ${errorText(err)}`));
          return;
        }
        if (payload.error) {
          yield errorEvent(wireErrorToError(payload.error));
          return;
        }

        // The event name arrives either as a `type` field or as the SSE event
        // name, depending on the producer.
        const name = payload.type !== "" ? payload.type : event.name;

        switch (name) {
          case "message_start":
            if (payload.message && payload.message.usage) {
              yield { ...emptyEvent(), type: "usage", usage: wireUsageToUsage(payload.message.usage) };
            }
            break;

          case "content_block_start":
            if (payload.content_block && payload.content_block.type === "tool_use") {
              openCall = { id: payload.content_block.id, name: payload.content_block.name, arguments: "" };
              openIndex = payload.index;
              open = true;
            }
            break;

          case "content_block_delta":
            if (!payload.delta) break;
            switch (payload.delta.type) {
              case "text_delta":
                if (payload.delta.text !== "") {
                  yield { ...emptyEvent(), type: "text", text: payload.delta.text };
                }
                break;
              case "input_json_delta":
                if (open && payload.index === openIndex && openCall) {
                  openCall.arguments =
                    (typeof openCall.arguments === "string" ? openCall.arguments : "") + payload.delta.partial_json;
                }
                break;
            }
            break;

          case "content_block_stop":
            if (open && payload.index === openIndex) {
              const flushed = flushCall(openCall);
              if (flushed) yield flushed;
              openCall = null;
              open = false;
            }
            break;

          case "message_delta":
            if (payload.usage) {
              yield { ...emptyEvent(), type: "usage", usage: wireUsageToUsage(payload.usage) };
            }
            if (payload.delta && payload.delta.stop_reason !== "") {
              yield doneEvent(mapStopReason(payload.delta.stop_reason));
              return;
            }
            break;

          case "message_stop":
            if (open) {
              const flushed = flushCall(openCall);
              if (flushed) yield flushed;
            }
            yield doneEvent("stop");
            return;

          case "ping":
            // Keep-alive.
            break;

          default:
            // An unknown event is ignored rather than fatal: the API adds new
            // ones over time and a stream should not break because of that.
            break;
        }
      }
    } finally {
      await resp.body.cancel().catch(() => undefined);
    }
  }

  /** Performs the HTTP call and normalises failures. */
  private async do(ctx: AbortSignal | undefined, body: string, stream: boolean): Promise<Response> {
    const url = this.cfg.baseUrl + "/v1/messages";
    const headers: Record<string, string> = {
      "Content-Type": "application/json",
      "anthropic-version": APIVersion,
      Accept: stream ? "text/event-stream" : "application/json",
    };
    if (this.cfg.apiKey !== "") headers["x-api-key"] = this.cfg.apiKey;
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
      const message = await errorBody(resp);
      const seconds = resp.headers.get("retry-after");
      if (seconds !== null && seconds !== "") {
        throw llmError(ErrRateLimited, `${message} (retry after ${seconds}s)`);
      }
      throw llmError(ErrRateLimited, message);
    }
    if (resp.status === 400 || resp.status === 422) {
      const message = await errorBody(resp);
      // A refusal that names the picture is a capability this model lacks
      // rather than a complaint about the request.
      if (refused(message)) throw statusError(resp, message, ErrUnsupported);
      throw statusError(resp, message, ErrInvalidRequest);
    }
    if (resp.status >= 500) {
      throw statusError(resp, await errorBody(resp), ErrProviderUnavailable);
    }
    throw statusError(resp, await errorBody(resp), ErrProviderUnavailable);
  }
}

/** Reports one assembled call, or null when it has no name. */
function flushCall(call: ToolCall | null): StreamEvent | null {
  if (call === null || call.name === "") return null;
  let args: unknown = {};
  const raw = typeof call.arguments === "string" ? call.arguments : "";
  if (raw !== "") {
    try {
      args = JSON.parse(raw);
    } catch {
      args = raw;
    }
  }
  return { ...emptyEvent(), type: "tool_call", toolCall: { ...call, arguments: args } };
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

async function errorBody(resp: Response): Promise<string> {
  let body: string;
  try {
    body = await readLimited(resp, 4 << 10);
  } catch {
    return "unreadable response";
  }

  try {
    const payload = JSON.parse(body) as { error?: { type?: string; message?: string } | null };
    if (payload.error) {
      return (payload.error.type ?? "") + ": " + (payload.error.message ?? "");
    }
  } catch {
    // Not JSON: fall through to the raw text.
  }

  const text = body.trim();
  return text.length > 300 ? text.slice(0, 300) + "..." : text;
}

function statusError(resp: Response, message: string, sentinel: typeof ErrInvalidRequest): Error {
  let host = resp.url;
  try {
    host = new URL(resp.url).host;
  } catch {
    // Keep the raw URL.
  }
  return llmError(sentinel, `${host} returned ${resp.status}: ${message}`);
}

/** Combines the caller's cancellation with a timeout, as http.Client.Timeout does. */
function combineSignals(ctx: AbortSignal | undefined, timeoutMillis: number): AbortSignal {
  const timeout = AbortSignal.timeout(timeoutMillis);
  return ctx ? AbortSignal.any([ctx, timeout]) : timeout;
}

/** Reads at most `limit` bytes of a response as text. */
async function readLimited(resp: Response, limit: number): Promise<string> {
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

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export type { wireError };
