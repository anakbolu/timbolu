/**
 * The two halves of one runtime are mutually dependent, and this is where that
 * is resolved.
 *
 * The service sends *through* the transport, and the engine that is the
 * transport in the gateway process records what arrives *through* the service.
 * Whichever is built first needs the other, so the service is built first — it
 * needs nothing from the engine to exist — and the engine's owner installs
 * itself here once it exists.
 *
 * Ported 1:1 from internal/modules/whatsapp/service/transport.go, plus the HTTP
 * client half of the seam (`packages/whatsapp/seam/gateway.go`) that this
 * module is the only consumer of in this process. The gateway owns the socket;
 * this module owns the records; they talk over the internal seam with the
 * shared token on every call.
 */

import type { Link, Scope, Snapshot, ToSend, Transport } from "../domain/whatsapp.ts";
import {
  ErrInvalidInput,
  ErrLinkExpired,
  ErrNotFound,
  ErrNotConfigured,
  ErrUnavailable,
  NilUUID,
  wrap,
} from "../domain/whatsapp.ts";

/** The shared secret that authenticates the API to the gateway. */
export const TokenHeader = "X-Wa-Gateway-Token";

/** Installs the transport a service sends through. */
export function useTransport(target: { useTransport(transport: Transport): void }, transport: Transport): void {
  target.useTransport(transport);
}

/** The status one error becomes, which is the mapping the client reverses. */
export function statusFor(err: unknown): number {
  if (isError(err, ErrNotFound)) return 404;
  if (isError(err, ErrInvalidInput)) return 400;
  if (isError(err, ErrLinkExpired)) return 410;
  if (isError(err, ErrUnavailable)) return 503;
  return 500;
}

/** The sentinel a gateway status maps back to. */
export function sentinelForStatus(status: number): Error {
  switch (status) {
    case 404:
      return ErrNotFound;
    case 400:
    case 409:
      // Both are the caller's input: one is a request that cannot be acted on,
      // the other is a state that refuses it. The message says which.
      return ErrInvalidInput;
    case 410:
      return ErrLinkExpired;
    case 503:
      return ErrUnavailable;
    default:
      return new Error("whatsapp: the gateway failed");
  }
}

/**
 * The module's own words for a refusal: the error with its sentinel's text
 * removed, so what travels is what a person should read.
 */
export function sentence(err: unknown): string {
  if (!(err instanceof Error)) return err === null || err === undefined ? "" : String(err);

  let text = err.message;
  for (const sentinel of [
    ErrNotFound,
    ErrInvalidInput,
    ErrUnavailable,
    ErrLinkExpired,
    ErrNotConfigured,
  ]) {
    if (text.startsWith(`${sentinel.message}:`)) {
      text = text.slice(sentinel.message.length + 1);
    }
  }
  text = text.trim();

  if (text === "" || statusFor(err) === 500) return "internal error";
  return text;
}

/** Reports whether an error is or wraps the sentinel. */
function isError(err: unknown, sentinel: Error): boolean {
  let current: unknown = err;
  for (let guard = 0; guard < 32; guard++) {
    if (current === sentinel) return true;
    if (!(current instanceof Error)) return false;
    if ("sentinel" in current && current.sentinel instanceof Error) {
      current = current.sentinel;
      continue;
    }
    current = "cause" in current ? current.cause : undefined;
  }
  return false;
}

/** The API's side of the seam: it implements the Transport by calling the gateway. */
export class Client implements Transport {
  private readonly baseURL: string;
  private readonly token: string;
  private readonly timeout: number;

  /** An empty base URL or token leaves it unconfigured, and every call says so. */
  constructor(baseURL: string, token: string, timeoutMs = 30_000) {
    this.baseURL = baseURL.replace(/\/+$/, "");
    this.token = token;
    this.timeout = timeoutMs > 0 ? timeoutMs : 30_000;
  }

