/**
 * The side-effecting half of memory extraction: the method the worker
 * registers, which reads the thread through the service.
 *
 * It exists as a thin layer rather than as a temporal-aware service for two
 * reasons: the workflow package must stay free of the module's own services so
 * the replay stays deterministic, and the service must stay free of the SDK so a
 * unit test can call it without a server.
 *
 * Ported 1:1 from internal/modules/memory/activity/activity.go.
 */

import type { Logger } from "../../../platform/logger.ts";
import { ErrInvalidInput, parseUUID, wrap, type Scope } from "../domain/memory.ts";
import type { Service } from "../domain/service.ts";
import type { ExtractionInput, ExtractionResult } from "../workflow/workflow.ts";

/** The activity dependencies. */
export interface Deps {
  memories: Service | null;
  logger?: Logger | null;
}

/** The set the worker registers. */
export class Activities {
  constructor(private readonly deps: Deps) {}

  /**
   * Reads one quiet conversation and stores what it finds.
   *
   * It returns a result rather than an error for the states that are not
   * failures: a Bolu that switched extraction off, and a thread that ended up
   * with nothing worth keeping. An error is reserved for the cases a retry could
   * fix.
   */
  async ExtractConversation(input: ExtractionInput): Promise<ExtractionResult> {
    if (!this.deps.memories) {
      throw wrap(ErrInvalidInput, "the memory service is not configured");
    }

    const workspaceId = parseID(input.WorkspaceID, "workspace");
    const conversationId = parseID(input.ConversationID, "conversation");
    const agentId = parseID(input.AgentID, "bolu");

    const scope: Scope = { workspaceId };
    const result = await this.deps.memories.extract(scope, { conversationId, agentId });

    if (this.deps.logger && (result.stored > 0 || result.merged > 0)) {
      this.deps.logger.info(
        {
          conversation_id: conversationId,
          stored: result.stored,
          merged: result.merged,
          skipped: result.skipped,
          refused: result.refused,
        },
        "memory extracted",
      );
    }

    return {
      Disabled: result.disabled,
      Stored: result.stored,
      Merged: result.merged,
      Skipped: result.skipped,
      Refused: result.refused,
    };
  }
}

/**
 * Reads an id that travelled through the workflow as a string. An empty agent id
 * is allowed and means "no Bolu to check" — a conversation whose participants
 * are all gone still has memories worth keeping.
 */
function parseID(value: string, field: string): string {
  if (!value) {
    return "";
  }
  const parsed = parseUUID(value);
  if (!parsed) {
    throw wrap(ErrInvalidInput, `${field} id is not valid`);
  }
  return parsed;
}
