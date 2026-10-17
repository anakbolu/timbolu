/**
 * The memory contract: what a Bolu remembers, what it knows, and the ports the
 * service needs to write, retrieve, and delete both.
 *
 * Two things are stored here and they are not the same thing:
 *
 *   - a **memory** is something a Bolu was told or worked out about the
 *     workspace and its own role. It is short, it is written by a person or by
 *     extraction, and it travels in the prompt on every call.
 *   - a **knowledge item** is a piece of a document the workspace owns. It is
 *     long, it is one chunk of many, and it is retrieved by similarity to the
 *     task at hand.
 *
 * Both are filtered by workspace at every read, and both are deleted for real:
 * there is no soft-delete column, because "forget this" that leaves the text in
 * the database is not forgetting.
 *
 * Ported 1:1 from internal/modules/memory/domain/memory.go.
 */

import type {
  ChatRequest,
  ChatResponse,
  Embedder as LLMEmbedder,
  Scope as LLMScope,
} from "../../llm/domain/gateway.ts";

/** The zero uuid, as Go's `uuid.Nil`. */
export const NilUUID = "00000000-0000-0000-0000-000000000000";

// Memory kinds, matching the check constraint on `memories.kind`.
export const KindFact = "fact";
export const KindPreference = "preference";
export const KindInstruction = "instruction";

/** Kinds lists the kinds a write may use, in the order a screen offers them. */
export const Kinds: string[] = [KindFact, KindPreference, KindInstruction];

// CreatedBy values, matching the check constraint on `memories.created_by`. A
// memory a person asked for and one the extractor guessed are told apart because
// they earn different trust.
export const SourceUser = "user";
export const SourceExtraction = "extraction";

// Knowledge processing states, matching the check constraint on
// `knowledge_items.status`. `pending` is a row whose chunks are stored but not
// yet embedded; only `ready` is ever searchable.
export const StatusPending = "pending";
export const StatusReady = "ready";
export const StatusFailed = "failed";

// Audit actions this module records. They are the vocabulary of "who changed
// what a Bolu remembers".
export const ActionCreated = "memory.created";
export const ActionUpdated = "memory.updated";
export const ActionDeleted = "memory.deleted";
export const ActionMoved = "memory.moved";
export const ActionIngested = "knowledge.ingested";
export const ActionKnowledgeDel = "knowledge.deleted";

// Errors the service and the handler map onto responses.
export const ErrNotFound = new Error("memory: not found");
export const ErrInvalidInput = new Error("memory: invalid input");
export const ErrSensitive = new Error("memory: the content is sensitive");
export const ErrNotRemembered = new Error("memory: the message is not a memory request");
export const ErrNoEmbedding = new Error("memory: no provider can embed");
export const ErrNoModel = new Error("memory: no model is configured to read the conversation");
export const ErrVectorWidth = new Error("memory: the embedding has the wrong width");
export const ErrExtractTooSoon = new Error("memory: the conversation is not quiet yet");

/**
 * An error carrying the sentinel a caller switches on, so `is` reproduces Go's
 * `errors.Is` for the messages `fmt.Errorf("%w: ...")` produced.
 */
export class WrappedError extends Error {
  constructor(
    readonly sentinel: Error,
    message: string,
  ) {
    super(message);
    this.name = "WrappedError";
  }
}

/** Wraps a sentinel with a detail, rendering the message as Go's `%w: detail`. */
export function wrap(sentinel: Error, detail: string): WrappedError {
  return new WrappedError(sentinel, `${sentinel.message}: ${detail}`);
}

/** True when `err` is the sentinel or wraps it. */
export function is(err: unknown, sentinel: Error): boolean {
  let current: unknown = err;
  for (let guard = 0; guard < 32; guard++) {
    if (current === sentinel) {
      return true;
    }
    if (!(current instanceof Error)) {
      return false;
    }
    if (current instanceof WrappedError) {
      current = current.sentinel;
      continue;
    }
    current = "cause" in current ? current.cause : undefined;
  }
  return false;
}

/** The message of an unknown thrown value. */
export function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/** Scope is the tenant identity a query runs as. */
export interface Scope {
  userId?: string;
  workspaceId: string;
}

/** IsZero reports whether the scope carries no workspace at all. */
export function scopeIsZero(s: Scope): boolean {
  return !s.workspaceId || s.workspaceId === NilUUID;
}

