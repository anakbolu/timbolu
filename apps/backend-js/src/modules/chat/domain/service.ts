/**
 * The conversation use-case contract: what the handler calls.
 *
 * Ported 1:1 from internal/modules/chat/domain/service.go.
 */

import type { Attachment, Conversation, Message } from "./conversation.ts";
import type { Event } from "./event.ts";
import type { AgentRef, Cursor, Page, Scope } from "./ports.ts";

/** The conversation use case contract. */
export interface Service {
  /** The threads of the workspace, most recent first. */
  conversations(scope: Scope): Promise<Conversation[]>;
  /** One thread with its participants. */
  conversation(scope: Scope, id: string): Promise<Conversation>;
  /** The 1:1 thread with one Bolu, creating it on first use. */
  direct(scope: Scope, agentId: string): Promise<Conversation>;
  /** A group with the named Bolu and members. */
  createGroup(scope: Scope, req: CreateGroupRequest): Promise<Conversation>;
  addParticipants(scope: Scope, id: string, req: AddParticipantsRequest): Promise<Conversation>;
  /** One page of a conversation's messages, newest first. */
  history(scope: Scope, conversationId: string, cursor: Cursor): Promise<Page>;
  /**
   * Stores the user's message and starts the reply. It returns as soon as the
   * message is stored: the answer arrives on the event stream.
   */
  send(scope: Scope, req: SendRequest): Promise<SendResult>;
  /** The workspace stream after an id, which is how a reconnect fills the gap. */
  events(scope: Scope, afterId: number, limit: number): Promise<Event[]>;
  /** The backlog a client missed, then everything live. */
  stream(scope: Scope, afterId: number, signal?: AbortSignal): Promise<AsyncIterable<Event>>;
  /** Stores a file and returns its metadata. */
  attach(scope: Scope, req: AttachRequest, content: Uint8Array): Promise<Attachment>;
  /** The metadata of one file. */
  attachment(scope: Scope, id: string): Promise<Attachment>;
  /** The metadata and the bytes of one file. */
  attachmentContent(scope: Scope, id: string): Promise<{ attachment: Attachment; content: Uint8Array }>;
  /** Removes every object this workspace owns and reports how many went. */
  eraseObjects(workspaceId: string): Promise<number>;
  /** Removes the files past a moment, row and object together. */
  pruneAttachments(scope: Scope, olderThan: Date, limit: number): Promise<AttachmentPrune>;
  /**
   * One sandboxed HTML document by its reference. It carries no tenant scope on
   * purpose: the reference is the capability.
   */
  content(reference: string): Promise<Uint8Array>;
}

/** A new group. */
export interface CreateGroupRequest {
  title: string;
  agentIds: string[];
  userIds: string[];
}

/** Adds people to an existing group. */
export interface AddParticipantsRequest {
  agentIds: string[];
  userIds: string[];
}

/** One message from the user. */
export interface SendRequest {
  conversationId: string;
  /** The message body. It is stored as one text block. */
  text: string;
  /** Files already uploaded, referenced by id. */
  attachmentIds: string[];
  /** A client that only wants to post (a note) leaves it false. */
  reply: boolean;
  /** Opens a message that will carry an upload; it may have no text. */
  forAttachment: boolean;
  /**
   * Pins the responder. In a group it overrides both the router and the names
   * in the text, so exactly this one Bolu answers.
   */
  agentId: string;
}

/** What storing a message produced. */
export interface SendResult {
  message: Message;
  /** The placeholders the answers will fill, in the order the Bolu were picked. */
  replyMessageIds: string[];
  /** The Bolu that will answer. */
  responders: AgentRef[];
  /** The group router chose, rather than the caller pinning a Bolu. */
  routed: boolean;
}

/** One file to store. */
export interface AttachRequest {
  /**
   * The message the file belongs to. When empty the file belongs to a message
   * the server opens for it, and `conversationId` names the thread.
   */
  messageId: string;
  /** The thread a file without a messageId belongs to. */
  conversationId: string;
  /**
   * The name the client sent, which decides the file's type: the extension must
   * be one the deployment allows, and the stored type is derived from it rather
   * than taken from the request.
   */
  filename: string;
}

/** Caps one upload. */
export const AttachmentMaxBytes = 25 * 1024 * 1024;

/** What one attachment sweep removed. */
export interface AttachmentPrune {
  count: number;
  bytesFreed: number;
}

/** Bounds one attachment sweep when the caller does not say how many. */
export const DefaultPruneLimit = 500;
