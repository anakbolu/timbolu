/**
 * Adapts Duitku's payment gateway to the billing module's payment port.
 *
 * Ported from apps/backend/internal/platform/payment/duitku/duitku.go.
 *
 * Two things about Duitku decide how this is written:
 *
 *   - it is a hosted payment page, not a charge API. The inquiry answers a
 *     `paymentUrl` a person opens and a `vaNumber` for the methods that have one,
 *     so what crosses the port is the instructions a person acts on rather than
 *     a link or a number.
 *   - both directions are authenticated with the same primitive: HMAC-SHA256
 *     over a documented string, with the merchant's API key as the secret. The
 *     signature is therefore the whole security of the callback, and it is
 *     compared in constant time.
 */

import { createHmac, timingSafeEqual } from "node:crypto";

/**
 * The billing module's shapes this adapter speaks.
 *
 * The Go file imports `modules/billing/domain`; the module tree is ported
 * separately, so the handful of fields this adapter reads are declared here.
 * They are structural: a `billing/domain/invoice.ts` value satisfies them
 * without an adapter.
 */
export interface Invoice {
  id: string;
  /** `subscription` or `topup`, matching `billing_invoices.kind`. */
  kind: string;
  amountIDR: number;
}

export interface Customer {
  email: string;
  name: string;
}

export interface Charge {
  /** The provider's own id for the payment, which the callback names. */
  reference: string;
  instructions: string;
  expiresAt: Date;
}

export interface Delivery {
  provider: string;
  body: Uint8Array;
  headers: Record<string, string>;
}

export interface Payment {
  reference: string;
  amountIDR: number;
  paid: boolean;
  expired: boolean;
  /** The zero time in Go is `null` here. */
  paidAt: Date | null;
}

/** `domain.KindTopup`: the invoice kind that buys tokens. */
const KIND_TOPUP = "topup";

/** The provider key invoices record and callbacks arrive under. */
export const Name = "duitku";

/**
 * Hosts. Which one is used is configuration rather than a code change, because
 * a sandbox key against the production host is a charge on somebody's card.
 */
export const ProductionBaseURL = "https://passport.duitku.com";
export const SandboxBaseURL = "https://sandbox.duitku.com";

/** Where a transaction is asked for. */
export const InquiryPath = "/webapi/api/merchant/v2/inquiry";

/**
 * Payment methods this adapter offers by name. They are Duitku's own
 * two-letter codes, and the list is short on purpose: the codes change as banks
 * are added, and the deployment names the one it sells.
 */
export const MethodBCAVA = "BC";
export const MethodMaybankVA = "VA";
export const MethodPermataVA = "BT";
export const MethodQRIS = "SP";

/**
 * What a deployment gets when it names none. A virtual account is what the
 * product's customers already use, and a number they know how to pay is worth
 * more than an optimal fee.
 */
export const DefaultMethod = MethodBCAVA;

/**
 * How long a payment stays payable when nothing says otherwise.
 *
 * A day is long enough for a transfer made after work and short enough that a
 * workspace still inside its grace window can be asked again — and it is well
 * under the grace window, so an expired link is never the reason a workspace
 * stopped.
 */
export const DefaultExpiryMinutes = 1440;

/** The Go client's own bound on one inquiry. */
const DEFAULT_HTTP_TIMEOUT_MS = 20_000;

/** The largest inquiry answer this adapter reads, in bytes. */
const MAX_RESPONSE_BYTES = 1 << 20;

/** What a deployment sets to take online payment. */
export interface Config {
  /** Identifies the project at Duitku. Empty means this deployment takes no online payment. */
  merchantCode: string;
  /** Signs the inquiry and verifies the callback. Empty is refused at startup. */
  apiKey: string;
  /** Selects Duitku's sandbox host. */
  sandbox: boolean;
  /** Which Duitku payment channel a person is offered. */
  paymentMethod: string;
  /** How long the payment stays payable. */
  expiryMinutes: number;
  /** Where Duitku posts the payment notification. */
  callbackURL: string;
  /** Where a person lands after the payment page. Empty omits it. */
  returnURL: string;
  /** Overrides Duitku's host, for a stand-in. */
  baseURL: string;
  /**
   * The equivalent of the Go `HTTPClient` field: a test or a deployment
   * verifying its own wiring injects one. `undefined` takes the global fetch
   * with the default timeout.
   */
  fetch?: typeof fetch;
}

/** The Duitku-backed payment gateway. */
export class Gateway {
  private readonly cfg: Config;
  private readonly doFetch: typeof fetch;

