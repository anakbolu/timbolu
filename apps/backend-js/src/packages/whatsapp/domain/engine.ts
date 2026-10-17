/**
 * What the transport needs from the module.
 *
 * Five small ports rather than one big `Service` on purpose: the engine that
 * owns the connection does four unrelated things with the product — it records
 * what arrived, pulls what is due, reports what left, and says how a number is
 * doing — and naming them separately keeps a driver from reaching parts of the
 * product it has no business in.
 *
 * Ported 1:1 from packages/whatsapp/domain/engine.go. Go's `context.Context` is
 * the request scope and is not carried in the TS signatures below.
 */

import type { ReportRequest } from "./service.ts";
import type { Inbound, Media, Number, Outbox, Scope } from "./whatsapp.ts";

/** Records what arrived. */
export interface Inbox {
  /** Records one inbound message and answers whether it was new. */
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

/** What the transport asks for when it is ready to send. */
export interface Drain {
  /**
   * Returns the messages of one number that may go out now, and how many were
   * held by the hourly ceiling.
   */
  claim(scope: Scope, numberID: string, limit: number): Promise<Claimed>;
  /** Records that a message left, with WhatsApp's own id for it. */
  sent(scope: Scope, id: string, waMessageID: string, at: Date): Promise<void>;
  /** Puts a message back in the queue with a later time. */
  retry(scope: Scope, id: string, reason: string, at: Date): Promise<void>;
  /** Records that a message will not be retried, so it stays visible. */
  failed(scope: Scope, id: string, reason: string, attempts: number): Promise<void>;
  /** Puts messages a dead gateway left mid-send back in the queue. */
  reclaim(scope: Scope, olderThan: Date): Promise<number>;
}

/** One number's due work. */
export interface Claimed {
  /** May be sent now, oldest first. */
  Ready: Outbox[];
  /** How many the hourly ceiling pushed to later. */
  Held: number;
}

/** What the transport knows about a number. */
export interface Status {
  /** Records a status and why. A status that needs a person to act alerts them. */
  report(scope: Scope, request: ReportRequest): Promise<Number>;
  /** Returns one number, which the transport reads for phone and ceiling. */
  number(scope: Scope, id: string): Promise<Number>;
}

/** Told when an attempt to link a number finishes. */
export interface LinkSink {
  paired(scope: Scope, paired: PairedLink): Promise<void>;
}

/** A number that finished pairing. */
export interface PairedLink {
  NumberID: string;
  /** What WhatsApp reported, in E.164 without the plus. */
  Phone: string;
  /** The account the device answers as. */
  JID: string;
}

/** Starts the routines watching an event a message announced. */
export interface Watchers {
  /** Starts the routines watching one event and reports how many started. */
  fire(scope: Scope, event: string): Promise<number>;
}

/** Records a linked number as a connection the hub shows. */
export interface Connections {
  link(scope: Scope, link: ConnectionLink): Promise<string>;
  disconnect(scope: Scope, integrationID: string): Promise<void>;
}

/** One number as a connection. */
export interface ConnectionLink {
  App: string;
  /** What a screen shows: the number, masked. */
  AccountLabel: string;
  /**
   * The credential a tool call carries. For WhatsApp it names which session
   * speaks, and the hub seals it at rest like any other credential.
   */
  Handle: string;
}

/** Keeps the bytes of a message's attachment. */
export interface MediaStore {
  /** Stores one message's bytes and answers where they went. */
  put(scope: Scope, messageID: string, media: Media, content: Uint8Array): Promise<StoredObject>;
  /** Reads them back. */
  get(key: string): Promise<Uint8Array>;
}

/** Where one attachment ended up. */
export interface StoredObject {
  Key: string;
  ContentType: string;
  Filename: string;
  Size: number;
}
