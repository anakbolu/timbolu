-- Retention: the job's own reads and writes.
--
-- Three of these are deliberately unscoped — the queue that says which workspace to open,
-- the run log, and the listing of expired objects — and each says why where it is written.
-- Everything that touches a task step or an attachment runs inside the workspace's own scope,
-- so Row Level Security is what keeps one tenant's sweep out of another's.

-- ---------------------------------------------------------------- the queue

-- name: ListDueRetentionTargets :many
-- The one read with no tenant scope, and it carries the workspace id and a moment and nothing
-- else. The sweep cannot open a scope until a row tells it which one.
SELECT workspace_id FROM retention_targets
WHERE next_prune_at <= sqlc.arg('now')
ORDER BY next_prune_at
LIMIT sqlc.arg('limit_count');

-- name: TouchRetentionTarget :exec
UPDATE retention_targets
SET next_prune_at = sqlc.arg('next_prune_at'), updated_at = now()
WHERE workspace_id = sqlc.arg('workspace_id');

-- ---------------------------------------------------------------- the run log

-- name: StartRetentionRun :one
INSERT INTO retention_runs (started_at) VALUES (now())
RETURNING id;

-- name: FinishRetentionRun :exec
-- The counts and the reason are written as values rather than coalesced, because a run that
-- finished with nothing to do is a different fact from one that has not finished.
UPDATE retention_runs
SET finished_at = now(),
    steps_pruned = sqlc.arg('steps_pruned'),
    attachments_pruned = sqlc.arg('attachments_pruned'),
    bytes_freed = sqlc.arg('bytes_freed'),
    error = sqlc.arg('error')
WHERE id = sqlc.arg('id');

-- name: ListRetentionRuns :many
-- What a monitor reads: the newest runs, so "was retention running" is answerable.
SELECT * FROM retention_runs
ORDER BY started_at DESC
LIMIT sqlc.arg('limit_count');

-- ---------------------------------------------------------------- erasure evidence

-- name: StartErasure :one
-- Opens an erasure record before anything is removed.
--
-- It is written first for the reason the retention run is: a request that dies half way must leave a
-- row saying it started and never finished, which is exactly the state an operator has to see. A row
-- written only on success is one that hides the failure that matters.
INSERT INTO data_erasures (
    workspace_id, workspace_name, requested_by, requested_by_email
) VALUES (
    sqlc.arg('workspace_id'), sqlc.arg('workspace_name'),
    sqlc.arg('requested_by'), sqlc.arg('requested_by_email')
)
RETURNING id;

-- name: FinishErasure :exec
-- Closes the record with what was removed, or with why it stopped.
UPDATE data_erasures
SET completed_at = now(),
    objects_deleted = sqlc.arg('objects_deleted'),
    rows_deleted = sqlc.arg('rows_deleted'),
    error = sqlc.arg('error')
WHERE id = sqlc.arg('id');

-- name: ListErasures :many
-- What was erased, newest first: the answer an auditor and a person who asked both read.
SELECT * FROM data_erasures
ORDER BY requested_at DESC
LIMIT sqlc.arg('limit_count');

-- name: FindErasure :one
-- Whether one workspace's data was erased, which is the question its owner actually has.
SELECT * FROM data_erasures
WHERE workspace_id = sqlc.arg('workspace_id') AND completed_at IS NOT NULL
ORDER BY requested_at DESC
LIMIT 1;
