/**
 * The conversation endpoints: threads, message history, sending, attachments,
 * the event replay, and the live stream over WebSocket with an SSE fallback.
 *
 * Ported 1:1 from internal/modules/chat/handler/handler.go. Every status,
 * message and error code below is the frontend's contract.
 */

import { upgradeWebSocket } from "hono/bun";
import type { Hono } from "hono";
import type { WSContext } from "hono/ws";

import type { Logger } from "../../../platform/logger.ts";
import { isUuid, scope as requestScope, type AppContext, type AppEnv } from "../../../platform/context.ts";
import { AppError } from "../../../platform/errors.ts";
import { error as errorEnvelope, ok, success, unavailable } from "../../../platform/response.ts";
import {
  ErrAgentNotInConversation,
  ErrAttachmentNotFound,
  ErrBlockTooLarge,
  ErrConversationNotFound,
  ErrFileTypeNotAllowed,
  ErrInvalidBlock,
  ErrInvalidInput,
  ErrMessageNotFound,
  ErrNoResponder,
  ErrStorageUnavailable,
  is,
  type Conversation,
  type Participant,
} from "../domain/conversation.ts";
import { Heartbeat, type Event, type MessagePayload } from "../domain/event.ts";
import { ErrNoBudget, type AgentRef, type Cursor, type Scope } from "../domain/ports.ts";
import type {
  AddParticipantsRequest,
  AttachRequest,
  CreateGroupRequest,
  SendRequest,
  SendResult,
  Service,
} from "../domain/service.ts";
import { WireAttachment, WireMessage } from "../domain/wire.ts";

/** Caps one upload, from the module's own contract. */
const AttachmentMaxBytes = 25 * 1024 * 1024;

/** Describes the sandboxed content origin. */
export interface ContentConfig {
  /**
   * The origins allowed to frame a document. The document is served from a
   * different host than the app, so `frame-ancestors 'self'` would refuse every
   * frame.
   */
  frameAncestors: string[];
}

/** Serves the conversation endpoints. */
export class Handler {
  private contentConfig: ContentConfig = { frameAncestors: ["'self'"] };
  private heartbeat = Heartbeat;

  constructor(
    private readonly service: Service,
    private readonly log: Logger,
  ) {}

  /** Sets the content origin policy. */
  withContent(config: ContentConfig): this {
    if (config.frameAncestors.length > 0) {
      this.contentConfig = config;
    }
    return this;
  }

  /** Sets how often a live stream sends a keep-alive, in milliseconds. */
  withHeartbeat(interval: number): this {
    if (interval > 0) {
      this.heartbeat = interval;
    }
    return this;
  }

  // ------------------------------------------------------------ conversations

  /** The threads of the workspace. */
  list = async (c: AppContext): Promise<Response> => {
    try {
      const conversations = await this.service.conversations(scopeOf(c));
      return ok(c, "ok", conversations.map(toConversationPayload));
    } catch (err) {
      return this.fail(c, "list conversations", err);
    }
  };

  /** One thread. */
  get = async (c: AppContext): Promise<Response> => {
    const id = parseId(c.req.param("conversationID") ?? "");
    if (id === null) {
      return invalidRequest(c, "invalid conversation id");
    }

    try {
      const conversation = await this.service.conversation(scopeOf(c), id);
      return ok(c, "ok", toConversationPayload(conversation));
    } catch (err) {
      return this.fail(c, "get conversation", err);
    }
  };

  /** The 1:1 thread with one Bolu, creating it on first use. */
  direct = async (c: AppContext): Promise<Response> => {
    const body = await jsonBody(c);
    if (body === null) {
      return invalidRequest(c, "invalid request body");
    }

    const agentId = parseId(stringOf(body.agent_id));
    if (agentId === null) {
      return invalidRequest(c, "invalid agent id");
    }

    try {
      const conversation = await this.service.direct(scopeOf(c), agentId);
      return ok(c, "ok", toConversationPayload(conversation));
    } catch (err) {
      return this.fail(c, "open direct conversation", err);
    }
  };

