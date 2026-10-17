DROP INDEX IF EXISTS task_steps_task_created_idx;
DROP INDEX IF EXISTS tasks_parent_idx;
DROP INDEX IF EXISTS tasks_live_idx;
DROP INDEX IF EXISTS tasks_workspace_status_created_idx;

ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_started_has_workflow;
ALTER TABLE tasks DROP COLUMN IF EXISTS reply_message_id;
ALTER TABLE tasks DROP COLUMN IF EXISTS waiting_draft_id;
ALTER TABLE tasks DROP COLUMN IF EXISTS waiting_reason;
ALTER TABLE tasks DROP COLUMN IF EXISTS heartbeat_at;
ALTER TABLE tasks DROP COLUMN IF EXISTS depth;
