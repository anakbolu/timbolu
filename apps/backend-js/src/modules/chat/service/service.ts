/**
 * The conversation use cases: threads, block messages, attachments, the reply
 * stream, the group router, and the activity events every live view is built
 * from.
 *
 * Two rules run through the module:
 *   - a message is a list of validated blocks, so a renderer is never handed a
 *     shape the backend did not check;
 *   - an event is written before it is published, so a subscriber that
 *     reconnects finds the row it missed rather than a gap.
 *
 * Ported 1:1 from internal/modules/chat/service/service.go.
 */

import type { Logger } from "../../../platform/logger.ts";
import { DecodeContentRef } from "../domain/block.ts";
import { AttachmentMaxBytes as MaxUploadBytes } from "../domain/service.ts";
import {
  ErrInvalidInput,
  ErrStorageUnavailable,
  KindGroup,
  MessageComplete,
  WrappedError,
  isNil,
  newMessage,
  type Attachment,
  type Conversation,
  type Message,
} from "../domain/conversation.ts";
import {
  EventAgentState,
  EventMessageNew,
  EventMessageUpdated,
  EventRouterDecision,
  type AgentStatePayload,
  type Event,
  type EventPublisher,
  type EventStore,
} from "../domain/event.ts";
import {
  AttachmentsPrefix,
  DefaultPruneLimit,
  DefaultRepairBudget,
  ObjectKey,
  ObjectPrefixes,
  WorkspaceObjectPrefix,
  type AgentDirectory,
  type AgentRef,
  type AttachmentRepository,
  type ConversationRepository,
  type Cursor,
  type Memory,
  type MessageRepository,
  type ObjectStore,
  type Page,
  type Responder,
  type Router,
  type RouterDecision,
  type Scope,
  type TaskStarter,
} from "../domain/ports.ts";
import type {
  AttachRequest,
  AttachmentPrune,
  CreateGroupRequest,
  AddParticipantsRequest,
  SendRequest,
  SendResult,
  Service as ChatService,
} from "../domain/service.ts";
import { WireMessage, WireState } from "../domain/wire.ts";
import { allowedExtensions, validateAttachment } from "./attachment_type.ts";
import { remember as rememberMessage, scheduleExtraction, type MemoryDeps } from "./memory.ts";
import { sendMessage, runReply } from "./reply.ts";
import { streamEvents } from "./stream.ts";

/** Limits the service enforces on top of the block schemas. */
export const TextMaxRunes = 8000;
export const GroupMaxParticipants = 12;
export const TitleMaxRunes = 120;
export const EventPageLimit = 200;
export const EventPageMax = 1000;

/** Bounds one reply, so a stuck provider cannot hold a placeholder open forever. */
export const replyTimeout = 5 * 60_000;

/** Bounds the detached context a sweep publishes its updates on. */
export const pruneEmitTimeout = 5_000;

/** The service settings. */
export interface Config {
  /** How many times a block may be regenerated. Zero means DefaultRepairBudget. */
  repairBudget: number;
  /** How often a growing reply is written and published, in milliseconds. */
  streamFlush: number;
  /**
   * Narrows which file types an upload may be, by extension. Empty means the
   * default list; it can only narrow.
   */
  attachmentAllowedExtensions: string[];
}

/** The flush interval of a streaming reply. */
export const DefaultStreamFlush = 150;

/** The service dependencies. */
export interface Deps {
  conversations: ConversationRepository;
  messages: MessageRepository;
  attachments: AttachmentRepository;
  events: EventStore;
  publisher: EventPublisher | null;
  agents: AgentDirectory;
  responder: Responder;
  router: Router | null;
  /** Opens the durable task a message belongs to. Optional. */
  tasks: TaskStarter | null;
  /** Stores what a message asks to be remembered. Optional. */
  memory: Memory | null;
  /** Holds attachment bytes. Optional: without it the endpoints report 503. */
  storage: ObjectStore | null;
  logger: Logger | null;
  config?: Partial<Config>;
}

