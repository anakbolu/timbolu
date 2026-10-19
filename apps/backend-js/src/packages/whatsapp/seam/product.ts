/**
 * The second half of the seam: what the gateway asks the product to do.
 *
 * The gateway owns the socket and the backend owns the product, so the four
 * things the engine does with the product — it records what arrived, pulls what
 * is due, reports what left, and says how a number is doing — travel back over
 * the same internal address the other direction uses. The two directions share
 * one secret, one status table, and one sentence: a refusal reads the same
 * whichever way it crosses.
 *
 * Ported 1:1 from packages/whatsapp/seam/product.go. The wire shapes at the
 * bottom are declared here rather than reusing the domain structs so the two
 * processes agree on a document rather than on a TS type: the JSON tags are
 * what actually cross.
 */

import type { Context, Hono } from "hono";

import { isUuid } from "../../../platform/context.ts";
import type { Logger } from "../../../platform/logger.ts";
import { nopLogger } from "../../../platform/logger.ts";
import type { Claimed, Drain, Inbox, LinkSink, PairedLink, Status } from "../domain/engine.ts";
import type { ReportRequest } from "../domain/service.ts";
import type { Inbound, Media, Number as WaNumber, Outbox, Scope } from "../domain/whatsapp.ts";
import { ErrNotConfigured, ErrUnavailable, NilUUID, scopeIsZero, wrap } from "../domain/whatsapp.ts";
import { Sentence, SentinelForStatus, StatusFor, TokenHeader } from "./gateway.ts";

/** How the product side authenticates the gateway. */
export interface ProductConfig {
  /** The same shared secret the gateway is reached with. Empty refuses every call. */
  Token: string;
  /** Bounds a request body in bytes. Zero or less means one megabyte. */
  MaxBody: number;
}

/** The ports the product side serves. */
export interface ProductDeps {
  Inbox: Inbox;
  Drain: Drain;
  Status: Status;
  Links: LinkSink;
}

/** Serves the gateway's requests to the backend. */
export class Product {
  private readonly deps: ProductDeps;
  private readonly cfg: ProductConfig;
  private readonly log: Logger;

  constructor(deps: ProductDeps, cfg: ProductConfig, log?: Logger | null) {
    this.deps = deps;
    this.cfg = {
      Token: cfg.Token,
      MaxBody: cfg.MaxBody > 0 ? cfg.MaxBody : 1 << 20,
    };
    this.log = log ?? nopLogger;
  }

  /**
   * The product surface's whole route table. The workspace travels in the same
   * header the other direction uses: it is the tenant the operation runs in
   * rather than part of the resource.
   */
  routes(app: Hono): void {
    app.post("/internal/whatsapp/numbers/:numberID/inbound", (c) => this.receive(c));
    app.post("/internal/whatsapp/media/pending", (c) => this.pendingMedia(c));
    app.post("/internal/whatsapp/messages/:messageID/media", (c) => this.storeMedia(c));
    app.post("/internal/whatsapp/numbers/:numberID/outbox/claim", (c) => this.claim(c));
    app.post("/internal/whatsapp/outbox/reclaim", (c) => this.reclaim(c));
    app.post("/internal/whatsapp/outbox/:messageID/sent", (c) => this.sent(c));
    app.post("/internal/whatsapp/outbox/:messageID/retry", (c) => this.retry(c));
    app.post("/internal/whatsapp/outbox/:messageID/failed", (c) => this.failed(c));
    app.get("/internal/whatsapp/numbers/:numberID", (c) => this.number(c));
    app.post("/internal/whatsapp/numbers/:numberID/status", (c) => this.report(c));
    app.post("/internal/whatsapp/numbers/:numberID/paired", (c) => this.paired(c));
  }

