-- EPIC 13 · Erasure: the record that a workspace's data was removed on request.
--
-- UU PDP (UU No. 27/2022) gives a person the right to have their data erased, and it gives them the
-- right to be answered. That second half is why this table exists rather than a log line: a
-- deletion that left no trace is a deletion nobody can prove, and a request answered from a log that
-- has already rotated is a request answered from memory.
--
-- **It carries no foreign key, and that is the whole design.** The workspace it names has been
-- deleted by the time the row is written — that is what the row records — so a `REFERENCES
-- workspaces (id)` would either cascade the evidence away with the data or refuse the delete. The
-- identifiers are kept as values: `workspace_id` and `requested_by` are the two facts an auditor
-- asks for, and both are needed *after* the rows they pointed at are gone.
--
-- It also has no policy, for the reason `retention_runs` has none: it is a record of what the
-- deployment did, not a tenant's data, and the person who has to show it may be an operator rather
-- than a member of any workspace. What it holds is an id, a name, a moment, and counts — nothing a
-- customer said.
CREATE TABLE data_erasures (
    id BIGSERIAL PRIMARY KEY,
    -- The workspace that was erased. A value rather than a reference: there is nothing left to
    -- reference once the erasure completes.
    workspace_id UUID NOT NULL,
    -- workspace_name is the name the workspace had, kept because an id proves nothing to the person
    -- who asked for their data to be removed and was told "Toko Sinar".
    workspace_name TEXT NOT NULL DEFAULT '',
    -- requested_by is who asked, and by_email is how they are reached, both as values for the same
    -- reason.
    requested_by       UUID NOT NULL,
    requested_by_email TEXT NOT NULL DEFAULT '',
    requested_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- completed_at is what tells a reader whether the erasure finished. A row without it is a
    -- request that was accepted and has not completed, which is a different state from one that
    -- failed and left its reason.
    completed_at TIMESTAMPTZ,
    -- objects_deleted counts the bytes removed from object storage: attachments and sandboxed
    -- content live there rather than in Postgres, and a row-only deletion would leave them behind
    -- in a bucket nothing points at.
    objects_deleted BIGINT NOT NULL DEFAULT 0,
    -- rows_deleted is what the database cascaded, which is the count an operator compares against a
    -- backup to show that nothing was left.
    rows_deleted BIGINT NOT NULL DEFAULT 0,
    -- error is why a request stopped, in its own words. A request that failed keeps its row.
    error TEXT NOT NULL DEFAULT ''
);

-- The read that answers "what was erased, and when": newest first, so a monitor and an auditor look
-- at the head of the table.
CREATE INDEX data_erasures_requested_idx ON data_erasures (requested_at DESC);

-- And the read that answers "was *this* workspace's data erased", which is the question a person who
-- asked actually has. The index is partial on the completed rows because an in-flight or failed
-- request is answered by the newest-first read above rather than by this lookup.
CREATE INDEX data_erasures_workspace_idx ON data_erasures (workspace_id) WHERE completed_at IS NOT NULL;
