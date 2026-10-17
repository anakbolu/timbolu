-- Identity and tenancy: the user context used by Row Level Security, the
-- sessions and one-time tokens that back authentication, and workspace
-- invitations.
--
-- Two settings drive the policies:
--   app.workspace_id  the active tenant, set by the tenant middleware only
--                     after the caller's membership has been verified
--   app.user_id       the authenticated user, set from the access token
--
-- Membership resolution needs to work before a workspace context exists (the
-- workspace switcher, and the tenant middleware itself), so the policies on
-- members and workspaces additionally allow a user to see their own rows. The
-- WITH CHECK clauses stay strict: a row can only be written inside the active
-- workspace, so the extra visibility cannot be turned into a write.

CREATE FUNCTION app_current_user_id() RETURNS uuid
    LANGUAGE sql STABLE AS $$
    SELECT NULLIF(current_setting('app.user_id', true), '')::uuid
$$;

-- --------------------------------------------------------------- onboarding

-- Set once the workspace and its default teams exist, which makes the
-- onboarding endpoint idempotent.
ALTER TABLE users ADD COLUMN onboarded_at TIMESTAMPTZ;

-- The account that created the workspace. It is what lets a workspace be read
-- back inside the transaction that creates it: `INSERT ... RETURNING` applies
-- the SELECT policy as well, and at that moment the workspace has no members
-- yet, so a membership-based policy alone would reject its own creator.
ALTER TABLE workspaces
    ADD COLUMN owner_user_id UUID REFERENCES users (id) ON DELETE SET NULL;

-- ------------------------------------------------------------------ sessions

-- A session is one refresh token. The token itself is never stored: only its
-- SHA-256 hash, so a database leak does not hand over live sessions.
CREATE TABLE sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    refresh_token_hash TEXT NOT NULL UNIQUE,
    -- Rotation keeps the chain: a rotated session points at the session it
    -- replaced, so replaying an old refresh token can be detected.
    rotated_from UUID REFERENCES sessions (id) ON DELETE SET NULL,
    user_agent TEXT NOT NULL DEFAULT '',
    ip TEXT NOT NULL DEFAULT '',
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX sessions_user_idx ON sessions (user_id);
CREATE INDEX sessions_expires_idx ON sessions (expires_at);

-- One-time tokens. `used_at` enforces single use; the hash keeps the raw token
-- out of the database.
CREATE TABLE email_verification_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX email_verification_tokens_user_idx ON email_verification_tokens (user_id);

CREATE TABLE password_reset_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX password_reset_tokens_user_idx ON password_reset_tokens (user_id);

-- --------------------------------------------------------------- invitations

CREATE TABLE invitations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    email TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'member')),
    token_hash TEXT NOT NULL UNIQUE,
    invited_by UUID REFERENCES users (id) ON DELETE SET NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    accepted_at TIMESTAMPTZ,
    accepted_by UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One live invitation per email and workspace; re-inviting after acceptance is
-- allowed.
CREATE UNIQUE INDEX invitations_pending_key
    ON invitations (workspace_id, lower(email))
    WHERE accepted_at IS NULL;

CREATE INDEX invitations_email_idx ON invitations (lower(email));

-- --------------------------------------------------- membership visibility

-- Replaces the single workspace-only policy from 000001. A member can now read
-- the rows that answer "which workspaces am I in, and with which role" without
-- an active workspace, while writes still require the active workspace.
DROP POLICY members_tenant_isolation ON members;

CREATE POLICY members_read ON members FOR SELECT
    USING (
        workspace_id = app_current_workspace_id()
        OR user_id = app_current_user_id()
    );

CREATE POLICY members_insert ON members FOR INSERT
    WITH CHECK (workspace_id = app_current_workspace_id());

CREATE POLICY members_update ON members FOR UPDATE
    USING (workspace_id = app_current_workspace_id())
    WITH CHECK (workspace_id = app_current_workspace_id());

CREATE POLICY members_delete ON members FOR DELETE
    USING (
        workspace_id = app_current_workspace_id()
        OR user_id = app_current_user_id()
    );

-- A workspace is visible to its active tenant and to any user who is a member
-- of it. Creation is allowed for an authenticated user: onboarding creates the
-- workspace and the owner membership in one transaction, so the row is never
-- left without an owner.
--
-- workspaces was not part of the 000001 policy loop (it has no workspace_id
-- column), so Row Level Security is switched on here.
ALTER TABLE workspaces ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspaces FORCE ROW LEVEL SECURITY;

CREATE POLICY workspaces_read ON workspaces FOR SELECT
    USING (
        id = app_current_workspace_id()
        OR owner_user_id = app_current_user_id()
        OR EXISTS (
            SELECT 1 FROM members m
            WHERE m.workspace_id = workspaces.id
              AND m.user_id = app_current_user_id()
        )
        -- An invitee may read the workspace they were invited to, so the
        -- invitation screen can name it before they accept.
        OR EXISTS (
            SELECT 1 FROM invitations i
            WHERE i.workspace_id = workspaces.id
              AND i.accepted_at IS NULL
              AND lower(i.email) = (
                  SELECT lower(u.email) FROM users u WHERE u.id = app_current_user_id()
              )
        )
    );

-- A workspace can only be created for the caller: naming another account as the
-- owner is refused.
CREATE POLICY workspaces_insert ON workspaces FOR INSERT
    WITH CHECK (owner_user_id = app_current_user_id());

CREATE POLICY workspaces_update ON workspaces FOR UPDATE
    USING (id = app_current_workspace_id())
    WITH CHECK (id = app_current_workspace_id());

CREATE POLICY workspaces_delete ON workspaces FOR DELETE
    USING (id = app_current_workspace_id());

-- Invitations: a workspace member sees the invitations of the active
-- workspace; an invitee sees the invitation addressed to their own email, which
-- is what lets them accept it before they are a member.
ALTER TABLE invitations ENABLE ROW LEVEL SECURITY;
ALTER TABLE invitations FORCE ROW LEVEL SECURITY;

CREATE POLICY invitations_read ON invitations FOR SELECT
    USING (
        workspace_id = app_current_workspace_id()
        OR lower(email) = (
            SELECT lower(u.email) FROM users u WHERE u.id = app_current_user_id()
        )
    );

CREATE POLICY invitations_insert ON invitations FOR INSERT
    WITH CHECK (workspace_id = app_current_workspace_id());

CREATE POLICY invitations_update ON invitations FOR UPDATE
    USING (workspace_id = app_current_workspace_id())
    WITH CHECK (workspace_id = app_current_workspace_id());

CREATE POLICY invitations_delete ON invitations FOR DELETE
    USING (workspace_id = app_current_workspace_id());