  /** Records one inbound message. */
  private receive = async (c: Context): Promise<Response> => {
    const scope = this.scoped(c);
    if (scope instanceof Response) return scope;

    const numberID = this.pathID(c, "numberID", "number");
    if (numberID instanceof Response) return numberID;

    const body = await this.body(c, inboundFromJSON);
    if (body instanceof Response) return body;

    try {
      const stored = await this.deps.Inbox.receive(scope, numberID, inboundToDomain(body));
      return json(200, { message: fromInbound(stored.message), created: stored.created });
    } catch (err) {
      return this.fail("receive", err);
    }
  };

  /** Returns the messages whose bytes have not been fetched. */
  private pendingMedia = async (c: Context): Promise<Response> => {
    const scope = this.scoped(c);
    if (scope instanceof Response) return scope;

    const body = await this.body(c, (raw) => ({ limit: num(raw, "limit") }));
    if (body instanceof Response) return body;

    try {
      const pending = await this.deps.Inbox.pendingMedia(scope, body.limit);
      return json(200, { messages: pending.map(fromInbound) });
    } catch (err) {
      return this.fail("pending media", err);
    }
  };

  /** Records where a message's bytes were stored. */
  private storeMedia = async (c: Context): Promise<Response> => {
    const scope = this.scoped(c);
    if (scope instanceof Response) return scope;

    const messageID = this.pathID(c, "messageID", "message");
    if (messageID instanceof Response) return messageID;

    const body = await this.body(c, mediaFromJSON);
    if (body instanceof Response) return body;

    try {
      await this.deps.Inbox.storeMedia(scope, messageID, mediaToDomain(body));
      return json(200, { status: "stored" });
    } catch (err) {
      return this.fail("store media", err);
    }
  };

  /** Returns the messages of one number that may go out now. */
  private claim = async (c: Context): Promise<Response> => {
    const scope = this.scoped(c);
    if (scope instanceof Response) return scope;

    const numberID = this.pathID(c, "numberID", "number");
    if (numberID instanceof Response) return numberID;

    const body = await this.body(c, (raw) => ({ limit: num(raw, "limit") }));
    if (body instanceof Response) return body;

    try {
      const claimed = await this.deps.Drain.claim(scope, numberID, body.limit);
      return json(200, { ready: claimed.Ready.map(fromOutbox), held: claimed.Held });
    } catch (err) {
      return this.fail("claim", err);
    }
  };

  /** Returns the messages a dead gateway left mid-send to the queue. */
  private reclaim = async (c: Context): Promise<Response> => {
    const scope = this.scoped(c);
    if (scope instanceof Response) return scope;

    const body = await this.body(c, (raw) => ({ older_than: time(raw, "older_than") }));
    if (body instanceof Response) return body;

    try {
      const reclaimed = await this.deps.Drain.reclaim(scope, body.older_than);
      return json(200, { reclaimed });
    } catch (err) {
      return this.fail("reclaim", err);
    }
  };

  /** Records that a message left. */
  private sent = async (c: Context): Promise<Response> => {
    const named = this.outboxRequest(c);
    if (named instanceof Response) return named;

    const body = await this.body(c, (raw) => ({
      wa_message_id: str(raw, "wa_message_id"),
      at: time(raw, "at"),
    }));
    if (body instanceof Response) return body;

    try {
      await this.deps.Drain.sent(named.scope, named.messageID, body.wa_message_id, body.at);
      return json(200, { status: "sent" });
    } catch (err) {
      return this.fail("mark sent", err);
    }
  };

  /** Puts a message back in the queue with a later time. */
  private retry = async (c: Context): Promise<Response> => {
    const named = this.outboxRequest(c);
    if (named instanceof Response) return named;

    const body = await this.body(c, (raw) => ({
      reason: str(raw, "reason"),
      at: time(raw, "at"),
    }));
    if (body instanceof Response) return body;

    try {
      await this.deps.Drain.retry(named.scope, named.messageID, body.reason, body.at);
      return json(200, { status: "retried" });
    } catch (err) {
      return this.fail("retry", err);
    }
  };

