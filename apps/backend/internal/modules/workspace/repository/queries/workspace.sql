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
-- only needs the three columns the rule is about.
SELECT timezone, work_hours_start, work_hours_end
FROM workspaces WHERE id = $1;

-- name: UpdateWorkspace :one
UPDATE workspaces
SET name = $2, business_field = $3, timezone = $4, updated_at = now()
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
