-- The platform role of an account: `admin` reaches the operator surface at
-- /admin, `user` is everybody else.
--
-- It is deliberately not `members.role`. That one answers "what may this person
-- do inside this workspace" and is granted by whoever owns the workspace; this
-- one answers "may this person see the deployment", and no screen grants it.
--
-- The default is what makes the migration safe on a live database: every
-- account that exists when this runs, and every account the sign-up flow
-- creates afterwards, is a plain user. Promotion is an explicit act, and the
-- only way to make the first admin is to run one statement by hand:
--
--   UPDATE users SET role = 'admin' WHERE lower(email) = 'someone@example.com';
--
-- A second admin can then be promoted from the operator surface itself.

ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'user'
    CHECK (role IN ('user', 'admin'));

-- The role check runs on every request that reaches the operator surface, and
-- the list of admins is a handful of rows out of the whole table, so only the
-- admin rows are indexed.
CREATE INDEX users_admin_idx ON users (id) WHERE role = 'admin';