  /** A group. */
  createGroup = async (c: AppContext): Promise<Response> => {
    const body = await jsonBody(c);
    if (body === null) {
      return invalidRequest(c, "invalid request body");
    }

    const agentIds = parseIds(stringArrayOf(body.agent_ids));
    if (agentIds === null) {
      return invalidRequest(c, "invalid agent id");
    }
    const userIds = parseIds(stringArrayOf(body.user_ids));
    if (userIds === null) {
      return invalidRequest(c, "invalid user id");
    }

    const req: CreateGroupRequest = { title: stringOf(body.title), agentIds, userIds };
    try {
      const conversation = await this.service.createGroup(scopeOf(c), req);
      return success(c, 201, "group created", toConversationPayload(conversation));
    } catch (err) {
      return this.fail(c, "create group", err);
    }
  };

  /** Adds Bolu and members to a group. */
  addParticipants = async (c: AppContext): Promise<Response> => {
    const id = parseId(c.req.param("conversationID") ?? "");
    if (id === null) {
      return invalidRequest(c, "invalid conversation id");
    }

    const body = await jsonBody(c);
    if (body === null) {
      return invalidRequest(c, "invalid request body");
    }

    const agentIds = parseIds(stringArrayOf(body.agent_ids));
    if (agentIds === null) {
      return invalidRequest(c, "invalid agent id");
    }
    const userIds = parseIds(stringArrayOf(body.user_ids));
    if (userIds === null) {
      return invalidRequest(c, "invalid user id");
    }

    const req: AddParticipantsRequest = { agentIds, userIds };
    try {
      const conversation = await this.service.addParticipants(scopeOf(c), id, req);
      return ok(c, "participants added", toConversationPayload(conversation));
    } catch (err) {
      return this.fail(c, "add participants", err);
    }
  };

  // ---------------------------------------------------------------- messages

  /** One page of messages, newest first. */
  history = async (c: AppContext): Promise<Response> => {
    const id = parseId(c.req.param("conversationID") ?? "");
    if (id === null) {
      return invalidRequest(c, "invalid conversation id");
    }

    const parsed = parseCursor(c);
    if (parsed === null) {
      return invalidRequest(c, "invalid cursor");
    }

    try {
      const page = await this.service.history(scopeOf(c), id, parsed);
      const payload: PagePayload = {
        messages: page.messages.map(WireMessage),
        has_more: page.hasMore,
      };
      if (page.hasMore) {
        payload.next_cursor = encodeCursor(page.nextBeforeCreatedAtNanos, page.nextBeforeId);
      }
      return ok(c, "ok", payload);
    } catch (err) {
      return this.fail(c, "read history", err);
    }
  };

  /** Stores a message and starts the reply. */
  send = async (c: AppContext): Promise<Response> => {
    const id = parseId(c.req.param("conversationID") ?? "");
    if (id === null) {
      return invalidRequest(c, "invalid conversation id");
    }

    const body = await jsonBody(c);
    if (body === null) {
      return invalidRequest(c, "invalid request body");
    }

    const attachmentIds = parseIds(stringArrayOf(body.attachment_ids));
    if (attachmentIds === null) {
      return invalidRequest(c, "invalid attachment id");
    }
    const agentId = parseOptionalId(stringOf(body.agent_id));
    if (agentId === null) {
      return invalidRequest(c, "invalid agent id");
    }

    // A message is answered by default: sending one and expecting nothing is the
    // exception, and a client that wants to post a note says so explicitly.
    const reply = typeof body.reply === "boolean" ? body.reply : true;

    const req: SendRequest = {
      conversationId: id,
      text: stringOf(body.text),
      attachmentIds,
      reply,
      forAttachment: false,
      agentId,
    };

    try {
      const result = await this.service.send(scopeOf(c), req);
      return success(c, 201, "message sent", toSendPayload(result));
    } catch (err) {
      return this.fail(c, "send message", err);
    }
  };

  // ------------------------------------------------------------- attachments

