/**
 * The WhatsApp gateway contract: the number that stays online, the messages it
 * carries, the queue that paces them, and the one process that owns the
 * connection.
 *
 * Three rules run through it:
 *
 *   - one number has exactly one owner. A session that two processes drive
 *     sends every message twice and gets the number banned, so ownership is a
 *     lease rather than a convention.
 *   - the session material is sealed with the workspace's own key and never
 *     read outside the scope of the workspace that owns it.
 *   - nothing leaves without a person's approval. This module carries an
 *     approved action out; it never decides that an action should happen.
 *
 * Ported 1:1 from packages/whatsapp/domain/whatsapp.go.
 */

/** The zero uuid, as Go's `uuid.Nil`. */
export const NilUUID = "00000000-0000-0000-0000-000000000000";

/** No such number, link attempt, or message in this workspace. */
export const ErrNotFound = new Error("whatsapp: not found");
/** The request cannot be acted on as written. */
export const ErrInvalidInput = new Error("whatsapp: invalid input");
/** No transport is wired, which is a deployment without a gateway. */
export const ErrNotConfigured = new Error("whatsapp: the gateway is not configured");
/** The runtime is there and WhatsApp could not be reached. */
export const ErrUnavailable = new Error("whatsapp: WhatsApp is not reachable");
/** The terms-of-service warning was not accepted, so a link cannot start. */
export const ErrTermsNotAccepted = new Error("whatsapp: the terms of service warning was not accepted");
/** The QR ran out of time. The answer is a new code. */
export const ErrLinkExpired = new Error("whatsapp: the link code expired");
/** This number is linked to another workspace. */
export const ErrAlreadyLinked = new Error("whatsapp: this number is already linked");
/** The number is not online, so nothing can be sent as it. */
export const ErrNotConnected = new Error("whatsapp: the number is not connected");
/** The number spent its allowance for this hour. It is a hold, not a refusal. */
export const ErrHourlyLimit = new Error("whatsapp: the hourly send limit is reached");
/** The record exists but has no Bolu assigned. */
export const ErrNoAgent = new Error("whatsapp: no bolu is assigned to this number");

/** An error carrying the sentinel it was wrapped from, the way Go's `%w` does. */
export class WrappedError extends Error {
  readonly sentinel: Error;

  constructor(sentinel: Error, message: string) {
    super(message);
    this.name = "WrappedError";
    this.sentinel = sentinel;
  }
}

/** Wraps `sentinel` with a detail, matching `fmt.Errorf("%w: %s", ...)`. */
export function wrap(sentinel: Error, detail: string): WrappedError {
  return new WrappedError(sentinel, `${sentinel.message}: ${detail}`);
}

/** Reports whether `err` is `sentinel` or wraps it, matching `errors.Is`. */
export function is(err: unknown, sentinel: Error): boolean {
  let current: unknown = err;
  for (let guard = 0; guard < 32; guard++) {
    if (current === sentinel) return true;
    if (!(current instanceof Error)) return false;
    if (current instanceof WrappedError) {
      current = current.sentinel;
      continue;
    }
    current = "cause" in current ? current.cause : undefined;
  }
  return false;
}

/** Renders an error for a person: the detail after the sentinel's own text. */
export function userMessage(err: unknown, sentinel: Error): string {
  const raw = err instanceof Error ? err.message : String(err);
  const detail = raw.startsWith(sentinel.message) ? raw.slice(sentinel.message.length) : raw;
  return detail.trim().replace(/^:/, "").trim();
}

/** The tenant a call runs in. */
export interface Scope {
  userId?: string | null;
  workspaceId: string;
}

/** Reports whether the scope carries no tenant at all. */
export function scopeIsZero(scope: Scope): boolean {
  return !scope.workspaceId || scope.workspaceId === NilUUID;
}

