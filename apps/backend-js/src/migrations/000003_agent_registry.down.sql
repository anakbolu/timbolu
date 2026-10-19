-- Reverse of 000003_agent_registry.up.sql.

DROP INDEX IF EXISTS tasks_agent_live_idx;
DROP INDEX IF EXISTS drafts_agent_status_idx;

DROP TABLE IF EXISTS tool_catalog;

DROP INDEX IF EXISTS agents_live_idx;

ALTER TABLE agents DROP COLUMN IF EXISTS tools;
ALTER TABLE agents DROP COLUMN IF EXISTS deleted_at;
