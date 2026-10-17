/**
 * The order module's data access.
 *
 * Every method runs inside the caller's tenant scope, so Row Level Security
 * filters the statements even where one forgets its workspace filter. The
 * interesting method here is `record`, which is one transaction for what the
 * flow needs to be atomic: a customer, an order, its lines, and the total that
 * is summed from them.
 *
 * Ported 1:1 from internal/modules/order/repository/postgres.go, with
 * repository/sqlcgen/*.go folded in: each query keeps its name as a comment and
 * its SQL text unchanged.
 */

import type { Database, Tx } from "../../../platform/database/index.ts";
import {
  DefaultLimit,
  ErrDuplicateProduct,
  ErrInvalidInput,
  ErrNotFound,
  UnknownProductError,
  errorMessage,
  isZeroScope,
  normaliseOrderFilter,
  normaliseProductFilter,
  wrap,
  type NewOrder,
  type NewProduct,
  type Order,
  type OrderFilter,
  type Product,
  type ProductChange,
  type ProductFilter,
  type Repository,
  type Scope,
} from "../domain/order.ts";

/** One row of `products`. */
interface ProductRow {
  id: string;
  workspace_id: string;
  sku: string;
  name: string;
  unit_price_micros: number;
  unit: string;
  active: boolean;
  created_at: Date;
  updated_at: Date;
}

/** One row of `customers`. */
interface CustomerRow {
  id: string;
  workspace_id: string;
  name: string;
  phone: string;
  email: string;
  note: string;
  created_at: Date;
  updated_at: Date;
}

/** One row of `orders`. */
interface OrderRow {
  id: string;
  workspace_id: string;
  customer_id: string | null;
  customer_phone: string;
  customer_name: string;
  agent_id: string | null;
  task_id: string | null;
  code: string;
  status: string;
  total_micros: number;
  note: string;
  created_at: Date;
  updated_at: Date;
}

/** One row of `order_items`. */
interface OrderItemRow {
  id: string;
  workspace_id: string;
  order_id: string;
  product_id: string | null;
  description: string;
  unit: string;
  /** NUMERIC arrives as a string from postgres.js. */
  quantity: string;
  unit_price_micros: number;
  line_total_micros: number;
}

/** The Postgres-backed implementation of the module's repository. */
export class PostgresRepository implements Repository {
  constructor(private readonly db: Database) {}

  // ---------------------------------------------------------------- the catalogue

  /** Returns the workspace's catalogue. */
  async products(scope: Scope, filter: ProductFilter): Promise<Product[]> {
    const bounded = normaliseProductFilter(filter);

    return this.read(scope, async (tx) => {
      // name: ListProducts
      const rows = await tx<ProductRow[]>`
        SELECT * FROM products
        WHERE workspace_id = ${scope.workspaceId}
          AND (${bounded.includeInactive}::boolean OR active)
          AND (${bounded.search.trim()}::text = '' OR lower(name) LIKE '%' || lower(${bounded.search.trim()}::text) || '%')
        ORDER BY lower(name)
        LIMIT ${bounded.limit}
      `;

      return rows.map(toProduct);
    });
  }

  /** Returns one catalogue entry. */
  async product(scope: Scope, id: string): Promise<Product> {
    return this.read(scope, async (tx) => {
      // name: GetProduct
      const rows = await tx<ProductRow[]>`
        SELECT * FROM products WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
      `;
      const row = rows[0];
      if (!row) {
        throw wrap(ErrNotFound, `product ${id}`);
      }

      return toProduct(row);
    });
  }

  /** Adds a catalogue entry. */
  async createProduct(scope: Scope, product: NewProduct): Promise<Product> {
    return this.write(scope, async (tx) => {
      // name: CreateProduct
      const rows = await tx<ProductRow[]>`
        INSERT INTO products (workspace_id, sku, name, unit_price_micros, unit)
        VALUES (${scope.workspaceId}, ${product.sku.trim()}, ${product.name.trim()}, ${product.unitPriceMicros}, ${product.unit.trim()})
        RETURNING *
      `.catch((err: unknown) => {
        throw translateWrite(err, "create product");
      });

      const row = rows[0];
      if (!row) {
        throw new Error("order: create product returned no row");
      }

      return toProduct(row);
    });
  }

