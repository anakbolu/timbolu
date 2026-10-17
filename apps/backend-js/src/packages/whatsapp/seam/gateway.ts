/**
 * The HTTP seam between the two processes.
 *
 * The gateway owns the connection and the API owns the product, so they talk
 * over one small surface: the nine things a number can be asked to do. The
 * surface is the Transport interface in both directions — the API holds a
 * client that implements it, and the gateway serves it — which is what keeps
 * the API process from importing whatsmeow and the gateway from importing a
 * router.
 *
 * Ported 1:1 from packages/whatsapp/seam/gateway.go. Paths, header names,
 * status codes and error bodies are the running wire contract; the Go side of
 * this seam is `apps/whatsapp-gw`, so nothing here may drift.
 *
 * It is an internal address. The gateway is not publicly reachable: it speaks
 * for every tenant it serves, and a caller who reached it could link a number
 * into any workspace.
 */

import type { Context, Hono } from "hono";

import { isUuid } from "../../../platform/context.ts";
import type { Logger } from "../../../platform/logger.ts";
import { nopLogger } from "../../../platform/logger.ts";
import type { Link, Scope, Snapshot, ToSend, Transport } from "../domain/whatsapp.ts";
import {
  ErrAlreadyLinked,
  ErrInvalidInput,
  ErrLinkExpired,
  ErrNoAgent,
  ErrNotConfigured,
  ErrNotFound,
  ErrNotConnected,
  ErrTermsNotAccepted,
  ErrUnavailable,
  NilUUID,
  is,
  scopeIsZero,
  wrap,
} from "../domain/whatsapp.ts";

/** The shared secret that authenticates the API to the gateway. */
export const TokenHeader = "X-Wa-Gateway-Token";

/** How a deployment lets the API reach the gateway. */
export interface GatewayConfig {
  /** Authenticates the caller. Empty refuses every request. */
  Token: string;
  /** Bounds a request body in bytes. Zero or less means one megabyte. */
  MaxBody: number;
}

/** Serves one runtime over HTTP for the API process. */
export class Gateway {
  private readonly engine: Transport;
  private readonly cfg: GatewayConfig;
  private readonly log: Logger;

  constructor(transport: Transport, cfg: GatewayConfig, log?: Logger | null) {
    this.engine = transport;
    this.cfg = {
      Token: cfg.Token,
      MaxBody: cfg.MaxBody > 0 ? cfg.MaxBody : 1 << 20,
    };
    this.log = log ?? nopLogger;
  }

  /**
   * The gateway's whole surface.
   *
   * The paths name the number they act on because the engine has to know which
   * session to use, and the workspace travels in a header rather than in the
   * path because it is the tenant the operation runs in rather than part of the
   * resource.
   */
  routes(app: Hono): void {
    app.get("/health", (c) => this.health());
    app.post("/numbers/:numberID/link", (c) => this.link(c));
    app.get("/links/:linkID", (c) => this.linkState(c));
    app.post("/links/:linkID/pair-code", (c) => this.pairCode(c));
    app.delete("/links/:linkID", (c) => this.cancelLink(c));
    app.post("/numbers/:numberID/send", (c) => this.send(c));
    app.post("/numbers/:numberID/typing", (c) => this.typing(c));
    app.post("/numbers/:numberID/unlink", (c) => this.unlink(c));
    app.get("/numbers/:numberID/online", (c) => this.online(c));
  }

  /** Answers whether the process is up. It does not claim any number is online. */
  private health = (): Response => json(200, { status: "ok" });

  /** Starts one attempt. */
  private link = async (c: Context): Promise<Response> => {
    const request = c.req.raw;
    const scope = this.scoped(request);
    if (scope instanceof Response) return scope;

    const numberID = this.numberID(c.req.param("numberID") ?? "");
    if (numberID instanceof Response) return numberID;

    const body = await this.decode<{ phone?: string }>(request);
    if (body instanceof Response) return body;

    try {
      const link = await this.engine.link(scope, numberID, body.phone ?? "");
      return json(200, link);
    } catch (err) {
      return this.fail("start link", err);
    }
  };

