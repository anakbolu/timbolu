/**
 * The conversation contract: conversations with their participants, messages
 * made of content blocks, attachments, and the activity event stream every live
 * view is built from.
 *
 * A message body is a list of typed blocks rather than a string, because the
 * frontend renders each type with its own component: Markdown text, a table, an
 * approval card, a chart specification, Mermaid source, or a sandboxed HTML
 * document. Validating a block here, before it is stored, is what keeps a
 * malformed chart from ever reaching a renderer.
 *
 * Ported 1:1 from internal/modules/chat/domain/conversation.go.
 */

/** Block types. */
export const BlockText = "text";
export const BlockTable = "table";
export const BlockDraft = "draft";
export const BlockChart = "chart";
export const BlockMermaid = "mermaid";
export const BlockHTML = "html";

/** Every block type in the order the UI documents them. */
export const BlockTypes: string[] = [BlockText, BlockTable, BlockDraft, BlockChart, BlockMermaid, BlockHTML];

/**
 * Limits on block content. They exist to bound what one message can cost to
 * render and to store, and the HTML limit is a security boundary as well.
 */
export const HTMLMaxBytes = 500 * 1024;
export const MermaidMaxBytes = 64 * 1024;
export const TextMaxBytes = 256 * 1024;
export const TableMaxColumns = 40;
export const TableMaxRows = 2000;
export const BlocksPerMessage = 64;
export const ChartMaxSeries = 12;
export const ChartMaxPoints = 2000;

/** Errors the service and the handlers map onto responses. */
export const ErrInvalidBlock = new Error("chat: invalid block");
export const ErrBlockTooLarge = new Error("chat: block is too large");
export const ErrConversationNotFound = new Error("chat: conversation not found");
export const ErrMessageNotFound = new Error("chat: message not found");
export const ErrAgentNotInConversation = new Error("chat: agent is not a participant");
export const ErrNoResponder = new Error("chat: no Bolu can answer this message");
export const ErrInvalidInput = new Error("chat: invalid input");
export const ErrFileTypeNotAllowed = new Error("chat: file type not allowed");
export const ErrAttachmentNotFound = new Error("chat: attachment not found");
export const ErrStorageUnavailable = new Error("chat: object storage is not configured");
export const ErrRepairLimitReached = new Error("chat: block repair limit reached");

/** The zero uuid, as Go's `uuid.Nil`. */
export const NilUUID = "00000000-0000-0000-0000-000000000000";

/** True when an id carries no identity. */
export function isNil(id: string | null | undefined): boolean {
  return !id || id === NilUUID;
}

/**
 * An error that carries the sentinel a caller compares against, the way Go's
 * `%w` wrapping does.
 */
export class WrappedError extends Error {
  readonly sentinel: Error;

  constructor(sentinel: Error, detail = "") {
    super(detail ? `${sentinel.message}: ${detail}` : sentinel.message);
    this.name = "WrappedError";
    this.sentinel = sentinel;
  }
}

/** True when `err` is the sentinel or wraps it. */
export function is(err: unknown, sentinel: Error): boolean {
  return err === sentinel || (err instanceof WrappedError && err.sentinel === sentinel);
}

/**
 * One content block of a message.
 *
 * The concrete shape of a block is the JSON document in `body`, and `type` says
 * which schema it must satisfy. Keeping the two together means a message is one
 * JSONB value that the repository stores verbatim, while the type stays
 * addressable for the renderer and for the repair loop.
 */
export interface Block {
  type: string;
  /** The whole block document, including its "type" field. */
  body: unknown;
}

/** Message status. */
export const MessageComplete = "complete";
export const MessageStreaming = "streaming";
export const MessagePartial = "partial";
export const MessageFailed = "failed";

/** Conversation kinds. */
export const KindDirect = "direct";
export const KindGroup = "group";