  /** Edits a catalogue entry. */
  async updateProduct(scope: Scope, id: string, change: ProductChange): Promise<Product> {
    return this.write(scope, async (tx) => {
      // name: UpdateProduct
      // One statement for every combination of edited fields: a caller sends the
      // whole change set and the columns it left absent keep their value, which
      // is what makes an edit idempotent under a retry.
      const rows = await tx<ProductRow[]>`
        UPDATE products
        SET sku = COALESCE(${change.sku === undefined ? null : change.sku.trim()}, sku),
            name = COALESCE(${change.name === undefined ? null : change.name.trim()}, name),
            unit_price_micros = COALESCE(${change.unitPriceMicros ?? null}, unit_price_micros),
            unit = COALESCE(${change.unit === undefined ? null : change.unit.trim()}, unit),
            active = COALESCE(${change.active ?? null}, active),
            updated_at = now()
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
        RETURNING *
      `.catch((err: unknown) => {
        throw translateWrite(err, "update product");
      });

      const row = rows[0];
      if (!row) {
        throw wrap(ErrNotFound, `product ${id}`);
      }

      return toProduct(row);
    });
  }

  /**
   * Removes a catalogue entry.
   *
   * The orders that sold it keep their lines, because a line carries its own
   * copy of what was agreed — and the line's product reference becomes null
   * rather than taking the order with it.
   */
  async deleteProduct(scope: Scope, id: string): Promise<void> {
    return this.write(scope, async (tx) => {
      // name: DeleteProduct
      const removed = await tx`
        DELETE FROM products WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
      `.catch((err: unknown) => {
        throw translateWrite(err, "delete product");
      });

      if (removed.count === 0) {
        throw wrap(ErrNotFound, `product ${id}`);
      }
    });
  }

  /** Finds the catalogue entries a term could mean. */
  async matchProducts(scope: Scope, term: string, limit: number): Promise<Product[]> {
    const trimmed = term.trim();
    if (trimmed === "") {
      return [];
    }
    if (limit <= 0 || limit > DefaultLimit) {
      limit = 5;
    }

    return this.read(scope, async (tx) => {
      // name: MatchProducts
      const rows = await tx<ProductRow[]>`
        SELECT * FROM products
        WHERE workspace_id = ${scope.workspaceId}
          AND active
          AND (lower(name) LIKE '%' || lower(${trimmed}::text) || '%'
               OR lower(${trimmed}::text) LIKE '%' || lower(name) || '%'
               OR lower(sku) = lower(${trimmed}::text))
        ORDER BY
            CASE
                WHEN lower(name) = lower(${trimmed}::text) THEN 0
                WHEN lower(name) LIKE lower(${trimmed}::text) || '%' THEN 1
                WHEN lower(name) LIKE '%' || lower(${trimmed}::text) || '%' THEN 2
                ELSE 3
            END,
            lower(name)
        LIMIT ${limit}
      `;

      return rows.map(toProduct);
    });
  }

  // ---------------------------------------------------------------- the flow

