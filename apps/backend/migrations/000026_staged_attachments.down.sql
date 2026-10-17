-- The reverse of 000026: an attachment is a file that belongs to a message again.
--
-- A staged row cannot be reversed into that shape — it has no message to point at, and the old
-- column is NOT NULL — so the unbound rows are removed first. They are files nobody has named in a
-- message: the bytes are still in object storage, and this file cannot remove them, so a deployment
-- that rolls back keeps an object nobody points at rather than a row it cannot write. The
-- conversation and owner columns are dropped rather than kept, because the shape they were added
-- for is the one being removed.

DELETE FROM message_attachments WHERE message_id IS NULL;

DROP INDEX message_attachments_staged_idx;

ALTER TABLE message_attachments
    DROP CONSTRAINT message_attachments_binding,
    DROP COLUMN staged_at,
    DROP COLUMN status,
    DROP COLUMN user_id,
    DROP COLUMN conversation_id,
    ALTER COLUMN message_id SET NOT NULL;
