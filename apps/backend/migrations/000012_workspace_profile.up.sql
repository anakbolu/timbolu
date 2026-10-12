-- Workspace profile: the columns the Pengaturan screen edits.
--
-- `language`, `work_hours_start`, and `work_hours_end` already exist from 000001
-- and are what the approval gate reads when it decides whether an action may
-- leave now. What is missing is the set of days the window applies to: the rule
-- holds an action until "the next open window", and a window that opens on a day
-- the business is closed is not a window. The days are stored as ISO weekday
-- numbers (1 = Monday … 7 = Sunday) so a week is a sorted list rather than seven
-- booleans that can contradict each other.

ALTER TABLE workspaces ADD COLUMN work_days INTEGER[] NOT NULL DEFAULT '{1,2,3,4,5,6}';

-- A day outside 1–7 is not a weekday, and a rule that cannot tell whether today
-- is a working day would either hold an action forever or send it at midnight.
ALTER TABLE workspaces ADD CONSTRAINT workspaces_work_days_valid CHECK (
    array_length(work_days, 1) IS NOT NULL
    AND work_days <@ ARRAY[1,2,3,4,5,6,7]::INTEGER[]
);

-- An empty window is not a window: the gate would read "always closed". Start
-- equal to end would mean the same thing, so it is refused here as well.
ALTER TABLE workspaces ADD CONSTRAINT workspaces_work_hours_ordered CHECK (
    work_hours_start <> work_hours_end
);

-- The reply language. `id` is the app's own language, `en` is English, and `both`
-- asks the Bolu to answer in whatever language the customer wrote in.
ALTER TABLE workspaces ADD CONSTRAINT workspaces_language_valid CHECK (
    language IN ('id', 'en', 'both')
);

-- The six profile templates now name a real body from the character generator
-- (`apps/frontend/src/components/bolu/characters.tsx`) and the colour that
-- character is drawn in, instead of the placeholder `circle` and the default
-- orange. A Bolu created from a template copies both, so it is drawn as the
-- character the design drew rather than as whatever the fallback picks.
UPDATE agent_templates SET shape = 'kacang', color = '#F28C4E' WHERE key = 'oren';
UPDATE agent_templates SET shape = 'hantu',  color = '#2E9BEF' WHERE key = 'biru';
UPDATE agent_templates SET shape = 'gumpal', color = '#A77DC9' WHERE key = 'lila';
UPDATE agent_templates SET shape = 'mochi',  color = '#4DBB72' WHERE key = 'ijo';
UPDATE agent_templates SET shape = 'awan',   color = '#F59ABF' WHERE key = 'pinky';
UPDATE agent_templates SET shape = 'tetes',  color = '#FFC21A' WHERE key = 'kunyit';

-- The six seeded Bolu already created from those templates carry the old
-- placeholder. They are matched by name so a Bolu the workspace renamed or made
-- by hand is left exactly as its owner set it.
UPDATE agents SET shape = 'kacang', color = '#F28C4E' WHERE shape = 'circle' AND name = 'Oren';
UPDATE agents SET shape = 'hantu',  color = '#2E9BEF' WHERE shape = 'circle' AND name = 'Biru';
UPDATE agents SET shape = 'gumpal', color = '#A77DC9' WHERE shape = 'circle' AND name = 'Lila';
UPDATE agents SET shape = 'mochi',  color = '#4DBB72' WHERE shape = 'circle' AND name = 'Ijo';
UPDATE agents SET shape = 'awan',   color = '#F59ABF' WHERE shape = 'circle' AND name = 'Pinky';
UPDATE agents SET shape = 'tetes',  color = '#FFC21A' WHERE shape = 'circle' AND name = 'Kunyit';