/** The conversation implementation. */
export class Service implements ChatService {
  readonly deps: Deps;
  readonly settings: Config;
  /** The time source, so the stream flush is testable without waiting. */
  clock: () => Date = () => new Date();

  constructor(deps: Deps) {
    this.deps = deps;
    const config = deps.config ?? {};
    this.settings = {
      repairBudget:
        config.repairBudget && config.repairBudget > 0 ? config.repairBudget : DefaultRepairBudget,
      streamFlush: config.streamFlush && config.streamFlush > 0 ? config.streamFlush : DefaultStreamFlush,
      attachmentAllowedExtensions: allowedExtensions(
        config.attachmentAllowedExtensions ?? [],
        deps.logger,
      ),
    };
  }

  /** Replaces the time source. */
  setClock(clock: () => Date): void {
    this.clock = clock;
  }

  // ----------------------------------------------------------- conversations

  async conversations(scope: Scope): Promise<Conversation[]> {
    requireWorkspace(scope);
    return this.deps.conversations.list(scope);
  }

  async conversation(scope: Scope, id: string): Promise<Conversation> {
    requireWorkspace(scope);
    if (isNil(id)) {
      throw new WrappedError(ErrInvalidInput, "conversation id is required");
    }
    return this.deps.conversations.getConversation(scope, id);
  }

  async direct(scope: Scope, agentId: string): Promise<Conversation> {
    requireWorkspace(scope);
    if (isNil(agentId)) {
      throw new WrappedError(ErrInvalidInput, "agent id is required");
    }

    // The Bolu must exist in this workspace, which the directory checks; a
    // conversation with a Bolu from another tenant would otherwise be creatable.
    await this.deps.agents.agent(scope, agentId);

    return this.deps.conversations.direct(scope, agentId);
  }

  async createGroup(scope: Scope, req: CreateGroupRequest): Promise<Conversation> {
    requireWorkspace(scope);

    const title = req.title.trim();
    if (title === "") {
      throw new WrappedError(ErrInvalidInput, "a group needs a name");
    }
    if (Array.from(title).length > TitleMaxRunes) {
      throw new WrappedError(ErrInvalidInput, `name is longer than ${TitleMaxRunes} characters`);
    }

    const agentIds = await this.knownAgents(scope, req.agentIds);
    const userIds = dedupe(req.userIds);
    if (agentIds.length + userIds.length < 2) {
      throw new WrappedError(ErrInvalidInput, "a group needs at least two participants");
    }
    if (agentIds.length + userIds.length > GroupMaxParticipants) {
      throw new WrappedError(ErrInvalidInput, `at most ${GroupMaxParticipants} participants are allowed`);
    }

    return this.deps.conversations.create(scope, KindGroup, title, agentIds, userIds);
  }

  async addParticipants(
    scope: Scope,
    id: string,
    req: AddParticipantsRequest,
  ): Promise<Conversation> {
    requireWorkspace(scope);

    const conversation = await this.deps.conversations.getConversation(scope, id);
    if (conversation.kind !== KindGroup) {
      throw new WrappedError(ErrInvalidInput, "only a group can take new participants");
    }

    const agentIds = await this.knownAgents(scope, req.agentIds);
    const userIds = dedupe(req.userIds);
    if (agentIds.length + userIds.length === 0) {
      throw new WrappedError(ErrInvalidInput, "no participants were named");
    }
    if (conversation.participants.length + agentIds.length + userIds.length > GroupMaxParticipants) {
      throw new WrappedError(ErrInvalidInput, `at most ${GroupMaxParticipants} participants are allowed`);
    }

    await this.deps.conversations.addParticipants(scope, id, agentIds, userIds);
    return this.deps.conversations.getConversation(scope, id);
  }

