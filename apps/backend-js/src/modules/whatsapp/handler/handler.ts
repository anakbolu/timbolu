/**
 * Exposes the WhatsApp endpoints. It talks to the domain service interface only.
 *
 * Ported 1:1 from internal/modules/whatsapp/handler/handler.go. Every status,
 * message and error code below is the frontend's contract; the Indonesian
 * strings are copied verbatim.
 */

import type { Hono } from "hono";

import type { Logger } from "../../../platform/logger.ts";
import {
  currentUserId,
  isUuid,
  scope as requestScope,
  type AppContext,
  type AppEnv,
} from "../../../platform/context.ts";
import { error as errorEnvelope, created, ok, okWithMeta } from "../../../platform/response.ts";
import {
  ErrAlreadyLinked,
  ErrInvalidInput,
  ErrLinkExpired,
  ErrNoAgent,
  ErrNotFound,
  ErrNotConfigured,
  ErrNotConnected,
  ErrTermsNotAccepted,
  ErrUnavailable,
  KindAudio,
  KindDocument,
  KindImage,
  KindSticker,
  KindVideo,
  MediaReady,
  NilUUID,
  is,
  userMessage,
  type Inbound,
  type Link,
  type Number,
  type Outbox,
  type Scope,
} from "../domain/whatsapp.ts";
import type { ServicePort } from "../domain/service.ts";
import { qrMatrix } from "./qr.ts";
import { linkSocket } from "./stream.ts";

/** Serves the WhatsApp endpoints. */
export class Handler {
  constructor(
    private readonly service: ServicePort,
    private readonly log: Logger,
  ) {}

  /** Returns the workspace's numbers. */
  list = async (c: AppContext): Promise<Response> => {
    try {
      const numbers = await this.service.numbers(scopeOf(c));
      const payload = numbers.map(wireNumber);
      return okWithMeta(c, "ok", payload, { count: payload.length });
    } catch (err) {
      return this.fail(c, "list numbers", err);
    }
  };

  /** Returns one number. */
  get = async (c: AppContext): Promise<Response> => {
    const id = parseId(c.req.param("numberID") ?? "");
    if (id === null) {
      return errorEnvelope(c, 400, "number id is not a uuid", "invalid_input");
    }

    try {
      const number = await this.service.number(scopeOf(c), id);
      return ok(c, "ok", wireNumber(number));
    } catch (err) {
      return this.fail(c, "get number", err);
    }
  };

  /**
   * Prepares one number to be linked.
   *
   * It does not link anything: it records which number is about to be linked and
   * who answers it, so the warning can be shown against a row and the link has
   * somewhere to land.
   */
  create = async (c: AppContext): Promise<Response> => {
    const body = await jsonBody(c);
    if (body === null) {
      return errorEnvelope(c, 400, "the body is not valid JSON", "invalid_request");
    }

    const agentID = parseId(stringOf(body.agent_id));
    if (agentID === null) {
      return errorEnvelope(c, 400, "agent_id is not a uuid", "invalid_input");
    }

    try {
      const number = await this.service.prepare(scopeOf(c), {
        phone: stringOf(body.phone),
        label: stringOf(body.label),
        agentID,
      });
      return created(c, "number prepared", wireNumber(number));
    } catch (err) {
      return this.fail(c, "prepare number", err);
    }
  };

  /** Changes the label, the Bolu that answers, or the hourly ceiling. */
  update = async (c: AppContext): Promise<Response> => {
    const id = parseId(c.req.param("numberID") ?? "");
    if (id === null) {
      return errorEnvelope(c, 400, "number id is not a uuid", "invalid_input");
    }

    const body = await jsonBody(c);
    if (body === null) {
      return errorEnvelope(c, 400, "the body is not valid JSON", "invalid_request");
    }

    const scope = scopeOf(c);
    let current: Number;
    try {
      current = await this.service.number(scope, id);
    } catch (err) {
      return this.fail(c, "get number", err);
    }

    // Each field is its own statement, so a screen that changes one thing does
    // not have to send the rest back and risk overwriting a change made in
    // another tab with a stale copy.
    if (typeof body.label === "string") {
      try {
        current = await this.service.setLabel(scope, id, body.label);
      } catch (err) {
        return this.fail(c, "rename number", err);
      }
    }

    if (typeof body.agent_id === "string") {
      const agentID = parseId(body.agent_id);
      if (agentID === null) {
        return errorEnvelope(c, 400, "agent_id is not a uuid", "invalid_input");
      }
      try {
        current = await this.service.assignAgent(scope, id, agentID);
      } catch (err) {
        return this.fail(c, "assign agent", err);
      }
    }

    if (typeof body.hourly_limit === "number") {
      try {
        current = await this.service.setLimit(scope, id, Math.trunc(body.hourly_limit));
      } catch (err) {
        return this.fail(c, "set limit", err);
      }
    }

    return ok(c, "number updated", wireNumber(current));
  };