/** A number row that has no session yet. */
export const StatusUnlinked = "unlinked";
/** A link attempt in progress. */
export const StatusLinking = "linking";
/** A number with a live session. */
export const StatusConnected = "connected";
/** A session WhatsApp refused; a person fixes it by scanning again. */
export const StatusNeedsRelink = "needs_relink";
/** A session that stopped without being refused. */
export const StatusDisconnected = "disconnected";

/** One linked WhatsApp number. */
export interface Number {
  id: string;
  workspaceID: string;
  integrationID: string;
  /** E.164, as WhatsApp reports it. */
  phone: string;
  label: string;
  /** The Bolu that answers this number. Zero means nobody was chosen. */
  agentID: string;
  status: string;
  statusReason: string;
  /** When the workspace accepted the unsupported-route warning. Nil blocks a link. */
  tosAcceptedAt: Date | null;
  /** How many messages this number may send in an hour. */
  hourlyLimit: number;
  connectedAt: Date | null;
  lastSeenAt: Date | null;
  createdAt: Date;
  updatedAt: Date;
}

/** Reports whether the number currently has a live session. */
export function numberConnected(number: Number): boolean {
  return number.status === StatusConnected;
}

/** Reports whether a session exists, whether or not it is online now. */
export function numberLinked(number: Number): boolean {
  return (
    number.status === StatusConnected ||
    number.status === StatusDisconnected ||
    number.status === StatusNeedsRelink
  );
}

/**
 * The sealed device state of one number. It is opaque here on purpose: only the
 * transport knows what is inside.
 */
export interface Session {
  numberID: string;
  workspaceID: string;
  sealed: Uint8Array;
  /** The account the device belongs to, in the clear. */
  jid: string;
  updatedAt: Date;
}

/** An attempt that has not produced a code yet. */
export const LinkWaiting = "waiting";
/** An attempt showing a code to scan. */
export const LinkCode = "code";
/** An attempt that succeeded. */
export const LinkPaired = "paired";
/** An attempt whose code ran out. */
export const LinkExpired = "expired";
/** An attempt the phone or the service refused. */
export const LinkFailed = "failed";

/** One attempt to link a number. */
export interface Link {
  /** The transport's handle for this attempt. */
  id: string;
  numberID: string;
  workspaceID: string;
  state: string;
  /** The QR payload, present while `state` is `LinkCode`. */
  code: string;
  /** The alternative for a phone that cannot scan. */
  pairCode: string;
  /** The number being linked, once the transport knows it. */
  phone: string;
  expiresAt: Date;
  /** Explains a failed or expired attempt in the user's words. */
  reason: string;
}

/** Plain text. */
export const KindText = "text";
/** An image attachment. */
export const KindImage = "image";
/** A video attachment. */
export const KindVideo = "video";
/** An audio attachment. */
export const KindAudio = "audio";
/** A document attachment. */
export const KindDocument = "document";
/** A sticker. */
export const KindSticker = "sticker";
/** Anything else. */
export const KindOther = "other";

/** Bytes not fetched yet. */
export const MediaPending = "pending";
/** Bytes are in object storage. */
export const MediaReady = "ready";
/** Bytes could not be fetched. */
export const MediaFailed = "failed";

/** What has to be fetched after a message is recorded. */
export interface Media {
  /** The transport's own handle on the bytes. */
  ref: string;
  kind: string;
  size: number;
  status: string;
  /** Where the bytes were stored, once they were. */
  key: string;
  error: string;
}

/** One message that arrived. */
export interface Inbound {
  id: string;
  workspaceID: string;
  numberID: string;
  /** WhatsApp's own id. It is the dedupe key. */
  waMessageID: string;
  chatJID: string;
  senderJID: string;
  senderName: string;
  isGroup: boolean;
  isFromMe: boolean;
  kind: string;
  text: string;
  media: Media;
  receivedAt: Date;
  /** The task the message opened, once one was opened. */
  taskID: string;
  createdAt: Date;
}

