-- Work threads: a conversation a Bolu opens from another one to carry a piece of work.
--
-- A thread is a conversation like any other — the same messages, the same reply path, the same
-- stream — so it is a kind of conversation rather than a table of its own. What makes it a thread
-- is the conversation it was opened from, which is what lets the chat show the threads of the room
-- a person is in, and the room a thread came from. The link cascades: a thread belongs to the room
-- it was opened from, and erasing the room erases the work that was steered from it.
--
-- A thread is never opened from a thread. That rule is the service's, not the schema's, because
-- the refusal is a sentence the model has to read; the column alone cannot say why.

ALTER TABLE conversations
    DROP CONSTRAINT conversations_kind_check,
    ADD CONSTRAINT conversations_kind_check CHECK (kind IN ('direct', 'group', 'thread')),
    ADD COLUMN parent_conversation_id UUID REFERENCES conversations (id) ON DELETE CASCADE,
    ADD CONSTRAINT conversations_thread_parent CHECK ((kind = 'thread') = (parent_conversation_id IS NOT NULL));

CREATE INDEX conversations_parent_idx ON conversations (parent_conversation_id)
    WHERE parent_conversation_id IS NOT NULL;

-- The connector can now open a pull request. It writes outside Bolu, so it is a
-- `write_external` row and every workspace that already exists gets the conservative policy for
-- it, the same way 000025 did for `github.create_issue`.
INSERT INTO tool_catalog (name, integration_app, label, description) VALUES
    ('github.create_pull_request', 'github', 'write_external',
     'Buka pull request dari sebuah branch di repositori GitHub. Membutuhkan persetujuan manusia.')
ON CONFLICT (name) DO NOTHING;

INSERT INTO approval_policies (workspace_id, action_kind, mode)
SELECT w.id, 'github.create_pull_request', 'always_draft'
FROM workspaces w
ON CONFLICT (workspace_id, action_kind) DO NOTHING;
