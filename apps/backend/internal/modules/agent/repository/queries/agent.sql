-- name: ListAgents :many
-- The registry, in the order the sidebar shows it: team by name, then the position
-- a person gave each Bolu. Soft-deleted Bolu are invisible, and the team is joined
-- so the list can be grouped without a second query.
--
-- created_at is the tiebreaker rather than the order itself. sort_order is not
-- unique — two creates may compute the same next position — and a list with no
-- total order would let two reads of the same workspace disagree.
SELECT a.*, t.name AS team_name, t.kind AS team_kind
FROM agents a
JOIN teams t ON t.id = a.team_id
WHERE a.workspace_id = $1 AND a.deleted_at IS NULL
ORDER BY t.name, a.sort_order, a.created_at;

-- name: GetAgent :one
SELECT a.*, t.name AS team_name, t.kind AS team_kind
FROM agents a
JOIN teams t ON t.id = a.team_id
WHERE a.id = $1 AND a.workspace_id = $2 AND a.deleted_at IS NULL;

-- name: CountLiveAgents :one
SELECT count(*)::bigint FROM agents
WHERE workspace_id = $1 AND deleted_at IS NULL;

-- name: CreateAgent :one
-- A new Bolu lands at the end of its team's list, which is where a person looks
-- for the one they just made. The position is computed here rather than passed in:
-- a caller that had to know the current length would be a caller that could get it
-- wrong, and two Bolu with the same position still read in created_at order.
INSERT INTO agents (workspace_id, team_id, name, role, persona, tone, shape, color, tools, template_key, default_model, sort_order)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,
        (SELECT COALESCE(max(sort_order) + 1, 0) FROM agents
         WHERE workspace_id = $1 AND team_id = $2 AND deleted_at IS NULL))
RETURNING *;

-- name: UpdateAgent :one
-- Only the editable profile. The team and the position are changed through
-- SetAgentPlacement, so the workspace_id guard stays in one place.
UPDATE agents
SET name = $3,
    role = $4,
    persona = $5,
    tone = $6,
    shape = $7,
    color = $8,
    tools = $9,
    default_model = $10,
    updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: SetAgentStatus :one
-- The rest switch. The display status is never stored; see AgentActivity.
UPDATE agents
SET status = $3, updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: SetMemoryExtraction :one
-- The switch that decides whether a quiet conversation is read into memory. It
-- lives here because the column belongs to this table; the memory module reads it
-- and the memory screen writes it through this endpoint.
UPDATE agents
SET memory_extraction_enabled = $3, updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: SetAgentPlacement :one
-- Where a Bolu sits: which team, and at which position inside it. One statement for
-- both, because they are one decision — "up" and "into the other team" are the same
-- move, and splitting them would let a caller make half of it.
--
-- updated_at is deliberately not touched: a reorder is not an edit to the profile,
-- and a "last changed" a person reads on the Bolu's own screen would then report a
-- drag as if the persona had been rewritten.
UPDATE agents
SET team_id = $3, sort_order = $4
WHERE id = $1 AND workspace_id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteAgent :execrows
UPDATE agents
SET deleted_at = now(), updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND deleted_at IS NULL;

-- name: CountRunningTasksForAgent :one
-- Guards the delete: a Bolu that is working must be left alone until it stops.
SELECT count(*)::bigint FROM tasks
WHERE agent_id = $1 AND status IN ('queued', 'running');

-- name: AgentActivity :many
-- The raw inputs of the derived display status, for every live Bolu at once.
-- "working" is a task in flight, "waiting" is a draft waiting for a human.
SELECT a.id,
       EXISTS (
           SELECT 1 FROM tasks t
           WHERE t.agent_id = a.id AND t.status IN ('queued', 'running')
       ) AS working,
       EXISTS (
           SELECT 1 FROM drafts d
           WHERE d.agent_id = a.id AND d.status = 'pending'
       ) AS waiting
FROM agents a
WHERE a.workspace_id = $1 AND a.deleted_at IS NULL;

