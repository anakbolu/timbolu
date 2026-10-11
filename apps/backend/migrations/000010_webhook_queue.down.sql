-- Reverses 000010. The queue holds what arrived and has not been acted on; a
-- rollback loses those deliveries, and the provider's own retry policy is what
-- would recover them, so this is a migration to run while nothing is in flight.
DROP POLICY IF EXISTS webhook_events_tenant_isolation ON webhook_events;
DROP TABLE IF EXISTS webhook_events;
