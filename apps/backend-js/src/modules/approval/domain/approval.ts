/**
 * The approval contract: the draft a Bolu proposes, the policy that decides
 * whether a human is asked at all, and the trail of who decided what.
 *
 * The rule the whole package exists for is in `needsDraft`: an action that
 * reaches outside Bolu waits for a person unless the workspace has said, in
 * writing and for that specific kind of action, that it need not.
 *
 * Ported 1:1 from internal/modules/approval/domain/approval.go.
 */

/** The zero uuid, as Go's `uuid.Nil`. */
export const NilUUID = "00000000-0000-0000-0000-000000000000";

// Policy modes, matching the check constraint on `approval_policies.mode`.
/** Asks a person every time. It is the default, and what an unset kind behaves as. */
export const ModeAlwaysDraft = "always_draft";
/** Carries the action out unattended, but only while the value is within the ceiling. */
export const ModeAuto = "auto";

// Draft statuses, matching the check constraint on `drafts.status`.
/** A draft nobody has decided on. */
export const StatusPending = "pending";
/** A draft a person allowed, which may still be held until the workspace opens. */
export const StatusApproved = "approved";
/** A draft a person sent back with a note. */
export const StatusRevise = "revise";
/** An action that actually left. */
export const StatusSent = "sent";
/** A draft nobody will act on. */
export const StatusCanceled = "canceled";

/** The page a client gets when it does not ask for one. */
export const DefaultListLimit = 50;
/** Bounds one page. */
export const MaxListLimit = 200;

// Errors the service and the handlers map onto responses.
export const ErrDraftNotFound = new Error("approval: draft not found");
export const ErrPolicyNotFound = new Error("approval: policy not found");
export const ErrInvalidInput = new Error("approval: invalid input");
/** A caller asked for a mode the schema does not have. */
export const ErrUnknownMode = new Error("approval: unknown policy mode");
/** A decision arrived for a draft that has one. */
export const ErrAlreadyDecided = new Error("approval: the draft has already been decided");
/** The runtime tried to send a draft nobody approved. */
export const ErrNotApproved = new Error("approval: the draft is not approved");
/** Nothing can carry the action out, which is a configuration fact rather than a retry. */
export const ErrNoSender = new Error("approval: no sender is configured");

/**
 * An error carrying the sentinel a caller switches on, so `is` reproduces Go's
 * `errors.Is` for the messages `fmt.Errorf("%w: ...")` produced.
 */
export class WrappedError extends Error {
  constructor(
    readonly sentinel: Error,
    message: string,
    options?: ErrorOptions,
  ) {
    super(message, options);
    this.name = "WrappedError";
  }
}

/** Wraps a sentinel with a detail, rendering the message as Go's `%w: detail`. */
export function wrap(sentinel: Error, detail: string, options?: ErrorOptions): WrappedError {
  return new WrappedError(sentinel, `${sentinel.message}: ${detail}`, options);
}

/** True when `err` is the sentinel or wraps it. */
export function is(err: unknown, sentinel: Error): boolean {
  let current: unknown = err;
  for (let guard = 0; guard < 32; guard++) {
    if (current === sentinel) {
      return true;
    }
    if (!(current instanceof Error)) {
      return false;
    }
    if (current instanceof WrappedError) {
      current = current.sentinel;
      continue;
    }
    current = "cause" in current ? current.cause : undefined;
  }
  return false;
}

/** The message of an unknown thrown value. */
export function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/** True for the zero uuid, or the empty string the wire uses for one. */
export function isNil(id: string | null | undefined): boolean {
  return !id || id === NilUUID;
}

/** The tenant identity a query runs as. */
export interface Scope {
  userId?: string;
  workspaceId: string;
}

/** Reports whether the scope carries no workspace, which every approval read requires. */
export function isZeroScope(scope: Scope): boolean {
  return isNil(scope.workspaceId);
}

/** Decides whether one kind of action needs a person. */
export interface Policy {
  actionKind: string;
  mode: string;
  /** Caps what an auto policy may carry out unattended, in rupiah. Null means no ceiling. */
  maxAmount: number | null;
  updatedBy: string;
  createdAt: Date;
  updatedAt: Date;
}

/** What an action kind with no stored policy behaves as. */
export function defaultPolicy(actionKind: string): Policy {
  return {
    actionKind,
    mode: ModeAlwaysDraft,
    maxAmount: null,
    updatedBy: "",
    createdAt: new Date(0),
    updatedAt: new Date(0),
  };
}