  /** Reports where an attempt stands. */
  private linkState = async (c: Context): Promise<Response> => {
    const request = c.req.raw;
    try {
      return json(200, await this.engine.linkState(c.req.param("linkID") ?? ""));
    } catch (err) {
      return this.fail("read link", err);
    }
  };

  /** Asks for the phone-code alternative. */
  private pairCode = async (c: Context): Promise<Response> => {
    const request = c.req.raw;
    const body = await this.decode<{ phone?: string }>(request);
    if (body instanceof Response) return body;

    try {
      const link = await this.engine.pairCode(c.req.param("linkID") ?? "", body.phone ?? "");
      return json(200, link);
    } catch (err) {
      return this.fail("pair by phone", err);
    }
  };

  /** Abandons an attempt. */
  private cancelLink = async (c: Context): Promise<Response> => {
    const request = c.req.raw;
    try {
      await this.engine.cancel(c.req.param("linkID") ?? "");
      return json(200, { status: "cancelled" });
    } catch (err) {
      return this.fail("cancel link", err);
    }
  };

  /** Delivers one message. */
  private send = async (c: Context): Promise<Response> => {
    const request = c.req.raw;
    const scope = this.scoped(request);
    if (scope instanceof Response) return scope;

    const numberID = this.numberID(c.req.param("numberID") ?? "");
    if (numberID instanceof Response) return numberID;

    const body = await this.decode<SendPayload>(request);
    if (body instanceof Response) return body;

    // Go's `uuid.UUID` rejects an empty string as well as a malformed one, so a
    // present `id` that is not a uuid is a decode failure.
    if (body.id !== undefined && !isUuid(body.id)) {
      return json(400, { error: "the body is not valid JSON" });
    }

    try {
      // The engine reads the phone and the sealed session itself; the Go side
      // passes only the identity of the number.
      const snapshot: Snapshot = {
        NumberID: numberID,
        WorkspaceID: scope.WorkspaceID,
        Phone: "",
        Session: null,
      };
      const message: ToSend = {
        ID: body.id ?? NilUUID,
        ToJID: body.to ?? "",
        Kind: body.kind ?? "",
        Text: body.text ?? "",
        MediaKey: body.media_key ?? "",
      };
      const id = await this.engine.send(snapshot, message);
      return json(200, { message_id: id });
    } catch (err) {
      return this.fail("send", err);
    }
  };

  /** Shows the typing indicator. */
  private typing = async (c: Context): Promise<Response> => {
    const request = c.req.raw;
    const scope = this.scoped(request);
    if (scope instanceof Response) return scope;

    const numberID = this.numberID(c.req.param("numberID") ?? "");
    if (numberID instanceof Response) return numberID;

    const body = await this.decode<{ to?: string }>(request);
    if (body instanceof Response) return body;

    try {
      const snapshot: Snapshot = {
        NumberID: numberID,
        WorkspaceID: scope.WorkspaceID,
        Phone: "",
        Session: null,
      };
      await this.engine.typing(snapshot, body.to ?? "");
      return json(200, { status: "ok" });
    } catch (err) {
      return this.fail("typing", err);
    }
  };

  /** Ends a session. */
  private unlink = async (c: Context): Promise<Response> => {
    const request = c.req.raw;
    const scope = this.scoped(request);
    if (scope instanceof Response) return scope;

    const numberID = this.numberID(c.req.param("numberID") ?? "");
    if (numberID instanceof Response) return numberID;

    try {
      const snapshot: Snapshot = {
        NumberID: numberID,
        WorkspaceID: scope.WorkspaceID,
        Phone: "",
        Session: null,
      };
      await this.engine.unlink(snapshot);
      return json(200, { status: "unlinked" });
    } catch (err) {
      return this.fail("unlink", err);
    }
  };

  /**
   * Reports whether this instance holds the number.
   *
   * It needs the token but no tenant: the answer is about this process rather
   * than about a workspace's data. The token is still checked, because an
   * unauthenticated caller could otherwise enumerate how many numbers a
   * deployment serves.
   */
  private online = async (c: Context): Promise<Response> => {
    const request = c.req.raw;
    const denied = this.authorized(request);
    if (denied) return denied;

    const numberID = this.numberID(c.req.param("numberID") ?? "");
    if (numberID instanceof Response) return numberID;

    try {
      return json(200, { online: await this.engine.online(numberID) });
    } catch (err) {
      return this.fail("read online", err);
    }
  };