  /**
   * Writes an order, its lines, and its customer, in one transaction.
   *
   * **The price is resolved inside this transaction, from the catalogue** rather
   * than being passed in: a model names a product and a quantity, and the price
   * is whatever the workspace says it is right now. That is what makes an
   * invented price impossible — there is no field to put it in — and it is also
   * why an unknown product refuses the whole order instead of writing a line
   * with a guessed price.
   */
  async record(scope: Scope, order: NewOrder): Promise<Order> {
    return this.write(scope, async (tx) => {
      const phone = normalizePhone(order.customerPhone);

      // name: UpsertCustomer
      const customers = await tx<CustomerRow[]>`
        INSERT INTO customers (workspace_id, phone, name)
        VALUES (${scope.workspaceId}, ${phone}, ${order.customerName.trim()})
        ON CONFLICT (workspace_id, phone) WHERE phone <> ''
        DO UPDATE SET
            name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE customers.name END,
            updated_at = now()
        RETURNING *
      `.catch((err: unknown) => {
        throw translateWrite(err, "upsert customer");
      });
      const customer = customers[0];
      if (!customer) {
        throw new Error("order: upsert customer returned no row");
      }

      // name: RecordOrder
      // The id is generated here rather than by the caller so the code and the
      // row cannot disagree: both come from the same value.
      const headers = await tx<OrderRow[]>`
        WITH identified AS (
            SELECT gen_random_uuid() AS id
        )
        INSERT INTO orders (id, workspace_id, customer_id, customer_phone, customer_name, agent_id, task_id, code, note)
        SELECT identified.id,
               ${scope.workspaceId},
               ${optionalId(customer.id)},
               ${customer.phone},
               ${customer.name},
               ${optionalId(order.agentId)},
               ${optionalId(order.taskId)},
               'P-' || upper(substr(replace(identified.id::text, '-', ''), 1, 6)),
               ${order.note.trim()}
        FROM identified
        RETURNING *
      `.catch((err: unknown) => {
        throw translateWrite(err, "record order");
      });
      const header = headers[0];
      if (!header) {
        throw new Error("order: record order returned no row");
      }

      for (const item of order.items) {
        const product = await resolveProduct(tx, scope.workspaceId, item.product);

        const description = item.description.trim() === "" ? product.name : item.description.trim();

        // name: RecordOrderItem
        await tx`
          INSERT INTO order_items (workspace_id, order_id, product_id, description, unit, quantity, unit_price_micros)
          VALUES (${scope.workspaceId}, ${header.id}, ${optionalId(product.id)}, ${description}, ${product.unit}, ${formatNumeric(item.quantity)}, ${product.unitPriceMicros})
        `.catch((err: unknown) => {
          throw translateWrite(err, "record order item");
        });
      }

      // name: SetOrderTotal
      // The total is summed from the rows that were just written rather than
      // added up here, so it cannot disagree with its own lines.
      await tx`
        UPDATE orders
        SET total_micros = (
                SELECT COALESCE(sum(items.line_total_micros), 0) FROM order_items AS items
                WHERE items.order_id = orders.id AND items.workspace_id = orders.workspace_id
            ),
            updated_at = now()
        WHERE orders.id = ${header.id} AND orders.workspace_id = ${scope.workspaceId}
      `.catch((err: unknown) => {
        throw translateWrite(err, "set order total");
      });

      return loadOrder(tx, scope.workspaceId, header.id);
    });
  }

  /** Returns the workspace's orders, newest first. */
  async orders(scope: Scope, filter: OrderFilter): Promise<Order[]> {
    const bounded = normaliseOrderFilter(filter);

    return this.read(scope, async (tx) => {
      // name: ListOrders
      const rows = await tx<OrderRow[]>`
        SELECT * FROM orders
        WHERE workspace_id = ${scope.workspaceId}
          AND (${normalizePhone(bounded.customerPhone)}::text = '' OR customer_phone = ${normalizePhone(bounded.customerPhone)}::text)
        ORDER BY created_at DESC
        LIMIT ${bounded.limit}
      `;

      return withLines(tx, scope.workspaceId, rows);
    });
  }

  /** Returns one order with its lines. */
  async order(scope: Scope, id: string): Promise<Order> {
    return this.read(scope, (tx) => loadOrder(tx, scope.workspaceId, id));
  }

  /** Moves an order to another status. */
  async setOrderStatus(scope: Scope, id: string, status: string): Promise<Order> {
    return this.write(scope, async (tx) => {
      // name: SetOrderStatus
      const rows = await tx<OrderRow[]>`
        UPDATE orders
        SET status = ${status}, updated_at = now()
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
        RETURNING *
      `.catch((err: unknown) => {
        throw translateWrite(err, "set order status");
      });

      if (!rows[0]) {
        throw wrap(ErrNotFound, `order ${id}`);
      }

      return loadOrder(tx, scope.workspaceId, id);
    });
  }