  /** Stores an uploaded file. */
  attach = async (c: AppContext): Promise<Response> => {
    const id = parseId(c.req.param("conversationID") ?? "");
    if (id === null) {
      return invalidRequest(c, "invalid conversation id");
    }

    let form: Record<string, string | File | (string | File)[]>;
    try {
      form = (await c.req.parseBody()) as Record<string, string | File | (string | File)[]>;
    } catch {
      return invalidRequest(c, "a file is required");
    }

    const file = form.file;
    if (!(file instanceof File)) {
      return invalidRequest(c, "a file is required");
    }
    if (file.size > AttachmentMaxBytes) {
      return errorEnvelope(
        c,
        413,
        `the file is larger than ${AttachmentMaxBytes} bytes`,
        "file_too_large",
      );
    }

    const messageId = parseOptionalId(formValue(form, "message_id"));
    if (messageId === null) {
      return invalidRequest(c, "invalid message id");
    }

    const content = new Uint8Array(await file.arrayBuffer());
    if (content.length > AttachmentMaxBytes) {
      return errorEnvelope(
        c,
        413,
        `the file is larger than ${AttachmentMaxBytes} bytes`,
        "file_too_large",
      );
    }

    // One call for both shapes. Without a message id the service opens the
    // message the file belongs to inside the same transaction that stores it.
    // The part's own Content-Type is not passed on: the file's type is read from
    // its extension and its bytes, and a header a client chose is neither.
    const req: AttachRequest = {
      messageId,
      conversationId: id,
      filename: file.name,
    };

    try {
      const attachment = await this.service.attach(scopeOf(c), req, content);
      return success(c, 201, "file stored", WireAttachment(attachment));
    } catch (err) {
      return this.fail(c, "store attachment", err);
    }
  };

  /** Streams one attachment's bytes. */
  download = async (c: AppContext): Promise<Response> => {
    const id = parseId(c.req.param("attachmentID") ?? "");
    if (id === null) {
      return invalidRequest(c, "invalid attachment id");
    }

    try {
      const { attachment, content } = await this.service.attachmentContent(scopeOf(c), id);

      const headers = new Headers();
      headers.set("Content-Type", attachment.contentType);
      headers.set(
        "Content-Disposition",
        `inline; filename="${attachment.filename.replaceAll('"', "")}"`,
      );
      // An attachment is served as data, never executed: this is what stops a
      // crafted content type from turning a download into a script the browser
      // runs on the API's origin.
      headers.set("X-Content-Type-Options", "nosniff");
      headers.set("Content-Security-Policy", "default-src 'none'; sandbox");
      return new Response(content, { status: 200, headers });
    } catch (err) {
      return this.fail(c, "read attachment", err);
    }
  };

  // ------------------------------------------------------------------ events

  /** The workspace stream after an id. */
  events = async (c: AppContext): Promise<Response> => {
    const afterId = queryInt(c, "after_id", 0);
    const limit = queryInt(c, "limit", 0);

    try {
      const events = await this.service.events(scopeOf(c), afterId, limit);
      return ok(c, "ok", events.map(toEventPayload));
    } catch (err) {
      return this.fail(c, "read events", err);
    }
  };