/** True for an absent id, which Go expresses as the zero uuid. */
export function isNilID(id: string | null | undefined): boolean {
  return !id || id === NilUUID;
}

/**
 * Memory is one thing a Bolu remembers.
 *
 * The ids are carried as strings on the wire and typed inside the domain, because
 * `encoding/json` cannot omit a `uuid.UUID` — it is an array — so a typed field
 * would publish `00000000-0000-0000-0000-000000000000` and a client would read it
 * as an author: a main memory would look like it belonged to a Bolu whose id is
 * all zeroes, and "is this superseded" would be true for every row.
 */
export interface Memory {
  id: string;
  workspaceId: string;
  /** Empty for the main memory, which the whole workspace shares, and set for a Bolu's private memory. */
  agentId: string;
  content: string;
  kind: string;
  sourceMessageId: string;
  pinned: boolean;
  createdBy: string;
  /** Names the memory that replaced this one. A superseded memory is history: it is shown, and it is never sent to a model again. */
  supersededBy: string;
  /** How close the memory was to the query, when it came back from a search. Zero for a list read. */
  similarity: number;
  createdAt: Date;
  updatedAt: Date;
}

/** Layer reports which of the two layers the memory belongs to. */
export function memoryLayer(m: Memory): string {
  if (isNilID(m.agentId)) {
    return LayerMain;
  }
  return LayerPrivate;
}

// Layers.
export const LayerMain = "main";
export const LayerPrivate = "private";

/** The wire shape `MarshalJSON` emits. */
export interface MemoryWire {
  id: string;
  workspace_id: string;
  agent_id?: string;
  content: string;
  kind: string;
  source_message_id?: string;
  pinned: boolean;
  created_by: string;
  superseded_by?: string;
  similarity?: number;
  layer: string;
  created_at: string;
  updated_at: string;
}

/**
 * Renders a memory for a client, with the ids that are absent left out rather
 * than sent as the zero value.
 */
export function memoryJSON(m: Memory): MemoryWire {
  const out: MemoryWire = {
    id: m.id,
    workspace_id: m.workspaceId,
    content: m.content,
    kind: m.kind,
    pinned: m.pinned,
    created_by: m.createdBy,
    layer: memoryLayer(m),
    created_at: m.createdAt.toISOString(),
    updated_at: m.updatedAt.toISOString(),
  };
  const agentId = wireID(m.agentId);
  if (agentId) out.agent_id = agentId;
  const sourceMessageId = wireID(m.sourceMessageId);
  if (sourceMessageId) out.source_message_id = sourceMessageId;
  const supersededBy = wireID(m.supersededBy);
  if (supersededBy) out.superseded_by = supersededBy;
  if (m.similarity !== 0) out.similarity = m.similarity;
  return out;
}

/** NewMemory is one memory to write. */
export interface NewMemory {
  agentId: string;
  content: string;
  kind: string;
  sourceMessageId: string;
  pinned: boolean;
  createdBy: string;
  /**
   * The vector of Content. A write without one stores a memory that can never be
   * found by similarity, so the service refuses instead.
   */
  embedding: number[];
}

/** MemoryChange is one edit. An absent field is left alone; `moveMain` moves the memory to the main layer. */
export interface MemoryChange {
  content?: string | null;
  kind?: string | null;
  pinned?: boolean | null;
  moveTo?: string | null;
  moveMain: boolean;
  /**
   * The vector of a new Content. It is required when Content changes, because
   * the stored vector has to describe the text it sits next to.
   */
  embedding: number[];
}

/** MemoryFilter narrows a list read. */
export interface MemoryFilter {
  /** Limits the read to one layer. `main` filters the workspace-wide rows. */
  agentId: string;
  main: boolean;
  /** A substring match on the content, for the screen's search box. */
  query: string;
  /** Keeps the history in the answer. */
  includeSuperseded: boolean;
  /** Bounds the read; zero means the repository default. */
  limit: number;
}

/** KnowledgeItem is one chunk of a document the workspace owns. */
export interface KnowledgeItem {
  id: string;
  workspaceId: string;
  agentId: string;
  source: string;
  chunkIndex: number;
  content: string;
  status: string;
  similarity: number;
  createdAt: Date;
}

/** The wire shape `MarshalJSON` emits for a chunk. */
export interface KnowledgeWire {
  id: string;
  workspace_id: string;
  agent_id?: string;
  source: string;
  chunk_index: number;
  content: string;
  status: string;
  similarity?: number;
  created_at: string;
}

