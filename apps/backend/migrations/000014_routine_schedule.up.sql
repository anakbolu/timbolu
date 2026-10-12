-- E11.1 · What the scheduler needs on top of the routine tables.
--
-- The bootstrap migration created `routines` and `routine_runs` as the schema
-- document describes them. This one adds the four things a real schedule needs,
-- and each is here because its absence would be a defect rather than a gap:

-- 1. The minute of a fixed-time schedule. `hour_start` alone cannot express
--    "06:30", and rounding every routine to the top of the hour is a product
--    decision nobody made.
ALTER TABLE routines ADD COLUMN minute INT NOT NULL DEFAULT 0
    CONSTRAINT routines_minute_valid CHECK (minute BETWEEN 0 AND 59);

-- 2. The day of a weekly schedule, as an ISO weekday (1 = Monday … 7 = Sunday),
--    matching `workspaces.work_days` so the two cannot mean different things.
ALTER TABLE routines ADD COLUMN day_of_week INT
    CONSTRAINT routines_day_of_week_valid CHECK (day_of_week BETWEEN 1 AND 7);

-- 3. One Schedule per routine, enforced by the database rather than by hoping the
--    service is careful. The sync is create-or-update against this column, and a
--    duplicate would mean two Schedules firing the same routine: the double
--    execution the epic's first gate forbids.
CREATE UNIQUE INDEX routines_schedule_id_key ON routines (schedule_id)
    WHERE schedule_id <> '';

-- 4. History outlives the routine it belongs to.
--
--    E11.1 asks that deleting a routine removes its Schedule while the run history
--    stays. With the bootstrap's ON DELETE CASCADE, deleting a routine erased its
--    own history — the opposite of what the task says, and the worst direction to
--    be wrong in: "what did this routine do last month" is exactly the question
--    somebody asks after turning it off.
--
--    So the link becomes nullable and the name is snapshotted. A run whose routine
--    is gone still reads as a line in the history, and the row that names it is the
--    name at the time it ran rather than a dangling id.
ALTER TABLE routine_runs ALTER COLUMN routine_id DROP NOT NULL;
ALTER TABLE routine_runs ADD COLUMN routine_name TEXT NOT NULL DEFAULT '';

UPDATE routine_runs r
SET routine_name = routines.name
FROM routines
WHERE routines.id = r.routine_id AND r.routine_name = '';

ALTER TABLE routine_runs DROP CONSTRAINT routine_runs_routine_id_fkey;
ALTER TABLE routine_runs ADD CONSTRAINT routine_runs_routine_id_fkey
    FOREIGN KEY (routine_id) REFERENCES routines (id) ON DELETE SET NULL;

-- 5. Why a routine stopped running by itself, in the user's words.
--
--    A Schedule that Temporal paused (a failed action, with pause-on-failure) is a
--    state the screen has to explain; without this column the routine would just
--    read as disabled with no reason given.
ALTER TABLE routines ADD COLUMN paused_reason TEXT NOT NULL DEFAULT '';

-- The trigger lookup: a routine started by an event is found by the event it
-- watches, and the run's cheap check asks the same question.
CREATE INDEX routines_trigger_idx ON routines (workspace_id, trigger_source)
    WHERE trigger_source <> '' AND enabled;

-- The list the screen reads: one Bolu's routines, newest first.
CREATE INDEX routines_workspace_created_idx ON routines (workspace_id, created_at DESC);

-- The history the screen reads, per routine and workspace-wide when a routine has
-- been deleted (routine_id is then null, so the name is what remains).
CREATE INDEX routine_runs_workspace_started_idx
    ON routine_runs (workspace_id, started_at DESC);