  private constructor(cfg: Config, doFetch: typeof fetch) {
    this.cfg = cfg;
    this.doFetch = doFetch;
  }

  /**
   * Builds the gateway.
   *
   * A missing merchant code or key is refused here rather than at the first
   * charge, so a deployment that meant to take payment but mistyped a value
   * finds out at startup. A missing callback URL is refused for the same
   * reason: Duitku would accept the transaction and then have nowhere to tell
   * us it was paid, which is the one failure this whole path exists to avoid.
   */
  static create(cfg: Config): Gateway {
    if (cfg.merchantCode.trim() === "") {
      throw new Error("duitku: DUITKU_MERCHANT_CODE is required");
    }
    if (cfg.apiKey.trim() === "") {
      throw new Error("duitku: DUITKU_API_KEY is required");
    }
    if (cfg.callbackURL.trim() === "") {
      throw new Error("duitku: DUITKU_CALLBACK_URL is required, or no payment can be confirmed");
    }

    const resolved: Config = {
      ...cfg,
      paymentMethod: cfg.paymentMethod.trim() === "" ? DefaultMethod : cfg.paymentMethod,
      expiryMinutes: cfg.expiryMinutes <= 0 ? DefaultExpiryMinutes : cfg.expiryMinutes,
    };

    return new Gateway(resolved, cfg.fetch ?? fetch);
  }

  /** Implements the port. */
  name(): string {
    return Name;
  }

  /**
   * Asks Duitku for a payment for one invoice.
   *
   * The order id is derived from the invoice rather than random, so a charge
   * whose response was lost is the same transaction when it is tried again —
   * and Duitku refuses a repeated order id, which turns a lost response into a
   * readable refusal instead of a second payment nobody can reconcile.
   */
  async charge(invoice: Invoice, customer: Customer): Promise<Charge> {
    const orderID = OrderID(invoice.id, invoice.kind);
    const product = invoice.kind === KIND_TOPUP ? "Paket tambahan token Bolu" : "Langganan Bolu";

    const request: InquiryRequest = {
      merchantCode: this.cfg.merchantCode,
      paymentAmount: invoice.amountIDR,
      paymentMethod: this.cfg.paymentMethod,
      merchantOrderId: orderID,
      productDetails: product,
      customerVaName: customerName(customer),
      email: customer.email,
      itemDetails: [{ name: product, price: invoice.amountIDR, quantity: 1 }],
      callbackUrl: this.cfg.callbackURL,
      signature: this.sign(orderID, invoice.amountIDR),
      expiryPeriod: this.cfg.expiryMinutes,
    };
    // `omitempty`: Go drops these two when they are empty.
    if (this.cfg.returnURL !== "") request.returnUrl = this.cfg.returnURL;

    const response = await this.post(InquiryPath, JSON.stringify(request));
    const payload = await readLimited(response, MAX_RESPONSE_BYTES);

    let answer: InquiryResponse;
    try {
      answer = JSON.parse(payload) as InquiryResponse;
    } catch (err) {
      throw new Error(`duitku: decode inquiry response: ${(err as Error).message}`);
    }

    // Duitku answers 200 for an inquiry it refused, with the reason in the
    // body, so the status code alone is not the verdict — and a charge read as
    // successful would leave a payment page that does not exist.
    if (response.status >= 300 || (answer.statusCode ?? "").trim() !== "00") {
      let message = (answer.statusMessage ?? "").trim();
      if (message === "") message = payload.trim();
      throw new Error(`duitku: inquiry refused (${answer.statusCode}): ${message}`);
    }
    if ((answer.paymentUrl ?? "").trim() === "" && (answer.vaNumber ?? "").trim() === "") {
      throw new Error("duitku: the inquiry carries neither a payment page nor a virtual account");
    }

    // The reference recorded is the *order id*, not Duitku's own reference, and
    // that is the one thing here that has to be right: the callback names
    // `merchantOrderId`, so a settlement is looked up by it.
    return {
      reference: orderID,
      instructions: instructions(answer),
      expiresAt: new Date(Date.now() + this.cfg.expiryMinutes * 60_000),
    };
  }