  /** Ends a session and clears what was stored. */
  unlink = async (c: AppContext): Promise<Response> => {
    const id = parseId(c.req.param("numberID") ?? "");
    if (id === null) {
      return errorEnvelope(c, 400, "number id is not a uuid", "invalid_input");
    }

    try {
      const number = await this.service.unlink(scopeOf(c), id);
      return ok(c, "number unlinked", wireNumber(number));
    } catch (err) {
      return this.fail(c, "unlink number", err);
    }
  };

  /** Records that the workspace accepted the warning. */
  acceptTerms = async (c: AppContext): Promise<Response> => {
    const id = parseId(c.req.param("numberID") ?? "");
    if (id === null) {
      return errorEnvelope(c, 400, "number id is not a uuid", "invalid_input");
    }

    try {
      const number = await this.service.acceptTerms(scopeOf(c), id);
      return ok(c, "terms accepted", wireNumber(number));
    } catch (err) {
      return this.fail(c, "accept terms", err);
    }
  };

  /** Starts a link attempt and answers the first state to show. */
  beginLink = async (c: AppContext): Promise<Response> => {
    const numberID = parseId(c.req.param("numberID") ?? "");
    if (numberID === null) {
      return errorEnvelope(c, 400, "number id is not a uuid", "invalid_input");
    }

    const body = await jsonBody(c);
    if (body === null) {
      return errorEnvelope(c, 400, "the body is not valid JSON", "invalid_request");
    }

    const scope = scopeOf(c);
    let number: Number;
    try {
      number = await this.service.number(scope, numberID);
    } catch (err) {
      return this.fail(c, "get number", err);
    }

    try {
      const link = await this.service.beginLink(scope, {
        phone: phoneOr(stringOf(body.phone), number.phone),
        label: number.label,
        agentID: number.agentID,
      });
      return ok(c, "link started", wireLink(link));
    } catch (err) {
      return this.fail(c, "begin link", err);
    }
  };

  /** Asks for the phone-code alternative to scanning. */
  pairCode = async (c: AppContext): Promise<Response> => {
    const body = await jsonBody(c);
    if (body === null) {
      return errorEnvelope(c, 400, "the body is not valid JSON", "invalid_request");
    }

    try {
      const link = await this.service.pairCode(
        scopeOf(c),
        c.req.param("linkID") ?? "",
        stringOf(body.phone),
      );
      return ok(c, "pair code issued", wireLink(link));
    } catch (err) {
      return this.fail(c, "pair by phone", err);
    }
  };

  /**
   * Reports where an attempt stands.
   *
   * It is polled rather than streamed. A link lasts a couple of minutes and
   * answers a handful of polls; a second stream would be a second
   * authentication path and a second thing to break.
   */
  linkState = async (c: AppContext): Promise<Response> => {
    try {
      const link = await this.service.linkState(scopeOf(c), c.req.param("linkID") ?? "");
      return ok(c, "ok", wireLink(link));
    } catch (err) {
      return this.fail(c, "read link", err);
    }
  };

  /** Abandons an attempt. */
  cancelLink = async (c: AppContext): Promise<Response> => {
    try {
      await this.service.cancelLink(scopeOf(c), c.req.param("linkID") ?? "");
      return ok(c, "link cancelled");
    } catch (err) {
      return this.fail(c, "cancel link", err);
    }
  };

  /** Returns what customers wrote, newest first. */
  messages = async (c: AppContext): Promise<Response> => {
    const numberID = parseId(c.req.param("numberID") ?? "");
    if (numberID === null) {
      return errorEnvelope(c, 400, "number id is not a uuid", "invalid_input");
    }

    try {
      const messages = await this.service.messages(scopeOf(c), {
        numberID,
        chatJID: (c.req.query("chat") ?? "").trim(),
        limit: queryInt(c, "limit", 0),
      });
      const payload = messages.map(wireMessage);
      return okWithMeta(c, "ok", payload, { count: payload.length });
    } catch (err) {
      return this.fail(c, "list messages", err);
    }
  };

