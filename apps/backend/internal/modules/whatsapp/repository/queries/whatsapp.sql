-- The WhatsApp module's statements.
--
-- Every one of them runs inside the caller's tenant scope, so Row Level
-- Security filters it even when a query forgets its workspace. The two
-- exceptions are named as such: the directory read, which the migration explains,
-- and it is the only one.

-- ------------------------------------------------------------------ numbers

-- name: CreateWhatsAppNumber :one
-- The insert is a SELECT so a Bolu from another workspace comes back as no rows
-- rather than as a foreign key violation, the way the routine module's does.
INSERT INTO whatsapp_numbers (
    workspace_id, integration_id, phone, label, agent_id, status, tos_accepted_at, hourly_limit
)
SELECT
    sqlc.arg('workspace_id'),
    sqlc.narg('integration_id')::uuid,
    sqlc.arg('phone'),
    sqlc.arg('label'),
    sqlc.arg('agent_id'),
    sqlc.arg('status'),
    sqlc.arg('tos_accepted_at'),
    sqlc.arg('hourly_limit')
WHERE EXISTS (
    SELECT 1 FROM agents
    WHERE agents.id = sqlc.arg('agent_id')
      AND agents.workspace_id = sqlc.arg('workspace_id')
)
RETURNING *;

-- name: GetWhatsAppNumber :one
SELECT * FROM whatsapp_numbers
WHERE id = $1 AND workspace_id = $2;

-- name: GetWhatsAppNumberByPhone :one
SELECT * FROM whatsapp_numbers
WHERE phone = $1;

-- name: GetWhatsAppNumberByIntegration :one
SELECT * FROM whatsapp_numbers
WHERE integration_id = $1 AND workspace_id = $2;

-- name: ListWhatsAppNumbers :many
SELECT * FROM whatsapp_numbers
WHERE workspace_id = $1
ORDER BY created_at DESC
LIMIT sqlc.arg('limit');

-- name: UpdateWhatsAppNumber :one
UPDATE whatsapp_numbers
SET label = sqlc.arg('label'),
    agent_id = sqlc.arg('agent_id'),
    hourly_limit = sqlc.arg('hourly_limit'),
    updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
RETURNING *;

-- name: SetWhatsAppNumberStatus :one
UPDATE whatsapp_numbers
SET status = sqlc.arg('status'),
    status_reason = sqlc.arg('reason'),
    connected_at = CASE WHEN sqlc.arg('status') = 'connected' THEN now() ELSE connected_at END,
    last_seen_at = CASE
        WHEN sqlc.arg('status') = 'connected' THEN GREATEST(COALESCE(last_seen_at, to_timestamp(0)), now())
        ELSE last_seen_at
    END,
    updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
RETURNING *;

-- name: TouchWhatsAppNumber :execrows
-- The gateway's heartbeat for one number. It moves last_seen_at forward only, so
-- a late report cannot walk it backwards.
UPDATE whatsapp_numbers
SET last_seen_at = GREATEST(COALESCE(last_seen_at, to_timestamp(0)), sqlc.arg('at')),
    updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id');

-- name: SetWhatsAppNumberPhone :execrows
-- Written once, when a phone pairs: WhatsApp reports the number in its own
-- international form, and that is the number this row is from then on. The value
-- the person typed is only a hint until then.
UPDATE whatsapp_numbers
SET phone = sqlc.arg('phone'), updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id');

-- name: SetWhatsAppNumberIntegration :execrows
-- Written once, when a link finishes: the connection the hub created for this
-- number.
UPDATE whatsapp_numbers
SET integration_id = sqlc.arg('integration_id'), updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id');

-- name: AcceptWhatsAppTerms :one
UPDATE whatsapp_numbers
SET tos_accepted_at = COALESCE(tos_accepted_at, now()),
    updated_at = now()
WHERE id = $1 AND workspace_id = $2
RETURNING *;

-- name: DeleteWhatsAppNumber :execrows
DELETE FROM whatsapp_numbers
WHERE id = $1 AND workspace_id = $2;

