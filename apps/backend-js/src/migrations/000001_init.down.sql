-- Reverse of 000001_init.up.sql. Extensions and the helper function are
-- dropped last so a down/up cycle on the same database stays clean.

DROP POLICY IF EXISTS members_tenant_isolation ON members;
DROP POLICY IF EXISTS teams_tenant_isolation ON teams;
DROP POLICY IF EXISTS integrations_tenant_isolation ON integrations;
DROP POLICY IF EXISTS agents_tenant_isolation ON agents;
DROP POLICY IF EXISTS agent_grants_tenant_isolation ON agent_grants;
DROP POLICY IF EXISTS conversations_tenant_isolation ON conversations;
DROP POLICY IF EXISTS conversation_participants_tenant_isolation ON conversation_participants;
DROP POLICY IF EXISTS messages_tenant_isolation ON messages;
DROP POLICY IF EXISTS tasks_tenant_isolation ON tasks;
DROP POLICY IF EXISTS task_steps_tenant_isolation ON task_steps;
DROP POLICY IF EXISTS approval_policies_tenant_isolation ON approval_policies;
DROP POLICY IF EXISTS drafts_tenant_isolation ON drafts;
DROP POLICY IF EXISTS routines_tenant_isolation ON routines;
DROP POLICY IF EXISTS routine_runs_tenant_isolation ON routine_runs;
DROP POLICY IF EXISTS knowledge_items_tenant_isolation ON knowledge_items;
DROP POLICY IF EXISTS memories_tenant_isolation ON memories;
DROP POLICY IF EXISTS activity_events_tenant_isolation ON activity_events;
DROP POLICY IF EXISTS audit_logs_tenant_isolation ON audit_logs;
DROP POLICY IF EXISTS usage_ledger_tenant_isolation ON usage_ledger;
DROP POLICY IF EXISTS subscriptions_tenant_isolation ON subscriptions;
DROP POLICY IF EXISTS billing_invoices_tenant_isolation ON billing_invoices;
DROP POLICY IF EXISTS quota_periods_tenant_isolation ON quota_periods;

DROP TABLE IF EXISTS quota_periods;
DROP TABLE IF EXISTS billing_invoices;
DROP TABLE IF EXISTS subscriptions;
DROP TABLE IF EXISTS plans;
DROP TABLE IF EXISTS usage_ledger;
DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS activity_events;
DROP TABLE IF EXISTS memories;
DROP TABLE IF EXISTS knowledge_items;
DROP TABLE IF EXISTS routine_runs;
DROP TABLE IF EXISTS routines;
DROP TABLE IF EXISTS drafts;
DROP TABLE IF EXISTS approval_policies;
DROP TABLE IF EXISTS task_steps;

ALTER TABLE IF EXISTS messages DROP CONSTRAINT IF EXISTS messages_task_fk;

DROP TABLE IF EXISTS tasks;
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS conversation_participants;
DROP TABLE IF EXISTS conversations;
DROP TABLE IF EXISTS agent_grants;
DROP TABLE IF EXISTS agents;
DROP TABLE IF EXISTS integrations;
DROP TABLE IF EXISTS agent_templates;
DROP TABLE IF EXISTS teams;
DROP TABLE IF EXISTS members;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS workspaces;

DROP FUNCTION IF EXISTS app_current_workspace_id();
