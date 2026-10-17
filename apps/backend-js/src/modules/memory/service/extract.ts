/**
 * The quiet-conversation extractor: reads a thread that has stopped moving and
 * writes what is worth keeping.
 *
 * Ported 1:1 from internal/modules/memory/service/extract.go.
 */

import { PurposeExtraction, RoleUser } from "../../llm/domain/provider.ts";
import {
  ActionCreated,
  ActionUpdated,
  ErrInvalidInput,
  ErrNoModel,
  LayerMain,
  LayerPrivate,
  Kinds,
  KindFact,
  SourceExtraction,
  isNilID,
  memoryLayer,
  scopeIsZero,
  utteranceFromAgent,
  wrap,
  type Extracted,
  type ExtractResult,
  type Scope,
  type Utterance,
} from "../domain/memory.ts";
import type { ExtractRequest } from "../domain/service.ts";
import { runeLength, trimRight, trimSpace } from "./chunk.ts";
import { sensitive } from "./filter.ts";
import { oneLine } from "./recall.ts";
import type { Service } from "./service.ts";

/** Bounds what one pass may write, so a model that answers with forty memories costs a bounded number of rows and embeddings. */
const maxCandidates = 12;

/**
 * How short a proposed memory may be. "oke" and "makasih" are conversation, not
 * memory, and a length floor is what catches them when the model ignores the
 * instruction to skip them.
 */
const minCandidateRunes = 12;

/**
 * The instruction the extractor runs under.
 *
 * It asks for JSON and names the two layers, because the model is the only
 * thing that can tell a general workspace fact from a role-specific one. It
 * also says what to ignore, which is the part that keeps a transcript from
 * becoming a log.
 */
const extractionSystem = `Kamu merawat memori sebuah usaha kecil. Baca transkrip percakapan dan ambil HANYA hal yang berguna di percakapan berikutnya:
- fakta tentang usaha (jam buka, harga, ongkir, alamat, kebijakan),
- preferensi pemilik (cara yang dia sukai),
- instruksi yang tahan lama ("mulai sekarang ...", "jangan ...").

Abaikan obrolan sesaat: sapaan, ucapan terima kasih, pertanyaan sekali pakai, dan hasil pekerjaan yang sudah selesai.

Balas HANYA JSON, tanpa penjelasan:
{"memories":[{"content":"satu kalimat berdiri sendiri","kind":"fact|preference|instruction","layer":"main|private"}]}

layer "main" untuk hal yang berlaku bagi seluruh workspace; layer "private" untuk hal yang hanya menyangkut cara kerja Bolu ini.
Tulis maksimal 5 memori, masing-masing di bawah 200 karakter. Kalau tidak ada yang perlu diingat, balas {"memories":[]}.`;

/**
 * Reads a quiet conversation and writes what is worth keeping.
 *
 * It is the activity the extraction workflow calls. Everything it writes passes
 * the same door a person's request does: the sensitive filter, an embedding,
 * and then the comparison against what is already remembered, which is what
 * keeps a thread read twice from producing two copies of one fact.
 */
export async function extractConversation(
  s: Service,
  scope: Scope,
  request: ExtractRequest,
): Promise<ExtractResult> {
  const empty: ExtractResult = { disabled: false, stored: 0, merged: 0, skipped: 0, refused: 0 };

  if (scopeIsZero(scope)) {
    throw wrap(ErrInvalidInput, "workspace is required");
  }
  if (isNilID(request.conversationId)) {
    throw wrap(ErrInvalidInput, "conversation id is required");
  }

  if (!isNilID(request.agentId)) {
    const enabled = await s.deps.repository.extractionEnabled(scope, request.agentId);
    if (!enabled) {
      return { ...empty, disabled: true };
    }
  }

  const utterances = await read(s, scope, request);
  if (utterances.length < 2) {
    // One message is not a conversation, and there is nothing to compare it
    // against.
    return empty;
  }

  const proposed = await propose(s, scope, request, utterances);

  return keep(s, scope, request, utterances, proposed);
}

/** Loads the transcript. */
async function read(
  s: Service,
  scope: Scope,
  request: ExtractRequest,
): Promise<Utterance[]> {
  if (!s.deps.threads) {
    throw wrap(ErrNoModel, "no conversation reader is configured");
  }

  return s.deps.threads.recent(scope, request.conversationId, s.limits.extractionMessages);
}

