/**
 * The message half: what arrived, what is waiting to leave, and how the two are
 * paced.
 *
 * The functions here take the service's dependencies as a value rather than
 * living on the class, because the Go package splits them across two files and
 * this module keeps the same split: `service.ts` owns the number's life,
 * `messages.ts` owns the queue.
 *
 * Ported 1:1 from internal/modules/whatsapp/service/messages.go.
 */

import type { Logger } from "../../../platform/logger.ts";
import type {
  Alerter,
  Inbound,
  Media,
  MessageFilter,
  Number,
  Outbox,
  OutboxFilter,
  Replies,
  Scope,
  Store,
  Watchers,
} from "../domain/whatsapp.ts";
import {
  AlertLimit,
  ErrInvalidInput,
  ErrNotConnected,
  ErrNoAgent,
  KindDocument,
  KindImage,
  KindOther,
  KindText,
  MediaFailed,
  MediaPending,
  MediaReady,
  NilUUID,
  OutboxQueued,
  numberLinked,
  scopeIsZero,
  wrap,
} from "../domain/whatsapp.ts";
import type { Claimed } from "../domain/engine.ts";
import { EventMessageReceived, type EnqueueRequest } from "../domain/service.ts";
import { maskPhone, normalizePhone } from "./service.ts";

/** How long a message waits when the hourly ceiling stopped it. */
const holdDuration = 5 * 60 * 1000;

/** What the message half reads from the service. */
export interface MessageDeps {
  repository: Store;
  watchers: Watchers | null;
  replies: Replies | null;
  logger: Logger | null;
  alerter: Alerter | null;
  /** The deployment's default ceiling. */
  hourlyLimit: number;
  now: () => Date;
  /** Publishes one event into the workspace's live stream. */
  emit: (scope: Scope, kind: string, number: Number) => Promise<void>;
}

/**
 * Records one inbound message and answers whether it was new.
 *
 * The order is what makes the path idempotent rather than merely careful: the
 * message is recorded first, and only a message this call created opens
 * anything. A redelivery — which WhatsApp does on purpose when an
 * acknowledgement is slow — finds its own row and stops there.
 */
export async function receive(
  deps: MessageDeps,
  scope: Scope,
  numberID: string,
  message: Inbound,
): Promise<{ message: Inbound; created: boolean }> {
  if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");
  if (message.waMessageID.trim() === "") {
    throw wrap(ErrInvalidInput, "a message needs whatsapp's own id");
  }

  const incoming: Inbound = {
    ...message,
    receivedAt: message.receivedAt.getTime() === 0 ? deps.now() : message.receivedAt,
    numberID,
    kind: message.kind.trim() === "" ? KindOther : message.kind,
  };

  const number = await deps.repository.get(scope, numberID);
  const stored = await deps.repository.record(scope, incoming);
  if (!stored.created) return stored;

  await deps.emit(scope, "whatsapp.message_received", number);

  // The event is announced before anything is opened, so a routine watching it
  // gets the message whether or not a Bolu is assigned to the number.
  let started = 0;
  if (deps.watchers) {
    try {
      started = await deps.watchers.fire(scope, EventMessageReceived);
    } catch (err) {
      // A routine that could not start is reported and not fatal: the message is
      // recorded either way, and the fallback below still gives it a reader.
      deps.logger?.warn({ err }, "could not start the routines watching whatsapp messages");
    }
  }
  if (started > 0) return stored;

  // Nothing is watching, so the Bolu assigned to this number answers. A group is
  // not a customer writing to the business, and a message this number sent is
  // not one to answer.
  const message2 = stored.message;
  if (message2.isFromMe || message2.isGroup) return stored;

  if (!number.agentID || number.agentID === NilUUID) {
    throw wrap(ErrNoAgent, "nomor ini belum punya Bolu yang menjawab");
  }
  if (!deps.replies) return stored;

  let taskID: string;
  try {
    taskID = await openTask(deps, scope, number, message2);
  } catch (err) {
    // A task that could not be opened is reported rather than hidden, and the
    // message keeps its place.
    deps.logger?.warn(
      { message: message2.waMessageID, err },
      "could not open a task for an inbound message",
    );
    throw err;
  }

  try {
    await deps.repository.attachTask(scope, message2.id, taskID);
  } catch (err) {
    deps.logger?.warn({ err }, "could not link a message to its task");
    throw err;
  }

  return { message: { ...message2, taskID }, created: true };
}

