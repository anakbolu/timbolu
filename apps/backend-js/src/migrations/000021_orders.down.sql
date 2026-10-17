-- The reverse of 000021: the workspace's own data — catalogue, customers, orders — and the three
-- catalogue rows that exposed it as tools.
--
-- The orders go with it, and that is worth stating rather than discovering: unlike a retention pass or
-- an erasure, this is a schema rollback, so anything a workspace recorded through the flow is dropped
-- with the tables. A migration that is rolled back after a release is a migration whose data is gone.

DROP INDEX IF EXISTS order_items_order_idx;
DROP TABLE IF EXISTS order_items;

DROP INDEX IF EXISTS orders_workspace_phone_idx;
DROP INDEX IF EXISTS orders_workspace_created_idx;
DROP INDEX IF EXISTS orders_workspace_code_key;
DROP TABLE IF EXISTS orders;

DROP INDEX IF EXISTS customers_workspace_phone_key;
DROP TABLE IF EXISTS customers;

DROP INDEX IF EXISTS products_workspace_name_key;
DROP TABLE IF EXISTS products;

-- The policies go with their tables; the catalogue rows and the column are this migration's own.
DELETE FROM approval_policies
WHERE action_kind IN ('order.find_product', 'order.record_order', 'order.read_orders');

DELETE FROM tool_catalog
WHERE name IN ('order.find_product', 'order.record_order', 'order.read_orders');

ALTER TABLE tool_catalog DROP COLUMN IF EXISTS needs_connection;

-- The template goes back to what it was, with the values written out rather than read from anywhere:
-- a rollback has to land on the exact state the forward migration replaced, and `oren`'s persona and
-- tone are outside this migration's subject.
UPDATE agent_templates
SET tools = '["gmail.search","gmail.create_draft","gmail.send"]'::jsonb,
    persona = 'Menangani pesanan dan menagih pelanggan dengan ramah.',
    tone = 'hangat dan ringkas'
WHERE key = 'oren';
