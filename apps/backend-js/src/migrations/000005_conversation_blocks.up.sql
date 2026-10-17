-- Conversation history: the message status a streamed reply needs, attachment
-- metadata, and the indexes the cursor pagination and the event replay read.
--
-- The tables themselves come from 000001; this migration adds what the chat
-- module needs on top of them.

-- ------------------------------------------------------------ message status

-- A reply is written before it is finished, so a client can follow it while it
-- grows and so a stream that dies leaves the tokens it produced behind rather
-- than losing them. `status` says which of those states a row is in.
--
--   complete  — the answer finished
--   streaming — the answer is still arriving
--   partial   — the stream ended early; the body is what arrived
--   failed    — the provider refused or errored; the body may be empty
ALTER TABLE messages ADD COLUMN status TEXT NOT NULL DEFAULT 'complete'
    CHECK (status IN ('complete', 'streaming', 'partial', 'failed'));

-- Why a reply stopped early, in the provider's own words. Kept separate from
-- `status` so the UI can explain a partial answer instead of just marking it.
ALTER TABLE messages ADD COLUMN finish_reason TEXT NOT NULL DEFAULT '';

-- The edited timestamp, so a streamed message that grows is distinguishable
-- from one that was written once.
ALTER TABLE messages ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- A message may point at the task that produced it. The column existed from the
-- start; the constraint arrives now, because `tasks` is created later in 000001
-- than `messages` and a foreign key needs its target to exist.
ALTER TABLE messages
    ADD CONSTRAINT messages_task_id_fkey
    FOREIGN KEY (task_id) REFERENCES tasks (id) ON DELETE SET NULL;

-- A conversation's open replies, which the UI shows as "sedang mengetik".
CREATE INDEX messages_streaming_idx ON messages (workspace_id, conversation_id, status)
    WHERE status IN ('streaming', 'partial', 'failed');

-- ------------------------------------------------------------- attachments

-- Attachment bytes live in object storage; only the metadata is a row, so a
-- large file never passes through Postgres and a message read stays cheap.
--
-- `storage_key` is what the storage layer resolves. It is unique because two
-- attachments sharing a key would mean one overwrote the other's bytes.
CREATE TABLE message_attachments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    message_id UUID NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    storage_key TEXT NOT NULL UNIQUE,
    filename TEXT NOT NULL,
    content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
    byte_size BIGINT NOT NULL CHECK (byte_size >= 0),
    checksum_sha256 TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX message_attachments_message_idx
    ON message_attachments (workspace_id, message_id);

ALTER TABLE message_attachments ENABLE ROW LEVEL SECURITY;
ALTER TABLE message_attachments FORCE ROW LEVEL SECURITY;
CREATE POLICY message_attachments_tenant_isolation ON message_attachments
    USING (workspace_id = app_current_workspace_id())
    WITH CHECK (workspace_id = app_current_workspace_id());

-- ------------------------------------------------------------- event replay

-- A reconnecting client asks for everything after the last ID it saw. The
-- existing index (workspace_id, id) already serves that read; what it does not
-- serve is the per-type filter the feed and the office use to skip events they
-- do not render.
CREATE INDEX activity_events_workspace_type_idx
    ON activity_events (workspace_id, type, id DESC);

-- The office derives a Bolu's state from its work, and the router reads a
-- conversation's Bolu. Both walk the participant table by conversation, which
-- the unique indexes do not cover for the agent-only case.
CREATE INDEX conversation_participants_conversation_idx
    ON conversation_participants (conversation_id);