/** Renders a chunk for a client, with the ids that are absent left out. */
export function knowledgeJSON(k: KnowledgeItem): KnowledgeWire {
  const out: KnowledgeWire = {
    id: k.id,
    workspace_id: k.workspaceId,
    source: k.source,
    chunk_index: k.chunkIndex,
    content: k.content,
    status: k.status,
    created_at: k.createdAt.toISOString(),
  };
  const agentId = wireID(k.agentId);
  if (agentId) out.agent_id = agentId;
  if (k.similarity !== 0) out.similarity = k.similarity;
  return out;
}

/** Renders an id that may be absent, which is the empty string rather than the zero uuid. */
function wireID(id: string): string {
  if (isNilID(id)) {
    return "";
  }
  return id;
}

/** NewKnowledge is one chunk to write. */
export interface NewKnowledge {
  agentId: string;
  source: string;
  chunkIndex: number;
  content: string;
}

/** KnowledgeFilter narrows a list read. */
export interface KnowledgeFilter {
  agentId: string;
  allAgents: boolean;
  status: string;
  query: string;
  source: string;
  limit: number;
  withVector: boolean;
}

/** SearchQuery is one similarity read. */
export interface SearchQuery {
  /**
   * Narrows a memory search to one Bolu's private layer. Knowledge is searched
   * workspace-wide by default and narrowed when a document belongs to one Bolu.
   */
  agentId: string;
  main: boolean;
  vector: number[];
  limit: number;
  /** Drops weak matches. Zero keeps everything the index returns. */
  minSimilarity: number;
}

/**
 * Retrieval is one record of a chunk that reached a prompt.
 *
 * Exactly one of memoryId and knowledgeId is set: a row that named both would
 * not answer "which memory did this answer see".
 */
export interface Retrieval {
  taskId: string;
  agentId: string;
  memoryId: string;
  knowledgeId: string;
  similarity: number;
}

/** RecallRequest is what one round asks for. */
export interface RecallRequest {
  agentId: string;
  taskId: string;
  /** The task at hand, which is what the private memory and the knowledge base are searched with. */
  query: string;
}

/**
 * Recall is the memory a round is given.
 *
 * The two halves travel separately because they are cached differently: stable
 * goes into the prefix a provider caches — the persona, the instructions, and the
 * main memory — while retrieved is the part that changes with the question.
 */
export interface Recall {
  /** The main memory of the workspace, rendered for a prompt. */
  stable: string;
  /** This Bolu's private memory and the knowledge chunks the search found, wrapped in a marker that says it is data. */
  retrieved: string;
  /** How many chunks were injected, which is what the quality measurement counts. */
  used: number;
}

/** RememberRequest is one message that may ask for something to be remembered. */
export interface RememberRequest {
  /** What the user wrote. */
  text: string;
  /** The Bolu being addressed, which is the layer a private memory belongs to. */
  agentId: string;
  /** The message it came from, so the memory can point back at it. */
  messageId: string;
}

/** RememberResult is what the memory module answered. */
export interface RememberResult {
  /** Says the message asked to remember something and was answered here. A false leaves the message to the ordinary reply path. */
  handled: boolean;
  /** The memory that was written, when one was. */
  stored: Memory | null;
  /** Says the content was sensitive: nothing was stored and the confirmation says so. */
  refused: boolean;
  /** What the Bolu answers. It names exactly what was stored, which is what makes the answer checkable. */
  confirmation: string;
}

/** IngestRequest is a document to chunk, embed, and store. */
export interface IngestRequest {
  agentId: string;
  source: string;
  text: string;
}

/** ExtractResult is what one extraction pass did. */
export interface ExtractResult {
  /** Says the Bolu does not want automatic extraction. */
  disabled: boolean;
  /** Account for every candidate the model proposed, so "why did nothing happen" has an answer. */
  stored: number;
  merged: number;
  skipped: number;
  refused: number;
}

/** Extracted is one memory the model proposed. */
export interface Extracted {
  content: string;
  kind: string;
  /** "main" or "private": general workspace facts go to the main memory, role-specific ones to the Bolu's own. */
  layer: string;
}

/** Utterance is one message of a conversation, as the extractor reads it. */
export interface Utterance {
  messageId: string;
  agentId: string;
  agentName: string;
  text: string;
  createdAt: Date;
}

