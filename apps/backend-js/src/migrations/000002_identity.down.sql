-- Reverse of 000002_identity.up.sql.
--
-- Order matters: the workspaces policy references the invitations table, so the
-- policy has to be dropped before the table, otherwise PostgreSQL refuses the
-- DROP TABLE with "other objects depend on it".

DROP POLICY IF EXISTS invitations_read ON invitations;
DROP POLICY IF EXISTS invitations_insert ON invitations;
DROP POLICY IF EXISTS invitations_update ON invitations;
DROP POLICY IF EXISTS invitations_delete ON invitations;

DROP POLICY IF EXISTS workspaces_read ON workspaces;
DROP POLICY IF EXISTS workspaces_insert ON workspaces;
DROP POLICY IF EXISTS workspaces_update ON workspaces;
DROP POLICY IF EXISTS workspaces_delete ON workspaces;

DROP POLICY IF EXISTS members_read ON members;
DROP POLICY IF EXISTS members_insert ON members;
DROP POLICY IF EXISTS members_update ON members;
DROP POLICY IF EXISTS members_delete ON members;

DROP TABLE IF EXISTS invitations;
DROP TABLE IF EXISTS password_reset_tokens;
DROP TABLE IF EXISTS email_verification_tokens;
DROP TABLE IF EXISTS sessions;

ALTER TABLE users DROP COLUMN IF EXISTS onboarded_at;

-- Restore the single workspace-scoped policies from 000001.
CREATE POLICY members_tenant_isolation ON members
    USING (workspace_id = app_current_workspace_id())
    WITH CHECK (workspace_id = app_current_workspace_id());

ALTER TABLE workspaces DISABLE ROW LEVEL SECURITY;
ALTER TABLE workspaces DROP COLUMN IF EXISTS owner_user_id;

DROP FUNCTION IF EXISTS app_current_user_id();
