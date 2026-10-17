/**
 * The memory data access: memories, knowledge, and the record of what was
 * retrieved.
 *
 * Every statement runs inside the caller's tenant scope, so Row Level Security
 * filters it even when a query forgets its workspace filter, and the similarity
 * reads filter by layer as well: a private memory that leaked into another
 * Bolu's prompt would be as wrong as one that leaked into another workspace.
 *
 * Vectors are sent as text and never read back. Postgres computes the
 * similarity, and the stored embedding is not a column any caller selects: 1536
 * floats per row are not worth shipping to a handler that will not read them.
 *
 * Ported 1:1 from internal/modules/memory/repository/postgres.go and the sqlc
 * queries in repository/queries/memory.sql (folded in below, one function per
 * named query with the query name kept as a comment).
 */

import type { Database, Scope as DatabaseScope, Tx } from "../../../platform/database/index.ts";
import {
  ErrInvalidInput,
  ErrNotFound,
  isNilID,
  wrap,
  type AgentInfo,
  type KnowledgeFilter,
  type KnowledgeItem,
  type Memory,
  type MemoryChange,
  type MemoryFilter,
  type NewKnowledge,
  type NewMemory,
  type Repository as RepositoryPort,
  type Retrieval,
  type Scope,
  type SearchQuery,
} from "../domain/memory.ts";
import {
  fromCreateKnowledgeRow,
  fromCreateRow,
  fromGetKnowledgeRow,
  fromGetRow,
  fromListKnowledgeRow,
  fromListRow,
  fromSearchKnowledgeRow,
  fromSearchRow,
  fromUpdateRow,
  type KnowledgeRow,
  type MemoryRow,
  type SearchKnowledgeRow,
  type SearchMemoryRow,
} from "./convert.ts";

/** The Postgres-backed implementation of the memory ports. */
export class PostgresRepository implements RepositoryPort {
  constructor(private readonly db: Database) {}

  // --------------------------------------------------------------- memory

  /** Stores one memory. */
  async createMemory(scope: Scope, memory: NewMemory): Promise<Memory> {
    const agentId = optionalID(memory.agentId);
    const workspaceId = scope.workspaceId;

    // name: CreateMemory
    // A memory for one Bolu is only written when that Bolu is in this
    // workspace. The statement checks it itself because a foreign key runs with
    // the table owner's rights and bypasses Row Level Security.
    return this.write(scope, async (tx) => {
      const rows = await tx<MemoryRow[]>`
        INSERT INTO memories (
            workspace_id, agent_id, content, kind, source_message_id, embedding, pinned, created_by
        )
        SELECT
            ${workspaceId},
            ${agentId}::uuid,
            ${memory.content},
            ${memory.kind},
            ${optionalID(memory.sourceMessageId)}::uuid,
            ${vectorLiteral(memory.embedding)}::vector,
            ${memory.pinned},
            ${memory.createdBy}
        WHERE ${agentId}::uuid IS NULL
           OR EXISTS (
                SELECT 1 FROM agents a
                WHERE a.id = ${agentId}::uuid AND a.workspace_id = ${workspaceId}
           )
        RETURNING
            id, workspace_id, agent_id, content, kind, source_message_id,
            pinned, created_by, superseded_by, created_at, updated_at
      `;

      const row = rows[0];
      if (!row) {
        // The insert is a SELECT, so a Bolu outside this workspace comes back
        // as no rows rather than as a constraint violation.
        throw wrap(ErrInvalidInput, "the bolu is not in this workspace");
      }
      return fromCreateRow(row);
    });
  }

  /** Returns one memory. */
  async memory(scope: Scope, id: string): Promise<Memory> {
    // name: GetMemory
    return this.read(scope, async (tx) => {
      const rows = await tx<MemoryRow[]>`
        SELECT
            id, workspace_id, agent_id, content, kind, source_message_id,
            pinned, created_by, superseded_by, created_at, updated_at
        FROM memories
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
      `;

      const row = rows[0];
      if (!row) {
        throw wrap(ErrNotFound, "get memory");
      }
      return fromGetRow(row);
    });
  }

  /** Returns the memories the screen shows. */
  async listMemories(scope: Scope, filter: MemoryFilter): Promise<Memory[]> {
    const agentId = optionalID(filter.agentId);
    const query = optionalString(filter.query);

    // name: ListMemories
    // The screen reads this: a layer, a search term, and the history when asked
    // for.
    return this.read(scope, async (tx) => {
      const rows = await tx<MemoryRow[]>`
        SELECT
            id, workspace_id, agent_id, content, kind, source_message_id,
            pinned, created_by, superseded_by, created_at, updated_at
        FROM memories
        WHERE workspace_id = ${scope.workspaceId}
          AND (
                ${agentId}::uuid IS NULL
                OR agent_id = ${agentId}::uuid
              )
          AND (NOT ${filter.main}::boolean OR agent_id IS NULL)
          AND (${filter.includeSuperseded}::boolean OR superseded_by IS NULL)
          AND (
                ${query}::text IS NULL
                OR content ILIKE '%' || ${query}::text || '%'
              )
        ORDER BY pinned DESC, updated_at DESC
        LIMIT ${limitOf(filter.limit)}
      `;

      return rows.map(fromListRow);
    });
  }