  // ---------------------------------------------------------------- helpers

  /** Runs one query in the caller's scope, read-only. */
  private read<T>(scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    this.requireScope(scope);
    return this.db.inScopeRead(scope, fn);
  }

  /** Runs one query in the caller's scope. */
  private write<T>(scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    this.requireScope(scope);
    return this.db.inScope(scope, fn);
  }

  private requireScope(scope: Scope): void {
    if (isZeroScope(scope)) {
      throw wrap(ErrInvalidInput, "workspace is required");
    }
  }
}

// ---------------------------------------------------------------- queries in a transaction

/**
 * Finds the catalogue entry one line names.
 *
 * An exact name wins, and everything else is a suggestion: a request that named
 * "kopi" when the catalogue has "Kopi Arabika" is refused with the candidates
 * rather than priced by a guess, because the guess would be an invoice for the
 * wrong product.
 */
async function resolveProduct(tx: Tx, workspaceId: string, name: string): Promise<Product> {
  const trimmed = name.trim();
  if (trimmed === "") {
    throw wrap(ErrInvalidInput, "an order line names no product");
  }

  // name: MatchProducts
  const rows = await tx<ProductRow[]>`
    SELECT * FROM products
    WHERE workspace_id = ${workspaceId}
      AND active
      AND (lower(name) LIKE '%' || lower(${trimmed}::text) || '%'
           OR lower(${trimmed}::text) LIKE '%' || lower(name) || '%'
           OR lower(sku) = lower(${trimmed}::text))
    ORDER BY
        CASE
            WHEN lower(name) = lower(${trimmed}::text) THEN 0
            WHEN lower(name) LIKE lower(${trimmed}::text) || '%' THEN 1
            WHEN lower(name) LIKE '%' || lower(${trimmed}::text) || '%' THEN 2
            ELSE 3
        END,
        lower(name)
    LIMIT 5
  `;

  for (const candidate of rows) {
    if (
      equalFold(candidate.name, trimmed) ||
      (candidate.sku !== "" && equalFold(candidate.sku, trimmed))
    ) {
      return toProduct(candidate);
    }
  }

  throw new UnknownProductError(trimmed, rows.map(toProduct));
}

/** Reads one order and its lines. */
async function loadOrder(tx: Tx, workspaceId: string, id: string): Promise<Order> {
  // name: GetOrder
  const rows = await tx<OrderRow[]>`
    SELECT * FROM orders WHERE id = ${id} AND workspace_id = ${workspaceId}
  `;
  const row = rows[0];
  if (!row) {
    throw wrap(ErrNotFound, `order ${id}`);
  }

  // name: ListOrderItems
  const items = await tx<OrderItemRow[]>`
    SELECT * FROM order_items
    WHERE workspace_id = ${workspaceId} AND order_id = ${id}
    ORDER BY id
  `;

  return toOrder(row, items);
}

/** Reads the lines of many orders in one query. */
async function withLines(tx: Tx, workspaceId: string, rows: OrderRow[]): Promise<Order[]> {
  if (rows.length === 0) {
    return [];
  }

  const ids = rows.map((row) => row.id);

  // name: ListOrderItemsForOrders
  const items = await tx<OrderItemRow[]>`
    SELECT * FROM order_items
    WHERE workspace_id = ${workspaceId} AND order_id = ANY(${ids}::uuid[])
    ORDER BY order_id, id
  `;

  const byOrder = new Map<string, OrderItemRow[]>();
  for (const item of items) {
    const bucket = byOrder.get(item.order_id);
    if (bucket) {
      bucket.push(item);
    } else {
      byOrder.set(item.order_id, [item]);
    }
  }

  return rows.map((row) => toOrder(row, byOrder.get(row.id) ?? []));
}

// ---------------------------------------------------------------- conversions

