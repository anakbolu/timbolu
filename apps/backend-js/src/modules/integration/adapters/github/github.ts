/**
 * The native adapter for GitHub: the tools a Bolu uses to see the repositories
 * a connection reaches and to file an issue, and the OAuth endpoints that
 * connect an account.
 *
 * It speaks GitHub's REST API directly rather than through a wrapper library: a
 * tool call is one HTTP request, and what matters is that a refusal arrives as
 * a reason rather than as a retry.
 *
 * Ported 1:1 from internal/modules/integration/adapters/github/github.go.
 */

import { ErrInvalidInput, LabelRead, LabelWriteExternal } from "../../domain/integration.ts";
import {
  ErrRefreshFailed,
  type OAuthTokens,
} from "../../domain/oauth.ts";
import {
  ErrUnknownTool,
  toolName,
  type Adapter,
  type Call,
  type Result,
  type ToolSpec,
  type TriggerSpec,
} from "../../domain/adapter.ts";

/** The application key, and the prefix of every tool this adapter declares. */
export const App = "github";

/** GitHub's endpoints. */
const authEndpoint = "https://github.com/login/oauth/authorize";
const tokenEndpoint = "https://github.com/login/oauth/access_token";
const apiBase = "https://api.github.com";

/** The narrowest classic scopes that let the tools work. */
export const ScopeRepo = "repo";
export const ScopeReadUser = "read:user";

/** GitHub refuses a request without a user agent. */
const userAgent = "timbolu-backend";

/** Pins the REST shape. */
const apiVersion = "2022-11-28";

/** Bounds what is read from GitHub. */
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

/** GitHub's own refusal, told apart from a transport failure. */
class TokenRefused extends Error {
  constructor(message: string) {
    super(`github: github refused the grant: ${message}`);
    this.name = "TokenRefused";
  }
}

/** The GitHub integration. */
export class GithubAdapter implements Adapter {
  private readonly clientId: string;
  private readonly clientSecret: string;
  private readonly apiBase: string;
  private readonly authUrl: string;
  private readonly tokenUrl: string;
  private readonly timeout: number;