  async history(scope: Scope, conversationId: string, cursor: Cursor): Promise<Page> {
    requireWorkspace(scope);

    // Reading a page confirms the caller may see the thread: a conversation id
    // from another workspace must answer "not found" rather than "empty".
    await this.deps.conversations.getConversation(scope, conversationId);

    return this.deps.messages.history(scope, conversationId, cursor);
  }

  // ---------------------------------------------------------------- messages

  send(scope: Scope, req: SendRequest): Promise<SendResult> {
    return sendMessage(this, scope, req);
  }

  /** Produces one answer and keeps the stored message in step with it. */
  runReply(
    scope: Scope,
    conversation: Conversation,
    agent: AgentRef,
    prompt: Message,
    placeholder: Message,
  ): Promise<void> {
    return runReply(this, scope, conversation, agent, prompt, placeholder);
  }

  // ------------------------------------------------------------------ events

  async events(scope: Scope, afterId: number, limit: number): Promise<Event[]> {
    requireWorkspace(scope);

    let bounded = limit;
    if (bounded <= 0) {
      bounded = EventPageLimit;
    }
    if (bounded > EventPageMax) {
      bounded = EventPageMax;
    }

    return this.deps.events.sinceEvents(scope.workspaceId, afterId, bounded);
  }

  stream(scope: Scope, afterId: number, signal?: AbortSignal): Promise<AsyncIterable<Event>> {
    return Promise.resolve(streamEvents(this.deps, scope, afterId, signal));
  }

  /**
   * Records an event and publishes it, in that order.
   *
   * The order is the whole point: the row is the record, so a publish that fails
   * costs latency for the clients connected right now and never an event.
   */
  async emit(event: Event): Promise<void> {
    if (isNil(event.workspaceId)) {
      throw new WrappedError(ErrInvalidInput, "an event needs a workspace");
    }

    const stored = await this.deps.events.appendEvent(event);

    if (this.deps.publisher === null) {
      return;
    }
    try {
      await this.deps.publisher.publish(stored.workspaceId, stored);
    } catch (err) {
      // Not an error: the row is written, so a reconnecting client still sees
      // it. It does mean the live view is behind until then.
      this.deps.logger?.warn(
        { type: stored.type, err: (err as Error).message },
        "event published only to the store",
      );
    }
  }

  // ------------------------------------------------------------- attachments

  /**
   * Stores a file and returns its metadata.
   *
   * With a messageId the file joins a message that already exists. Without one
   * the file *is* the message: the carrier is opened inside the same transaction
   * that records the row, so an upload that cannot be stored leaves neither a
   * row nor an empty bubble.
   */
  async attach(scope: Scope, req: AttachRequest, content: Uint8Array): Promise<Attachment> {
    requireWorkspace(scope);
    if (this.deps.storage === null) {
      throw ErrStorageUnavailable;
    }
    if (content.length === 0) {
      throw new WrappedError(ErrInvalidInput, "the file is empty");
    }
    if (content.length > MaxUploadBytes) {
      throw new WrappedError(ErrInvalidInput, `the file is larger than ${MaxUploadBytes} bytes`);
    }

    // A file with no name has no extension either, which the type check refuses.
    const filename = req.filename.trim();

    // The type is settled before the first write: a refused upload must not open
    // a message, leave an object, or publish anything.
    const contentType = validateAttachment(filename, content, this.settings.attachmentAllowedExtensions);

    const opensMessage = isNil(req.messageId);
    if (opensMessage) {
      if (isNil(req.conversationId)) {
        throw new WrappedError(ErrInvalidInput, "a message id or a conversation id is required");
      }
      // The thread must exist in this workspace before a message may be opened.
      await this.deps.conversations.getConversation(scope, req.conversationId);
    } else {
      // The message must exist in this workspace before a file may point at it.
      await this.deps.messages.getMessage(scope, req.messageId);
    }

    const attachmentId = crypto.randomUUID();
    const key = ObjectKey(AttachmentsPrefix, scope.workspaceId, attachmentId, filename);

    // The bytes go to storage first: a row that points at nothing would be worse
    // than an object nobody points at, because the row is what a user sees.
    const object = await this.deps.storage.put(key, contentType, content);

    const attachment: Attachment = {
      id: attachmentId,
      workspaceId: scope.workspaceId,
      messageId: req.messageId,
      storageKey: object.key,
      filename,
      contentType,
      byteSize: object.byteSize,
      checksumSha256: object.checksumSha256,
      createdAt: new Date(0),
    };

    if (opensMessage) {
      let opened: { message: Message; attachment: Attachment };
      try {
        opened = await this.deps.attachments.saveWithNewMessage(
          scope,
          newMessage({
            workspaceId: scope.workspaceId,
            conversationId: req.conversationId,
            authorUserId: scope.userId,
            blocks: [],
            status: MessageComplete,
          }),
          attachment,
          content,
        );
      } catch (err) {
        await this.discardObject(key, err);
        throw err;
      }

      // One event, and it is "new": the message did not exist before this
      // request, so there is no earlier event a client could have rendered.
      await this.emitMessage(scope, opened.message, opened.message.conversationId, EventMessageNew);
      return opened.attachment;
    }

    let stored: Attachment;
    try {
      stored = await this.deps.attachments.save(scope, attachment, content);
    } catch (err) {
      await this.discardObject(key, err);
      throw err;
    }

    // The message is reloaded and published, because what a client renders is
    // the message's attachments, not the row just written. A failure to read it
    // back must not fail the upload.
    try {
      const message = await this.deps.messages.getMessage(scope, req.messageId);
      await this.emitMessage(scope, message, message.conversationId, EventMessageUpdated);
    } catch (err) {
      this.deps.logger?.warn(
        { message_id: req.messageId, err: (err as Error).message },
        "could not reload the message an attachment was added to",
      );
    }

    return stored;
  }

