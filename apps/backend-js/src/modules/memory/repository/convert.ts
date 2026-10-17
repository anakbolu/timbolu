/**
 * The conversions from the query rows to the domain types.
 *
 * sqlc gave every query its own row type; here they are structurally identical,
 * so one shape is declared and the per-query names are kept as the call sites
 * the Go file had.
 *
 * Ported 1:1 from internal/modules/memory/repository/convert.go.
 */

import type { KnowledgeItem, Memory } from "../domain/memory.ts";

/** The columns every memory query returns. */
export interface MemoryRow {
  id: string;
  workspace_id: string;
  agent_id: string | null;
  content: string;
  kind: string;
  source_message_id: string | null;
  pinned: boolean;
  created_by: string;
  superseded_by: string | null;
  created_at: Date;
  updated_at: Date;
}

/** A memory search adds the computed similarity. */
export interface SearchMemoryRow extends MemoryRow {
  similarity: number;
}

/** The columns every knowledge query returns. */
export interface KnowledgeRow {
  id: string;
  workspace_id: string;
  agent_id: string | null;
  source: string;
  chunk_index: number;
  content: string;
  status: string;
  created_at: Date;
}

/** A knowledge search adds the computed similarity. */
export interface SearchKnowledgeRow extends KnowledgeRow {
  similarity: number;
}

export function fromCreateRow(row: MemoryRow): Memory {
  return memory(row, 0);
}

export function fromGetRow(row: MemoryRow): Memory {
  return memory(row, 0);
}

export function fromListRow(row: MemoryRow): Memory {
  return memory(row, 0);
}

export function fromUpdateRow(row: MemoryRow): Memory {
  return memory(row, 0);
}

export function fromSearchRow(row: SearchMemoryRow): Memory {
  return memory(row, row.similarity);
}

export function fromCreateKnowledgeRow(row: KnowledgeRow): KnowledgeItem {
  return knowledge(row, 0);
}

export function fromGetKnowledgeRow(row: KnowledgeRow): KnowledgeItem {
  return knowledge(row, 0);
}

export function fromListKnowledgeRow(row: KnowledgeRow): KnowledgeItem {
  return knowledge(row, 0);
}

export function fromSearchKnowledgeRow(row: SearchKnowledgeRow): KnowledgeItem {
  return knowledge(row, row.similarity);
}

/** Builds one memory. */
function memory(row: MemoryRow, similarity: number): Memory {
  return {
    id: row.id,
    workspaceId: row.workspace_id,
    agentId: row.agent_id ?? "",
    content: row.content,
    kind: row.kind,
    sourceMessageId: row.source_message_id ?? "",
    pinned: row.pinned,
    createdBy: row.created_by,
    supersededBy: row.superseded_by ?? "",
    similarity,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

/** Builds one chunk. */
function knowledge(row: KnowledgeRow, similarity: number): KnowledgeItem {
  return {
    id: row.id,
    workspaceId: row.workspace_id,
    agentId: row.agent_id ?? "",
    source: row.source,
    chunkIndex: row.chunk_index,
    content: row.content,
    status: row.status,
    similarity,
    createdAt: row.created_at,
  };
}
