-- The reverse of 000020: the erasure evidence and its two indexes.
--
-- What is *not* reversed is the erasure itself, and that is worth stating: the workspaces this table
-- recorded are gone, along with their rows and their objects. Rolling this back removes the only
-- record that they were removed.

DROP INDEX IF EXISTS data_erasures_workspace_idx;
DROP INDEX IF EXISTS data_erasures_requested_idx;
DROP TABLE IF EXISTS data_erasures;