-- ---------------------------------------------------------------- directory

-- name: PublishWhatsAppDirectory :exec
-- Written by the API inside a tenant scope; read by the whole fleet.
INSERT INTO whatsapp_directory (number_id, workspace_id, enabled)
VALUES ($1, $2, $3)
ON CONFLICT (number_id) DO UPDATE
SET workspace_id = EXCLUDED.workspace_id,
    enabled = EXCLUDED.enabled,
    updated_at = now();

-- name: ServingWhatsAppNumbers :many
-- The fleet's read. It is deliberately unscoped: a gateway has to find out which
-- tenants have a number before it can open any workspace's scope. It selects the
-- routing pair and nothing else.
SELECT number_id, workspace_id FROM whatsapp_directory
WHERE enabled
ORDER BY created_at;

-- ---------------------------------------------------------------- sessions

-- name: UpsertWhatsAppSession :one
INSERT INTO whatsapp_sessions (number_id, workspace_id, sealed_device, jid)
VALUES (sqlc.arg('number_id'), sqlc.arg('workspace_id'), sqlc.arg('sealed_device'), sqlc.arg('jid'))
ON CONFLICT (number_id) DO UPDATE
SET sealed_device = EXCLUDED.sealed_device,
    jid = EXCLUDED.jid,
    updated_at = now()
RETURNING *;

-- name: GetWhatsAppSession :one
SELECT * FROM whatsapp_sessions
WHERE number_id = $1 AND workspace_id = $2;

-- name: DeleteWhatsAppSession :execrows
DELETE FROM whatsapp_sessions
WHERE number_id = $1 AND workspace_id = $2;

-- ---------------------------------------------------------------- inbound

-- name: RecordWhatsAppMessage :one
-- A redelivery collides on the dedupe key and returns no rows, which is how the
-- caller learns the message was already recorded.
INSERT INTO whatsapp_messages (
    workspace_id, number_id, wa_message_id, chat_jid, sender_jid, sender_name,
    is_group, is_from_me, kind, text, media_key, media_kind, media_size,
    media_status, media_error, received_at
) VALUES (
    sqlc.arg('workspace_id'), sqlc.arg('number_id'), sqlc.arg('wa_message_id'),
    sqlc.arg('chat_jid'), sqlc.arg('sender_jid'), sqlc.arg('sender_name'),
    sqlc.arg('is_group'), sqlc.arg('is_from_me'), sqlc.arg('kind'), sqlc.arg('text'),
    sqlc.arg('media_key'), sqlc.arg('media_kind'), sqlc.arg('media_size'),
    sqlc.arg('media_status'), sqlc.arg('media_error'), sqlc.arg('received_at')
)
ON CONFLICT (workspace_id, wa_message_id) DO NOTHING
RETURNING *;

-- name: GetWhatsAppMessageByWaID :one
SELECT * FROM whatsapp_messages
WHERE workspace_id = $1 AND wa_message_id = $2;

-- name: GetWhatsAppMessage :one
SELECT * FROM whatsapp_messages
WHERE id = $1 AND workspace_id = $2;

-- name: ListWhatsAppMessages :many
SELECT * FROM whatsapp_messages
WHERE workspace_id = sqlc.arg('workspace_id')
  AND (sqlc.narg('number_id')::uuid IS NULL OR number_id = sqlc.narg('number_id'))
  AND (sqlc.narg('chat_jid')::text IS NULL OR chat_jid = sqlc.narg('chat_jid'))
ORDER BY received_at DESC, id DESC
LIMIT sqlc.arg('limit');

-- name: ListWhatsAppMediaPending :many
SELECT * FROM whatsapp_messages
WHERE workspace_id = $1 AND media_status = 'pending'
ORDER BY received_at
LIMIT $2;

-- name: SetWhatsAppMessageMedia :execrows
UPDATE whatsapp_messages
SET media_key = sqlc.arg('media_key'),
    media_kind = sqlc.arg('media_kind'),
    media_size = sqlc.arg('media_size'),
    media_status = sqlc.arg('media_status'),
    media_error = sqlc.arg('media_error')
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id');

