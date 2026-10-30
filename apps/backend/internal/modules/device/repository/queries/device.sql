-- The device registry: the Expo push address a phone registered, one row per account and token.
--
-- Every statement runs inside the caller's scope, and the policy is keyed on the user rather than
-- the workspace: a query that forgot its user filter would still see only the caller's own devices.

-- name: RegisterDeviceToken :one
-- The upsert is the whole of "registering twice does not add a row". A phone re-registers on every
-- launch, so the second registration updates the row it already has and `created_at` stays the
-- moment it first appeared. `workspace_id` is deliberately absent from the update list: the address
-- belongs to the account, and switching workspaces must not rewrite where it was first registered.
INSERT INTO device_tokens (user_id, workspace_id, platform, token, app_version, last_seen_at)
VALUES (
    sqlc.arg('user_id'),
    sqlc.narg('workspace_id'),
    sqlc.arg('platform'),
    sqlc.arg('token'),
    sqlc.narg('app_version'),
    now()
)
ON CONFLICT (user_id, token) DO UPDATE
SET platform = EXCLUDED.platform,
    app_version = EXCLUDED.app_version,
    last_seen_at = now()
RETURNING id, user_id, workspace_id, platform, token, app_version, last_seen_at, created_at;

-- name: DeleteDeviceToken :execrows
-- A delete that matches nothing is the answer "not yours or not there". The handler turns it into
-- 404 rather than 403, because whether another account registered a token is not this endpoint's to
-- tell.
DELETE FROM device_tokens
WHERE user_id = sqlc.arg('user_id') AND token = sqlc.arg('token');
