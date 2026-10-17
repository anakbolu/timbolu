/**
 * The order module's contract: the catalogue a workspace sells from, the
 * customers it talks to, and the orders it took.
 *
 * The module exists because the MVP flow needed somewhere to put an order. A
 * Bolu reads a customer's message, looks up what was ordered, records it, and
 * composes the invoice a person approves — and the first two of those are
 * writes, which need a table and a rule about who may write.
 *
 * Two rules are the whole reason this package is more than a table definition:
 *
 *   - **A price comes from the catalogue, never from the model.** `record`
 *     takes product names and quantities; the price is resolved from `products`
 *     inside the write. A model cannot pass a price, so an invented one is
 *     impossible by construction rather than by instruction — which is what
 *     makes "an incomplete order never produces a wrong invoice" a property of
 *     the code.
 *   - **A line keeps its own copy of what was agreed.** The invoice a person
 *     approves and the record it came from must not change when somebody edits
 *     a price tomorrow, so the line snapshots the description, the unit, and
 *     the unit price while still pointing at the product it came from.
 *
 * Ported 1:1 from internal/modules/order/domain/order.go.
 */

/** A request that cannot be honoured: a missing product name, a quantity that is not positive, an order with no lines. */
export const ErrInvalidInput = new Error("order: invalid input");
/** A product, customer, or order that is not in this workspace. */
export const ErrNotFound = new Error("order: not found");
/** A record request names a product the catalogue does not have. */
export const ErrProductUnknown = new Error("order: the product is not in the catalogue");
/** A product whose name already exists in the workspace. */
export const ErrDuplicateProduct = new Error("order: a product with that name already exists");

/** The order was recorded. */
export const StatusRecorded = "recorded";
/** A person decided not to fulfil the order. */
export const StatusCanceled = "canceled";

/** Every legal status value, which is what a check constraint and a screen both read. */
export const Statuses: string[] = [StatusRecorded, StatusCanceled];

/** The zero uuid, as Go's `uuid.Nil`. */
export const NilUUID = "00000000-0000-0000-0000-000000000000";

/** The tenant a call runs as. */
export interface Scope {
  userId?: string;
  workspaceId: string;
}

/** Reports whether the scope names no workspace. */
export function isZeroScope(scope: Scope): boolean {
  return !scope.workspaceId || scope.workspaceId === NilUUID;
}

/** One thing a workspace sells. */
export interface Product {
  id: string;
  sku: string;
  name: string;
  /**
   * The price of one unit, in the micro-rupiah the usage ledger also uses, so
   * an invoice total and a cost are the same unit rather than two numbers
   * somebody adds together.
   */
  unitPriceMicros: number;
  /** How it is sold: "pcs", "kg", "box". It travels onto the line so an invoice reads "2 kg". */
  unit: string;
  /**
   * Hides a product from the catalogue without deleting it: what stopped being
   * sold must not disappear from the orders that sold it.
   */
  active: boolean;
  createdAt: Date;
  updatedAt: Date;
}

/** The price in rupiah, which is what a person reads and what a model is told. */
export function productPriceIdr(product: Product): number {
  return product.unitPriceMicros / 1_000_000;
}

/** Somebody a workspace sells to. */
export interface Customer {
  id: string;
  name: string;
  phone: string;
  email: string;
  note: string;
  /** How many orders this customer has, which is what makes "pelanggan baru" answerable. */
  orders: number;
  createdAt: Date;
  updatedAt: Date;
}

/** One product on an order, with its own copy of what was agreed. */
export interface OrderLine {
  id: string;
  orderId: string;
  productId: string;
  description: string;
  unit: string;
  quantity: number;
  /**
   * The catalogue's price at the moment the order was taken, copied onto the
   * line rather than read through the product.
   */
  unitPriceMicros: number;
  /**
   * Computed by the database from the quantity and the unit price, so a line's
   * total cannot disagree with its own two fields.
   */
  lineTotalMicros: number;
}

/**
 * The agreed unit price in rupiah, which is what a person and a model read. The
 * stored value is micro-rupiah, so the conversion is one place: a line that
 * divided by a thousand somewhere else would be a quote a thousand times off.
 */
export function linePriceIdr(line: OrderLine): number {
  return line.unitPriceMicros / 1_000_000;
}

/** The line's total in rupiah, as the database computed it. */
export function lineTotalIdr(line: OrderLine): number {
  return line.lineTotalMicros / 1_000_000;
}

/** One customer's order. */
export interface Order {
  id: string;
  code: string;
  status: string;
  note: string;
  agentId: string;
  taskId: string;
  customer: Customer;
  /** The sum of the lines, written by the statement that inserted them. */
  totalMicros: number;
  lines: OrderLine[];
  createdAt: Date;
  updatedAt: Date;
}

