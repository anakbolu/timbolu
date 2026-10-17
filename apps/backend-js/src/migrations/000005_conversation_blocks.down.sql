DROP INDEX IF EXISTS conversation_participants_conversation_idx;
DROP INDEX IF EXISTS activity_events_workspace_type_idx;

DROP POLICY IF EXISTS message_attachments_tenant_isolation ON message_attachments;
DROP INDEX IF EXISTS message_attachments_message_idx;
DROP TABLE IF EXISTS message_attachments;

DROP INDEX IF EXISTS messages_streaming_idx;
ALTER TABLE messages DROP CONSTRAINT IF EXISTS messages_task_id_fkey;
ALTER TABLE messages DROP COLUMN IF EXISTS updated_at;
ALTER TABLE messages DROP COLUMN IF EXISTS finish_reason;
ALTER TABLE messages DROP COLUMN IF EXISTS status;
