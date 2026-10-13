-- The reverse of 000017: the trail loses its guard and its read indexes.
--
-- What is *not* reversed is the trail itself: every entry the deployment recorded stays.
-- Removing the guard is the point of rolling this back — a deployment that does it can
-- rewrite its own history again, which is stated here rather than discovered later.

DROP INDEX IF EXISTS audit_logs_object_idx;
DROP INDEX IF EXISTS audit_logs_action_created_idx;
DROP INDEX IF EXISTS audit_logs_agent_created_idx;

DROP TRIGGER IF EXISTS audit_logs_append_only ON audit_logs;
DROP FUNCTION IF EXISTS audit_logs_append_only();