  /** The metadata of one attachment. */
  async attachment(scope: Scope, id: string): Promise<Attachment> {
    requireWorkspace(scope);
    return this.deps.attachments.open(scope, id);
  }

  /** The metadata and the bytes of one attachment. */
  async attachmentContent(
    scope: Scope,
    id: string,
  ): Promise<{ attachment: Attachment; content: Uint8Array }> {
    requireWorkspace(scope);
    if (this.deps.storage === null) {
      throw ErrStorageUnavailable;
    }

    const attachment = await this.deps.attachments.open(scope, id);
    const content = await this.deps.storage
      .get(attachment.storageKey)
      .catch((err: unknown) => {
        throw new Error(`chat: read attachment: ${(err as Error).message}`);
      });
    return { attachment, content };
  }

  /**
   * Removes the attachments of one workspace that are past a moment, row and
   * object together, and reports what went.
   *
   * The order is the object first and the row second, the reverse of attach and
   * for the same reason: a row is what a user can see, so it is the half that
   * must not point at nothing. A row whose object is gone is fixed by the next
   * pass, because removing an absent object is not an error.
   */
  async pruneAttachments(scope: Scope, olderThan: Date, limit: number): Promise<AttachmentPrune> {
    requireWorkspace(scope);
    if (this.deps.storage === null) {
      throw ErrStorageUnavailable;
    }

    const expired = await this.deps.attachments.listExpired(
      scope,
      olderThan,
      limit <= 0 ? DefaultPruneLimit : limit,
    );

    const result: AttachmentPrune = { count: 0, bytesFreed: 0 };
    const failures: Error[] = [];

    // The last message Delete answered with for each message, in the order the
    // messages were first touched: that message is the read model after that row
    // went, so the final one per message is the list a client should render.
    const updated = new Map<string, Message>();
    const order: string[] = [];

    for (const attachment of expired) {
      try {
        await this.deps.storage.delete(attachment.storageKey);
      } catch (err) {
        // The row stays, so the attachment is still whole and still readable —
        // and the next pass tries again rather than losing the file silently.
        failures.push(new Error(`chat: remove attachment object: ${(err as Error).message}`));
        continue;
      }

      let message: Message;
      try {
        message = await this.deps.attachments.delete(scope, attachment.id);
      } catch (err) {
        failures.push(new Error(`chat: remove attachment row: ${(err as Error).message}`));
        continue;
      }
      if (!isNil(message.id)) {
        if (!updated.has(message.id)) {
          order.push(message.id);
        }
        updated.set(message.id, message);
      }

      result.count++;
      result.bytesFreed += attachment.byteSize;
    }

    // One event per message, not one per row, and it carries the whole
    // attachment list as it reads after the last row of that message went.
    for (const id of order) {
      const message = updated.get(id);
      if (!message) continue;
      try {
        await this.emitMessage(scope, message, message.conversationId, EventMessageUpdated);
      } catch (err) {
        this.deps.logger?.error(
          {
            message_id: message.id,
            conversation_id: message.conversationId,
            err: (err as Error).message,
          },
          "could not publish the message an attachment was pruned from",
        );
      }
    }

    if (failures.length > 0) {
      throw new AggregateError(failures, "chat: prune attachments");
    }
    return result;
  }