/** Maps a product row. */
function toProduct(row: ProductRow): Product {
  return {
    id: row.id,
    sku: row.sku,
    name: row.name,
    unitPriceMicros: row.unit_price_micros,
    unit: row.unit,
    active: row.active,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

/** Maps a header and its lines. */
function toOrder(row: OrderRow, items: OrderItemRow[]): Order {
  return {
    id: row.id,
    code: row.code,
    status: row.status,
    note: row.note,
    agentId: row.agent_id ?? "",
    taskId: row.task_id ?? "",
    totalMicros: row.total_micros,
    customer: {
      id: row.customer_id ?? "",
      name: row.customer_name,
      phone: row.customer_phone,
      email: "",
      note: "",
      orders: 0,
      createdAt: row.created_at,
      updatedAt: row.updated_at,
    },
    lines: items.map((item) => ({
      id: item.id,
      orderId: item.order_id,
      productId: item.product_id ?? "",
      description: item.description,
      unit: item.unit,
      quantity: Number.parseFloat(item.quantity),
      unitPriceMicros: item.unit_price_micros,
      lineTotalMicros: item.line_total_micros,
    })),
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

// ---------------------------------------------------------------- phone and numeric

/**
 * Reduces a number to the digits a customer is keyed by.
 *
 * A WhatsApp chat id arrives as `6281234567890@s.whatsapp.net` and a person
 * types `0812-3456-7890`; both are the same customer, so both become
 * `6281234567890`. The leading zero of the local form is the one thing that
 * cannot be recovered by stripping punctuation, so it is rewritten to the
 * country code the deployment is in. Indonesia is the deployment's own country
 * (ADR 0001), and a workspace with international customers supplies the full
 * number, which needs no rewriting.
 */
export function normalizePhone(raw: string): string {
  let value = raw.trim();
  if (value === "") {
    return "";
  }

  // A JID keeps the number before the `@`.
  const at = value.indexOf("@");
  if (at >= 0) {
    value = value.slice(0, at);
  }

  const cleaned = value.replace(/[^0-9]/g, "");

  if (cleaned === "") {
    return "";
  }
  if (cleaned.startsWith("0")) {
    return `62${cleaned.replace(/^0+/, "")}`;
  }
  return cleaned;
}

/**
 * Renders a quantity for the NUMERIC column.
 *
 * The column is NUMERIC(12,3) because a product sold by weight has a fraction
 * of a unit, and a float round-tripped through Postgres is how "1.005" becomes
 * "1.0049999". The conversion is checked here because a NaN a caller computed
 * would otherwise arrive as a database error naming a column rather than a
 * request.
 */
function formatNumeric(quantity: number): string {
  if (!Number.isFinite(quantity)) {
    return "NaN";
  }
  return quantity.toFixed(3);
}

/** Renders an id for a nullable column, or null when it is absent. */
function optionalId(id: string | null | undefined): string | null {
  if (!id || id === "00000000-0000-0000-0000-000000000000") {
    return null;
  }
  return id;
}

/** Go's `strings.EqualFold` for the ASCII case folding a product name needs. */
function equalFold(left: string, right: string): boolean {
  return left.toLowerCase() === right.toLowerCase();
}

/** Turns a driver error into this module's own. */
function translateWrite(err: unknown, operation: string): Error {
  const pgErr = err as { code?: string; constraint_name?: string; detail?: string; message?: string };

  switch (pgErr?.code) {
    case "23505":
      // unique_violation
      if ((pgErr.constraint_name ?? "").includes("products_workspace_name_key")) {
        return wrap(ErrDuplicateProduct, pgErr.detail ?? errorMessage(err));
      }
      return wrap(ErrInvalidInput, pgErr.detail ?? errorMessage(err));
    case "23503": // foreign key
    case "23514": // check
    case "22P02": // invalid text representation
    case "22003": // numeric out of range
      return wrap(ErrInvalidInput, pgErr.message ?? errorMessage(err));
    default:
      return new Error(`order: ${operation}: ${errorMessage(err)}`, { cause: err });
  }
}
