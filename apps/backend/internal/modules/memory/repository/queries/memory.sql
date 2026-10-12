-- Memory and knowledge queries. Every statement carries workspace_id as well as
-- the Row Level Security policy, and every similarity read filters by layer as
-- well: a private memory that leaked into another Bolu's prompt would be as
-- wrong as one that leaked into another workspace.
--
-- Two conventions run through this file:
--
--   * a vector never crosses the wire as a value. It is sent as text with an
--     explicit cast, and it is not in any column list on the way back: the
--     similarity a caller needs is computed by Postgres, and shipping 1536
--     floats to a handler that will not read them is waste.
--   * every read says which columns it wants, for the same reason.

-- ------------------------------------------------------------------- memory

-- name: CreateMemory :one
-- A memory for one Bolu is only written when that Bolu is in this workspace. The
-- statement checks it itself because a foreign key runs with the table owner's
-- rights and bypasses Row Level Security.
INSERT INTO memories (
    workspace_id, agent_id, content, kind, source_message_id, embedding, pinned, created_by
)
SELECT
    sqlc.arg('workspace_id'),
    sqlc.narg('agent_id'),
    sqlc.arg('content'),
    sqlc.arg('kind'),
    sqlc.narg('source_message_id'),
    sqlc.arg('embedding')::vector,
    sqlc.arg('pinned'),
    sqlc.arg('created_by')
WHERE sqlc.narg('agent_id')::uuid IS NULL
   OR EXISTS (
        SELECT 1 FROM agents a
        WHERE a.id = sqlc.narg('agent_id') AND a.workspace_id = sqlc.arg('workspace_id')
   )
RETURNING
    id, workspace_id, agent_id, content, kind, source_message_id,
    pinned, created_by, superseded_by, created_at, updated_at;

-- name: GetMemory :one
SELECT
    id, workspace_id, agent_id, content, kind, source_message_id,
    pinned, created_by, superseded_by, created_at, updated_at
FROM memories
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id');

-- name: ListMemories :many
-- The screen reads this: a layer, a search term, and the history when asked for.
SELECT
    id, workspace_id, agent_id, content, kind, source_message_id,
    pinned, created_by, superseded_by, created_at, updated_at
FROM memories
WHERE workspace_id = sqlc.arg('workspace_id')
  AND (
        sqlc.narg('agent_id')::uuid IS NULL
        OR agent_id = sqlc.narg('agent_id')
      )
  AND (NOT sqlc.arg('main_only')::boolean OR agent_id IS NULL)
  AND (sqlc.arg('include_superseded')::boolean OR superseded_by IS NULL)
  AND (
        sqlc.narg('query')::text IS NULL
        OR content ILIKE '%' || sqlc.narg('query')::text || '%'
      )
ORDER BY pinned DESC, updated_at DESC
LIMIT sqlc.arg('limit');

-- name: UpdateMemory :one
-- One statement covers every edit the screen offers. A null keeps the stored
-- value; moving a memory to the main layer is its own flag because an absent
-- agent id already means "leave the layer alone".
UPDATE memories SET
    content = COALESCE(sqlc.narg('content')::text, content),
    kind = COALESCE(sqlc.narg('kind')::text, kind),
    pinned = COALESCE(sqlc.narg('pinned')::boolean, pinned),
    embedding = COALESCE(sqlc.narg('embedding')::text::vector, embedding),
    agent_id = CASE
        WHEN sqlc.arg('move_to_main')::boolean THEN NULL
        ELSE COALESCE(sqlc.narg('agent_id')::uuid, agent_id)
    END,
    updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
RETURNING
    id, workspace_id, agent_id, content, kind, source_message_id,
    pinned, created_by, superseded_by, created_at, updated_at;

-- name: DeleteMemory :execrows
DELETE FROM memories
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id');

-- name: SupersedeMemory :exec
-- Only a memory that is still the current one is replaced, so a second pass over
-- the same conversation cannot rewrite history twice.
UPDATE memories
SET superseded_by = sqlc.arg('newer'), updated_at = now()
WHERE id = sqlc.arg('older')
  AND workspace_id = sqlc.arg('workspace_id')
  AND superseded_by IS NULL;

-- name: SearchMemories :many
-- The layer is the argument, not a filter: a null agent id reads the main
-- memory, a set one reads that Bolu's own. The similarity comes back with the
-- row, because the caller records how close the match was.
SELECT
    id, workspace_id, agent_id, content, kind, source_message_id,
    pinned, created_by, superseded_by, created_at, updated_at,
    (1.0 - (embedding <=> sqlc.arg('vector')::vector))::float8 AS similarity
