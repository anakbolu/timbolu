-- The sidebar's own order: where each Bolu sits in the list, and in which team.
--
-- The sidebar is the screen a person actually works in, and until now its order was whatever the
-- registry happened to return — `t.name, a.created_at` — so a Bolu could not be moved and a new one
-- landed wherever the clock put it. This migration gives the position a column, which is what makes
-- "naik turun" something the workspace keeps rather than something a browser tab remembers.
--
-- Two decisions are worth reading rather than inferring:
--
--   1. **The order is per team, and dense.** `sort_order` counts from 0 inside its team, so the
--      arrangement is a list rather than a sparse field a client has to interpret. Moving a Bolu
--      into another team is the same write as moving it up: the pair (team_id, sort_order) is the
--      position, and there is no separate "reorder" state to keep in step with the team column.
--   2. **Ties are broken by created_at, not left to the planner.** The column is not unique — a
--      concurrent create may compute the same next position — so every read has a total order to
--      fall back on. The arrangement endpoint rewrites dense positions, so a tie is a moment rather
--      than a state.
--
-- Backfill: existing rows get the order they were already being shown in, so the sidebar does not
-- rearrange itself under somebody's cursor when this ships.

ALTER TABLE agents ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0;

WITH ordered AS (
    SELECT id,
           row_number() OVER (PARTITION BY workspace_id, team_id ORDER BY created_at, id) - 1 AS position
    FROM agents
)
UPDATE agents AS a
SET sort_order = ordered.position
FROM ordered
WHERE a.id = ordered.id;

-- The registry lists live Bolu of one workspace grouped by team, in position order.
-- It replaces `agents_live_idx` rather than joining it: the two would have the same
-- columns and the same predicate, and a second index on the same rows is write cost
-- for nothing. Every query that used the old one can use this one.
DROP INDEX IF EXISTS agents_live_idx;
CREATE INDEX agents_order_idx ON agents (workspace_id, team_id, sort_order)
    WHERE deleted_at IS NULL;