-- name: AttachWhatsAppMessageTask :execrows
UPDATE whatsapp_messages
SET task_id = sqlc.arg('task_id')
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id');

-- ---------------------------------------------------------------- outbox

-- name: EnqueueWhatsAppMessage :one
INSERT INTO whatsapp_outbox (
    workspace_id, number_id, to_jid, kind, text, media_key, draft_id, not_before
) VALUES (
    sqlc.arg('workspace_id'), sqlc.arg('number_id'), sqlc.arg('to_jid'),
    sqlc.arg('kind'), sqlc.arg('text'), sqlc.arg('media_key'),
    sqlc.arg('draft_id'), sqlc.arg('not_before')
)
RETURNING *;

-- name: ClaimWhatsAppOutbox :many
-- One message goes to one sender.
--
-- The read and the claim are one statement on purpose: the gateway asks for what
-- is due and marks it taken in the same step, so two workers of one fleet cannot
-- both send it — and SKIP LOCKED means a second reader skips rows a first one is
-- claiming rather than waiting for them.
UPDATE whatsapp_outbox SET status = 'sending', claimed_at = now(), attempts = attempts + 1
WHERE id IN (
    SELECT pending.id FROM whatsapp_outbox AS pending
    WHERE pending.workspace_id = sqlc.arg('workspace_id')
      AND pending.number_id = sqlc.arg('number_id')
      AND pending.status = 'queued'
      AND pending.not_before <= now()
    ORDER BY pending.not_before, pending.id
    LIMIT sqlc.arg('limit')
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: MarkWhatsAppOutboxSent :execrows
UPDATE whatsapp_outbox
SET status = 'sent', wa_message_id = sqlc.arg('wa_message_id'), sent_at = sqlc.arg('sent_at'),
    last_error = ''
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id');

-- name: MarkWhatsAppOutboxHeld :execrows
-- Held rather than failed: the ceiling is a pace, so the message keeps its place
-- in the queue and its attempt is given back.
UPDATE whatsapp_outbox
SET status = 'queued', not_before = sqlc.arg('not_before'), last_error = sqlc.arg('reason'),
    attempts = GREATEST(attempts - 1, 0)
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id');

-- name: MarkWhatsAppOutboxRetry :execrows
-- Back in the queue after a failed attempt. The attempt count is kept, unlike a
-- hold: a transient failure that keeps happening is eventually parked, and a
-- pace is not a failure.
UPDATE whatsapp_outbox
SET status = 'queued', not_before = sqlc.arg('not_before'), last_error = sqlc.arg('reason'),
    claimed_at = NULL
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id');

-- name: MarkWhatsAppOutboxFailed :execrows
UPDATE whatsapp_outbox
SET status = 'failed', last_error = sqlc.arg('reason'), attempts = sqlc.arg('attempts')
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id');

-- name: CountWhatsAppOutboxSentSince :one
-- The hourly ceiling is counted from what actually left, not from a counter that
-- can drift.
SELECT count(*) FROM whatsapp_outbox
WHERE number_id = $1 AND workspace_id = $2 AND status = 'sent' AND sent_at >= $3;

-- name: ListWhatsAppOutbox :many
SELECT * FROM whatsapp_outbox
WHERE workspace_id = sqlc.arg('workspace_id')
  AND (sqlc.narg('number_id')::uuid IS NULL OR number_id = sqlc.narg('number_id'))
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('limit');

-- name: ReclaimWhatsAppOutbox :execrows
-- A gateway killed mid-send leaves messages in `sending`. They go back to the
-- queue rather than sitting there forever: the send either happened or it did
-- not, and a duplicate is worse than a delay, so the attempt is spent and the
-- message waits.
UPDATE whatsapp_outbox
SET status = 'queued', not_before = sqlc.arg('not_before'), claimed_at = NULL,
    last_error = 'gateway terputus saat mengirim'
WHERE workspace_id = sqlc.arg('workspace_id')
  AND status = 'sending'
  AND claimed_at < sqlc.arg('older_than');