/** Hands one customer message to the Bolu that answers the number. */
async function openTask(
  deps: MessageDeps,
  scope: Scope,
  number: Number,
  message: Inbound,
): Promise<string> {
  let title = `Pesan WhatsApp dari ${maskPhone(number.phone)}`;
  if (message.senderName !== "") {
    title = `Pesan WhatsApp dari ${message.senderName}`;
  }

  return (deps.replies as Replies).answer(scope, {
    agentID: number.agentID,
    numberID: number.id,
    phone: number.phone,
    name: message.senderName,
    title,
    prompt: promptFor(number, message),
  });
}

/**
 * What the Bolu is told.
 *
 * The customer's own words are the instruction, and the number it came from is
 * the reply address.
 */
export function promptFor(number: Number, message: Inbound): string {
  const out: string[] = [];

  out.push(`Seorang pelanggan menulis ke nomor WhatsApp ${maskPhone(number.phone)}.\n\n`);

  if (message.senderName !== "") {
    out.push(`Nama yang dipakai pengirim: ${message.senderName}\n`);
  }
  out.push(`Nomor pengirim: +${message.chatJID.replace(/^\+/, "")}\n\n`);

  if (message.kind === KindText) {
    out.push(`Isi pesannya:\n\n${message.text}`);
  } else if (message.kind === KindOther) {
    out.push("Pesan ini bukan teks dan tidak bisa dibaca di sini.\n");
  } else {
    out.push(`Pelanggan mengirim sebuah ${message.kind}`);
    if (message.text !== "") {
      out.push(` dengan keterangan:\n\n${message.text}`);
    } else {
      out.push(".\n");
    }

    if (message.media.status === MediaReady) {
      out.push("\nLampirannya sudah tersimpan dan bisa dibaca lewat alat yang tersedia.");
    } else if (message.media.status === MediaFailed) {
      out.push(`\nLampirannya gagal diunduh: ${message.media.error}`);
    } else if (message.media.status === MediaPending) {
      out.push("\nLampirannya sedang diunduh.");
    }
  }

  out.push(
    "\n\nBalas pelanggan ini dengan alat `whatsapp.send_message`, ke nomor pengirim di atas. " +
      "Balasan itu keluar lewat gerbang persetujuan: tulis drafnya, jangan mengirim sendiri.",
  );

  return out.join("");
}

/** Returns what customers wrote, newest first. */
export async function messages(
  deps: MessageDeps,
  scope: Scope,
  filter: MessageFilter,
): Promise<Inbound[]> {
  if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");
  return deps.repository.messages(scope, filter);
}

/** Returns what the workspace asked one number to send, newest first. */
export async function outbox(
  deps: MessageDeps,
  scope: Scope,
  filter: OutboxFilter,
): Promise<Outbox[]> {
  if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");
  return deps.repository.outbox(scope, filter);
}

/** Answers which number a connection speaks as. */
export async function numberByIntegration(
  deps: MessageDeps,
  scope: Scope,
  integrationID: string,
): Promise<string> {
  if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");
  if (!integrationID || integrationID === NilUUID) {
    throw wrap(ErrInvalidInput, "no connection was named");
  }

  const number = await deps.repository.byIntegration(scope, integrationID);
  return number.id;
}

/**
 * Returns one conversation, oldest first.
 *
 * The repository reads newest first because that is what a screen wants; a
 * conversation handed to a model reads the other way round, so it is reversed
 * here rather than in the query.
 */
