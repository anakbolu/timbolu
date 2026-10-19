/**
 * The memory use cases: what a Bolu remembers, what it knows, what one round is
 * given, and what a conversation leaves behind.
 *
 * Three rules run through the package:
 *
 *   - nothing is stored without a vector. A memory or a chunk the search can
 *     never find is worse than a refused write, because it looks like the Bolu
 *     knows something and then never uses it.
 *   - a write is filtered before it is stored. Card numbers, passwords, and one
 *     time codes are refused rather than remembered.
 *   - reading degrades, writing does not. A task must run when the embedding
 *     provider is down; a memory must not be written when it cannot be.
 *
 * Ported 1:1 from internal/modules/memory/service/service.go.
 */

import type { Logger } from "../../../platform/logger.ts";
import {
  EmbeddingDimensions,
  PurposeEmbedding,
} from "../../llm/domain/provider.ts";
import {
  ActionCreated,
  ActionDeleted,
  ActionIngested,
  ActionKnowledgeDel,
  ActionMoved,
  ActionUpdated,
  ErrInvalidInput,
  ErrNoEmbedding,
  ErrSensitive,
  ErrVectorWidth,
  Kinds,
  KindFact,
  SourceUser,
  StatusReady,
  errorMessage,
  isNilID,
  memoryLayer,
  scopeIsZero,
  wrap,
  type AuditEntry,
  type Auditor,
  type Completer,
  type Conversation,
  type Embedder,
  type ExtractResult,
  type IngestRequest,
  type KnowledgeFilter,
  type KnowledgeItem,
  type Memory,
  type MemoryChange,
  type MemoryFilter,
  type NewKnowledge,
  type Recall,
  type RecallRequest,
  type RememberRequest,
  type RememberResult,
  type Repository,
  type Scheduler,
  type Scope,
} from "../domain/memory.ts";
import { normaliseLimits, type AddRequest, type EditRequest, type ExtractRequest, type Limits, type Service as ServicePort } from "../domain/service.ts";
import { chunkText, maxChunks, maxDocumentRunes, runeLength, trimSpace } from "./chunk.ts";
import { extractConversation } from "./extract.ts";
import { sensitive } from "./filter.ts";
import { recallRound } from "./recall.ts";
import { rememberMessage } from "./remember.ts";

/**
 * The service dependencies.
 *
 * The embedder and the model are optional in the sense that the module still
 * serves reads without them: the memory list, the knowledge list, and the delete
 * endpoints keep working, while a write that needs a vector answers with the
 * reason instead of pretending it stored something.
 */
export interface Deps {
  repository: Repository;
  embedder?: Embedder | null;
  model?: Completer | null;
  threads?: Conversation | null;
  auditor?: Auditor | null;
  scheduler?: Scheduler | null;
  limits: Limits;
  logger?: Logger | null;
}

/** The memory implementation. */
export class Service implements ServicePort {
  readonly deps: Deps;
  readonly limits: Limits;

  constructor(deps: Deps) {
    this.deps = deps;
    this.limits = normaliseLimits(deps.limits);
  }

  /**
   * Resets a conversation's quiet period.
   *
   * A deployment without the durable runtime has no scheduler, and then a
   * conversation is simply never extracted: the switch exists, the writes do
   * not.
   */
  async active(scope: Scope, conversationId: string, agentId: string): Promise<void> {
    if (!this.deps.scheduler) {
      return;
    }
    if (isNilID(conversationId)) {
      throw wrap(ErrInvalidInput, "conversation id is required");
    }

    return this.deps.scheduler.conversationActive(scope, conversationId, agentId);
  }

  // ------------------------------------------------------------- embedding

  /**
   * Turns text into vectors and refuses an answer that does not fit the schema.
   *
   * The width is checked before the vector is stored rather than after: a model
   * that answers with 1024 numbers would otherwise fail on the insert, inside a
   * transaction, with an error nobody can act on.
   */
  async embed(
    scope: Scope,
    agentId: string,
    taskId: string,
    inputs: string[],
  ): Promise<number[][]> {
    if (inputs.length === 0) {
      return [];
    }
    if (!this.deps.embedder) {
      throw wrap(ErrNoEmbedding, "no embedding provider is configured");
    }

    const response = await this.deps.embedder.embed(undefined, {
      userId: scope.userId ?? "",
      workspaceId: scope.workspaceId,
      agentId: isNilID(agentId) ? "" : agentId,
    }, {
      model: "",
      inputs,
      metadata: {
        workspaceId: scope.workspaceId,
        agentId: isNilID(agentId) ? "" : agentId,
        taskId: isNilID(taskId) ? "" : taskId,
        purpose: PurposeEmbedding,
      },
    });
    if (response.vectors.length !== inputs.length) {
      throw wrap(
        ErrVectorWidth,
        `${response.vectors.length} vectors for ${inputs.length} inputs`,
      );
    }

    for (let i = 0; i < response.vectors.length; i++) {
      const vector = response.vectors[i];
      if (vector.length !== EmbeddingDimensions) {
        throw wrap(
          ErrVectorWidth,
          `input ${i} has ${vector.length} numbers, the schema stores ${EmbeddingDimensions}`,
        );
      }
    }

    return response.vectors;
  }

