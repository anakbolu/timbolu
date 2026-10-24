-- The reverse of 000027: threads go, with their messages, and the pull request tool leaves the
-- catalogue.
--
-- A thread cannot be turned back into a direct conversation — it may hold a Bolu and a person
-- that already have a direct one — so the rows are removed rather than reshaped.

DELETE FROM approval_policies WHERE action_kind = 'github.create_pull_request';
DELETE FROM tool_catalog WHERE name = 'github.create_pull_request';

DELETE FROM conversations WHERE kind = 'thread';

DROP INDEX conversations_parent_idx;

ALTER TABLE conversations
    DROP CONSTRAINT conversations_thread_parent,
    DROP COLUMN parent_conversation_id,
    DROP CONSTRAINT conversations_kind_check,
    ADD CONSTRAINT conversations_kind_check CHECK (kind IN ('direct', 'group'));
