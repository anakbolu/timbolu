-- The gateway's own half of the WhatsApp schema: the sealed device state it reads
-- and writes, and the directory it follows to find out which numbers exist.
--
-- It is a second sqlc entry rather than a shared one because the two processes
-- own different halves of the same tables: the product keeps the numbers, the
-- messages, the outbox, and the directory's writes, while the gateway is the only
-- thing that reads and writes a device blob. Sharing a query file would make the
-- gateway carry statements about tables it must never touch.

-- ---------------------------------------------------------------- directory

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

-- ------------------------------------------------------------- workspace keys

-- The workspace's data key, which is what seals a device blob. It is read here
-- rather than in the backend because the gateway is the process that writes a
-- session: a blob cannot be sealed by one process and read by another without the
-- key travelling, and the key is the one thing that must not.

-- name: GetWorkspaceKey :one
SELECT wrapped_data_key, kms_key_id FROM workspace_keys
WHERE workspace_id = $1;

-- name: MintWorkspaceKey :execrows
-- Created on first use, the way the hub creates it: a workspace whose first act is
-- linking a number has no data key yet, and the seal is what needs one. DO NOTHING
-- rather than an upsert, so a key minted by the hub in the same moment is kept —
-- replacing it would make every token already sealed under it unreadable.
INSERT INTO workspace_keys (workspace_id, wrapped_data_key, kms_key_id)
VALUES ($1, $2, $3)
ON CONFLICT (workspace_id) DO NOTHING;