  /**
   * Removes every object this workspace owns and reports how many went.
   *
   * It walks the module's own prefixes rather than the attachment rows: a
   * sandboxed HTML document a Bolu wrote is an object with no row at all, so a
   * deletion driven from the database would leave it in the bucket with nothing
   * pointing at it.
   */
  async eraseObjects(workspaceId: string): Promise<number> {
    if (isNil(workspaceId)) {
      throw new WrappedError(ErrInvalidInput, "workspace id is required");
    }
    if (this.deps.storage === null) {
      throw ErrStorageUnavailable;
    }

    let removed = 0;
    const failures: Error[] = [];

    for (const prefix of ObjectPrefixes) {
      let keys: string[];
      try {
        keys = await this.deps.storage.list(WorkspaceObjectPrefix(prefix, workspaceId));
      } catch (err) {
        failures.push(new Error(`chat: list ${prefix} objects: ${(err as Error).message}`));
        continue;
      }

      for (const key of keys) {
        try {
          await this.deps.storage.delete(key);
        } catch (err) {
          failures.push(new Error(`chat: delete object: ${(err as Error).message}`));
          continue;
        }
        removed++;
      }
    }

    if (failures.length > 0) {
      throw new AggregateError(failures, "chat: erase objects");
    }
    return removed;
  }

  /**
   * One sandboxed HTML document by its reference.
   *
   * The reference names an object under the content prefix and nothing else,
   * which is what keeps this read from becoming a way to fetch any object in the
   * bucket: a reference that does not decode to a content key is refused before
   * storage is touched.
   */
  async content(reference: string): Promise<Uint8Array> {
    if (this.deps.storage === null) {
      throw ErrStorageUnavailable;
    }

    const key = DecodeContentRef(reference);
    return this.deps.storage.get(key).catch((err: unknown) => {
      throw new Error(`chat: read content: ${(err as Error).message}`);
    });
  }

  // ----------------------------------------------------------------- helpers

  /** Publishes a message event, and drops whether it went. */
  async emitMessage(
    scope: Scope,
    message: Message,
    conversationId: string,
    eventType: string,
  ): Promise<void> {
    await this.emitMessageErr(scope, message, conversationId, eventType);
  }

  /** Publishes a message event and reports whether it went. */
  async emitMessageErr(
    scope: Scope,
    message: Message,
    conversationId: string,
    eventType: string,
  ): Promise<void> {
    const payload = WireMessage({ ...message, conversationId });
    payload.conversation_id = conversationId;

    await this.emit({
      id: 0,
      workspaceId: scope.workspaceId,
      type: eventType,
      actorAgentId: message.authorAgentId,
      actorUserId: message.authorUserId,
      conversationId,
      taskId: message.taskId,
      draftId: "",
      payload,
      createdAt: new Date(0),
    });
  }

