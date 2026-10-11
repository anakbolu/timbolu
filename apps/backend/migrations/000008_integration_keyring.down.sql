-- Reverses 000008: the key ring goes away and the envelope columns return.
--
-- The columns come back empty, which is what they were: nothing ever wrote them.
-- A deployment that rolls back loses the ability to open stored tokens, so the
-- connections must be re-authorised — that is the honest cost of rolling back a
-- migration that changed how secrets are sealed.

ALTER TABLE integrations
    DROP COLUMN IF EXISTS last_refreshed_at,
    DROP COLUMN IF EXISTS status_reason;

ALTER TABLE integrations
    ADD COLUMN IF NOT EXISTS data_key_wrapped BYTEA,
    ADD COLUMN IF NOT EXISTS data_key_kms_key_id TEXT NOT NULL DEFAULT '';

DROP POLICY IF EXISTS workspace_keys_tenant_isolation ON workspace_keys;
DROP TABLE IF EXISTS workspace_keys;