/**
 * Reports whether an action must wait for a person.
 *
 * The answer is yes unless the workspace opted out for this action kind, and
 * even then only while the value is known and within the ceiling. An action
 * whose amount cannot be read is treated as unbounded: a policy that names a
 * ceiling cannot be satisfied by a payload that never states one.
 */
export function needsDraft(policy: Policy, amount: number | null): boolean {
  if (policy.mode !== ModeAuto) {
    return true;
  }
  if (policy.maxAmount === null) {
    // An auto policy with no ceiling means the workspace trusts this action
    // kind entirely, which is a choice it is allowed to make.
    return false;
  }
  if (amount === null) {
    return true;
  }
  return amount > policy.maxAmount;
}

/** A draft to store. */
export interface NewDraft {
  taskId: string;
  agentId: string;
  conversationId: string;
  actionKind: string;
  title: string;
  summary: string;
  payload: unknown;
  /** Identifies the proposal, so a retried activity cannot open the same draft twice. */
  idempotencyKey: string;
}

/** One proposed action and what became of it. */
export interface Draft {
  id: string;
  workspaceId: string;
  taskId: string;
  agentId: string;
  conversationId: string;
  actionKind: string;
  title: string;
  summary: string;
  payload: unknown;
  status: string;
  /** How many times this draft was sent back. */
  revision: number;
  decidedBy: string;
  decidedAt: Date | null;
  decisionNote: string;
  /** The earliest moment the action may leave, set when decided outside working hours. */
  notBefore: Date | null;
  /** Why it has not gone out, in the user's words. */
  blockedReason: string;
  sentAt: Date | null;
  /** The message in the thread carrying the card. */
  cardMessageId: string;
  idempotencyKey: string;
  createdAt: Date;
  updatedAt: Date;
}

/** Reports whether the draft still needs a person. */
export function waiting(draft: Draft): boolean {
  return draft.status === StatusPending;
}

/** Reports whether an approved action is waiting for the working day. */
export function held(draft: Draft, now: Date): boolean {
  return draft.status === StatusApproved && draft.notBefore !== null && draft.notBefore.getTime() > now.getTime();
}

/** One turn of the trail: what the person was looking at, and what they said about it. */
export interface Revision {
  id: string;
  draftId: string;
  revision: number;
  note: string;
  payload: unknown;
  decidedBy: string;
  createdAt: Date;
}

/** A person's answer about a draft. */
export interface Decision {
  draftId: string;
  /** approved, revise, or canceled. */
  status: string;
  /** What the person said, which the agent reads when the draft was sent back. */
  note: string;
  /** Who answered. */
  decidedBy: string;
}

/** One thing that happened, for the record. */
export interface AuditEntry {
  /**
   * What happened, as `<object>.<verb>`: draft.created, draft.approved,
   * draft.sent, policy.updated, grant.set, member.role_changed,
   * provider.created.
   */
  action: string;
  objectType: string;
  objectId: string;
  actorUserId: string;
  actorAgentId: string;
  /** The detail: a payload summary, never a secret. */
  metadata: Record<string, unknown>;
}

/** Narrows a draft list. */
export interface Filter {
  /** One status; empty means every status. */
  status: string;
  /** One Bolu. */
  agentId: string;
  /** One kind of action. */
  actionKind: string;
  /** The drafts one task produced. */
  taskId: string;
  limit: number;
}

/** Returns the filter with the limit filled from the defaults. */
export function normaliseFilter(filter: Filter): Filter {
  let limit = filter.limit;
  if (limit <= 0) {
    limit = DefaultListLimit;
  } else if (limit > MaxListLimit) {
    limit = MaxListLimit;
  }
  return { ...filter, limit };
}

/** The persistence contract for approvals. */
export interface Repository {
  /** Stores a proposal. A repeated idempotency key returns the stored draft. */
  createDraft(scope: Scope, draft: NewDraft): Promise<Draft>;
  getDraft(scope: Scope, id: string): Promise<Draft>;
  listDrafts(scope: Scope, filter: Filter): Promise<Draft[]>;
  /** Records a person's answer, but only while the draft is still pending. */
  decide(scope: Scope, decision: Decision): Promise<Draft>;
  markSent(scope: Scope, id: string): Promise<Draft>;
  hold(scope: Scope, id: string, notBefore: Date, reason: string): Promise<Draft>;
  setCardMessage(scope: Scope, id: string, messageId: string): Promise<Draft>;
  appendRevision(scope: Scope, revision: Revision): Promise<Revision>;
  revisions(scope: Scope, draftId: string): Promise<Revision[]>;

