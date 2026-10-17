-- E8.4 · The queue an inbound webhook lands in.
--
-- A provider that calls a webhook gives up after a few seconds and retries, so
-- the endpoint's only job is to decide whether the request is really from the
-- provider and, if it is, to remember it. Everything slow happens afterwards, in
-- the worker, out of the request's path.
--
-- The unique index is the idempotency: providers retry, and a delivery that
-- arrives twice must become one event. `delivery_id` is the provider's own id for
-- the delivery, so the second insert conflicts rather than duplicating.

CREATE TABLE webhook_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    integration_id UUID REFERENCES integrations (id) ON DELETE CASCADE,
    app TEXT NOT NULL,
    -- The provider's own id for this delivery. It is what makes a retry
    -- harmless.
    delivery_id TEXT NOT NULL,
    -- What happened, as `app.thing`, which is what a routine is started by.
    event TEXT NOT NULL,
    -- The provider's body, kept whole so a consumer reads the field it needs
    -- rather than the subset somebody guessed at when writing this table.
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Null until the worker has turned it into something the product acts on.
    processed_at TIMESTAMPTZ,
    attempts INT NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT ''
);

-- One delivery is one event, whatever the provider's retry policy is.
CREATE UNIQUE INDEX webhook_events_delivery_key
    ON webhook_events (workspace_id, app, delivery_id);

-- The worker's read: the oldest unprocessed first, so nothing starves.
CREATE INDEX webhook_events_pending_idx
    ON webhook_events (received_at)
    WHERE processed_at IS NULL;

ALTER TABLE webhook_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_events FORCE ROW LEVEL SECURITY;
CREATE POLICY webhook_events_tenant_isolation ON webhook_events
    USING (workspace_id = app_current_workspace_id())
    WITH CHECK (workspace_id = app_current_workspace_id());
