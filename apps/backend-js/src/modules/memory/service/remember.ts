/**
 * What a message asks to be remembered.
 *
 * Ported 1:1 from internal/modules/memory/service/remember.go.
 */

import {
  ActionCreated,
  ErrInvalidInput,
  LayerMain,
  LayerPrivate,
  SourceUser,
  memoryLayer,
  scopeIsZero,
  wrap,
  type Memory,
  type RememberRequest,
  type RememberResult,
  type Scope,
} from "../domain/memory.ts";
import { runeLength } from "./chunk.ts";
import { classifyKind, classifyLayer, rememberIntent, sensitive, sensitiveSentence } from "./filter.ts";
import { oneLine } from "./recall.ts";
import type { Service } from "./service.ts";

/**
 * Stores what a message asked to be remembered and answers with a confirmation
 * that names it.
 *
 * The answer is written here rather than by the model so the confirmation and
 * the row cannot disagree: the sentence the user reads is built from the text
 * that was actually stored. A message that is not a request is left alone, and
 * the ordinary reply handles it.
 */
export async function rememberMessage(
  s: Service,
  scope: Scope,
  request: RememberRequest,
): Promise<RememberResult> {
  if (scopeIsZero(scope)) {
    throw wrap(ErrInvalidInput, "workspace is required");
  }

  const [content, ok] = rememberIntent(request.text);
  if (!ok) {
    return { handled: false, stored: null, refused: false, confirmation: "" };
  }

  if (runeLength(content) > s.limits.maxContentRunes) {
    return {
      handled: true,
      stored: null,
      refused: true,
      confirmation:
        "Itu terlalu panjang untuk kuingat. Ringkas jadi satu atau dua kalimat ya, lalu tulis lagi.",
    };
  }

  const [reason, isSensitive] = sensitive(content);
  if (isSensitive) {
    return {
      handled: true,
      stored: null,
      refused: true,
      confirmation: sensitiveSentence(reason),
    };
  }

  // The kind is read from the whole message, because the marker itself carries
  // the instruction: "mulai sekarang" is what makes the rest an instruction.
  const kind = classifyKind(request.text.toLowerCase());

  let agentId = request.agentId;
  if (classifyLayer(content) === LayerMain) {
    agentId = "";
  }

  let vectors: number[][];
  try {
    vectors = await s.embed(scope, agentId, "", [content]);
  } catch (err) {
    s.deps.logger?.warn(
      { err: err instanceof Error ? err.message : String(err) },
      "a memory request could not be embedded",
    );
    return {
      handled: true,
      stored: null,
      refused: false,
      confirmation:
        "Maaf, aku belum bisa mengingatnya sekarang: penyimpanan memori sedang tidak tersedia. Coba lagi sebentar lagi ya.",
    };
  }

  const stored = await s.deps.repository.createMemory(scope, {
    agentId,
    content,
    kind,
    sourceMessageId: request.messageId,
    pinned: false,
    createdBy: SourceUser,
    embedding: vectors[0],
  });

  await s.audit(scope, {
    action: ActionCreated,
    objectType: "memory",
    objectId: stored.id,
    actorUserId: scope.userId ?? "",
    actorAgentId: "",
    metadata: {
      layer: memoryLayer(stored),
      kind: stored.kind,
      source: "message",
    },
  });

  return {
    handled: true,
    stored,
    refused: false,
    confirmation: confirmationFor(stored),
  };
}

/**
 * Names what was stored and where it lives, which is what makes the answer
 * checkable against the memory screen.
 */
function confirmationFor(memory: Memory): string {
  const where =
    memoryLayer(memory) === LayerPrivate ? "di memori pribadiku" : "di memori utama workspace";

  return `Siap, aku ingat ${where}: "${oneLine(memory.content)}"`;
}
