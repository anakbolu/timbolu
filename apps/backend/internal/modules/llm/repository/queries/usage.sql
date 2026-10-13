-- Usage ledger access: the write that every provider call makes, and the daily
-- aggregation the dashboard and the quota screen read.

-- name: InsertUsageEntry :one
-- One row per provider call. This is the audit trail of what a workspace spent,
-- so it is inserted even when the call later failed to be useful.
INSERT INTO usage_ledger (
    workspace_id, agent_id, task_id, provider, model, purpose,
    input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_micros
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING id, created_at;

-- name: SumTokensSince :one
-- The durable quota fallback: the live counter lives in Redis, but a Redis
-- restart must not silently grant a fresh allowance.
SELECT COALESCE(sum(input_tokens + output_tokens), 0)::bigint AS tokens
FROM usage_ledger
WHERE workspace_id = $1 AND created_at >= $2;

-- name: ListDailyUsage :many
-- Per-day totals, most recent first. Days follow Asia/Jakarta, which is where
-- the customers are.
SELECT * FROM usage_daily
WHERE workspace_id = $1 AND day >= $2
ORDER BY day DESC, provider, model;

-- ---------------------------------------------------------------- the report

-- name: SumUsageSince :one
-- The period's totals, over the ledger rather than the daily view.
--
-- It is read from the ledger because a period is a *window*, not a set of whole days: a report that
-- covered "the last seven days" by summing seven rows of `usage_daily` would count a day that is
-- still running as if it were finished, and would miss the part of the first day that falls outside
-- the window. The bound is the caller's, so the report and the row it is compared against are the
-- same interval.
SELECT COALESCE(sum(input_tokens), 0)::bigint       AS input_tokens,
       COALESCE(sum(output_tokens), 0)::bigint      AS output_tokens,
       COALESCE(sum(cache_read_tokens), 0)::bigint  AS cache_read_tokens,
       COALESCE(sum(cache_write_tokens), 0)::bigint AS cache_write_tokens,
       COALESCE(sum(cost_micros), 0)::bigint        AS cost_micros,
       count(*)::bigint                             AS calls
FROM usage_ledger
WHERE workspace_id = $1 AND created_at >= $2;

-- name: ListUsageByAgent :many
-- One row per Bolu, over a window. The name is joined so the report says which bot rather than
-- which id, and a deleted Bolu keeps its rows: `agent_id` is ON DELETE SET NULL, so its spend
-- moves to the unattributed row rather than disappearing from the workspace's total.
SELECT COALESCE(a.id, '00000000-0000-0000-0000-000000000000'::uuid) AS agent_id,
       COALESCE(a.name, '')                                          AS agent_name,
       sum(u.input_tokens)::bigint                                   AS input_tokens,
       sum(u.output_tokens)::bigint                                  AS output_tokens,
       sum(u.cost_micros)::bigint                                    AS cost_micros,
       count(*)::bigint                                              AS calls
FROM usage_ledger u
LEFT JOIN agents a ON a.id = u.agent_id AND a.workspace_id = u.workspace_id
WHERE u.workspace_id = $1 AND u.created_at >= $2
GROUP BY 1, 2
ORDER BY 5 DESC, 3 DESC
LIMIT sqlc.arg('limit_count');

-- name: ListUsageByTask :many
-- One row per task, most expensive first, over a window.
--
-- Only tasks are grouped: a call made outside a task is in the workspace's total and has no row
-- here, which is why the report shows both numbers rather than presenting the rows as a complete
-- breakdown.
SELECT u.task_id,
       COALESCE(t.title, '')           AS task_title,
       COALESCE(t.status, '')          AS task_status,
       sum(u.input_tokens)::bigint     AS input_tokens,
       sum(u.output_tokens)::bigint    AS output_tokens,
       sum(u.cost_micros)::bigint      AS cost_micros,
       count(*)::bigint                AS calls
FROM usage_ledger u
LEFT JOIN tasks t ON t.id = u.task_id AND t.workspace_id = u.workspace_id
WHERE u.workspace_id = $1 AND u.created_at >= $2 AND u.task_id IS NOT NULL
GROUP BY 1, 2, 3
ORDER BY 6 DESC, 4 DESC
LIMIT sqlc.arg('limit_count');
