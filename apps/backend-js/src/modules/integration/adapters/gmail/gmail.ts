/**
 * The native adapter for Gmail: the tools a Bolu uses to read and send mail,
 * and the OAuth endpoints that connect an account.
 *
 * It speaks the Gmail REST API directly rather than through a wrapper library,
 * because a tool call is one HTTP request and what matters is that a refusal
 * from Google arrives as a reason rather than as a retry.
 *
 * Two rules are structural here. `gmail.create_draft` and `gmail.send` are
 * different tools with different labels, so writing a draft never leaves the
 * workspace and sending always waits for a person. And nothing in this file
 * decides whether an action is allowed: the label does, upstream.
 *
 * Ported 1:1 from internal/modules/integration/adapters/gmail/gmail.go.
 */

import { ErrInvalidInput, LabelRead, LabelWriteExternal, LabelWriteInternal } from "../../domain/integration.ts";
import { ErrRefreshFailed, type OAuthTokens } from "../../domain/oauth.ts";
import {
  ErrUnknownTool,
  TriggerPolling,
  toolName,
  type Adapter,
  type Call,
  type Result,
  type ToolSpec,
  type TriggerSpec,
} from "../../domain/adapter.ts";

/** The application key, and the prefix of every tool this adapter declares. */
export const App = "gmail";

/** Google's endpoints. */
const authEndpoint = "https://accounts.google.com/o/oauth2/v2/auth";
const tokenEndpoint = "https://oauth2.googleapis.com/token";
const apiBase = "https://gmail.googleapis.com/gmail/v1";

/** The narrowest set that makes the read tools work. */
export const ScopeReadonly = "https://www.googleapis.com/auth/gmail.readonly";
export const ScopeSend = "https://www.googleapis.com/auth/gmail.send";
export const ScopeCompose = "https://www.googleapis.com/auth/gmail.compose";

/** Bounds what is read from Google. */
const maxBody = 4 << 20;

/** Bounds one attachment read. */
const maxAttachment = 1 << 20;

/** What the adapter needs. */
export interface Config {
  clientId: string;
  clientSecret: string;
  apiBase?: string;
  authEndpoint?: string;
  tokenEndpoint?: string;
  /** Milliseconds. Zero takes the default. */
  timeout?: number;
}

/** Google's own refusal, told apart from a transport failure. */
class TokenRefused extends Error {
  constructor(message: string) {
    super(`gmail: google refused the grant: ${message}`);
    this.name = "TokenRefused";
  }
}

/** The Gmail integration. */
export class GmailAdapter implements Adapter {
  private readonly clientId: string;
  private readonly clientSecret: string;
  private readonly apiBase: string;
  private readonly authUrl: string;
  private readonly tokenUrl: string;
  private readonly timeout: number;

  constructor(cfg: Config) {
    if (cfg.clientId.trim() === "" || cfg.clientSecret.trim() === "") {
      throw new Error("gmail: an OAuth client id and secret are required");
    }
    this.clientId = cfg.clientId;
    this.clientSecret = cfg.clientSecret;
    this.apiBase = cfg.apiBase?.trim() ? cfg.apiBase : apiBase;
    this.authUrl = cfg.authEndpoint?.trim() ? cfg.authEndpoint : authEndpoint;
    this.tokenUrl = cfg.tokenEndpoint?.trim() ? cfg.tokenEndpoint : tokenEndpoint;
    this.timeout = cfg.timeout && cfg.timeout > 0 ? cfg.timeout : 30_000;
  }

  /** The application key. */
  app(): string {
    return App;
  }

  /** What a connection asks for. */
  defaultScopes(): string[] {
    return [ScopeReadonly, ScopeCompose, ScopeSend];
  }

