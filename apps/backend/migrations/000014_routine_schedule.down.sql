DROP INDEX IF EXISTS routine_runs_workspace_started_idx;
DROP INDEX IF EXISTS routines_workspace_created_idx;
DROP INDEX IF EXISTS routines_trigger_idx;
DROP INDEX IF EXISTS routines_schedule_id_key;

-- The link goes back to NOT NULL before the rows that would break it do: a run
-- whose routine was deleted has a null id and cannot be restored, so the down file
-- says so rather than failing halfway through on the constraint.
DELETE FROM routine_runs WHERE routine_id IS NULL;

ALTER TABLE routine_runs DROP CONSTRAINT routine_runs_routine_id_fkey;
ALTER TABLE routine_runs ADD CONSTRAINT routine_runs_routine_id_fkey
    FOREIGN KEY (routine_id) REFERENCES routines (id) ON DELETE CASCADE;
ALTER TABLE routine_runs ALTER COLUMN routine_id SET NOT NULL;
ALTER TABLE routine_runs DROP COLUMN IF EXISTS routine_name;

ALTER TABLE routines DROP COLUMN IF EXISTS paused_reason;
ALTER TABLE routines DROP COLUMN IF EXISTS day_of_week;
ALTER TABLE routines DROP COLUMN IF EXISTS minute;
