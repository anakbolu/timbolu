-- EPIC 13 · Usage: the two questions a per-workspace bill is asked with.
--
-- `usage_daily` answers "what did this workspace spend, by day, provider, model, and purpose" — which
-- is the fleet's view of a period. Two more questions are asked of the same ledger by the usage
-- screen, and neither is answerable from that view:
--
--   * **per Bolu** — which of the customer's bots costs the most, which is how a workspace decides
--     what to change. The ledger already carries `agent_id`; what it lacks is a way to read one
--     agent's rows over a window without scanning every row of the workspace.
--   * **per task** — what one piece of work cost, which is the figure a person recognizes: "the
--     rekonsiliasi run cost Rp 4.100" is a fact about a task, not about a day.
--
-- Both are aggregates over `usage_ledger` rather than views, and they are read with a `since` bound
-- and a `limit`: the ledger grows without bound, so a report that scanned it whole would get slower
-- every month the product is used.

-- The read behind the per-Bolu breakdown: the workspace's rows for one agent, over a window.
--
-- `agent_id` is nullable — a platform call made on nobody's behalf — so the index is partial on the
-- rows that name one: a scan for one Bolu's spend should not walk past the rows that belong to no
-- Bolu at all.
CREATE INDEX usage_ledger_workspace_agent_created_idx
    ON usage_ledger (workspace_id, agent_id, created_at DESC)
    WHERE agent_id IS NOT NULL;

-- The read behind the per-task breakdown, partial for the same reason: a call made outside a task
-- is a real row and not one this aggregation ever groups.
CREATE INDEX usage_ledger_workspace_task_created_idx
    ON usage_ledger (workspace_id, task_id, created_at DESC)
    WHERE task_id IS NOT NULL;
