-- EPIC 15 · The plan a Bolu keeps for itself: what it has to do, and what it has already done.
--
-- Every other table in this schema is about work a person gave a Bolu — a chat message, a routine, a
-- webhook. This one is about work a Bolu gave itself. The tools below let it write down the steps it
-- intends to take, tick them off as it goes, and read the list back after a restart, which is what
-- makes a multi-step job something it can hold on to rather than something it has to keep in a single
-- model round.
--
-- Four decisions are worth reading rather than inferring:
--
--   1. **One active list per Bolu, enforced by the database.** A partial unique index says so, the way
--      `routine_schedule_key` says a routine has one schedule. The product rule is "finish the plan
--      before you make another", and a rule that matters is a constraint rather than a check the
--      service remembers to make.
--   2. **A subtask is one level deep.** `parent_id` points at a task, and a trigger refuses a parent
--      that itself has a parent. A tree of arbitrary depth reads well in a diagram and badly in a
--      model's plan: the point of this table is a short list a person can scan, not a project plan.
--   3. **Completion is derived, not declared.** A list is `done` when every top-level task is ticked,
--      and a task with an unfinished subtask cannot be ticked — both enforced where the rows are
--      written. A status a model could set while items were open would be a status that lies about
--      what happened.
--   4. **Clearing is a delete.** `checklist.clear` removes the list and its items rather than marking
--      it abandoned: a plan the Bolu decided was wrong is not history worth keeping, and an
--      `abandoned` status would leave a row whose only reader is a screen asking "what is this?".
--      What a Bolu *did* is in `task_steps`; this table holds what it intends to do.

-- ------------------------------------------------------------------ the list

CREATE TABLE checklists (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    -- The Bolu that owns the plan. It is the identity the rule "one at a time" is about, and the
    -- column a screen groups by.
    agent_id UUID NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    title TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'done')),
    -- The task the Bolu was working on when it wrote the list down, for the same reason an order
    -- carries one: "which piece of work produced this" stays answerable. Null when the task is
    -- removed, because the plan outlives the task that happened to create it.
    task_id UUID REFERENCES tasks (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ
);

-- One active list per Bolu. Partial because the finished ones accumulate: a Bolu that has run a
-- hundred jobs has a hundred completed plans, and the rule is only about the one still open.
CREATE UNIQUE INDEX checklists_agent_active_key
    ON checklists (workspace_id, agent_id)
    WHERE status = 'active';

-- The screen's read: a workspace's plans, newest first, narrowed to one Bolu or one state.
CREATE INDEX checklists_workspace_created_idx ON checklists (workspace_id, created_at DESC);

ALTER TABLE checklists ENABLE ROW LEVEL SECURITY;
ALTER TABLE checklists FORCE ROW LEVEL SECURITY;
CREATE POLICY checklists_tenant_isolation ON checklists
    USING (workspace_id = app_current_workspace_id())
    WITH CHECK (workspace_id = app_current_workspace_id());

-- ------------------------------------------------------------------ the items

CREATE TABLE checklist_items (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    checklist_id UUID NOT NULL REFERENCES checklists (id) ON DELETE CASCADE,
    -- Denormalised from the list so the tenant policy is one predicate on this table too, and so a
    -- query never has to join upwards to know whose row it is looking at.
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    -- NULL for a top-level task. A subtask's row is otherwise identical, which is what keeps "check
    -- one thing" one statement whatever it is.
    parent_id UUID REFERENCES checklist_items (id) ON DELETE CASCADE,
    title TEXT NOT NULL,
    done BOOLEAN NOT NULL DEFAULT false,
    -- The order the Bolu wrote them in. It is what a person reads, so it is stored rather than
    -- derived from creation time: two items written in the same statement share a timestamp to the
    -- microsecond, and their order would then be whatever the database felt like.
    position INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    done_at TIMESTAMPTZ
);

CREATE INDEX checklist_items_list_idx ON checklist_items (checklist_id, position, created_at);