  /** Returns what the workspace asked this number to send. */
  outbox = async (c: AppContext): Promise<Response> => {
    const numberID = parseId(c.req.param("numberID") ?? "");
    if (numberID === null) {
      return errorEnvelope(c, 400, "number id is not a uuid", "invalid_input");
    }

    try {
      const items = await this.service.outbox(scopeOf(c), {
        numberID,
        status: (c.req.query("status") ?? "").trim(),
        limit: queryInt(c, "limit", 0),
      });
      const payload = items.map(wireOutbox);
      return okWithMeta(c, "ok", payload, { count: payload.length });
    } catch (err) {
      return this.fail(c, "list outbox", err);
    }
  };

  /**
   * Serves one message's attachment.
   *
   * It is on the API rather than on the gateway because the bytes are stored
   * here and the person asking is a member of the workspace: the gateway holds a
   * connection, not a file server.
   */
  media = async (c: AppContext): Promise<Response> => {
    const messageID = parseId(c.req.param("messageID") ?? "");
    if (messageID === null) {
      return errorEnvelope(c, 400, "message id is not a uuid", "invalid_input");
    }

    let message: Inbound;
    try {
      message = await this.service.messageByID(scopeOf(c), messageID);
    } catch (err) {
      return this.fail(c, "get message", err);
    }

    if (message.media.status !== MediaReady || message.media.key === "") {
      return errorEnvelope(c, 404, "lampiran pesan ini tidak tersedia", "media_not_found");
    }

    let content: Uint8Array;
    try {
      content = await this.service.mediaBytes(message.media.key);
    } catch (err) {
      return this.fail(c, "read media", err);
    }

    // The body is written raw rather than sniffed, because sniffing would
    // replace the type with whatever it guessed: a JPEG that starts with an
    // unusual marker would be served as octet-stream and downloaded instead of
    // shown.
    return c.body(content, 200, {
      "Content-Type": contentTypeFor(message.media.kind),
      "Content-Disposition": `inline; filename=${JSON.stringify(filenameFor(message))}`,
      "Cache-Control": "private, max-age=3600",
    });
  };

  /**
   * Streams one link attempt's state until it ends.
   *
   * The implementation is in stream.ts, as it is in the Go package's other file;
   * the method exists here so the routes and the wiring see one handler.
   */
  linkSocket = (c: AppContext): Promise<Response> => linkSocket(c, this.service);

  /** Maps the module's errors onto responses. */
  private fail(c: AppContext, operation: string, err: unknown): Response {
    if (is(err, ErrNotFound)) {
      // Two things answer 404: a row that is not in this workspace, and a number
      // WhatsApp does not know. The second is the caller's input, and the module
      // already wrote a sentence about it — so it is passed through.
      let message = "nomor atau pesan itu tidak ada di workspace ini";
      const user = userMessage(err, ErrNotFound);
      if (user.startsWith("WhatsApp")) message = user;
      return errorEnvelope(c, 404, message, "whatsapp_not_found");
    }
    if (is(err, ErrTermsNotAccepted)) {
      return errorEnvelope(c, 409, "peringatan tentang jalur tidak resmi belum diterima", "terms_not_accepted");
    }
    if (is(err, ErrAlreadyLinked)) {
      return errorEnvelope(c, 409, "nomor ini sudah tertaut", "already_linked");
    }
    if (is(err, ErrNoAgent)) {
      return errorEnvelope(c, 409, "nomor ini belum punya Bolu yang menjawab", "no_agent");
    }
    if (is(err, ErrNotConnected)) {
      return errorEnvelope(c, 409, "nomornya belum tertaut atau sedang offline", "not_connected");
    }
    if (is(err, ErrLinkExpired)) {
      return errorEnvelope(c, 410, "penautan ini sudah kedaluwarsa; minta kode baru", "link_expired");
    }
    if (is(err, ErrNotConfigured)) {
      return errorEnvelope(c, 503, "gateway WhatsApp belum dikonfigurasi di deployment ini", "whatsapp_unavailable");
    }
    if (is(err, ErrUnavailable)) {
      // The module knows why WhatsApp could not be reached, and the difference
      // matters to the person reading it: being throttled means waiting a few
      // minutes, while a network that is down means trying later.
      let message = "WhatsApp sedang tidak bisa dihubungi dari deployment ini; coba lagi sebentar lagi";
      const user = userMessage(err, ErrUnavailable);
      if (user !== "") message = user;
      return errorEnvelope(c, 503, message, "whatsapp_unreachable");
    }
    if (is(err, ErrInvalidInput)) {
      return errorEnvelope(c, 400, userMessage(err, ErrInvalidInput), "invalid_input");
    }

    this.log.error({ operation, err }, "whatsapp request failed");
    return errorEnvelope(c, 500, "internal server error", "internal_error");
  }
}

