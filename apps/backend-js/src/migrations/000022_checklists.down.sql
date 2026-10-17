-- The reverse of 000022: the plan a Bolu keeps for itself, and the five tools that exposed it.
--
-- The lists go with the tables. Like every schema rollback that is worth saying out loud: a plan a
-- Bolu is in the middle of is dropped with them, and rolling back after a release loses it. Nothing
-- else in the schema references these rows, so nothing else has to be repaired.

DROP TRIGGER IF EXISTS checklist_items_guard_trigger ON checklist_items;
DROP FUNCTION IF EXISTS checklist_items_guard();

DROP INDEX IF EXISTS checklist_items_list_idx;
DROP TABLE IF EXISTS checklist_items;

DROP INDEX IF EXISTS checklists_workspace_created_idx;
DROP INDEX IF EXISTS checklists_agent_active_key;
DROP TABLE IF EXISTS checklists;

DELETE FROM tool_catalog
WHERE name IN ('checklist.create', 'checklist.check', 'checklist.remove', 'checklist.clear', 'checklist.read');

-- The Bolus and the templates go back to the tool lists they had, with the five names taken out
-- wherever they appear. Written as a filter rather than as a list of the values that were there,
-- because the forward migration appended to whatever each row already had.
UPDATE agents
SET tools = COALESCE((
        SELECT jsonb_agg(entry)
        FROM jsonb_array_elements_text(tools) AS entry
        WHERE entry NOT LIKE 'checklist.%'
    ), '[]'::jsonb);

UPDATE agent_templates
SET tools = COALESCE((
        SELECT jsonb_agg(entry)
        FROM jsonb_array_elements_text(tools) AS entry
        WHERE entry NOT LIKE 'checklist.%'
    ), '[]'::jsonb);
