/**
 * The model calls' spans.
 *
 * Ported from internal/modules/llm/service/tracing.go. With no OpenTelemetry
 * SDK installed the span is a no-op object that still yields a trace id, but
 * the attributes and call sites are the same: which workspace and which Bolu,
 * which provider and which model, how many tokens, whether a fallback happened,
 * and how many tool calls came back. **The prompt and the answer are never
 * attributes.**
 */

import type { Scope, ProviderConfig } from "../domain/gateway.ts";
import type { ChatRequest, ChatResponse, EmbedRequest } from "../domain/provider.ts";
import { estimateTokens } from "../domain/provider.ts";

/** One span's mutable state. */
export interface ChatSpan {
  /** The correlation id of the span; always present, even with telemetry off. */
  readonly traceId: string;
  /** Records who answered and what it cost, and closes it. */
  finish(response: ChatResponse, attempts: number): void;
  /** Records why a completion failed and closes it. */
  fail(attempts: number, err: unknown): void;
}

/** Starts the spans of a model call. The container supplies it. */
export interface Tracer {
  start(
    name: string,
    attributes: Record<string, string | number>,
  ): { traceId: string; end: (attributes: Record<string, string | number>) => void };
}

/** A span that records nothing, which is what a deployment with telemetry off runs. */
function noopSpan(name: string, attributes: Record<string, string | number>): ChatSpan {
  const traceId = crypto.randomUUID().replace(/-/g, "");
  let closed = false;

  const close = (extra: Record<string, string | number>): void => {
    if (closed) return;
    closed = true;
    // Nothing to send; the guard keeps a double end from being observable.
    void extra;
    void attributes;
    void name;
  };

  return {
    traceId,
    finish(response, attempts) {
      close({
        provider: response.provider,
        model: response.model,
        input_tokens: response.usage.inputTokens,
        output_tokens: response.usage.outputTokens,
        tool_calls: response.toolCalls.length,
        finish_reason: response.finishReason,
        attempts,
      });
    },
    fail(attempts, err) {
      void err;
      close({ attempts });
    },
  };
}

/** Opens the span for one completion, or the no-op span when telemetry is not wired. */
export function chatSpan(
  tracer: Tracer | null,
  name: string,
  scope: Scope,
  req: ChatRequest,
): ChatSpan {
  const attributes: Record<string, string | number> = {
    workspace_id: scope.workspaceId,
    message_count: req.messages.length,
    tool_count: req.tools.length,
    // How big the request was, in the unit the bill is in.
    prompt_tokens_estimate: estimateTokens(req),
    max_tokens: req.maxTokens,
  };
  if (scope.agentId) attributes.agent_id = scope.agentId;

  if (!tracer) return noopSpan(name, attributes);

  const started = tracer.start(name, attributes);
  let closed = false;
  const close = (extra: Record<string, string | number>): void => {
    if (closed) return;
    closed = true;
    started.end(extra);
  };

  return {
    traceId: started.traceId,
    finish(response, attempts) {
      close({
        provider: response.provider,
        model: response.model,
        input_tokens: response.usage.inputTokens,
        output_tokens: response.usage.outputTokens,
        tool_calls: response.toolCalls.length,
        finish_reason: response.finishReason,
        attempts,
      });
    },
    fail(attempts, err) {
      if (err) close({ attempts, error: err instanceof Error ? err.message : String(err) });
      else close({ attempts });
    },
  };
}

/** Opens the span for one embedding call. */
export function embedSpan(
  tracer: Tracer | null,
  scope: Scope,
  req: EmbedRequest,
): ChatSpan {
  const attributes: Record<string, string | number> = {
    workspace_id: scope.workspaceId,
    inputs: req.inputs.length,
  };
  if (scope.agentId) attributes.agent_id = scope.agentId;

  if (!tracer) return noopSpan("llm.embed", attributes);

  const started = tracer.start("llm.embed", attributes);
  let closed = false;
  const close = (extra: Record<string, string | number>): void => {
    if (closed) return;
    closed = true;
    started.end(extra);
  };

  return {
    traceId: started.traceId,
    finish(response, attempts) {
      close({
        provider: response.provider,
        model: response.model,
        input_tokens: response.usage.inputTokens,
        output_tokens: response.usage.outputTokens,
        tool_calls: response.toolCalls.length,
        finish_reason: response.finishReason,
        attempts,
      });
    },
    fail(attempts, err) {
      if (err) close({ attempts, error: err instanceof Error ? err.message : String(err) });
      else close({ attempts });
    },
  };
}

/**
 * Reports one call that reached a provider. The purpose comes from the
 * request's own metadata rather than from the call site.
 */
export function metric(
  metrics: import("../domain/provider.ts").Metrics | null,
  ctx: AbortSignal | undefined,
  provider: string,
  meta: ChatRequest["metadata"],
  attempts: number,
  elapsedMs: number,
): void {
  if (!metrics) return;
  const purpose = meta.purpose || "agent";
  metrics.modelCall(ctx, provider, purpose, attempts > 1, elapsedMs / 1000);
}

/** The provider a resolved configuration builds, named for the trace. */
export function configLabel(cfg: ProviderConfig): string {
  return `${cfg.adapter}/${cfg.model}`;
}