  /** Publishes a Bolu's derived state to the live stream. */
  async emitAgentState(
    scope: Scope,
    agentId: string,
    state: string,
    payload: AgentStatePayload,
  ): Promise<void> {
    await this.emit({
      id: 0,
      workspaceId: scope.workspaceId,
      type: EventAgentState,
      actorAgentId: agentId,
      actorUserId: "",
      conversationId: "",
      taskId: parseWireId(payload.task_id ?? ""),
      draftId: parseWireId(payload.draft_id ?? ""),
      payload: WireState(agentId, state, payload.reason ?? "", payload.task_id ?? "", payload.draft_id ?? ""),
      createdAt: new Date(0),
    });
  }

  /** Publishes who the router picked and why. */
  async emitRouterDecision(
    scope: Scope,
    conversation: Conversation,
    message: Message,
    decision: RouterDecision,
  ): Promise<void> {
    await this.emit({
      id: 0,
      workspaceId: scope.workspaceId,
      type: EventRouterDecision,
      actorAgentId: decision.agentId,
      actorUserId: "",
      conversationId: conversation.id,
      taskId: message.taskId,
      draftId: "",
      payload: {
        agent_id: decision.agentId,
        reason: decision.reason,
        source: decision.source,
        candidates: decision.candidates,
      },
      createdAt: new Date(0),
    });
  }

  /** Removes an object whose row could not be written. */
  private async discardObject(key: string, cause: unknown): Promise<void> {
    if (this.deps.storage === null) return;
    try {
      await this.deps.storage.delete(key);
    } catch (err) {
      this.deps.logger?.warn(
        { key, err: (err as Error).message, cause: (cause as Error)?.message },
        "attachment object left behind after a failed write",
      );
    }
  }

  /** Validates the Bolu of a request and returns them deduplicated, in order. */
  private async knownAgents(scope: Scope, ids: string[]): Promise<string[]> {
    const unique = dedupe(ids);
    if (unique.length === 0) {
      return [];
    }

    // The directory checks membership, so a Bolu from another workspace cannot
    // be added to a conversation here.
    await this.deps.agents.agents(scope, unique);
    return unique;
  }

  /** The memory helpers' view of this service. */
  memoryDeps(): MemoryDeps {
    return {
      memory: this.deps.memory as Memory,
      append: (scope, message) => this.deps.messages.append(scope, message),
      emitMessage: (scope, message, conversationId, type) =>
        this.emitMessage(scope, message, conversationId, type),
      logger: this.deps.logger,
    };
  }

  /** The two memory questions, as the send path asks them. */
  scheduleExtraction(scope: Scope, conversation: Conversation, addressed: string): Promise<void> {
    return scheduleExtraction(this.memoryDeps(), scope, conversation, addressed);
  }

  remember(
    scope: Scope,
    conversation: Conversation,
    responderId: string,
    userMessage: Message,
    text: string,
    result: SendResult,
  ): Promise<boolean> {
    return rememberMessage(
      this.memoryDeps(),
      scope,
      conversation,
      responderId,
      userMessage,
      text,
      result,
    );
  }
}

/** What the handler calls. */
export interface ServicePort extends ChatService {}

/** Raises when the scope carries no workspace. */
export function requireWorkspace(scope: Scope): void {
  if (isNil(scope.workspaceId)) {
    throw new WrappedError(ErrInvalidInput, "workspace is required");
  }
}

/** Drops the zero id and duplicates, keeping the order asked for. */
export function dedupe(ids: string[]): string[] {
  const seen = new Set<string>();
  const unique: string[] = [];
  for (const id of ids) {
    if (isNil(id) || seen.has(id)) {
      continue;
    }
    seen.add(id);
    unique.push(id);
  }
  return unique;
}

/**
 * Reads an id off the wire shape, which carries absence as an empty string. An
 * unreadable value is treated as absent rather than as an error: a bad task id
 * must not drop the event.
 */
function parseWireId(value: string): string {
  return /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/.test(value)
    ? value
    : "";
}