/**
 * Registers the endpoints every member of the workspace may use.
 *
 * Linking a number is every member's business for the same reason a routine is:
 * the person who notices the shop's WhatsApp is offline is not always the owner,
 * and a number that is down is a customer who got no answer.
 */
export function routes(group: Hono<AppEnv>, h: Handler): void {
  group.get("/whatsapp/numbers", h.list);
  group.post("/whatsapp/numbers", h.create);
  group.get("/whatsapp/numbers/:numberID", h.get);
  group.patch("/whatsapp/numbers/:numberID", h.update);
  group.delete("/whatsapp/numbers/:numberID", h.unlink);

  group.post("/whatsapp/numbers/:numberID/terms", h.acceptTerms);
  group.post("/whatsapp/numbers/:numberID/link", h.beginLink);
  group.post("/whatsapp/links/:linkID/pair-code", h.pairCode);
  group.get("/whatsapp/links/:linkID", h.linkState);
  group.delete("/whatsapp/links/:linkID", h.cancelLink);

  group.get("/whatsapp/numbers/:numberID/messages", h.messages);
  group.get("/whatsapp/numbers/:numberID/outbox", h.outbox);
  group.get("/whatsapp/messages/:messageID/media", h.media);
}

// ------------------------------------------------------------------- payloads

/** One number, as a screen reads it. */
interface NumberPayload {
  id: string;
  phone: string;
  label: string;
  agent_id?: string;
  status: string;
  reason?: string;
  linked: boolean;
  connected: boolean;
  terms_accepted: boolean;
  hourly_limit: number;
  created_at: string;
  last_seen_at?: string;
}

/** One attempt, as a screen reads it. */
interface LinkPayload {
  link_id: string;
  number_id?: string;
  state: string;
  qr?: string;
  qr_matrix?: string[];
  pair_code?: string;
  expires_at?: string;
  reason?: string;
}

/** One message, as a screen reads it. */
interface MessagePayload {
  id: string;
  wa_message_id: string;
  chat: string;
  from?: string;
  name?: string;
  group: boolean;
  ours: boolean;
  kind: string;
  text: string;
  media_kind?: string;
  media_status?: string;
  media_error?: string;
  task_id?: string;
  received_at: string;
}

/** One queued message, as a screen reads it. */
interface OutboxPayload {
  id: string;
  to: string;
  kind: string;
  text: string;
  status: string;
  attempts: number;
  last_error?: string;
  sent_at?: string;
  created_at: string;
}

/** Renders one number for a screen. */
export function wireNumber(number: Number): NumberPayload {
  const payload: NumberPayload = {
    id: number.id,
    phone: number.phone,
    label: number.label,
    status: number.status,
    linked: numberLinkedStatus(number.status),
    connected: number.status === "connected",
    terms_accepted: number.tosAcceptedAt !== null,
    hourly_limit: number.hourlyLimit,
    created_at: rfc3339(number.createdAt),
  };
  if (number.statusReason !== "") payload.reason = number.statusReason;
  if (number.agentID !== NilUUID) payload.agent_id = number.agentID;
  if (number.lastSeenAt !== null) payload.last_seen_at = rfc3339(number.lastSeenAt);
  return payload;
}

/** Renders one attempt for a screen. */
export function wireLink(link: Link): LinkPayload {
  const payload: LinkPayload = { link_id: link.id, state: link.state };

  if (link.numberID !== NilUUID) payload.number_id = link.numberID;
  if (link.code !== "") payload.qr = link.code;
  const matrix = qrMatrix(link.code);
  if (matrix !== null) payload.qr_matrix = matrix;
  if (link.pairCode !== "") payload.pair_code = link.pairCode;
  if (link.expiresAt.getTime() !== 0) payload.expires_at = rfc3339(link.expiresAt);
  if (link.reason !== "") payload.reason = link.reason;

  return payload;
}