  /** What a Bolu can do with a connected mailbox. */
  tools(): ToolSpec[] {
    return [
      {
        name: toolName(App, "search"),
        label: LabelRead,
        description: "Cari email di kotak masuk dengan kata kunci Gmail",
        schema: `{
				"type": "object",
				"properties": {
					"q":     {"type": "string", "description": "Kata kunci, sintaks Gmail"},
					"limit": {"type": "integer", "minimum": 1, "maximum": 50}
				},
				"required": ["q"],
				"additionalProperties": false
			}`,
      },
      {
        name: toolName(App, "read_attachment"),
        label: LabelRead,
        description: "Baca isi satu lampiran email",
        schema: `{
				"type": "object",
				"properties": {
					"message_id":   {"type": "string"},
					"attachment_id": {"type": "string"}
				},
				"required": ["message_id", "attachment_id"],
				"additionalProperties": false
			}`,
      },
      {
        name: toolName(App, "create_draft"),
        label: LabelWriteInternal,
        description: "Buat draf email tanpa mengirimnya",
        schema: `{
				"type": "object",
				"properties": {
					"to":      {"type": "string"},
					"subject": {"type": "string"},
					"body":    {"type": "string"}
				},
				"required": ["to", "subject", "body"],
				"additionalProperties": false
			}`,
      },
      {
        name: toolName(App, "send"),
        label: LabelWriteExternal,
        description: "Kirim email ke penerima. Membutuhkan persetujuan manusia.",
        schema: `{
				"type": "object",
				"properties": {
					"to":      {"type": "string"},
					"subject": {"type": "string"},
					"body":    {"type": "string"}
				},
				"required": ["to", "subject", "body"],
				"additionalProperties": false
			}`,
      },
    ];
  }

  /**
   * Gmail pushes nothing without a Pub/Sub subscription, which is a
   * deployment's own infrastructure. So the trigger is a poll.
   */
  triggers(): TriggerSpec[] {
    return [
      {
        app: App,
        kind: TriggerPolling,
        event: "gmail.email_received",
        description: "Email baru masuk ke kotak masuk",
      },
    ];
  }

  // ------------------------------------------------------------------- OAuth

  /** Google's consent page for a state. */
  authCodeUrl(state: string, redirectUri: string): string {
    const query = encodeQuery([
      ["client_id", this.clientId],
      ["redirect_uri", redirectUri],
      ["response_type", "code"],
      ["scope", this.defaultScopes().join(" ")],
      ["state", state],
      // A refresh token is the whole point: without one the connection dies
      // when the access token does.
      ["access_type", "offline"],
      // Google only returns a refresh token when the consent screen is shown.
      ["prompt", "consent"],
    ]);
    return `${this.authUrl}?${query}`;
  }

  /** Trades a code for tokens. */
  async exchange(_ctx: unknown, code: string, redirectUri: string): Promise<OAuthTokens> {
    return this.token([
      ["grant_type", "authorization_code"],
      ["code", code],
      ["redirect_uri", redirectUri],
    ]);
  }

  /** Trades a refresh token for a new access token. */
  async refresh(_ctx: unknown, refreshToken: string): Promise<OAuthTokens> {
    try {
      return await this.token([
        ["grant_type", "refresh_token"],
        ["refresh_token", refreshToken],
      ]);
    } catch (err) {
      if (err instanceof TokenRefused) {
        return Promise.reject(new Error(`${ErrRefreshFailed.message}: ${err.message}`));
      }
      throw err;
    }
  }

  /** Reads the address the account belongs to. */
  async accountLabel(ctx: unknown, tokens: OAuthTokens): Promise<string> {
    const profile = await this.get<{ emailAddress?: string }>(
      ctx,
      tokens.accessToken,
      "/users/me/profile",
      [],
    );
    if (!profile.emailAddress || profile.emailAddress.trim() === "") {
      throw new Error("gmail: the account has no address");
    }
    return profile.emailAddress;
  }

  /** The token-endpoint exchange, for both a code and a refresh. */
  private async token(form: [string, string][]): Promise<OAuthTokens> {
    const body = encodeQuery([...form, ["client_id", this.clientId], ["client_secret", this.clientSecret]]);

    const response = await this.request(this.tokenUrl, {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body,
    });

    const text = await readCapped(response);

    if (response.status >= 400) {
      // Google reports a refused grant as 400 with an error code.
      throw new TokenRefused(`(status ${response.status}): ${truncate(text, 200)}`);
    }

    let decoded: { access_token?: string; refresh_token?: string; expires_in?: number; scope?: string };
    try {
      decoded = JSON.parse(text);
    } catch (err) {
      throw new Error(`gmail: token answer is not readable: ${message(err)}`);
    }

    const tokens: OAuthTokens = {
      accessToken: decoded.access_token ?? "",
      refreshToken: decoded.refresh_token ?? "",
      expiresAt: null,
      scopes: (decoded.scope ?? "").split(/\s+/).filter((field) => field !== ""),
    };
    if ((decoded.expires_in ?? 0) > 0) {
      tokens.expiresAt = new Date(Date.now() + (decoded.expires_in as number) * 1000 - 60_000);
    }
    return tokens;
  }