  /** Records that a message will not be retried. */
  private failed = async (c: Context): Promise<Response> => {
    const named = this.outboxRequest(c);
    if (named instanceof Response) return named;

    const body = await this.body(c, (raw) => ({
      reason: str(raw, "reason"),
      attempts: num(raw, "attempts"),
    }));
    if (body instanceof Response) return body;

    try {
      await this.deps.Drain.failed(named.scope, named.messageID, body.reason, body.attempts);
      return json(200, { status: "failed" });
    } catch (err) {
      return this.fail("mark failed", err);
    }
  };

  /** Returns one number, which the engine reads for its phone and ceiling. */
  private number = async (c: Context): Promise<Response> => {
    const scope = this.scoped(c);
    if (scope instanceof Response) return scope;

    const numberID = this.pathID(c, "numberID", "number");
    if (numberID instanceof Response) return numberID;

    try {
      return json(200, fromNumber(await this.deps.Status.number(scope, numberID)));
    } catch (err) {
      return this.fail("read number", err);
    }
  };

  /** Records what the transport knows about a number. */
  private report = async (c: Context): Promise<Response> => {
    const scope = this.scoped(c);
    if (scope instanceof Response) return scope;

    const numberID = this.pathID(c, "numberID", "number");
    if (numberID instanceof Response) return numberID;

    const body = await this.body(c, (raw) => ({
      status: str(raw, "status"),
      reason: str(raw, "reason"),
      at: time(raw, "at"),
    }));
    if (body instanceof Response) return body;

    try {
      const number = await this.deps.Status.report(scope, {
        NumberID: numberID,
        Status: body.status,
        Reason: body.reason,
        At: body.at,
      });
      return json(200, fromNumber(number));
    } catch (err) {
      return this.fail("report", err);
    }
  };

  /** Records that a phone finished pairing. */
  private paired = async (c: Context): Promise<Response> => {
    const scope = this.scoped(c);
    if (scope instanceof Response) return scope;

    const numberID = this.pathID(c, "numberID", "number");
    if (numberID instanceof Response) return numberID;

    const body = await this.body(c, (raw) => ({
      phone: str(raw, "phone"),
      jid: str(raw, "jid"),
    }));
    if (body instanceof Response) return body;

    try {
      await this.deps.Links.paired(scope, {
        NumberID: numberID,
        Phone: body.phone,
        JID: body.jid,
      });
      return json(200, { status: "paired" });
    } catch (err) {
      return this.fail("paired", err);
    }
  };

  /** Reads the tenant and the message a call names. */
  private outboxRequest(c: Context): { scope: Scope; messageID: string } | Response {
    const scope = this.scoped(c);
    if (scope instanceof Response) return scope;

    const messageID = this.pathID(c, "messageID", "message");
    if (messageID instanceof Response) return messageID;

    return { scope, messageID };
  }

  /** Reads and checks the tenant a request runs in. */
  private scoped(c: Context): Scope | Response {
    const denied = this.authorized(c);
    if (denied) return denied;

    const raw = (c.req.header("X-Workspace-Id") ?? "").trim();
    if (raw === "") {
      return json(400, { error: "workspace is required" });
    }
    if (!isUuid(raw)) {
      return json(400, { error: "workspace is not a uuid" });
    }

    return { UserID: NilUUID, WorkspaceID: raw };
  }

  /** Checks the shared secret. */
  private authorized(c: Context): Response | null {
    if (this.cfg.Token === "" || c.req.header(TokenHeader) !== this.cfg.Token) {
      return json(401, { error: "unauthorized" });
    }
    return null;
  }

  /** Reads one uuid the path names. */
  private pathID(c: Context, name: string, what: string): string | Response {
    const id = c.req.param(name) ?? "";
    if (!isUuid(id)) {
      return json(400, { error: `${what} is not a uuid` });
    }
    return id;
  }

