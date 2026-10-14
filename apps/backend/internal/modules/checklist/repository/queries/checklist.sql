-- The checklist module's queries: the plan a Bolu writes for itself, and the items it is made of.
--
-- Everything here runs inside the caller's tenant scope, so Row Level Security filters it even where a
-- statement forgets its workspace filter. Three of the statements are more than a read — writing a
-- plan with its items, settling a plan whose last task was ticked, and settling one whose last open
-- task was removed — and each is one transaction, because a plan half-written or half-settled is a
-- state the product promises cannot exist.

-- ------------------------------------------------------------------ the list

-- name: GetActiveChecklist :one
-- The one plan this Bolu is working on. There is at most one, and the partial unique index is what
-- says so; this read is what turns that into a fact the service can act on.
SELECT * FROM checklists
WHERE workspace_id = $1 AND agent_id = $2 AND status = 'active';

-- name: CreateChecklist :one
INSERT INTO checklists (workspace_id, agent_id, title, task_id)
VALUES ($1, $2, $3, sqlc.narg('task_id'))
RETURNING *;

-- name: GetChecklist :one
-- One plan by its id, which is what a write reads back when it did not settle it.
SELECT * FROM checklists WHERE id = $1 AND workspace_id = $2;

-- name: CompleteChecklist :one
-- Settling a plan is one statement, and the caller reads the settled row back rather than assuming
-- what it says. Only the settle path calls it, and only after the last open task went away, so there
-- is no state for a guard to catch.
UPDATE checklists
SET status = 'done', completed_at = now(), updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
RETURNING *;

-- name: DeleteChecklist :execrows
-- Clearing is a delete, and the items go with it through the foreign key. A plan the Bolu decided was
-- wrong is not history worth keeping: what it *did* is in `task_steps`, and this table holds what it
-- intended to do.
DELETE FROM checklists WHERE id = $1 AND workspace_id = $2;

-- name: ListChecklists :many
-- The screen's read: a workspace's plans, newest first, narrowed to one Bolu or one state. The two
-- filters are optional in one statement rather than one query per combination, so the page a person
-- sees and the page the database was asked for cannot drift apart.
SELECT * FROM checklists
WHERE workspace_id = sqlc.arg('workspace_id')
  AND (sqlc.narg('agent_id')::uuid IS NULL OR agent_id = sqlc.narg('agent_id')::uuid)
  AND (sqlc.arg('status')::text = '' OR status = sqlc.arg('status')::text)
ORDER BY created_at DESC
LIMIT sqlc.arg('limit_count');

-- ------------------------------------------------------------------ the items

-- name: ListChecklistItems :many
-- Every item of a plan, tasks and subtasks together; the repository assembles the tree. The order is
-- the one the Bolu wrote, and `position` is what carries it: two items written in one statement share
-- a timestamp, so ordering by time alone would be whatever the database felt like.
SELECT * FROM checklist_items
WHERE checklist_id = sqlc.arg('checklist_id') AND workspace_id = sqlc.arg('workspace_id')
ORDER BY position, created_at;

-- name: ListItemsForChecklists :many
-- The items of many plans in one query, which is what the screen's list read needs: a page of fifty
-- plans would otherwise be fifty-one round trips to render.
SELECT * FROM checklist_items
WHERE workspace_id = sqlc.arg('workspace_id') AND checklist_id = ANY(sqlc.arg('checklist_ids')::uuid[])
ORDER BY position, created_at;

-- name: CreateChecklistItem :one
INSERT INTO checklist_items (checklist_id, workspace_id, parent_id, title, position)
VALUES (sqlc.arg('checklist_id'), sqlc.arg('workspace_id'), sqlc.narg('parent_id'), sqlc.arg('title'), sqlc.arg('position'))
RETURNING *;

-- name: MarkChecklistItemDone :one
-- Ticking one item. The trigger on this table refuses the two states the product promises cannot
-- exist — a task with an open subtask ticked off, and a subtask with subtasks of its own — so a
-- service that skipped its own check is still stopped here.
UPDATE checklist_items
SET done = true, done_at = now(), updated_at = now()
WHERE id = sqlc.arg('id') AND checklist_id = sqlc.arg('checklist_id') AND workspace_id = sqlc.arg('workspace_id')
RETURNING *;

-- name: CountOpenTasks :one
-- How many top-level tasks are still open. Zero is what settles a plan, which is why this is a count
-- of tasks rather than of items: a task is open until it is ticked, and it cannot be ticked while its
-- subtasks are.
SELECT count(*) FROM checklist_items
WHERE checklist_id = $1 AND workspace_id = $2 AND parent_id IS NULL AND NOT done;

-- name: DeleteChecklistItem :execrows
DELETE FROM checklist_items WHERE id = $1 AND checklist_id = $2 AND workspace_id = $3;
