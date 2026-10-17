/**
 * The native adapter for Slack: the tools a Bolu uses to read the channels a
 * workspace installed it into and to post into one, and the OAuth endpoints
 * that install it.
 *
 * Two facts about Slack shape this file. Every Web API answer is an envelope
 * that carries `ok`, and a refusal is still answered **200** — so the envelope,
 * not the status, is what tells a success from a failure. And the credential is
 * a bot token (`xoxb-`), which belongs to the installation rather than to a
 * person.
 *
 * Ported 1:1 from internal/modules/integration/adapters/slack/slack.go.
 */

import { ErrInvalidInput, LabelRead, LabelWriteExternal } from "../../domain/integration.ts";
import { ErrRefreshFailed, type OAuthTokens } from "../../domain/oauth.ts";
import {
  ErrUnknownTool,
  TriggerWebhook,
  toolName,
  type Adapter,
  type Call,
  type Result,
  type ToolSpec,
  type TriggerSpec,
} from "../../domain/adapter.ts";

/** The application key, and the prefix of every tool this adapter declares. */
export const App = "slack";

/** Slack's endpoints. */
const authEndpoint = "https://slack.com/oauth/v2/authorize";
const tokenEndpoint = "https://slack.com/api/oauth.v2.access";
const apiBase = "https://slack.com/api";

/** The set the tools need, and no more. Both are bot scopes. */
export const ScopeChannelsRead = "channels:read";
export const ScopeChatWrite = "chat:write";

/** Bounds what is read from Slack. */
const maxBody = 4 << 20;

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

/** Slack's own refusal, told apart from a transport failure. */
class TokenRefused extends Error {
  constructor(message: string) {
    super(`slack: slack refused the grant: ${message}`);
    this.name = "TokenRefused";
  }
}

/** The Slack integration. */
export class SlackAdapter implements Adapter {
  private readonly clientId: string;
  private readonly clientSecret: string;
  private readonly apiBase: string;
  private readonly authUrl: string;
  private readonly tokenUrl: string;
  private readonly timeout: number;