  /**
   * Streams the workspace events as Server-Sent Events.
   *
   * It is the fallback for a client that cannot open a WebSocket. Every frame
   * carries the event id, so a client that reconnects with the `Last-Event-ID`
   * header (which the browser sends by itself) resumes exactly where it stopped.
   */
  sse = async (c: AppContext): Promise<Response> => {
    const controller = new AbortController();
    const signal = controller.signal;

    let events: AsyncIterable<Event>;
    try {
      events = await this.service.stream(scopeOf(c), lastEventId(c), signal);
    } catch (err) {
      return this.fail(c, "open stream", err);
    }

    const heartbeat = this.heartbeat;
    const encoder = new TextEncoder();

    const body = new ReadableStream<Uint8Array>({
      async start(stream) {
        let closed = false;
        const write = (text: string): void => {
          if (closed) return;
          try {
            stream.enqueue(encoder.encode(text));
          } catch {
            closed = true;
          }
        };

        // The head goes out on its own, before there is an event to carry it: a
        // client that resumes with nothing to replay would otherwise wait for
        // the first heartbeat before its EventSource reported itself open. A
        // comment is not an event, so a client ignores the line.
        write(": connected\n\n");

        // A comment keeps an idle connection alive through a proxy without being
        // an event the client must ignore by type.
        const timer = setInterval(() => write(": keep-alive\n\n"), heartbeat);

        try {
          for await (const event of events) {
            if (signal.aborted) break;
            write(`id: ${event.id}\nevent: ${event.type}\ndata: ${JSON.stringify(toEventPayload(event))}\n\n`);
          }
        } catch {
          // The stream ended; the client reconnects and resumes by id.
        } finally {
          clearInterval(timer);
          closed = true;
          // The client disconnecting must release the Redis subscription, and
          // aborting the signal is what closes it.
          controller.abort();
          try {
            stream.close();
          } catch {
            // Already closed by the runtime when the client went away.
          }
        }
      },
      cancel() {
        controller.abort();
      },
    });

    return new Response(body, {
      status: 200,
      headers: {
        "Content-Type": "text/event-stream",
        "Cache-Control": "no-cache, no-transform",
        Connection: "keep-alive",
        // Stops a reverse proxy from buffering the stream into a batch.
        "X-Accel-Buffering": "no",
      },
    });
  };

  /**
   * Streams the workspace events over a WebSocket.
   *
   * The scope and the resume point are read before the upgrade: once the
   * connection is hijacked, the request context no longer holds them.
   */
  socket = async (c: AppContext): Promise<Response> => {
    const upgrade = c.req.header("Upgrade")?.toLowerCase() ?? "";
    if (upgrade !== "websocket") {
      return errorEnvelope(
        c,
        426,
        "this endpoint speaks WebSocket; use /events/stream for SSE",
        "upgrade_required",
      );
    }

    const streamScope = scopeOf(c);
    const afterId = lastEventId(c);
    const heartbeat = this.heartbeat;
    // The controller is the connection's lifetime: a close, an error, or the
    // stream ending all abort it, and aborting is what releases the Redis
    // subscription. A leak here is what kills the process over a day.
    const controller = new AbortController();

    return upgradeWebSocket(c, {
      onOpen: (_event, ws) => {
        void this.pump(ws, streamScope, afterId, heartbeat, controller);
      },
      onMessage: () => {
        // A client may send a resume request on the open socket, which is what
        // makes a reconnect cheap. The service's backlog already covers the
        // connection, so the message is acknowledged by being read.
      },
      onClose: () => controller.abort(),
      onError: () => controller.abort(),
    });
  };

  /** Pumps the workspace stream onto one open socket. */
  private async pump(
    ws: WSContext,
    streamScope: Scope,
    afterId: number,
    heartbeat: number,
    controller: AbortController,
  ): Promise<void> {
    let events: AsyncIterable<Event>;
    try {
      events = await this.service.stream(streamScope, afterId, controller.signal);
    } catch (err) {
      ws.send(JSON.stringify({ type: "error", message: (err as Error).message }));
      ws.close();
      return;
    }

    const timer = setInterval(() => {
      if (ws.readyState === 1) {
        ws.send(JSON.stringify({ type: "ping" }));
      }
    }, heartbeat);

    try {
      for await (const event of events) {
        if (controller.signal.aborted || ws.readyState !== 1) {
          break;
        }
        ws.send(JSON.stringify(toEventPayload(event)));
      }
    } catch {
      // The connection went away; the loop below releases the subscription.
    } finally {
      clearInterval(timer);
      controller.abort();
      ws.close();
    }
  }

  // ----------------------------------------------------------------- content

