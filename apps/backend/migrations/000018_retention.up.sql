-- EPIC 13 · Retention: raw contents go, the shape of the task stays.
--
-- A `task_steps` row is two different things at once. Its metadata — which step, which tool,
-- how many tokens — is what a task history and the cost report are built from, and it is
-- kept for good. Its `input` and `output` are the customer's own words and the model's
-- answer, which is a copy of a conversation nobody needs to keep forever.
--
-- The retention job removes the second and never the first, which is the whole design: after
-- a prune a task still reads as "four thinking rounds, two tool calls, 12k tokens, 40
-- seconds", and the one-line summary on the task itself is what a person reads. Without that
-- split, retention and "the task history stays readable" would be two rules that contradict
-- each other.
--
-- `pruned_at` already exists on the table from the first migration, which is where this was
-- designed to land.

-- The prune's own read: the oldest unpruned step first, so the job makes progress in bounded
-- batches rather than scanning a table that grows with the deployment.
--
-- It is partial on `pruned_at IS NULL` because a pruned step is not a candidate again: a
-- total index would keep growing with rows the job has already finished with.
CREATE INDEX task_steps_unpruned_created_idx
    ON task_steps (created_at)
    WHERE pruned_at IS NULL;

-- The attachment sweep's read, by age. Bytes live in object storage and only their metadata
-- is a row, so this index is what lets the sweep find the expired ones without reading the
-- table.
CREATE INDEX message_attachments_created_idx
    ON message_attachments (created_at);

-- ---------------------------------------------------------------- what each run did

-- One row per retention pass, so "did retention run, and what did it remove" is answerable
-- from the database rather than from a log line that has already rotated.
--
-- It carries counts and a moment and nothing else — no workspace, no task, no content — for
-- the reason the billing sweep's queue does: it is a record of the job, not of anybody's
-- data, and it is deliberately outside Row Level Security so an operator can read it.
CREATE TABLE retention_runs (
    id BIGSERIAL PRIMARY KEY,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- finished_at is what tells a reader whether the run completed or is still going.
    finished_at TIMESTAMPTZ,
    -- steps_pruned counts task step contents removed in this run.
    steps_pruned BIGINT NOT NULL DEFAULT 0,
    -- attachments_pruned counts attachment rows and their objects removed.
    attachments_pruned BIGINT NOT NULL DEFAULT 0,
    -- bytes_freed is what the object storage gave back, which is the number an operator
    -- watches when the bucket is the thing that is growing.
    bytes_freed BIGINT NOT NULL DEFAULT 0,
    -- error is why a run stopped early, in its own words. A run that failed leaves the row
    -- with its reason rather than no row at all.
    error TEXT NOT NULL DEFAULT ''
);

-- The read that answers "was retention running": the newest runs, so a monitor looks at the
-- head of the table.
CREATE INDEX retention_runs_started_idx ON retention_runs (started_at DESC);

-- ---------------------------------------------------------------- the workspaces to sweep

-- Which workspaces the retention job still has to look at, and when.
--
-- It exists for the reason the fleet directory and the billing queue do: every table the job
-- prunes is protected by Row Level Security on `app_current_workspace_id()`, so a read with no
-- tenant returns nothing — and the sweep runs on behalf of the deployment rather than of a
-- workspace, so it cannot open a scope until a row says which one. This table carries the
-- workspace id and a moment and nothing else; everything it then does runs inside that
-- workspace's own scope.
--
-- It has no policy on purpose. A policy keyed on `app_current_workspace_id()` would match
-- nothing in the read that exists to learn which workspace to open.
CREATE TABLE retention_targets (
    workspace_id UUID PRIMARY KEY REFERENCES workspaces (id) ON DELETE CASCADE,
    -- When this workspace is next due a pass. The job moves it forward after each pass, so a
    -- workspace that is up to date is not read again — and a pass that never ran is caught by
    -- the next one rather than lost.
    next_prune_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX retention_targets_due_idx ON retention_targets (next_prune_at);

-- Every workspace that already exists is queued, in the same place the billing migration
-- backfilled its own queue — inside the migration, where the owner may read the table that
-- Row Level Security would otherwise hide.
INSERT INTO retention_targets (workspace_id)
SELECT id FROM workspaces
ON CONFLICT (workspace_id) DO NOTHING;

-- And every workspace created from here on is queued by the database rather than by a caller.
--
-- It is a trigger where the billing queue uses a provisioner inside the onboarding
-- transaction, and the difference is deliberate: that queue is written by a module that has
-- to be assembled, while this one has one job and no logic. A trigger cannot be forgotten by
-- a new code path that creates a workspace — and a workspace missing from the queue is a
-- workspace whose customer's words and model answers are kept forever, which is the failure
-- nobody notices.
CREATE OR REPLACE FUNCTION queue_retention_target() RETURNS TRIGGER AS $$
BEGIN
    INSERT INTO retention_targets (workspace_id) VALUES (NEW.id)
    ON CONFLICT (workspace_id) DO NOTHING;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER workspaces_queue_retention
    AFTER INSERT ON workspaces
    FOR EACH ROW EXECUTE FUNCTION queue_retention_target();
