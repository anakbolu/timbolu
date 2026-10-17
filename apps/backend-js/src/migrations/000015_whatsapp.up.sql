-- EPIC 9 · What a WhatsApp gateway needs that the integration hub does not
-- already have.
--
-- The hub owns connections and their sealed tokens; it knows nothing about a
-- number that stays online, the messages it carries, or the queue that paces
-- them. Four new tables carry that, and each is separate from `integrations`
-- on purpose: the session material in particular must never sit beside a row a
-- screen reads.

-- ---------------------------------------------------------------- numbers

-- One linked number: who it belongs to, which Bolu answers it, and how it is
-- doing.
CREATE TABLE whatsapp_numbers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    -- The connection the hub shows and the approval executor picks, which is why
    -- linking a number creates an `integrations` row alongside this one.
    --
    -- Nullable, because a number exists before it is linked: preparing one records
    -- which number is about to be linked and who answers it, and that is what the
    -- warning is accepted against and what the pairing lands on. Demanding a
    -- connection here would mean the row could only be written after the thing it
    -- was written for.
    integration_id UUID REFERENCES integrations (id) ON DELETE SET NULL,
    -- E.164 as WhatsApp reports it, e.g. 628123456789.
    phone TEXT NOT NULL,
    label TEXT NOT NULL DEFAULT '',
    -- Which Bolu answers this number. Somebody has to decide, and the link flow
    -- is where that decision belongs: without it an inbound message has no
    -- reader, and inventing a rule ("the first Bolu") would hide the choice.
    agent_id UUID REFERENCES agents (id) ON DELETE SET NULL,
    -- The five states a number can be in, and `unlinked` is one of them: it is
    -- where a row starts, before anybody has scanned anything. Leaving it out of
    -- the constraint would make the column's own default illegal, and the very
    -- first number a workspace prepares would be refused.
    status TEXT NOT NULL DEFAULT 'unlinked'
        CHECK (status IN ('unlinked', 'linking', 'connected', 'needs_relink', 'disconnected')),
    -- Why it is in that state, in the user's words.
    status_reason TEXT NOT NULL DEFAULT '',
    -- When this workspace accepted the terms-of-service warning. Null means the
    -- warning was never shown, which is what stops a link from completing.
    tos_accepted_at TIMESTAMPTZ,
    -- How many messages this number may send in one hour. Zero means the
    -- deployment's own ceiling rather than unlimited: an unbounded number is a
    -- blast, and the product does not have one.
    hourly_limit INT NOT NULL DEFAULT 40
        CONSTRAINT whatsapp_numbers_hourly_limit_valid CHECK (hourly_limit BETWEEN 0 AND 500),
    connected_at TIMESTAMPTZ,
    -- The last time the gateway saw this number online, which is what tells a
    -- person whether the connection is actually alive.
    last_seen_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One number is one connection. A second link of the same number is a new
-- session for the same row, never a second row firing messages twice.
--
-- Partial, because a number that is not linked yet has no connection: a plain
-- unique index would allow that anyway (Postgres does not compare nulls), and
-- saying so here is what keeps the intent readable.
CREATE UNIQUE INDEX whatsapp_numbers_integration_key
    ON whatsapp_numbers (integration_id)
    WHERE integration_id IS NOT NULL;

-- One number is one workspace. The same phone cannot be linked twice from two
-- tenants, because the WhatsApp session is the number's, not the tenant's.
CREATE UNIQUE INDEX whatsapp_numbers_phone_key ON whatsapp_numbers (phone);

-- ---------------------------------------------------------------- sessions