  // -------------------------------------------------------------- auditing

  /**
   * Records one change. A missing auditor is not an error: the trail is worth
   * having, and losing a write because the trail is unavailable is worse.
   */
  async audit(scope: Scope, entry: AuditEntry): Promise<void> {
    if (!this.deps.auditor) {
      return;
    }

    entry.actorUserId = scope.userId ?? "";
    try {
      await this.deps.auditor.record(scope, entry);
    } catch (err) {
      this.deps.logger?.warn(
        {
          action: entry.action,
          object_id: entry.objectId,
          err: errorMessage(err),
        },
        "could not record a memory change in the audit trail",
      );
    }
  }

  // --------------------------------------------------------------- reading

  /** Returns the memories of the workspace. */
  async memories(scope: Scope, filter: MemoryFilter): Promise<Memory[]> {
    if (scopeIsZero(scope)) {
      throw wrap(ErrInvalidInput, "workspace is required");
    }

    return this.deps.repository.listMemories(scope, filter);
  }

  /** Returns the chunks of the workspace, including the ones whose embedding failed. */
  async knowledge(scope: Scope, filter: KnowledgeFilter): Promise<KnowledgeItem[]> {
    if (scopeIsZero(scope)) {
      throw wrap(ErrInvalidInput, "workspace is required");
    }

    return this.deps.repository.listKnowledge(scope, filter);
  }

  // --------------------------------------------------------------- writing

  /** Stores one memory a person wrote. */
  async add(scope: Scope, request: AddRequest): Promise<Memory> {
    if (scopeIsZero(scope)) {
      throw wrap(ErrInvalidInput, "workspace is required");
    }

    const content = await this.accept(request.content);

    let kind = request.kind;
    if (kind === "") {
      kind = KindFact;
    }
    if (!knownKind(kind)) {
      throw wrap(ErrInvalidInput, `unknown kind ${JSON.stringify(kind)}`);
    }

    const vectors = await this.embed(scope, request.agentId, "", [content]);

    const memory = await this.deps.repository.createMemory(scope, {
      agentId: request.agentId,
      content,
      kind,
      sourceMessageId: "",
      pinned: request.pinned,
      createdBy: SourceUser,
      embedding: vectors[0],
    });

    // The actor is the person who asked, taken from the scope rather than from
    // the request body: a line saying what changed but not who changed it cannot
    // answer the question the trail exists for, and the caller's identity is
    // what the scope already carries.
    await this.audit(scope, {
      action: ActionCreated,
      objectType: "memory",
      objectId: memory.id,
      actorUserId: scope.userId ?? "",
      actorAgentId: "",
      metadata: {
        layer: memoryLayer(memory),
        kind: memory.kind,
        pinned: memory.pinned,
      },
    });

    return memory;
  }

  /** Changes one memory. */
  async edit(scope: Scope, request: EditRequest): Promise<Memory> {
    if (scopeIsZero(scope)) {
      throw wrap(ErrInvalidInput, "workspace is required");
    }

    const stored = await this.deps.repository.memory(scope, request.id);

    const change: MemoryChange = {
      kind: request.kind ?? null,
      pinned: request.pinned ?? null,
      moveMain: false,
      embedding: [],
    };

    if (request.kind != null && !knownKind(request.kind)) {
      throw wrap(ErrInvalidInput, `unknown kind ${JSON.stringify(request.kind)}`);
    }

    if (request.content != null) {
      const content = await this.accept(request.content);

      // A changed text needs a new vector: the stored one describes the words
      // that used to be there, and leaving it would make the memory findable by
      // something it no longer says.
      if (content !== stored.content) {
        const vectors = await this.embed(scope, stored.agentId, "", [content]);

        change.content = content;
        change.embedding = vectors[0];
      }
    }

    let moved = false;
    if (request.moveToMain) {
      change.moveMain = true;
      moved = !isNilID(stored.agentId);
    } else if (request.agentId != null && request.agentId !== stored.agentId) {
      change.moveTo = request.agentId;
      moved = true;
    }

    const updated = await this.deps.repository.updateMemory(scope, request.id, change);

    await this.audit(scope, {
      action: moved ? ActionMoved : ActionUpdated,
      objectType: "memory",
      objectId: updated.id,
      actorUserId: scope.userId ?? "",
      actorAgentId: "",
      metadata: {
        layer: memoryLayer(updated),
        from: memoryLayer(stored),
        kind: updated.kind,
        pinned: updated.pinned,
        rewritten: change.content != null,
      },
    });

    return updated;
  }

  /** Deletes one memory and its vector, permanently. */
  async forget(scope: Scope, id: string): Promise<void> {
    if (scopeIsZero(scope)) {
      throw wrap(ErrInvalidInput, "workspace is required");
    }

    const stored = await this.deps.repository.memory(scope, id);

    await this.deps.repository.deleteMemory(scope, id);

    // The trail says what was deleted and never copies it: a memory is deleted
    // because it should not be there, and writing it into another table would
    // keep it.
    await this.audit(scope, {
      action: ActionDeleted,
      objectType: "memory",
      objectId: id,
      actorUserId: scope.userId ?? "",
      actorAgentId: "",
      metadata: { layer: memoryLayer(stored), kind: stored.kind },
    });
  }

