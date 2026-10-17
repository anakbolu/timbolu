-- Approval runtime: what the draft gate needs on top of the bootstrap schema.
--
-- `approval_policies`, `drafts`, and `audit_logs` already exist from 000001, and
-- `workspaces` already carries `work_hours_start`, `work_hours_end`, and
-- `timezone`. What this adds is what deciding a draft and holding one until the
-- working day actually needs.

-- ------------------------------------------------------------- hold until open

-- The earliest moment a draft may leave. A draft approved outside working hours
-- is held until the next window opens, and the workflow waits on this rather than
-- on a guess: the value is computed from the workspace's own hours and timezone
-- at the moment the decision is carried out, so changing either recomputes it.
ALTER TABLE drafts ADD COLUMN not_before TIMESTAMPTZ;

-- Why a draft has not gone out, in the user's words. It is separate from the
-- decision: a draft can be approved and still be held, and the card has to say
-- which of the two it is waiting on.
ALTER TABLE drafts ADD COLUMN blocked_reason TEXT NOT NULL DEFAULT '';

-- ----------------------------------------------------------------- the card

-- What the draft is, and one line about what it does. Both are what the chat card
-- and the dashboard row show.
--
-- Stored rather than derived: the payload is the action's own shape, and reading
-- a label out of it is the action's business rather than the UI's. A card with no
-- label would leave a person deciding on a payload alone.
ALTER TABLE drafts ADD COLUMN title TEXT NOT NULL DEFAULT '';
ALTER TABLE drafts ADD COLUMN summary TEXT NOT NULL DEFAULT '';

-- The message in the thread that carries the draft card.
--
-- A decision rewrites that message, so a reload shows what actually happened
-- instead of a card still offering to approve something already sent. It is the
-- same shape as `tasks.reply_message_id`, for the same reason.
ALTER TABLE drafts ADD COLUMN card_message_id UUID REFERENCES messages (id) ON DELETE SET NULL;

-- ------------------------------------------------------------- revision trail

-- Every revision a draft went through, with the note the person left.
--
-- A revision ends the draft it was made against: the agent goes back to its loop
-- and produces a new draft, so the old row keeps `status = 'revise'` forever. The
-- trail is what makes "why was this sent, after three attempts" answerable.
CREATE TABLE draft_revisions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    draft_id UUID NOT NULL REFERENCES drafts (id) ON DELETE CASCADE,
    -- revision is the draft's revision counter after this decision, so the trail
    -- reads in the order the person saw it.
    revision INT NOT NULL,
    note TEXT NOT NULL DEFAULT '',
    -- payload is what the draft held when the revision was asked for, which is
    -- what the person was looking at.
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    decided_by UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX draft_revisions_draft_idx ON draft_revisions (draft_id, revision);

-- ---------------------------------------------------------------- invariants

-- A draft that has been sent has a send time, and one that has not has none. The
-- pair is what makes "was this actually sent" answerable from the row alone.
ALTER TABLE drafts ADD CONSTRAINT drafts_sent_has_timestamp
    CHECK ((status = 'sent') = (sent_at IS NOT NULL));

-- A held draft always says why it is held.
ALTER TABLE drafts ADD CONSTRAINT drafts_held_has_reason
    CHECK (not_before IS NULL OR blocked_reason <> '');

-- ------------------------------------------------------------- read patterns

-- The dashboard lists a workspace's waiting drafts, newest first.
CREATE INDEX drafts_workspace_status_created_idx
    ON drafts (workspace_id, status, created_at DESC);

-- One Bolu's drafts, which the agent screen and the task card filter by.
CREATE INDEX drafts_agent_created_idx ON drafts (agent_id, created_at DESC);

-- The drafts a task produced, which is how a task's card reads its decisions.
CREATE INDEX drafts_task_created_idx ON drafts (task_id, created_at DESC);

-- ---------------------------------------------------------------- RLS on the trail

-- draft_revisions is a new tenant table, so it needs the same policy every other
-- one has. Written here rather than added to 000001's loop because a migration is
-- the record of what changed and when.
ALTER TABLE draft_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE draft_revisions FORCE ROW LEVEL SECURITY;

CREATE POLICY draft_revisions_tenant_isolation ON draft_revisions
    USING (workspace_id = app_current_workspace_id())
    WITH CHECK (workspace_id = app_current_workspace_id());

-- ------------------------------------------------------------- policy defaults

-- Every workspace gets an explicit conservative row for every action kind that
-- reaches outside Bolu.
--
-- The code already treats an absent policy as `always_draft`, so this is not what
-- makes the product safe: it is what makes the policy screen show something to
-- edit, and what makes "this workspace has no policy for sending email" a state
-- that cannot exist. `ON CONFLICT DO NOTHING` keeps it idempotent and leaves an
-- existing decision alone.
INSERT INTO approval_policies (workspace_id, action_kind, mode)
SELECT w.id, tc.name, 'always_draft'
FROM workspaces w
CROSS JOIN tool_catalog tc
WHERE tc.label = 'write_external'
ON CONFLICT (workspace_id, action_kind) DO NOTHING;
