/**
 * The backend's side of the control plane seam: an HTTP client that implements
 * `ComputerUseClient`.
 *
 * It talks to apps/virtual-computer/control over the internal network and nothing
 * else. The control plane is never published, it speaks for every bot at once,
 * and a caller that reached it from the internet could drive any bot's desktop —
 * so the address is a configuration value, and an empty one refuses every call
 * instead of reaching a URL nobody meant.
 *
 * Every `/v1` route requires `Authorization: Bearer <token>`, the shared secret
 * from `VCOMPUTER_CONTROL_TOKEN`; a missing or wrong token comes back as a 401,
 * which is a configuration mistake rather than a failure worth retrying.
 *
 * Ported 1:1 from internal/modules/vcomputer/service/client.go.
 */

import {
  ErrInvalidInput,
  ErrNoSession,
  ErrNotConfigured,
  ErrUnavailable,
  errorMessage,
  wrap,
  type ComputerUseClient,
  type ExecResult,
  type Session,
} from "../domain/ports.ts";

/** Bounds an answer. A screenshot is the largest thing that crosses this seam. */
const maxResponse = 8 << 20;

/**
 * Bounds one call to the control plane when the configuration does not set a
 * positive one. It is generous because a screenshot of a cold sandbox includes
 * the container starting.
 */
export const DefaultTimeout = 30_000;

/**
 * The timeout a call to the control plane really gets: the configured one when it
 * is positive, `DefaultTimeout` otherwise.
 *
 * A zero or negative value is not a bound at all — a client with no timeout waits
 * forever — so it is replaced rather than honored, and every caller that reports
 * the timeout in force asks here instead of repeating the rule (issue #257).
 */
export function EffectiveTimeout(timeout: number): number {
  return timeout <= 0 ? DefaultTimeout : timeout;
}

/** Calls the control plane. */
export class Client implements ComputerUseClient {
  private readonly baseURL: string;
  private readonly token: string;
  private readonly timeout: number;

  /**
   * An empty address leaves the client unconfigured: every call then reports
   * that, which is what a deployment without a virtual computer looks like.
   *
   * An empty token is not refused here, because the control plane is the one that
   * knows whether it is wrong: it answers 401, which `classify` turns into
   * `ErrNotConfigured`.
   */
  constructor(baseURL: string, token: string, timeout: number) {
    this.baseURL = baseURL.trim().replace(/\/+$/, "");
    this.token = token.trim();
    this.timeout = EffectiveTimeout(timeout);
  }

  /** The timeout in force, which is what a startup log must report. */
  effectiveTimeout(): number {
    return this.timeout;
  }

  /** Creates the session if it is not there and returns it either way. */
  async ensureSession(ctx: AbortSignal | undefined, botId: string): Promise<Session> {
    return this.do<Session>(ctx, "POST", "/v1/sessions", { bot_id: botId });
  }

  /** Ends the session and keeps its volume. */
  async stopSession(ctx: AbortSignal | undefined, botId: string): Promise<void> {
    await this.do(ctx, "DELETE", sessionPath(botId), null);
  }

  /**
   * The current desktop as PNG bytes.
   *
   * It is the one call whose answer is not JSON, so it reads the body directly
   * rather than going through `do`.
   */
  async screenshot(ctx: AbortSignal | undefined, botId: string): Promise<Uint8Array> {
    const unconfigured = this.configured();
    if (unconfigured) throw unconfigured;

    let response: Response;
    try {
      response = await this.fetch(ctx, "POST", `${sessionPath(botId)}/screenshot`, null);
    } catch (err) {
      throw wrap(ErrUnavailable, errorMessage(err));
    }

    const payload = await readBounded(response);
    if (response.status >= 400) {
      throw classify(response.status, payload);
    }
    if (payload.length === 0) {
      throw new Error("vcomputer: the control plane answered an empty screenshot");
    }
    return payload;
  }

  /** Presses a point on the desktop. */
  async click(ctx: AbortSignal | undefined, botId: string, x: number, y: number): Promise<void> {
    await this.do(ctx, "POST", `${sessionPath(botId)}/click`, { x, y });
  }

  /** Sends text to the focused window. */
  async type(ctx: AbortSignal | undefined, botId: string, text: string): Promise<void> {
    await this.do(ctx, "POST", `${sessionPath(botId)}/type`, { text });
  }

  /**
   * Points the browser at a page.
   *
   * The control plane is what validates the URL — it accepts http, https, data
   * and about, and refuses anything else with a 400 — and nothing here
   * second-guesses that.
   */
  async navigate(ctx: AbortSignal | undefined, botId: string, url: string): Promise<void> {
    await this.do(ctx, "POST", `${sessionPath(botId)}/navigate`, { url });
  }

