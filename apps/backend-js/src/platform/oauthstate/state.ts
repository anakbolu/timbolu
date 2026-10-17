/**
 * Signed values for the integration flows: the OAuth `state` and the webhook
 * reference.
 *
 * Ported from apps/backend/internal/platform/oauthstate/state.go. The encoding
 * is byte-compatible with the Go deployment, because the two share
 * `OAUTH_STATE_KEY` during a cutover: a state minted by Go must verify here and
 * a state minted here must verify in Go.
 *
 * The value is
 *
 *     base64url(payload JSON) + "." + base64url(HMAC-SHA256(key, payload))
 *
 * both base64url *raw* (RFC 4648 §5, no `=` padding), the HMAC rendered the
 * same way. The payload is a compact JSON object with the keys in Go's struct
 * declaration order — `p`, `w`, `u`, `a`, `n`, `r`, `e` — and `r` is omitted
 * when empty, exactly as `omitempty` does.
 *
 * Being signed is not the same as being single-use, and this codec does not
 * pretend otherwise: replay within the value's life is refused by the state
 * store the integration module consumes the nonce from, and expiry is refused by
 * the flow (`expired`) after the signature checks out. Both live there in Go
 * too; moving either into the codec would change what a minted value means.
 */

import { createHmac, timingSafeEqual } from "node:crypto";

/** The shortest signing key accepted. A guessable key means a forged callback. */
const MIN_KEY_SIZE = 32;

const STATE_PURPOSE = "oauth-state";
const REF_PURPOSE = "webhook";

/** What the authorization flow carries between the app and the provider. */
export interface OAuthState {
  workspaceId: string;
  userId: string;
  app: string;
  nonce: string;
  returnTo: string;
  expiresAt: Date;
}

/** The connection a webhook URL belongs to. */
export interface WebhookRef {
  workspaceId: string;
  integrationId: string;
  app: string;
}

/**
 * The state the provider sent back is not one this deployment issued, or was
 * already spent, or has expired. The message is the Go sentinel's, verbatim, so
 * a caller that switches on it keeps working.
 */
export class InvalidStateError extends Error {
  constructor(detail?: string) {
    super(detail ? `integration: the authorization state is not valid: ${detail}` : "integration: the authorization state is not valid");
    this.name = "InvalidStateError";
  }
}

/** The webhook reference was not signed here, or was signed for another purpose. */
export class InvalidSignatureError extends Error {
  constructor(detail?: string) {
    super(detail ? `integration: the webhook signature is not valid: ${detail}` : "integration: the webhook signature is not valid");
    this.name = "InvalidSignatureError";
  }
}

/** True when a state failure is the "not ours" kind, mirroring `errors.Is`. */
export function isInvalidState(err: unknown): err is InvalidStateError {
  return err instanceof InvalidStateError;
}

/** True when a reference failure is the "not ours" kind. */
export function isInvalidSignature(err: unknown): err is InvalidSignatureError {
  return err instanceof InvalidSignatureError;
}

/** The payload that actually travels, keyed as the Go struct tags key it. */
interface Payload {
  p: string;
  w: string;
  u: string;
  a: string;
  n: string;
  r?: string;
  e: number;
}

/** What a webhook URL carries. */
interface RefPayload {
  w: string;
  i: string;
  a: string;
  p: string;
}

/**
 * Signs and verifies state values with an HMAC.
 *
 * An absent key is not an error: a deployment that has not registered an OAuth
 * application simply cannot start a connection flow, and the screen says so
 * rather than the process refusing to start. A key that is present and too
 * short is a mistake, and is reported as one.
 */
export class Codec {
  private constructor(private readonly key: Buffer) {}

  /** Builds a codec from a configured key; `null` when no key is configured. */
  static create(key: string | null | undefined): Codec | null {
    const trimmed = (key ?? "").trim();
    if (trimmed === "") {
      return null;
    }
    if (trimmed.length < MIN_KEY_SIZE) {
      throw new Error(`oauthstate: the signing key must be at least ${MIN_KEY_SIZE} characters`);
    }
    return new Codec(Buffer.from(trimmed, "utf8"));
  }

