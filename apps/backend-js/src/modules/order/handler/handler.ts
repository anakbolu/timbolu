/**
 * Exposes the workspace's own data over HTTP: the product catalogue a workspace
 * maintains, and the orders it took.
 *
 * The catalogue is a screen's business — a person types their prices, and every
 * price a Bolu later quotes comes from here — so it is the only write surface.
 * Orders are read-only over HTTP: an order is recorded by the Bolu from what a
 * customer said, and a client that could insert one directly could claim an
 * order that never happened. Cancelling is the one exception, because a person
 * deciding not to fulfil an order is exactly the kind of decision the product
 * puts in a human's hands.
 *
 * Ported 1:1 from internal/modules/order/handler/handler.go.
 */

import type { Hono } from "hono";
import { currentUserId, isUuid, workspaceUUID, type AppContext, type AppEnv } from "../../../platform/context.ts";
import type { Logger } from "../../../platform/logger.ts";
import { created, error, ok, okWithMeta } from "../../../platform/response.ts";
import {
  ErrDuplicateProduct,
  ErrInvalidInput,
  ErrNotFound,
  ErrProductUnknown,
  errorMessage,
  is,
  linePriceIdr,
  lineTotalIdr,
  orderTotalIdr,
  productPriceIdr,
  type Order,
  type OrderFilter,
  type Product,
  type ProductFilter,
  type Scope,
  type Service,
} from "../domain/order.ts";

/** Serves the catalogue and the orders. */
export class Handler {
  constructor(
    private readonly service: Service,
    private readonly log: Logger,
  ) {}

  /** Lists the catalogue. */
  products = async (c: AppContext): Promise<Response> => {
    const filter: ProductFilter = {
      search: c.req.query("q") ?? "",
      includeInactive: queryBool(c, "include_inactive"),
      limit: queryInt(c, "limit", 0),
    };

    let products: Product[];
    try {
      products = await this.service.products(c, requestScopeOf(c), filter);
    } catch (err) {
      return this.fail(c, "list products", err);
    }

    const payload = products.map(toProductPayload);
    return okWithMeta(c, "ok", payload, { count: payload.length });
  };

  /** Adds a catalogue entry. */
  createProduct = async (c: AppContext): Promise<Response> => {
    const req = await readProductRequest(c);
    if (!req) {
      return error(c, 400, "invalid request body", "invalid_request");
    }

    let product: Product;
    try {
      product = await this.service.createProduct(c, requestScopeOf(c), {
        sku: req.sku ?? "",
        name: req.name ?? "",
        // The wire carries rupiah because a screen has rupiah and the API does
        // not ask a client to know the storage unit; the conversion happens
        // here, once, with the same factor the usage report uses.
        unitPriceMicros: rupiahToMicros(req.price_idr ?? 0),
        unit: req.unit ?? "",
      });
    } catch (err) {
      return this.fail(c, "create product", err);
    }

    return created(c, "produk ditambahkan", toProductPayload(product));
  };

  /** Edits a catalogue entry. */
  updateProduct = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("productID");
    if (!isUuid(id)) {
      return error(c, 400, "product id is not a uuid", "invalid_input");
    }

    const req = await readProductRequest(c);
    if (!req) {
      return error(c, 400, "invalid request body", "invalid_request");
    }

    let product: Product;
    try {
      product = await this.service.updateProduct(c, requestScopeOf(c), id, {
        sku: req.sku,
        name: req.name,
        unitPriceMicros: req.price_idr === undefined ? undefined : rupiahToMicros(req.price_idr),
        unit: req.unit,
        active: req.active,
      });
    } catch (err) {
      return this.fail(c, "update product", err);
    }

    return ok(c, "produk diperbarui", toProductPayload(product));
  };

  /** Removes a catalogue entry. */
  deleteProduct = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("productID");
    if (!isUuid(id)) {
      return error(c, 400, "product id is not a uuid", "invalid_input");
    }

    try {
      await this.service.deleteProduct(c, requestScopeOf(c), id);
    } catch (err) {
      return this.fail(c, "delete product", err);
    }

    return ok(c, "produk dihapus");
  };

  /** Lists the workspace's orders. */
  orders = async (c: AppContext): Promise<Response> => {
    const filter: OrderFilter = {
      customerPhone: c.req.query("customer_phone") ?? "",
      limit: queryInt(c, "limit", 0),
    };

    let orders: Order[];
    try {
      orders = await this.service.orders(c, requestScopeOf(c), filter);
    } catch (err) {
      return this.fail(c, "list orders", err);
    }

    const payload = orders.map(toOrderPayload);
    return okWithMeta(c, "ok", payload, { count: payload.length });
  };

  /** Returns one order with its lines. */
  order = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("orderID");
    if (!isUuid(id)) {
      return error(c, 400, "order id is not a uuid", "invalid_input");
    }

    let order: Order;
    try {
      order = await this.service.order(c, requestScopeOf(c), id);
    } catch (err) {
      return this.fail(c, "get order", err);
    }

    return ok(c, "ok", toOrderPayload(order));
  };

  /** Marks an order a person decided not to fulfil. */
  cancel = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("orderID");
    if (!isUuid(id)) {
      return error(c, 400, "order id is not a uuid", "invalid_input");
    }

    let order: Order;
    try {
      order = await this.service.cancel(c, requestScopeOf(c), id);
    } catch (err) {
      return this.fail(c, "cancel order", err);
    }

    return ok(c, "pesanan dibatalkan", toOrderPayload(order));
  };

  /** Maps a service error onto a response. */
  private fail(c: AppContext, operation: string, err: unknown): Response {
    if (is(err, ErrNotFound)) {
      return error(c, 404, "tidak ditemukan", "not_found");
    }
    if (is(err, ErrDuplicateProduct)) {
      return error(c, 409, "produk dengan nama itu sudah ada di katalog", "duplicate_product");
    }
    if (is(err, ErrInvalidInput) || is(err, ErrProductUnknown)) {
      return error(c, 400, errorMessage(err), "invalid_input");
    }

    this.log.error({ operation, err }, "order request failed");
    return error(c, 500, "permintaan gagal", "internal_error");
  }
}

