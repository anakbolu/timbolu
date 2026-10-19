-- Bootstrap schema for Bolu: multi-tenant workspaces, Bolu (agents), chat with
-- block content, the durable task runtime, approvals, routines, memory and
-- billing. Every tenant table carries workspace_id and is protected by Row
-- Level Security, so a missing filter in application code cannot leak rows
-- across workspaces.
--
-- Naming note: the architecture document calls the account table `user`, but
-- `user` is a reserved keyword in PostgreSQL, so the table is named `users`.
--
-- Extensions: pgvector for memory/knowledge embeddings, pgcrypto for
-- gen_random_uuid() on PostgreSQL 16.

CREATE EXTENSION IF NOT EXISTS "vector";
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- The active tenant is carried in a transaction-local setting. Unset or empty
-- means "no workspace" and every RLS policy then matches nothing.
CREATE FUNCTION app_current_workspace_id() RETURNS uuid
    LANGUAGE sql STABLE AS $$
    SELECT NULLIF(current_setting('app.workspace_id', true), '')::uuid
$$;

-- ---------------------------------------------------------------- tenant root

CREATE TABLE workspaces (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL,
    business_field TEXT NOT NULL DEFAULT '',
    timezone TEXT NOT NULL DEFAULT 'Asia/Jakarta',
    language TEXT NOT NULL DEFAULT 'id',
    work_hours_start TIME NOT NULL DEFAULT '08:00',
    work_hours_end TIME NOT NULL DEFAULT '21:00',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- accounts. `password_hash` is empty for Google-only accounts; `google_subject`
-- is empty for password accounts.
CREATE TABLE users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email TEXT NOT NULL,
    password_hash TEXT NOT NULL DEFAULT '',
    email_verified_at TIMESTAMPTZ,
    google_subject TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX users_email_lower_key ON users (lower(email));
CREATE UNIQUE INDEX users_google_subject_key ON users (google_subject)
    WHERE google_subject IS NOT NULL;

CREATE TABLE members (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'member')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX members_workspace_user_key ON members (workspace_id, user_id);
CREATE INDEX members_user_idx ON members (user_id);

-- Teams group a workspace's Bolu. A new workspace is created with one of them
-- (Tim Bolu, kind 'bolu'); 'hore' is the kind the second seeded team used to
-- carry and is kept so workspaces created before it was dropped still read.
CREATE TABLE teams (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('bolu', 'hore')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX teams_workspace_name_key ON teams (workspace_id, name);

-- ------------------------------------------------------------------ agent

-- The catalogue copied into a workspace on onboarding. Not tenant data, so no
-- RLS: every workspace reads the same six Bolu templates.
CREATE TABLE agent_templates (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    key TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    role TEXT NOT NULL,
    persona TEXT NOT NULL,
    tone TEXT NOT NULL DEFAULT '',
    shape TEXT NOT NULL DEFAULT 'circle',
    color TEXT NOT NULL DEFAULT '',
    tools JSONB NOT NULL DEFAULT '[]'::jsonb,
    integrations JSONB NOT NULL DEFAULT '[]'::jsonb,
    default_model JSONB NOT NULL DEFAULT '{}'::jsonb,
    sort_order INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Integrations are declared before agents because agent_grants references them.
-- Tokens are stored as envelope-encrypted ciphertext: the per-workspace data
-- key is wrapped by KMS (`data_key_wrapped`), never stored in the clear.
CREATE TABLE integrations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    app TEXT NOT NULL,
    account_label TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'disconnected'
        CHECK (status IN ('connected', 'needs_relogin', 'disconnected')),
    scopes TEXT[] NOT NULL DEFAULT '{}',
    access_token_encrypted BYTEA,
    refresh_token_encrypted BYTEA,
    token_expires_at TIMESTAMPTZ,
    data_key_wrapped BYTEA,
    data_key_kms_key_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX integrations_workspace_app_account_key
    ON integrations (workspace_id, app, account_label);

CREATE TABLE agents (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    team_id UUID NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT '',
    persona TEXT NOT NULL DEFAULT '',
    tone TEXT NOT NULL DEFAULT '',
    shape TEXT NOT NULL DEFAULT 'circle',
    color TEXT NOT NULL DEFAULT '',
    -- The UI status (Bekerja / Menunggu kamu / Santai / Istirahat) is derived
    -- from running tasks and pending drafts; only the rest switch is stored.
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'resting')),
    template_key TEXT NOT NULL DEFAULT '',
    default_model JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX agents_workspace_idx ON agents (workspace_id);
CREATE INDEX agents_team_idx ON agents (workspace_id, team_id);

CREATE TABLE agent_grants (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    agent_id UUID NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    integration_id UUID NOT NULL REFERENCES integrations (id) ON DELETE CASCADE,
    permission TEXT NOT NULL CHECK (permission IN ('read', 'read_write')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX agent_grants_agent_integration_key
    ON agent_grants (agent_id, integration_id);

-- --------------------------------------------------------------- conversation

CREATE TABLE conversations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('direct', 'group')),
    title TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX conversations_workspace_idx ON conversations (workspace_id);

CREATE TABLE conversation_participants (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    conversation_id UUID NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    agent_id UUID REFERENCES agents (id) ON DELETE CASCADE,
    user_id UUID REFERENCES users (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((agent_id IS NULL) <> (user_id IS NULL))
);

CREATE UNIQUE INDEX conversation_participants_agent_key
    ON conversation_participants (conversation_id, agent_id)
    WHERE agent_id IS NOT NULL;
CREATE UNIQUE INDEX conversation_participants_user_key
    ON conversation_participants (conversation_id, user_id)
    WHERE user_id IS NOT NULL;

-- Message bodies are block lists (text, table, chart, mermaid, html, draft) so
-- each block type gets its own renderer in the frontend.
CREATE TABLE messages (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    conversation_id UUID NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    author_agent_id UUID REFERENCES agents (id) ON DELETE SET NULL,
    author_user_id UUID REFERENCES users (id) ON DELETE SET NULL,
    blocks JSONB NOT NULL DEFAULT '[]'::jsonb,
    attachments JSONB NOT NULL DEFAULT '[]'::jsonb,
    task_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((author_agent_id IS NULL) <> (author_user_id IS NULL))
);

-- Cursor pagination reads newest-first, so the index matches that order.
CREATE INDEX messages_conversation_created_idx
    ON messages (conversation_id, created_at DESC, id DESC);

-- -------------------------------------------------------------- task runtime

CREATE TABLE tasks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    agent_id UUID NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    parent_task_id UUID REFERENCES tasks (id) ON DELETE SET NULL,
    conversation_id UUID REFERENCES conversations (id) ON DELETE SET NULL,
    trigger TEXT NOT NULL CHECK (trigger IN ('chat', 'routine', 'webhook', 'handoff')),
    title TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'running', 'waiting_approval', 'succeeded', 'failed', 'canceled')),
    workflow_id TEXT NOT NULL DEFAULT '',
    temporal_run_id TEXT NOT NULL DEFAULT '',
    stopped_reason TEXT NOT NULL DEFAULT '',
    -- `summary` survives the raw task_step retention window.
    summary TEXT NOT NULL DEFAULT '',
    step_count INT NOT NULL DEFAULT 0,
    input_tokens BIGINT NOT NULL DEFAULT 0,
    output_tokens BIGINT NOT NULL DEFAULT 0,
    cost_micros BIGINT NOT NULL DEFAULT 0,
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX tasks_workspace_created_idx ON tasks (workspace_id, created_at DESC);
CREATE INDEX tasks_agent_status_idx ON tasks (workspace_id, agent_id, status);

ALTER TABLE messages
    ADD CONSTRAINT messages_task_fk FOREIGN KEY (task_id) REFERENCES tasks (id) ON DELETE SET NULL;

CREATE TABLE task_steps (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    task_id UUID NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    seq INT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('think', 'tool_call', 'tool_result', 'draft', 'final')),
    tool_name TEXT NOT NULL DEFAULT '',
    tool_label TEXT NOT NULL DEFAULT ''
        CHECK (tool_label IN ('', 'read', 'write_internal', 'write_external')),
    input JSONB NOT NULL DEFAULT '{}'::jsonb,
    output JSONB NOT NULL DEFAULT '{}'::jsonb,
    input_tokens INT NOT NULL DEFAULT 0,
    output_tokens INT NOT NULL DEFAULT 0,
    -- Set by the retention job once the raw payload is pruned (EPIC 13, #105).
    pruned_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX task_steps_task_seq_key ON task_steps (task_id, seq);
CREATE INDEX task_steps_workspace_idx ON task_steps (workspace_id);

-- ---------------------------------------------------------------- approvals

CREATE TABLE approval_policies (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    action_kind TEXT NOT NULL,
    mode TEXT NOT NULL DEFAULT 'always_draft' CHECK (mode IN ('always_draft', 'auto')),
    max_amount BIGINT,
    updated_by UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX approval_policies_workspace_action_key
    ON approval_policies (workspace_id, action_kind);

CREATE TABLE drafts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    task_id UUID NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    agent_id UUID NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    action_kind TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'approved', 'revise', 'sent', 'canceled')),
    revision INT NOT NULL DEFAULT 0,
    decided_by UUID REFERENCES users (id) ON DELETE SET NULL,
    decided_at TIMESTAMPTZ,
    decision_note TEXT NOT NULL DEFAULT '',
    sent_at TIMESTAMPTZ,
    -- Guards against a duplicate approve signal sending twice.
    idempotency_key TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX drafts_workspace_idempotency_key
    ON drafts (workspace_id, idempotency_key);
CREATE INDEX drafts_workspace_status_idx ON drafts (workspace_id, status);

-- ---------------------------------------------------------------- routines

CREATE TABLE routines (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    agent_id UUID NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('daily', 'hourly_range', 'weekly', 'trigger')),
    cron TEXT NOT NULL DEFAULT '',
    hour_start INT CHECK (hour_start BETWEEN 0 AND 23),
    hour_end INT CHECK (hour_end BETWEEN 0 AND 23),
    trigger_source TEXT NOT NULL DEFAULT '',
    instruction TEXT NOT NULL DEFAULT '',
    enabled BOOLEAN NOT NULL DEFAULT true,
    -- Temporal Schedule id, kept so pause/resume/delete stay idempotent.
    schedule_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX routines_workspace_agent_idx ON routines (workspace_id, agent_id);

CREATE TABLE routine_runs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    routine_id UUID NOT NULL REFERENCES routines (id) ON DELETE CASCADE,
    task_id UUID REFERENCES tasks (id) ON DELETE SET NULL,
    status TEXT NOT NULL DEFAULT 'running'
        CHECK (status IN ('running', 'succeeded', 'failed', 'skipped')),
    summary TEXT NOT NULL DEFAULT '',
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ
);

CREATE INDEX routine_runs_routine_started_idx ON routine_runs (routine_id, started_at DESC);

-- ------------------------------------------------------- knowledge & memory

-- Embedding dimension is fixed to 1536 (the default small embedding model);
-- changing it requires a new column plus a re-embed job.
CREATE TABLE knowledge_items (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    agent_id UUID REFERENCES agents (id) ON DELETE CASCADE,
    source TEXT NOT NULL DEFAULT '',
    chunk_index INT NOT NULL DEFAULT 0,
    content TEXT NOT NULL,
    embedding vector(1536),
    status TEXT NOT NULL DEFAULT 'ready' CHECK (status IN ('pending', 'ready', 'failed')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX knowledge_items_workspace_idx ON knowledge_items (workspace_id);
CREATE INDEX knowledge_items_embedding_idx ON knowledge_items
    USING hnsw (embedding vector_cosine_ops);

-- Two memory layers: agent_id IS NULL is the workspace-wide main memory, a set
-- agent_id is that Bolu's private memory.
CREATE TABLE memories (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    agent_id UUID REFERENCES agents (id) ON DELETE CASCADE,
    content TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('fact', 'preference', 'instruction')),
    source_message_id UUID REFERENCES messages (id) ON DELETE SET NULL,
    embedding vector(1536),
    pinned BOOLEAN NOT NULL DEFAULT false,
    created_by TEXT NOT NULL DEFAULT 'extraction' CHECK (created_by IN ('user', 'extraction')),
    superseded_by UUID REFERENCES memories (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX memories_layer_idx ON memories (workspace_id, agent_id)
    WHERE superseded_by IS NULL;
CREATE INDEX memories_embedding_idx ON memories USING hnsw (embedding vector_cosine_ops);

-- ------------------------------------------------------ realtime & auditing

-- One append-only stream feeds the activity feed, the dashboard and the office
-- view. `id` doubles as the reconnect cursor (last_event_id).
CREATE TABLE activity_events (
    id BIGSERIAL PRIMARY KEY,
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    type TEXT NOT NULL,
    actor_agent_id UUID REFERENCES agents (id) ON DELETE SET NULL,
    actor_user_id UUID REFERENCES users (id) ON DELETE SET NULL,
    conversation_id UUID REFERENCES conversations (id) ON DELETE CASCADE,
    task_id UUID REFERENCES tasks (id) ON DELETE SET NULL,
    draft_id UUID REFERENCES drafts (id) ON DELETE SET NULL,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX activity_events_workspace_id_idx ON activity_events (workspace_id, id);

CREATE TABLE audit_logs (
    id BIGSERIAL PRIMARY KEY,
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    actor_user_id UUID REFERENCES users (id) ON DELETE SET NULL,
    actor_agent_id UUID REFERENCES agents (id) ON DELETE SET NULL,
    action TEXT NOT NULL,
    object_type TEXT NOT NULL,
    object_id TEXT NOT NULL DEFAULT '',
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX audit_logs_workspace_created_idx ON audit_logs (workspace_id, created_at DESC);

CREATE TABLE usage_ledger (
    id BIGSERIAL PRIMARY KEY,
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    agent_id UUID REFERENCES agents (id) ON DELETE SET NULL,
    task_id UUID REFERENCES tasks (id) ON DELETE SET NULL,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    purpose TEXT NOT NULL DEFAULT 'agent'
        CHECK (purpose IN ('agent', 'routing', 'extraction', 'embedding')),
    input_tokens INT NOT NULL DEFAULT 0,
    output_tokens INT NOT NULL DEFAULT 0,
    cache_read_tokens INT NOT NULL DEFAULT 0,
    cache_write_tokens INT NOT NULL DEFAULT 0,
    cost_micros BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX usage_ledger_workspace_created_idx ON usage_ledger (workspace_id, created_at DESC);

-- ---------------------------------------------------------------- billing

CREATE TABLE plans (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    key TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    monthly_price_idr BIGINT NOT NULL DEFAULT 0,
    token_quota BIGINT NOT NULL DEFAULT 0,
    max_agents INT NOT NULL DEFAULT 6,
    max_integrations INT NOT NULL DEFAULT 1,
    max_members INT NOT NULL DEFAULT 1,
    is_trial BOOLEAN NOT NULL DEFAULT false,
    sort_order INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE subscriptions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    plan_id UUID NOT NULL REFERENCES plans (id),
    status TEXT NOT NULL CHECK (status IN ('trial', 'active', 'past_due', 'canceled')),
    current_period_start TIMESTAMPTZ NOT NULL,
    current_period_end TIMESTAMPTZ NOT NULL,
    grace_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A workspace has at most one live subscription; canceled rows stay for history.
CREATE UNIQUE INDEX subscriptions_live_workspace_key
    ON subscriptions (workspace_id) WHERE status <> 'canceled';

CREATE TABLE billing_invoices (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    subscription_id UUID REFERENCES subscriptions (id) ON DELETE SET NULL,
    provider TEXT NOT NULL DEFAULT '',
    provider_reference TEXT NOT NULL DEFAULT '',
    amount_idr BIGINT NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'paid', 'failed', 'expired')),
    period_start TIMESTAMPTZ NOT NULL,
    period_end TIMESTAMPTZ NOT NULL,
    paid_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX billing_invoices_workspace_created_idx
    ON billing_invoices (workspace_id, created_at DESC);

-- Fast quota check: the LLM gateway reads the Redis counter and this table is
-- the durable record it is reconciled against.
CREATE TABLE quota_periods (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    period_start TIMESTAMPTZ NOT NULL,
    period_end TIMESTAMPTZ NOT NULL,
    tokens_granted BIGINT NOT NULL DEFAULT 0,
    tokens_used BIGINT NOT NULL DEFAULT 0,
    warning_80_sent_at TIMESTAMPTZ,
    warning_100_sent_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX quota_periods_workspace_period_key
    ON quota_periods (workspace_id, period_start);

-- ------------------------------------------------------------------- seeds

INSERT INTO plans (key, name, monthly_price_idr, token_quota, max_agents, max_integrations, max_members, is_trial, sort_order)
VALUES ('coba', 'Paket Coba', 0, 200000, 6, 1, 1, true, 1)
ON CONFLICT (key) DO NOTHING;

INSERT INTO agent_templates (key, name, role, persona, tone, shape, color, tools, integrations, sort_order)
VALUES
    ('oren', 'Oren', 'Penjualan', 'Menangani pesanan dan menagih pelanggan dengan ramah.', 'hangat dan ringkas', 'circle', '#F97316', '["gmail.search","gmail.create_draft","gmail.send"]', '["gmail","whatsapp"]', 1),
    ('biru', 'Biru', 'Operasional', 'Mengurus pengiriman, stok, dan ongkos kirim.', 'tenang dan teliti', 'circle', '#3B82F6', '["gmail.search"]', '["gmail"]', 2),
    ('lila', 'Lila', 'Keuangan', 'Mencatat pemasukan, pengeluaran, dan menyiapkan tagihan.', 'rapi dan hati-hati', 'circle', '#A855F7', '["gmail.search"]', '["gmail"]', 3),
    ('ijo', 'Ijo', 'Data', 'Menyusun rekap dan grafik dari data usaha.', 'analitis dan jelas', 'circle', '#22C55E', '["gmail.search"]', '["gmail","drive"]', 4),
    ('pinky', 'Pinky', 'Pemasaran', 'Menulis konten promosi dan membalas ulasan pelanggan.', 'ceria dan persuasif', 'circle', '#EC4899', '["gmail.search","gmail.create_draft"]', '["gmail"]', 5),
    ('kunyit', 'Kunyit', 'Pelanggan', 'Menjawab pertanyaan pelanggan dan mencatat keluhan.', 'sabar dan membantu', 'circle', '#EAB308', '["gmail.search"]', '["gmail","whatsapp"]', 6)
ON CONFLICT (key) DO NOTHING;

-- ------------------------------------------------------------- row level security

-- FORCE makes the policies apply to the table owner too, so application code
-- connecting as the owner cannot bypass tenant isolation.
DO $$
DECLARE
    tenant_table TEXT;
BEGIN
    FOREACH tenant_table IN ARRAY ARRAY[
        'members', 'teams', 'integrations', 'agents', 'agent_grants',
        'conversations', 'conversation_participants', 'messages',
        'tasks', 'task_steps', 'approval_policies', 'drafts',
        'routines', 'routine_runs', 'knowledge_items', 'memories',
        'activity_events', 'audit_logs', 'usage_ledger',
        'subscriptions', 'billing_invoices', 'quota_periods'
    ]
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', tenant_table);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', tenant_table);
        EXECUTE format(
            'CREATE POLICY %I ON %I USING (workspace_id = app_current_workspace_id()) WITH CHECK (workspace_id = app_current_workspace_id())',
            tenant_table || '_tenant_isolation', tenant_table
        );
    END LOOP;
END
$$;