  /** Renders a state value: the payload, then its signature. */
  sign(state: OAuthState): string {
    const document = marshalPayload({
      p: STATE_PURPOSE,
      w: state.workspaceId,
      u: state.userId,
      a: state.app,
      n: state.nonce,
      ...(state.returnTo ? { r: state.returnTo } : {}),
      e: Math.floor(state.expiresAt.getTime() / 1000),
    });

    const encoded = base64urlEncode(Buffer.from(document, "utf8"));
    return `${encoded}.${this.signature(encoded)}`;
  }

  /**
   * Reads a value back, refusing anything this codec did not sign.
   *
   * The signature is checked before the payload is parsed, so a forged value is
   * refused without the decoder ever seeing attacker-controlled structure.
   */
  verify(value: string): OAuthState {
    const cut = value.indexOf(".");
    if (cut < 0) {
      throw new InvalidStateError();
    }
    const encoded = value.slice(0, cut);
    const signature = value.slice(cut + 1);
    if (!encoded || !signature) {
      throw new InvalidStateError();
    }

    if (!safeEqual(this.signature(encoded), signature)) {
      throw new InvalidStateError("the signature does not match");
    }

    const document = base64urlDecode(encoded);
    if (document === null) {
      throw new InvalidStateError("it is not readable");
    }

    let decoded: Payload;
    try {
      decoded = JSON.parse(document) as Payload;
    } catch {
      throw new InvalidStateError("it is not readable");
    }

    const workspaceId = parseUUID(decoded.w);
    if (!workspaceId) {
      throw new InvalidStateError("it names no workspace");
    }
    const userId = parseUUID(decoded.u);
    if (!userId) {
      throw new InvalidStateError("it names no person");
    }
    if (decoded.p !== STATE_PURPOSE) {
      throw new InvalidStateError("it was not signed as an authorization state");
    }
    if (!decoded.a || !decoded.n) {
      throw new InvalidStateError("it is incomplete");
    }

    return {
      workspaceId,
      userId,
      app: decoded.a,
      nonce: decoded.n,
      returnTo: decoded.r ?? "",
      expiresAt: new Date(decoded.e * 1000),
    };
  }

  /** Renders a webhook reference as the value that travels in a URL. */
  signRef(ref: WebhookRef): string {
    const document = marshalRefPayload({
      w: ref.workspaceId,
      i: ref.integrationId,
      a: ref.app,
      p: REF_PURPOSE,
    });

    const encoded = base64urlEncode(Buffer.from(document, "utf8"));
    return `${encoded}.${this.signature(encoded)}`;
  }

  /**
   * Reads a webhook reference back, refusing anything this codec did not sign
   * or that was signed for another purpose.
   */
  verifyRef(value: string): WebhookRef {
    const cut = value.indexOf(".");
    if (cut < 0) {
      throw new InvalidSignatureError();
    }
    const encoded = value.slice(0, cut);
    const signature = value.slice(cut + 1);
    if (!encoded || !signature) {
      throw new InvalidSignatureError();
    }
    if (!safeEqual(this.signature(encoded), signature)) {
      throw new InvalidSignatureError("the signature does not match");
    }

    const document = base64urlDecode(encoded);
    if (document === null) {
      throw new InvalidSignatureError("it is not readable");
    }

    let decoded: RefPayload;
    try {
      decoded = JSON.parse(document) as RefPayload;
    } catch {
      throw new InvalidSignatureError("it is not readable");
    }
    if (decoded.p !== REF_PURPOSE) {
      throw new InvalidSignatureError("it was not signed as a webhook reference");
    }

    const workspaceId = parseUUID(decoded.w);
    if (!workspaceId) {
      throw new InvalidSignatureError("it names no workspace");
    }
    const integrationId = parseUUID(decoded.i);
    if (!integrationId) {
      throw new InvalidSignatureError("it names no connection");
    }
    if (!decoded.a) {
      throw new InvalidSignatureError("it names no app");
    }

    return { workspaceId, integrationId, app: decoded.a };
  }

