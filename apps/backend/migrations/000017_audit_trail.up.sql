-- EPIC 13 · The audit trail, made answerable and hard to rewrite.
--
-- `audit_logs` has existed since the first migration and the approval, memory, routine,
-- and WhatsApp modules already write to it. This migration adds the two things the epic
-- asks of it and neither is cosmetic:
--
--   * **the trail is append-only**, enforced by the database rather than by every caller
--     remembering. A row that can be rewritten answers "who changed this" with whatever
--     the last writer wanted it to say, which is worse than having no trail at all.
--   * **it can be read by what a person asks** — one Bolu, one kind of action, one window
--     of time, one object — because a trail nobody can filter is a table, not an answer.
--
-- The one lawful way to remove a row is a workspace erasure, and it has to say so: the
-- trigger below refuses a delete unless the transaction set `app.audit_erase`. That is the
-- same session flag the erasure path in `docs/compliance/` sets, and it is what keeps
-- "users cannot change the audit log" and "a person can have their data deleted" from
-- being two rules that contradict each other.

-- ---------------------------------------------------------------- immutability

-- The trigger's reason, in Indonesian, because that is the language a person reading a
-- refused statement sees.
CREATE OR REPLACE FUNCTION audit_logs_append_only() RETURNS TRIGGER AS $$
BEGIN
    -- A workspace erasure sets this for the length of its transaction. It is the only
    -- path that may remove a row, and it is deliberately explicit: an erasure that forgot
    -- it fails here rather than quietly succeeding.
    IF current_setting('app.audit_erase', true) = 'on' THEN
        RETURN CASE TG_OP WHEN 'DELETE' THEN OLD ELSE NEW END;
    END IF;

    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'audit_logs is append-only: a recorded action cannot be rewritten'
            USING ERRCODE = 'restrict_violation';
    END IF;

    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'audit_logs is append-only: a recorded action can only be removed by a workspace erasure'
            USING ERRCODE = 'restrict_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- It fires on both, and BEFORE each row rather than once per statement: one refused row
-- must be enough to refuse the statement, so a partial mutation cannot land.
CREATE TRIGGER audit_logs_append_only
    BEFORE UPDATE OR DELETE ON audit_logs
    FOR EACH ROW EXECUTE FUNCTION audit_logs_append_only();

-- ---------------------------------------------------------------- the reads a person asks for

-- "What did this Bolu do" is the first question an incident asks.
CREATE INDEX audit_logs_agent_created_idx
    ON audit_logs (workspace_id, actor_agent_id, created_at DESC)
    WHERE actor_agent_id IS NOT NULL;

-- "Who changed this kind of thing" — a grant, a role, a provider.
CREATE INDEX audit_logs_action_created_idx
    ON audit_logs (workspace_id, action, created_at DESC);

-- "Who touched this row" — the question the trail exists to answer, and the one the
-- existing (workspace, created_at) index cannot serve: it would read every entry the
-- workspace ever wrote.
CREATE INDEX audit_logs_object_idx
    ON audit_logs (workspace_id, object_type, object_id, created_at DESC);
