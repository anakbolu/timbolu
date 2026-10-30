-- The reverse of 000028: the device registry goes, with its indexes and its policy.
--
-- A rollback loses every registered address, which is worth stating rather than discovering. The
-- push sender that will read this table does not exist yet, so nothing else in the schema references
-- these rows and nothing else has to be repaired.

DROP INDEX IF EXISTS device_tokens_user_idx;
DROP INDEX IF EXISTS device_tokens_user_token_key;
DROP TABLE IF EXISTS device_tokens;
