-- E8.1 · The integration key ring, and the envelope that holds tokens.
--
-- Tokens are the most sensitive thing the product stores, so they are sealed in
-- two layers: a data key per workspace encrypts the token, and a key-management
-- key encrypts that data key. Only the wrapped data key is stored, and only the
-- integration worker opens it, at the moment a token is used.
--
-- The key lives here rather than on each integration row. A workspace has one
-- data key, so rotating it is one row and one re-seal rather than a migration of
-- every row that happened to be written under the old key.

CREATE TABLE workspace_keys (
    workspace_id UUID PRIMARY KEY REFERENCES workspaces (id) ON DELETE CASCADE,
    -- The data key, sealed by the key-management key. Never stored in the clear.
    wrapped_data_key BYTEA NOT NULL,
    -- Which key-management key wrapped it, so a rotation can tell a wrap made
    -- under the old key from one made under the new without opening either.
    kms_key_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    rotated_at TIMESTAMPTZ
);

ALTER TABLE workspace_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspace_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY workspace_keys_tenant_isolation ON workspace_keys
    USING (workspace_id = app_current_workspace_id())
    WITH CHECK (workspace_id = app_current_workspace_id());

-- The per-row envelope columns are dropped: nothing ever wrote them, and two
-- places that could hold a workspace's key is one place too many.
ALTER TABLE integrations
    DROP COLUMN IF EXISTS data_key_wrapped,
    DROP COLUMN IF EXISTS data_key_kms_key_id;

-- A connection that could not be refreshed needs a reason a person can act on,
-- so the screen can say what went wrong rather than only that it did.
ALTER TABLE integrations
    ADD COLUMN status_reason TEXT NOT NULL DEFAULT '',
    ADD COLUMN last_refreshed_at TIMESTAMPTZ;