  /**
   * Checks a callback's signature and says what happened.
   *
   * The signature is Duitku's own: HMAC-SHA256 over
   * `merchantCode + amount + merchantOrderId` with the API key. Recomputing it
   * is the only thing standing between a stranger and a paid package, so it is
   * compared in constant time and a delivery that fails it is refused rather
   * than parsed hopefully.
   */
  verify(delivery: Delivery): Payment {
    const payload = parseCallback(delivery);

    if (payload.merchantCode !== this.cfg.merchantCode) {
      // A callback for another project is not ours to act on, whatever its
      // signature says: the key is per project, so this is the one case a
      // correct signature on a foreign payload would otherwise be accepted.
      throw new Error("duitku: the callback names another merchant");
    }

    const expected = this.verifySignature(payload.merchantOrderId, payload.amount);
    if (!constantTimeEqual(expected, payload.signature.trim())) {
      throw new Error("duitku: the callback signature does not match");
    }

    const amountText = payload.amount.trim();
    const amount = parseDecimal(amountText);
    if (amount === null) {
      throw new Error(`duitku: the callback amount ${JSON.stringify(payload.amount)} is not a number`);
    }

    const payment: Payment = {
      reference: payload.merchantOrderId,
      amountIDR: amount,
      paid: false,
      expired: false,
      paidAt: null,
    };

    // `00` is a paid payment; `01` is a failed one and anything else is a
    // status this build does not know. A payment link that ran out arrives as
    // `01` too, and the sweep closes it as expired.
    if (payload.resultCode.trim() === "00") {
      payment.paid = true;
      payment.paidAt = settledAt(payload.settlementDate);
    }

    return payment;
  }

  /** Sends one JSON request. */
  private async post(path: string, body: string): Promise<Response> {
    try {
      const host =
        this.cfg.baseURL.trim() !== ""
          ? this.cfg.baseURL.replace(/\/+$/, "")
          : this.cfg.sandbox
            ? SandboxBaseURL
            : ProductionBaseURL;
      return await this.doFetch(host + path, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Accept: "application/json",
        },
        body,
        signal: AbortSignal.timeout(DEFAULT_HTTP_TIMEOUT_MS),
      });
    } catch (err) {
      throw new Error(`duitku: ${(err as Error).message}`);
    }
  }

  /** The signature of an inquiry: `merchantCode + merchantOrderId + paymentAmount`. */
  private sign(orderID: string, amount: number): string {
    return createHmac("sha256", this.cfg.apiKey)
      .update(this.cfg.merchantCode + orderID + String(amount), "utf8")
      .digest("hex");
  }

  /**
   * The signature of a callback: `merchantCode + amount + merchantOrderId`.
   *
   * The two are different strings, and that is Duitku's own asymmetry rather
   * than a mistake here: the inquiry signs the amount from our side and the
   * callback signs the amount from theirs, and the order of the parts is what
   * their documentation states.
   */
  private verifySignature(orderID: string, amount: string): string {
    return createHmac("sha256", this.cfg.apiKey)
      .update(this.cfg.merchantCode + amount + orderID, "utf8")
      .digest("hex");
  }
}

/** Duitku's own shape for a transaction request. */
interface InquiryRequest {
  merchantCode: string;
  paymentAmount: number;
  paymentMethod: string;
  merchantOrderId: string;
  productDetails: string;
  additionalParam?: string;
  customerVaName: string;
  email: string;
  itemDetails?: ItemDetail[];
  callbackUrl: string;
  returnUrl?: string;
  signature: string;
  expiryPeriod: number;
}

interface ItemDetail {
  name: string;
  price: number;
  quantity: number;
}

/** The part of Duitku's answer this adapter reads. */
interface InquiryResponse {
  merchantCode: string;
  reference: string;
  paymentUrl: string;
  vaNumber: string;
  amount: string;
  statusCode: string;
  statusMessage: string;
}

/**
 * What a person acts on.
 *
 * A Duitku transaction usually has both: a payment page that lists every
 * channel and a virtual account number for the channel that was asked for. Both
 * are given, because the page is what reaches somebody who wants to choose and
 * the number is what reaches somebody who already knows.
 */
function instructions(answer: InquiryResponse): string {
  const vaNumber = (answer.vaNumber ?? "").trim();
  const paymentURL = (answer.paymentUrl ?? "").trim();
  if (vaNumber !== "" && paymentURL !== "") {
    return `Buka ${answer.paymentUrl} atau transfer ke virtual account ${answer.vaNumber}`;
  }
  if (paymentURL !== "") {
    return "Buka " + answer.paymentUrl;
  }
  return "Transfer ke virtual account " + answer.vaNumber;
}

/** The name the bank side shows, which Duitku caps at twenty characters. */
function customerName(customer: Customer): string {
  let name = (customer.name ?? "").trim();
  if (name === "") name = "Pemilik usaha";
  const runes = Array.from(name);
  return runes.length > 20 ? runes.slice(0, 20).join("") : name;
}