  /** Starts one attempt. */
  async link(scope: Scope, numberID: string, phone: string): Promise<Link> {
    return this.post<Link>(scope, `/numbers/${numberID}/link`, { phone });
  }

  /** Reports where an attempt stands. */
  async linkState(id: string): Promise<Link> {
    return this.get<Link>({ workspaceId: "" }, `/links/${id}`);
  }

  /** Asks for the phone-code alternative. */
  async pairCode(id: string, phone: string): Promise<Link> {
    return this.post<Link>({ workspaceId: "" }, `/links/${id}/pair-code`, { phone });
  }

  /** Abandons an attempt. */
  async cancel(id: string): Promise<void> {
    await this.request("DELETE", { workspaceId: "" }, `/links/${id}`, null);
  }

  /** Delivers one message. */
  async send(snapshot: Snapshot, message: ToSend): Promise<string> {
    const answer = await this.post<{ message_id: string }>(
      { workspaceId: snapshot.workspaceID },
      `/numbers/${snapshot.numberID}/send`,
      {
        id: message.id,
        to: message.toJID,
        kind: message.kind,
        text: message.text,
        media_key: message.mediaKey,
      },
    );
    return answer.message_id;
  }

  /** Shows the typing indicator. */
  async typing(snapshot: Snapshot, toJID: string): Promise<void> {
    await this.post(
      { workspaceId: snapshot.workspaceID },
      `/numbers/${snapshot.numberID}/typing`,
      { to: toJID },
    );
  }

  /** Ends a session. */
  async unlink(snapshot: Snapshot): Promise<void> {
    await this.post(
      { workspaceId: snapshot.workspaceID },
      `/numbers/${snapshot.numberID}/unlink`,
      null,
    );
  }

  /** Reports whether the gateway holds the number. */
  async online(numberID: string): Promise<boolean> {
    const answer = await this.get<{ online: boolean }>({ workspaceId: "" }, `/numbers/${numberID}/online`);
    return answer.online;
  }

  private async post<T>(scope: Scope, path: string, body: unknown): Promise<T> {
    return this.request<T>("POST", scope, path, body);
  }

  private async get<T>(scope: Scope, path: string): Promise<T> {
    return this.request<T>("GET", scope, path, null);
  }

  /**
   * Performs one call. The shared token goes on every request: the gateway
   * speaks for every tenant it serves, so there is no read-only path worth
   * leaving open.
   */
  private async request<T>(method: string, scope: Scope, path: string, body: unknown): Promise<T> {
    if (this.baseURL === "" || this.token === "") {
      throw wrap(ErrNotConfigured, "the gateway address and token are not configured");
    }

    const headers: Record<string, string> = {
      [TokenHeader]: this.token,
      "Content-Type": "application/json",
    };
    if (scope.workspaceId && scope.workspaceId !== NilUUID) {
      headers["X-Workspace-Id"] = scope.workspaceId;
    }

    let response: Response;
    try {
      response = await fetch(`${this.baseURL}${path}`, {
        method,
        headers,
        body: body === null ? undefined : JSON.stringify(body),
        signal: AbortSignal.timeout(this.timeout),
      });
    } catch (err) {
      // An unreachable gateway is not a refusal by the product: the message it
      // was asked to send is still queued, and the answer says the platform was
      // the problem.
      throw wrap(ErrNotConfigured, err instanceof Error ? err.message : String(err));
    }

    const payload = await response.text();

    if (response.status >= 400) {
      let message = "";
      try {
        const failure = JSON.parse(payload) as { error?: unknown };
        message = typeof failure.error === "string" ? failure.error.trim() : "";
      } catch {
        // Not JSON: the status line is all there is to say.
      }
      if (message === "") message = `${response.status} ${response.statusText}`;

      // The status is mapped back to the sentinel the other end put it in, and
      // that mapping is the whole reason this seam keeps the classification.
      throw wrap(sentinelForStatus(response.status), message);
    }

    if (payload.trim() === "") return undefined as T;
    return JSON.parse(payload) as T;
  }
}
