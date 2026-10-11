-- E8.6 · The MCP servers a workspace added.
--
-- For an application that already publishes an MCP server, the adapter is a
-- wrapper rather than a reimplementation. The server's address and its credential
-- belong to the workspace, so they live here; the credential is sealed with the
-- same key ring the integration tokens use, because it is the same kind of secret.

CREATE TABLE mcp_servers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    -- The application key this server is registered under, which is the prefix of
    -- every tool it contributes: `atlassian.search`. It is what a grant names.
    app TEXT NOT NULL,
    -- What a person calls it on the Integrations screen.
    label TEXT NOT NULL DEFAULT '',
    endpoint TEXT NOT NULL,
    -- The credential, sealed. Null means the server is open, which is what a
    -- server on a private network looks like.
    credential_encrypted BYTEA,
    enabled BOOLEAN NOT NULL DEFAULT true,
    -- The last thing that went wrong talking to it, so a screen can say why a
    -- server's tools are missing instead of showing an empty list.
    last_error TEXT NOT NULL DEFAULT '',
    -- The tools the server last reported, kept so a task does not have to reach
    -- the server before it can be offered a tool.
    tools JSONB NOT NULL DEFAULT '[]'::jsonb,
    tool_count INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One application key per workspace, because a grant names the app rather than
-- the server row.
CREATE UNIQUE INDEX mcp_servers_workspace_app_key ON mcp_servers (workspace_id, app);

ALTER TABLE mcp_servers ENABLE ROW LEVEL SECURITY;
ALTER TABLE mcp_servers FORCE ROW LEVEL SECURITY;
CREATE POLICY mcp_servers_tenant_isolation ON mcp_servers
    USING (workspace_id = app_current_workspace_id())
    WITH CHECK (workspace_id = app_current_workspace_id());