export async function history(
  deps: MessageDeps,
  scope: Scope,
  integrationID: string,
  chatJID: string,
  limit: number,
): Promise<Inbound[]> {
  const numberID = await numberByIntegration(deps, scope, integrationID);

  if (limit <= 0 || limit > 100) {
    limit = 20;
  }

  let chat = chatJID.trim();
  if (chat !== "") {
    chat = normalizeJID(chat);
  }

  const newest = await deps.repository.messages(scope, { numberID, chatJID: chat, limit });
  return newest.slice().reverse();
}

/**
 * Records one message to send.
 *
 * It is what the integration adapter calls when an approved action reaches
 * WhatsApp, so nothing in this module decides that a message should go out.
 */
export async function enqueue(
  deps: MessageDeps,
  scope: Scope,
  request: EnqueueRequest,
): Promise<Outbox> {
  if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");

  const number = await deps.repository.get(scope, request.numberID);
  if (!numberLinked(number)) {
    throw wrap(ErrNotConnected, `nomor ${maskPhone(number.phone)} belum tertaut`);
  }

  const to = normalizeJID(request.toJID);
  if (to === "") {
    throw wrap(ErrInvalidInput, "pesan tidak menyebut nomor tujuan");
  }

  const kind = request.kind === "" ? KindText : request.kind;
  if (!knownOutgoingKind(kind)) {
    throw wrap(ErrInvalidInput, `jenis pesan ${JSON.stringify(kind)} tidak dikenal`);
  }
  if (request.text.trim() === "" && request.mediaKey.trim() === "") {
    throw wrap(ErrInvalidInput, "pesan kosong");
  }

  const stored = await deps.repository.enqueue(scope, {
    id: NilUUID,
    workspaceID: scope.workspaceId,
    numberID: number.id,
    toJID: to,
    kind,
    text: request.text,
    mediaKey: request.mediaKey,
    draftID: request.draftID,
    status: OutboxQueued,
    attempts: 0,
    lastError: "",
    // Nothing leaves immediately: the gap before the first message of a batch
    // is what keeps a burst from leaving in one second.
    notBefore: new Date(deps.now().getTime() + firstGap()),
    claimedAt: null,
    waMessageID: "",
    sentAt: null,
    createdAt: new Date(0),
  });

  deps.logger?.info({ number: number.id, message: stored.id }, "a whatsapp message is queued");
  return stored;
}

/**
 * Returns the messages of one number that may go out now.
 *
 * The hourly ceiling is checked here rather than by the sender, and what it does
 * when it is reached is the point: the due messages are pushed to later rather
 * than parked as failed, because a limit is a pace and a customer's answer is
 * still owed.
 */
export async function claim(
  deps: MessageDeps,
  scope: Scope,
  numberID: string,
  limit: number,
): Promise<Claimed> {
  if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");
  if (limit <= 0) limit = 5;

  const number = await deps.repository.get(scope, numberID);

  const ceiling = number.hourlyLimit > 0 ? number.hourlyLimit : deps.hourlyLimit;
  const sent = await deps.repository.sentSince(scope, numberID, new Date(deps.now().getTime() - 60 * 60 * 1000));

  const remaining = ceiling - sent;
  if (remaining <= 0) {
    // Everything due waits. It is reported so a person can be told their message
    // is paced rather than lost, and the alert fires once per hold.
    const held = await holdDue(deps, scope, numberID, limit);
    if (held > 0) await alertLimit(deps, scope, number, ceiling, held);
    return { ready: [], held };
  }

  if (limit > remaining) limit = remaining;

  const ready = await deps.repository.due(scope, numberID, limit);
  return { ready, held: 0 };
}

/** Pushes one number's due messages to later and answers how many. */
async function holdDue(
  deps: MessageDeps,
  scope: Scope,
  numberID: string,
  limit: number,
): Promise<number> {
  const due = await deps.repository.due(scope, numberID, limit);

  let held = 0;
  for (const item of due) {
    await deps.repository.markHeld(
      scope,
      item.id,
      "batas kirim per jam tercapai",
      new Date(deps.now().getTime() + holdDuration),
    );
    held++;
  }
  return held;
}

