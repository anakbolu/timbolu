-- Conversation queries. Every statement carries workspace_id as well as the
-- Row Level Security policy, so a wrong id in application code still cannot
-- reach another tenant's row.

-- name: ListConversations :many
-- The sidebar: threads with their message count and last activity, so the list
-- renders without a query per row.
--
-- Last activity falls back to the creation time rather than being nullable: a
-- thread nobody has written in yet sorts by when it was opened, which is exactly
-- the order the sidebar wants and one fewer null to handle everywhere.
SELECT c.id, c.workspace_id, c.kind, c.title, c.created_at, c.updated_at,
       (SELECT count(*) FROM messages m WHERE m.conversation_id = c.id)::bigint AS message_count,
       COALESCE(
           (SELECT max(m.created_at) FROM messages m WHERE m.conversation_id = c.id),
           c.created_at
       )::timestamptz AS last_activity_at
FROM conversations c
WHERE c.workspace_id = $1
ORDER BY last_activity_at DESC;

-- name: GetConversation :one
SELECT c.id, c.workspace_id, c.kind, c.title, c.created_at, c.updated_at,
       (SELECT count(*) FROM messages m WHERE m.conversation_id = c.id)::bigint AS message_count,
       COALESCE(
           (SELECT max(m.created_at) FROM messages m WHERE m.conversation_id = c.id),
           c.created_at
       )::timestamptz AS last_activity_at
FROM conversations c
WHERE c.id = $1 AND c.workspace_id = $2;

-- name: CreateConversation :one
INSERT INTO conversations (workspace_id, kind, title)
VALUES ($1, $2, $3)
RETURNING id, workspace_id, kind, title, created_at, updated_at;

-- name: RenameConversation :one
UPDATE conversations
SET title = $3, updated_at = now()
WHERE id = $1 AND workspace_id = $2
RETURNING id, workspace_id, kind, title, created_at, updated_at;

-- name: TouchConversation :execrows
-- New activity moves the thread up the list, which is what the sidebar order
-- reads.
UPDATE conversations
SET updated_at = now()
WHERE id = $1 AND workspace_id = $2;

-- name: ListParticipants :many
-- The participant list joins the two possible identities so the UI shows a name
-- and a role without a second query.
--
-- The workspace filter is cast explicitly: a parameter that only appears in a
-- comparison the planner can fold away leaves sqlc without a type to infer.
SELECT p.id, p.conversation_id, p.agent_id, p.user_id, p.created_at,
       COALESCE(a.name, u.email) AS display_name,
       COALESCE(a.role, 'member') AS role
FROM conversation_participants p
LEFT JOIN agents a ON a.id = p.agent_id
LEFT JOIN users u ON u.id = p.user_id
WHERE p.workspace_id = sqlc.arg('workspace_id')::uuid
  AND p.conversation_id = sqlc.arg('conversation_id')::uuid
ORDER BY (p.agent_id IS NULL), display_name;

-- name: AddAgentParticipant :execrows
INSERT INTO conversation_participants (workspace_id, conversation_id, agent_id)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;

-- name: AddUserParticipant :execrows
INSERT INTO conversation_participants (workspace_id, conversation_id, user_id)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;

-- name: FindDirectConversation :one
-- The 1:1 thread with one Bolu. A direct conversation has exactly two
-- participants, so this matches the one whose agent is the asked-for Bolu and
-- which has no third participant.
SELECT c.id, c.workspace_id, c.kind, c.title, c.created_at, c.updated_at
FROM conversations c
JOIN conversation_participants p
    ON p.conversation_id = c.id AND p.agent_id = $2
WHERE c.workspace_id = $1
  AND c.kind = 'direct'
  AND (SELECT count(*) FROM conversation_participants q WHERE q.conversation_id = c.id) = 2
LIMIT 1;

-- name: CountConversations :one
SELECT count(*)::bigint FROM conversations WHERE workspace_id = $1;

