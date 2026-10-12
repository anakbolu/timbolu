-- Reverses 000012.
--
-- Only the constraints and the column this migration added are removed. The
-- language and hours columns belong to 000001 and are left in place, because the
-- approval gate reads them and rolling this back must not take the gate's own
-- settings away. The template bodies are restored to the placeholder they had,
-- so an up/down/up cycle lands on the same rows.

UPDATE agents SET shape = 'circle' WHERE name IN ('Oren', 'Biru', 'Lila', 'Ijo', 'Pinky', 'Kunyit');
UPDATE agent_templates SET shape = 'circle' WHERE key IN ('oren', 'biru', 'lila', 'ijo', 'pinky', 'kunyit');

ALTER TABLE workspaces DROP CONSTRAINT IF EXISTS workspaces_language_valid;
ALTER TABLE workspaces DROP CONSTRAINT IF EXISTS workspaces_work_hours_ordered;
ALTER TABLE workspaces DROP CONSTRAINT IF EXISTS workspaces_work_days_valid;

ALTER TABLE workspaces DROP COLUMN IF EXISTS work_days;