  /** Changes one memory. */
  async updateMemory(scope: Scope, id: string, change: MemoryChange): Promise<Memory> {
    // name: UpdateMemory
    // One statement covers every edit the screen offers. A null keeps the stored
    // value; moving a memory to the main layer is its own flag because an absent
    // agent id already means "leave the layer alone".
    //
    // The vector is re-written only when the text changed: an edit that only
    // pins a memory must not spend an embedding call.
    const embedding =
      change.embedding.length > 0 ? vectorLiteral(change.embedding) : null;

    return this.write(scope, async (tx) => {
      const rows = await tx<MemoryRow[]>`
        UPDATE memories SET
            content = COALESCE(${change.content ?? null}::text, content),
            kind = COALESCE(${change.kind ?? null}::text, kind),
            pinned = COALESCE(${change.pinned ?? null}::boolean, pinned),
            embedding = COALESCE(${embedding}::text::vector, embedding),
            agent_id = CASE
                WHEN ${change.moveMain}::boolean THEN NULL
                ELSE COALESCE(${change.moveTo ?? null}::uuid, agent_id)
            END,
            updated_at = now()
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
        RETURNING
            id, workspace_id, agent_id, content, kind, source_message_id,
            pinned, created_by, superseded_by, created_at, updated_at
      `;

      const row = rows[0];
      if (!row) {
        throw wrap(ErrNotFound, "update memory");
      }
      return fromUpdateRow(row);
    });
  }

  /** Removes a memory and its vector. */
  async deleteMemory(scope: Scope, id: string): Promise<void> {
    // name: DeleteMemory
    return this.write(scope, async (tx) => {
      const rows = await tx`
        DELETE FROM memories
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
      `;
      if (rows.count === 0) {
        throw ErrNotFound;
      }
    });
  }

  /** Marks an older memory as replaced. */
  async supersede(scope: Scope, older: string, newer: string): Promise<void> {
    // name: SupersedeMemory
    // Only a memory that is still the current one is replaced, so a second pass
    // over the same conversation cannot rewrite history twice.
    await this.write(scope, async (tx) => {
      await tx`
        UPDATE memories
        SET superseded_by = ${optionalID(newer)}::uuid, updated_at = now()
        WHERE id = ${older}
          AND workspace_id = ${scope.workspaceId}
          AND superseded_by IS NULL
      `;
    });
  }

  /** Returns the memories of one layer nearest to a vector. */
  async searchMemories(scope: Scope, query: SearchQuery): Promise<Memory[]> {
    const agentId = optionalID(query.agentId);
    const vector = vectorLiteral(query.vector);

    // name: SearchMemories
    // The layer is the argument, not a filter: a null agent id reads the main
    // memory, a set one reads that Bolu's own. The similarity comes back with
    // the row, because the caller records how close the match was.
    //
    // The operator is pgvector's cosine distance (`<=>`), the same one the Go
    // SQL uses; similarity is `1 - distance`, so a higher number is a closer
    // match.
    return this.read(scope, async (tx) => {
      const rows = await tx<SearchMemoryRow[]>`
        SELECT
            id, workspace_id, agent_id, content, kind, source_message_id,
            pinned, created_by, superseded_by, created_at, updated_at,
            (1.0 - (embedding <=> ${vector}::vector))::float8 AS similarity
        FROM memories
        WHERE workspace_id = ${scope.workspaceId}
          AND superseded_by IS NULL
          AND embedding IS NOT NULL
          AND (
                (${agentId}::uuid IS NULL AND agent_id IS NULL)
                OR agent_id = ${agentId}::uuid
              )
          AND (1.0 - (embedding <=> ${vector}::vector)) >= ${query.minSimilarity}::float8
        ORDER BY embedding <=> ${vector}::vector
        LIMIT ${limitOf(query.limit)}
      `;

      return rows.map(fromSearchRow);
    });
  }

  // ------------------------------------------------------------ knowledge