FROM memories
WHERE workspace_id = sqlc.arg('workspace_id')
  AND superseded_by IS NULL
  AND embedding IS NOT NULL
  AND (
        (sqlc.narg('agent_id')::uuid IS NULL AND agent_id IS NULL)
        OR agent_id = sqlc.narg('agent_id')
      )
  AND (1.0 - (embedding <=> sqlc.arg('vector')::vector)) >= sqlc.arg('min_similarity')::float8
ORDER BY embedding <=> sqlc.arg('vector')::vector
LIMIT sqlc.arg('limit');

-- ---------------------------------------------------------------- knowledge

-- name: CreateKnowledge :many
-- The chunks of one document arrive together and start as `pending`: the row
-- exists, so an upload is visible while it is being embedded, and nothing reads
-- it until the vector is there.
INSERT INTO knowledge_items (workspace_id, agent_id, source, chunk_index, content, status)
SELECT
    sqlc.arg('workspace_id'),
    sqlc.narg('agent_id'),
    sqlc.arg('source'),
    chunk.index - 1,
    chunk.content,
    'pending'
FROM unnest(sqlc.arg('chunks')::text[]) WITH ORDINALITY AS chunk(content, index)
WHERE sqlc.narg('agent_id')::uuid IS NULL
   OR EXISTS (
        SELECT 1 FROM agents a
        WHERE a.id = sqlc.narg('agent_id') AND a.workspace_id = sqlc.arg('workspace_id')
   )
RETURNING id, workspace_id, agent_id, source, chunk_index, content, status, created_at;

-- name: SetKnowledgeEmbedding :exec
UPDATE knowledge_items
SET embedding = sqlc.arg('embedding')::vector, status = 'ready'
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id');

-- name: FailKnowledge :exec
-- A chunk whose embedding failed keeps its row and says so. It is not
-- searchable — every read requires `ready` — and it is not left in a state
-- nobody can explain.
UPDATE knowledge_items
SET status = 'failed'
WHERE workspace_id = sqlc.arg('workspace_id')
  AND id = ANY(sqlc.arg('ids')::uuid[])
  AND status <> 'ready';

-- name: GetKnowledge :one
SELECT id, workspace_id, agent_id, source, chunk_index, content, status, created_at
FROM knowledge_items
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id');

-- name: ListKnowledge :many
SELECT id, workspace_id, agent_id, source, chunk_index, content, status, created_at
FROM knowledge_items
WHERE workspace_id = sqlc.arg('workspace_id')
  AND (sqlc.narg('agent_id')::uuid IS NULL OR agent_id = sqlc.narg('agent_id'))
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'))
  AND (sqlc.narg('source')::text IS NULL OR source = sqlc.narg('source'))
  AND (
        sqlc.narg('query')::text IS NULL
        OR content ILIKE '%' || sqlc.narg('query')::text || '%'
      )
ORDER BY created_at DESC, chunk_index ASC
LIMIT sqlc.arg('limit');

-- name: DeleteKnowledge :execrows
DELETE FROM knowledge_items
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id');

-- name: SearchKnowledge :many
-- A Bolu's search reads the workspace's documents and its own; only `ready`
-- chunks are returned, so a failed upload can never reach a prompt.
SELECT
    id, workspace_id, agent_id, source, chunk_index, content, status, created_at,
    (1.0 - (embedding <=> sqlc.arg('vector')::vector))::float8 AS similarity
FROM knowledge_items
WHERE workspace_id = sqlc.arg('workspace_id')
  AND status = 'ready'
  AND embedding IS NOT NULL
  AND (sqlc.narg('agent_id')::uuid IS NULL OR agent_id IS NULL OR agent_id = sqlc.narg('agent_id'))
  AND (1.0 - (embedding <=> sqlc.arg('vector')::vector)) >= sqlc.arg('min_similarity')::float8
ORDER BY embedding <=> sqlc.arg('vector')::vector
LIMIT sqlc.arg('limit');

-- ------------------------------------------------------- what was retrieved

-- name: RecordRetrieval :exec
INSERT INTO retrieval_usages (workspace_id, task_id, agent_id, memory_id, knowledge_item_id, similarity)
VALUES (
    sqlc.arg('workspace_id'),
    sqlc.narg('task_id'),
    sqlc.narg('agent_id'),
    sqlc.narg('memory_id'),
    sqlc.narg('knowledge_item_id'),
    sqlc.arg('similarity')
);

-- ------------------------------------------------------------------- agents

-- name: ListWorkspaceAgents :many
-- The rows belong to the agent module; this reads the two fields memory needs.
SELECT id, name, memory_extraction_enabled FROM agents
WHERE workspace_id = sqlc.arg('workspace_id')
ORDER BY name;

-- name: AgentExtractionEnabled :one
SELECT memory_extraction_enabled FROM agents
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id');
