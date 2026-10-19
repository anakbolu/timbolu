-- Reverses 000009. Nothing else refers to the table, so it goes with its data:
-- a state that has not been consumed is a flow somebody abandoned.
DROP POLICY IF EXISTS oauth_states_tenant_isolation ON oauth_states;
DROP TABLE IF EXISTS oauth_states;