  /**
   * Reads a JSON body and hands the parsed document to `parse`, which raises on
   * a field of the wrong shape. That is the same refusal Go gives when
   * `json.Unmarshal` cannot fill the target.
   */
  private async body<T>(c: Context, parse: (raw: Record<string, unknown>) => T): Promise<T | Response> {
    let text: string;
    try {
      const bytes = await c.req.arrayBuffer();
      text = new TextDecoder().decode(bytes.slice(0, this.cfg.MaxBody));
    } catch {
      return json(400, { error: "the body could not be read" });
    }

    let parsed: unknown = {};
    if (text.trim() !== "") {
      try {
        parsed = JSON.parse(text);
      } catch {
        return json(400, { error: "the body is not valid JSON" });
      }
    }

    // Go's `json.Unmarshal("null", &struct)` is a no-op success.
    if (parsed === null) parsed = {};
    if (typeof parsed !== "object" || Array.isArray(parsed)) {
      return json(400, { error: "the body is not valid JSON" });
    }

    try {
      return parse(parsed as Record<string, unknown>);
    } catch {
      return json(400, { error: "the body is not valid JSON" });
    }
  }

  /** Maps the product's errors onto statuses with the one shared table. */
  private fail(operation: string, err: unknown): Response {
    const status = StatusFor(err);

    if (status === 500) {
      this.log.error({ operation, err }, "a product request failed");
    }

    return json(status, { error: Sentence(err) });
  }
}

/** Mounts the product's routes on an app, mirroring Go's `Routes(mux)`. */
export function routes(app: Hono, product: Product): void {
  product.routes(app);
}

// --------------------------------------------------------------------------
// The client the gateway holds.
// --------------------------------------------------------------------------

/** The gateway's side: it implements the four ports the engine declares. */
export class ProductClient implements Inbox, Drain, Status, LinkSink {
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

  /** Records one inbound message. */
  async receive(
    scope: Scope,
    numberID: string,
    message: Inbound,
  ): Promise<{ message: Inbound; created: boolean }> {
    const response = await this.post<{ message: InboundWire; created: boolean }>(
      scope,
      `/internal/whatsapp/numbers/${numberID}/inbound`,
      fromInbound(message),
    );
    return { message: inboundToDomain(response.message), created: response.created };
  }

  /** Returns the messages whose bytes have not been fetched. */
  async pendingMedia(scope: Scope, limit: number): Promise<Inbound[]> {
    const response = await this.post<{ messages: InboundWire[] }>(
      scope,
      "/internal/whatsapp/media/pending",
      { limit },
    );
    return response.messages.map(inboundToDomain);
  }

  /** Records where a message's bytes were stored. */
  async storeMedia(scope: Scope, id: string, media: Media): Promise<void> {
    await this.post(scope, `/internal/whatsapp/messages/${id}/media`, fromMedia(media));
  }

  /** Returns the messages of one number that may go out now. */
  async claim(scope: Scope, numberID: string, limit: number): Promise<Claimed> {
    const response = await this.post<{ ready: OutboxWire[]; held: number }>(
      scope,
      `/internal/whatsapp/numbers/${numberID}/outbox/claim`,
      { limit },
    );
    return { Ready: response.ready.map(outboxToDomain), Held: response.held };
  }

  /** Records that a message left. */
  async sent(scope: Scope, id: string, waMessageID: string, at: Date): Promise<void> {
    await this.post(scope, `/internal/whatsapp/outbox/${id}/sent`, {
      wa_message_id: waMessageID,
      at,
    });
  }

  /** Puts a message back in the queue with a later time. */
  async retry(scope: Scope, id: string, reason: string, at: Date): Promise<void> {
    await this.post(scope, `/internal/whatsapp/outbox/${id}/retry`, { reason, at });
  }

  /** Records that a message will not be retried. */
  async failed(scope: Scope, id: string, reason: string, attempts: number): Promise<void> {
    await this.post(scope, `/internal/whatsapp/outbox/${id}/failed`, { reason, attempts });
  }

