-- Task runtime: the columns the agent workflow needs on top of 000001.
--
-- The `tasks` and `task_steps` tables already exist from the bootstrap
-- migration. What this adds is what running a durable task needs and what the
-- task endpoints read.

-- --------------------------------------------------------- handoff chain depth

-- How far down a chain of handoffs a task is: a task a user started is depth 0,
-- and a task another Bolu was asked to do is one deeper. The workflow refuses to
-- go past a ceiling, so a chain of handoffs cannot recurse without bound.
--
-- Stored rather than computed by walking parents: the check happens before each
-- handoff, on a hot path, and a recursive query per decision is the wrong shape
-- for it.
ALTER TABLE tasks ADD COLUMN depth INT NOT NULL DEFAULT 0
    CHECK (depth >= 0 AND depth <= 16);

-- ------------------------------------------------------------------ liveness

-- A running task touches this as it works. It is what tells a task that is
-- working from one whose worker died: the status alone cannot, because both
-- read `running`.
ALTER TABLE tasks ADD COLUMN heartbeat_at TIMESTAMPTZ;

-- What a task is waiting for, in the user's words. It is separate from
-- `stopped_reason` because a waiting task has not stopped: it is parked, and the
-- office shows it at the approval desk rather than as finished.
ALTER TABLE tasks ADD COLUMN waiting_reason TEXT NOT NULL DEFAULT '';

-- A task waiting on an approval points at the draft it created, so the office
-- and the task endpoint can link the two without scanning the drafts table.
ALTER TABLE tasks ADD COLUMN waiting_draft_id UUID REFERENCES drafts (id) ON DELETE SET NULL;

-- ------------------------------------------------------------- history reads

-- The message a task's answer fills. The chat module writes the placeholder
-- before dispatching, so the task records it here: a restart mid-answer must
-- finish the same message rather than start a second one.
ALTER TABLE tasks ADD COLUMN reply_message_id UUID REFERENCES messages (id) ON DELETE SET NULL;

-- The history endpoint filters by status and orders newest first, which the
-- creation index does not serve once a status filter is applied.
CREATE INDEX tasks_workspace_status_created_idx
    ON tasks (workspace_id, status, created_at DESC);

-- Tasks still running or parked, which is what the office and the "stuck" mark
-- read. Partial, because a finished task is never looked up this way.
CREATE INDEX tasks_live_idx ON tasks (workspace_id, heartbeat_at)
    WHERE status IN ('queued', 'running', 'waiting_approval');

-- A child task points at its parent, which is how a handoff chain is read.
CREATE INDEX tasks_parent_idx ON tasks (parent_task_id)
    WHERE parent_task_id IS NOT NULL;

-- The steps of a task are read in order, and the summary reads the last one.
CREATE INDEX task_steps_task_created_idx ON task_steps (task_id, created_at DESC);

-- ---------------------------------------------------------------- invariants

-- A task that has started always has a workflow, and a task that never started
-- never has one. The workflow id is what a restart resumes with, so a running
-- task without one is a bug rather than a state.
ALTER TABLE tasks ADD CONSTRAINT tasks_started_has_workflow
    CHECK (
        status NOT IN ('running', 'waiting_approval')
        OR workflow_id <> ''
    );
