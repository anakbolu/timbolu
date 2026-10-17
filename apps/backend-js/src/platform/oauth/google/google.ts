/**
 * The OpenID Connect code flow against Google: building the consent URL,
 * exchanging the code, and reading the verified email.
 *
 * Ported from apps/backend/internal/platform/oauth/google/google.go. The
 * endpoints are configuration so a test can point them at a stub, and the scope
 * list is frozen at the identity and the verified address.
 */

/** Default endpoints. */
export const DefaultAuthURL = "https://accounts.google.com/o/oauth2/v2/auth";
export const DefaultTokenURL = "https://oauth2.googleapis.com/token";
export const DefaultUserInfoURL = "https://openidconnect.googleapis.com/v1/userinfo";

/** Requests the identity and the verified email address only. */
export const Scope = "openid email";

const TIMEOUT_MS = 10_000;

/** The provider response is small; cap it so a misbehaving endpoint cannot exhaust memory. */
const MAX_BODY_BYTES = 1 << 20;

/** Holds the client registration and the endpoints. */
export interface Config {
  clientId: string;
  clientSecret: string;
  redirectUrl: string;
  authUrl?: string;
  tokenUrl?: string;
  userInfoUrl?: string;
}

/** The part of the userinfo response this product needs. */
export interface UserInfo {
  sub: string;
  email: string;
  email_verified: boolean;
}

/** The provider did not assert the address, so the account must not be linked. */
export class EmailNotVerifiedError extends Error {
  constructor() {
    super("google: email is not verified by the provider");
    this.name = "EmailNotVerifiedError";
  }
}

/** Performs the code flow. */
export class Client {
  private readonly cfg: Required<Config>;

  /**
   * Builds a client. The client id and secret are required: without them the
   * consent URL would be rejected by Google at the worst possible moment.
   */
  constructor(cfg: Config) {
    if (!cfg.clientId?.trim()) {
      throw new Error("google: client id is required");
    }
    if (!cfg.clientSecret?.trim()) {
      throw new Error("google: client secret is required");
    }
    if (!cfg.redirectUrl?.trim()) {
      throw new Error("google: redirect url is required");
    }
    this.cfg = {
      clientId: cfg.clientId,
      clientSecret: cfg.clientSecret,
      redirectUrl: cfg.redirectUrl,
      authUrl: cfg.authUrl || DefaultAuthURL,
      tokenUrl: cfg.tokenUrl || DefaultTokenURL,
      userInfoUrl: cfg.userInfoUrl || DefaultUserInfoURL,
    };
  }

  /**
   * Builds the consent URL. `state` is the anti-CSRF value the caller stores in
   * a short-lived cookie and compares on the callback.
   *
   * The parameters are encoded in the order Go's `url.Values.Encode` produces —
   * sorted by key, space as `+` — so the URL a Go deployment redirects to and
   * the one built here are the same string.
   */
  authCodeURL(state: string): string {
    const query = encodeQuery([
      ["access_type", "online"],
      ["client_id", this.cfg.clientId],
      ["prompt", "select_account"],
      ["redirect_uri", this.cfg.redirectUrl],
      ["response_type", "code"],
      ["scope", Scope],
      ["state", state],
    ]);
    return `${this.cfg.authUrl}?${query}`;
  }

  /**
   * Trades the authorization code for an access token and reads the verified
   * email from the userinfo endpoint.
   */
  async exchange(code: string): Promise<UserInfo> {
    if (!code?.trim()) {
      throw new Error("google: authorization code is required");
    }

    const accessToken = await this.exchangeCode(code);
    const info = await this.fetchUserInfo(accessToken);

    if (!info.email_verified) {
      throw new EmailNotVerifiedError();
    }
    if (!info.sub || !info.email) {
      throw new Error("google: userinfo response is missing sub or email");
    }

    return info;
  }

  private async exchangeCode(code: string): Promise<string> {
    const form = encodeQuery([
      ["code", code],
      ["client_id", this.cfg.clientId],
      ["client_secret", this.cfg.clientSecret],
      ["redirect_uri", this.cfg.redirectUrl],
      ["grant_type", "authorization_code"],
    ]);

    const payload = await this.do<{
      access_token?: string;
      error?: string;
      error_description?: string;
    }>(this.cfg.tokenUrl, {
      method: "POST",
      headers: {
        "Content-Type": "application/x-www-form-urlencoded",
        Accept: "application/json",
      },
      body: form.toString(),
    });

    if (payload.error) {
      throw new Error(`google: token endpoint rejected the code: ${payload.error} ${payload.error_description ?? ""}`);
    }
    if (!payload.access_token) {
      throw new Error("google: token endpoint returned no access token");
    }

    return payload.access_token;
  }

  private async fetchUserInfo(accessToken: string): Promise<UserInfo> {
    return this.do<UserInfo>(this.cfg.userInfoUrl, {
      method: "GET",
      headers: {
        Authorization: `Bearer ${accessToken}`,
        Accept: "application/json",
      },
    });
  }

  private async do<T>(url: string, init: RequestInit): Promise<T> {
    let response: Response;
    try {
      response = await fetch(url, { ...init, signal: AbortSignal.timeout(TIMEOUT_MS) });
    } catch (err) {
      throw new Error(`google: call ${new URL(url).host}: ${err instanceof Error ? err.message : String(err)}`);
    }

    const body = await readCapped(response);

    if (response.status < 200 || response.status > 299) {
      throw new Error(`google: ${new URL(url).pathname} returned ${response.status}: ${truncate(body, 200)}`);
    }

    try {
      return JSON.parse(body) as T;
    } catch (err) {
      throw new Error(`google: decode response: ${err instanceof Error ? err.message : String(err)}`);
    }
  }
}

/** Reads at most one mebibyte of the body, as the Go adapter does. */
async function readCapped(response: Response): Promise<string> {
  const buffer = await response.arrayBuffer();
  const bytes = new Uint8Array(buffer).subarray(0, MAX_BODY_BYTES);
  return new TextDecoder().decode(bytes);
}

/**
 * Renders query parameters the way Go's `url.Values.Encode` does: keys sorted,
 * space as `+`, and only `A-Z a-z 0-9 - _ . ~` left unescaped.
 *
 * `URLSearchParams` is not a substitute: it leaves `*` alone and escapes `~`,
 * so the consent URL would differ from the one the Go deployment builds.
 */
function encodeQuery(pairs: [string, string][]): string {
  return [...pairs]
    .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
    .map(([key, value]) => `${queryEscape(key)}=${queryEscape(value)}`)
    .join("&");
}

function queryEscape(value: string): string {
  const bytes = new TextEncoder().encode(value);
  let out = "";
  for (const byte of bytes) {
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

function truncate(value: string, limit: number): string {
  if (value.length <= limit) {
    return value;
  }
  return `${value.slice(0, limit)}...`;
}