/** The order's total in rupiah. */
export function orderTotalIdr(order: Order): number {
  return order.totalMicros / 1_000_000;
}

/**
 * What recording an order returns for a product the catalogue does not have.
 *
 * It carries the candidates — with their prices, which is the part that saves a
 * round trip: a model that reads "kandidat: Kopi Arabika (Rp 15000)" can ask the
 * customer which one they meant, while a model that reads a bare "not found"
 * tends to try another name it invented.
 */
export class UnknownProductError extends Error {
  /** The sentinel this error matches, for `is`. */
  readonly sentinel = ErrProductUnknown;

  constructor(
    /** What the caller asked for. */
    readonly productName: string,
    /** The catalogue entries it could mean, best match first. */
    readonly candidates: Product[],
  ) {
    super(unknownProductMessage(productName, candidates));
    this.name = "UnknownProductError";
  }
}

/** The message Go's `(*UnknownProductError).Error()` writes for the model. */
function unknownProductMessage(name: string, candidates: Product[]): string {
  if (candidates.length === 0) {
    return (
      `${ErrProductUnknown.message}: ${JSON.stringify(name)} tidak ada di katalog workspace ini. ` +
      "Tanyakan ke pemilik usaha untuk menambahkannya; jangan mengarang harga."
    );
  }

  const options = candidates.map(
    (candidate) => `${candidate.name} (${rupiah(productPriceIdr(candidate))})`,
  );

  return (
    `${ErrProductUnknown.message}: ${JSON.stringify(name)} tidak ada di katalog. ` +
    `Kandidat: ${options.join(", ")}. ` +
    "Tanyakan ke pelanggan produk mana yang dimaksud, atau pakai salah satu kandidat kalau memang itu maksudnya; jangan mengarang harga."
  );
}

/**
 * Renders an amount the way a person writes it: "Rp 15000", and "Rp 15000,50"
 * when there are sen. It is exported because the tool adapter writes summaries a
 * person reads, and two renderings of the same amount is two answers to what a
 * price looks like — one of which will eventually end a sentence with a decimal
 * point.
 */
export function rupiah(amount: number): string {
  let whole = Math.trunc(amount);
  // Rounded rather than truncated: 12345.999 rupiah is 12346 to everybody except a cast.
  let fraction = Math.trunc((amount - whole) * 100 + 0.5);
  if (fraction >= 100) {
    whole++;
    fraction = 0;
  }

  if (fraction === 0) {
    return `Rp ${whole}`;
  }

  return `Rp ${whole},${fraction.toString().padStart(2, "0")}`;
}

/** A product to insert. */
export interface NewProduct {
  sku: string;
  name: string;
  unitPriceMicros: number;
  unit: string;
  active: boolean;
}

/** One edit. An absent field is left alone. */
export interface ProductChange {
  sku?: string;
  name?: string;
  unitPriceMicros?: number;
  unit?: string;
  active?: boolean;
}

/** Narrows a catalogue read. */
export interface ProductFilter {
  /** Matches a name, case insensitively. */
  search: string;
  /** Returns products that are no longer sold. */
  includeInactive: boolean;
  limit: number;
}

/** One line of an order, named by the product rather than by a price. */
export interface NewItem {
  /** Names a catalogue entry. The price is resolved from it inside the write. */
  product: string;
  /** May be fractional, because a product sold by weight is. */
  quantity: number;
  /** Anything the customer said about this line. */
  note: string;
  /** Overrides the catalogue name when the customer used different words. */
  description: string;
}

/** An order to record. */
export interface NewOrder {
  /**
   * The identity of the buyer. It is what a second order from the same person
   * resolves to.
   */
  customerPhone: string;
  customerName: string;
  agentId: string;
  taskId: string;
  note: string;
  items: NewItem[];
}

/** Narrows an order read. */
export interface OrderFilter {
  /** Limits the read to one customer. */
  customerPhone: string;
  limit: number;
}

/** Bounds a list when the caller does not say how many. */
export const DefaultLimit = 50;
/** Bounds one page. */
export const MaxLimit = 200;

/** Returns the filter with its bounds applied, so every caller reads the same page size. */
export function normaliseProductFilter(filter: ProductFilter): ProductFilter {
  let limit = filter.limit;
  if (limit <= 0) {
    limit = DefaultLimit;
  } else if (limit > MaxLimit) {
    limit = MaxLimit;
  }
  return { ...filter, limit };
}

/** Returns the order filter with its bounds applied. */
export function normaliseOrderFilter(filter: OrderFilter): OrderFilter {
  let limit = filter.limit;
  if (limit <= 0) {
    limit = DefaultLimit;
  } else if (limit > MaxLimit) {
    limit = MaxLimit;
  }
  return { ...filter, limit };
}

