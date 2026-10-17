/**
 * Embeddings for the Messages API.
 *
 * Ported from internal/platform/llm/anthropic/embed.go. It is honest rather
 * than convenient: the Messages API has no route that returns a vector, so a
 * deployment that wants memory retrieval points an [OI]-compatible provider at
 * the workspace.
 */

import type { EmbedResponse } from "../registry.ts";
import { ErrUnsupported, llmError } from "../registry.ts";
import type { Client } from "./client.ts";

/** Reports that this API has no embedding endpoint. */
export function embedUnsupported(c: Client): Promise<EmbedResponse> {
  return Promise.reject(llmError(ErrUnsupported, `${c.name()} has no embedding endpoint`));
}
