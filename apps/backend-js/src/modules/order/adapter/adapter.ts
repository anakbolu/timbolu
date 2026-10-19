/**
 * Exposes the workspace's own data — its product catalogue and the orders it
 * takes — as tools the hub can run.
 *
 * It is an adapter in the hub's sense, and it is the first one that is *not* a
 * connected account: a catalogue is the workspace's, there is no provider, no
 * token, and nothing outside this deployment to reach. `needsConnection` is what
 * says so, and it is what the tool filter reads to offer these tools to a Bolu
 * whose profile asks for them without demanding a grant.
 *
 * The adapter is deliberately thin. Everything that decides what an order says
 * lives in the service: this file turns a tool call's JSON into a service call
 * and the answer back into JSON, and it does no arithmetic, no resolution, and
 * no validation of its own — a second place that decided what a price is would
 * be a second answer to the same question.
 *
 * Ported 1:1 from internal/modules/order/adapter/adapter.go.
 */

import {
  ErrSchemaViolation,
  ErrUnknownTool,
  type Adapter as IntegrationAdapter,
  type Call,
  type Result,
  type ToolSpec,
  type TriggerSpec,
} from "../../integration/domain/adapter.ts";
import { LabelRead, LabelWriteInternal } from "../../integration/domain/integration.ts";
import {
  ErrInvalidInput,
  errorMessage,
  linePriceIdr,
  lineTotalIdr,
  orderTotalIdr,
  productPriceIdr,
  rupiah,
  type Lookup,
  type NewItem,
  type NewOrder,
  type Order,
  type OrderFilter,
  type Product,
  type Scope,
} from "../domain/order.ts";

/** The application key this adapter answers for. */
export const App = "order";

/**
 * Answers what the catalogue charges for something. It is `read`: it looks, and
 * nothing outside the workspace changes.
 */
export const ToolFindProduct = "order.find_product";
/**
 * Writes the order into the workspace's own data. It is `write_internal`, which
 * is the label that means "this changes the workspace and nobody is asked": an
 * order a customer placed is a record of something that already happened, not an
 * action that leaves the building.
 */
export const ToolRecordOrder = "order.record_order";
/**
 * Answers what has been ordered, so a follow-up question about a previous order
 * is answerable without asking the customer to repeat themselves.
 */
export const ToolReadOrders = "order.read_orders";

/**
 * The part of the module this adapter uses.
 *
 * It is declared here rather than taken as the whole service, so the adapter can
 * say exactly what it needs: look something up, record an order, read orders
 * back. Anything wider would let a tool reach parts of the module that are not
 * its business.
 */
export interface Service {
  lookup(ctx: unknown, scope: Scope, term: string): Promise<Lookup>;
  record(ctx: unknown, scope: Scope, order: NewOrder): Promise<Order>;
  orders(ctx: unknown, scope: Scope, filter: OrderFilter): Promise<Order[]>;
}

/** The workspace-data integration. */
export class Adapter implements IntegrationAdapter {
  constructor(private readonly orders: Service | null) {}

  /** The application key. */
  app(): string {
    return App;
  }

  /**
   * There is no token, because there is no account. The workspace is the
   * account.
   */
  needsToken(): boolean {
    return false;
  }

  /**
   * A catalogue and an order are the workspace's own data, so a call needs no
   * connected account and no grant.
   */
  needsConnection(): boolean {
    return false;
  }

  /** A catalogue announces nothing. */
  triggers(): TriggerSpec[] {
    return [];
  }

