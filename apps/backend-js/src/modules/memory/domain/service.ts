/**
 * The service interface the HTTP handlers and the other modules call, plus the
 * request types and the limits that bound what memory costs.
 *
 * Ported 1:1 from internal/modules/memory/domain/service.go.
 */

import type {
  ExtractResult,
  KnowledgeFilter,
  KnowledgeItem,
  IngestRequest,
  Memory,
  MemoryFilter,
  Recall,
  RecallRequest,
  RememberRequest,
  RememberResult,
  Scope,
} from "./memory.ts";

/** Service is what the HTTP handlers and the other modules call. */
export interface Service {
  // --- memory a person manages

  /** Returns the memories of the workspace, newest first, optionally narrowed to one layer and filtered by a search term. */
  memories(scope: Scope, filter: MemoryFilter): Promise<Memory[]>;
  /** Stores one memory a person wrote. */
  add(scope: Scope, request: AddRequest): Promise<Memory>;
  /** Changes one memory. Changing the content re-embeds it, because the stored vector has to describe the text it sits next to. */
  edit(scope: Scope, request: EditRequest): Promise<Memory>;
  /** Deletes one memory and its vector, permanently. */
  forget(scope: Scope, id: string): Promise<void>;

  // --- knowledge

  knowledge(scope: Scope, filter: KnowledgeFilter): Promise<KnowledgeItem[]>;
  /** Chunks one document, embeds it, and stores the chunks. */
  ingest(scope: Scope, request: IngestRequest): Promise<KnowledgeItem[]>;
  /** Deletes one chunk and its vector. */
  forgetKnowledge(scope: Scope, id: string): Promise<void>;

  // --- what a round is given

  /** Assembles the memory one round answers with, and records which chunks it used. */
  recall(scope: Scope, request: RecallRequest): Promise<Recall>;

  // --- what a message asks for

  /** Stores a direct request — "ingat ya, ..." — and answers with a confirmation that names what was stored. */
  remember(scope: Scope, request: RememberRequest): Promise<RememberResult>;

  // --- extraction

  /** Resets the quiet period of a conversation, so extraction runs when the thread stops moving rather than on every message. */
  active(scope: Scope, conversationId: string, agentId: string): Promise<void>;
  /** Reads a quiet conversation and writes what is worth keeping. It is the activity the extraction workflow calls. */
  extract(scope: Scope, request: ExtractRequest): Promise<ExtractResult>;
}

/** ExtractRequest is one extraction pass. */
export interface ExtractRequest {
  conversationId: string;
  /** The Bolu whose switch decides whether extraction runs at all, and whose private layer the role-specific memories belong to. */
  agentId: string;
}

/** AddRequest is one memory a person writes, from the memory screen. */
export interface AddRequest {
  content: string;
  kind: string;
  /** Empty means the main memory. */
  agentId: string;
  pinned: boolean;
}

/** EditRequest is one edit from the memory screen. */
export interface EditRequest {
  id: string;
  content?: string | null;
  kind?: string | null;
  pinned?: boolean | null;
  /**
   * Moves the memory to another layer. `moveToMain` moves it back to the
   * workspace-wide one, which is a value an empty agent id cannot express: an
   * absent field has to mean "leave the layer alone".
   */
  agentId?: string | null;
  moveToMain: boolean;
}

/** Limits bound what memory costs: how much of it travels in every prompt, and how long a conversation must be still before it is read. */
export interface Limits {
  /** Caps the main memory that is sent on every call. The cap is in tokens rather than rows because that is what the bill is in. */
  mainTokens: number;
  /** Bound what one round retrieves. */
  recallMemories: number;
  recallKnowledge: number;
  /** How close a candidate has to be to be considered the same fact. */
  similarity: number;
  /** How long a conversation must not move before extraction reads it. Milliseconds. */
  quietPeriod: number;
  /** Bounds how much of the thread the extractor reads. */
  extractionMessages: number;
  /**
   * The size of one knowledge chunk, and how much of the end of one chunk
   * repeats at the start of the next. Zero means the default rather than no
   * overlap.
   */
  chunkRunes: number;
  chunkOverlapRunes: number;
  /** Bounds one memory and one chunk. */
  maxContentRunes: number;
}

/** The product's judgement, so they live in one place rather than being repeated per call site. */
export const DefaultLimits: Limits = {
  mainTokens: 1500,
  recallMemories: 6,
  recallKnowledge: 4,
  similarity: 0.9,
  quietPeriod: 5 * 60_000,
  extractionMessages: 30,
  chunkRunes: 1000,
  chunkOverlapRunes: 150,
  maxContentRunes: 2000,
};

/** Fills the zero fields from the defaults, so a partially configured deployment still has bounds. */
export function normaliseLimits(l: Limits): Limits {
  return {
    mainTokens: l.mainTokens <= 0 ? DefaultLimits.mainTokens : l.mainTokens,
    recallMemories: l.recallMemories <= 0 ? DefaultLimits.recallMemories : l.recallMemories,
    recallKnowledge: l.recallKnowledge <= 0 ? DefaultLimits.recallKnowledge : l.recallKnowledge,
    similarity: l.similarity <= 0 ? DefaultLimits.similarity : l.similarity,
    quietPeriod: l.quietPeriod <= 0 ? DefaultLimits.quietPeriod : l.quietPeriod,
    extractionMessages:
      l.extractionMessages <= 0 ? DefaultLimits.extractionMessages : l.extractionMessages,
    chunkRunes: l.chunkRunes <= 0 ? DefaultLimits.chunkRunes : l.chunkRunes,
    chunkOverlapRunes:
      l.chunkOverlapRunes <= 0 ? DefaultLimits.chunkOverlapRunes : l.chunkOverlapRunes,
    maxContentRunes: l.maxContentRunes <= 0 ? DefaultLimits.maxContentRunes : l.maxContentRunes,
  };
}