  /** Puts messages a dead gateway left mid-send back in the queue. */
  async reclaim(scope: Scope, olderThan: Date): Promise<number> {
    const response = await this.post<{ reclaimed: number }>(scope, "/internal/whatsapp/outbox/reclaim", {
      older_than: olderThan,
    });
    return response.reclaimed;
  }

  /** Returns one number. */
  async number(scope: Scope, id: string): Promise<WaNumber> {
    const wire = await this.get<NumberWire>(scope, `/internal/whatsapp/numbers/${id}`);
    return numberToDomain(wire);
  }

  /** Records what the transport knows about a number. */
  async report(scope: Scope, request: ReportRequest): Promise<WaNumber> {
    const wire = await this.post<NumberWire>(
      scope,
      `/internal/whatsapp/numbers/${request.NumberID}/status`,
      { status: request.Status, reason: request.Reason, at: request.At },
    );
    return numberToDomain(wire);
  }

  /** Records that a phone finished pairing. */
  async paired(scope: Scope, paired: PairedLink): Promise<void> {
    await this.post(scope, `/internal/whatsapp/numbers/${paired.NumberID}/paired`, {
      phone: paired.Phone,
      jid: paired.JID,
    });
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
      throw wrap(ErrNotConfigured, "the backend address and token are not configured");
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
      // An unreachable backend is not a refusal by the product: the message is
      // still queued, and the answer says the platform was the problem.
      throw wrap(ErrUnavailable, err instanceof Error ? err.message : String(err));
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

      throw wrap(SentinelForStatus(response.status), message);
    }

    if (payload.trim() === "") return undefined as T;
    return JSON.parse(payload) as T;
  }
}

// --------------------------------------------------------------------------
// The wire shapes.
// --------------------------------------------------------------------------

/** A message attachment, as it crosses the seam. */
export interface MediaWire {
  ref: string;
  kind: string;
  size: number;
  status: string;
  key: string;
  error: string;
}

/** One inbound message, as it crosses the seam. */
export interface InboundWire {
  id: string;
  workspace_id: string;
  number_id: string;
  wa_message_id: string;
  chat_jid: string;
  sender_jid: string;
  sender_name: string;
  is_group: boolean;
  is_from_me: boolean;
  kind: string;
  text: string;
  media: MediaWire;
  received_at: string;
  task_id: string;
  created_at: string;
}

/** One outbox message, as it crosses the seam. */
export interface OutboxWire {
  id: string;
  workspace_id: string;
  number_id: string;
  to_jid: string;
  kind: string;
  text: string;
  media_key: string;
  draft_id: string;
  status: string;
  attempts: number;
  last_error: string;
  not_before: string;
  claimed_at: string | null;
  wa_message_id: string;
  sent_at: string | null;
  created_at: string;
}

/** One number, as it crosses the seam. */
export interface NumberWire {
  id: string;
  workspace_id: string;
  integration_id: string;
  phone: string;
  label: string;
  agent_id: string;
  status: string;
  status_reason: string;
  tos_accepted_at: string | null;
  hourly_limit: number;
  connected_at: string | null;
  last_seen_at: string | null;
  created_at: string;
  updated_at: string;
}

/** Go's zero `time.Time`, which is what an absent field unmarshals to. */
const ZERO_TIME = new Date("0001-01-01T00:00:00Z");

/** Builds the wire form of an attachment. */
export function fromMedia(media: Media): MediaWire {
  return {
    ref: media.Ref,
    kind: media.Kind,
    size: media.Size,
    status: media.Status,
    key: media.Key,
    error: media.Error,
  };
}

/** Reads the wire form of an attachment. */
export function mediaToDomain(m: MediaWire): Media {
  return {
    Ref: m.ref,
    Kind: m.kind,
    Size: m.size,
    Status: m.status,
    Key: m.key,
    Error: m.error,
  };
}

