-- EPIC 14 · The workspace's own data: a product catalogue, the customers a workspace talks to, and the
-- orders it took.
--
-- This is the table set the MVP flow needs and nothing more. A customer writes an order on WhatsApp,
-- a Bolu records it (product, quantity, price, contact), and the invoice that follows is a message a
-- person approves. Before this migration there was nowhere to put that record: `memories` holds what a
-- Bolu remembers as text, and `knowledge_items` holds documents, and neither is a place a total can be
-- summed from.
--
-- Four decisions are worth reading rather than inferring:
--
--   1. **Prices are stored in micro-rupiah**, the unit `usage_ledger` and `plans` already use, so an
--      invoice total and a cost are the same number of the same thing. A price in rupiah written as
--      `15000` next to a cost written as `15000000` is the kind of pair somebody eventually adds.
--   2. **An order line snapshots the name and the price.** An invoice is a record of what was agreed,
--      so changing a product's price tomorrow must not rewrite what a customer was quoted today. The
--      product is still referenced — which line came from which catalogue entry stays answerable —
--      but the line carries its own copy of the two fields the invoice is read from.
--   3. **A customer is keyed by phone within a workspace.** That is the identity the flow actually has:
--      a WhatsApp conversation is a number, and the number is what a second order from the same person
--      must resolve to. The unique index is partial because a customer with no number yet (recorded by
--      hand) must be allowed.
--   4. **There is no `invoiced` status.** The records of an invoice leaving are the approval draft and
--      the WhatsApp outbox row; a status column the product never transitions would be a column that
--      lies about what happened. `status` carries the two states a person can actually cause.

-- ------------------------------------------------------------------ products

