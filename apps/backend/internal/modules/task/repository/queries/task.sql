-- Task runtime queries. Every statement carries workspace_id as well as the Row
-- Level Security policy, so a wrong id in application code still cannot reach
-- another tenant's task.

-- name: CreateTask :one
-- The insert is a SELECT rather than a VALUES list so the statement itself
-- refuses an agent, a conversation, or a parent task from another workspace.
-- A foreign key cannot catch that: a key check runs with the table owner's
-- rights and bypasses Row Level Security, so a cross-tenant reference would be
-- accepted while every read of it stays invisible.
INSERT INTO tasks (
    workspace_id, agent_id, parent_task_id, conversation_id, reply_message_id,
    trigger, title, status, depth
)
SELECT
    sqlc.arg('workspace_id'),
    a.id,
    sqlc.narg('parent_task_id')::uuid,
    sqlc.narg('conversation_id')::uuid,
    sqlc.narg('reply_message_id')::uuid,
    sqlc.arg('trigger'),
    sqlc.arg('title'),
    'queued',
    sqlc.arg('depth')
FROM agents a
WHERE a.id = sqlc.arg('agent_id')
  AND a.workspace_id = sqlc.arg('workspace_id')
  AND a.deleted_at IS NULL
  AND (
      sqlc.narg('conversation_id')::uuid IS NULL
      OR EXISTS (
          SELECT 1 FROM conversations c
          WHERE c.id = sqlc.narg('conversation_id')::uuid
            AND c.workspace_id = sqlc.arg('workspace_id')
      )
  )
  AND (
      sqlc.narg('reply_message_id')::uuid IS NULL
      OR EXISTS (
          SELECT 1 FROM messages m
          WHERE m.id = sqlc.narg('reply_message_id')::uuid
            AND m.workspace_id = sqlc.arg('workspace_id')
      )
  )
  AND (
      sqlc.narg('parent_task_id')::uuid IS NULL
      OR EXISTS (
          SELECT 1 FROM tasks p
          WHERE p.id = sqlc.narg('parent_task_id')::uuid
            AND p.workspace_id = sqlc.arg('workspace_id')
      )
  )
RETURNING *;

-- name: GetTask :one
SELECT * FROM tasks WHERE id = $1 AND workspace_id = $2;

-- name: ListTasks :many
-- The history. Every filter is optional and the ordering is fixed, so the
-- endpoint has one shape rather than one per combination.
SELECT * FROM tasks
WHERE workspace_id = $1
  AND (sqlc.narg('agent_id')::uuid IS NULL OR agent_id = sqlc.narg('agent_id')::uuid)
  AND (sqlc.narg('parent_task_id')::uuid IS NULL OR parent_task_id = sqlc.narg('parent_task_id')::uuid)
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status')::text)
  AND (
      sqlc.arg('live')::boolean = false
      OR status IN ('queued', 'running', 'waiting_approval')
  )
ORDER BY created_at DESC, id DESC
LIMIT $2;

-- name: ListChildTasks :many
SELECT * FROM tasks
WHERE workspace_id = $1 AND parent_task_id = $2
ORDER BY created_at;

-- name: StartTask :one
-- Recording the workflow is what makes a restart resume the task instead of
-- opening a second one, so it is written in the same statement as the status.
UPDATE tasks
SET status = 'running',
    workflow_id = $3,
    temporal_run_id = $4,
    started_at = COALESCE(started_at, now()),
    heartbeat_at = now(),
    updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND status IN ('queued', 'running')
RETURNING *;

-- name: ProgressTask :one
-- The heartbeat. A task that stops touching this is the one a worker died on,
-- which is why it is written with the running totals rather than alone.
UPDATE tasks
SET heartbeat_at = now(),
    input_tokens = $3,
    output_tokens = $4,
    cost_micros = $5,
    step_count = $6,
    updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND status IN ('queued', 'running')
RETURNING *;

-- name: ParkTask :one
-- A task waiting for a human has not stopped; it is parked, and the reason is
-- what the office and the task endpoint show.
UPDATE tasks
SET status = 'waiting_approval',
    waiting_reason = $3,
    waiting_draft_id = $4,
    heartbeat_at = now(),
    updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND status IN ('queued', 'running')
RETURNING *;

-- name: ResumeTask :one
UPDATE tasks
SET status = 'running',
    waiting_reason = '',
    waiting_draft_id = NULL,
    heartbeat_at = now(),
    updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND status = 'waiting_approval'
RETURNING *;

-- name: FinishTask :one
-- The terminal write. Only a task that is still live can be finished, so a
-- retried activity cannot overwrite a result the user has already seen.
UPDATE tasks
SET status = $3,
    summary = $4,
    stopped_reason = $5,
    input_tokens = $6,
    output_tokens = $7,
    cost_micros = $8,
    step_count = $9,
    waiting_reason = '',
    waiting_draft_id = NULL,
    finished_at = now(),
    heartbeat_at = now(),
    updated_at = now()
WHERE id = $1 AND workspace_id = $2
  AND status IN ('queued', 'running', 'waiting_approval')
RETURNING *;

-- name: CancelTask :one
UPDATE tasks
SET status = 'canceled',
    stopped_reason = $3,
    finished_at = now(),
    updated_at = now()
WHERE id = $1 AND workspace_id = $2
  AND status IN ('queued', 'running', 'waiting_approval')
RETURNING *;

-- name: CountLiveTasksForAgent :one
SELECT count(*)::bigint FROM tasks
WHERE workspace_id = $1 AND agent_id = $2
  AND status IN ('queued', 'running', 'waiting_approval');

-- ------------------------------------------------------------------- steps

-- name: AppendTaskStep :one
-- The sequence number is assigned in the statement rather than by the caller:
-- two activities writing at once would otherwise pick the same number and lose
-- a step to the unique index.
INSERT INTO task_steps (
    workspace_id, task_id, seq, kind, tool_name, tool_label,
    input, output, input_tokens, output_tokens
)
VALUES (
    $1, $2,
    COALESCE((SELECT max(seq) FROM task_steps WHERE task_id = $2), 0) + 1,
    $3, $4, $5, $6, $7, $8, $9
)
RETURNING *;

-- name: ListTaskSteps :many
SELECT * FROM task_steps
WHERE workspace_id = $1 AND task_id = $2
ORDER BY seq;

-- name: LastTaskStep :one
-- The final answer, which the summary is built from.
SELECT * FROM task_steps
WHERE workspace_id = $1 AND task_id = $2
ORDER BY seq DESC
LIMIT 1;