/** Parses an attachment out of a JSON body. */
function mediaFromJSON(raw: Record<string, unknown>): MediaWire {
  return {
    ref: str(raw, "ref"),
    kind: str(raw, "kind"),
    size: num(raw, "size"),
    status: str(raw, "status"),
    key: str(raw, "key"),
    error: str(raw, "error"),
  };
}

/** Builds the wire form of an inbound message. */
export function fromInbound(message: Inbound): InboundWire {
  return {
    id: message.ID,
    workspace_id: message.WorkspaceID,
    number_id: message.NumberID,
    wa_message_id: message.WaMessageID,
    chat_jid: message.ChatJID,
    sender_jid: message.SenderJID,
    sender_name: message.SenderName,
    is_group: message.IsGroup,
    is_from_me: message.IsFromMe,
    kind: message.Kind,
    text: message.Text,
    media: fromMedia(message.Media),
    received_at: message.ReceivedAt.toISOString(),
    task_id: message.TaskID,
    created_at: message.CreatedAt.toISOString(),
  };
}

/** Reads the wire form of an inbound message. */
export function inboundToDomain(m: InboundWire): Inbound {
  return {
    ID: m.id,
    WorkspaceID: m.workspace_id,
    NumberID: m.number_id,
    WaMessageID: m.wa_message_id,
    ChatJID: m.chat_jid,
    SenderJID: m.sender_jid,
    SenderName: m.sender_name,
    IsGroup: m.is_group,
    IsFromMe: m.is_from_me,
    Kind: m.kind,
    Text: m.text,
    Media: mediaToDomain(m.media),
    ReceivedAt: new Date(m.received_at),
    TaskID: m.task_id,
    CreatedAt: new Date(m.created_at),
  };
}

/** Parses an inbound message out of a JSON body. */
function inboundFromJSON(raw: Record<string, unknown>): InboundWire {
  const media = raw["media"];
  return {
    id: uuid(raw, "id"),
    workspace_id: uuid(raw, "workspace_id"),
    number_id: uuid(raw, "number_id"),
    wa_message_id: str(raw, "wa_message_id"),
    chat_jid: str(raw, "chat_jid"),
    sender_jid: str(raw, "sender_jid"),
    sender_name: str(raw, "sender_name"),
    is_group: bool(raw, "is_group"),
    is_from_me: bool(raw, "is_from_me"),
    kind: str(raw, "kind"),
    text: str(raw, "text"),
    media:
      media === undefined || media === null
        ? mediaFromJSON({})
        : mediaFromJSON(object(media, "media")),
    received_at: time(raw, "received_at").toISOString(),
    task_id: uuid(raw, "task_id"),
    created_at: time(raw, "created_at").toISOString(),
  };
}

/** Builds the wire form of an outbox message. */
export function fromOutbox(item: Outbox): OutboxWire {
  return {
    id: item.ID,
    workspace_id: item.WorkspaceID,
    number_id: item.NumberID,
    to_jid: item.ToJID,
    kind: item.Kind,
    text: item.Text,
    media_key: item.MediaKey,
    draft_id: item.DraftID,
    status: item.Status,
    attempts: item.Attempts,
    last_error: item.LastError,
    not_before: item.NotBefore.toISOString(),
    claimed_at: item.ClaimedAt === null ? null : item.ClaimedAt.toISOString(),
    wa_message_id: item.WaMessageID,
    sent_at: item.SentAt === null ? null : item.SentAt.toISOString(),
    created_at: item.CreatedAt.toISOString(),
  };
}

/** Reads the wire form of an outbox message. */
export function outboxToDomain(m: OutboxWire): Outbox {
  return {
    ID: m.id,
    WorkspaceID: m.workspace_id,
    NumberID: m.number_id,
    ToJID: m.to_jid,
    Kind: m.kind,
    Text: m.text,
    MediaKey: m.media_key,
    DraftID: m.draft_id,
    Status: m.status,
    Attempts: m.attempts,
    LastError: m.last_error,
    NotBefore: new Date(m.not_before),
    ClaimedAt: m.claimed_at === null ? null : new Date(m.claimed_at),
    WaMessageID: m.wa_message_id,
    SentAt: m.sent_at === null ? null : new Date(m.sent_at),
    CreatedAt: new Date(m.created_at),
  };
}

