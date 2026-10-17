DROP POLICY IF EXISTS retrieval_usages_tenant_isolation ON retrieval_usages;

DROP INDEX IF EXISTS memories_main_idx;
DROP INDEX IF EXISTS knowledge_items_workspace_status_idx;
DROP INDEX IF EXISTS retrieval_usages_workspace_idx;

DROP TABLE IF EXISTS retrieval_usages;

ALTER TABLE agents DROP COLUMN IF EXISTS memory_extraction_enabled;
