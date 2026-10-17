/**
 * The embeddings route of the [OI]-compatible wire format.
 *
 * Ported from internal/platform/llm/openai/embed.go.
 */

import type { EmbedRequest, EmbedResponse, Usage } from "../registry.ts";
import {
  ErrInvalidRequest,
  ErrProviderUnavailable,
  ErrUnsupported,
  llmError,
} from "../registry.ts";
import type { Client } from "./client.ts";
import { readLimited } from "./client.ts";
import type { wireError } from "./wire.ts";
import { wireErrorToError } from "./wire.ts";

/** The answer of the embeddings route. */
interface embeddingResponse {
  model: string;
  data: embeddingData[];
  usage: { prompt_tokens: number; total_tokens: number } | null;
  error: wireError | null;
}

interface embeddingData {
  index: number;
  embedding: number[];
}

/**
 * Turns text into vectors.
 *
 * The inputs travel as one request rather than one per string: embedding a
 * document chunk by chunk costs a round trip each and reports the same cost,
 * and the answer is what the caller indexes.
 */
export async function embed(
  c: Client,
  ctx: AbortSignal | undefined,
  req: EmbedRequest,
): Promise<EmbedResponse> {
  if (!c.capabilities().embeddings) {
    throw llmError(ErrUnsupported, `${c.name()} has no embedding route`);
  }
  if (req.inputs.length === 0) {
    throw llmError(ErrInvalidRequest, "at least one input is required");
  }

  const model = req.model !== "" ? req.model : c.model();

  let body: string;
  try {
    body = JSON.stringify({ model, input: req.inputs });
  } catch (err) {
    throw llmError(ErrInvalidRequest, `encode request: ${err instanceof Error ? err.message : String(err)}`);
  }

  const resp = await c.post(ctx, "/v1/embeddings", body, false);

  let raw: string;
  try {
    raw = await readLimited(resp, 64 << 20);
  } catch (err) {
    throw llmError(ErrProviderUnavailable, `read response: ${err instanceof Error ? err.message : String(err)}`);
  }

  let payload: embeddingResponse;
  try {
    payload = JSON.parse(raw) as embeddingResponse;
  } catch (err) {
    throw llmError(ErrProviderUnavailable, `decode response: ${err instanceof Error ? err.message : String(err)}`);
  }
  if (payload.error) throw wireErrorToError(payload.error);

  const data = payload.data ?? [];
  // A count that does not match the inputs would silently pair a vector with
  // the wrong text, which is worse than a failed call.
  if (data.length !== req.inputs.length) {
    throw llmError(
      ErrProviderUnavailable,
      `provider returned ${data.length} vectors for ${req.inputs.length} inputs`,
    );
  }

  // The index is what pairs a vector with its input, so the order is restored
  // from it rather than assumed.
  const sorted = [...data].sort((a, b) => a.index - b.index);

  const vectors: number[][] = [];
  for (const item of sorted) {
    if (item.index < 0 || item.index >= req.inputs.length) {
      throw llmError(
        ErrProviderUnavailable,
        `provider returned index ${item.index} for ${req.inputs.length} inputs`,
      );
    }
    vectors.push(item.embedding);
  }

  const usage: Usage = {
    inputTokens: payload.usage ? payload.usage.prompt_tokens : 0,
    outputTokens: 0,
    cacheReadTokens: 0,
    cacheWriteTokens: 0,
  };

  return {
    vectors,
    model: payload.model !== "" ? payload.model : model,
    provider: c.name(),
    usage,
  };
}
