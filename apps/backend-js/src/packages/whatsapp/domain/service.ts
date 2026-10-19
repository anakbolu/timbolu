/**
 * What the HTTP handlers, the wa-gateway, and the integration adapter call.
 *
 * It has two halves by design: what a person does (link a number, choose who
 * answers it, unlink it) and what the gateway does (report it is online, pull
 * what is due, report what left). Both are here rather than in two types
 * because they are two views of one state machine.
 *
 * Ported 1:1 from packages/whatsapp/domain/service.go. Go's `context.Context`
 * is the request scope and is not carried in the TS signatures below.
 */

import type { Claimed, PairedLink } from "./engine.ts";
import type { Inbound, Link, Media, MessageFilter, Number, Outbox, OutboxFilter, Scope } from "./whatsapp.ts";

/**
 * The event name an inbound message is announced under. It is the same shape
 * the hub uses (`app.thing`).
 */
export const EventMessageReceived = "whatsapp.message_received";

/** What the handlers, the gateway, and the integration adapter call. */
export interface Service {
  // --- what a person does -------------------------------------------------

  /** Returns the workspace's numbers. */
  numbers(scope: Scope): Promise<Number[]>;
  /** Returns one number. */
  number(scope: Scope, id: string): Promise<Number>;

  /**
   * Readies one number to be linked: it records which number is about to be
   * linked and which Bolu answers it. It links nothing.
   */
  prepare(scope: Scope, request: LinkRequest): Promise<Number>;
  /** Returns one message, which is what serving its attachment needs. */
  messageByID(scope: Scope, id: string): Promise<Inbound>;
  /** Reads one stored attachment. */
  mediaBytes(key: string): Promise<Uint8Array>;

  /**
   * Starts linking one number and answers the attempt to watch. It refuses
   * until the terms warning has been accepted and a Bolu has been chosen.
   */
  beginLink(scope: Scope, request: LinkRequest): Promise<Link>;
  /** Reports where an attempt stands, finishing the link when the phone paired. */
  linkState(scope: Scope, linkID: string): Promise<Link>;
  /** Asks for the phone-code alternative to scanning. */
  pairCode(scope: Scope, linkID: string, phone: string): Promise<Link>;
  /** Abandons an attempt. */
  cancelLink(scope: Scope, linkID: string): Promise<void>;
  /** Records that the workspace accepted the warning, which unblocks BeginLink. */
  acceptTerms(scope: Scope, id: string): Promise<Number>;
  /** Chooses which Bolu answers this number. */
  assignAgent(scope: Scope, id: string, agentID: string): Promise<Number>;
  /** Changes the number's hourly ceiling. */
  setLimit(scope: Scope, id: string, limit: number): Promise<Number>;
  /** Renames the number as the workspace refers to it. */
  setLabel(scope: Scope, id: string, label: string): Promise<Number>;
  /** Ends the session and clears what was stored; the message history stays. */
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

  /**
   * What the transport calls when a phone finished pairing: the connection is
   * created, the number is marked connected, and the fleet is told to serve it.
   */
  paired(scope: Scope, paired: PairedLink): Promise<void>;

  /**
   * Records what the transport knows about a number: online, offline, or
   * refused. A status that needs a person to act alerts them.
   */
  report(scope: Scope, request: ReportRequest): Promise<Number>;

  /**
   * Returns the messages of one number that may go out now, claiming them in
   * the same step and holding them to the hourly ceiling.
   */
  claim(scope: Scope, id: string, limit: number): Promise<Claimed>;

  /** Records that a message left, with WhatsApp's own id for it. */
  sent(scope: Scope, id: string, waMessageID: string, at: Date): Promise<void>;
  /** Puts a message back in the queue with a later time. */
  retry(scope: Scope, id: string, reason: string, at: Date): Promise<void>;
  /** Records that a message will not be retried, so it stays visible. */
  failed(scope: Scope, id: string, reason: string, attempts: number): Promise<void>;

  /** Puts messages a dead gateway left mid-send back in the queue. */
  reclaim(scope: Scope, olderThan: Date): Promise<number>;

  /** Records one message to send; the module never decides that it should. */
  enqueue(scope: Scope, request: EnqueueRequest): Promise<Outbox>;

  /**
   * Records one inbound message and answers whether it was new. A redelivery
   * answers false and opens nothing, which makes the path idempotent.
   */
  receive(
    scope: Scope,
    numberID: string,
    message: Inbound,
  ): Promise<{ message: Inbound; created: boolean }>;

  /** Returns the messages whose bytes have not been fetched. */
  pendingMedia(scope: Scope, limit: number): Promise<Inbound[]>;
  /** Records where a message's bytes were stored, or that they could not be. */
  storeMedia(scope: Scope, id: string, media: Media): Promise<void>;
}

/** One number to link. */
export interface LinkRequest {
  /**
   * What the person says the number is, as E.164. Used for the phone-code
   * alternative; the authoritative value comes back from WhatsApp.
   */
  Phone: string;
  Label: string;
  /** The Bolu that answers this number. Required. */
  AgentID: string;
}

/** What the transport knows about a number. */
export interface ReportRequest {
  NumberID: string;
  Status: string;
  /** Explains a status a person has to act on, in their words. */
  Reason: string;
  /** When the observation was made, so a stale report cannot move it backwards. */
  At: Date;
}

/** One message to send. */
export interface EnqueueRequest {
  NumberID: string;
  ToJID: string;
  Kind: string;
  Text: string;
  /** Where the attachment was stored, for a message that carries one. */
  MediaKey: string;
  /** The draft the message came from, when it came from one. */
  DraftID: string;
}