/** One chat thread: one Bolu and one user, or a group. */
export interface Conversation {
  id: string;
  workspaceId: string;
  kind: string;
  title: string;
  participants: Participant[];
  messageCount: number;
  lastActivityAt: Date;
  createdAt: Date;
  updatedAt: Date;
}

/** A member of a conversation: exactly one of agentId and userId is set. */
export interface Participant {
  id: string;
  conversationId: string;
  agentId: string;
  userId: string;
  displayName: string;
  role: string;
  createdAt: Date;
}

/** Reports whether the participant is a Bolu. */
export function participantIsAgent(p: Participant): boolean {
  return !isNil(p.agentId);
}

/** One turn of a conversation. */
export interface Message {
  id: string;
  workspaceId: string;
  conversationId: string;
  authorAgentId: string;
  authorUserId: string;
  blocks: Block[];
  attachments: Attachment[];
  taskId: string;
  status: string;
  finishReason: string;
  createdAt: Date;
  updatedAt: Date;
}

/** Reports whether a Bolu wrote the message. */
export function messageFromAgent(m: Message): boolean {
  return !isNil(m.authorAgentId);
}

/** Whichever identity wrote the message. */
export function messageAuthorId(m: Message): string {
  return !isNil(m.authorAgentId) ? m.authorAgentId : m.authorUserId;
}

/** One file attached to a message. */
export interface Attachment {
  id: string;
  workspaceId: string;
  messageId: string;
  storageKey: string;
  filename: string;
  contentType: string;
  byteSize: number;
  checksumSha256: string;
  createdAt: Date;
}

/** Renders blocks for the JSONB column. */
export function marshalBlocks(blocks: Block[]): unknown[] {
  const encoded: unknown[] = [];
  for (const block of blocks) {
    if (block.body === undefined || block.body === null) {
      throw new WrappedError(ErrInvalidBlock, "empty block body");
    }
    encoded.push(block.body);
  }
  return encoded;
}

/**
 * Reads blocks from the JSONB column.
 *
 * It reads the "type" of each block out of the document itself, so the stored
 * value is the single source of truth and a row cannot claim a type its body
 * does not have.
 */
export function unmarshalBlocks(raw: unknown): Block[] {
  if (raw === null || raw === undefined) {
    return [];
  }

  let documents: unknown[];
  if (typeof raw === "string") {
    try {
      documents = JSON.parse(raw) as unknown[];
    } catch (err) {
      throw new Error(`chat: decode blocks: ${(err as Error).message}`);
    }
  } else if (Array.isArray(raw)) {
    documents = raw;
  } else {
    throw new Error("chat: decode blocks: blocks column is not an array");
  }

  const blocks: Block[] = [];
  for (const document of documents) {
    if (typeof document !== "object" || document === null) {
      throw new Error("chat: decode block header: block is not an object");
    }
    const header = document as { type?: unknown };
    blocks.push({ type: typeof header.type === "string" ? header.type : "", body: document });
  }
  return blocks;
}

/**
 * Builds a message with the fields a new row does not carry.
 *
 * The repository fills id, timestamps and status; a caller that writes a
 * message only names what it is writing, so the defaults live here rather than
 * at every call site.
 */
export function newMessage(fields: Partial<Message> & { conversationId: string }): Message {
  return {
    id: "",
    workspaceId: "",
    authorAgentId: "",
    authorUserId: "",
    blocks: [],
    attachments: [],
    taskId: "",
    status: MessageComplete,
    finishReason: "",
    createdAt: new Date(0),
    updatedAt: new Date(0),
    ...fields,
  };
}

/**
 * Reads a block body as an object, or null when it is not one. Block bodies are
 * stored as JSONB, so a value may arrive as an object or as its JSON text.
 */
export function decodeOrNull(raw: unknown): Record<string, unknown> | null {
  let value = raw;
  if (typeof value === "string") {
    try {
      value = JSON.parse(value) as unknown;
    } catch {
      return null;
    }
  }
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}
