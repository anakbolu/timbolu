DROP VIEW IF EXISTS usage_daily;

DROP POLICY IF EXISTS llm_providers_tenant_isolation ON llm_providers;
DROP INDEX IF EXISTS llm_providers_chain_idx;
DROP INDEX IF EXISTS llm_providers_one_default_key;
DROP INDEX IF EXISTS llm_providers_workspace_name_key;
DROP TABLE IF EXISTS llm_providers;