-- E8.3 · The single-use half of the OAuth state.
--
-- The state value handed to the provider carries a signed payload naming the
-- workspace, the person, the app, and a random nonce. The signature is verified
-- in the process that receives the callback, so the tenant is known before the
-- database is touched and the row below is consumed inside the ordinary tenant
-- scope. Nothing here needs a policy that steps around Row Level Security.
--
-- A signature alone is not enough, though: it is replayable for as long as it
-- lives, and this callback stores a credential that can act as the workspace.
-- The row is what makes the state single-use. Consuming is one
-- UPDATE ... WHERE consumed_at IS NULL, so two callbacks racing with the same
-- value cannot both win.

CREATE TABLE oauth_states (
    -- The hash of the nonce, which is the unguessable part. The value itself is
    -- never stored, so a leaked backup cannot be used to finish a flow.
    nonce_hash TEXT PRIMARY KEY,
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    app TEXT NOT NULL,
    -- Where the callback sends the person afterwards, so the flow returns to the
    -- screen it started on.
    return_to TEXT NOT NULL DEFAULT '',
    expires_at TIMESTAMPTZ NOT NULL,
    -- Set by the consume. A state that has one is spent, whatever its expiry says.
    consumed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX oauth_states_expiry_idx ON oauth_states (expires_at);

ALTER TABLE oauth_states ENABLE ROW LEVEL SECURITY;
ALTER TABLE oauth_states FORCE ROW LEVEL SECURITY;
CREATE POLICY oauth_states_tenant_isolation ON oauth_states
    USING (workspace_id = app_current_workspace_id())
    WITH CHECK (workspace_id = app_current_workspace_id());
