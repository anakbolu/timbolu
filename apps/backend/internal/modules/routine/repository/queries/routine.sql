-- Routine queries. Every statement carries workspace_id as well as the Row Level
-- Security policy, so a wrong id in application code still cannot reach another
-- tenant's routine.

-- name: CreateRoutine :one
-- The insert is a SELECT rather than a VALUES list, for the same reason the task
-- insert is: a foreign key check runs with the table owner's rights and bypasses
-- Row Level Security, so the statement has to refuse a Bolu from another workspace
-- itself.
INSERT INTO routines (
    workspace_id, agent_id, name, kind, minute, hour_start, hour_end,
    day_of_week, trigger_source, instruction, enabled
)
SELECT
    sqlc.arg('workspace_id'),
    a.id,
    sqlc.arg('name'),
    sqlc.arg('kind'),
    sqlc.arg('minute'),
    sqlc.arg('hour_start'),
    sqlc.arg('hour_end'),
    sqlc.narg('day_of_week'),
    sqlc.arg('trigger_source'),
    sqlc.arg('instruction'),
    sqlc.arg('enabled')
FROM agents a
WHERE a.id = sqlc.arg('agent_id')
  AND a.workspace_id = sqlc.arg('workspace_id')
  AND a.deleted_at IS NULL
RETURNING
    id, workspace_id, agent_id, name, kind, minute, hour_start, hour_end,
    day_of_week, trigger_source, instruction, enabled, paused_reason,
    schedule_id, created_at, updated_at;

-- name: GetRoutine :one
SELECT
    id, workspace_id, agent_id, name, kind, minute, hour_start, hour_end,
    day_of_week, trigger_source, instruction, enabled, paused_reason,
    schedule_id, created_at, updated_at
FROM routines
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id');

-- name: ListRoutines :many
SELECT
    id, workspace_id, agent_id, name, kind, minute, hour_start, hour_end,
    day_of_week, trigger_source, instruction, enabled, paused_reason,
    schedule_id, created_at, updated_at
FROM routines
WHERE workspace_id = sqlc.arg('workspace_id')
  AND (sqlc.narg('agent_id')::uuid IS NULL OR agent_id = sqlc.narg('agent_id'))
  AND (sqlc.narg('enabled')::boolean IS NULL OR enabled = sqlc.narg('enabled'))
ORDER BY created_at DESC
LIMIT sqlc.arg('limit');

-- name: ListRoutinesByTrigger :many
-- The routines a delivery is dispatched to. Only the enabled ones: a paused routine
-- watches nothing.
SELECT
    id, workspace_id, agent_id, name, kind, minute, hour_start, hour_end,
    day_of_week, trigger_source, instruction, enabled, paused_reason,
    schedule_id, created_at, updated_at
FROM routines
WHERE workspace_id = sqlc.arg('workspace_id')
  AND trigger_source = sqlc.arg('trigger_source')
  AND enabled
ORDER BY created_at;

-- name: ListEnabledRoutines :many
-- The reschedule walk: every routine whose schedule has to be re-registered when
-- the workspace's clock changes.
SELECT
    id, workspace_id, agent_id, name, kind, minute, hour_start, hour_end,
    day_of_week, trigger_source, instruction, enabled, paused_reason,
    schedule_id, created_at, updated_at
FROM routines
WHERE workspace_id = sqlc.arg('workspace_id') AND enabled
ORDER BY created_at;

-- name: UpdateRoutine :one
-- Every column a form edits, in one statement: a cadence that is half-written
-- would be registered as a schedule nobody asked for.
UPDATE routines SET
    agent_id = sqlc.arg('agent_id'),
    name = sqlc.arg('name'),
    kind = sqlc.arg('kind'),
    minute = sqlc.arg('minute'),
    hour_start = sqlc.arg('hour_start'),
    hour_end = sqlc.arg('hour_end'),
    day_of_week = sqlc.narg('day_of_week'),
    trigger_source = sqlc.arg('trigger_source'),
    instruction = sqlc.arg('instruction'),
    enabled = sqlc.arg('enabled'),
    paused_reason = sqlc.arg('paused_reason'),
    updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
RETURNING
    id, workspace_id, agent_id, name, kind, minute, hour_start, hour_end,
    day_of_week, trigger_source, instruction, enabled, paused_reason,
    schedule_id, created_at, updated_at;