/** Tells the workspace that a number is paced. */
async function alertLimit(
  deps: MessageDeps,
  scope: Scope,
  number: Number,
  ceiling: number,
  held: number,
): Promise<void> {
  deps.logger?.info({ number: number.id, ceiling, held }, "a whatsapp number reached its hourly ceiling");

  await deps.emit(scope, "whatsapp.limit_reached", number);

  if (!deps.alerter) return;

  try {
    await deps.alerter.alert(scope, {
      kind: AlertLimit,
      numberID: number.id,
      phone: number.phone,
      status: number.status,
      reason: `batas ${ceiling} pesan per jam tercapai; ${held} pesan menunggu`,
      workspaceID: scope.workspaceId,
    });
  } catch (err) {
    deps.logger?.warn({ err }, "could not report a paced number");
  }
}

/** Records that a message left. */
export async function sent(
  deps: MessageDeps,
  scope: Scope,
  id: string,
  waMessageID: string,
  at: Date,
): Promise<void> {
  if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");
  await deps.repository.markSent(scope, id, waMessageID, at);
}

/** Puts a message back in the queue with a later time. */
export async function retry(
  deps: MessageDeps,
  scope: Scope,
  id: string,
  reason: string,
  at: Date,
): Promise<void> {
  if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");
  await deps.repository.markRetry(scope, id, reason, at);
}

/** Parks a message that will not be retried, so it stays visible. */
export async function failed(
  deps: MessageDeps,
  scope: Scope,
  id: string,
  reason: string,
  attempts: number,
): Promise<void> {
  if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");

  await deps.repository.markFailed(scope, id, reason, attempts);
  deps.logger?.warn({ message: id, reason }, "a whatsapp message was not delivered");
}

/** Puts messages a dead gateway left mid-send back in the queue. */
export async function reclaim(deps: MessageDeps, scope: Scope, olderThan: Date): Promise<number> {
  if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");
  return deps.repository.reclaim(scope, olderThan);
}

/** Returns the messages whose bytes have not been fetched. */
export async function pendingMedia(
  deps: MessageDeps,
  scope: Scope,
  limit: number,
): Promise<Inbound[]> {
  if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");
  return deps.repository.pendingMedia(scope, limit);
}

/** Records where a message's bytes were stored, or that they could not be. */
export async function storeMedia(
  deps: MessageDeps,
  scope: Scope,
  id: string,
  media: Media,
): Promise<void> {
  if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");
  if (!knownMediaStatus(media.status)) {
    throw wrap(ErrInvalidInput, `unknown media status ${JSON.stringify(media.status)}`);
  }

  await deps.repository.setMedia(scope, id, media);
}

/**
 * The pause before the first message of a batch.
 *
 * It is short and randomised for the same reason the gap between two messages
 * is: a batch that leaves in the same instant it was approved reads as a machine
 * posting.
 */
function firstGap(): number {
  return 2000 + randomGap(3000);
}

/** Draws a pause below a ceiling, so the wait is not the same every time. */
function randomGap(ceiling: number): number {
  if (ceiling <= 0) return 0;
  return Math.floor(Math.random() * ceiling);
}

/** Reports whether this deployment can send that kind. */
function knownOutgoingKind(kind: string): boolean {
  return kind === KindText || kind === KindImage || kind === KindDocument;
}

/** Reports whether a media status is one this module uses. */
function knownMediaStatus(status: string): boolean {
  return status === MediaPending || status === MediaReady || status === MediaFailed || status === "";
}

/**
 * Turns a phone number or an address into a WhatsApp address.
 *
 * A person writing a reply says "+62812…", and WhatsApp addresses are
 * "62812…@s.whatsapp.net".
 */
export function normalizeJID(value: string): string {
  const trimmed = value.trim();
  if (trimmed === "") return "";
  if (trimmed.includes("@")) return trimmed;

  const digits = normalizePhone(trimmed);
  if (digits === "") return "";
  return `${digits}@s.whatsapp.net`;
}
