/**
 * The ports the chat module declares, and which other modules implement.
 *
 * A module may only import another module's domain, so the seams this module
 * needs — the Bolu directory, the task runtime, memory, the model responder and
 * the group router — are interfaces here rather than imports of the modules
 * that satisfy them.
 *
 * Ported 1:1 from internal/modules/chat/domain/ports.go.
 */

import { extname } from "node:path";

import { ContentRefPrefix } from "./block.ts";
import { isNil, type Attachment, type Conversation, type Message } from "./conversation.ts";
import type { StreamChunk } from "./event.ts";

/** The tenant identity a query runs as. */
export interface Scope {
  userId: string;
  workspaceId: string;
}

/** Reports whether the scope carries no identity. */
export function scopeIsZero(s: Scope): boolean {
  return isNil(s.userId) && isNil(s.workspaceId);
}

/**
 * One page request over a conversation's history.
 *
 * Pagination is by cursor rather than offset because a chat is append-heavy: an
 * offset shifts under the reader as new messages arrive. The cursor names the
 * last message of the previous page and the index reads newest-first, so a page
 * boundary is stable no matter how much arrives meanwhile.
 */
export interface Cursor {
  /**
   * The last message of the previous page. Zero means "start from the newest".
   *
   * Nanoseconds because Postgres keeps microseconds: a millisecond cursor would
   * be older than every row it should follow.
   */
  beforeCreatedAtNanos: number;
  beforeId: string;
  /** The page size. Zero means the repository default. */
  limit: number;
}

/** One page of messages plus the cursor for the next one. */
export interface Page {
  messages: Message[];
  nextBeforeCreatedAtNanos: number;
  nextBeforeId: string;
  hasMore: boolean;
}

/** The cursor that reads the page after this one. */
export function nextCursor(page: Page): Cursor {
  return {
    beforeCreatedAtNanos: page.nextBeforeCreatedAtNanos,
    beforeId: page.nextBeforeId,
    limit: page.messages.length,
  };
}

/** The page a client gets when it does not ask for one. */
export const DefaultPageSize = 40;

/** Bounds a page, so one request cannot pull a whole history. */
export const MaxPageSize = 200;

/** The persistence contract for conversations. */
export interface ConversationRepository {
  list(scope: Scope): Promise<Conversation[]>;
  getConversation(scope: Scope, id: string): Promise<Conversation>;
  /** Creates a conversation with its participants in one transaction. */
  create(
    scope: Scope,
    kind: string,
    title: string,
    agentIds: string[],
    userIds: string[],
  ): Promise<Conversation>;
  addParticipants(scope: Scope, id: string, agentIds: string[], userIds: string[]): Promise<void>;
  rename(scope: Scope, id: string, title: string): Promise<void>;
  /** The 1:1 conversation with one Bolu, created on first use. */
  direct(scope: Scope, agentId: string): Promise<Conversation>;
}

/** The persistence contract for messages. */
export interface MessageRepository {
  append(scope: Scope, message: Message): Promise<Message>;
  /** Replaces a message's body and status. */
  update(scope: Scope, message: Message): Promise<Message>;
  getMessage(scope: Scope, id: string): Promise<Message>;
  /** One page of a conversation, newest first. */
  history(scope: Scope, conversationId: string, cursor: Cursor): Promise<Page>;
  /** The newest messages of a conversation, oldest first. */
  latestMessages(scope: Scope, conversationId: string, limit: number): Promise<Message[]>;
}

/** Stores attachment metadata and the bytes behind it. */
export interface AttachmentRepository {
  save(scope: Scope, attachment: Attachment, content: Uint8Array): Promise<Attachment>;
  /**
   * Stores the bytes, opens the message that carries them, and writes the row
   * and the message's read model, all in one transaction.
   */
  saveWithNewMessage(
    scope: Scope,
    message: Message,
    attachment: Attachment,
    content: Uint8Array,
  ): Promise<{ message: Message; attachment: Attachment }>;
  open(scope: Scope, id: string): Promise<Attachment>;
  listForMessage(scope: Scope, messageId: string): Promise<Attachment[]>;
  /** The attachments past a moment, oldest first. */
  listExpired(scope: Scope, olderThan: Date, limit: number): Promise<Attachment[]>;
  /**
   * Removes one attachment's row and the element it left in its message's read
   * model, and answers that message as it now reads. A row that is already gone
   * is not an error: the zero message comes back.
   */
  delete(scope: Scope, id: string): Promise<Message>;
}

/** What a write to object storage produced. */
export interface StoredObject {
  key: string;
  contentType: string;
  byteSize: number;
  /** Hex SHA-256 of the content. */
  checksumSha256: string;
}

/**
 * Holds the bytes that do not belong in Postgres: attachments and the sandboxed
 * HTML documents a message renders.
 */
export interface ObjectStore {
  put(key: string, contentType: string, content: Uint8Array): Promise<StoredObject>;
  get(key: string): Promise<Uint8Array>;
  delete(key: string): Promise<void>;
  /** Every key under a prefix, which is what an erasure needs. */
  list(prefix: string): Promise<string[]>;
  driver(): string;
}

/**
 * Builds the key one object is stored under.
 *
 * The key is namespaced by workspace so a listing of one tenant's objects is a
 * prefix scan, and the filename is sanitized so a name from a client cannot
 * escape its own directory.
 */
export function ObjectKey(prefix: string, workspaceId: string, objectId: string, filename: string): string {
  return [prefix, "workspaces", workspaceId, "objects", objectId, SanitizeFilename(filename)].join("/");
}