-- A task with unfinished subtasks cannot be ticked off, and a subtask cannot have subtasks of its
-- own. The service refuses both first, with a sentence a model can read and act on; this trigger is
-- the guarantee underneath it, so the state cannot exist even if a future writer forgets to ask.
CREATE OR REPLACE FUNCTION checklist_items_guard() RETURNS TRIGGER AS $$
BEGIN
    -- One level of nesting: a subtask cannot be a parent.
    IF NEW.parent_id IS NOT NULL THEN
        IF EXISTS (SELECT 1 FROM checklist_items WHERE id = NEW.parent_id AND parent_id IS NOT NULL) THEN
            RAISE EXCEPTION 'checklist: a subtask cannot have its own subtasks'
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;

    -- And a parent with an unfinished subtask cannot be ticked off.
    IF NEW.done AND NEW.parent_id IS NULL THEN
        IF EXISTS (
            SELECT 1 FROM checklist_items
            WHERE parent_id = NEW.id AND NOT done
        ) THEN
            RAISE EXCEPTION 'checklist: a task with unfinished subtasks cannot be marked done'
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER checklist_items_guard_trigger
    BEFORE INSERT OR UPDATE OF done, parent_id ON checklist_items
    FOR EACH ROW EXECUTE FUNCTION checklist_items_guard();

ALTER TABLE checklist_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE checklist_items FORCE ROW LEVEL SECURITY;
CREATE POLICY checklist_items_tenant_isolation ON checklist_items
    USING (workspace_id = app_current_workspace_id())
    WITH CHECK (workspace_id = app_current_workspace_id());

-- ------------------------------------------------------------------ the tools

-- Five tools, and none of them needs a connection: a plan belongs to the Bolu, not to an account
-- somebody connected. `needs_connection = false` is what offers them to any Bolu whose profile asks
-- for them, the same fact `order.find_product` carries.
--
-- Four of the five are `write_internal`: they change what is inside Bolu and nothing outside it, so
-- nobody is asked before they run. `checklist.read` is a read, which is what the label means
-- everywhere else.
INSERT INTO tool_catalog (name, integration_app, label, description, needs_connection) VALUES
    ('checklist.create', 'checklist', 'write_internal',
     'Tulis rencana kerja: daftar tugas, masing-masing boleh punya sub-tugas. Satu Bolu hanya boleh punya satu rencana aktif.',
     false),
    ('checklist.check', 'checklist', 'write_internal',
     'Tandai satu tugas atau sub-tugas sudah selesai.',
     false),
    ('checklist.remove', 'checklist', 'write_internal',
     'Hapus satu tugas (beserta sub-tugasnya) dari rencana yang sedang aktif.',
     false),
    ('checklist.clear', 'checklist', 'write_internal',
     'Buang seluruh rencana yang sedang aktif, kalau rencananya memang sudah tidak dipakai lagi.',
     false),
    ('checklist.read', 'checklist', 'read',
     'Baca rencana yang sedang aktif beserta tugas dan sub-tugasnya.',
     false)
ON CONFLICT (name) DO NOTHING;

-- Every Bolu that already exists gets the five tools, and this is the one place in the codebase where
-- a migration grants a permission nobody asked for. The reason it is right here and was wrong for the
-- order tools: a checklist reaches nothing outside the workspace, so it is not a grant at all — it is
-- the Bolu's own notebook, and a Bolu that cannot write down what it is doing is the state this
-- migration exists to end. A workspace that does not want it can still edit the Bolu's tools on the
-- Bolu screen.
UPDATE agents
SET tools = tools || '["checklist.create","checklist.check","checklist.remove","checklist.clear","checklist.read"]'::jsonb
WHERE deleted_at IS NULL AND NOT tools ? 'checklist.create';

-- And the templates, so a Bolu made from here on is born with the notebook too.
UPDATE agent_templates
SET tools = tools || '["checklist.create","checklist.check","checklist.remove","checklist.clear","checklist.read"]'::jsonb
WHERE NOT tools ? 'checklist.create';