/** The body Duitku posts when a payment happens. */
interface Callback {
  merchantCode: string;
  amount: string;
  merchantOrderId: string;
  resultCode: string;
  reference: string;
  paymentCode: string;
  settlementDate: string;
  signature: string;
}

/** Reads the form Duitku posts. */
function parseCallback(delivery: Delivery): Callback {
  // The body is a urlencoded form, so it is parsed as one. The header map is
  // not consulted: Duitku sends everything in the body and nothing that has to
  // be read from a header.
  const raw = new TextDecoder().decode(delivery.body);
  const values = parseQuery(raw);

  const payload: Callback = {
    merchantCode: values.get("merchantCode") ?? "",
    amount: values.get("amount") ?? "",
    merchantOrderId: values.get("merchantOrderId") ?? "",
    resultCode: values.get("resultCode") ?? "",
    reference: values.get("reference") ?? "",
    paymentCode: values.get("paymentCode") ?? "",
    settlementDate: values.get("settlementDate") ?? "",
    signature: values.get("signature") ?? "",
  };

  if (payload.merchantOrderId === "" || payload.signature === "") {
    throw new Error("duitku: the callback names no order");
  }
  if (payload.amount === "") {
    throw new Error("duitku: the callback states no amount");
  }

  return payload;
}

/**
 * Parses a urlencoded body the way Go's `url.ParseQuery` does: a semicolon is
 * a separator Go refuses, and an invalid percent escape is an error rather
 * than something to pass through.
 */
function parseQuery(raw: string): URLSearchParams {
  if (raw.includes(";")) {
    throw new Error("duitku: decode callback: invalid semicolon separator in query");
  }
  if (/%(?![0-9A-Fa-f]{2})/.test(raw)) {
    throw new Error("duitku: decode callback: invalid URL escape");
  }
  try {
    return new URLSearchParams(raw);
  } catch (err) {
    throw new Error(`duitku: decode callback: ${(err as Error).message}`);
  }
}

/**
 * Constant-time comparison, and false when the lengths differ — what Go's
 * `hmac.Equal` answers.
 */
function constantTimeEqual(a: string, b: string): boolean {
  const left = Buffer.from(a, "utf8");
  const right = Buffer.from(b, "utf8");
  if (left.length !== right.length) return false;
  return timingSafeEqual(left, right);
}

/**
 * The reference Duitku echoes back, and therefore the one the callback names.
 *
 * Duitku caps the field at fifty characters, and a uuid plus a prefix and a
 * suffix is forty.
 */
export function OrderID(invoiceID: string, kind: string): string {
  const suffix = kind === KIND_TOPUP ? "t" : "m";
  return "bolu-" + invoiceID + "-" + suffix;
}

/**
 * Reads the settlement date, which Duitku sends as a day.
 *
 * Returns the zero time as `null` when nothing parses.
 */
function settledAt(raw: string): Date | null {
  const text = (raw ?? "").trim();
  if (text === "") return null;

  const day = /^(\d{4})-(\d{2})-(\d{2})$/.exec(text);
  if (day) {
    return new Date(Date.UTC(Number(day[1]), Number(day[2]) - 1, Number(day[3])));
  }

  const stamp = /^(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(Z|[+-]\d{2}:\d{2})?$/.exec(text);
  if (stamp) {
    const [, y, mo, d, h, mi, s, zone] = stamp;
    const millis = Date.UTC(
      Number(y),
      Number(mo) - 1,
      Number(d),
      Number(h),
      Number(mi),
      Number(s),
    );
    if (!zone || zone === "Z") return new Date(millis);
    const sign = zone.startsWith("-") ? -1 : 1;
    const [zh, zm] = zone.slice(1).split(":");
    const offsetMinutes = sign * (Number(zh) * 60 + Number(zm));
    return new Date(millis - offsetMinutes * 60_000);
  }

  return null;
}

/** Parses a base-10 integer, or null when the text is not one. */
function parseDecimal(text: string): number | null {
  if (!/^[+-]?\d+$/.test(text)) return null;
  const value = Number(text);
  if (!Number.isSafeInteger(value)) return null;
  return value;
}

/** Reads a response body, refusing to hold more than `limit` bytes. */
async function readLimited(response: Response, limit: number): Promise<string> {
  const buffer = new Uint8Array(await response.arrayBuffer());
  const bytes = buffer.byteLength > limit ? buffer.subarray(0, limit) : buffer;
  return new TextDecoder().decode(bytes);
}

/** What a caller gets when a deployment has no gateway. */
export const ErrNotConfigured = new Error("duitku: not configured");
