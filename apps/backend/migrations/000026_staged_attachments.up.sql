-- EPIC · Staged attachments: a file is uploaded before the message that carries it.
--
-- The upload used to be two requests in the wrong order. The client sent the message first — with
-- `reply: true` — and only then uploaded the file against the message id that came back, so the
-- reply task had already been started by the time the attachment was linked and the model never
-- saw the file on that turn. The fix is to let a file exist on its own: upload it, then name it in
-- the message, and bind the two inside the transaction that stores the message.
--
-- What that needs from the schema is a row that is allowed to have no message yet, which is why
-- `message_id` stops being NOT NULL. Everything else here exists to keep such a row honest:
--
--   * `conversation_id` and `user_id` are the two facts a bind is checked against. A staged file
--     may only be bound by the person who uploaded it, in the conversation it was uploaded to, so
--     an attachment id from somewhere else cannot be attached to somebody else's message. The
--     columns are filled for rows that already exist as well, from the message each points at, so
--     there is one meaning for the column rather than "set for new rows".
--   * `status` is the binding state, and the check below ties it to `message_id` in both
--     directions. The two must never disagree: a row marked bound with no message would be a file
--     no message renders, and one marked staged with a message would be swept away by the staged
--     sweep while a client is still showing it.
--   * `staged_at` is when the file was uploaded, which is the age the staged sweep reads. It is
--     kept when the row is bound: it says when the upload happened, and nothing reads it for a
--     bound row.
--
-- The staged sweep also needs its own read: unbound rows past their window, per workspace. It is
-- partial on `status = 'staged'` because a bound row is not a candidate — a total index would keep
-- growing with rows the sweep has already finished with, exactly like the step prune's index in
-- 000018.

ALTER TABLE message_attachments
    ALTER COLUMN message_id DROP NOT NULL,
    ADD COLUMN conversation_id UUID REFERENCES conversations (id) ON DELETE CASCADE,
    ADD COLUMN user_id UUID REFERENCES users (id) ON DELETE SET NULL,
    ADD COLUMN status TEXT NOT NULL DEFAULT 'bound'
        CHECK (status IN ('staged', 'bound')),
    ADD COLUMN staged_at TIMESTAMPTZ,
    ADD CONSTRAINT message_attachments_binding
        CHECK ((status = 'bound') = (message_id IS NOT NULL));

-- Every row that exists was written by the upload that carried its message, so it is bound and its
-- conversation is the message's. The status is the column default; the conversation is derived
-- here rather than left null so the ownership rule a bind applies reads the same for old rows.
UPDATE message_attachments a
SET conversation_id = m.conversation_id
FROM messages m
WHERE a.message_id = m.id
  AND a.conversation_id IS NULL;

-- The staged sweep's read, by age: the oldest unbound file first, so the job makes progress in
-- bounded batches.
CREATE INDEX message_attachments_staged_idx
    ON message_attachments (workspace_id, staged_at)
    WHERE status = 'staged';