  policies(scope: Scope): Promise<Policy[]>;
  /** One action kind's policy, or ErrPolicyNotFound when the workspace never set one. */
  policy(scope: Scope, actionKind: string): Promise<Policy>;
  setPolicy(scope: Scope, policy: Policy): Promise<Policy>;
  /** The action kinds that reach outside Bolu, which is what a workspace has policies for. */
  actionKinds(): Promise<string[]>;

  recordAudit(scope: Scope, entry: AuditEntry): Promise<void>;
  auditTrail(scope: Scope, filter: AuditFilter): Promise<AuditRecord[]>;
}

/** Narrows a trail read. */
export interface AuditFilter {
  /** A value ending in a dot is a prefix, so `grant.` reads every grant change. */
  action: string;
  objectType: string;
  objectId: string;
  actorAgentId: string;
  actorUserId: string;
  /** Until is exclusive, so two windows that abut do not return the same entry twice. */
  since: Date | null;
  until: Date | null;
  /** Caps one page. Zero takes the default. */
  limit: number;
}

/** Reports whether the filter asks for a bounded window. */
export function windowed(filter: AuditFilter): boolean {
  return filter.since !== null || filter.until !== null;
}

/** One stored entry. */
export interface AuditRecord {
  id: number;
  actorUserId: string;
  actorAgentId: string;
  action: string;
  objectType: string;
  objectId: string;
  metadata: Record<string, unknown>;
  createdAt: Date;
}

/** Carries out an approved action. */
export interface Sender {
  send(ctx: AbortSignal | undefined, scope: Scope, draft: Draft): Promise<unknown>;
}

/** Writes the draft card into the conversation a task came from. */
export interface Cards {
  /** Puts the card in the thread and returns the message carrying it. */
  write(ctx: AbortSignal | undefined, scope: Scope, draft: Draft): Promise<string>;
  /** Updates that message after a decision. */
  rewrite(ctx: AbortSignal | undefined, scope: Scope, messageId: string, draft: Draft): Promise<void>;
}

// Event names the approval publishes.
export const EventDraftCreated = "draft.created";
export const EventDraftDecided = "draft.decided";

/** One entry of the activity stream. */
export interface Event {
  workspaceId: string;
  type: string;
  actorAgentId: string;
  actorUserId: string;
  conversationId: string;
  taskId: string;
  draftId: string;
  payload: unknown;
}

/** Publishes what a draft is doing. */
export interface EventSink {
  emit(ctx: AbortSignal | undefined, event: Event): Promise<void>;
}

/** The window a workspace lets actions leave in. */
export interface WorkingHours {
  /** Wall-clock times in the workspace's own zone, e.g. 08:00 and 21:00. */
  start: string;
  end: string;
  /** The IANA name, e.g. Asia/Jakarta. */
  timezone: string;
}

/** The result of `open`: whether it may leave, and when it next may. */
export interface Window {
  open: boolean;
  next: Date;
  /** The resolved IANA zone, so a caller can render `next` in the workspace's own clock. */
  zone: string;
}

/**
 * Reports whether an action may leave at now, and when it may next.
 *
 * A window that closes at or before it opens is read as spanning midnight,
 * which is what a workspace that works nights means. An unparsable setting
 * leaves the window open: refusing every send because a timezone name is wrong
 * would be a worse failure than letting one through, and the mistake is visible
 * in the policy screen.
 */
export function openWindow(hours: WorkingHours, now: Date): Window {
  const zone = resolveZone(hours.timezone);
  if (zone === null) {
    return { open: true, next: now, zone: "" };
  }

  const start = parseClock(hours.start);
  const end = parseClock(hours.end);
  if (start === null || end === null) {
    return { open: true, next: now, zone };
  }

  const local = partsInZone(now, zone);
  const today = fromZoned({ ...local, hour: 0, minute: 0, second: 0 }, 0, zone);

  // Go adds a `time.Duration` to the instant, not to the wall clock, so on a
  // DST transition day these are absolute offsets from local midnight. The port
  // keeps that arithmetic: a window is what the backend actually enforced.
  const opens = new Date(today.getTime() + start * 60_000);
  const closes = new Date(today.getTime() + end * 60_000);

  if (start <= end) {
    if (now.getTime() >= opens.getTime() && now.getTime() < closes.getTime()) {
      return { open: true, next: now, zone };
    }
    if (now.getTime() < opens.getTime()) {
      return { open: false, next: opens, zone };
    }
    return { open: false, next: addDaysToInstant(opens, zone), zone };
  }

  // A window that spans midnight is open from `start` today until `end`
  // tomorrow, and it is also still open in the early hours from yesterday.
  if (now.getTime() >= opens.getTime() || now.getTime() < closes.getTime()) {
    return { open: true, next: now, zone };
  }
  return { open: false, next: opens, zone };
}