CREATE TABLE products (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    -- sku is the workspace's own code when it has one; it is not unique because a workspace that does
    -- not use codes would then be limited to one product.
    sku TEXT NOT NULL DEFAULT '',
    name TEXT NOT NULL,
    -- unit_price_micros is the price of one unit. Indonesian rupiah has no minor unit in practice, so
    -- the micros exist to agree with the usage ledger rather than to express fractions of a rupiah.
    unit_price_micros BIGINT NOT NULL DEFAULT 0 CHECK (unit_price_micros >= 0),
    -- unit is how the product is sold: "pcs", "kg", "box". It travels onto the line so an invoice
    -- reads "2 kg" rather than "2".
    unit TEXT NOT NULL DEFAULT '',
    -- active hides a product from the catalogue without deleting it, because a product that stopped
    -- being sold must not disappear from the orders that sold it.
    active BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A Bolu looks a product up by the name a customer used, so the name is the lookup key. Case
-- insensitive because "Kopi Arabika" and "kopi arabika" are the same product to everybody except a
-- database.
CREATE UNIQUE INDEX products_workspace_name_key ON products (workspace_id, lower(name));

ALTER TABLE products ENABLE ROW LEVEL SECURITY;
ALTER TABLE products FORCE ROW LEVEL SECURITY;
CREATE POLICY products_tenant_isolation ON products
    USING (workspace_id = app_current_workspace_id())
    WITH CHECK (workspace_id = app_current_workspace_id());

-- ------------------------------------------------------------------ customers

CREATE TABLE customers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    name TEXT NOT NULL DEFAULT '',
    -- phone is the digits of the number without a country code prefix, which is the shape the
    -- WhatsApp adapter's `to` and `chat` arguments are normalized to.
    phone TEXT NOT NULL DEFAULT '',
    email TEXT NOT NULL DEFAULT '',
    note TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX customers_workspace_phone_key
    ON customers (workspace_id, phone)
    WHERE phone <> '';

ALTER TABLE customers ENABLE ROW LEVEL SECURITY;
ALTER TABLE customers FORCE ROW LEVEL SECURITY;
CREATE POLICY customers_tenant_isolation ON customers
    USING (workspace_id = app_current_workspace_id())
    WITH CHECK (workspace_id = app_current_workspace_id());

-- ------------------------------------------------------------------ orders

CREATE TABLE orders (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    -- The customer survives as a snapshot of the phone on the order, so an order whose customer row is
    -- removed stays attributable — which is the same reason the line snapshots its price.
    customer_id UUID REFERENCES customers (id) ON DELETE SET NULL,
    customer_phone TEXT NOT NULL DEFAULT '',
    customer_name TEXT NOT NULL DEFAULT '',
    -- The Bolu and the task that recorded it, so "who took this order" is answerable from the order
    -- rather than only from the audit trail.
    agent_id UUID REFERENCES agents (id) ON DELETE SET NULL,
    task_id UUID REFERENCES tasks (id) ON DELETE SET NULL,
    -- code is what a person quotes: short, unique, and derived from the row's own id so two orders
    -- taken at the same instant cannot collide.
    code TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'recorded' CHECK (status IN ('recorded', 'canceled')),
    total_micros BIGINT NOT NULL DEFAULT 0 CHECK (total_micros >= 0),
    note TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX orders_workspace_code_key ON orders (workspace_id, code);

-- The read the screen and the tool share: a workspace's orders, newest first.
CREATE INDEX orders_workspace_created_idx ON orders (workspace_id, created_at DESC);

-- And the read that answers "what did this customer order", which is the second question of the flow.
CREATE INDEX orders_workspace_phone_idx ON orders (workspace_id, customer_phone, created_at DESC);

ALTER TABLE orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE orders FORCE ROW LEVEL SECURITY;
CREATE POLICY orders_tenant_isolation ON orders
    USING (workspace_id = app_current_workspace_id())
    WITH CHECK (workspace_id = app_current_workspace_id());

-- ------------------------------------------------------------------ order lines

CREATE TABLE order_items (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    order_id UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    -- product_id names the catalogue entry the line came from, when it came from one.
    product_id UUID REFERENCES products (id) ON DELETE SET NULL,
    -- description, unit and unit_price_micros are the line's own copy of what was agreed.
    description TEXT NOT NULL,
    unit TEXT NOT NULL DEFAULT '',
    quantity NUMERIC(12, 3) NOT NULL CHECK (quantity > 0),
    unit_price_micros BIGINT NOT NULL DEFAULT 0 CHECK (unit_price_micros >= 0),
    -- line_total_micros is written by the statement rather than by the caller, so a line's total
    -- cannot disagree with its own quantity and price.
    line_total_micros BIGINT NOT NULL GENERATED ALWAYS AS
        ((round(quantity * unit_price_micros))::bigint) STORED
);

CREATE INDEX order_items_order_idx ON order_items (workspace_id, order_id);

ALTER TABLE order_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE order_items FORCE ROW LEVEL SECURITY;
CREATE POLICY order_items_tenant_isolation ON order_items
    USING (workspace_id = app_current_workspace_id())
    WITH CHECK (workspace_id = app_current_workspace_id());

-- ------------------------------------------------------------------ the tools

-- The two new tools are part of the workspace itself rather than of a connected account, and that is
-- what `needs_connection` says. Every other catalogue row requires a connection plus a grant: a Bolu
-- may only use what somebody connected and handed it. Reading the workspace's own catalogue and
-- recording an order into it needs neither — there is no account, no token, and nothing outside this
-- deployment — so a row that demanded one would be a tool nobody could ever be granted.
ALTER TABLE tool_catalog
    ADD COLUMN needs_connection BOOLEAN NOT NULL DEFAULT true;

COMMENT ON COLUMN tool_catalog.needs_connection IS
    'False for a tool that belongs to the workspace itself and needs no connected account. The tool filter offers such a tool to any Bolu whose profile asks for it, and the hub runs it without a connection.';

INSERT INTO tool_catalog (name, integration_app, label, description, needs_connection) VALUES
    ('order.find_product', 'order', 'read',
     'Cari harga satu produk di katalog workspace. Pakai ini sebelum menyebut harga ke pelanggan.',
     false),
    ('order.record_order', 'order', 'write_internal',
     'Catat pesanan pelanggan (produk, jumlah, kontak) ke data workspace.',
     false),
    ('order.read_orders', 'order', 'read',
     'Baca pesanan terakhir workspace ini, atau pesanan satu pelanggan.',
     false)
ON CONFLICT (name) DO NOTHING;

-- Every workspace that already exists gets the conservative policy for the new catalogue rows it can
-- reach. The provisioner does this for a workspace created from here on; this covers the ones that
-- exist, the same way 000007 did for the tools that existed then.
INSERT INTO approval_policies (workspace_id, action_kind, mode)
SELECT w.id, tc.name, 'always_draft'
FROM workspaces w
CROSS JOIN tool_catalog tc
WHERE tc.label = 'write_external'
ON CONFLICT (workspace_id, action_kind) DO NOTHING;

-- ------------------------------------------------------------------ the template

-- The Bolu whose job this is. `oren` is the sales template, and until now it could search a mailbox
-- and send from one — it had nothing to record an order *into*, so the flow the MVP is built around
-- could not be run by any Bolu the product ships.
--
-- The persona is where the flow is described, and it is data rather than code on purpose: adding a
-- Bolu is adding a profile, and a workspace that wants a different salesperson rewrites this text
-- rather than waiting for a release. Three things are stated in it because each is a guarantee the
-- product makes rather than a style preference: the price always comes from the catalogue (a model
-- that quotes a price it decided is the failure this design exists to prevent), the reply goes out as
-- a message that waits for approval (nothing leaves without a person), and an incomplete or unknown
-- order is asked about rather than guessed at.
--
-- The tools it names are the three of this migration plus WhatsApp's two: it has to read the message
-- that arrived, find the price, record the order, and send the invoice back. `whatsapp.send_message` is
-- `write_external`, so it is the call the approval gate stops — which is exactly the path the flow is
-- supposed to take.
--
-- Existing Bolus are deliberately not changed. A Bolu's tool list is what somebody decided it may use,
-- and adding tools to it from a migration would be granting permissions nobody asked for — a workspace
-- that wants its sales Bolu to take orders edits that Bolu's profile on the Bolu screen. New workspaces
-- are provisioned from this row and get the flow out of the box.
UPDATE agent_templates
SET tools = '["gmail.search","gmail.create_draft","gmail.send","whatsapp.read_messages","whatsapp.send_message","order.find_product","order.record_order","order.read_orders"]'::jsonb,
    persona = 'Menangani pesanan dan menagih pelanggan untuk usaha ini. Alur kerjanya: baca pesan pelanggan, cari harganya di katalog dengan alat order.find_product, catat pesanannya dengan order.record_order, lalu susun tagihan yang memuat kode pesanan, daftar baris, dan total, dan kirim ke pelanggan dengan whatsapp.send_message - pesan itu menunggu persetujuan pemilik usaha dulu sebelum terkirim. Harga selalu diambil dari katalog; jangan pernah menyebut harga yang kamu hitung atau kira sendiri. Kalau produk yang disebut pelanggan tidak ada di katalog, atau ada beberapa kandidat yang mirip, tanyakan dulu produk mana yang dimaksud. Kalau pesanannya belum lengkap - produk atau jumlahnya tidak jelas - tanyakan, jangan mengira-ngira. Nama usaha, sapaan, jam buka, dan aturan usaha diambil dari memori utama workspace, bukan dikarang sendiri; kalau belum ada, tanyakan ke pemilik usaha. Pelanggan yang baru pertama kali menulis diperlakukan sebagai pelanggan baru: sebut namanya setelah dia menyebutkannya, dan jawab singkat.',
    tone = 'hangat dan ringkas, menagih tanpa terkesan menagih'
WHERE key = 'oren';
