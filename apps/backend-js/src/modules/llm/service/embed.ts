/**
 * Embeddings, walking the same fallback chain a completion does.
 *
 * Ported from internal/modules/llm/service/embed.go.
 */

import type { ProviderConfig, Scope } from "../domain/gateway.ts";
import type { EmbedRequest, EmbedResponse } from "../domain/provider.ts";
import { ErrInvalidRequest, ErrNoProvider, ErrUnsupported, llmError } from "../domain/provider.ts";
import { tryNext } from "./gateway.ts";
import type { Service } from "./service.ts";

/**
 * Turns text into vectors, skipping the providers that have no embedding
 * endpoint at all.
 *
 * The rule is a step stricter than Chat's: a provider that advertises no
 * embeddings is passed over without being called, because asking a completion
 * API for a vector is a request that cannot succeed. If nothing in the chain
 * can embed, the caller is told so — a configuration fact, not something a
 * retry cures.
 */
export async function embed(
  s: Service,
  ctx: AbortSignal | undefined,
  scope: Scope,
  req: EmbedRequest,
): Promise<EmbedResponse> {
  if (req.inputs.length === 0) {
    throw llmError(ErrInvalidRequest, "at least one input is required");
  }

  await s.check(ctx, scope.workspaceId);

  const span = s.embedSpan(scope, req);

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
    if (!provider.capabilities().embeddings) {
      lastErr = new Error(`llm: ${cfg.name} tidak menyediakan embedding: ${ErrUnsupported.message}`);
      continue;
    }

    // The embedding model is a deployment fact, not something a workspace
    // configures per provider: the key is the workspace's, the model is the one
    // whose vectors fit the schema.
    const call: EmbedRequest = {
      ...req,
      model: firstNonEmpty(req.model, s.deps.embeddingModel, cfg.model),
    };

    let response: EmbedResponse;
    try {
      response = await provider.embed(ctx, call);
    } catch (err) {
      lastErr = new Error(`llm: ${cfg.name} (${call.model}): ${errorText(err)}`);
      if (tryNext(err)) continue;
      span.fail(attempts, lastErr);
      throw lastErr;
    }

    response.provider = cfg.adapter;
    if (!response.model) response.model = call.model;

    try {
      await s.record(
        ctx,
        scope,
        cfg,
        {
          text: "",
          toolCalls: [],
          finishReason: "unknown",
          usage: response.usage,
          model: response.model,
          provider: "",
          rawFinishReason: "",
        },
        req.metadata,
      );
    } catch (err) {
      span.fail(attempts, err);
      throw err;
    }

    s.metric(ctx, cfg.adapter, req.metadata, attempts, performance.now() - started);
    span.finish(
      {
        text: "",
        toolCalls: [],
        finishReason: "unknown",
        usage: response.usage,
        model: response.model,
        provider: response.provider,
        rawFinishReason: "",
      },
      attempts,
    );

    return response;
  }

  if (lastErr === null) lastErr = ErrNoProvider;
  span.fail(attempts, lastErr);
  throw lastErr;
}

/** The first value that carries something. */
function firstNonEmpty(...values: string[]): string {
  return values.find((value) => value !== "") ?? "";
}

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