  /** What a Bolu can do with the workspace's own data. */
  tools(): ToolSpec[] {
    return [
      {
        name: ToolFindProduct,
        label: LabelRead,
        description:
          "Cari harga satu produk di katalog workspace. Pakai ini sebelum menyebut " +
          "harga ke pelanggan; kalau produknya tidak ada, tanyakan ke pelanggan jangan mengarang harga.",
        schema: `{
				"type": "object",
				"properties": {
					"product": {"type": "string", "description": "Nama produk seperti yang disebut pelanggan"}
				},
				"required": ["product"]
			}`,
      },
      {
        name: ToolRecordOrder,
        label: LabelWriteInternal,
        description:
          "Catat pesanan pelanggan ke data workspace: produk, jumlah, dan kontak. " +
          "Harganya diambil dari katalog, jadi cukup sebut nama produknya. " +
          "Panggil ini setelah semua produk dan jumlahnya jelas.",
        schema: `{
				"type": "object",
				"properties": {
					"customer_name": {"type": "string", "description": "Nama pelanggan, kalau disebutkan"},
					"customer_phone": {"type": "string", "description": "Nomor WhatsApp pelanggan, apa adanya"},
					"note": {"type": "string", "description": "Catatan pesanan, misalnya alamat kirim"},
					"items": {
						"type": "array",
						"minItems": 1,
						"description": "Satu entri per produk yang dipesan",
						"items": {
							"type": "object",
							"properties": {
								"product": {"type": "string", "description": "Nama produk di katalog"},
								"quantity": {"type": "number", "description": "Jumlah, boleh pecahan untuk barang timbang"},
								"note": {"type": "string", "description": "Catatan untuk baris ini"}
							},
							"required": ["product", "quantity"]
						}
					}
				},
				"required": ["items"]
			}`,
      },
      {
        name: ToolReadOrders,
        label: LabelRead,
        description:
          "Baca pesanan terakhir workspace ini, atau pesanan satu pelanggan. " +
          "Pakai untuk menjawab pertanyaan tentang pesanan sebelumnya.",
        schema: `{
				"type": "object",
				"properties": {
					"customer_phone": {"type": "string", "description": "Nomor pelanggan; kosongkan untuk pesanan terakhir siapa pun"},
					"limit": {"type": "integer", "minimum": 1, "maximum": 50}
				}
			}`,
      },
    ];
  }

  /** Runs one tool call. */
  async execute(ctx: unknown, call: Call): Promise<Result> {
    const scope: Scope = {
      userId: call.scope.userId,
      workspaceId: call.scope.workspaceId,
    };

    switch (call.tool) {
      case ToolFindProduct:
        return this.findProduct(ctx, scope, call);
      case ToolRecordOrder:
        return this.recordOrder(ctx, scope, call);
      case ToolReadOrders:
        return this.readOrders(ctx, scope, call);
      default:
        throw new Error(`${ErrUnknownTool.message}: ${call.tool}`);
    }
  }

  /**
   * Answers what the catalogue charges, or what it does have that is close.
   *
   * **A product that is not in the catalogue is a successful answer, not an
   * error.** The tool found out something true — the workspace does not sell
   * that — and a model that receives it as data can ask the customer what they
   * meant; a model that receives it as a failure tends to retry with a name it
   * invents. The JSON says which of the three cases it is.
   */
  private async findProduct(ctx: unknown, scope: Scope, call: Call): Promise<Result> {
    const args = parseArguments<FindArguments>(call.arguments, (raw) => ({
      product: typeof raw.product === "string" ? raw.product : "",
    }));

    const lookup = await this.requireOrders().lookup(ctx, scope, args.product);

    const payload: Record<string, unknown> = {
      ditemukan: lookup.found !== null,
      kandidat: productList(lookup.candidates),
      keterangan: explain(lookup),
      satuan_uang: "rupiah",
    };

    let summary = `Katalog: ${JSON.stringify(args.product.trim())} tidak ada`;
    if (lookup.found) {
      payload.produk = productPayload(lookup.found);
      summary = `Harga ${lookup.found.name}: ${rupiah(productPriceIdr(lookup.found))}`;
    }

    return { content: JSON.stringify(payload), summary };
  }

  /**
   * Writes the order and answers with what was recorded.
   *
   * The answer carries the order's code and its total, which is what the Bolu
   * composes the invoice from. It does not carry the price per line as something
   * the model may change: the model reads them, and the numbers it quotes come
   * from here rather than from its own arithmetic.
   */
  private async recordOrder(ctx: unknown, scope: Scope, call: Call): Promise<Result> {
    const args = parseArguments<RecordArguments>(call.arguments, (raw) => ({
      customer_name: typeof raw.customer_name === "string" ? raw.customer_name : "",
      customer_phone: typeof raw.customer_phone === "string" ? raw.customer_phone : "",
      note: typeof raw.note === "string" ? raw.note : "",
      items: Array.isArray(raw.items)
        ? raw.items.map((entry: unknown) => {
            const item = (entry ?? {}) as Record<string, unknown>;
            return {
              product: typeof item.product === "string" ? item.product : "",
              quantity: typeof item.quantity === "number" ? item.quantity : 0,
              note: typeof item.note === "string" ? item.note : "",
            };
          })
        : [],
    }));

    const items: NewItem[] = args.items.map((item) => ({
      product: item.product,
      quantity: item.quantity,
      note: item.note,
      description: "",
    }));

    // A refusal is returned as an error rather than as a result, and that is the
    // runtime's own convention: the workflow hands a failed tool call back to
    // the model as something it can read and react to. Nothing was written: the
    // service refuses before the first insert, so a partial order cannot exist
    // and a second attempt is a fresh one.
    const order = await this.requireOrders().record(ctx, scope, {
      customerPhone: args.customer_phone,
      customerName: args.customer_name,
      note: args.note,
      agentId: call.scope.agentId ?? "",
      taskId: call.scope.taskId ?? "",
      items,
    });

    const payload = {
      order_id: order.id,
      kode: order.code,
      status: order.status,
      total_rupiah: orderTotalIdr(order),
      pelanggan: {
        nama: order.customer.name,
        nomor: order.customer.phone,
      },
      baris: lineList(order),
    };

    return {
      content: JSON.stringify(payload),
      summary: `Pesanan ${order.code} dicatat: ${order.lines.length} baris, ${rupiah(orderTotalIdr(order))}`,
    };
  }

