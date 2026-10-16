-- The operator surface's queries: every account on the deployment, and what the
-- accounts add up to.
--
-- They are their own file rather than part of the auth module's for the reason
-- the module exists: the auth statements are about one account and the tokens
-- that prove a request is that account, while these answer about the whole
-- deployment. It is the one surface whose subject is the deployment rather than
-- a tenant, which is also why its boundary is drawn where it is: every statement
-- below reads `users`, and the only tenant table it touches is `members`, which
-- it counts rather than reads — an operator sees how many workspaces an account
-- belongs to, never which. Bolus, tasks, conversations and money all stay behind
-- the workspace policies, unreadable from here.
--
-- The schema is `migrations`, the same as every other module, so this package's
-- sqlcgen also carries the tenant tables' models. Nothing here selects from
-- them, and that is the whole point of the boundary.

-- name: ListUsers :many
-- The accounts, newest first, with an optional address search and role filter.
-- `*` is the whole account row, which is what lets the payload say whether an
-- account has a password and whether it is a Google one — the two facts that
-- answer "why can this person not sign in".
SELECT * FROM users
WHERE (sqlc.arg('search')::text = '' OR lower(email) LIKE '%' || lower(sqlc.arg('search')::text) || '%')
  AND (sqlc.arg('role')::text = '' OR role = sqlc.arg('role')::text)
ORDER BY created_at DESC
LIMIT sqlc.arg('limit_count') OFFSET sqlc.arg('offset_count');

-- name: CountUsers :one
-- The same filter as ListUsers, so the number of pages and the rows on them
-- cannot disagree.
SELECT count(*) FROM users
WHERE (sqlc.arg('search')::text = '' OR lower(email) LIKE '%' || lower(sqlc.arg('search')::text) || '%')
  AND (sqlc.arg('role')::text = '' OR role = sqlc.arg('role')::text);

-- name: GetUser :one
-- One account, in the same shape the list reads, so a promotion answers with the
-- account it changed.
SELECT * FROM users WHERE id = $1;

-- name: PromoteUser :one
-- Makes an account a platform admin. It is unconditional on purpose: adding an
-- admin can never lock anybody out, so there is nothing to guard.
UPDATE users
SET role = 'admin', updated_at = now()
WHERE id = $1
RETURNING *;

-- name: DemoteUser :one
-- Takes the platform role away, and refuses to take the last one.
--
-- The guard is in the statement rather than in the service because the two
-- conditions it has to hold under — "there is more than one admin" and "this row
-- is still an admin" — are the same two a concurrent demotion would slip
-- between. Two admins demoting each other at the same moment would each read a
-- count of two and each write, and the deployment would be left with nobody able
-- to reach this surface. Reading the count and writing the row in one statement
-- is what makes that impossible; a no-rows answer means the rule held, and the
-- service reports it as such.
UPDATE users
SET role = 'user', updated_at = now()
WHERE users.id = $1
  AND users.role = 'admin'
  AND (SELECT count(*) FROM users AS admins WHERE admins.role = 'admin') > 1
RETURNING *;

-- name: CountUserWorkspaces :many
-- How many workspaces each account on this page belongs to, counted in one
-- statement rather than one query per row.
SELECT m.user_id, count(*) AS workspace_count
FROM members m
WHERE m.user_id = ANY(sqlc.arg('user_ids')::uuid[])
GROUP BY m.user_id;

-- name: UserStats :one
-- What the accounts add up to, counted in one pass rather than six.
SELECT count(*) AS total,
       count(*) FILTER (WHERE role = 'admin') AS admins,
       count(*) FILTER (WHERE role = 'user') AS users,
       count(*) FILTER (WHERE email_verified_at IS NOT NULL) AS verified,
       count(*) FILTER (WHERE onboarded_at IS NOT NULL) AS onboarded,
       count(*) FILTER (WHERE google_subject IS NOT NULL) AS google_accounts,
       count(*) FILTER (WHERE created_at >= now() - interval '7 days') AS new_last_7_days
FROM users;