-- The whatsmeow device state: the noise key, the identity key, the signed
-- pre-key, the sessions, and the app-state keys that let this deployment speak
-- as the number.
--
-- It is sealed with the workspace's own data key, and the workspace id is the
-- associated data of that seal. Two things follow, and both are the point: a
-- blob cannot be opened under another workspace's key, and it cannot be replayed
-- against another workspace even if the row is copied.
CREATE TABLE whatsapp_sessions (
    number_id UUID PRIMARY KEY REFERENCES whatsapp_numbers (id) ON DELETE CASCADE,
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    sealed_device BYTEA NOT NULL,
    -- The JID the device belongs to, in the clear: it is what the gateway
    -- matches against the number it was told to serve, and it is public.
    jid TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------- inbound

-- One inbound message, and the record that makes a redelivery harmless.
CREATE TABLE whatsapp_messages (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    number_id UUID NOT NULL REFERENCES whatsapp_numbers (id) ON DELETE CASCADE,
    -- WhatsApp's own id for this message. It is the dedupe key, and it is what
    -- makes the receive path idempotent rather than merely careful.
    wa_message_id TEXT NOT NULL,
    chat_jid TEXT NOT NULL,
    sender_jid TEXT NOT NULL DEFAULT '',
    sender_name TEXT NOT NULL DEFAULT '',
    is_group BOOLEAN NOT NULL DEFAULT false,
    is_from_me BOOLEAN NOT NULL DEFAULT false,
    kind TEXT NOT NULL DEFAULT 'text'
        CHECK (kind IN ('text', 'image', 'video', 'audio', 'document', 'sticker', 'other')),
    text TEXT NOT NULL DEFAULT '',
    -- Media is fetched after the message is recorded, so a 40 MB video never
    -- holds up the receive path. `media_status` is what a screen reads instead of
    -- waiting on the download.
    media_key TEXT NOT NULL DEFAULT '',
    media_kind TEXT NOT NULL DEFAULT '',
    media_size BIGINT NOT NULL DEFAULT 0,
    media_status TEXT NOT NULL DEFAULT ''
        CHECK (media_status IN ('', 'pending', 'ready', 'failed')),
    media_error TEXT NOT NULL DEFAULT '',
    received_at TIMESTAMPTZ NOT NULL,
    -- The task the message opened, which is how the history reads back to what
    -- the Bolu did with it.
    task_id UUID REFERENCES tasks (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One delivery is one message, whatever WhatsApp's retry policy is.
CREATE UNIQUE INDEX whatsapp_messages_dedupe_key
    ON whatsapp_messages (workspace_id, wa_message_id);

-- The media worker's read: the oldest undownloaded first, so nothing starves.
CREATE INDEX whatsapp_messages_media_pending_idx
    ON whatsapp_messages (received_at)
    WHERE media_status = 'pending';

CREATE INDEX whatsapp_messages_workspace_received_idx
    ON whatsapp_messages (workspace_id, received_at DESC);

-- ---------------------------------------------------------------- outbound

-- One message waiting to leave. The queue is in Postgres rather than in the
-- gateway's memory because a send that is lost when a process is replaced is a
-- customer who never got an answer.
CREATE TABLE whatsapp_outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    number_id UUID NOT NULL REFERENCES whatsapp_numbers (id) ON DELETE CASCADE,
    to_jid TEXT NOT NULL,
    kind TEXT NOT NULL DEFAULT 'text'
        CHECK (kind IN ('text', 'image', 'document')),
    text TEXT NOT NULL DEFAULT '',
    media_key TEXT NOT NULL DEFAULT '',
    -- The draft this message came from, when it came from one. It is what makes
    -- "who allowed this to be sent" answerable from the message itself.
    draft_id UUID,
    status TEXT NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'sending', 'sent', 'failed', 'held')),
    attempts INT NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    -- Nothing leaves before this. It is the random gap between two messages of
    -- one number, so a batch reads as a person writing rather than as a blast.
    not_before TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_at TIMESTAMPTZ,
    wa_message_id TEXT NOT NULL DEFAULT '',
    sent_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The drain's read: the messages of one number that are due, oldest first.
CREATE INDEX whatsapp_outbox_ready_idx
    ON whatsapp_outbox (number_id, not_before)
    WHERE status = 'queued';

-- The hourly ceiling is counted from this, so the count is of what actually
-- left rather than of an incrementing number that can drift.
CREATE INDEX whatsapp_outbox_sent_idx
    ON whatsapp_outbox (number_id, sent_at)
    WHERE status = 'sent';

-- ---------------------------------------------------------------- directories

-- Which tenants the wa-gateway fleet serves.
--
-- This is the one table here without row level security, and the reason is
-- structural rather than convenient: the gateway is not acting *for* a tenant,
-- it is a fleet worker that has to discover which tenants have a number before
-- it can open any workspace's scope at all. A policy would make that discovery
-- impossible and the number would simply never come online.
--
-- What makes it safe to leave open is what it holds and how the rest is read:
--
--   * it carries a routing pair and nothing else — no session material, no
--     message, no credential, no phone number;
--   * every row is written by the API inside a tenant scope, so a deployment's
--     own code is the only writer;
--   * the session blob it leads to is sealed with its workspace's own key and
--     bound to that workspace id as associated data, so a directory row that
--     named the wrong tenant would produce an unopenable blob rather than
--     another tenant's session. The directory is a hint the fleet follows, not
--     an authority it trusts.
CREATE TABLE whatsapp_directory (
    number_id UUID PRIMARY KEY REFERENCES whatsapp_numbers (id) ON DELETE CASCADE,
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    -- Off while a number is unlinked or deliberately parked, so a fleet that is
    -- being drained stops claiming it.
    enabled BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ------------------------------------------------------------------ policies

ALTER TABLE whatsapp_numbers ENABLE ROW LEVEL SECURITY;
ALTER TABLE whatsapp_numbers FORCE ROW LEVEL SECURITY;
CREATE POLICY whatsapp_numbers_tenant_isolation ON whatsapp_numbers
    USING (workspace_id = app_current_workspace_id())
    WITH CHECK (workspace_id = app_current_workspace_id());

ALTER TABLE whatsapp_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE whatsapp_sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY whatsapp_sessions_tenant_isolation ON whatsapp_sessions
    USING (workspace_id = app_current_workspace_id())
    WITH CHECK (workspace_id = app_current_workspace_id());

ALTER TABLE whatsapp_messages ENABLE ROW LEVEL SECURITY;
ALTER TABLE whatsapp_messages FORCE ROW LEVEL SECURITY;
CREATE POLICY whatsapp_messages_tenant_isolation ON whatsapp_messages
    USING (workspace_id = app_current_workspace_id())
    WITH CHECK (workspace_id = app_current_workspace_id());

ALTER TABLE whatsapp_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE whatsapp_outbox FORCE ROW LEVEL SECURITY;
CREATE POLICY whatsapp_outbox_tenant_isolation ON whatsapp_outbox
    USING (workspace_id = app_current_workspace_id())
    WITH CHECK (workspace_id = app_current_workspace_id());