-- ---------------------------------------------------------------- messages

-- name: AppendMessage :one
INSERT INTO messages (
    workspace_id, conversation_id, author_agent_id, author_user_id,
    blocks, attachments, task_id, status, finish_reason
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: UpdateMessage :one
-- The body and status of a message that already exists: how a streamed reply is
-- finished, and how a partial one is marked.
UPDATE messages
SET blocks = $3,
    attachments = $4,
    task_id = COALESCE($5, task_id),
    status = $6,
    finish_reason = $7
WHERE id = $1 AND workspace_id = $2
RETURNING *;

-- name: AppendMessageAttachment :one
-- The read model of one message's files, appended in the same transaction as the
-- row that records the file. The metadata is stored twice on purpose: the rows
-- are the storage record, this column is what history and the event stream read,
-- so appending the element here is what keeps a message rendering the file it
-- points at without a read-modify-write of the whole column.
UPDATE messages
SET attachments = COALESCE(attachments, '[]'::jsonb) || jsonb_build_array(sqlc.arg('attachment')::jsonb)
WHERE id = $1 AND workspace_id = $2
RETURNING *;


-- name: GetMessage :one
SELECT * FROM messages WHERE id = $1 AND workspace_id = $2;

-- name: ListMessagesDesc :many
-- One page of history, newest first.
--
-- The cursor names the last message of the previous page, and the row comparison
-- matches the index order exactly, so the boundary is stable while new messages
-- arrive. A zero cursor starts from the newest.
SELECT * FROM messages
WHERE workspace_id = $1
  AND conversation_id = $2
  AND (
      sqlc.narg('before_created_at')::timestamptz IS NULL
      OR (created_at, id) < (sqlc.narg('before_created_at')::timestamptz, sqlc.narg('before_id')::uuid)
  )
ORDER BY created_at DESC, id DESC
LIMIT $3;

-- name: ListMessagesAsc :many
-- The newest messages, oldest first, which is what a chat opens with.
SELECT * FROM (
    SELECT * FROM messages
    WHERE workspace_id = $1 AND conversation_id = $2
    ORDER BY created_at DESC, id DESC
    LIMIT $3
) recent
ORDER BY recent.created_at, recent.id;

-- name: ListMessagesForTask :many
SELECT * FROM messages
WHERE workspace_id = $1 AND task_id = $2
ORDER BY created_at;

-- ------------------------------------------------------------- attachments

-- name: CreateAttachment :one
-- A file that already belongs to a message, which is the shape both older upload paths write:
-- with `message_id` the client names the message, and without it the service opens one first.
-- The row is bound from the moment it exists, which is why `status` is left to its default of
-- 'bound': the check constraint ties that to the message id being present.
--
-- `conversation_id` and `user_id` are recorded here too, not only on the staged path, so one
-- rule reads the same for every row — a file belongs to a thread and to whoever uploaded it.
INSERT INTO message_attachments (
    workspace_id, message_id, conversation_id, user_id, storage_key, filename,
    content_type, byte_size, checksum_sha256
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: CreateStagedAttachment :one
-- An upload that is not part of a message yet: the client sends the file first and names it in
-- the message that follows, so one transaction can bind the two and hand the model the file on
-- the turn it was sent with.
--
-- `message_id` stays NULL, which is what the nullable column is for, and the row says who
-- uploaded the file and where, so a bind can refuse one that came from another thread, another
-- workspace, or another person. `staged_at` is the age the staged sweep reads.
INSERT INTO message_attachments (
    workspace_id, conversation_id, user_id, storage_key, filename, content_type,
    byte_size, checksum_sha256, status, staged_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'staged', now())
RETURNING *;

-- name: LockAttachment :one
-- One attachment of this workspace, locked for the rest of the transaction.
--
-- The row is read rather than assumed because the answers a bind can give are not one failure:
-- a file that is not here is not found, one that is already bound is a conflict, and one that
-- belongs to another thread or another person must not be distinguishable from a file that does
-- not exist. The lock is what keeps a concurrent send from binding the same file twice between
-- this read and the update below.
SELECT * FROM message_attachments
WHERE id = $1 AND workspace_id = $2
FOR UPDATE;

-- name: BindAttachment :one
-- Binds one staged file to the message that named it.
--
-- The `message_id IS NULL` predicate is the guard that matters: two sends racing over one
-- staged file both pass the read above, and the one that loses finds no row here rather than
-- overwriting the message id the winner wrote. `status` moves with `message_id` because the
-- check constraint ties the two together.
UPDATE message_attachments
SET message_id = $3, status = 'bound'
WHERE id = $1 AND workspace_id = $2 AND message_id IS NULL
RETURNING *;

-- name: GetAttachment :one
SELECT * FROM message_attachments WHERE id = $1 AND workspace_id = $2;

-- name: ListAttachmentsForMessage :many
SELECT * FROM message_attachments
WHERE workspace_id = $1 AND message_id = $2
ORDER BY created_at;

-- name: ListExpiredAttachments :many
-- The retention job's read: the attachments of one workspace that are past their
-- window, oldest first.
--
-- It runs in the workspace's scope like every other statement here, and it answers
-- the storage key the bytes live under, because the row and the object are two
-- halves of one attachment and a row removed without its object is a leak the
-- database cannot see.
SELECT * FROM message_attachments
WHERE workspace_id = $1 AND created_at < $2
ORDER BY created_at
LIMIT $3;

-- name: ListStagedExpiredAttachments :many
-- The staged sweep's read: uploads nobody ever named in a message, older than the staged
-- window, oldest first.
--
-- It is a separate read from the retention sweep's above because the two windows mean
-- different things. An attachment past its policy window leaves because the deployment said
-- how long it keeps what a customer sent; a staged one is a file that was uploaded and then
-- never sent at all, so it goes on its own clock whatever the policy is — and a row bound to
-- a message is never a candidate here.
SELECT * FROM message_attachments
WHERE workspace_id = $1 AND status = 'staged' AND staged_at < $2
ORDER BY staged_at
LIMIT $3;

-- name: DeleteAttachment :one
-- The row is the storage record, and it answers the message it belonged to so
-- the read model can be fixed in the same transaction. That answer is empty for
-- a staged upload, which no message lists and which the sweep removes on its own
-- clock. A row that is not there is not an error: a retry after a partial
-- failure has to converge.
DELETE FROM message_attachments
WHERE id = $1 AND workspace_id = $2
RETURNING message_id;

-- name: RemoveMessageAttachment :one
-- Drops one element from a message's read model.
--
-- The element is matched by the id it carries, which is the key the encoder
-- writes, rather than by position: the column is what a client renders, and a
-- position that shifted since the read would remove the wrong file.
UPDATE messages
SET attachments = COALESCE((
        SELECT jsonb_agg(element ORDER BY ordinality)
        FROM jsonb_array_elements(COALESCE(attachments, '[]'::jsonb)) WITH ORDINALITY AS element_at(element, ordinality)
        WHERE element_at.element->>'ID' <> sqlc.arg('attachment_id')::text
    ), '[]'::jsonb)
WHERE id = $1 AND workspace_id = $2
RETURNING *;

-- ------------------------------------------------------------ activity feed

-- name: AppendEvent :one
INSERT INTO activity_events (
    workspace_id, type, actor_agent_id, actor_user_id,
    conversation_id, task_id, draft_id, payload
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: EventsSince :many
-- The replay a reconnecting client asks for: everything after the last ID it
-- saw, oldest first, so the gap is filled in order.
SELECT * FROM activity_events
WHERE workspace_id = $1 AND id > $2
ORDER BY id
LIMIT $3;

-- name: LatestEvents :many
SELECT * FROM activity_events
WHERE workspace_id = $1
ORDER BY id DESC
LIMIT $2;