  /**
   * Checks the shared secret. Returns the refusal to send, or null when the
   * caller may proceed.
   */
  private authorized(request: Request): Response | null {
    if (this.cfg.Token === "" || request.headers.get(TokenHeader) !== this.cfg.Token) {
      return json(401, { error: "unauthorized" });
    }
    return null;
  }

  /** Reads and checks the tenant a request runs in. */
  private scoped(request: Request): Scope | Response {
    const denied = this.authorized(request);
    if (denied) return denied;

    const raw = (request.headers.get("X-Workspace-Id") ?? "").trim();
    if (raw === "") {
      return json(400, { error: "workspace is required" });
    }
    if (!isUuid(raw)) {
      return json(400, { error: "workspace is not a uuid" });
    }

    return { UserID: NilUUID, WorkspaceID: raw };
  }

  /** Reads the number a path names. */
  private numberID(id: string): string | Response {
    if (!isUuid(id)) {
      return json(400, { error: "number is not a uuid" });
    }
    return id;
  }

  /**
   * Reads a JSON body. An empty body leaves the target's zero values in place,
   * exactly as Go's `json.Unmarshal` of an empty slice never runs.
   */
  private async decode<T extends object>(request: Request): Promise<T | Response> {
    let raw: string;
    try {
      const bytes = await request.arrayBuffer();
      raw = new TextDecoder().decode(bytes.slice(0, this.cfg.MaxBody));
    } catch {
      return json(400, { error: "the body could not be read" });
    }

    if (raw.trim() === "") return {} as T;

    try {
      const parsed: unknown = JSON.parse(raw);
      // Go's `json.Unmarshal("null", &struct)` is a no-op success; an array or a
      // scalar is a type error.
      if (parsed === null) return {} as T;
      if (typeof parsed !== "object" || Array.isArray(parsed)) {
        return json(400, { error: "the body is not valid JSON" });
      }
      return parsed as T;
    } catch {
      return json(400, { error: "the body is not valid JSON" });
    }
  }

  /**
   * Maps the engine's errors onto statuses.
   *
   * What crosses the wire is the *sentence* the module wrote, with its own
   * prefix removed, rather than the rendered error: the API turns the status
   * back into the same sentinel and wraps it, so a message that still carried
   * its prefix would arrive wrapped twice.
   */
  private fail(operation: string, err: unknown): Response {
    const status = StatusFor(err);

    // A failure nobody classified is logged with its own words: without this
    // line an unclassified error is a 500 with no trace at all.
    if (status === 500) {
      this.log.error({ operation, err }, "a gateway request failed");
    }

    return json(status, { error: Sentence(err) });
  }
}

/** One message to deliver, as the gateway's `sendPayload`. */
interface SendPayload {
  to?: string;
  kind?: string;
  text?: string;
  media_key?: string;
  id?: string;
}

/** Mounts the gateway's routes on an app, mirroring Go's `Routes(mux)`. */
export function routes(app: Hono, gateway: Gateway): void {
  gateway.routes(app);
}

/**
 * The status one error becomes, which is the mapping the client reverses. The
 * two are one table written twice, so a change to either is a change to the
 * seam.
 */
export function StatusFor(err: unknown): number {
  if (is(err, ErrNotFound)) return 404;
  if (is(err, ErrInvalidInput) || is(err, ErrTermsNotAccepted)) return 400;
  if (is(err, ErrAlreadyLinked) || is(err, ErrNoAgent)) return 409;
  if (is(err, ErrNotConnected) || is(err, ErrUnavailable)) return 503;
  if (is(err, ErrLinkExpired)) return 410;
  return 500;
}

/**
 * The module's own words for a refusal: the error with its sentinel's text
 * removed, so what travels is what a person should read.
 */
export function Sentence(err: unknown): string {
  if (err === null || err === undefined) return "";

  let text = err instanceof Error ? err.message : String(err);
  for (const sentinel of [
    ErrNotFound,
    ErrInvalidInput,
    ErrTermsNotAccepted,
    ErrAlreadyLinked,
    ErrNotConnected,
    ErrUnavailable,
    ErrLinkExpired,
    ErrNoAgent,
    ErrNotConfigured,
  ]) {
    const prefix = `${sentinel.message}:`;
    if (text.startsWith(prefix)) text = text.slice(prefix.length);
    text = text.trim();
  }

  // A failure nobody classified is not something to hand a caller.
  if (text.trim() === "" || StatusFor(err) === 500) return "internal error";
  return text;
}