/** FromAgent reports whether a Bolu wrote the message. */
export function utteranceFromAgent(u: Utterance): boolean {
  return !isNilID(u.agentId);
}

/** AgentInfo is the per-Bolu part the memory screen shows. */
export interface AgentInfo {
  id: string;
  name: string;
  extractionEnabled: boolean;
}

/**
 * Repository persists memories, knowledge, and the record of what was retrieved.
 *
 * Every method takes the scope rather than a workspace id, so the same
 * transaction-local tenant setting that Row Level Security reads is applied,
 * and a query that forgot its filter still cannot cross a workspace.
 */
export interface Repository {
  // --- memory
  createMemory(scope: Scope, memory: NewMemory): Promise<Memory>;
  memory(scope: Scope, id: string): Promise<Memory>;
  listMemories(scope: Scope, filter: MemoryFilter): Promise<Memory[]>;
  updateMemory(scope: Scope, id: string, change: MemoryChange): Promise<Memory>;
  /** Removes the row and its vector. There is no soft delete: a memory that is not deleted is a memory a model can still be given. */
  deleteMemory(scope: Scope, id: string): Promise<void>;
  /** Marks an older memory as replaced by a newer one, which is what keeps a corrected fact from being sent alongside its own correction. */
  supersede(scope: Scope, older: string, newer: string): Promise<void>;
  searchMemories(scope: Scope, query: SearchQuery): Promise<Memory[]>;

  // --- knowledge
  createKnowledge(scope: Scope, items: NewKnowledge[]): Promise<KnowledgeItem[]>;
  /** Stores the vectors of the chunks that were written and settles their status, in one statement per chunk set. */
  embedKnowledge(scope: Scope, ids: string[], vectors: number[][]): Promise<void>;
  /** Settles chunks that could not be embedded, so an upload is monitorable instead of left pending forever. */
  failKnowledge(scope: Scope, ids: string[]): Promise<void>;
  knowledge(scope: Scope, id: string): Promise<KnowledgeItem>;
  listKnowledge(scope: Scope, filter: KnowledgeFilter): Promise<KnowledgeItem[]>;
  deleteKnowledge(scope: Scope, id: string): Promise<void>;
  searchKnowledge(scope: Scope, query: SearchQuery): Promise<KnowledgeItem[]>;

  // --- what was used
  recordRetrieval(scope: Scope, usage: Retrieval): Promise<void>;

  // --- other modules' reads
  /** Returns the Bolu of the workspace with the two facts this module needs: the name a memory layer is labelled with, and the extraction switch. */
  agents(scope: Scope): Promise<AgentInfo[]>;
  /** Reports one Bolu's switch. */
  extractionEnabled(scope: Scope, agentId: string): Promise<boolean>;
}

/** Embedder is the model gateway's embedding port, aliased so the pipeline and the gateway cannot drift apart. */
export type Embedder = LLMEmbedder;

/** Completer is the model call the extractor makes. */
export interface Completer {
  chat(ctx: AbortSignal | undefined, scope: LLMScope, req: ChatRequest): Promise<ChatResponse>;
}

/** Conversation reads the thread an extraction runs over. */
export interface Conversation {
  /** Returns the last messages, oldest first, with the text a model reads. */
  recent(scope: Scope, conversationId: string, limit: number): Promise<Utterance[]>;
}

/** Auditor records what a person did, for the trail the product answers "who changed this" with. */
export interface Auditor {
  record(scope: Scope, entry: AuditEntry): Promise<void>;
}

/** AuditEntry is one thing that happened, for the record. */
export interface AuditEntry {
  action: string;
  objectType: string;
  objectId: string;
  /** Who did it, when a person did. ActorAgentId is the Bolu, when one did. */
  actorUserId: string;
  actorAgentId: string;
  /** The detail: the layer, the kind, the old and new content, and never a secret. */
  metadata: Record<string, unknown>;
}

/**
 * Scheduler asks the durable runtime to extract later.
 *
 * It is a port rather than a direct call because the debounce is a Temporal
 * schedule: the memory module declares what it wants and the container, which is
 * the only package that knows both sides, supplies the engine.
 */
export interface Scheduler {
  /** Tells the extractor that a conversation moved, which resets the quiet period it waits for. */
  conversationActive(scope: Scope, conversationId: string, agentId: string): Promise<void>;
}
