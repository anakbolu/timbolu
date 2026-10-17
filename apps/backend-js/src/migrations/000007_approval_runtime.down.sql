-- Reverses 000007.
--
-- The seeded policy rows are removed only where they were never edited: a row the
-- API wrote carries `updated_by`, so a workspace that changed its mode keeps that
-- change through a rollback. Re-running the up migration seeds the rest again,
-- which is what keeps the up/down/up cycle in the test suite meaningful.

DROP POLICY IF EXISTS draft_revisions_tenant_isolation ON draft_revisions;
DROP TABLE IF EXISTS draft_revisions;

DROP INDEX IF EXISTS drafts_task_created_idx;
DROP INDEX IF EXISTS drafts_agent_created_idx;
DROP INDEX IF EXISTS drafts_workspace_status_created_idx;

ALTER TABLE drafts DROP CONSTRAINT IF EXISTS drafts_held_has_reason;
ALTER TABLE drafts DROP CONSTRAINT IF EXISTS drafts_sent_has_timestamp;

ALTER TABLE drafts DROP COLUMN IF EXISTS card_message_id;
ALTER TABLE drafts DROP COLUMN IF EXISTS summary;
ALTER TABLE drafts DROP COLUMN IF EXISTS title;
ALTER TABLE drafts DROP COLUMN IF EXISTS blocked_reason;
ALTER TABLE drafts DROP COLUMN IF EXISTS not_before;

DELETE FROM approval_policies
WHERE mode = 'always_draft' AND updated_by IS NULL;