/**
 * Maps a gateway status back to the error the module uses. `fail` decides which
 * status each sentinel becomes, and this is the other direction of the same
 * table.
 */
export function SentinelForStatus(status: number): Error {
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
 * The API's side of the seam: it implements the Transport the module declares
 * by calling the gateway.
 */
export class Client implements Transport {
  private readonly baseURL: string;
  private readonly token: string;
  private readonly timeout: number;

  /**
   * An empty base URL or token leaves it unconfigured, and every call reports
   * that rather than reaching a URL nobody meant. `timeoutMs` defaults to 30s.
   */
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
    return this.get<Link>({ UserID: NilUUID, WorkspaceID: NilUUID }, `/links/${id}`);
  }

  /** Asks for the phone-code alternative. */
  async pairCode(id: string, phone: string): Promise<Link> {
    return this.post<Link>({ UserID: NilUUID, WorkspaceID: NilUUID }, `/links/${id}/pair-code`, { phone });
  }

  /** Abandons an attempt. */
  async cancel(id: string): Promise<void> {
    await this.request("DELETE", { UserID: NilUUID, WorkspaceID: NilUUID }, `/links/${id}`, null);
  }

  /** Delivers one message. */
  async send(snapshot: Snapshot, message: ToSend): Promise<string> {
    const response = await this.post<{ message_id: string }>(
      { UserID: NilUUID, WorkspaceID: snapshot.WorkspaceID },
      `/numbers/${snapshot.NumberID}/send`,
      {
        id: message.ID,
        to: message.ToJID,
        kind: message.Kind,
        text: message.Text,
        media_key: message.MediaKey,
      },
    );
    return response.message_id;
  }

  /** Shows the typing indicator. */
  async typing(snapshot: Snapshot, toJID: string): Promise<void> {
    await this.post(
      { UserID: NilUUID, WorkspaceID: snapshot.WorkspaceID },
      `/numbers/${snapshot.NumberID}/typing`,
      { to: toJID },
    );
  }

  /** Ends a session. */
  async unlink(snapshot: Snapshot): Promise<void> {
    await this.post(
      { UserID: NilUUID, WorkspaceID: snapshot.WorkspaceID },
      `/numbers/${snapshot.NumberID}/unlink`,
      null,
    );
  }

  /** Reports whether the gateway holds the number. */
  async online(numberID: string): Promise<boolean> {
    const response = await this.get<{ online: boolean }>(
      { UserID: NilUUID, WorkspaceID: NilUUID },
      `/numbers/${numberID}/online`,
    );
    return response.online;
  }

  private async post<T>(scope: Scope, path: string, body: unknown): Promise<T> {
    return this.request<T>("POST", scope, path, body);
  }

  private async get<T>(scope: Scope, path: string): Promise<T> {
    return this.request<T>("GET", scope, path, null);
  }

  /** Performs one call. */
  private async request<T>(method: string, scope: Scope, path: string, body: unknown): Promise<T> {
    if (this.baseURL === "" || this.token === "") {
      throw wrap(ErrNotConfigured, "the gateway address and token are not configured");
    }

    const headers: Record<string, string> = {
      [TokenHeader]: this.token,
      "Content-Type": "application/json",
    };
    if (!scopeIsZero(scope)) {
      headers["X-Workspace-Id"] = scope.WorkspaceID;
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

      // The status is mapped back to the sentinel the other end put it in.
      throw wrap(SentinelForStatus(response.status), message);
    }

    if (payload.trim() === "") return undefined as T;
    return JSON.parse(payload) as T;
  }
}

/**
 * Writes one JSON answer, as Go's `writeJSON`. `json.Encoder.Encode` appends a
 * newline, and this keeps the body byte-identical to what the Go gateway emits.
 */
function json(status: number, payload: unknown): Response {
  return new Response(`${JSON.stringify(payload)}\n`, {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