/** The persistence contract. */
export interface Repository {
  // --- the catalogue

  products(scope: Scope, filter: ProductFilter): Promise<Product[]>;
  product(scope: Scope, id: string): Promise<Product>;
  createProduct(scope: Scope, product: NewProduct): Promise<Product>;
  updateProduct(scope: Scope, id: string, change: ProductChange): Promise<Product>;
  deleteProduct(scope: Scope, id: string): Promise<void>;
  /**
   * Finds the catalogue entries a name could mean, best match first. It is what
   * turns "the product is not in the catalogue" into "did you mean one of these".
   */
  matchProducts(scope: Scope, term: string, limit: number): Promise<Product[]>;

  // --- the flow

  /**
   * Writes an order, its lines, and the customer in one transaction, and
   * answers with what it stored. A product the catalogue does not have is
   * refused with ErrProductUnknown and the candidates, and nothing is written.
   */
  record(scope: Scope, order: NewOrder): Promise<Order>;
  orders(scope: Scope, filter: OrderFilter): Promise<Order[]>;
  order(scope: Scope, id: string): Promise<Order>;
  setOrderStatus(scope: Scope, id: string, status: string): Promise<Order>;
}

/** What the handler and the tool adapter call. */
export interface Service {
  // --- the catalogue, for the screen that maintains it

  products(ctx: unknown, scope: Scope, filter: ProductFilter): Promise<Product[]>;
  product(ctx: unknown, scope: Scope, id: string): Promise<Product>;
  createProduct(ctx: unknown, scope: Scope, request: CreateProductRequest): Promise<Product>;
  updateProduct(ctx: unknown, scope: Scope, id: string, request: UpdateProductRequest): Promise<Product>;
  deleteProduct(ctx: unknown, scope: Scope, id: string): Promise<void>;

  // --- the flow

  /**
   * Answers what a name means in this workspace's catalogue: the product, or
   * the candidates it could be, or neither.
   */
  lookup(ctx: unknown, scope: Scope, term: string): Promise<Lookup>;
  /** Writes an order from product names and quantities. The prices come from the catalogue. */
  record(ctx: unknown, scope: Scope, order: NewOrder): Promise<Order>;
  orders(ctx: unknown, scope: Scope, filter: OrderFilter): Promise<Order[]>;
  order(ctx: unknown, scope: Scope, id: string): Promise<Order>;
  /** Marks an order a person decided not to fulfil. */
  cancel(ctx: unknown, scope: Scope, id: string): Promise<Order>;
}

/** A catalogue entry from the screen. */
export interface CreateProductRequest {
  sku: string;
  name: string;
  unitPriceMicros: number;
  unit: string;
}

/** One edit from the screen. */
export interface UpdateProductRequest {
  sku?: string;
  name?: string;
  unitPriceMicros?: number;
  unit?: string;
  active?: boolean;
}

/**
 * What a product name resolved to.
 *
 * It carries three answers rather than one because the flow needs all three: an
 * exact product to price an order, candidates to ask a person about, and
 * "nothing at all" to tell the customer the product is not sold.
 */
export interface Lookup {
  /** The product whose name matches, when one does. */
  found: Product | null;
  /**
   * The catalogue entries the term could mean, best match first. It is never
   * empty when `found` is null and the workspace has any product at all.
   */
  candidates: Product[];
}

/**
 * An error carrying the sentinel a caller switches on, so `is` reproduces Go's
 * `errors.Is` for the messages `fmt.Errorf("%w: ...")` produced.
 */
export class WrappedError extends Error {
  constructor(
    readonly sentinel: Error,
    message: string,
  ) {
    super(message);
    this.name = "WrappedError";
  }
}

/** Wraps a sentinel with a detail, rendering the message as Go's `%w: detail`. */
export function wrap(sentinel: Error, detail: string): WrappedError {
  return new WrappedError(sentinel, `${sentinel.message}: ${detail}`);
}

/** True when `err` is the sentinel or wraps it. */
export function is(err: unknown, sentinel: Error): boolean {
  let current: unknown = err;
  for (let guard = 0; guard < 32; guard++) {
    if (current === sentinel) {
      return true;
    }
    if (!(current instanceof Error)) {
      return false;
    }
    if (current instanceof WrappedError) {
      current = current.sentinel;
      continue;
    }
    if (current instanceof UnknownProductError) {
      return current.sentinel === sentinel;
    }
    current = "cause" in current ? current.cause : undefined;
  }
  return false;
}

/** The message of an unknown thrown value. */
export function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
