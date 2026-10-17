-- Reverses 000011. The servers are a configuration, and a rollback loses it: a
-- deployment that rolls back must add them again, which is the honest cost of a
-- migration that introduced a table nothing else reads.
DROP POLICY IF EXISTS mcp_servers_tenant_isolation ON mcp_servers;
DROP TABLE IF EXISTS mcp_servers;
