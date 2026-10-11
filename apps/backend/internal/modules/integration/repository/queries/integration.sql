-- Queries for the integration hub: the connected accounts, the workspace data
-- key that seals their tokens, and the grants that say which Bolu may use them.
--
-- Every statement runs inside the caller's tenant scope, so Row Level Security
-- filters it as well. The token columns are only ever selected by name in the
-- statements that exist to open them, so a list of connections cannot leak a
-- ciphertext into a response by accident.

-- name: ListIntegrations :many
SELECT * FROM integrations
WHERE workspace_id = $1
ORDER BY app, account_label;

-- name: GetIntegration :one
SELECT * FROM integrations
WHERE workspace_id = $1 AND id = $2;

-- name: UpsertIntegration :one
-- Re-authorising the same app and account updates the row rather than adding a
-- second connection to the same mailbox.
INSERT INTO integrations (
    workspace_id, app, account_label, status, status_reason, scopes,
    access_token_encrypted, refresh_token_encrypted, token_expires_at, last_refreshed_at
) VALUES ($1, $2, $3, 'connected', '', $4, $5, $6, $7, now())
ON CONFLICT (workspace_id, app, account_label) DO UPDATE SET
    status = 'connected',
    status_reason = '',
    scopes = EXCLUDED.scopes,
    access_token_encrypted = EXCLUDED.access_token_encrypted,
    refresh_token_encrypted = EXCLUDED.refresh_token_encrypted,
    token_expires_at = EXCLUDED.token_expires_at,
    last_refreshed_at = now(),
    updated_at = now()
RETURNING *;

-- name: GetIntegrationToken :one
-- The sealed token of one connection. It is a separate statement so that opening
-- a token is a deliberate read rather than a column that comes along with a list.
SELECT access_token_encrypted, refresh_token_encrypted, token_expires_at, scopes
FROM integrations
WHERE workspace_id = $1 AND id = $2;

-- name: SaveRefreshedToken :one
UPDATE integrations
SET access_token_encrypted = $3,
    refresh_token_encrypted = $4,
    token_expires_at = $5,
    last_refreshed_at = now(),
    status = 'connected',
    status_reason = '',
    updated_at = now()
WHERE workspace_id = $1 AND id = $2
RETURNING *;

-- name: SetIntegrationStatus :one
-- A status a person may have to act on. The tokens are deliberately left alone:
-- a refresh that failed is a reason to ask for a login, not a reason to forget
-- what the connection already knows.
UPDATE integrations
SET status = $3, status_reason = $4, updated_at = now()
WHERE workspace_id = $1 AND id = $2
RETURNING *;

-- name: DisconnectIntegration :one
UPDATE integrations
SET status = 'disconnected',
    status_reason = '',
    access_token_encrypted = NULL,
    refresh_token_encrypted = NULL,
    token_expires_at = NULL,
    updated_at = now()
WHERE workspace_id = $1 AND id = $2
RETURNING *;

-- name: DeleteIntegration :execrows
DELETE FROM integrations
WHERE workspace_id = $1 AND id = $2;

-- ------------------------------------------------------------- the key ring

-- name: GetWorkspaceKey :one
SELECT wrapped_data_key, kms_key_id FROM workspace_keys
WHERE workspace_id = $1;

-- name: UpsertWorkspaceKey :one
-- A rotation replaces the wrap. The data key inside is the same one, so every
-- token sealed with it still opens: only the outer layer changed.
INSERT INTO workspace_keys (workspace_id, wrapped_data_key, kms_key_id)
VALUES ($1, $2, $3)
ON CONFLICT (workspace_id) DO UPDATE SET
    wrapped_data_key = EXCLUDED.wrapped_data_key,
    kms_key_id = EXCLUDED.kms_key_id,
    rotated_at = CASE
        WHEN workspace_keys.kms_key_id <> EXCLUDED.kms_key_id THEN now()
        ELSE workspace_keys.rotated_at
    END
RETURNING *;

-- ------------------------------------------------------------------ grants

-- name: ListGrants :many
SELECT * FROM agent_grants
WHERE workspace_id = $1 AND agent_id = $2
ORDER BY integration_id;

-- name: UpsertGrant :one
INSERT INTO agent_grants (workspace_id, agent_id, integration_id, permission)
VALUES ($1, $2, $3, $4)
ON CONFLICT (agent_id, integration_id) DO UPDATE SET
    permission = EXCLUDED.permission,
    updated_at = now()
RETURNING *;

-- name: DeleteGrant :execrows
DELETE FROM agent_grants
WHERE workspace_id = $1 AND agent_id = $2 AND integration_id = $3;