  /** The HMAC over the encoded payload, rendered base64url without padding. */
  private signature(encoded: string): string {
    return base64urlEncode(createHmac("sha256", this.key).update(encoded, "utf8").digest());
  }
}

/**
 * Reports whether the state is past its life.
 *
 * `!ExpiresAt.After(now)` in Go: a state that expires exactly now is expired.
 */
export function expired(state: OAuthState, now: Date): boolean {
  return state.expiresAt.getTime() <= now.getTime();
}

/** Constant-time comparison of two base64url strings. */
function safeEqual(a: string, b: string): boolean {
  const left = Buffer.from(a, "utf8");
  const right = Buffer.from(b, "utf8");
  if (left.length !== right.length) {
    return false;
  }
  return timingSafeEqual(left, right);
}

function base64urlEncode(value: Buffer): string {
  return value.toString("base64url");
}

/** Decodes base64url, returning null for anything that is not one. */
function base64urlDecode(value: string): string | null {
  if (!/^[A-Za-z0-9_-]*$/.test(value)) {
    return null;
  }
  try {
    return Buffer.from(value, "base64url").toString("utf8");
  } catch {
    return null;
  }
}

const UUID_SHAPE = /^(?:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|[0-9a-f]{32})$/i;
const UUID_URN = /^urn:uuid:([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$/i;
const UUID_BRACED = /^\{([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\}$/i;

/**
 * Parses the forms Go's `uuid.Parse` accepts — canonical, 32-hex, `urn:uuid:`
 * and braced — and returns the canonical lowercase form, or null.
 */
function parseUUID(value: unknown): string | null {
  if (typeof value !== "string") {
    return null;
  }
  const urn = UUID_URN.exec(value);
  if (urn) {
    return urn[1].toLowerCase();
  }
  const braced = UUID_BRACED.exec(value);
  if (braced) {
    return braced[1].toLowerCase();
  }
  if (!UUID_SHAPE.test(value)) {
    return null;
  }
  const hex = value.replace(/-/g, "").toLowerCase();
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

/**
 * The state payload as Go's `json.Marshal` renders a struct: keys in
 * declaration order, no whitespace, `<`, `>` and `&` escaped as `\u003c`,
 * `\u003e` and `\u0026`, and `r` absent when empty.
 *
 * The order and the escaping are what make a value minted here identical to one
 * minted by the Go deployment.
 */
function marshalPayload(payload: Payload): string {
  const parts = [
    `"p":${jsonString(payload.p)}`,
    `"w":${jsonString(payload.w)}`,
    `"u":${jsonString(payload.u)}`,
    `"a":${jsonString(payload.a)}`,
    `"n":${jsonString(payload.n)}`,
  ];
  if (payload.r) {
    parts.push(`"r":${jsonString(payload.r)}`);
  }
  parts.push(`"e":${payload.e}`);
  return `{${parts.join(",")}}`;
}

/** The webhook reference payload, keyed as the Go struct tags key it. */
function marshalRefPayload(payload: RefPayload): string {
  return `{${[
    `"w":${jsonString(payload.w)}`,
    `"i":${jsonString(payload.i)}`,
    `"a":${jsonString(payload.a)}`,
    `"p":${jsonString(payload.p)}`,
  ].join(",")}}`;
}

/** A JSON string literal with Go's escaping rules. */
function jsonString(value: string): string {
  let out = '"';
  for (const char of value) {
    switch (char) {
      case '"':
        out += '\\"';
        continue;
      case "\\":
        out += "\\\\";
        continue;
      case "\n":
        out += "\\n";
        continue;
      case "\r":
        out += "\\r";
        continue;
      case "\t":
        out += "\\t";
        continue;
      case "<":
        out += "\\u003c";
        continue;
      case ">":
        out += "\\u003e";
        continue;
      case "&":
        out += "\\u0026";
        continue;
      default:
        break;
    }
    const code = char.codePointAt(0) ?? 0;
    if (code < 0x20) {
      out += `\\u${code.toString(16).padStart(4, "0")}`;
    } else {
      out += char;
    }
  }
  return `${out}"`;
}
