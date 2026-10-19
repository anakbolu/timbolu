/**
 * The service contract: what the HTTP handlers, the wa-gateway, and the
 * integration adapter call.
 *
 * It has two halves by design. The first is what a person does: link a number,
 * choose who answers it, unlink it. The second is what the gateway does: report
 * it is online, pull what is due, report what left. Both are here rather than in
 * two types because they are two views of one state machine.
 *
 * Ported 1:1 from packages/whatsapp/domain/service.go.
 */

import type { Claimed, Drain, Inbox, LinkSink, Status } from "./engine.ts";
import type {
  Inbound,
  Link,
  MessageFilter,
  Number,
  Outbox,
  OutboxFilter,
  Scope,
} from "./whatsapp.ts";

/** The event name an inbound message is announced under. */
export const EventMessageReceived = "whatsapp.message_received";

/** One number to link. */
export interface LinkRequest {
  /** What the person says the number is, as E.164. */
  phone: string;
  label: string;
  /** The Bolu that answers this number. Required. */
  agentID: string;
}

/** What the transport knows about a number. */
export interface ReportRequest {
  numberID: string;
  status: string;
  /** Explains a status a person has to act on, in their words. */
  reason: string;
  /** When the observation was made. */
  at: Date;
}

/** One message to send. */
export interface EnqueueRequest {
  numberID: string;
  toJID: string;
  kind: string;
  text: string;
  /** Where the attachment was stored, for a message that carries one. */
  mediaKey: string;
  /** The draft the message came from, when it came from one. */
  draftID: string;
}

/**
 * What the handlers, the gateway, and the adapter call. It extends the five
 * transport-facing ports so one concrete class can be handed to each of them.
 */
export interface ServicePort extends Inbox, Drain, Status, LinkSink {
  // --- what a person does -------------------------------------------------

  /** Returns the workspace's numbers. */
  numbers(scope: Scope): Promise<Number[]>;
  /** Returns one number. */
  number(scope: Scope, id: string): Promise<Number>;

  /** Records one number and who answers it, before anything is linked. */
  prepare(scope: Scope, request: LinkRequest): Promise<Number>;
  /** Returns one message, which is what serving its attachment needs. */
  messageByID(scope: Scope, id: string): Promise<Inbound>;
  /** Reads one stored attachment. */
  mediaBytes(key: string): Promise<Uint8Array>;

  /** Starts linking one number and answers the attempt to watch. */
  beginLink(scope: Scope, request: LinkRequest): Promise<Link>;
  /** Reports where an attempt stands, and finishes the link when paired. */
  linkState(scope: Scope, linkID: string): Promise<Link>;
  /** Asks for the phone-code alternative to scanning. */
  pairCode(scope: Scope, linkID: string, phone: string): Promise<Link>;
  /** Abandons an attempt. */
  cancelLink(scope: Scope, linkID: string): Promise<void>;
  /** Records that the workspace accepted the warning. */
  acceptTerms(scope: Scope, id: string): Promise<Number>;
  /** Chooses which Bolu answers this number. */
  assignAgent(scope: Scope, id: string, agentID: string): Promise<Number>;
  /** Changes the number's hourly ceiling. */
  setLimit(scope: Scope, id: string, limit: number): Promise<Number>;
  /** Renames the number as the workspace refers to it. */
  setLabel(scope: Scope, id: string, label: string): Promise<Number>;
  /** Ends the session and clears what was stored. */
  unlink(scope: Scope, id: string): Promise<Number>;

  /** Returns what customers wrote, newest first. */
  messages(scope: Scope, filter: MessageFilter): Promise<Inbound[]>;
  /** Returns one conversation oldest first, for the connection a call names. */
  history(scope: Scope, integrationID: string, chatJID: string, limit: number): Promise<Inbound[]>;
  /** Answers which number a connection speaks as. */
  numberByIntegration(scope: Scope, integrationID: string): Promise<string>;
  /** Returns what this number has been asked to send, newest first. */
  outbox(scope: Scope, filter: OutboxFilter): Promise<Outbox[]>;

  // --- what the gateway does ----------------------------------------------

  /** Records one message to send. */
  enqueue(scope: Scope, request: EnqueueRequest): Promise<Outbox>;
  /** Records one inbound message and answers whether it was new. */
  receive(
    scope: Scope,
    numberID: string,
    message: Inbound,
  ): Promise<{ message: Inbound; created: boolean }>;

  /** Returns the messages of one number that may go out now. */
  claim(scope: Scope, id: string, limit: number): Promise<Claimed>;
}