  /**
   * Runs a command inside the sandbox.
   *
   * Whether the runtime really ran it is in the answer's `stub` field, so a
   * caller has to read that field rather than the exit code.
   */
  async exec(ctx: AbortSignal | undefined, botId: string, command: string): Promise<ExecResult> {
    return this.do<ExecResult>(ctx, "POST", `${sessionPath(botId)}/exec`, { command });
  }

  /** Performs one JSON call. */
  private async do<T = void>(
    ctx: AbortSignal | undefined,
    method: string,
    path: string,
    body: unknown,
  ): Promise<T> {
    const unconfigured = this.configured();
    if (unconfigured) throw unconfigured;

    let response: Response;
    try {
      response = await this.fetch(ctx, method, path, body);
    } catch (err) {
      // Unreachable is not a refusal: the sandbox may simply not be up yet, and
      // the caller is expected to retry rather than to change what it asked for.
      throw wrap(ErrUnavailable, errorMessage(err));
    }

    const payload = await readBounded(response);
    if (response.status >= 400) {
      throw classify(response.status, payload);
    }

    if (payload.length === 0) {
      return undefined as T;
    }
    try {
      return JSON.parse(new TextDecoder().decode(payload)) as T;
    } catch (err) {
      throw new Error(`vcomputer: read control plane answer: ${errorMessage(err)}`);
    }
  }

  /** Sends one request with the bearer and the bound in force. */
  private async fetch(
    ctx: AbortSignal | undefined,
    method: string,
    path: string,
    body: unknown,
  ): Promise<Response> {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.timeout);
    const onAbort = () => controller.abort();
    ctx?.addEventListener("abort", onAbort, { once: true });

    try {
      return await globalThis.fetch(`${this.baseURL}${path}`, {
        method,
        headers: {
          "Content-Type": "application/json",
          // A screenshot is a /v1 route like any other, so it needs the bearer
          // too: the one call that skipped it would be the one call that can only
          // ever answer 401.
          Authorization: `Bearer ${this.token}`,
        },
        body: body === null ? undefined : JSON.stringify(body),
        signal: controller.signal,
      });
    } finally {
      clearTimeout(timer);
      ctx?.removeEventListener("abort", onAbort);
    }
  }

  /** Reports whether a control plane address was set. */
  private configured(): Error | null {
    return this.baseURL === "" ? ErrNotConfigured : null;
  }
}

/** The resource path for one bot. */
function sessionPath(botId: string): string {
  // The id is not escaped because the control plane refuses an id that would need
  // escaping: a bot id is an identifier, not free text.
  return `/v1/sessions/${botId}`;
}

/** Reads at most `maxResponse` bytes of a body. */
async function readBounded(response: Response): Promise<Uint8Array> {
  const buffer = new Uint8Array(await response.arrayBuffer());
  return buffer.length > maxResponse ? buffer.subarray(0, maxResponse) : buffer;
}

/**
 * Maps a status code back onto the sentinel the control plane put it in.
 *
 * A 404 means "no session, create one", a 401 means the token is missing or
 * wrong, and a 400 means the request itself was refused. Flattening them into a
 * string is how a caller ends up retrying something that will never succeed. The
 * sentence the control plane wrote is carried through rather than replaced.
 */
export function classify(status: number, payload: Uint8Array): Error {
  const message = controlPlaneMessage(payload) || httpStatusText(status);

  switch (status) {
    case 400:
      return wrap(ErrInvalidInput, message);
    case 401:
      // A refused token is a deployment misconfiguration rather than a transient
      // failure: retrying sends the same header the control plane just rejected.
      return wrap(ErrNotConfigured, message);
    case 404:
      return wrap(ErrNoSession, message);
    default:
      return wrap(ErrUnavailable, message);
  }
}

/** Reads the control plane's `{"error": "…"}` sentence, or "". */
function controlPlaneMessage(payload: Uint8Array): string {
  if (payload.length === 0) return "";
  try {
    const decoded: unknown = JSON.parse(new TextDecoder().decode(payload));
    if (decoded && typeof decoded === "object" && "error" in decoded && typeof decoded.error === "string") {
      return decoded.error.trim();
    }
  } catch {
    // A body that is not JSON carries no sentence to preserve.
  }
  return "";
}

/** The standard phrase for a status, as Go's `http.StatusText` gives it. */
function httpStatusText(status: number): string {
  switch (status) {
    case 400:
      return "Bad Request";
    case 401:
      return "Unauthorized";
    case 403:
      return "Forbidden";
    case 404:
      return "Not Found";
    case 409:
      return "Conflict";
    case 422:
      return "Unprocessable Entity";
    case 429:
      return "Too Many Requests";
    case 500:
      return "Internal Server Error";
    case 502:
      return "Bad Gateway";
    case 503:
      return "Service Unavailable";
    case 504:
      return "Gateway Timeout";
    default:
      return `status ${status}`;
  }
}