/** Reads an "HH:MM" setting into minutes since midnight. */
export function parseClock(value: string): number | null {
  // Go's `time.Parse("15:04", ...)` takes a one- or two-digit hour and exactly
  // two minute digits, and refuses an hour or minute out of range.
  const match = /^(\d{1,2}):(\d{2})$/.exec(value.trim());
  if (!match) {
    return null;
  }
  const hour = Number.parseInt(match[1], 10);
  const minute = Number.parseInt(match[2], 10);
  if (hour > 23 || minute > 59) {
    return null;
  }
  return hour * 60 + minute;
}

/** The clock the working-hours rule reads, so it is testable without waiting. */
export type Clock = () => Date;

// ------------------------------------------------------------------ time zones

interface ZonedParts {
  year: number;
  month: number;
  day: number;
  hour: number;
  minute: number;
  second: number;
}

/**
 * Resolves a Go location name to an Intl time zone. Go treats "" and "UTC" as
 * UTC and "Local" as the system zone; an unknown name is an error there and
 * null here.
 */
function resolveZone(name: string): string | null {
  const trimmed = name.trim();
  if (trimmed === "" || trimmed === "UTC") {
    return "UTC";
  }
  if (trimmed === "Local") {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  }
  try {
    new Intl.DateTimeFormat("en-US", { timeZone: trimmed });
    return trimmed;
  } catch {
    return null;
  }
}

const formatters = new Map<string, Intl.DateTimeFormat>();

/** The wall-clock parts of an instant in a zone. */
function partsInZone(at: Date, zone: string): ZonedParts {
  let formatter = formatters.get(zone);
  if (!formatter) {
    formatter = new Intl.DateTimeFormat("en-US", {
      timeZone: zone,
      hourCycle: "h23",
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
    });
    formatters.set(zone, formatter);
  }
  const parts = formatter.formatToParts(at);
  const read = (type: Intl.DateTimeFormatPartTypes): number => {
    const found = parts.find((part) => part.type === type);
    return found ? Number.parseInt(found.value, 10) : 0;
  };
  return {
    year: read("year"),
    month: read("month"),
    day: read("day"),
    hour: read("hour") % 24,
    minute: read("minute"),
    second: read("second"),
  };
}

/** The zone's offset from UTC at one instant, in milliseconds. */
function zoneOffset(at: Date, zone: string): number {
  const parts = partsInZone(at, zone);
  const asUtc = Date.UTC(parts.year, parts.month - 1, parts.day, parts.hour, parts.minute, parts.second);
  return asUtc - Math.floor(at.getTime() / 1000) * 1000;
}

/** The instant a local wall-clock time in a zone names. */
function fromZoned(parts: ZonedParts, minutes: number, zone: string): Date {
  const hour = Math.floor(minutes / 60);
  const minute = minutes % 60;
  const naive = Date.UTC(parts.year, parts.month - 1, parts.day, hour, minute, 0);
  let instant = naive - zoneOffset(new Date(naive), zone);
  // A DST boundary can make the first guess land in the other offset; one more
  // pass settles it the way Go's time.Date normalises an ambiguous wall time.
  instant = naive - zoneOffset(new Date(instant), zone);
  return new Date(instant);
}

/** Adds one calendar day to an instant, keeping its wall clock, as `AddDate(0, 0, 1)`. */
function addDaysToInstant(at: Date, zone: string): Date {
  const parts = partsInZone(at, zone);
  const date = new Date(Date.UTC(parts.year, parts.month - 1, parts.day + 1));
  return fromZoned(
    {
      year: date.getUTCFullYear(),
      month: date.getUTCMonth() + 1,
      day: date.getUTCDate(),
      hour: parts.hour,
      minute: parts.minute,
      second: parts.second,
    },
    0,
    zone,
  );
}
