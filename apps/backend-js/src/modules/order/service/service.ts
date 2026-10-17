/**
 * The order module's use cases: maintaining the catalogue, and recording the
 * order a customer placed.
 *
 * It depends only on the module's domain contracts, so the flow can be tested
 * without a database — which matters more here than elsewhere, because this is
 * the module whose rules decide what an invoice says.
 *
 * Ported 1:1 from internal/modules/order/service/service.go.
 */

import { nopLogger, type Logger } from "../../../platform/logger.ts";
import {
  ErrInvalidInput,
  NilUUID,
  StatusCanceled,
  isZeroScope,
  wrap,
  type CreateProductRequest,
  type Lookup,
  type NewOrder,
  type Order,
  type OrderFilter,
  type Product,
  type ProductFilter,
  type Repository,
  type Scope,
  type Service as ServicePort,
  type UpdateProductRequest,
} from "../domain/order.ts";

/** The service dependencies. */
export interface Deps {
  repository: Repository;
  logger?: Logger | null;
}

/** The order module's implementation. */
export class Service implements ServicePort {
  private readonly deps: Repository;
  private readonly log: Logger;

  constructor(deps: Deps) {
    this.deps = deps.repository;
    this.log = deps.logger ?? nopLogger;
  }

  // ---------------------------------------------------------------- the catalogue

  /** Returns the workspace's catalogue. */
  async products(ctx: unknown, scope: Scope, filter: ProductFilter): Promise<Product[]> {
    requireScope(scope);
    return this.deps.products(scope, filter);
  }

  /** Returns one catalogue entry. */
  async product(ctx: unknown, scope: Scope, id: string): Promise<Product> {
    requireScope(scope);
    if (isNilId(id)) {
      throw wrap(ErrInvalidInput, "product id is required");
    }
    return this.deps.product(scope, id);
  }

  /** Adds a catalogue entry. */
  async createProduct(ctx: unknown, scope: Scope, request: CreateProductRequest): Promise<Product> {
    requireScope(scope);

    const name = request.name.trim();
    if (name === "") {
      throw wrap(ErrInvalidInput, "a product needs a name");
    }
    if (request.unitPriceMicros < 0) {
      throw wrap(ErrInvalidInput, "a price cannot be negative");
    }

    return this.deps.createProduct(scope, {
      sku: request.sku,
      name,
      unitPriceMicros: request.unitPriceMicros,
      unit: request.unit,
      active: true,
    });
  }

  /** Edits a catalogue entry. */
  async updateProduct(
    ctx: unknown,
    scope: Scope,
    id: string,
    request: UpdateProductRequest,
  ): Promise<Product> {
    requireScope(scope);
    if (isNilId(id)) {
      throw wrap(ErrInvalidInput, "product id is required");
    }
    if (request.name !== undefined && request.name.trim() === "") {
      throw wrap(ErrInvalidInput, "a product needs a name");
    }
    if (request.unitPriceMicros !== undefined && request.unitPriceMicros < 0) {
      throw wrap(ErrInvalidInput, "a price cannot be negative");
    }

    // The two shapes are the same five optional fields, and the conversion is
    // written as one so a field added to the request cannot be silently dropped
    // on the way to the repository.
    return this.deps.updateProduct(scope, id, {
      sku: request.sku,
      name: request.name,
      unitPriceMicros: request.unitPriceMicros,
      unit: request.unit,
      active: request.active,
    });
  }

  /**
   * Removes a catalogue entry.
   *
   * It is a read-then-delete rather than a bare delete so that removing
   * something already gone is reported as not found: a screen that showed
   * "deleted" for a product somebody else removed a minute earlier would be
   * telling the person something untrue about their own workspace.
   */
  async deleteProduct(ctx: unknown, scope: Scope, id: string): Promise<void> {
    requireScope(scope);
    if (isNilId(id)) {
      throw wrap(ErrInvalidInput, "product id is required");
    }

    return this.deps.deleteProduct(scope, id);
  }

  // ---------------------------------------------------------------- the flow

  /**
   * Answers what a name means in this workspace's catalogue.
   *
   * It reads the catalogue once and answers in one of three ways, which is what
   * the flow needs: the product to price an order, the candidates to ask a
   * person about, or nothing at all to tell the customer the product is not sold
   * here. It is a read, so a Bolu can call it as often as it needs to before it
   * records anything.
   */
  async lookup(ctx: unknown, scope: Scope, term: string): Promise<Lookup> {
    requireScope(scope);

    const trimmed = term.trim();
    if (trimmed === "") {
      throw wrap(ErrInvalidInput, "nama produk belum disebut");
    }

    const matches = await this.deps.matchProducts(scope, trimmed, 5);

    const lookup: Lookup = { found: null, candidates: matches };
    for (let index = 0; index < matches.length; index++) {
      // An exact name (or sku) is the answer; anything else is only a
      // suggestion. The comparison is case-folded because a customer types
      // "Kopi arabika" and the catalogue says "Kopi Arabika" — the same product
      // to everybody except a string.
      const candidate = matches[index];
      if (
        equalFold(candidate.name, trimmed) ||
        (candidate.sku !== "" && equalFold(candidate.sku, trimmed))
      ) {
        lookup.found = candidate;
        lookup.candidates = matches.slice(index);

        break;
      }
    }

    return lookup;
  }