  /** Stores the chunks of one document as pending rows. */
  async createKnowledge(scope: Scope, items: NewKnowledge[]): Promise<KnowledgeItem[]> {
    if (items.length === 0) {
      return [];
    }

    const agentId = items[0].agentId;
    const chunks: string[] = [];
    for (const item of items) {
      if (item.agentId !== agentId) {
        throw wrap(ErrInvalidInput, "one upload belongs to one layer");
      }
      chunks.push(item.content);
    }

    // name: CreateKnowledge
    // The chunks of one document arrive together and start as `pending`: the row
    // exists, so an upload is visible while it is being embedded, and nothing
    // reads it until the vector is there.
    return this.write(scope, async (tx) => {
      const rows = await tx<KnowledgeRow[]>`
        INSERT INTO knowledge_items (workspace_id, agent_id, source, chunk_index, content, status)
        SELECT
            ${scope.workspaceId},
            ${optionalID(agentId)}::uuid,
            ${items[0].source},
            chunk.index - 1,
            chunk.content,
            'pending'
        FROM unnest(${chunks}::text[]) WITH ORDINALITY AS chunk(content, index)
        WHERE ${optionalID(agentId)}::uuid IS NULL
           OR EXISTS (
                SELECT 1 FROM agents a
                WHERE a.id = ${optionalID(agentId)}::uuid AND a.workspace_id = ${scope.workspaceId}
           )
        RETURNING id, workspace_id, agent_id, source, chunk_index, content, status, created_at
      `;

      if (rows.length !== chunks.length) {
        // The guard clause refuses a Bolu from another workspace, and the answer
        // is then "no rows". Saying so is better than storing half a document.
        throw wrap(ErrInvalidInput, "the bolu is not in this workspace");
      }

      return rows.map(fromCreateKnowledgeRow);
    });
  }

  /**
   * Stores the vectors of the chunks that were written, in one transaction: a
   * document is either embedded or not, never half.
   */
  async embedKnowledge(scope: Scope, ids: string[], vectors: number[][]): Promise<void> {
    if (ids.length !== vectors.length) {
      throw wrap(ErrInvalidInput, `${ids.length} chunks and ${vectors.length} vectors`);
    }

    // name: SetKnowledgeEmbedding
    await this.write(scope, async (tx) => {
      for (let i = 0; i < ids.length; i++) {
        await tx`
          UPDATE knowledge_items
          SET embedding = ${vectorLiteral(vectors[i])}::vector, status = 'ready'
          WHERE id = ${ids[i]} AND workspace_id = ${scope.workspaceId}
        `;
      }
    });
  }

  /** Settles chunks that could not be embedded. */
  async failKnowledge(scope: Scope, ids: string[]): Promise<void> {
    if (ids.length === 0) {
      return;
    }

    // name: FailKnowledge
    // A chunk whose embedding failed keeps its row and says so. It is not
    // searchable — every read requires `ready` — and it is not left in a state
    // nobody can explain.
    await this.write(scope, async (tx) => {
      await tx`
        UPDATE knowledge_items
        SET status = 'failed'
        WHERE workspace_id = ${scope.workspaceId}
          AND id = ANY(${ids}::uuid[])
          AND status <> 'ready'
      `;
    });
  }

  /** Returns one chunk. */
  async knowledge(scope: Scope, id: string): Promise<KnowledgeItem> {
    // name: GetKnowledge
    return this.read(scope, async (tx) => {
      const rows = await tx<KnowledgeRow[]>`
        SELECT id, workspace_id, agent_id, source, chunk_index, content, status, created_at
        FROM knowledge_items
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
      `;

      const row = rows[0];
      if (!row) {
        throw wrap(ErrNotFound, "get knowledge");
      }
      return fromGetKnowledgeRow(row);
    });
  }

  /**
   * Returns the chunks the screen shows, including the ones whose embedding
   * failed: an upload that did not work is something a person has to be able to
   * see.
   */
  async listKnowledge(scope: Scope, filter: KnowledgeFilter): Promise<KnowledgeItem[]> {
    const agentId = optionalID(filter.agentId);
    const status = optionalString(filter.status);
    const source = optionalString(filter.source);
    const query = optionalString(filter.query);

    // name: ListKnowledge
    return this.read(scope, async (tx) => {
      const rows = await tx<KnowledgeRow[]>`
        SELECT id, workspace_id, agent_id, source, chunk_index, content, status, created_at
        FROM knowledge_items
        WHERE workspace_id = ${scope.workspaceId}
          AND (${agentId}::uuid IS NULL OR agent_id = ${agentId}::uuid)
          AND (${status}::text IS NULL OR status = ${status}::text)
          AND (${source}::text IS NULL OR source = ${source}::text)
          AND (
                ${query}::text IS NULL
                OR content ILIKE '%' || ${query}::text || '%'
              )
        ORDER BY created_at DESC, chunk_index ASC
        LIMIT ${limitOf(filter.limit)}
      `;

      return rows.map(fromListKnowledgeRow);
    });
  }

