-- The reverse of 000023: the sidebar's order goes back to being derived.
--
-- Nothing else reads `sort_order`, so dropping the column loses the arrangement and nothing more: the
-- registry falls back to `t.name, a.created_at`, which is the order every workspace had before this
-- migration existed. Rolling back does not lose a Bolu, a team, or a task.
--
-- The index 000003 created comes back with the shape it had, because this migration is what replaced
-- it and a rollback has to leave the schema 000003 described.

DROP INDEX IF EXISTS agents_order_idx;
ALTER TABLE agents DROP COLUMN IF EXISTS sort_order;

CREATE INDEX agents_live_idx ON agents (workspace_id, team_id)
    WHERE deleted_at IS NULL;