-- name: AgentActivityByID :one
SELECT a.id,
       EXISTS (
           SELECT 1 FROM tasks t
           WHERE t.agent_id = a.id AND t.status IN ('queued', 'running')
       ) AS working,
       EXISTS (
           SELECT 1 FROM drafts d
           WHERE d.agent_id = a.id AND d.status = 'pending'
       ) AS waiting
FROM agents a
WHERE a.id = $1 AND a.workspace_id = $2 AND a.deleted_at IS NULL;

-- name: LatestTaskForAgent :one
-- The "why" behind a derived status, shown next to the Bolu in the UI.
SELECT id, title, status, created_at
FROM tasks
WHERE agent_id = $1 AND status IN ('queued', 'running')
ORDER BY created_at DESC
LIMIT 1;

-- name: LatestPendingDraftForAgent :one
SELECT id, action_kind, created_at
FROM drafts
WHERE agent_id = $1 AND status = 'pending'
ORDER BY created_at DESC
LIMIT 1;

-- ------------------------------------------------------------------ templates

-- name: ListAgentTemplates :many
-- Not tenant data: the same six profiles for every workspace.
SELECT * FROM agent_templates ORDER BY sort_order;

-- name: GetAgentTemplate :one
SELECT * FROM agent_templates WHERE key = $1;

-- ------------------------------------------------------------------- grants

-- name: ListGrants :many
SELECT g.id, g.agent_id, g.integration_id, g.permission, g.created_at, g.updated_at,
       i.app AS integration_app, i.account_label, i.status AS integration_status
FROM agent_grants g
JOIN integrations i ON i.id = g.integration_id
WHERE g.workspace_id = $1 AND g.agent_id = $2
ORDER BY i.app;

-- name: UpsertGrant :one
INSERT INTO agent_grants (workspace_id, agent_id, integration_id, permission)
VALUES ($1, $2, $3, $4)
ON CONFLICT (agent_id, integration_id)
DO UPDATE SET permission = EXCLUDED.permission, updated_at = now()
RETURNING *;

-- name: DeleteGrant :execrows
DELETE FROM agent_grants
WHERE workspace_id = $1 AND agent_id = $2 AND integration_id = $3;

-- name: AllowedToolsForAgent :many
-- The effective tool list: what the agent asks for, restricted to integrations
-- it was actually granted. A read-only grant keeps only the tools that cannot
-- touch anything outside Bolu.
--
-- A tool that belongs to the workspace itself — the catalogue and the order it records — needs no
-- connection and no grant, which is what `needs_connection = false` says: there is no account to
-- authorise and nothing outside this deployment to reach. The EXISTS is what keeps the two rules
-- apart: a connected tool is offered only through a grant, and a workspace tool is offered to any
-- Bolu whose profile asks for it.
SELECT tc.name, tc.integration_app, tc.label, tc.description, tc.needs_connection
FROM agents a
JOIN tool_catalog tc ON tc.name IN (SELECT jsonb_array_elements_text(a.tools))
WHERE a.id = $1 AND a.workspace_id = $2 AND a.deleted_at IS NULL
  AND (
    NOT tc.needs_connection
    OR EXISTS (
      SELECT 1
      FROM integrations i
      JOIN agent_grants g ON g.agent_id = a.id AND g.integration_id = i.id
      WHERE i.workspace_id = a.workspace_id
        AND i.app = tc.integration_app
        AND (g.permission = 'read_write' OR tc.label = 'read')
    )
  )
ORDER BY tc.name;

-- name: ListToolCatalog :many
SELECT * FROM tool_catalog ORDER BY integration_app, name;

-- name: ListIntegrations :many
-- The integrations of the workspace, which the grant screen lists.
SELECT * FROM integrations WHERE workspace_id = $1 ORDER BY app, account_label;

-- name: ListWorkspaceTeams :many
-- The teams of the workspace, which the registry groups its Bolu by.
SELECT * FROM teams WHERE workspace_id = $1 ORDER BY name;