  /** Answers what has been ordered. */
  private async readOrders(ctx: unknown, scope: Scope, call: Call): Promise<Result> {
    const args = parseArguments<ReadArguments>(call.arguments, (raw) => ({
      customer_phone: typeof raw.customer_phone === "string" ? raw.customer_phone : "",
      limit: typeof raw.limit === "number" ? raw.limit : 0,
    }));

    const orders = await this.requireOrders().orders(ctx, scope, {
      customerPhone: args.customer_phone,
      limit: args.limit,
    });

    const listed = orders.map((order) => ({
      kode: order.code,
      status: order.status,
      total_rupiah: orderTotalIdr(order),
      dibuat: formatDay(order.createdAt),
      pelanggan: order.customer.name,
      baris: lineList(order),
    }));

    return {
      content: JSON.stringify({ pesanan: listed, jumlah: listed.length }),
      summary: `${listed.length} pesanan terbaca`,
    };
  }

  private requireOrders(): Service {
    if (this.orders === null) {
      throw new Error(`${ErrInvalidInput.message}: the order module is not configured`);
    }
    return this.orders;
  }
}

// ---------------------------------------------------------------- arguments

/** What the lookup tool accepts. */
interface FindArguments {
  product: string;
}

/** What the recording tool accepts. */
interface RecordArguments {
  customer_name: string;
  customer_phone: string;
  note: string;
  items: { product: string; quantity: number; note: string }[];
}

/** What the read tool accepts. */
interface ReadArguments {
  customer_phone: string;
  limit: number;
}

/**
 * Parses a tool call's arguments, answering ErrSchemaViolation for anything that
 * is not a JSON object, as `json.Unmarshal` does.
 */
function parseArguments<T>(raw: string, read: (value: Record<string, unknown>) => T): T {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch (err) {
    throw new Error(`${ErrSchemaViolation.message}: ${errorMessage(err)}`);
  }

  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new Error(`${ErrSchemaViolation.message}: the arguments are not a JSON object`);
  }

  return read(parsed as Record<string, unknown>);
}

// ---------------------------------------------------------------- rendering

/** Says which of the three answers this is, in a sentence a model can act on. */
function explain(lookup: Lookup): string {
  if (lookup.found) {
    return "Produk ini ada di katalog. Harga di atas per satuan; jangan mengubahnya.";
  }
  if (lookup.candidates.length > 0) {
    return "Produk dengan nama itu tidak ada. Ada kandidat di daftar kandidat: tanyakan ke pelanggan maksudnya yang mana, jangan menebak harga.";
  }
  return "Produk itu tidak ada di katalog workspace, dan tidak ada kandidat yang mirip. Tanyakan ke pelanggan atau ke pemilik usaha; jangan mengarang harga.";
}

/** Renders one product for the model, in rupiah and in the unit it is sold by. */
function productPayload(product: Product): Record<string, unknown> {
  return {
    nama: product.name,
    sku: product.sku,
    satuan: product.unit,
    harga_rupiah: productPriceIdr(product),
  };
}

/** Renders candidates. */
function productList(products: Product[]): Record<string, unknown>[] {
  return products.map(productPayload);
}

/** Renders an order's lines. */
function lineList(order: Order): Record<string, unknown>[] {
  return order.lines.map((line) => ({
    produk: line.description,
    jumlah: line.quantity,
    satuan: line.unit,
    harga_satuan_rupiah: linePriceIdr(line),
    total_rupiah: lineTotalIdr(line),
  }));
}

/** Formats a time the way Go's `Format("2006-01-02")` does. */
function formatDay(at: Date): string {
  return at.toISOString().slice(0, 10);
}
