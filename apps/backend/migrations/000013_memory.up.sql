-- EPIC 10 — memory and knowledge (RAG).
--
-- The tables themselves (`knowledge_items`, `memories`) were created by the
-- bootstrap migration; this one adds what the runtime needs on top of them:
-- the per-Bolu extraction switch, the record of which chunks a round was given,
-- and the indexes the two retrieval queries read.

-- A Bolu of a workspace that does not want automatic extraction keeps the
-- switch off. It defaults on, because a memory that is never written is a
-- feature nobody notices.
ALTER TABLE agents ADD COLUMN memory_extraction_enabled BOOLEAN NOT NULL DEFAULT true;

-- One row per chunk a round was given. It is what makes "which memories and
-- documents did this answer actually see" answerable after the fact, which is
-- the only way to evaluate retrieval quality.
--
-- The task id is kept without a hard dependency on the task surviving: it is a
-- pointer for a person reading the record, and a pruned task must not erase the
-- measurement. A memory or a document that is deleted takes its records with it,
-- because a row naming something that no longer exists is a count nobody can
-- interpret — and a permanent delete has to leave nothing behind.
CREATE TABLE retrieval_usages (
    id BIGSERIAL PRIMARY KEY,
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    task_id UUID,
    agent_id UUID REFERENCES agents (id) ON DELETE CASCADE,
    memory_id UUID REFERENCES memories (id) ON DELETE CASCADE,
    knowledge_item_id UUID REFERENCES knowledge_items (id) ON DELETE CASCADE,
    similarity REAL NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Exactly one of the two names a chunk: a row that names both, or neither,
    -- is not a retrieval.
    CONSTRAINT retrieval_usages_one_chunk
        CHECK ((memory_id IS NULL) <> (knowledge_item_id IS NULL))
);

CREATE INDEX retrieval_usages_workspace_idx ON retrieval_usages (workspace_id, created_at DESC);

-- The upload monitor reads "what is still pending or failed in this workspace".
CREATE INDEX knowledge_items_workspace_status_idx ON knowledge_items (workspace_id, status);

-- The main-memory read is the workspace-wide rows, and the pinned ones first.
CREATE INDEX memories_main_idx ON memories (workspace_id, updated_at DESC)
    WHERE agent_id IS NULL AND superseded_by IS NULL;

ALTER TABLE retrieval_usages ENABLE ROW LEVEL SECURITY;
ALTER TABLE retrieval_usages FORCE ROW LEVEL SECURITY;

CREATE POLICY retrieval_usages_tenant_isolation ON retrieval_usages
    USING (workspace_id = app_current_workspace_id())
    WITH CHECK (workspace_id = app_current_workspace_id());
