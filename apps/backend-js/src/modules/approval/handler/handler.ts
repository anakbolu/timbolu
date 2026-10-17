/**
 * Exposes the approval runtime over HTTP: the drafts waiting for a decision,
 * one draft with its revision trail, the policy screen, and the audit trail.
 *
 * A decision is the one write here, and it is deliberately not a generic status
 * update: approving, sending back, and stopping are three different acts with
 * three different consequences, so each has its own endpoint.
 *
 * Ported 1:1 from internal/modules/approval/handler/handler.go.
 */

import type { Hono } from "hono";
import {
  currentUserId,
  isUuid,
  workspaceUUID,
  type AppContext,
  type AppEnv,
} from "../../../platform/context.ts";
import type { Logger } from "../../../platform/logger.ts";
import { error, ok, okWithMeta } from "../../../platform/response.ts";
import {
  ErrAlreadyDecided,
  ErrDraftNotFound,
  ErrInvalidInput,
  ErrNoSender,
  ErrNotApproved,
  ErrUnknownMode,
  StatusApproved,
  is,
  isNil,
  type AuditFilter,
  type AuditRecord,
  type Draft,
  type Filter,
  type Policy,
  type Revision,
  type Scope,
} from "../domain/approval.ts";
import type { Service } from "../domain/service.ts";

/** Hands a decision to the durable runtime. */
export interface Signaler {
  /** Tells the task parked on a draft what a person decided. */
  signalApproval(
    ctx: AbortSignal | undefined,
    scope: Scope,
    taskId: string,
    draftId: string,
    approved: boolean,
    note: string,
  ): Promise<void>;
}

/** Serves the approval endpoints. */
export class Handler {
  private notify: Signaler | null = null;

  constructor(
    private readonly service: Service,
    private readonly log: Logger,
  ) {}

  /**
   * Installs the runtime signal, so a decision unblocks the task immediately
   * rather than at its next checkpoint.
   */
  withSignaler(signaler: Signaler): Handler {
    this.notify = signaler;
    return this;
  }

  /** The workspace's drafts, newest first. */
  list = async (c: AppContext): Promise<Response> => {
    const filter: Filter = {
      status: c.req.query("status") ?? "",
      actionKind: c.req.query("action_kind") ?? "",
      limit: queryInt(c, "limit", 0),
      agentId: "",
      taskId: "",
    };

    const agentId = parseOptionalId(c.req.query("agent_id"));
    if (agentId === null) {
      return error(c, 400, "agent_id is not a uuid", "invalid_input");
    }
    filter.agentId = agentId;

    const taskId = parseOptionalId(c.req.query("task_id"));
    if (taskId === null) {
      return error(c, 400, "task_id is not a uuid", "invalid_input");
    }
    filter.taskId = taskId;

    let drafts: Draft[];
    try {
      drafts = await this.service.drafts(undefined, requestScope(c), filter);
    } catch (err) {
      return this.fail(c, "list drafts", err);
    }

    const payload = drafts.map(wireDraft);
    return okWithMeta(c, "ok", payload, { count: payload.length });
  };

  /** One draft. */
  get = async (c: AppContext): Promise<Response> => {
    const id = parseUuid(c.req.param("draftID"));
    if (id === null) {
      return error(c, 400, "draft id is not a uuid", "invalid_input");
    }

    let draft: Draft;
    try {
      draft = await this.service.draft(undefined, requestScope(c), id);
    } catch (err) {
      return this.fail(c, "get draft", err);
    }

    return ok(c, "ok", wireDraft(draft));
  };

  /** A draft's trail, oldest first. */
  revisions = async (c: AppContext): Promise<Response> => {
    const id = parseUuid(c.req.param("draftID"));
    if (id === null) {
      return error(c, 400, "draft id is not a uuid", "invalid_input");
    }

    let revisions: Revision[];
    try {
      revisions = await this.service.revisions(undefined, requestScope(c), id);
    } catch (err) {
      return this.fail(c, "list revisions", err);
    }

    const payload = revisions.map(wireRevision);
    return okWithMeta(c, "ok", payload, { count: payload.length });
  };

  /** Allows a draft and hands the decision to the task waiting on it. */
  approve = async (c: AppContext): Promise<Response> => {
    return this.decide(c, "approve");
  };

  /** Sends a draft back to the agent, with the note it reads. */
  revise = async (c: AppContext): Promise<Response> => {
    return this.decide(c, "revise");
  };

  /** Stops a draft. */
  cancel = async (c: AppContext): Promise<Response> => {
    return this.decide(c, "cancel");
  };