/** A message waiting for its turn. */
export const OutboxQueued = "queued";
/** A message claimed by the gateway and in flight. */
export const OutboxSending = "sending";
/** A message WhatsApp accepted. */
export const OutboxSent = "sent";
/** A message that will not be retried. */
export const OutboxFailed = "failed";
/** A message waiting on a limit. It is a pace, not a refusal. */
export const OutboxHeld = "held";

/** One message waiting to leave. */
export interface Outbox {
  id: string;
  workspaceID: string;
  numberID: string;
  toJID: string;
  kind: string;
  text: string;
  mediaKey: string;
  /** The draft this message came from, when it came from one. */
  draftID: string;
  status: string;
  attempts: number;
  lastError: string;
  notBefore: Date;
  claimedAt: Date | null;
  /** WhatsApp's id for the sent message, once it left. */
  waMessageID: string;
  sentAt: Date | null;
  createdAt: Date;
}

/** One message the transport is asked to deliver. */
export interface ToSend {
  id: string;
  toJID: string;
  kind: string;
  text: string;
  mediaKey: string;
}

/**
 * Everything the transport needs to speak as one number. It is a value rather
 * than an id so the transport never reads the database.
 */
export interface Snapshot {
  numberID: string;
  workspaceID: string;
  phone: string;
  /** Nil for a number that was never linked. */
  session: Session | null;
}

/** One directory row: a number the fleet is expected to serve. */
export interface Assignment {
  numberID: string;
  workspaceID: string;
}

/** The WhatsApp side: what can be done with a number. */
export interface Transport {
  /** Starts an attempt for one number and returns its handle. */
  link(scope: Scope, numberID: string, phone: string): Promise<Link>;
  /** Reports where an attempt stands. */
  linkState(id: string): Promise<Link>;
  /** Asks for the phone-code alternative to a QR. */
  pairCode(id: string, phone: string): Promise<Link>;
  /** Abandons an attempt and discards its session. */
  cancel(id: string): Promise<void>;
  /** Delivers one message and answers WhatsApp's id for it. */
  send(snapshot: Snapshot, message: ToSend): Promise<string>;
  /** Shows the typing indicator in one chat. */
  typing(snapshot: Snapshot, toJID: string): Promise<void>;
  /** Ends the session at WhatsApp and forgets it locally. */
  unlink(snapshot: Snapshot): Promise<void>;
  /** Reports whether the transport currently holds this number. */
  online(numberID: string): Promise<boolean>;
}

/** The half of the session store the product keeps. */
export interface SessionForgetter {
  forgetSession(scope: Scope, numberID: string): Promise<void>;
}

/** Persists the sealed device state of one number. */
export interface SessionStore extends SessionForgetter {
  /** Returns the stored session, or throws ErrNotFound when never linked. */
  load(scope: Scope, numberID: string): Promise<Session>;
  save(scope: Scope, session: Session): Promise<void>;
}

/** Seals and opens a device blob with one workspace's key. */
export interface Sealer {
  seal(workspaceID: string, plaintext: Uint8Array): Promise<Uint8Array>;
  open(workspaceID: string, ciphertext: Uint8Array): Promise<Uint8Array>;
}

/** Lists the numbers a gateway instance is expected to serve. */
export interface Directory {
  serving(): Promise<Assignment[]>;
}

/** Decides which gateway instance owns one number. */
export interface Lease {
  /** Takes the lease when free or already ours. */
  acquire(numberID: string, owner: string, ttlMs: number): Promise<boolean>;
  /** Extends a lease we hold. */
  renew(numberID: string, owner: string, ttlMs: number): Promise<boolean>;
  /** Gives it up. */
  release(numberID: string, owner: string): Promise<void>;
}

/** One entry in the workspace's live stream. */
export interface Event {
  workspaceID: string;
  type: string;
  payload: unknown;
}

/** Records what happened to a number in the workspace's stream. */
export interface Activity {
  emit(event: Event): Promise<void>;
}

/** Something a workspace's owners have to act on. */
export interface Alert {
  /** One of the Alert* constants. */
  kind: string;
  numberID: string;
  phone: string;
  status: string;
  reason: string;
  workspaceID: string;
}

