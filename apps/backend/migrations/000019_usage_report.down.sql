-- The reverse of 000019: the two indexes the usage report's aggregations read.
--
-- Nothing else is undone, because nothing else was created: the report is a query over
-- `usage_ledger`, and the rows it aggregates are the rows the deployment already keeps. Rolling this
-- back costs the report its index and nothing else — which is the point of the design rather than a
-- shortcoming of it.

DROP INDEX IF EXISTS usage_ledger_workspace_task_created_idx;
DROP INDEX IF EXISTS usage_ledger_workspace_agent_created_idx;
