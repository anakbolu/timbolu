-- name: CreateWorkspace :one
-- Runs with app.user_id set; the RLS insert policy requires the owner to be the
-- authenticated user, so a workspace can never be created for someone else.
INSERT INTO workspaces (name, business_field, timezone, owner_user_id)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetWorkspace :one
SELECT * FROM workspaces WHERE id = $1;

-- name: GetWorkspaceHours :one
-- The window a workspace lets actions leave in. It is read on its own rather than
-- through GetWorkspace because the approval runtime asks for it on every send and
-- only needs the columns the rule is about.
SELECT timezone, work_hours_start, work_hours_end, work_days
FROM workspaces WHERE id = $1;

-- name: UpdateWorkspace :one
-- Every column the Pengaturan screen edits, in one statement: the screen saves a
-- field at a time and a half-written profile would let the approval gate hold an
-- action against a window that never existed.
UPDATE workspaces
SET name = $2,
    business_field = $3,
    timezone = $4,
    language = $5,
    work_hours_start = $6,
    work_hours_end = $7,
    work_days = $8,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ListWorkspacesForUser :many
-- Powers the workspace switcher. The members read policy lets a user see their
-- own membership rows without an active workspace.
SELECT w.*, m.role
FROM workspaces w
JOIN members m ON m.workspace_id = w.id
WHERE m.user_id = $1
ORDER BY w.created_at;

-- name: GetOwnedWorkspaceForUser :one
-- Onboarding idempotency: the workspace this user already created.
SELECT * FROM workspaces
WHERE owner_user_id = $1
ORDER BY created_at
LIMIT 1;

-- name: CreateTeam :one
INSERT INTO teams (workspace_id, name, kind)
VALUES ($1, $2, $3)
RETURNING *;

-- name: ListTeams :many
SELECT * FROM teams WHERE workspace_id = $1 ORDER BY name;

-- name: CreateMember :one
INSERT INTO members (workspace_id, user_id, role)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetMembership :one
-- The tenant middleware uses this to turn a requested workspace id into a role.
SELECT * FROM members WHERE workspace_id = $1 AND user_id = $2;

-- name: ListMembers :many
SELECT m.*, u.email, u.email_verified_at
FROM members m
JOIN users u ON u.id = m.user_id
WHERE m.workspace_id = $1
ORDER BY m.created_at;

-- name: UpdateMemberRole :one
UPDATE members
SET role = $3, updated_at = now()
WHERE workspace_id = $1 AND user_id = $2
RETURNING *;

-- name: DeleteMember :execrows
DELETE FROM members WHERE workspace_id = $1 AND user_id = $2;

-- name: CountOwners :one
SELECT count(*)::bigint FROM members WHERE workspace_id = $1 AND role = 'owner';

-- name: CountMembers :one
SELECT count(*)::bigint FROM members WHERE workspace_id = $1;

-- name: CreateInvitation :one
INSERT INTO invitations (workspace_id, email, role, token_hash, invited_by, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetInvitationByTokenHash :one
-- Readable by the invitee (policy matches their email) so they can accept
-- before they are a member. The LEFT JOIN keeps the row even if the workspace
-- is not readable, in which case the name is empty.
SELECT i.*, coalesce(w.name, '')::text AS workspace_name
FROM invitations i
LEFT JOIN workspaces w ON w.id = i.workspace_id
WHERE i.token_hash = $1;

-- name: AcceptInvitation :one
UPDATE invitations
SET accepted_at = now(), accepted_by = $2
WHERE id = $1 AND accepted_at IS NULL
RETURNING *;

-- name: ListInvitations :many
SELECT * FROM invitations
WHERE workspace_id = $1 AND accepted_at IS NULL
ORDER BY created_at DESC;

-- name: DeleteInvitation :execrows
DELETE FROM invitations WHERE workspace_id = $1 AND id = $2;

-- name: GetMemberDetail :one
-- A single membership with the account's address, used when a role change has
-- to be reported back with the member's email.
SELECT m.id, m.workspace_id, m.user_id, m.role, m.created_at, m.updated_at,
       u.email, u.email_verified_at
FROM members m
JOIN users u ON u.id = m.user_id
WHERE m.workspace_id = $1 AND m.user_id = $2;

-- ------------------------------------------------------------- erasure

-- name: WorkspaceName :one
-- The name, read before the row that holds it is deleted: an erasure record that says a uuid was
-- removed cannot be shown to the person who asked for their data back.
SELECT name FROM workspaces WHERE id = $1;

-- name: RequesterEmail :one
-- The requester's address, read for the same reason.
SELECT email FROM users WHERE id = $1;

-- name: CountWorkspaceRows :one
-- How many rows the cascade will take, counted from the schema rather than from a list.
--
-- **This is the design.** A hand-written list of tables would be a list that drifts: a migration
-- adds a table with a `workspace_id` and whoever writes it has no reason to know a deletion walk
-- exists — and the failure is silent, because the row that was missed is still there and nothing
-- looks wrong. Reading `information_schema` means every table that declares the column is counted,
-- so a new one is included the day it is created.
--
-- What it cannot see is a table that stores a workspace id *without* the reference — which is
-- exactly what `data_erasures` does deliberately, and the one case where the reference is absent
-- because the row must outlive the workspace.
SELECT COALESCE(sum(counted.rows), 0)::bigint AS rows
FROM (
    SELECT (
        xpath(
            '/row/c/text()',
            query_to_xml(
                format('SELECT count(*) AS c FROM %I.%I WHERE workspace_id = %L',
                       c.table_schema, c.table_name, sqlc.arg('workspace_id')::text),
                false, true, ''
            )
        )
    )[1]::text::bigint AS rows
    FROM information_schema.columns c
    JOIN information_schema.tables t
      ON t.table_schema = c.table_schema AND t.table_name = c.table_name
    WHERE c.column_name = 'workspace_id'
      AND c.table_schema = current_schema()
      AND t.table_type = 'BASE TABLE'
      AND t.table_name <> 'data_erasures'
) AS counted;

-- name: DeleteWorkspace :execrows
-- Deletes the workspace, and with it every row that references it.
--
-- One statement rather than twenty: the foreign keys are what carry the deletion, so a module that
-- adds a table is covered by the reference it declares. The row it removes is the tenant itself,
-- which is what makes this a deletion rather than a soft delete — a flag would leave every other
-- row in place and make "erased" a promise about a column.
DELETE FROM workspaces WHERE id = $1;
