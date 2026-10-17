-- The reverse of 000018: the retention job's own bookkeeping and the two indexes its reads use.
--
-- What is *not* reversed is the pruning itself, and that is the point worth stating: raw step
-- contents this deployment already removed are gone for good, and no rollback brings them
-- back. The rows stay — with their metadata, their token counts, and their `pruned_at` — so a
-- task history still reads the same after this file runs.

DROP INDEX IF EXISTS message_attachments_created_idx;
DROP INDEX IF EXISTS task_steps_unpruned_created_idx;

DROP INDEX IF EXISTS retention_runs_started_idx;
DROP TABLE IF EXISTS retention_runs;

DROP TRIGGER IF EXISTS workspaces_queue_retention ON workspaces;
DROP FUNCTION IF EXISTS queue_retention_target();

DROP INDEX IF EXISTS retention_targets_due_idx;
DROP TABLE IF EXISTS retention_targets;