  /**
   * Writes the order a customer placed.
   *
   * Three checks happen before anything is written, and each of them is a case
   * the flow has to handle:
   *
   *   - **A request with no lines is refused.** An order with no products is not
   *     an order, and the invoice that followed it would be for nothing.
   *   - **A quantity that is not positive is refused**, naming the line: "0" and
   *     "-2" are both a customer's typo or a model's slip, and neither belongs
   *     on an invoice.
   *   - **A product the catalogue does not have refuses the whole order**, with
   *     the candidates. The alternative — writing the lines that did resolve —
   *     is an invoice for part of what the customer asked for, which is worse
   *     than asking.
   */
  async record(ctx: unknown, scope: Scope, order: NewOrder): Promise<Order> {
    requireScope(scope);

    if (order.items.length === 0) {
      throw wrap(ErrInvalidInput, "pesanan belum menyebut produk apa pun");
    }

    const items = order.items.map((item) => {
      const name = item.product.trim();
      if (name === "") {
        throw wrap(ErrInvalidInput, "ada baris pesanan yang belum menyebut nama produk");
      }
      if (item.quantity <= 0) {
        throw wrap(
          ErrInvalidInput,
          `jumlah untuk ${JSON.stringify(name)} harus lebih dari nol (diminta ${formatG(item.quantity)})`,
        );
      }
      return { ...item, product: name };
    });

    const next: NewOrder = {
      ...order,
      items,
      customerPhone: order.customerPhone.trim(),
      customerName: order.customerName.trim(),
    };

    // A phone is what a customer is keyed by, so an order without one is
    // recorded against an empty customer rather than refused: a Bolu that was
    // asked to note an order from a phone call has no number to give, and losing
    // the order is worse than losing the link.
    if (next.customerPhone === "") {
      this.log.warn({ customer: next.customerName }, "recording an order with no phone number");
    }

    return this.deps.record(scope, next);
  }

  /** Returns the workspace's orders, newest first. */
  async orders(ctx: unknown, scope: Scope, filter: OrderFilter): Promise<Order[]> {
    requireScope(scope);
    return this.deps.orders(scope, filter);
  }

  /** Returns one order with its lines. */
  async order(ctx: unknown, scope: Scope, id: string): Promise<Order> {
    requireScope(scope);
    if (isNilId(id)) {
      throw wrap(ErrInvalidInput, "order id is required");
    }
    return this.deps.order(scope, id);
  }

  /** Marks an order a person decided not to fulfil. */
  async cancel(ctx: unknown, scope: Scope, id: string): Promise<Order> {
    requireScope(scope);
    if (isNilId(id)) {
      throw wrap(ErrInvalidInput, "order id is required");
    }
    return this.deps.setOrderStatus(scope, id, StatusCanceled);
  }

  /**
   * Returns the catalogue entries a term could mean, which is what a screen's
   * search box and the tool's suggestion list both read.
   */
  async candidates(ctx: unknown, scope: Scope, term: string, limit: number): Promise<Product[]> {
    requireScope(scope);
    return this.deps.matchProducts(scope, term, limit);
  }
}

/** What a caller gets when the module was assembled without storage. */
export const ErrNoRepository = new Error("order: no repository configured");

/**
 * Refuses a call with no tenant, which is the one thing every method here
 * needs.
 */
function requireScope(scope: Scope): void {
  if (isZeroScope(scope)) {
    throw wrap(ErrInvalidInput, "workspace is required");
  }
}

/** Go's `uuid.Nil` check for an id that is absent. */
function isNilId(id: string): boolean {
  return id === "" || id === NilUUID;
}

/**
 * Go's `strings.EqualFold` for the ASCII case folding a product name needs.
 * `toLowerCase` is not the same thing for every script; product names are
 * Latin-script in practice, and a case-sensitive comparison would be the worse
 * failure.
 */
function equalFold(left: string, right: string): boolean {
  return left.toLowerCase() === right.toLowerCase();
}

/** Renders a float the way Go's `%g` does for the message a model reads. */
function formatG(value: number): string {
  if (Number.isInteger(value)) {
    return String(value);
  }
  return String(Number.parseFloat(value.toPrecision(6)));
}