  /**
   * Serves one sandboxed HTML document from the content origin.
   *
   * The reference is a storage key, so nothing is looked up in the database and
   * no session is needed: this route exists to be framed, not to be
   * authenticated.
   *
   * The policy is the security boundary, not the sandbox attribute alone: with
   * `connect-src 'none'` a script inside the document cannot reach the network
   * at all, and `default-src 'none'` leaves it no way to load anything but an
   * inline script and its own styles.
   */
  content = async (c: AppContext): Promise<Response> => {
    const reference = (c.req.param("reference") ?? "").trim();
    if (reference === "") {
      return invalidRequest(c, "a content reference is required");
    }

    try {
      const document = await this.service.content(reference);

      const headers = new Headers();
      headers.set("Content-Type", "text/html; charset=utf-8");
      headers.set("X-Content-Type-Options", "nosniff");
      headers.set("Content-Security-Policy", this.contentPolicy());
      headers.set("Referrer-Policy", "no-referrer");
      // The frame is the only place this document may appear, and it may not
      // navigate the application.
      headers.set("Cross-Origin-Resource-Policy", "same-site");
      return new Response(document, { status: 200, headers });
    } catch (err) {
      return this.fail(c, "read content", err);
    }
  };

  /** The CSP the sandboxed documents are served under. */
  contentPolicy(): string {
    const ancestors = this.contentConfig.frameAncestors.join(" ");

    return (
      "default-src 'none'; " +
      "script-src 'unsafe-inline' https://cdn.jsdelivr.net https://unpkg.com; " +
      "style-src 'unsafe-inline'; " +
      "img-src data: blob:; " +
      "font-src data:; " +
      "media-src data: blob:; " +
      "connect-src 'none'; " +
      "form-action 'none'; " +
      `frame-ancestors ${ancestors}; ` +
      "base-uri 'none'"
    );
  }

  // ------------------------------------------------------------------ errors

  /** Maps the module's errors onto responses. */
  private fail(c: AppContext, operation: string, err: unknown): Response {
    if (err instanceof AppError) {
      return errorEnvelope(c, err.status, err.message, err.code, err.details);
    }

    const message = err instanceof Error ? err.message : String(err);

    if (is(err, ErrConversationNotFound)) {
      return errorEnvelope(c, 404, "conversation not found", "conversation_not_found");
    }
    if (is(err, ErrMessageNotFound)) {
      return errorEnvelope(c, 404, "message not found", "message_not_found");
    }
    if (is(err, ErrAttachmentNotFound)) {
      return errorEnvelope(c, 404, "attachment not found", "attachment_not_found");
    }
    if (is(err, ErrAgentNotInConversation)) {
      return errorEnvelope(c, 400, "that Bolu is not in this conversation", "agent_not_in_conversation");
    }
    if (is(err, ErrNoResponder)) {
      return errorEnvelope(c, 409, message, "no_responder");
    }
    if (is(err, ErrNoBudget)) {
      // 402 rather than 403: nothing is forbidden about who is asking, and a
      // screen that showed "ask an admin" would send somebody to the wrong
      // place. What fixes this is a payment.
      return errorEnvelope(c, 402, message, "no_budget");
    }
    if (is(err, ErrInvalidBlock)) {
      return errorEnvelope(c, 400, message, "invalid_block");
    }
    if (is(err, ErrBlockTooLarge)) {
      return errorEnvelope(c, 413, message, "block_too_large");
    }
    if (is(err, ErrFileTypeNotAllowed)) {
      // 415 rather than 400: the request is well formed and the file is real,
      // and what is wrong is its media type — which is what a client checks for
      // to tell the user to pick another file.
      return errorEnvelope(c, 415, message, "file_type_not_allowed");
    }
    if (is(err, ErrInvalidInput)) {
      return errorEnvelope(c, 400, message, "invalid_input");
    }
    if (is(err, ErrStorageUnavailable)) {
      return unavailable(c, message, "storage_not_configured");
    }

    this.log.error({ operation, err }, "chat request failed");
    return errorEnvelope(c, 500, "internal server error", "internal_error");
  }
}

/** Registers the endpoints every member of the workspace may use. */
export function routes(group: Hono<AppEnv>, h: Handler): void {
  group.get("/conversations", h.list);
  group.post("/conversations/direct", h.direct);
  group.post("/conversations/groups", h.createGroup);
  group.get("/conversations/:conversationID", h.get);
  group.get("/conversations/:conversationID/messages", h.history);
  group.post("/conversations/:conversationID/messages", h.send);
  group.post("/conversations/:conversationID/attachments", h.attach);
  group.get("/attachments/:attachmentID", h.download);
  group.get("/events", h.events);
}