/** Renders one message for a screen. */
export function wireMessage(message: Inbound): MessagePayload {
  const payload: MessagePayload = {
    id: message.id,
    wa_message_id: message.waMessageID,
    chat: message.chatJID,
    group: message.isGroup,
    ours: message.isFromMe,
    kind: message.kind,
    text: message.text,
    received_at: rfc3339(message.receivedAt),
  };

  if (message.senderJID !== "") payload.from = message.senderJID;
  if (message.senderName !== "") payload.name = message.senderName;
  if (message.media.kind !== "") payload.media_kind = message.media.kind;
  if (message.media.status !== "") payload.media_status = message.media.status;
  if (message.media.error !== "") payload.media_error = message.media.error;
  if (message.taskID !== NilUUID) payload.task_id = message.taskID;

  return payload;
}

/** Renders one queued message for a screen. */
export function wireOutbox(item: Outbox): OutboxPayload {
  const payload: OutboxPayload = {
    id: item.id,
    to: item.toJID,
    kind: item.kind,
    text: item.text,
    status: item.status,
    attempts: item.attempts,
    created_at: rfc3339(item.createdAt),
  };

  if (item.lastError !== "") payload.last_error = item.lastError;
  if (item.sentAt !== null) payload.sent_at = rfc3339(item.sentAt);

  return payload;
}

/** Reports whether a session exists, whether or not it is online now. */
function numberLinkedStatus(status: string): boolean {
  return status === "connected" || status === "disconnected" || status === "needs_relink";
}

/**
 * Names what an attachment is, so a browser shows it rather than downloading it.
 *
 * A kind with no entry answers the empty string, exactly as the Go map does.
 */
function contentTypeFor(kind: string): string {
  switch (kind) {
    case KindImage:
      return "image/jpeg";
    case KindVideo:
      return "video/mp4";
    case KindAudio:
      return "audio/ogg";
    case KindSticker:
      return "image/webp";
    case KindDocument:
      return "application/octet-stream";
    default:
      return "";
  }
}

/** What the browser calls a downloaded attachment. */
function filenameFor(message: Inbound): string {
  const extension =
    message.media.kind === KindImage
      ? ".jpg"
      : message.media.kind === KindVideo
        ? ".mp4"
        : message.media.kind === KindAudio
          ? ".ogg"
          : message.media.kind === KindSticker
            ? ".webp"
            : "";

  return `whatsapp-${message.waMessageID}${extension}`;
}

/** Falls back to the number already on the row. */
function phoneOr(given: string, stored: string): string {
  return given.trim() !== "" ? given : stored;
}

/** Reads a uuid from the request. */
function parseId(raw: string): string | null {
  const trimmed = raw.trim();
  return isUuid(trimmed) ? trimmed : null;
}

/** Reads the tenant the request runs in, from the identity the middlewares resolved. */
function scopeOf(c: AppContext): Scope {
  const s = requestScope(c);
  return { userId: currentUserId(c) ?? s.userId ?? null, workspaceId: s.workspaceId ?? "" };
}

/** Reads the body as an object, or null when it is not one. */
async function jsonBody(c: AppContext): Promise<Record<string, unknown> | null> {
  const raw = await c.req.json().catch(() => null);
  return typeof raw === "object" && raw !== null && !Array.isArray(raw)
    ? (raw as Record<string, unknown>)
    : null;
}

/** A string field of a parsed body. */
function stringOf(value: unknown): string {
  return typeof value === "string" ? value : "";
}

/** A query parameter as an integer, the way Fiber's `QueryInt` reads it. */
function queryInt(c: AppContext, key: string, fallback: number): number {
  const raw = (c.req.query(key) ?? "").trim();
  if (raw === "" || !/^[+-]?\d+$/.test(raw)) return fallback;
  return Number.parseInt(raw, 10);
}

/** Formats a time the way Go's `Format(time.RFC3339)` does, in UTC. */
function rfc3339(value: Date): string {
  return value.toISOString().replace(/\.\d{3}Z$/, "Z");
}
