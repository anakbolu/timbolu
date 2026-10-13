-- The order module's queries: the catalogue a workspace sells from, the customers it talks to, and
-- the orders it took.
--
-- Everything here runs inside the caller's tenant scope, so Row Level Security filters it even where a
-- statement forgets its workspace filter. The one statement that is more than a read is `RecordOrder`,
-- which writes a customer, an order, and its lines in a single transaction.

-- ------------------------------------------------------------------ products

-- name: ListProducts :many
-- The catalogue, newest first. A search matches the name, and inactive products are hidden unless the
-- caller asked for them — what stopped being sold must not clutter the list it is chosen from.
SELECT * FROM products
WHERE workspace_id = $1
  AND (sqlc.arg('include_inactive')::boolean OR active)
  AND (sqlc.arg('search')::text = '' OR lower(name) LIKE '%' || lower(sqlc.arg('search')::text) || '%')
ORDER BY lower(name)
LIMIT sqlc.arg('limit_count');

-- name: GetProduct :one
SELECT * FROM products WHERE id = $1 AND workspace_id = $2;

-- name: CreateProduct :one
INSERT INTO products (workspace_id, sku, name, unit_price_micros, unit)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: UpdateProduct :one
-- One statement for every combination of edited fields: a caller sends the whole change set and the
-- columns it left nil keep their value, which is what makes an edit idempotent under a retry.
UPDATE products
SET sku = COALESCE(sqlc.narg('sku'), sku),
    name = COALESCE(sqlc.narg('name'), name),
    unit_price_micros = COALESCE(sqlc.narg('unit_price_micros'), unit_price_micros),
    unit = COALESCE(sqlc.narg('unit'), unit),
    active = COALESCE(sqlc.narg('active'), active),
    updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
RETURNING *;

-- name: DeleteProduct :execrows
DELETE FROM products WHERE id = $1 AND workspace_id = $2;

-- name: MatchProducts :many
-- The catalogue entries a term could mean, best match first.
--
-- **The match runs both ways, and both ways are needed.** A customer who says less than the catalogue
-- does — "kopi" for "Kopi Arabika" — is matched by the name containing the term. A customer who says
-- more than the catalogue does — "Kopi Arabika Gayo 1 kg" for "Kopi Arabika" — is matched by the term
-- containing the name. With only the first, the second case is answered as "we do not sell that" and
-- the model has nothing to offer, which is exactly the case where a person would say "you mean our
-- Arabika?".
--
-- The ordering is what makes this a suggestion rather than a list: an exact name wins, then a name
-- that starts with the term, then one that contains it, and last one the term contains — so "kopi"
-- finds "Kopi Arabika" before "Sepatu Kopi", and the vaguest overlap sorts last. It is a
-- trigram-free approximation on purpose: an extension is a dependency to maintain for a hint.
SELECT * FROM products
WHERE workspace_id = $1
  AND active
  AND (lower(name) LIKE '%' || lower(sqlc.arg('term')::text) || '%'
       OR lower(sqlc.arg('term')::text) LIKE '%' || lower(name) || '%'
       OR lower(sku) = lower(sqlc.arg('term')::text))
ORDER BY
    CASE
        WHEN lower(name) = lower(sqlc.arg('term')::text) THEN 0
        WHEN lower(name) LIKE lower(sqlc.arg('term')::text) || '%' THEN 1
        WHEN lower(name) LIKE '%' || lower(sqlc.arg('term')::text) || '%' THEN 2
        ELSE 3
    END,
    lower(name)
LIMIT sqlc.arg('limit_count');

-- ------------------------------------------------------------------ customers

-- name: GetCustomerByPhone :one
SELECT c.*,
       (SELECT count(*) FROM orders o
        WHERE o.workspace_id = c.workspace_id AND o.customer_id = c.id)::int AS order_count
FROM customers c
WHERE c.workspace_id = $1 AND c.phone = $2;

-- name: UpsertCustomer :one
-- The customer a phone number belongs to, created when it is new.
--
-- The name is only filled in when it is known, and never overwritten by an empty one: a customer whose
-- push name arrives on a later message must not lose the name somebody typed, and one that arrives
-- without a name must not erase one that is already there.
INSERT INTO customers (workspace_id, phone, name)
VALUES ($1, $2, $3)
ON CONFLICT (workspace_id, phone) WHERE phone <> ''
DO UPDATE SET
    name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE customers.name END,
    updated_at = now()
RETURNING *;

-- ------------------------------------------------------------------ orders

-- name: RecordOrder :one
-- The order header, with the code derived from the row's own id.
--
-- The id is generated here rather than by the caller so the code and the row cannot disagree: both
-- come from the same value, and two orders taken in the same second cannot collide on a counter.
WITH identified AS (
    SELECT gen_random_uuid() AS id
)
INSERT INTO orders (id, workspace_id, customer_id, customer_phone, customer_name, agent_id, task_id, code, note)
SELECT identified.id,
       sqlc.arg('workspace_id'),
       sqlc.narg('customer_id'),
       sqlc.arg('customer_phone'),
       sqlc.arg('customer_name'),
       sqlc.narg('agent_id'),
       sqlc.narg('task_id'),
       'P-' || upper(substr(replace(identified.id::text, '-', ''), 1, 6)),
       sqlc.arg('note')
FROM identified
RETURNING *;

-- name: RecordOrderItem :one
-- One line, with the price the catalogue had at this moment copied onto it.
--
-- The description, the unit, and the unit price are the line's own copy of what was agreed, and the
-- product id beside them says where it came from. An invoice is a record of a deal, so a price edited
-- tomorrow must not rewrite what a customer was quoted today.
INSERT INTO order_items (workspace_id, order_id, product_id, description, unit, quantity, unit_price_micros)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: SetOrderTotal :execrows
-- The total, summed from the lines that were just written rather than from the caller's arithmetic.
UPDATE orders
SET total_micros = (
        SELECT COALESCE(sum(items.line_total_micros), 0) FROM order_items AS items
        WHERE items.order_id = orders.id AND items.workspace_id = orders.workspace_id
    ),
    updated_at = now()
WHERE orders.id = $1 AND orders.workspace_id = $2;

-- name: GetOrder :one
SELECT * FROM orders WHERE id = $1 AND workspace_id = $2;

-- name: ListOrders :many
SELECT * FROM orders
WHERE workspace_id = $1
  AND (sqlc.arg('customer_phone')::text = '' OR customer_phone = sqlc.arg('customer_phone')::text)
ORDER BY created_at DESC
LIMIT sqlc.arg('limit_count');

-- name: ListOrderItems :many
-- One order's lines, in the order they were recorded.
SELECT * FROM order_items
WHERE workspace_id = $1 AND order_id = $2
ORDER BY id;

-- name: ListOrderItemsForOrders :many
-- The lines of several orders in one read, which is what a list of orders needs: a query per order
-- would be a query per row of every screen that shows them.
SELECT * FROM order_items
WHERE workspace_id = $1 AND order_id = ANY(sqlc.arg('order_ids')::uuid[])
ORDER BY order_id, id;

-- name: SetOrderStatus :one
UPDATE orders
SET status = sqlc.arg('status'), updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
RETURNING *;