  /** Deletes one chunk and its vector. */
  async forgetKnowledge(scope: Scope, id: string): Promise<void> {
    if (scopeIsZero(scope)) {
      throw wrap(ErrInvalidInput, "workspace is required");
    }

    const stored = await this.deps.repository.knowledge(scope, id);

    await this.deps.repository.deleteKnowledge(scope, id);

    await this.audit(scope, {
      action: ActionKnowledgeDel,
      objectType: "knowledge_item",
      objectId: id,
      actorUserId: scope.userId ?? "",
      actorAgentId: "",
      metadata: { source: stored.source, chunk_index: stored.chunkIndex },
    });
  }

  /**
   * Chunks one document, embeds it, and stores the chunks.
   *
   * The chunks are written before they are embedded so an upload is visible
   * while it runs, and they are settled to `ready` or `failed` afterwards. A
   * failed chunk keeps its row: it is not searchable — every search requires
   * `ready` — and it is the record that says the upload did not work.
   */
  async ingest(scope: Scope, request: IngestRequest): Promise<KnowledgeItem[]> {
    if (scopeIsZero(scope)) {
      throw wrap(ErrInvalidInput, "workspace is required");
    }

    const text = trimSpace(request.text);
    if (text === "") {
      throw wrap(ErrInvalidInput, "the document is empty");
    }
    if (runeLength(text) > maxDocumentRunes) {
      throw wrap(ErrInvalidInput, `the document is longer than ${maxDocumentRunes} characters`);
    }

    const chunks = chunkText(text, this.limits.chunkRunes, this.limits.chunkOverlapRunes);
    if (chunks.length === 0) {
      throw wrap(ErrInvalidInput, "the document has nothing to store");
    }
    if (chunks.length > maxChunks) {
      throw wrap(
        ErrInvalidInput,
        `the document is ${chunks.length} chunks, the limit is ${maxChunks}`,
      );
    }

    const items: NewKnowledge[] = chunks.map((chunk, i) => ({
      agentId: request.agentId,
      source: trimSpace(request.source),
      chunkIndex: i,
      content: chunk,
    }));

    const stored = await this.deps.repository.createKnowledge(scope, items);

    const ids = stored.map((item) => item.id);

    let vectors: number[][];
    try {
      vectors = await this.embed(scope, request.agentId, "", chunks);
    } catch (embedErr) {
      try {
        await this.deps.repository.failKnowledge(scope, ids);
      } catch (failErr) {
        this.deps.logger?.error(
          {
            workspace_id: scope.workspaceId,
            err: errorMessage(failErr),
          },
          "could not settle the chunks of a failed upload",
        );
      }
      throw embedErr;
    }

    await this.deps.repository.embedKnowledge(scope, ids, vectors);

    for (const item of stored) {
      item.status = StatusReady;
    }

    await this.audit(scope, {
      action: ActionIngested,
      objectType: "knowledge_item",
      objectId: stored[0].id,
      actorUserId: scope.userId ?? "",
      actorAgentId: "",
      metadata: {
        source: stored[0].source,
        chunks: stored.length,
        layer: stored[0].agentId,
      },
    });

    return stored;
  }

  // --------------------------------------------------------------- helpers

  /** Assembles the memory one round answers with. See `recall.ts`. */
  async recall(scope: Scope, request: RecallRequest): Promise<Recall> {
    return recallRound(this, scope, request);
  }

  /** Stores a direct request — "ingat ya, ...". See `remember.ts`. */
  async remember(scope: Scope, request: RememberRequest): Promise<RememberResult> {
    return rememberMessage(this, scope, request);
  }

  /** Reads a quiet conversation and writes what is worth keeping. See `extract.ts`. */
  async extract(scope: Scope, request: ExtractRequest): Promise<ExtractResult> {
    return extractConversation(this, scope, request);
  }

  /** Validates and filters one piece of text before it is stored. */
  private async accept(content: string): Promise<string> {
    const trimmed = trimSpace(content);
    if (trimmed === "") {
      throw wrap(ErrInvalidInput, "the content is empty");
    }
    if (runeLength(trimmed) > this.limits.maxContentRunes) {
      throw wrap(
        ErrInvalidInput,
        `the content is longer than ${this.limits.maxContentRunes} characters`,
      );
    }
    const [reason, isSensitive] = sensitive(trimmed);
    if (isSensitive) {
      // The message is the sentence a person reads, because the handler returns
      // it verbatim: an error string that leaks the package name and the rule's
      // own wording ("memory: the content is sensitive") reads as a system fault
      // rather than as an answer about what was typed.
      throw wrap(ErrSensitive, `isinya mengandung ${reason}, jadi tidak disimpan`);
    }

    return trimmed;
  }
}

function knownKind(kind: string): boolean {
  return Kinds.includes(kind);
}