/**
 * Registers the endpoints every member of the workspace may read.
 *
 * `/orders/products` is registered before `/orders/:orderID` on purpose: a
 * router matches in registration order, and the reverse would capture
 * "products" as an order id.
 */
export function routes(group: Hono<AppEnv>, h: Handler): void {
  group.get("/orders/products", h.products);
  group.get("/orders", h.orders);
  group.get("/orders/:orderID", h.order);
}

/**
 * Registers the writes. The container applies the tenant middleware and a
 * managing-role check.
 */
export function managerRoutes(group: Hono<AppEnv>, h: Handler): void {
  group.post("/orders/products", h.createProduct);
  group.put("/orders/products/:productID", h.updateProduct);
  group.delete("/orders/products/:productID", h.deleteProduct);
  group.post("/orders/:orderID/cancel", h.cancel);
}

/** The tenant the request runs as. */
function requestScopeOf(c: AppContext): Scope {
  return { userId: currentUserId(c), workspaceId: workspaceUUID(c) ?? "" };
}

// ---------------------------------------------------------------- the wire

/** A catalogue entry from a screen. */
interface ProductRequest {
  sku?: string;
  name?: string;
  price_idr?: number;
  unit?: string;
  active?: boolean;
}

/** A catalogue entry as a screen reads it. */
interface ProductPayload {
  product_id: string;
  sku: string;
  name: string;
  price_idr: number;
  unit: string;
  active: boolean;
  updated_at: string;
  created_at: string;
}

/** An order as a screen reads it. */
interface OrderPayload {
  order_id: string;
  code: string;
  status: string;
  note?: string;
  customer_name: string;
  customer_phone: string;
  agent_id?: string;
  task_id?: string;
  total_idr: number;
  lines: LinePayload[];
  created_at: string;
}

/** One order line. */
interface LinePayload {
  product_id?: string;
  description: string;
  unit: string;
  quantity: number;
  unit_price_idr: number;
  line_total_idr: number;
}

/** Renders a product. */
function toProductPayload(product: Product): ProductPayload {
  return {
    product_id: product.id,
    sku: product.sku,
    name: product.name,
    price_idr: productPriceIdr(product),
    unit: product.unit,
    active: product.active,
    created_at: rfc3339(product.createdAt),
    updated_at: rfc3339(product.updatedAt),
  };
}

/** Renders an order with its lines. */
function toOrderPayload(order: Order): OrderPayload {
  const payload: OrderPayload = {
    order_id: order.id,
    code: order.code,
    status: order.status,
    customer_name: order.customer.name,
    customer_phone: order.customer.phone,
    total_idr: orderTotalIdr(order),
    created_at: rfc3339(order.createdAt),
    lines: order.lines.map((line) => {
      const item: LinePayload = {
        description: line.description,
        unit: line.unit,
        quantity: line.quantity,
        unit_price_idr: linePriceIdr(line),
        line_total_idr: lineTotalIdr(line),
      };
      if (line.productId !== "") {
        item.product_id = line.productId;
      }
      return item;
    }),
  };

  if (order.note !== "") {
    payload.note = order.note;
  }
  if (order.agentId !== "") {
    payload.agent_id = order.agentId;
  }
  if (order.taskId !== "") {
    payload.task_id = order.taskId;
  }

  return payload;
}

/** Formats a time the way Go's `Format("2006-01-02T15:04:05Z07:00")` does. */
function rfc3339(at: Date): string {
  return at.toISOString().replace(/\.\d{3}Z$/, "Z");
}

/** Converts the rupiah a screen sends into the micro-rupiah everything else stores. */
function rupiahToMicros(rupiah: number): number {
  return Math.trunc(rupiah * 1_000_000);
}

/** Parses a request body, answering null when it is not a JSON object. */
async function readProductRequest(c: AppContext): Promise<ProductRequest | null> {
  const body: unknown = await c.req.json().catch(() => null);
  if (body === null || typeof body !== "object" || Array.isArray(body)) {
    return null;
  }

  const raw = body as Record<string, unknown>;
  return {
    sku: typeof raw.sku === "string" ? raw.sku : undefined,
    name: typeof raw.name === "string" ? raw.name : undefined,
    price_idr: typeof raw.price_idr === "number" ? raw.price_idr : undefined,
    unit: typeof raw.unit === "string" ? raw.unit : undefined,
    active: typeof raw.active === "boolean" ? raw.active : undefined,
  };
}

/** Reads a boolean query parameter the way Fiber's `QueryBool` does. */
function queryBool(c: AppContext, name: string): boolean {
  const raw = (c.req.query(name) ?? "").trim().toLowerCase();
  return raw === "1" || raw === "true" || raw === "t" || raw === "yes" || raw === "on";
}

/** Reads an integer query parameter, treating an unreadable value as the default. */
function queryInt(c: AppContext, name: string, fallback: number): number {
  const raw = c.req.query(name);
  if (!raw || !/^[+-]?\d+$/.test(raw.trim())) {
    return fallback;
  }
  return Number.parseInt(raw.trim(), 10);
}