  /** Removes one chunk and its vector. */
  async deleteKnowledge(scope: Scope, id: string): Promise<void> {
    // name: DeleteKnowledge
    return this.write(scope, async (tx) => {
      const rows = await tx`
        DELETE FROM knowledge_items
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
      `;
      if (rows.count === 0) {
        throw ErrNotFound;
      }
    });
  }

  /** Returns the ready chunks nearest to a vector. */
  async searchKnowledge(scope: Scope, query: SearchQuery): Promise<KnowledgeItem[]> {
    const agentId = optionalID(query.agentId);
    const vector = vectorLiteral(query.vector);

    // name: SearchKnowledge
    // A Bolu's search reads the workspace's documents and its own; only `ready`
    // chunks are returned, so a failed upload can never reach a prompt.
    return this.read(scope, async (tx) => {
      const rows = await tx<SearchKnowledgeRow[]>`
        SELECT
            id, workspace_id, agent_id, source, chunk_index, content, status, created_at,
            (1.0 - (embedding <=> ${vector}::vector))::float8 AS similarity
        FROM knowledge_items
        WHERE workspace_id = ${scope.workspaceId}
          AND status = 'ready'
          AND embedding IS NOT NULL
          AND (${agentId}::uuid IS NULL OR agent_id IS NULL OR agent_id = ${agentId}::uuid)
          AND (1.0 - (embedding <=> ${vector}::vector)) >= ${query.minSimilarity}::float8
        ORDER BY embedding <=> ${vector}::vector
        LIMIT ${limitOf(query.limit)}
      `;

      return rows.map(fromSearchKnowledgeRow);
    });
  }

  // ------------------------------------------------------- what was retrieved

  /** Notes one chunk that reached a prompt. */
  async recordRetrieval(scope: Scope, usage: Retrieval): Promise<void> {
    // name: RecordRetrieval
    await this.write(scope, async (tx) => {
      await tx`
        INSERT INTO retrieval_usages (workspace_id, task_id, agent_id, memory_id, knowledge_item_id, similarity)
        VALUES (
            ${scope.workspaceId},
            ${optionalID(usage.taskId)}::uuid,
            ${optionalID(usage.agentId)}::uuid,
            ${optionalID(usage.memoryId)}::uuid,
            ${optionalID(usage.knowledgeId)}::uuid,
            ${usage.similarity}
        )
      `;
    });
  }

  // --------------------------------------------------------------- agents

  /** Returns the Bolu of the workspace with the name and the switch. */
  async agents(scope: Scope): Promise<AgentInfo[]> {
    // name: ListWorkspaceAgents
    // The rows belong to the agent module; this reads the two fields memory
    // needs.
    return this.read(scope, async (tx) => {
      const rows = await tx<
        { id: string; name: string; memory_extraction_enabled: boolean }[]
      >`
        SELECT id, name, memory_extraction_enabled FROM agents
        WHERE workspace_id = ${scope.workspaceId}
        ORDER BY name
      `;

      return rows.map((row) => ({
        id: row.id,
        name: row.name,
        extractionEnabled: row.memory_extraction_enabled,
      }));
    });
  }

  /** Reports one Bolu's switch. */
  async extractionEnabled(scope: Scope, agentId: string): Promise<boolean> {
    // name: AgentExtractionEnabled
    return this.read(scope, async (tx) => {
      const rows = await tx<{ memory_extraction_enabled: boolean }[]>`
        SELECT memory_extraction_enabled FROM agents
        WHERE id = ${agentId} AND workspace_id = ${scope.workspaceId}
      `;

      const row = rows[0];
      if (!row) {
        throw wrap(ErrNotFound, "agent extraction switch");
      }
      return row.memory_extraction_enabled;
    });
  }

  // -------------------------------------------------------------- helpers

  private async read<T>(scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    return this.db.inScopeRead(databaseScope(scope), fn);
  }

  private async write<T>(scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    return this.db.inScope(databaseScope(scope), fn);
  }
}

function databaseScope(scope: Scope): DatabaseScope {
  return { userId: scope.userId ?? null, workspaceId: scope.workspaceId };
}

/**
 * Renders a vector the way Postgres reads it, which is the one representation
 * both sides of the wire agree on without the driver knowing the type.
 */
export function vectorLiteral(vector: number[]): string {
  return JSON.stringify(vector);
}

function optionalID(id: string | null | undefined): string | null {
  return isNilID(id) ? null : (id as string);
}

function optionalString(value: string | null | undefined): string | null {
  return value ? value : null;
}

/** The ceiling of a list read, so one request cannot pull a whole knowledge base. */
const listLimit = 200;

function limitOf(limit: number): number {
  if (limit <= 0) {
    return 50;
  }
  if (limit > listLimit) {
    return listLimit;
  }
  return limit;
}