/** Where uploaded files live. */
export const AttachmentsPrefix = "attachments";

/** The namespaces this module writes objects under. */
export const ObjectPrefixes: string[] = [AttachmentsPrefix, ContentRefPrefix];

/** Bounds one attachment sweep when the caller does not say how many. */
export const DefaultPruneLimit = 500;

/** The key prefix under which one workspace's objects live, for one namespace. */
export function WorkspaceObjectPrefix(prefix: string, workspaceId: string): string {
  return [prefix, "workspaces", workspaceId, "objects"].join("/");
}

/**
 * Keeps a filename usable as one path segment.
 *
 * A name from a client is untrusted input, so everything that could change the
 * path is replaced rather than escaped.
 */
export function SanitizeFilename(name: string): string {
  let cleaned = name.trim();
  cleaned = cleaned.split("\\").join("-");
  cleaned = cleaned.split("/").join("-");
  cleaned = cleaned.split("\x00").join("");
  cleaned = cleaned.split("..").join(".");
  cleaned = cleaned.replace(/^\.+/, "");
  if (cleaned === "") {
    return "file";
  }
  if (cleaned.length > 120) {
    // Keep the extension, which is what a browser needs to render it.
    const extension = extname(cleaned);
    cleaned = cleaned.slice(0, 120 - extension.length) + extension;
  }
  return cleaned;
}

/**
 * The minimum a conversation needs to know about a Bolu: who it is, what it is
 * for, and which model it uses.
 */
export interface AgentRef {
  id: string;
  name: string;
  role: string;
  persona: string;
  tone: string;
  stored: string;
  display: string;
  tools: string[];
  defaultModel: Record<string, unknown>;
}

/** Reads the Bolu of a workspace. */
export interface AgentDirectory {
  /** One Bolu, or an error when it is not in the workspace. */
  agent(scope: Scope, id: string): Promise<AgentRef>;
  /** The named Bolu, in the order they were asked for. */
  agents(scope: Scope, ids: string[]): Promise<AgentRef[]>;
  /** Reports whether a Bolu accepts new work. A resting Bolu never does. */
  active(ref: AgentRef): boolean;
}

/**
 * What a message becomes when the workspace's package does not allow the reply
 * to be opened. The answer is not "try again" — it is "buy a top-up or wait for
 * the next period".
 */
export const ErrNoBudget = new Error("chat: the workspace's package does not allow a reply");

/**
 * Hands a message to the task runtime. The chat module only records what the
 * user asked for and shows what comes back; running the agent loop is not its
 * job.
 */
export interface TaskStarter {
  /** Opens a task for a message and returns its id. */
  start(scope: Scope, req: StartRequest): Promise<string>;
}

/** One task to open. */
export interface StartRequest {
  conversationId: string;
  /** The placeholder message the task's answer fills. */
  messageId: string;
  agentId: string;
  /** What caused the task: chat, routine, webhook, or handoff. */
  trigger: string;
  title: string;
}

/** Task triggers, matching the schema's check constraint. */
export const TriggerChat = "chat";
export const TriggerRoutine = "routine";
export const TriggerWebhook = "webhook";
export const TriggerHandoff = "handoff";

/** What the chat module asks of the memory module. */
export interface Memory {
  /**
   * Stores what the message asked to be remembered and hands back the sentence
   * the Bolu answers with. A false `handled` means the message was not a memory
   * request.
   */
  remember(scope: Scope, req: RememberRequest): Promise<RememberResult>;
  /** Tells the extractor the thread moved, which pushes its quiet timer out. */
  conversationActive(scope: Scope, conversationId: string, agentId: string): Promise<void>;
}

/** One message that may ask for something to be remembered. */
export interface RememberRequest {
  text: string;
  /** The Bolu being addressed, whose private memory a fact belongs to. */
  agentId: string;
  messageId: string;
}

/** What the memory module answered. */
export interface RememberResult {
  handled: boolean;
  /** What the Bolu answers. It names exactly what was stored. */
  confirmation: string;
  /** Says nothing was stored: the content was sensitive. */
  refused: boolean;
}

/**
 * Answers a message with a stream of chunks. It is the seam the agent runtime
 * plugs into.
 */
export interface Responder {
  /** Streams the answer of one Bolu; the last chunk is Done or Error. */
  reply(scope: Scope, req: ReplyRequest): Promise<AsyncIterable<StreamChunk>>;
}

/** Who the group router picked and why. */
export interface RouterDecision {
  agentId: string;
  /** A short explanation, in the language the UI shows. */
  reason: string;
  /** "model" or "fallback". */
  source: string;
  /** How many Bolu were considered. */
  candidates: number;
}

/** Picks the Bolu that should answer a group message. */
export interface Router {
  /** The responder, or ErrNoResponder when none can answer. */
  pick(scope: Scope, conversation: Conversation, message: Message): Promise<RouterDecision>;
}

/** Router sources. */
export const RouterSourceModel = "model";
export const RouterSourceFallback = "fallback";

/** One answer to produce. */
export interface ReplyRequest {
  conversation: Conversation;
  agent: AgentRef;
  message: Message;
  /** The conversation so far, oldest first. */
  history: Message[];
  /** How many times a block may be regenerated after a validation failure. */
  repairBudget: number;
}

/** How many regeneration attempts a block gets. */
export const DefaultRepairBudget = 2;

export { ContentRefPrefix };