/** Asks the small model what is worth keeping. */
async function propose(
  s: Service,
  scope: Scope,
  request: ExtractRequest,
  utterances: Utterance[],
): Promise<Extracted[]> {
  if (!s.deps.model) {
    throw wrap(ErrNoModel, "no model is configured");
  }

  const response = await s.deps.model.chat(undefined, {
    userId: scope.userId ?? "",
    workspaceId: scope.workspaceId,
    agentId: isNilID(request.agentId) ? "" : request.agentId,
  }, {
    model: "",
    system: extractionSystem,
    retrieved: "",
    messages: [
      {
        role: RoleUser,
        text: transcript(utterances),
        images: [],
        toolCalls: [],
        toolResults: [],
      },
    ],
    tools: [],
    toolChoice: "auto",
    toolName: "",
    maxTokens: 800,
    temperature: 0,
    stopSequences: [],
    metadata: {
      workspaceId: scope.workspaceId,
      agentId: isNilID(request.agentId) ? "" : request.agentId,
      taskId: "",
      purpose: PurposeExtraction,
    },
  });

  return parseExtracted(response.text);
}

/** Stores what survived the filter, merging with what is already remembered. */
async function keep(
  s: Service,
  scope: Scope,
  request: ExtractRequest,
  utterances: Utterance[],
  proposed: Extracted[],
): Promise<ExtractResult> {
  const result: ExtractResult = { disabled: false, stored: 0, merged: 0, skipped: 0, refused: 0 };

  const candidates = validateCandidates(proposed, result);
  if (candidates.length === 0) {
    return result;
  }

  const contents = candidates.map((candidate) => candidate.content);

  let vectors: number[][];
  try {
    vectors = await s.embed(scope, request.agentId, "", contents);
  } catch (err) {
    // Nothing is written when the vectors are missing: a memory a search can
    // never find is worse than a memory that arrives on the next pass.
    throw err;
  }

  const source = lastUserMessage(utterances);

  for (let i = 0; i < candidates.length; i++) {
    const candidate = candidates[i];
    let agentId = "";
    if (candidate.layer === LayerPrivate) {
      agentId = request.agentId;
    }

    const nearest = await s.deps.repository.searchMemories(scope, {
      agentId,
      main: false,
      vector: vectors[i],
      limit: 1,
      minSimilarity: s.limits.similarity,
    });

    if (nearest.length > 0 && sameText(nearest[0].content, candidate.content)) {
      // The same fact, said again: what a second pass over one thread must not
      // produce.
      result.skipped++;
      continue;
    }

    const stored = await s.deps.repository.createMemory(scope, {
      agentId,
      content: candidate.content,
      kind: candidate.kind,
      sourceMessageId: source,
      pinned: false,
      createdBy: SourceExtraction,
      embedding: vectors[i],
    });

    if (nearest.length > 0) {
      // The old memory becomes history rather than a second opinion: two
      // versions of "ongkir Bandung" in one prompt is worse than one.
      await s.deps.repository.supersede(scope, nearest[0].id, stored.id);
      result.merged++;

      // The actor here is the workflow, not a person: nobody asked for this
      // line, and saying it was a person would make the trail lie about the one
      // thing it is read for. The source in the metadata is what says where it
      // came from.
      await s.audit(scope, {
        action: ActionUpdated,
        objectType: "memory",
        objectId: stored.id,
        actorUserId: "",
        actorAgentId: "",
        metadata: {
          layer: memoryLayer(stored),
          kind: stored.kind,
          superseded: nearest[0].id,
          source: "extraction",
        },
      });
      continue;
    }

    result.stored++;
    await s.audit(scope, {
      action: ActionCreated,
      objectType: "memory",
      objectId: stored.id,
      actorUserId: "",
      actorAgentId: "",
      metadata: {
        layer: memoryLayer(stored),
        kind: stored.kind,
        source: "extraction",
      },
    });
  }

  return result;
}

/**
 * Validates what the model proposed: known kinds, known layers, a length floor,
 * no secrets, and a bounded count.
 */
function validateCandidates(proposed: Extracted[], result: ExtractResult): Extracted[] {
  const kept: Extracted[] = [];

  for (const proposedCandidate of proposed) {
    const candidate: Extracted = { ...proposedCandidate };
    const content = oneLine(candidate.content);
    if (runeLength(content) < minCandidateRunes) {
      result.skipped++;
      continue;
    }

    if (sensitive(content)[1]) {
      result.refused++;
      continue;
    }

    if (!Kinds.includes(candidate.kind)) {
      candidate.kind = KindFact;
    }
    if (candidate.layer !== LayerPrivate) {
      candidate.layer = LayerMain;
    }
    candidate.content = sentence(content);

    kept.push(candidate);
    if (kept.length >= maxCandidates) {
      break;
    }
  }

  return kept;
}