/** Builds the wire form of a number. */
export function fromNumber(number: WaNumber): NumberWire {
  return {
    id: number.ID,
    workspace_id: number.WorkspaceID,
    integration_id: number.IntegrationID,
    phone: number.Phone,
    label: number.Label,
    agent_id: number.AgentID,
    status: number.Status,
    status_reason: number.StatusReason,
    tos_accepted_at: number.TOSAcceptedAt === null ? null : number.TOSAcceptedAt.toISOString(),
    hourly_limit: number.HourlyLimit,
    connected_at: number.ConnectedAt === null ? null : number.ConnectedAt.toISOString(),
    last_seen_at: number.LastSeenAt === null ? null : number.LastSeenAt.toISOString(),
    created_at: number.CreatedAt.toISOString(),
    updated_at: number.UpdatedAt.toISOString(),
  };
}

/** Reads the wire form of a number. */
export function numberToDomain(m: NumberWire): WaNumber {
  return {
    ID: m.id,
    WorkspaceID: m.workspace_id,
    IntegrationID: m.integration_id,
    Phone: m.phone,
    Label: m.label,
    AgentID: m.agent_id,
    Status: m.status,
    StatusReason: m.status_reason,
    TOSAcceptedAt: m.tos_accepted_at === null ? null : new Date(m.tos_accepted_at),
    HourlyLimit: m.hourly_limit,
    ConnectedAt: m.connected_at === null ? null : new Date(m.connected_at),
    LastSeenAt: m.last_seen_at === null ? null : new Date(m.last_seen_at),
    CreatedAt: new Date(m.created_at),
    UpdatedAt: new Date(m.updated_at),
  };
}

/**
 * The fields of one JSON body, read the way Go's `json.Unmarshal` reads them: an
 * absent field keeps the zero value, a present field of the wrong shape is a
 * refusal.
 */
function str(raw: Record<string, unknown>, key: string): string {
  const value = raw[key];
  if (value === undefined) return "";
  if (typeof value !== "string") throw new Error(`field ${key} is not a string`);
  return value;
}

/** Reads one boolean field. */
function bool(raw: Record<string, unknown>, key: string): boolean {
  const value = raw[key];
  if (value === undefined) return false;
  if (typeof value !== "boolean") throw new Error(`field ${key} is not a boolean`);
  return value;
}

/** Reads one number field. */
function num(raw: Record<string, unknown>, key: string): number {
  const value = raw[key];
  if (value === undefined) return 0;
  if (typeof value !== "number") throw new Error(`field ${key} is not a number`);
  return value;
}

/** Reads one uuid field; absent means the zero uuid. */
function uuid(raw: Record<string, unknown>, key: string): string {
  const value = raw[key];
  if (value === undefined) return NilUUID;
  if (typeof value !== "string" || !isUuid(value)) throw new Error(`field ${key} is not a uuid`);
  return value;
}

/** Reads one time field; absent means Go's zero time. */
function time(raw: Record<string, unknown>, key: string): Date {
  const value = raw[key];
  if (value === undefined) return ZERO_TIME;
  if (typeof value !== "string") throw new Error(`field ${key} is not a time`);
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) throw new Error(`field ${key} is not a time`);
  return parsed;
}

/** Reads one nested object field. */
function object(value: unknown, key: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`field ${key} is not an object`);
  }
  return value as Record<string, unknown>;
}

/**
 * Writes one JSON answer, as Go's `writeJSON`. `json.Encoder.Encode` appends a
 * newline, and this keeps the body byte-identical to what the Go product emits.
 */
function json(status: number, payload: unknown): Response {
  return new Response(`${JSON.stringify(payload)}\n`, {
    status,
    headers: { "Content-Type": "application/json" },
  });
}
