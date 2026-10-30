-- Fase 0 · Device registry: the address a push notification is delivered to.
--
-- A device token is Expo's address for one install of the app. It belongs to an account rather than
-- to a workspace: the same phone signs into one account and may switch workspaces, and a token that
-- is registered again must update the row it already has rather than pile up duplicates. The unique
-- index is what makes that upsert one statement.
--
-- `workspace_id` is nullable on purpose. The endpoint requires a tenant, so a registration carries
-- one, but the address is the account's: removing a workspace must not remove the phone from the
-- registry, which is why the foreign key sets the column to null instead of cascading.
--
-- Row Level Security is keyed on the user, not the workspace. Every statement here means "my
-- devices", and hiding somebody else's row is what lets the delete answer 404 rather than 403 — the
-- existence of another account's token is not something this endpoint tells.

CREATE TABLE device_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    workspace_id UUID REFERENCES workspaces (id) ON DELETE SET NULL,
    platform TEXT NOT NULL CHECK (platform IN ('ios', 'android')),
    token TEXT NOT NULL,
    app_version TEXT,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per account and token. A phone re-registers on every launch, and this is the constraint the
-- upsert names: the second registration is an update, not a second address.
CREATE UNIQUE INDEX device_tokens_user_token_key ON device_tokens (user_id, token);

-- The sender's read: every device one account registered. The unique index covers this prefix as
-- well, but the registry is read by account rather than by token, so the access path is named.
CREATE INDEX device_tokens_user_idx ON device_tokens (user_id);

ALTER TABLE device_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE device_tokens FORCE ROW LEVEL SECURITY;
CREATE POLICY device_tokens_owner ON device_tokens
    USING (user_id = app_current_user_id())
    WITH CHECK (user_id = app_current_user_id());