/** Renders the thread the way the extractor reads it. */
export function transcript(utterances: Utterance[]): string {
  let out = "Transkrip percakapan:\n";

  for (const utterance of utterances) {
    const text = oneLine(utterance.text);
    if (text === "") {
      continue;
    }

    const speaker = utteranceFromAgent(utterance) ? "Bolu " + utterance.agentName : "Pengguna";

    out += speaker;
    out += ": ";
    out += text;
    out += "\n";
  }

  return out;
}

/**
 * Reads the model's answer.
 *
 * It accepts either a bare array or the object the prompt asks for, because a
 * small model occasionally drops the wrapper, and refusing that answer would
 * throw away a usable extraction over punctuation.
 */
export function parseExtracted(raw: string): Extracted[] {
  const body = stripFence(raw);

  // The wrapper is only read when the answer is an object: a bare array parsed
  // as an object would succeed with an empty list, because a JSON object with
  // none of the fields it names is still a valid object. That silent success is
  // the failure mode this ordering exists to avoid.
  if (body.startsWith("{")) {
    if (body.includes('"memories"')) {
      try {
        const parsed: unknown = JSON.parse(body);
        const wrapped = decodeMemories(parsed);
        if (wrapped) {
          return wrapped;
        }
      } catch {
        // Fall through to the array scan below.
      }
    }
  }

  // Anything else falls back to the outermost array, which also covers an
  // answer wrapped in prose the model added anyway.
  const start = body.indexOf("[");
  if (start >= 0) {
    const end = body.lastIndexOf("]");
    if (end > start) {
      try {
        const bare = decodeExtractedArray(JSON.parse(body.slice(start, end + 1)));
        if (bare) {
          return bare;
        }
      } catch {
        // No usable JSON.
      }
    }
  }

  throw wrap(ErrInvalidInput, "the extractor did not answer with JSON");
}

/** Reads `{"memories": [...]}` the way `json.Unmarshal` into the wrapper struct would. */
function decodeMemories(parsed: unknown): Extracted[] | null {
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    return null;
  }
  const memories = (parsed as Record<string, unknown>)["memories"];
  if (memories === undefined || memories === null) {
    return [];
  }
  return decodeExtractedArray(memories);
}

/** Reads an array of `Extracted`, refusing a field of the wrong type as `json.Unmarshal` does. */
function decodeExtractedArray(parsed: unknown): Extracted[] | null {
  if (!Array.isArray(parsed)) {
    return null;
  }

  const out: Extracted[] = [];
  for (const entry of parsed) {
    if (typeof entry !== "object" || entry === null || Array.isArray(entry)) {
      return null;
    }
    const record = entry as Record<string, unknown>;
    const content = record["content"];
    const kind = record["kind"];
    const layer = record["layer"];
    if (content !== undefined && content !== null && typeof content !== "string") return null;
    if (kind !== undefined && kind !== null && typeof kind !== "string") return null;
    if (layer !== undefined && layer !== null && typeof layer !== "string") return null;

    out.push({
      content: typeof content === "string" ? content : "",
      kind: typeof kind === "string" ? kind : "",
      layer: typeof layer === "string" ? layer : "",
    });
  }
  return out;
}

/** Removes a markdown code fence, which small models add even when the instruction says not to. */
export function stripFence(raw: string): string {
  let body = trimSpace(raw);

  if (!body.startsWith("```")) {
    return body;
  }

  body = body.slice(3);
  const index = body.indexOf("\n");
  if (index >= 0) {
    body = body.slice(index + 1);
  }

  const trimmed = trimSpace(body);
  return trimmed.endsWith("```") ? trimmed.slice(0, -3) : trimmed;
}

/** Keeps one line and ends it with a full stop when it has none, so a memory reads as a statement rather than as a fragment. */
function sentence(content: string): string {
  if (content.endsWith(".") || content.endsWith("!") || content.endsWith("?")) {
    return content;
  }
  return content + ".";
}

/**
 * Reports whether two memories say the same thing. The comparison is
 * case-insensitive and on collapsed whitespace, because the model writes the
 * same fact slightly differently every time and a trailing full stop is not a
 * new memory.
 */
function sameText(left: string, right: string): boolean {
  return (
    trimRight(oneLine(left), ".").toLowerCase() === trimRight(oneLine(right), ".").toLowerCase()
  );
}

/**
 * The message a proposed memory is attributed to. The model reads a whole
 * thread, so the newest thing the user said is the closest honest answer to
 * "where did this come from".
 */
function lastUserMessage(utterances: Utterance[]): string {
  for (let i = utterances.length - 1; i >= 0; i--) {
    if (!utteranceFromAgent(utterances[i])) {
      return utterances[i].messageId;
    }
  }
  return "";
}