  constructor(cfg: Config) {
    if (cfg.clientId.trim() === "" || cfg.clientSecret.trim() === "") {
      throw new Error("slack: an OAuth client id and secret are required");
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

  /** What an installation asks for. */
  defaultScopes(): string[] {
    return [ScopeChannelsRead, ScopeChatWrite];
  }

  /** What a Bolu can do with an installed app. */
  tools(): ToolSpec[] {
    return [
      {
        name: toolName(App, "post_message"),
        label: LabelWriteExternal,
        description: "Kirim pesan ke sebuah kanal Slack. Membutuhkan persetujuan manusia.",
        schema: `{
				"type": "object",
				"properties": {
					"channel":   {"type": "string", "description": "ID atau nama kanal, mis. C012AB3CD atau #umum"},
					"text":      {"type": "string", "description": "Isi pesan"},
					"thread_ts": {"type": "string", "description": "Timestamp pesan induk, untuk membalas di dalam thread"}
				},
				"required": ["channel", "text"],
				"additionalProperties": false
			}`,
      },
      {
        name: toolName(App, "list_channels"),
        label: LabelRead,
        description: "Daftar kanal publik di workspace Slack yang terhubung",
        schema: `{
				"type": "object",
				"properties": {
					"limit":  {"type": "integer", "minimum": 1, "maximum": 200},
					"cursor": {"type": "string", "description": "Penanda halaman berikutnya dari jawaban sebelumnya"}
				},
				"additionalProperties": false
			}`,
      },
    ];
  }

  /**
   * Slack pushes, so the trigger is stated as a webhook — but the declaration
   * is only what a routine screen offers. Delivery is the deployment's own
   * Events API subscription pointed at the integration webhook route.
   */
  triggers(): TriggerSpec[] {
    return [
      {
        app: App,
        kind: TriggerWebhook,
        event: "slack.message_received",
        description: "Pesan baru masuk ke kanal Slack",
      },
    ];
  }

  // ------------------------------------------------------------------- OAuth

  /** Slack's consent page for a state. */
  authCodeUrl(state: string, redirectUri: string): string {
    const query = encodeQuery([
      ["client_id", this.clientId],
      ["redirect_uri", redirectUri],
      ["state", state],
      // Slack takes the bot scopes as one comma-separated list. There is no
      // `user_scope`: every tool here runs as the installation.
      ["scope", this.defaultScopes().join(",")],
    ]);
    return `${this.authUrl}?${query}`;
  }

  /** Trades a code for the installation's bot token. */
  async exchange(_ctx: unknown, code: string, redirectUri: string): Promise<OAuthTokens> {
    return this.token([
      ["grant_type", "authorization_code"],
      ["code", code],
      ["redirect_uri", redirectUri],
    ]);
  }

  /**
   * Trades a refresh token for a new access token.
   *
   * A bot token from the plain exchange does not expire and comes with no
   * refresh token; only an app with token rotation turned on reaches this with
   * a value.
   */
  async refresh(_ctx: unknown, refreshToken: string): Promise<OAuthTokens> {
    if (refreshToken.trim() === "") {
      throw new Error(`${ErrRefreshFailed.message}: slack issued no refresh token for this installation`);
    }

    try {
      return await this.token([
        ["grant_type", "refresh_token"],
        ["refresh_token", refreshToken],
      ]);
    } catch (err) {
      if (err instanceof TokenRefused) {
        throw new Error(`${ErrRefreshFailed.message}: ${err.message}`);
      }
      throw err;
    }
  }

  /** Names the workspace the app was installed into. */
  async accountLabel(ctx: unknown, tokens: OAuthTokens): Promise<string> {
    const profile = await this.get<{ team?: string; url?: string }>(
      ctx,
      tokens.accessToken,
      "auth.test",
      [],
    );

    // The name is what a person recognises, and the workspace's own domain is
    // what is unique: it is the fallback.
    const label = (profile.team ?? "").trim() || (profile.url ?? "").trim();
    if (label === "") {
      throw new Error("slack: the installation has no workspace name");
    }
    return label;
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
      throw new TokenRefused(`(status ${response.status}): ${truncate(text, 200)}`);
    }

    let decoded: {
      ok?: boolean;
      error?: string;
      access_token?: string;
      refresh_token?: string;
      expires_in?: number;
      scope?: string;
    };
    try {
      decoded = JSON.parse(text);
    } catch (err) {
      throw new Error(`slack: token answer is not readable: ${message(err)}`);
    }

    if (!decoded.ok) {
      // Slack answers a refused exchange with 200 and `ok:false`, so the
      // envelope is the only place the refusal is visible.
      throw new TokenRefused(decoded.error || "unknown_error");
    }

    const tokens: OAuthTokens = {
      accessToken: decoded.access_token ?? "",
      refreshToken: decoded.refresh_token ?? "",
      expiresAt: null,
      scopes: splitScopes(decoded.scope ?? ""),
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
      case toolName(App, "post_message"):
        return this.postMessage(ctx, call);
      case toolName(App, "list_channels"):
        return this.listChannels(ctx, call);
      default:
        throw new Error(`${ErrUnknownTool.message}: ${JSON.stringify(call.tool)}`);
    }
  }

  /**
   * Posts into a channel. It is only ever reached after a person approved it:
   * the label is `write_external`.
   */
  private async postMessage(ctx: unknown, call: Call): Promise<Result> {
    let input: { channel?: string; text?: string; thread_ts?: string };
    try {
      input = JSON.parse(call.arguments || "{}");
    } catch (err) {
      throw new Error(`slack: read post_message arguments: ${message(err)}`);
    }
    if (!input.channel?.trim()) {
      throw new Error(`${ErrInvalidInput.message}: a channel is required`);
    }
    if (!input.text?.trim()) {
      throw new Error(`${ErrInvalidInput.message}: a message body is required`);
    }

    const payload: Record<string, unknown> = { channel: input.channel, text: input.text };
    if ((input.thread_ts ?? "").trim() !== "") {
      payload["thread_ts"] = input.thread_ts;
    }

    const posted = await this.post<{ channel?: string; ts?: string }>(
      ctx,
      call.token.accessToken,
      "chat.postMessage",
      payload,
    );

    return {
      content: JSON.stringify({
        channel: posted.channel || input.channel,
        ts: posted.ts ?? "",
        posted: true,
      }),
      summary: `pesan terkirim ke ${input.channel}`,
    };
  }

  /**
   * Lists the public channels of the workspace.
   *
   * Only public channels: reading the private ones needs `groups:read`, which
   * no tool here asks for. A page is returned with the cursor that reads the
   * next one.
   */
  private async listChannels(ctx: unknown, call: Call): Promise<Result> {
    let input: { limit?: number; cursor?: string };
    try {
      input = JSON.parse(call.arguments || "{}");
    } catch (err) {
      throw new Error(`slack: read list_channels arguments: ${message(err)}`);
    }

    const limit = !input.limit || input.limit <= 0 || input.limit > 200 ? 100 : input.limit;

    const query: [string, string][] = [
      ["types", "public_channel"],
      ["exclude_archived", "true"],
      ["limit", String(limit)],
    ];
    if ((input.cursor ?? "").trim() !== "") {
      query.push(["cursor", input.cursor as string]);
    }

    const listing = await this.get<{
      channels?: { id?: string; name?: string; is_private?: boolean; num_members?: number }[];
      response_metadata?: { next_cursor?: string };
    }>(ctx, call.token.accessToken, "conversations.list", query);

    const channels = (listing.channels ?? []).map((channel) => ({
      id: channel.id ?? "",
      name: channel.name ?? "",
      private: channel.is_private ?? false,
      num_members: channel.num_members ?? 0,
    }));

    return {
      content: JSON.stringify({
        channels,
        next_cursor: listing.response_metadata?.next_cursor ?? "",
      }),
      summary: `${channels.length} kanal Slack`,
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
    let address = `${this.apiBase}/${path}`;
    if (query.length > 0) {
      address += `?${encodeQuery(query)}`;
    }
    return this.do<T>(address, token, path, { method: "GET" });
  }

  /** Performs a POST with a JSON body and decodes the answer. */
  private async post<T>(_ctx: unknown, token: string, path: string, body: unknown): Promise<T> {
    return this.do<T>(`${this.apiBase}/${path}`, token, path, {
      method: "POST",
      headers: { "Content-Type": "application/json; charset=utf-8" },
      body: JSON.stringify(body),
    });
  }

  /**
   * Sends a request and unwraps the envelope Slack answers everything with.
   *
   * The status is checked first because a transport-level failure is not a
   * refusal, and then the envelope's own `ok` is checked because Slack reports
   * a refusal — an unknown channel, a missing scope — as **200** with
   * `ok:false`.
   */
  private async do<T>(address: string, token: string, path: string, init: RequestInit): Promise<T> {
    const headers: Record<string, string> = {
      ...((init.headers as Record<string, string> | undefined) ?? {}),
      Authorization: `Bearer ${token}`,
    };

    const response = await this.request(address, { ...init, headers });
    const body = await readCapped(response);

    if (response.status >= 400) {
      throw new Error(`slack: ${path} failed (status ${response.status}): ${truncate(body, 300)}`);
    }

    let envelope: { ok?: boolean; error?: string };
    try {
      envelope = JSON.parse(body);
    } catch (err) {
      throw new Error(`slack: ${path} answered something unreadable: ${message(err)}`);
    }
    if (!envelope.ok) {
      throw new Error(`slack: ${path} was refused: ${envelope.error || "unknown_error"}`);
    }

    try {
      return JSON.parse(body) as T;
    } catch (err) {
      throw new Error(`slack: ${path} answered something unreadable: ${message(err)}`);
    }
  }

  private async request(address: string, init: RequestInit): Promise<Response> {
    try {
      return await fetch(address, { ...init, signal: AbortSignal.timeout(this.timeout) });
    } catch (err) {
      throw new Error(`slack: ${new URL(address).pathname}: ${message(err)}`);
    }
  }
}

/** Reads the scope list Slack answers with, which is comma separated. */
function splitScopes(value: string): string[] {
  return value
    .split(/[,\s]+/)
    .map((field) => field.trim())
    .filter((field) => field !== "");
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