  // ------------------------------------------------------------------- tools

  /** Runs one tool. */
  async execute(ctx: unknown, call: Call): Promise<Result> {
    switch (call.tool) {
      case toolName(App, "search"):
        return this.search(ctx, call);
      case toolName(App, "read_attachment"):
        return this.readAttachment(ctx, call);
      case toolName(App, "create_draft"):
        return this.createDraft(ctx, call);
      case toolName(App, "send"):
        return this.send(ctx, call);
      default:
        throw new Error(`${ErrUnknownTool.message}: ${JSON.stringify(call.tool)}`);
    }
  }

  /**
   * Lists the messages matching a query.
   *
   * It returns ids and headers rather than bodies: a search that returned full
   * messages would spend a task's whole token budget on one call.
   */
  private async search(ctx: unknown, call: Call): Promise<Result> {
    let input: { q?: string; limit?: number };
    try {
      input = JSON.parse(call.arguments || "{}");
    } catch (err) {
      throw new Error(`gmail: read search arguments: ${message(err)}`);
    }

    const limit = !input.limit || input.limit <= 0 || input.limit > 50 ? 10 : input.limit;

    const listing = await this.get<{
      messages?: { id?: string; threadId?: string }[];
      resultSizeEstimate?: number;
    }>(ctx, call.token.accessToken, "/users/me/messages", [
      ["q", input.q ?? ""],
      ["maxResults", String(limit)],
    ]);

    const messages: Record<string, unknown>[] = [];
    for (const item of listing.messages ?? []) {
      const entry: Record<string, unknown> = { id: item.id ?? "", thread_id: item.threadId ?? "" };

      // The headers are what makes a search result readable. A message that
      // cannot be read is still listed: dropping it would hide that it exists.
      try {
        const detail = await this.get<{
          payload?: { headers?: { name?: string; value?: string }[] };
          snippet?: string;
        }>(ctx, call.token.accessToken, `/users/me/messages/${encodeURIComponent(item.id ?? "")}`, [
          ["format", "metadata"],
        ]);

        for (const header of detail.payload?.headers ?? []) {
          switch ((header.name ?? "").toLowerCase()) {
            case "subject":
              entry["subject"] = header.value ?? "";
              break;
            case "from":
              entry["from"] = header.value ?? "";
              break;
            case "date":
              entry["date"] = header.value ?? "";
              break;
            default:
              break;
          }
        }
        entry["snippet"] = detail.snippet ?? "";
      } catch {
        // A message that cannot be read is still listed.
      }

      messages.push(entry);
    }

    return {
      content: JSON.stringify({ messages, total: listing.resultSizeEstimate ?? 0 }),
      summary: `${messages.length} email ditemukan`,
    };
  }

  /**
   * Reads one attachment.
   *
   * Google returns attachments base64url encoded, which is decoded here so what
   * the model reads is the file rather than an encoding of it.
   */
  private async readAttachment(ctx: unknown, call: Call): Promise<Result> {
    let input: { message_id?: string; attachment_id?: string };
    try {
      input = JSON.parse(call.arguments || "{}");
    } catch (err) {
      throw new Error(`gmail: read attachment arguments: ${message(err)}`);
    }

    const path = `/users/me/messages/${encodeURIComponent(input.message_id ?? "")}/attachments/${encodeURIComponent(input.attachment_id ?? "")}`;
    const attachment = await this.get<{ size?: number; data?: string }>(
      ctx,
      call.token.accessToken,
      path,
      [],
    );

    const decoded = decodeAttachment(attachment.data ?? "");

    const truncated = decoded.length > maxAttachment;
    const bytes = truncated ? decoded.subarray(0, maxAttachment) : decoded;

    return {
      content: JSON.stringify({
        size: attachment.size ?? 0,
        truncated,
        text: bytes.toString("utf8"),
      }),
      summary: `lampiran dibaca (${bytes.length} byte)`,
    };
  }

