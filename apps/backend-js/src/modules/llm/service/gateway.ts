/**
 * One completion, and its streaming form.
 *
 * Ported from internal/modules/llm/service/gateway.go.
 */

import type { ProviderConfig, Scope } from "../domain/gateway.ts";
import type { ChatRequest, ChatResponse, StreamEvent, Usage } from "../domain/provider.ts";
import {
  ErrContextTooLong,
  ErrProviderUnavailable,
  ErrUnsupported,
  ErrUsageNotRecorded,
  llmError,
  PurposeAgent,
  retryable,
  supports,
} from "../domain/provider.ts";
import type { ChatSpan } from "./tracing.ts";
import type { Service } from "./service.ts";

/**
 * Runs one completion, falling back to the next provider when the one it chose
 * fails in a way another might survive.
 *
 * The rule is deliberately narrow: a rate limit, an outage, a provider-side
 * quota, or a model that cannot do what the request asks moves to the next
 * provider. A malformed request fails everywhere, so it returns immediately
 * rather than burning the whole chain.
 */
export async function chat(
  s: Service,
  ctx: AbortSignal | undefined,
  scope: Scope,
  req: ChatRequest,
): Promise<ChatResponse> {
  await s.check(ctx, scope.workspaceId);

  const span = s.chatSpan("llm.chat", scope, req);

  let chain: ProviderConfig[];
  try {
    chain = await s.resolve(ctx, scope);
  } catch (err) {
    span.fail(0, err);
    throw err;
  }

  let lastErr: unknown = null;
  let attempts = 0;
  const started = performance.now();

  for (const cfg of chain) {
    attempts++;
    const provider = s.deps.factory.build(cfg);

    const unsupported = supports(provider.capabilities(), req);
    if (unsupported) {
      lastErr = new Error(`llm: ${cfg.name} tidak bisa melayani permintaan: ${unsupported.message}`);
      if (tryNext(unsupported)) continue;
      span.fail(attempts, lastErr);
      throw lastErr;
    }

    let response: ChatResponse;
    try {
      response = await provider.chat(ctx, req);
    } catch (err) {
      lastErr = new Error(`llm: ${cfg.name} (${cfg.model}): ${errorText(err)}`);
      if (tryNext(err)) continue;
      span.fail(attempts, lastErr);
      throw lastErr;
    }

    // The provider that actually answered is what the ledger stores, so a
    // fallback is visible in the cost report rather than invisible.
    response.provider = cfg.adapter;
    if (!response.model) response.model = cfg.model;

    try {
      await record(s, ctx, scope, cfg, response, req.metadata);
    } catch (err) {
      span.fail(attempts, err);
      throw err;
    }

    s.metric(ctx, cfg.adapter, req.metadata, attempts, performance.now() - started);
    span.finish(response, attempts);

    return response;
  }

  span.fail(attempts, lastErr);
  throw lastErr;
}

/**
 * Runs one completion and reports its progress.
 *
 * Falling back only happens before the first byte: once the caller has seen a
 * fragment of an answer, restarting on another provider would duplicate it, so a
 * later failure is reported as an error event and the caller decides.
 */
export async function stream(
  s: Service,
  ctx: AbortSignal | undefined,
  scope: Scope,
  req: ChatRequest,
): Promise<AsyncIterable<StreamEvent>> {
  await s.check(ctx, scope.workspaceId);

  const span = s.chatSpan("llm.chat.stream", scope, req);

  let chain: ProviderConfig[];
  try {
    chain = await s.resolve(ctx, scope);
  } catch (err) {
    span.fail(0, err);
    throw err;
  }

  let lastErr: unknown = null;
  let attempts = 0;
  const started = performance.now();

  for (const cfg of chain) {
    attempts++;

    const provider = s.deps.factory.build(cfg);

    const unsupported = supports(provider.capabilities(), req);
    if (unsupported) {
      lastErr = new Error(`llm: ${cfg.name} tidak bisa melayani permintaan: ${unsupported.message}`);
      if (tryNext(unsupported)) continue;
      span.fail(attempts, lastErr);
      throw lastErr;
    }

    let events: AsyncIterable<StreamEvent>;
    try {
      events = await provider.stream(ctx, req);
    } catch (err) {
      lastErr = new Error(`llm: ${cfg.name} (${cfg.model}): ${errorText(err)}`);
      if (tryNext(err)) continue;
      span.fail(attempts, lastErr);
      throw lastErr;
    }

    const iterator = events[Symbol.asyncIterator]();

    // The first event is the last chance to switch providers: an error before
    // it means the provider never started answering.
    const first = await iterator.next();
    if (first.done) {
      lastErr = new Error(
        `llm: ${cfg.name} closed the stream without an event: ${ErrProviderUnavailable.message}`,
      );
      continue;
    }
    if (first.value.type === "error" && tryNext(first.value.err)) {
      lastErr = new Error(`llm: ${cfg.name} (${cfg.model}): ${errorText(first.value.err)}`);
      continue;
    }

    // The call is counted here rather than in the pump: what this measures is
    // how long the provider took to *start* answering.
    s.metric(ctx, cfg.adapter, req.metadata, attempts, performance.now() - started);

    return pump(s, ctx, iterator, first.value, scope, cfg, req.metadata, span, attempts);
  }

  span.fail(attempts, lastErr);
  throw lastErr;
}