  /** The effective policy for every action kind that reaches outside Bolu. */
  policies = async (c: AppContext): Promise<Response> => {
    let policies: Policy[];
    try {
      policies = await this.service.policies(undefined, requestScope(c));
    } catch (err) {
      return this.fail(c, "list policies", err);
    }

    const payload = policies.map(wirePolicy);
    return okWithMeta(c, "ok", payload, { count: payload.length });
  };

  /** Stores a workspace's decision about one action kind. */
  setPolicy = async (c: AppContext): Promise<Response> => {
    const body = await readJson<PolicyRequestWire>(c);
    if (body === null) {
      return error(c, 400, "the body is not valid JSON", "invalid_request");
    }

    // BodyParser refuses a field of the wrong type, so a `max_amount` that is
    // not a number is a bad body rather than a policy with no ceiling.
    if (
      (body.action_kind !== undefined && typeof body.action_kind !== "string") ||
      (body.mode !== undefined && typeof body.mode !== "string") ||
      (body.max_amount !== undefined && body.max_amount !== null && typeof body.max_amount !== "number") ||
      (typeof body.max_amount === "number" && !Number.isInteger(body.max_amount))
    ) {
      return error(c, 400, "the body is not valid JSON", "invalid_request");
    }

    let policy: Policy;
    try {
      policy = await this.service.setPolicy(undefined, requestScope(c), {
        actionKind: body.action_kind ?? "",
        mode: body.mode ?? "",
        maxAmount: typeof body.max_amount === "number" ? body.max_amount : null,
      });
    } catch (err) {
      return this.fail(c, "set policy", err);
    }

    return ok(c, "policy saved", wirePolicy(policy));
  };

  /**
   * The trail the query names, newest first.
   *
   * The filters are what makes a trail an answer rather than a table: "what did
   * this Bolu do", "who touched this thing", and "what happened in this window"
   * are the three questions an incident asks, and each is an axis of the same
   * read.
   */
  audit = async (c: AppContext): Promise<Response> => {
    const filter: AuditFilter = {
      action: (c.req.query("action") ?? "").trim(),
      objectType: (c.req.query("object_type") ?? "").trim(),
      objectId: (c.req.query("object_id") ?? "").trim(),
      limit: queryInt(c, "limit", 0),
      actorAgentId: "",
      actorUserId: "",
      since: null,
      until: null,
    };

    // Each id is parsed rather than passed through, so a malformed one is the
    // caller's error instead of a filter that silently narrows nothing.
    const agentId = parseOptionalId(c.req.query("agent_id"));
    if (agentId === null) {
      return error(c, 400, "agent_id is not a uuid", "invalid_input");
    }
    filter.actorAgentId = agentId;

    const actorId = parseOptionalId(c.req.query("actor_id"));
    if (actorId === null) {
      return error(c, 400, "actor_id is not a uuid", "invalid_input");
    }
    filter.actorUserId = actorId;

    const since = parseOptionalTime(c.req.query("since"));
    if (since === undefined) {
      return error(c, 400, "since is not a timestamp", "invalid_input");
    }
    filter.since = since;

    const until = parseOptionalTime(c.req.query("until"));
    if (until === undefined) {
      return error(c, 400, "until is not a timestamp", "invalid_input");
    }
    filter.until = until;

    let records: AuditRecord[];
    try {
      records = await this.service.audit(undefined, requestScope(c), filter);
    } catch (err) {
      return this.fail(c, "list audit", err);
    }

    const payload = records.map(wireAudit);
    return okWithMeta(c, "ok", payload, { count: payload.length });
  };

  /** Runs one of the three decisions. */
  private async decide(c: AppContext, kind: "approve" | "revise" | "cancel"): Promise<Response> {
    const id = parseUuid(c.req.param("draftID"));
    if (id === null) {
      return error(c, 400, "draft id is not a uuid", "invalid_input");
    }

    let note = "";
    const raw = await c.req.text();
    if (raw.length > 0) {
      let body: unknown;
      try {
        body = JSON.parse(raw);
      } catch {
        return error(c, 400, "the body is not valid JSON", "invalid_request");
      }
      if (body === null || typeof body !== "object" || Array.isArray(body)) {
        return error(c, 400, "the body is not valid JSON", "invalid_request");
      }
      const value = (body as { note?: unknown }).note;
      if (value !== undefined && typeof value !== "string") {
        return error(c, 400, "the body is not valid JSON", "invalid_request");
      }
      note = value ?? "";
    }

    const actor = requestScope(c);

    let draft: Draft;
    try {
      if (kind === "approve") {
        draft = await this.service.approve(undefined, actor, id, actor.userId ?? "", note);
      } else if (kind === "revise") {
        draft = await this.service.revise(undefined, actor, id, actor.userId ?? "", note);
      } else {
        draft = await this.service.cancel(undefined, actor, id, actor.userId ?? "", note);
      }
    } catch (err) {
      return this.fail(c, kind + " draft", err);
    }

    // The task is told after the decision is stored. A signal that cannot be
    // delivered is not an error: the workflow reads its own status at its next
    // checkpoint, so the decision still takes effect.
    if (this.notify && !isNil(draft.taskId)) {
      try {
        await this.notify.signalApproval(
          undefined,
          actor,
          draft.taskId,
          draft.id,
          draft.status === StatusApproved,
          draft.decisionNote,
        );
      } catch (err) {
        this.log.warn(
          { draft_id: draft.id, err },
          "the decision was stored but the running task could not be told; it will read its own status",
        );
      }
    }

    return ok(c, "draft " + kind + "d", wireDraft(draft));
  }