/** Registers the endpoints that change a group. */
export function managerRoutes(group: Hono<AppEnv>, h: Handler): void {
  group.post("/conversations/:conversationID/participants", h.addParticipants);
}

/**
 * Registers the live stream. The container applies the middleware that
 * authenticates from the query string: a browser cannot set an Authorization
 * header on a WebSocket or an EventSource.
 */
export function streamRoutes(group: Hono<AppEnv>, h: Handler): void {
  group.get("/events/stream", h.sse);
  group.get("/events/socket", h.socket);
}

/**
 * Registers the sandboxed content origin.
 *
 * It is mounted outside the tenant middleware on purpose: an iframe without
 * allow-same-origin cannot send the application's cookies, so the reference in
 * the URL is the only capability.
 */
export function contentRoutes(group: Hono<AppEnv>, h: Handler): void {
  group.get("/content/:reference", h.content);
}

// ------------------------------------------------------------------- payloads

interface ConversationPayload {
  id: string;
  kind: string;
  title: string;
  participants: ParticipantPayload[];
  message_count: number;
  last_activity_at: string;
  created_at: string;
}

interface ParticipantPayload {
  id: string;
  agent_id?: string;
  user_id?: string;
  name: string;
  role: string;
  is_agent: boolean;
}

interface SendPayload {
  message: MessagePayload;
  reply_message_ids?: string[];
  responders?: ParticipantPayload[];
  routed: boolean;
}

interface PagePayload {
  messages: MessagePayload[];
  next_cursor?: string;
  has_more: boolean;
}

interface EventPayload {
  id: number;
  type: string;
  workspace_id: string;
  agent_id?: string;
  user_id?: string;
  conversation_id?: string;
  task_id?: string;
  draft_id?: string;
  payload?: unknown;
  created_at: string;
}

function toConversationPayload(conversation: Conversation): ConversationPayload {
  return {
    id: conversation.id,
    kind: conversation.kind,
    title: conversation.title,
    participants: conversation.participants.map(toParticipantPayload),
    message_count: conversation.messageCount,
    last_activity_at: rfc3339(conversation.lastActivityAt),
    created_at: rfc3339(conversation.createdAt),
  };
}

function toParticipantPayload(participant: Participant): ParticipantPayload {
  const payload: ParticipantPayload = {
    id: participant.id,
    name: participant.displayName,
    role: participant.role,
    is_agent: !isNil(participant.agentId),
  };
  if (!isNil(participant.agentId)) payload.agent_id = participant.agentId;
  if (!isNil(participant.userId)) payload.user_id = participant.userId;
  return payload;
}

function toParticipantPayloadAgent(agent: AgentRef): ParticipantPayload {
  return { id: "", agent_id: agent.id, name: agent.name, role: agent.role, is_agent: true };
}

function toSendPayload(result: SendResult): SendPayload {
  const payload: SendPayload = { message: WireMessage(result.message), routed: result.routed };
  if (result.replyMessageIds.length > 0) {
    payload.reply_message_ids = result.replyMessageIds;
  }
  if (result.responders.length > 0) {
    payload.responders = result.responders.map(toParticipantPayloadAgent);
  }
  return payload;
}

function toEventPayload(event: Event): EventPayload {
  const payload: EventPayload = {
    id: event.id,
    type: event.type,
    workspace_id: event.workspaceId,
    created_at: rfc3339Nano(event.createdAt),
  };
  if (!isNil(event.actorAgentId)) payload.agent_id = event.actorAgentId;
  if (!isNil(event.actorUserId)) payload.user_id = event.actorUserId;
  if (!isNil(event.conversationId)) payload.conversation_id = event.conversationId;
  if (!isNil(event.taskId)) payload.task_id = event.taskId;
  if (!isNil(event.draftId)) payload.draft_id = event.draftId;
  if (event.payload !== undefined && event.payload !== null) payload.payload = event.payload;
  return payload;
}

// -------------------------------------------------------------------- helpers