/**
 * Forwards one provider's events and records the cost when the answer
 * finishes.
 *
 * The record is written before the terminal event is handed over, so a caller
 * that sees a finished answer is looking at a stream whose cost is already in
 * the ledger. When the write fails the stream ends with an error instead of a
 * success: a call that cannot be billed is not one to report as done.
 */
async function* pump(
  s: Service,
  ctx: AbortSignal | undefined,
  iterator: AsyncIterator<StreamEvent>,
  first: StreamEvent,
  scope: Scope,
  cfg: ProviderConfig,
  meta: ChatRequest["metadata"],
  span: ChatSpan,
  attempts: number,
): AsyncGenerator<StreamEvent> {
  let usage: Usage = { inputTokens: 0, outputTokens: 0, cacheReadTokens: 0, cacheWriteTokens: 0 };
  let iteratorDone = false;

  try {
    let event: StreamEvent | undefined = first;
    while (event) {
      if (ctx?.aborted) return;

      if (event.type === "usage" && event.usage) {
        // The counts arrive before the terminal event, so the ledger row can
        // carry them.
        usage = event.usage;
      }

      if (event.type === "done" || event.type === "error") {
        try {
          await record(s, ctx, scope, cfg, response(cfg, usage), meta);
        } catch (err) {
          if (!ctx?.aborted) yield { ...emptyEvent, type: "error", err };
          return;
        }
        if (!ctx?.aborted) yield event;
        return;
      }

      yield event;

      if (ctx?.aborted) return;
      const next = await iterator.next();
      if (next.done) {
        iteratorDone = true;
        break;
      }
      event = next.value;
    }

    // The channel closed without a terminal event. Reporting an error is the
    // honest answer: the caller must not treat a truncated answer as complete.
    // The row is still written, with whatever counts were reported.
    try {
      await record(s, ctx, scope, cfg, response(cfg, usage), meta);
    } catch (err) {
      if (!ctx?.aborted) yield { ...emptyEvent, type: "error", err };
      return;
    }
    if (!ctx?.aborted) {
      yield {
        ...emptyEvent,
        type: "error",
        err: new Error(
          `llm: ${cfg.name} closed the stream without a final event: ${ErrProviderUnavailable.message}`,
        ),
      };
    }
  } finally {
    try {
      if (!iteratorDone) await iterator.return?.();
    } finally {
      span.finish(response(cfg, usage), attempts);
    }
  }
}

/** Names the provider and the model that answered. */
function response(cfg: ProviderConfig, usage: Usage): ChatResponse {
  return {
    text: "",
    toolCalls: [],
    finishReason: "unknown",
    usage,
    model: cfg.model,
    provider: cfg.adapter,
    rawFinishReason: "",
  };
}

/**
 * Writes one usage row and moves the live counter.
 *
 * The counter is only a cache in front of the ledger, so a failed increment is
 * not an error: the row above is the record, and the quota check recomputes
 * from the ledger when the counter is unavailable.
 */
export async function record(
  s: Service,
  ctx: AbortSignal | undefined,
  scope: Scope,
  cfg: ProviderConfig,
  chatResponse: ChatResponse,
  meta: ChatRequest["metadata"],
): Promise<void> {
  if (!s.deps.recorder) {
    throw llmError(ErrUsageNotRecorded, "no usage recorder is configured");
  }

  const purpose = meta.purpose || PurposeAgent;

  const entry = {
    workspaceId: scope.workspaceId,
    agentId: scope.agentId,
    taskId: meta.taskId,
    provider: cfg.adapter,
    model: chatResponse.model,
    purpose,
    usage: chatResponse.usage,
    costMicros: s.cost(chatResponse.model, chatResponse.usage),
    createdAt: s.now(),
  };

  try {
    await s.deps.recorder.record(ctx, entry);
  } catch (err) {
    throw llmError(ErrUsageNotRecorded, errorText(err));
  }

  if (s.deps.counter) {
    // The counter is a cache; a failed increment must not fail the call.
    await s.deps.counter
      .add(ctx, scope.workspaceId, chatResponse.usage.inputTokens + chatResponse.usage.outputTokens)
      .catch(() => {});
    // The day counter is moved with the same row, so the two windows cannot
    // disagree about what was spent.
    await s.deps.counter.addCost(ctx, scope.workspaceId, entry.costMicros).catch(() => {});
  }
}

const emptyEvent: StreamEvent = {
  type: "text",
  text: "",
  toolCall: null,
  usage: null,
  finishReason: "unknown",
  err: null,
};

/**
 * Reports whether the next provider in the chain is worth trying. A malformed
 * request or a missing configuration fails everywhere; a rate limit, an outage,
 * or a capability the model lacks may not.
 */
export function tryNext(err: unknown): boolean {
  return retryable(err) || isCode(err, ErrUnsupported) || isCode(err, ErrContextTooLong);
}

function isCode(err: unknown, sentinel: { code: string }): boolean {
  return (
    err instanceof Error && "code" in err && typeof err.code === "string" && err.code === sentinel.code
  );
}

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