  /**
   * Writes a draft without sending it.
   *
   * The label is `write_internal`, so this never waits for a person: nothing
   * left the workspace.
   */
  private async createDraft(ctx: unknown, call: Call): Promise<Result> {
    const { raw, input } = compose(call);

    const created = await this.post<{ id?: string; message?: { id?: string } }>(
      ctx,
      call.token.accessToken,
      "/users/me/drafts",
      { message: { raw } },
    );

    return {
      content: JSON.stringify({
        draft_id: created.id ?? "",
        to: input.to,
        subject: input.subject,
        sent: false,
      }),
      summary: `draf untuk ${input.to} dibuat, belum dikirim`,
    };
  }

  /**
   * Sends a message. It is only ever reached after a person approved it: the
   * label is `write_external`.
   */
  private async send(ctx: unknown, call: Call): Promise<Result> {
    const { raw, input } = compose(call);

    const sent = await this.post<{ id?: string; threadId?: string }>(
      ctx,
      call.token.accessToken,
      "/users/me/messages/send",
      { raw },
    );

    return {
      content: JSON.stringify({
        message_id: sent.id ?? "",
        thread_id: sent.threadId ?? "",
        to: input.to,
        sent: true,
      }),
      summary: `email terkirim ke ${input.to}`,
    };
  }

  // ------------------------------------------------------------ HTTP helpers

  /** Performs a GET and decodes the answer. */
  private async get<T>(
    ctx: unknown,
    token: string,
    path: string,
    query: [string, string][],
  ): Promise<T> {
    let address = this.apiBase + path;
    if (query.length > 0) {
      address += `?${encodeQuery(query)}`;
    }
    return this.do<T>(address, token, path, { method: "GET" });
  }