/** The tenant the request runs as. */
function scopeOf(c: AppContext): Scope {
  const s = requestScope(c);
  return { userId: s.userId ?? "", workspaceId: s.workspaceId ?? "" };
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

/** A string array field of a parsed body. */
function stringArrayOf(value: unknown): string[] {
  return Array.isArray(value) ? value.filter((item): item is string => typeof item === "string") : [];
}

/** A multipart field as a string. */
function formValue(
  form: Record<string, string | File | (string | File)[]>,
  key: string,
): string {
  const value = form[key];
  return typeof value === "string" ? value : "";
}

/** A query parameter as an integer, the way Fiber's `QueryInt` reads it. */
function queryInt(c: AppContext, key: string, fallback: number): number {
  const raw = (c.req.query(key) ?? "").trim();
  if (raw === "" || !/^[+-]?\d+$/.test(raw)) {
    return fallback;
  }
  return Number.parseInt(raw, 10);
}

/**
 * Reads the resume point: the `Last-Event-ID` header the browser sends on its
 * own, then the query parameter an explicit reconnect uses.
 */
function lastEventId(c: AppContext): number {
  for (const raw of [(c.req.header("Last-Event-ID") ?? "").trim(), (c.req.query("last_event_id") ?? "").trim()]) {
    if (raw === "" || !/^[+-]?\d+$/.test(raw)) {
      continue;
    }
    const id = Number.parseInt(raw, 10);
    if (Number.isFinite(id) && id > 0) {
      return id;
    }
  }
  return 0;
}

/** Reads the page cursor: the two parts of the last message of the previous page. */
function parseCursor(c: AppContext): Cursor | null {
  const cursor: Cursor = { beforeCreatedAtNanos: 0, beforeId: "", limit: queryInt(c, "limit", 0) };

  const raw = (c.req.query("cursor") ?? "").trim();
  if (raw === "") {
    return cursor;
  }

  const decoded = decodeCursor(raw);
  if (decoded === null) {
    return null;
  }
  cursor.beforeCreatedAtNanos = decoded.createdAtNanos;
  cursor.beforeId = decoded.id;
  return cursor;
}

/** Packs a page boundary into one opaque string. */
function encodeCursor(createdAtNanos: number, id: string): string {
  return `${createdAtNanos}.${id}`;
}

/** Reads a page boundary back, or null when it is not one. */
function decodeCursor(raw: string): { createdAtNanos: number; id: string } | null {
  const parts = raw.split(".", 2);
  if (parts.length !== 2 || !/^[+-]?\d+$/.test(parts[0]) || !isUuid(parts[1])) {
    return null;
  }
  return { createdAtNanos: Number.parseInt(parts[0], 10), id: parts[1] };
}

/** A uuid path or body value, or null when it is malformed. */
function parseId(raw: string): string | null {
  const trimmed = raw.trim();
  return isUuid(trimmed) ? trimmed : null;
}

/** An optional uuid, where absence is allowed and malformed is not. */
function parseOptionalId(raw: string): string | null {
  if (raw.trim() === "") {
    return "";
  }
  return parseId(raw);
}

/** A list of uuids, or null when one of them is malformed. */
function parseIds(raw: string[]): string[] | null {
  const ids: string[] = [];
  for (const value of raw) {
    const id = parseId(value);
    if (id === null) {
      return null;
    }
    ids.push(id);
  }
  return ids;
}

/** A 400 with the module's own code, as the Go `badRequest` helper answers. */
function invalidRequest(c: AppContext, message: string): Response {
  return errorEnvelope(c, 400, message, "invalid_request");
}

/** True when an id carries no identity. */
function isNil(id: string): boolean {
  return id === "" || id === "00000000-0000-0000-0000-000000000000";
}

/** Formats a time the way Go's `Format(time.RFC3339)` does, in UTC. */
function rfc3339(value: Date): string {
  return value.toISOString().replace(/\.\d{3}Z$/, "Z");
}

/** Formats a time the way Go's `Format(time.RFC3339Nano)` does. */
function rfc3339Nano(value: Date): string {
  return value.toISOString();
}