  constructor(cfg: Config) {
    if (cfg.clientId.trim() === "" || cfg.clientSecret.trim() === "") {
      throw new Error("github: an OAuth client id and secret are required");
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
    return [ScopeRepo, ScopeReadUser];
  }

  /** What a Bolu can do with a connected account. */
  tools(): ToolSpec[] {
    return [
      {
        name: toolName(App, "list_repos"),
        label: LabelRead,
        description: "Daftar repositori GitHub yang dapat diakses akun terhubung",
        schema: `{
				"type": "object",
				"properties": {
					"limit": {"type": "integer", "minimum": 1, "maximum": 100},
					"page":  {"type": "integer", "minimum": 1, "description": "Halaman ke berapa, mulai dari 1"}
				},
				"additionalProperties": false
			}`,
      },
      {
        name: toolName(App, "create_issue"),
        label: LabelWriteExternal,
        description: "Buat issue baru di sebuah repositori GitHub. Membutuhkan persetujuan manusia.",
        schema: `{
				"type": "object",
				"properties": {
					"owner": {"type": "string", "description": "Pemilik repositori, mis. robzlabz"},
					"repo":  {"type": "string", "description": "Nama repositori"},
					"title": {"type": "string"},
					"body":  {"type": "string", "description": "Isi issue, dalam Markdown"}
				},
				"required": ["owner", "repo", "title"],
				"additionalProperties": false
			}`,
      },
    ];
  }

  /**
   * Nothing is declared, and that is the honest answer: GitHub's push events
   * arrive through an app installation's webhook, which is a different
   * credential than the OAuth token this adapter stores.
   */
  triggers(): TriggerSpec[] {
    return [];
  }

  // ------------------------------------------------------------------- OAuth

  /** GitHub's consent page for a state. */
  authCodeUrl(state: string, redirectUri: string): string {
    const query = encodeQuery([
      ["client_id", this.clientId],
      ["redirect_uri", redirectUri],
      ["state", state],
      // GitHub takes the scopes as one space-separated list.
      ["scope", this.defaultScopes().join(" ")],
    ]);
    return `${this.authUrl}?${query}`;
  }

  /** Trades a code for an access token. */
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
   * An OAuth app only issues a refresh token when user-to-server token
   * expiration is turned on for it; without one there is nothing to refresh.
   */
  async refresh(_ctx: unknown, refreshToken: string): Promise<OAuthTokens> {
    if (refreshToken.trim() === "") {
      throw new Error(
        `${ErrRefreshFailed.message}: github issued no refresh token for this connection`,
      );
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

  /** Reads the login the account belongs to. */
  async accountLabel(ctx: unknown, tokens: OAuthTokens): Promise<string> {
    const profile = await this.get<{ login?: string }>(ctx, tokens.accessToken, "/user", []);
    if (!profile.login || profile.login.trim() === "") {
      throw new Error("github: the account has no login");
    }
    return profile.login;
  }

  /**
   * The token-endpoint exchange, for both a code and a refresh.
   *
   * GitHub answers the token endpoint with a form-encoded body unless it is
   * asked for JSON, so the Accept header is what makes the answer readable.
   */
  private async token(form: [string, string][]): Promise<OAuthTokens> {
    const body = encodeQuery([...form, ["client_id", this.clientId], ["client_secret", this.clientSecret]]);

    const response = await this.request(this.tokenUrl, {
      method: "POST",
      headers: {
        "Content-Type": "application/x-www-form-urlencoded",
        Accept: "application/json",
        "User-Agent": userAgent,
      },
      body,
    });

    const text = await readCapped(response);

    if (response.status >= 400) {
      throw new TokenRefused(`(status ${response.status}): ${truncate(text, 200)}`);
    }

    let decoded: {
      error?: string;
      error_description?: string;
      access_token?: string;
      refresh_token?: string;
      expires_in?: number;
      scope?: string;
    };
    try {
      decoded = JSON.parse(text);
    } catch (err) {
      throw new Error(`github: token answer is not readable: ${message(err)}`);
    }

    if (decoded.error) {
      // GitHub answers a refused exchange with 200 and an `error` field.
      throw new TokenRefused(`${decoded.error} ${truncate(decoded.error_description ?? "", 200)}`);
    }
    if (!decoded.access_token || decoded.access_token.trim() === "") {
      throw new TokenRefused("github answered no access token");
    }

    const tokens: OAuthTokens = {
      accessToken: decoded.access_token,
      refreshToken: decoded.refresh_token ?? "",
      expiresAt: null,
      scopes: splitScopes(decoded.scope ?? ""),
    };
    if ((decoded.expires_in ?? 0) > 0) {
      // A minute is taken off, so a token that expires while a slow call is in
      // flight is treated as expired rather than used.
      tokens.expiresAt = new Date(Date.now() + (decoded.expires_in as number) * 1000 - 60_000);
    }
    return tokens;
  }

  // ------------------------------------------------------------------- tools

  /** Runs one tool. */
  async execute(ctx: unknown, call: Call): Promise<Result> {
    switch (call.tool) {
      case toolName(App, "list_repos"):
        return this.listRepos(ctx, call);
      case toolName(App, "create_issue"):
        return this.createIssue(ctx, call);
      default:
        throw new Error(`${ErrUnknownTool.message}: ${JSON.stringify(call.tool)}`);
    }
  }

  /**
   * Lists the repositories the connection can reach.
   *
   * One page of names rather than everything: an account with a thousand
   * repositories would otherwise spend a task's whole token budget on one call.
   */
  private async listRepos(ctx: unknown, call: Call): Promise<Result> {
    let input: { limit?: number; page?: number };
    try {
      input = JSON.parse(call.arguments || "{}");
    } catch (err) {
      throw new Error(`github: read list_repos arguments: ${message(err)}`);
    }

    const limit = !input.limit || input.limit <= 0 || input.limit > 100 ? 30 : input.limit;
    const page = !input.page || input.page <= 0 ? 1 : input.page;

    const query: [string, string][] = [
      ["per_page", String(limit)],
      ["page", String(page)],
      ["sort", "updated"],
    ];

    const repos = await this.get<
      {
        full_name?: string;
        name?: string;
        private?: boolean;
        html_url?: string;
        default_branch?: string;
        updated_at?: string;
        description?: string;
      }[]
    >(ctx, call.token.accessToken, "/user/repos", query);

    const listed = repos.map((repo) => ({
      full_name: repo.full_name ?? "",
      name: repo.name ?? "",
      private: repo.private ?? false,
      html_url: repo.html_url ?? "",
      default_branch: repo.default_branch ?? "",
      updated_at: repo.updated_at ?? "",
      description: repo.description ?? "",
    }));

    return {
      content: JSON.stringify({ repos: listed, page }),
      summary: `${listed.length} repositori GitHub`,
    };
  }

  /**
   * Files an issue. It is only ever reached after a person approved it: the
   * label is `write_external`.
   */
  private async createIssue(ctx: unknown, call: Call): Promise<Result> {
    let input: { owner?: string; repo?: string; title?: string; body?: string };
    try {
      input = JSON.parse(call.arguments || "{}");
    } catch (err) {
      throw new Error(`github: read create_issue arguments: ${message(err)}`);
    }
    if (!input.owner?.trim() || !input.repo?.trim()) {
      throw new Error(`${ErrInvalidInput.message}: an owner and a repository are required`);
    }
    if (!input.title?.trim()) {
      throw new Error(`${ErrInvalidInput.message}: an issue title is required`);
    }

    // The owner and the repository are escaped rather than concatenated: a name
    // with a slash in it would otherwise address a different endpoint.
    const path = `/repos/${encodeURIComponent(input.owner)}/${encodeURIComponent(input.repo)}/issues`;

    const created = await this.post<{
      number?: number;
      html_url?: string;
      state?: string;
      title?: string;
    }>(ctx, call.token.accessToken, path, { title: input.title, body: input.body ?? "" });

    const number = created.number ?? 0;
    return {
      content: JSON.stringify({
        number,
        html_url: created.html_url ?? "",
        state: created.state ?? "",
        title: created.title ?? "",
      }),
      summary: `issue ${input.owner}/${input.repo}#${number} dibuat`,
    };
  }

  // ------------------------------------------------------------ HTTP helpers

  /** Performs a GET and decodes the answer. */
  private async get<T>(ctx: unknown, token: string, path: string, query: [string, string][]): Promise<T> {
    let address = this.apiBase + path;
    if (query.length > 0) {
      address += `?${encodeQuery(query)}`;
    }
    return this.do<T>(ctx, address, token, path, { method: "GET" });
  }

  /** Performs a POST with a JSON body and decodes the answer. */
  private async post<T>(ctx: unknown, token: string, path: string, body: unknown): Promise<T> {
    return this.do<T>(ctx, this.apiBase + path, token, path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
  }

  /**
   * Sends a request and decodes the answer.
   *
   * The status is checked before the body is decoded, because GitHub reports a
   * refusal as `{"message": "..."}` rather than as the shape the caller asked
   * for.
   */
  private async do<T>(
    ctx: unknown,
    address: string,
    token: string,
    path: string,
    init: RequestInit,
  ): Promise<T> {
    const headers: Record<string, string> = {
      ...((init.headers as Record<string, string> | undefined) ?? {}),
      Authorization: `Bearer ${token}`,
      Accept: "application/vnd.github+json",
      "X-GitHub-Api-Version": apiVersion,
      // GitHub refuses a request with no user agent.
      "User-Agent": userAgent,
    };

    const response = await this.request(address, { ...init, headers }, ctx);
    const body = await readCapped(response);

    if (response.status >= 400) {
      // The reason is passed on, so the model can fix its own call instead of
      // retrying the same one.
      throw new Error(`github: ${path} failed (status ${response.status}): ${truncate(apiReason(body), 300)}`);
    }

    try {
      return JSON.parse(body) as T;
    } catch (err) {
      throw new Error(`github: ${path} answered something unreadable: ${message(err)}`);
    }
  }

  private async request(address: string, init: RequestInit, _ctx?: unknown): Promise<Response> {
    try {
      return await fetch(address, { ...init, signal: AbortSignal.timeout(this.timeout) });
    } catch (err) {
      throw new Error(`github: ${new URL(address).pathname}: ${message(err)}`);
    }
  }
}

/** Reads the sentence GitHub puts on a refusal, falling back to the body. */
function apiReason(body: string): string {
  try {
    const refusal: { message?: string } = JSON.parse(body);
    if (refusal.message && refusal.message.trim() !== "") {
      return refusal.message;
    }
  } catch {
    // Not the documented shape; the body is the answer.
  }
  return body;
}

/** Reads the scope list GitHub answers with, which is comma separated. */
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