  /** Performs a POST with a JSON body and decodes the answer. */
  private async post<T>(_ctx: unknown, token: string, path: string, body: unknown): Promise<T> {
    return this.do<T>(this.apiBase + path, token, path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
  }

  /**
   * Sends a request and decodes the answer.
   *
   * The status is checked before the body is decoded, because Google reports a
   * refusal as a JSON error rather than as the shape the caller asked for.
   */
  private async do<T>(address: string, token: string, path: string, init: RequestInit): Promise<T> {
    const headers: Record<string, string> = {
      ...((init.headers as Record<string, string> | undefined) ?? {}),
      Authorization: `Bearer ${token}`,
    };

    const response = await this.request(address, { ...init, headers });
    const body = await readCapped(response);

    if (response.status >= 400) {
      throw new Error(`gmail: ${path} failed (status ${response.status}): ${truncate(body, 300)}`);
    }

    try {
      return JSON.parse(body) as T;
    } catch (err) {
      throw new Error(`gmail: ${path} answered something unreadable: ${message(err)}`);
    }
  }

  private async request(address: string, init: RequestInit): Promise<Response> {
    try {
      return await fetch(address, { ...init, signal: AbortSignal.timeout(this.timeout) });
    } catch (err) {
      throw new Error(`gmail: ${new URL(address).pathname}: ${message(err)}`);
    }
  }
}

/** The arguments both writing tools take. */
interface ComposeInput {
  to: string;
  subject: string;
  body: string;
}

/** Renders the RFC 5322 message Gmail's API takes, base64url encoded. */
function compose(call: Call): { raw: string; input: ComposeInput } {
  let input: ComposeInput;
  try {
    input = JSON.parse(call.arguments || "{}");
  } catch (err) {
    throw new Error(`gmail: read the message arguments: ${message(err)}`);
  }

  // A header value containing a newline would let a caller inject headers, so
  // the address and the subject are refused rather than escaped.
  if (/[\r\n]/.test(input.to ?? "") || /[\r\n]/.test(input.subject ?? "")) {
    throw new Error(`${ErrInvalidInput.message}: the recipient and the subject must not contain line breaks`);
  }
  if ((input.to ?? "").trim() === "") {
    throw new Error(`${ErrInvalidInput.message}: a recipient is required`);
  }

  const encodedSubject = encodeMimeWord("utf-8", input.subject ?? "");

  let text = `To: ${input.to}\r\n`;
  text += `Subject: ${encodedSubject}\r\n`;
  text += "MIME-Version: 1.0\r\n";
  text += 'Content-Type: text/plain; charset="UTF-8"\r\n';
  text += "\r\n";
  text += input.body ?? "";

  return { raw: Buffer.from(text, "utf8").toString("base64url"), input };
}

/**
 * Go's `mime.QEncoding.Encode("utf-8", s)`: an RFC 2047 encoded-word, split at
 * the 75-character limit without breaking a multi-byte character.
 */
function encodeMimeWord(charset: string, value: string): string {
  if (!needsEncoding(value)) {
    return value;
  }

  const maxContentLen = 75 - "=?UTF-8?q?".length - "?=".length;
  const parts: string[] = [];
  let current = "";
  let currentLen = 0;

  for (const char of value) {
    const bytes = new TextEncoder().encode(char);
    // Go counts a space as one character of content even though it encodes as
    // `_`; only `=`, `?`, `_` and non-ASCII cost three bytes per byte.
    const encLen = bytes.length === 1 && isQMeasured(bytes[0]) ? 1 : 3 * bytes.length;

    if (currentLen + encLen > maxContentLen) {
      parts.push(current);
      current = "";
      currentLen = 0;
    }
    current += qString(bytes);
    currentLen += encLen;
  }
  parts.push(current);

  return parts.map((part) => `=?${charset}?q?${part}?=`).join(" ");
}

/** Go's `needsEncoding`: a control character or anything outside printable ASCII. */
function needsEncoding(value: string): boolean {
  for (const char of value) {
    const code = char.codePointAt(0) ?? 0;
    if ((code < 0x20 || code > 0x7e) && code !== 0x09) {
      return true;
    }
  }
  return false;
}

/** True for a byte Q-encoding leaves alone. */
function isQPlain(byte: number): boolean {
  return byte >= 0x21 && byte <= 0x7e && byte !== 0x3d && byte !== 0x3f && byte !== 0x5f;
}

/** True for a byte Go counts as one character when splitting the word. */
function isQMeasured(byte: number): boolean {
  return byte >= 0x20 && byte <= 0x7e && byte !== 0x3d && byte !== 0x3f && byte !== 0x5f;
}

/**
 * Decodes Google's attachment body. Google pads some answers and not others, so
 * the raw encoding is tried first and the padded one second; anything else is
 * reported as unreadable.
 */
function decodeAttachment(data: string): Buffer {
  if (/^[A-Za-z0-9_-]*={0,2}$/.test(data) && !data.includes("=")) {
    return Buffer.from(data, "base64url");
  }
  if (/^[A-Za-z0-9+/]*={0,2}$/.test(data)) {
    return Buffer.from(data, "base64");
  }
  throw new Error("gmail: the attachment is not readable");
}

/** Go's `writeQString`: space as `_`, printable ASCII as itself, the rest `=XX`. */
function qString(bytes: Uint8Array): string {
  let out = "";
  for (const byte of bytes) {
    if (byte === 0x20) {
      out += "_";
    } else if (isQPlain(byte)) {
      out += String.fromCharCode(byte);
    } else {
      out += `=${byte.toString(16).toUpperCase().padStart(2, "0")}`;
    }
  }
  return out;
}

/** Reads at most four mebibytes of the body. */
async function readCapped(response: Response): Promise<string> {
  const buffer = await response.arrayBuffer();
  return new TextDecoder().decode(new Uint8Array(buffer).subarray(0, maxBody));
}

/** Bounds what an error message carries. */
function truncate(value: string, limit: number): string {
  return value.length <= limit ? value : `${value.slice(0, limit)}…`;
}

function message(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/**
 * Renders query parameters the way Go's `url.Values.Encode` does: keys sorted,
 * space as `+`, and only `A-Z a-z 0-9 - _ . ~` left unescaped.
 */
function encodeQuery(pairs: [string, string][]): string {
  return [...pairs]
    .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
    .map(([key, value]) => `${queryEscape(key)}=${queryEscape(value)}`)
    .join("&");
}

function queryEscape(value: string): string {
  let out = "";
  for (const byte of new TextEncoder().encode(value)) {
    if (
      (byte >= 0x30 && byte <= 0x39) ||
      (byte >= 0x41 && byte <= 0x5a) ||
      (byte >= 0x61 && byte <= 0x7a) ||
      byte === 0x2d ||
      byte === 0x5f ||
      byte === 0x2e ||
      byte === 0x7e
    ) {
      out += String.fromCharCode(byte);
    } else if (byte === 0x20) {
      out += "+";
    } else {
      out += `%${byte.toString(16).toUpperCase().padStart(2, "0")}`;
    }
  }
  return out;
}
