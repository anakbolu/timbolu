-- Approval queries. Every statement carries workspace_id as well as the Row
-- Level Security policy, so a wrong id in application code still cannot reach
-- another tenant's draft.

-- ------------------------------------------------------------------- drafts

-- name: CreateDraft :one
-- The insert is a SELECT rather than a VALUES list for the same reason the task
-- insert is: a foreign key check runs with the table owner's rights and bypasses
-- Row Level Security, so the statement has to refuse a task or an agent from
-- another workspace itself.
--
-- The conflict clause makes the proposal idempotent. A retried activity sends the
-- same key, and the no-op update returns the draft that already exists instead of
-- opening a second one for the same action.
INSERT INTO drafts (
    workspace_id, task_id, agent_id, action_kind, payload,
    status, title, summary, idempotency_key
)
SELECT
    sqlc.arg('workspace_id'),
    t.id,
    t.agent_id,
    sqlc.arg('action_kind'),
    sqlc.arg('payload'),
    'pending',
    sqlc.arg('title'),
    sqlc.arg('summary'),
    sqlc.arg('idempotency_key')
FROM tasks t
WHERE t.id = sqlc.arg('task_id')
  AND t.workspace_id = sqlc.arg('workspace_id')
  AND t.agent_id = sqlc.arg('agent_id')
ON CONFLICT (workspace_id, idempotency_key) DO UPDATE
    SET updated_at = drafts.updated_at
RETURNING *;

-- name: GetDraft :one
-- The conversation comes from the task, because that is where the card lives.
--
-- `sqlc.embed` keeps the draft's own columns as the generated row type, so one
-- converter serves this read and the list below rather than one per query.
SELECT sqlc.embed(d), t.conversation_id AS conversation_id
FROM drafts d
JOIN tasks t ON t.id = d.task_id
WHERE d.id = $1 AND d.workspace_id = $2;

-- name: ListDrafts :many
-- Every filter is optional and the ordering is fixed, so the endpoint has one
-- shape rather than one per combination.
SELECT sqlc.embed(d), t.conversation_id AS conversation_id
FROM drafts d
JOIN tasks t ON t.id = d.task_id
WHERE d.workspace_id = sqlc.arg('workspace_id')
  AND (sqlc.narg('status')::text IS NULL OR d.status = sqlc.narg('status')::text)
  AND (sqlc.narg('agent_id')::uuid IS NULL OR d.agent_id = sqlc.narg('agent_id')::uuid)
  AND (sqlc.narg('action_kind')::text IS NULL OR d.action_kind = sqlc.narg('action_kind')::text)
  AND (sqlc.narg('task_id')::uuid IS NULL OR d.task_id = sqlc.narg('task_id')::uuid)
ORDER BY d.created_at DESC, d.id DESC
LIMIT sqlc.arg('limit_count');

-- name: DecideDraft :one
-- Only a draft nobody has decided on can be decided, so a second decision
-- changes nothing: the row is already approved, revised, or canceled.
UPDATE drafts
SET status = sqlc.arg('status'),
    decided_by = sqlc.arg('decided_by'),
    decided_at = now(),
    decision_note = sqlc.arg('note'),
    revision = revision + sqlc.arg('bump_revision')::int,
    updated_at = now()
WHERE id = sqlc.arg('id')
  AND workspace_id = sqlc.arg('workspace_id')
  AND status = 'pending'
RETURNING *;

-- name: MarkDraftSent :one
-- The send time and the status are written together: the check constraint pairs
-- them, so a row claiming `sent` always says when.
UPDATE drafts
SET status = 'sent',
    sent_at = now(),
    not_before = NULL,
    blocked_reason = '',
    updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND status = 'approved'
RETURNING *;

-- name: HoldDraft :one
-- A hold always carries its reason, which the check constraint enforces.
UPDATE drafts
SET not_before = sqlc.arg('not_before'),
    blocked_reason = sqlc.arg('blocked_reason'),
    updated_at = now()
WHERE id = sqlc.arg('id')
  AND workspace_id = sqlc.arg('workspace_id')
  AND status = 'approved'
RETURNING *;

-- name: SetDraftCard :one
UPDATE drafts
SET card_message_id = sqlc.arg('card_message_id'), updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
RETURNING *;

-- ------------------------------------------------------------- revision trail

-- name: AppendDraftRevision :one
INSERT INTO draft_revisions (workspace_id, draft_id, revision, note, payload, decided_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListDraftRevisions :many
SELECT * FROM draft_revisions
WHERE workspace_id = $1 AND draft_id = $2
ORDER BY revision, created_at;

-- ----------------------------------------------------------------- policies

-- name: ListPolicies :many
SELECT * FROM approval_policies
WHERE workspace_id = $1
ORDER BY action_kind;

-- name: GetPolicy :one
SELECT * FROM approval_policies
WHERE workspace_id = $1 AND action_kind = $2;

-- name: UpsertPolicy :one
INSERT INTO approval_policies (workspace_id, action_kind, mode, max_amount, updated_by)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (workspace_id, action_kind) DO UPDATE
    SET mode = EXCLUDED.mode,
        max_amount = EXCLUDED.max_amount,
        updated_by = EXCLUDED.updated_by,
        updated_at = now()
RETURNING *;

-- name: SeedPolicies :execrows
-- The conservative rows a new workspace starts with. `ON CONFLICT DO NOTHING`
-- leaves a decision the workspace already made alone.
INSERT INTO approval_policies (workspace_id, action_kind, mode)
SELECT $1, tc.name, 'always_draft'
FROM tool_catalog tc
WHERE tc.label = 'write_external'
ON CONFLICT (workspace_id, action_kind) DO NOTHING;

-- name: ListExternalActionKinds :many
-- The action kinds that reach outside Bolu, which is what a workspace has
-- policies for. tool_catalog is not tenant-scoped: it is the catalogue the
-- product ships, not something a workspace owns.
SELECT name FROM tool_catalog
WHERE label = 'write_external'
ORDER BY name;

-- -------------------------------------------------------------------- audit

-- name: InsertAuditEntry :one
INSERT INTO audit_logs (
    workspace_id, actor_user_id, actor_agent_id, action, object_type, object_id, metadata
)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, created_at;

-- name: ListAuditEntries :many
SELECT * FROM audit_logs
WHERE workspace_id = $1
ORDER BY created_at DESC, id DESC
LIMIT $2;