/** A number WhatsApp refused. Waiting does not fix it. */
export const AlertNeedsRelink = "whatsapp.needs_relink";
/** A number that went offline. */
export const AlertDisconnected = "whatsapp.disconnected";
/** A number that reached its hourly ceiling. */
export const AlertLimit = "whatsapp.limit_reached";

/** Tells the workspace's owners that a number needs them. */
export interface Alerter {
  alert(scope: Scope, alert: Alert): Promise<void>;
}

/** Opens the task an inbound message asks for. */
export interface Replies {
  /** Opens a task for the Bolu that answers one number. */
  answer(scope: Scope, request: AnswerRequest): Promise<string>;
}

/** One inbound message that needs an answer. */
export interface AnswerRequest {
  agentID: string;
  numberID: string;
  /** The customer's number, as E.164. */
  phone: string;
  /** What WhatsApp calls the sender, when it says. */
  name: string;
  title: string;
  /** What the Bolu is told, with the customer's own words in it. */
  prompt: string;
}

/** Persists numbers, sessions, messages, the queue, and the directory. */
export interface Store {
  // Numbers
  create(scope: Scope, number: Number): Promise<Number>;
  get(scope: Scope, id: string): Promise<Number>;
  byPhone(scope: Scope, phone: string): Promise<Number>;
  /** Returns the number a connection speaks as. */
  byIntegration(scope: Scope, integrationID: string): Promise<Number>;
  list(scope: Scope): Promise<Number[]>;
  update(scope: Scope, number: Number): Promise<Number>;
  setStatus(scope: Scope, id: string, status: string, reason: string): Promise<Number>;
  setIntegration(scope: Scope, id: string, integrationID: string): Promise<void>;
  setPhone(scope: Scope, id: string, phone: string): Promise<void>;
  acceptTerms(scope: Scope, id: string): Promise<Number>;
  touch(scope: Scope, id: string, at: Date): Promise<void>;
  delete(scope: Scope, id: string): Promise<void>;

  // Directory
  publish(scope: Scope, numberID: string, enabled: boolean): Promise<void>;

  // Messages
  /** Stores an inbound message and answers whether it was new. */
  record(scope: Scope, message: Inbound): Promise<{ message: Inbound; created: boolean }>;
  messages(scope: Scope, filter: MessageFilter): Promise<Inbound[]>;
  byWaID(scope: Scope, waMessageID: string): Promise<Inbound>;
  messageByID(scope: Scope, id: string): Promise<Inbound>;
  pendingMedia(scope: Scope, limit: number): Promise<Inbound[]>;
  setMedia(scope: Scope, id: string, media: Media): Promise<void>;
  attachTask(scope: Scope, id: string, taskID: string): Promise<void>;

  // Outbox
  enqueue(scope: Scope, item: Outbox): Promise<Outbox>;
  /** Returns the messages ready to send, claiming them in the same step. */
  due(scope: Scope, numberID: string, limit: number): Promise<Outbox[]>;
  markSent(scope: Scope, id: string, waMessageID: string, at: Date): Promise<void>;
  markHeld(scope: Scope, id: string, reason: string, at: Date): Promise<void>;
  markRetry(scope: Scope, id: string, reason: string, at: Date): Promise<void>;
  markFailed(scope: Scope, id: string, reason: string, attempts: number): Promise<void>;
  /** Counts what left since a moment, which is how the ceiling is measured. */
  sentSince(scope: Scope, numberID: string, since: Date): Promise<number>;
  outbox(scope: Scope, filter: OutboxFilter): Promise<Outbox[]>;
  /** Returns messages stuck in `sending` to the queue. */
  reclaim(scope: Scope, olderThan: Date): Promise<number>;
}

/** Narrows an inbound read. */
export interface MessageFilter {
  numberID: string;
  chatJID: string;
  limit: number;
}

/** Narrows an outbox read. */
export interface OutboxFilter {
  numberID: string;
  status: string;
  limit: number;
}