  /** Maps the module's errors onto responses. */
  private fail(c: AppContext, operation: string, err: unknown): Response {
    if (is(err, ErrDraftNotFound)) {
      return error(c, 404, "draft not found", "draft_not_found");
    }
    if (is(err, ErrAlreadyDecided)) {
      return error(c, 409, "the draft has already been decided", "draft_already_decided");
    }
    if (is(err, ErrNotApproved)) {
      return error(c, 409, "the draft has not been approved", "draft_not_approved");
    }
    if (is(err, ErrUnknownMode)) {
      return error(c, 400, messageOf(err), "unknown_mode");
    }
    if (is(err, ErrInvalidInput)) {
      return error(c, 400, messageOf(err), "invalid_input");
    }
    if (is(err, ErrNoSender)) {
      return error(c, 503, "nothing can carry this action out yet", "approval_not_configured");
    }

    this.log.error({ operation, err }, "approval request failed");
    return error(c, 500, "internal server error", "internal_error");
  }
}

/** Registers the endpoints every member of the workspace may use. */
export function routes(group: Hono<AppEnv>, h: Handler): void {
  group.get("/drafts", h.list);
  group.get("/drafts/:draftID", h.get);
  group.get("/drafts/:draftID/revisions", h.revisions);
  group.post("/drafts/:draftID/approve", h.approve);
  group.post("/drafts/:draftID/revise", h.revise);
  group.post("/drafts/:draftID/cancel", h.cancel);

  group.get("/approval-policies", h.policies);
  group.get("/audit", h.audit);
}

/**
 * Registers the endpoints that change what the workspace allows.
 *
 * Changing a policy changes what may leave the workspace unattended, so it
 * needs a managing role the way the model provider keys do.
 */
export function managerRoutes(group: Hono<AppEnv>, h: Handler): void {
  group.put("/approval-policies", h.setPolicy);
}

// ------------------------------------------------------------------- the wire

interface PolicyRequestWire {
  action_kind?: string;
  mode?: string;
  /** Absent means no ceiling, which is a decision the workspace is allowed to make. */
  max_amount?: number | null;
}

interface DraftPayload {
  draft_id: string;
  task_id: string;
  agent_id: string;
  conversation_id?: string;
  action_kind: string;
  title: string;
  summary?: string;
  status: string;
  revision: number;
  /** The action's own shape, which is what the card shows. */
  payload: Record<string, unknown>;
  decided_by?: string;
  decided_at?: string;
  decision_note?: string;
  blocked_reason?: string;
  not_before?: string;
  sent_at?: string;
  card_message_id?: string;
  idempotency_key?: string;
  created_at: string;
}

interface RevisionPayload {
  revision: number;
  note: string;
  payload: Record<string, unknown>;
  decided_by?: string;
  created_at: string;
}

interface PolicyPayload {
  action_kind: string;
  mode: string;
  max_amount: number | null;
  updated_by?: string;
  updated_at?: string;
}

interface AuditPayload {
  id: number;
  action: string;
  object_type: string;
  object_id: string;
  actor_user_id?: string;
  actor_agent_id?: string;
  metadata: Record<string, unknown>;
  created_at: string;
}

function wireDraft(draft: Draft): DraftPayload {
  const payload: DraftPayload = {
    draft_id: draft.id,
    task_id: draft.taskId,
    agent_id: draft.agentId,
    action_kind: draft.actionKind,
    title: draft.title,
    status: draft.status,
    revision: draft.revision,
    payload: decodePayload(draft.payload),
    created_at: draft.createdAt.toISOString(),
  };

  const conversationId = wireId(draft.conversationId);
  if (conversationId) payload.conversation_id = conversationId;
  if (draft.summary) payload.summary = draft.summary;
  const decidedBy = wireId(draft.decidedBy);
  if (decidedBy) payload.decided_by = decidedBy;
  if (draft.decidedAt) payload.decided_at = draft.decidedAt.toISOString();
  if (draft.decisionNote) payload.decision_note = draft.decisionNote;
  if (draft.blockedReason) payload.blocked_reason = draft.blockedReason;
  if (draft.notBefore) payload.not_before = draft.notBefore.toISOString();
  if (draft.sentAt) payload.sent_at = draft.sentAt.toISOString();
  const cardMessageId = wireId(draft.cardMessageId);
  if (cardMessageId) payload.card_message_id = cardMessageId;
  if (draft.idempotencyKey) payload.idempotency_key = draft.idempotencyKey;

  return payload;
}