-- name: SetRoutineScheduleID :exec
UPDATE routines SET schedule_id = sqlc.arg('schedule_id'), updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id');

-- name: DeleteRoutine :execrows
-- The runs are kept: `routine_runs.routine_id` is ON DELETE SET NULL and the run
-- carries the routine's name, so the history of a routine that was deleted is
-- still readable afterwards.
DELETE FROM routines
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id');

-- ------------------------------------------------------------------- runs

-- name: StartRoutineRun :one
INSERT INTO routine_runs (workspace_id, routine_id, routine_name, status)
VALUES (
    sqlc.arg('workspace_id'),
    sqlc.narg('routine_id'),
    sqlc.arg('routine_name'),
    sqlc.arg('status')
)
RETURNING id, workspace_id, routine_id, routine_name, task_id, status, summary, started_at, finished_at;

-- name: FinishRoutineRun :one
-- Idempotent by construction: only a run that has not finished is written, so an
-- activity re-run after a worker restart returns the outcome the first attempt
-- already recorded rather than opening a second one.
UPDATE routine_runs SET
    status = sqlc.arg('status'),
    summary = sqlc.arg('summary'),
    task_id = sqlc.narg('task_id'),
    finished_at = sqlc.arg('finished_at')
WHERE id = sqlc.arg('id')
  AND workspace_id = sqlc.arg('workspace_id')
  AND finished_at IS NULL
RETURNING id, workspace_id, routine_id, routine_name, task_id, status, summary, started_at, finished_at;

-- name: AttachRoutineRunTask :one
-- The run keeps the task it opened, which is how the task's own outcome finds its
-- way back to the run: the task reports by its id, and this column is the link.
UPDATE routine_runs SET task_id = sqlc.arg('task_id')
WHERE id = sqlc.arg('id')
  AND workspace_id = sqlc.arg('workspace_id')
  AND finished_at IS NULL
RETURNING id, workspace_id, routine_id, routine_name, task_id, status, summary, started_at, finished_at;

-- name: GetRoutineRunByTask :one
-- The run a finished task belongs to. It is the lookup the outcome report makes,
-- and it answers no rows for a task a routine did not open, which is most of them.
SELECT id, workspace_id, routine_id, routine_name, task_id, status, summary, started_at, finished_at
FROM routine_runs
WHERE workspace_id = sqlc.arg('workspace_id') AND task_id = sqlc.arg('task_id')
ORDER BY started_at DESC
LIMIT 1;

-- name: GetRoutineRun :one
SELECT id, workspace_id, routine_id, routine_name, task_id, status, summary, started_at, finished_at
FROM routine_runs
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id');

-- name: LatestRoutineRun :one
-- The most recent run of one routine, which is what a re-fired routine looks at
-- before starting another.
SELECT id, workspace_id, routine_id, routine_name, task_id, status, summary, started_at, finished_at
FROM routine_runs
WHERE workspace_id = sqlc.arg('workspace_id')
  AND routine_id = sqlc.arg('routine_id')
ORDER BY started_at DESC, id DESC
LIMIT 1;

-- name: ListRoutineRuns :many
-- The history the screen reads. A run whose routine was deleted has a null id, so
-- the name is what identifies it.
SELECT
    run.id, run.workspace_id, run.routine_id, run.routine_name, run.task_id,
    run.status, run.summary, run.started_at, run.finished_at
FROM routine_runs run
LEFT JOIN routines r ON r.id = run.routine_id
WHERE run.workspace_id = sqlc.arg('workspace_id')
  AND (
        sqlc.narg('routine_id')::uuid IS NULL
        OR run.routine_id = sqlc.narg('routine_id')
      )
  AND (sqlc.narg('agent_id')::uuid IS NULL OR r.agent_id = sqlc.narg('agent_id'))
  AND (sqlc.narg('status')::text IS NULL OR run.status = sqlc.narg('status'))
ORDER BY run.started_at DESC, run.id DESC
LIMIT sqlc.arg('limit');

-- ------------------------------------------------------------------- agents

-- name: ListRoutineAgents :many
-- The rows belong to the agent module; this reads the two fields a routine's label
-- and its dispatch gate need.
SELECT id, name, (status = 'resting') AS resting FROM agents
WHERE workspace_id = sqlc.arg('workspace_id') AND deleted_at IS NULL
ORDER BY name;
