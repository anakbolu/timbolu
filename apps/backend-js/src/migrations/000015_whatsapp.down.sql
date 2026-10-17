-- The reverse of 000015: the four WhatsApp tables and their policies.
--
-- Nothing is restored and nothing is hidden: the session blobs are gone for
-- good, because a device that was unlinked cannot be re-derived from a number.
-- A deployment that rolls this back has to link its numbers again, which is why
-- the down file says so rather than pretending the state survives.

DROP TABLE IF EXISTS whatsapp_directory;
DROP TABLE IF EXISTS whatsapp_outbox;
DROP TABLE IF EXISTS whatsapp_messages;
DROP TABLE IF EXISTS whatsapp_sessions;
DROP TABLE IF EXISTS whatsapp_numbers;