function wireRevision(revision: Revision): RevisionPayload {
  const payload: RevisionPayload = {
    revision: revision.revision,
    note: revision.note,
    payload: decodePayload(revision.payload),
    created_at: revision.createdAt.toISOString(),
  };
  const decidedBy = wireId(revision.decidedBy);
  if (decidedBy) payload.decided_by = decidedBy;
  return payload;
}

function wirePolicy(policy: Policy): PolicyPayload {
  const payload: PolicyPayload = {
    action_kind: policy.actionKind,
    mode: policy.mode,
    max_amount: policy.maxAmount,
  };
  const updatedBy = wireId(policy.updatedBy);
  if (updatedBy) payload.updated_by = updatedBy;
  if (policy.updatedAt.getTime() !== 0) payload.updated_at = policy.updatedAt.toISOString();
  return payload;
}

function wireAudit(record: AuditRecord): AuditPayload {
  const payload: AuditPayload = {
    id: record.id,
    action: record.action,
    object_type: record.objectType,
    object_id: record.objectId,
    metadata: record.metadata ?? {},
    created_at: record.createdAt.toISOString(),
  };
  const actorUserId = wireId(record.actorUserId);
  if (actorUserId) payload.actor_user_id = actorUserId;
  const actorAgentId = wireId(record.actorAgentId);
  if (actorAgentId) payload.actor_agent_id = actorAgentId;
  return payload;
}

/** Renders the action's own shape for the card. */
function decodePayload(raw: unknown): Record<string, unknown> {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    return {};
  }
  return raw as Record<string, unknown>;
}

/**
 * Renders an id, or the empty string when there is none. Absence is an empty
 * string because Go cannot omit a zero `uuid.UUID`: it is an array, so
 * `omitempty` would publish it.
 */
function wireId(id: string | null | undefined): string {
  return isNil(id) ? "" : (id as string);
}

/** The tenant the request runs as. */
function requestScope(c: AppContext): Scope {
  return { userId: currentUserId(c) ?? "", workspaceId: workspaceUUID(c) ?? "" };
}

/** Parses an optional UUID, answering null for a malformed one and "" for an absent one. */
function parseOptionalId(value: string | undefined): string | null {
  const text = (value ?? "").trim();
  if (text === "") {
    return "";
  }
  return parseUuid(text);
}

/**
 * Parses a UUID the way `google/uuid.Parse` does: canonical, without dashes,
 * braced, or behind a `urn:uuid:` prefix. Answers the canonical lowercase form,
 * or null when the text is not one.
 */
function parseUuid(value: string | undefined): string | null {
  let text = (value ?? "").trim();
  if (text.toLowerCase().startsWith("urn:uuid:")) {
    text = text.slice(9);
  }
  if (text.startsWith("{") && text.endsWith("}")) {
    text = text.slice(1, -1);
  }
  if (/^[0-9a-fA-F]{32}$/.test(text)) {
    text = [
      text.slice(0, 8),
      text.slice(8, 12),
      text.slice(12, 16),
      text.slice(16, 20),
      text.slice(20),
    ].join("-");
  }
  return isUuid(text) ? text.toLowerCase() : null;
}

/** Parses an optional RFC 3339 timestamp, answering null for absent and undefined for malformed. */
function parseOptionalTime(value: string | undefined): Date | null | undefined {
  const text = (value ?? "").trim();
  if (text === "") {
    return null;
  }
  const parsed = new Date(text);
  return Number.isNaN(parsed.getTime()) ? undefined : parsed;
}

/** `c.QueryInt` with Go's default: a missing or malformed value answers the default. */
function queryInt(c: AppContext, key: string, fallback: number): number {
  const raw = c.req.query(key);
  if (raw === undefined || raw === "") return fallback;
  if (!/^[+-]?\d+$/.test(raw)) return fallback;
  const parsed = Number.parseInt(raw, 10);
  return Number.isSafeInteger(parsed) ? parsed : fallback;
}

/** Reads a JSON body, answering null for one that is not an object, as BodyParser refuses it. */
async function readJson<T>(c: AppContext): Promise<Partial<T> | null> {
  const body: unknown = await c.req.json().catch(() => null);
  if (body === null || typeof body !== "object" || Array.isArray(body)) {
    return null;
  }
  return body as Partial<T>;
}

function messageOf(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
