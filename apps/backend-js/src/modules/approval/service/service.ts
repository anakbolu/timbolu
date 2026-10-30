/**
 * The approval use cases: asking for a decision, recording one, and carrying
 * out what was allowed.
 *
 * The rule the package exists for is in `request`: an action that reaches
 * outside Bolu waits for a person unless the workspace has said otherwise for
 * that specific kind of action. An action kind with no stored policy behaves as
 * `always_draft`, so the safe answer is also the default one.
 *
 * Ported 1:1 from internal/modules/approval/service/service.go.
 */

import type { Logger } from "../../../platform/logger.ts";
import {
  EventDraftCreated,
  EventDraftDecided,
  ErrInvalidInput,
  ErrNoSender,
  ErrNotApproved,
  ErrPolicyNotFound,
  ErrUnknownMode,
  NilUUID,
  StatusApproved,
  StatusCanceled,
  StatusPending,
  StatusRevise,
  StatusSent,
  defaultPolicy,
  is,
  isMissingID,
  isNil,
  isZeroScope,
  needsDraft,
  openWindow,
  wrap,
  type AuditEntry,
  type AuditFilter,
  type AuditRecord,
  type Cards,
  type Clock,
  type Decision,
  type Draft,
  type EventSink,
  type Filter,
  type NewDraft,
  type Policy,
  type Repository,
  type Revision,
  type Scope,
  type Sender,
  type WorkingHours,
} from "../domain/approval.ts";
import type {
  Metrics,
  Outcome,
  PolicyRequest,
  Request,
  Service as ServicePort,
} from "../domain/service.ts";
import type { Notifier } from "../../push/domain/push.ts";
import { requestAmount } from "../domain/service.ts";
import { hashPayload } from "./digest.ts";

/** Bounds the one-line description a card shows. */
export const SummaryMaxRunes = 160;

/** Reads the window a workspace lets actions leave in. */
export interface HoursReader {
  workingHours(workspaceId: string): Promise<WorkingHours>;
}

/** The service dependencies. */
export interface Deps {
  /** Reports what people did with the drafts. Optional. */
  metrics?: Metrics | null;
  drafts: Repository;
  /** Carries an approved action out. Optional: without one a draft is held and says so. */
  sender?: Sender | null;
  /** Writes the draft card into the conversation. Optional. */
  cards?: Cards | null;
  /** Publishes what a draft is doing. Optional. */
  events?: EventSink | null;
  /** Reads the workspace's working hours. Optional: without it every window is open. */
  hours?: HoursReader | null;
  /**
   * Wakes the phones of the person who has to decide. Optional, and a no-op
   * when absent: a deployment without an Expo project still records drafts, it
   * just does not ring.
   */
  notifications?: Notifier | null;
  logger?: Logger | null;
  clock?: Clock;
}

/** The approval implementation. */
export class Service implements ServicePort {
  private readonly clock: Clock;

  constructor(private readonly deps: Deps) {
    this.clock = deps.clock ?? (() => new Date());
  }

  /**
   * Either stores a proposal for a person or carries the action out now.
   *
   * The policy decides which, and the decision is made here rather than by the
   * caller: a task proposes an action, and whether a human is asked is the
   * workspace's rule, not the task's.
   */
  async request(ctx: AbortSignal | undefined, scope: Scope, req: Request): Promise<Outcome> {
    this.validate(scope, req);

    const policy = await this.effectivePolicy(scope, req.actionKind);

    if (!needsDraft(policy, requestAmount(req.payload))) {
      // The workspace said this action kind may go out unattended, and the
      // value is within the ceiling it set. There is still a record: the draft
      // row is written and marked sent, so "what did the agents send" has an
      // answer even when nobody was asked.
      return this.carryOutNow(ctx, scope, req, policy);
    }

    let draft = await this.deps.drafts.createDraft(scope, this.newDraft(req));

    await this.recordAudit(scope, {
      action: "draft.created",
      objectType: "draft",
      objectId: draft.id,
      actorAgentId: req.agentId,
      actorUserId: "",
      metadata: {
        action_kind: req.actionKind,
        task_id: req.taskId,
        summary: draft.summary,
        mode: policy.mode,
      },
    });

    // The card goes into the thread the task came from, so the person decides
    // where they are already reading. A card that could not be written does not
    // fail the proposal: the draft is stored and the dashboard still lists it.
    const cards = this.deps.cards;
    if (cards && !isNil(draft.conversationId)) {
      try {
        const messageId = await cards.write(ctx, scope, draft);
        if (!isNil(messageId)) {
          draft = await this.deps.drafts.setCardMessage(scope, draft.id, messageId);
        }
      } catch (err) {
        this.deps.logger?.warn(
          { draft_id: draft.id, err },
          "could not write the draft card into the conversation",
        );
      }
    }

    await this.emit(ctx, scope, draft, EventDraftCreated);

    return {
      draftId: draft.id,
      status: draft.status,
      title: draft.title,
      summary: draft.summary,
      executed: false,
      content: null,
      reason: "",
      notBefore: null,
    };
  }

  /** Sends an action the policy allowed, recording it first. */
  private async carryOutNow(
    ctx: AbortSignal | undefined,
    scope: Scope,
    req: Request,
    policy: Policy,
  ): Promise<Outcome> {
    const draft = await this.deps.drafts.createDraft(scope, this.newDraft(req));

    // The row is moved straight to approved so the send path below is the same
    // one a person's approval takes: an auto policy skips the person, not the
    // machinery.
    const approved = await this.deps.drafts.decide(scope, {
      draftId: draft.id,
      status: StatusApproved,
      note: "disetujui otomatis oleh kebijakan workspace",
      decidedBy: NilUUID,
    });

    await this.recordAudit(scope, {
      action: "draft.approved",
      objectType: "draft",
      objectId: approved.id,
      actorAgentId: req.agentId,
      actorUserId: "",
      metadata: {
        action_kind: req.actionKind,
        mode: policy.mode,
        automatic: true,
      },
    });

    const outcome = await this.carryOut(ctx, scope, approved.id);
    outcome.summary = approved.summary;
    return outcome;
  }

  /** Records that a person allowed a draft. */
  async approve(
    ctx: AbortSignal | undefined,
    scope: Scope,
    draftId: string,
    userId: string,
    note: string,
  ): Promise<Draft> {
    return this.decide(ctx, scope, {
      draftId,
      status: StatusApproved,
      note,
      decidedBy: userId,
    }, "draft.approved");
  }

  /**
   * Records that a person sent a draft back.
   *
   * The note is what the agent reads on its next round, so an empty one is
   * refused rather than stored: a revision without a reason sends the agent
   * back to work with nothing to change.
   */
  async revise(
    ctx: AbortSignal | undefined,
    scope: Scope,
    draftId: string,
    userId: string,
    note: string,
  ): Promise<Draft> {
    if (note.trim() === "") {
      throw wrap(ErrInvalidInput, "a revision needs a note");
    }

    return this.decide(ctx, scope, {
      draftId,
      status: StatusRevise,
      note,
      decidedBy: userId,
    }, "draft.revised");
  }

  /** Records that a person stopped a draft. */
  async cancel(
    ctx: AbortSignal | undefined,
    scope: Scope,
    draftId: string,
    userId: string,
    note: string,
  ): Promise<Draft> {
    return this.decide(ctx, scope, {
      draftId,
      status: StatusCanceled,
      note,
      decidedBy: userId,
    }, "draft.canceled");
  }

  /**
   * Records one answer, once.
   *
   * The store refuses a second decision on the same draft, which is what makes
   * a duplicate signal harmless: the first answer stands and the second is
   * reported as already decided rather than overwriting it.
   */
  private async decide(
    ctx: AbortSignal | undefined,
    scope: Scope,
    decision: Decision,
    action: string,
  ): Promise<Draft> {
    this.requireScope(scope);
    // Only an absent id is a bad request. A zero uuid was sent and names no
    // draft, so the store answers not found rather than telling the client its
    // request was wrong (#480).
    if (isMissingID(decision.draftId)) {
      throw wrap(ErrInvalidInput, "draft id is required");
    }

    const draft = await this.deps.drafts.decide(scope, decision);

    // A revision keeps what the person was looking at, so the trail answers
    // "why was this sent, after three attempts" without the old payload being
    // lost.
    if (draft.status === StatusRevise) {
      try {
        await this.deps.drafts.appendRevision(scope, {
          id: NilUUID,
          draftId: draft.id,
          revision: draft.revision,
          note: decision.note,
          payload: draft.payload,
          decidedBy: decision.decidedBy,
          createdAt: new Date(0),
        });
      } catch (err) {
        this.deps.logger?.warn({ draft_id: draft.id, err }, "could not append the revision trail");
      }
    }

    await this.recordAudit(scope, {
      action,
      objectType: "draft",
      objectId: draft.id,
      actorUserId: decision.decidedBy,
      actorAgentId: draft.agentId,
      metadata: {
        action_kind: draft.actionKind,
        task_id: draft.taskId,
        note: decision.note,
        revision: draft.revision,
      },
    });

    // The card is rewritten so a reload shows what happened rather than a card
    // still offering to approve something already decided.
    const cards = this.deps.cards;
    if (cards && !isNil(draft.cardMessageId)) {
      try {
        await cards.rewrite(ctx, scope, draft.cardMessageId, draft);
      } catch (err) {
        this.deps.logger?.warn({ draft_id: draft.id, err }, "could not rewrite the draft card");
      }
    }

    // Recorded before the event, so a chart and a listener see the same
    // decision in the same order. The revision flag is the draft's own count at
    // decision time: a draft decided after a person sent it back is a different
    // fact from one approved as written.
    this.deps.metrics?.draftDecided(ctx, draft.status, draft.revision > 0);

    await this.emit(ctx, scope, draft, EventDraftDecided);
    return draft;
  }

  /**
   * Executes an approved draft.
   *
   * It holds the action when the workspace is closed, reporting the moment it
   * may leave rather than sending: the person approved what to send, not when,
   * and the workspace's hours decide the when.
   */
  async carryOut(ctx: AbortSignal | undefined, scope: Scope, draftId: string): Promise<Outcome> {
    this.requireScope(scope);

    const draft = await this.deps.drafts.getDraft(scope, draftId);
    if (draft.status !== StatusApproved) {
      throw wrap(ErrNotApproved, `status is ${JSON.stringify(draft.status)}`);
    }

    const window = await this.window(scope);
    if (!window.open) {
      const reason = "ditahan sampai jam kerja: " + holdStamp(window.next, window.zone);
      const held = await this.deps.drafts.hold(scope, draft.id, window.next, reason);

      await this.emit(ctx, scope, held, EventDraftDecided);
      return {
        draftId: held.id,
        status: held.status,
        title: held.title,
        summary: held.summary,
        executed: false,
        content: null,
        reason: held.blockedReason,
        notBefore: held.notBefore,
      };
    }

    const sender = this.deps.sender;
    if (!sender) {
      // Nothing can carry the action out. Saying so is the honest answer: a
      // draft marked sent when nothing left would be a lie the audit trail
      // would repeat forever.
      throw wrap(ErrNoSender, "an approved action cannot be executed");
    }

    const content = await sender.send(ctx, scope, draft);
    const sent = await this.deps.drafts.markSent(scope, draft.id);

    await this.recordAudit(scope, {
      action: "draft.sent",
      objectType: "draft",
      objectId: sent.id,
      actorUserId: sent.decidedBy,
      actorAgentId: sent.agentId,
      metadata: {
        action_kind: sent.actionKind,
        task_id: sent.taskId,
        idempotency_key: sent.idempotencyKey,
        approved_by: sent.decidedBy,
      },
    });

    const cards = this.deps.cards;
    if (cards && !isNil(sent.cardMessageId)) {
      try {
        await cards.rewrite(ctx, scope, sent.cardMessageId, sent);
      } catch (err) {
        this.deps.logger?.warn({ draft_id: sent.id, err }, "could not rewrite the draft card after sending");
      }
    }

    await this.emit(ctx, scope, sent, EventDraftDecided);

    return {
      draftId: sent.id,
      status: sent.status,
      title: sent.title,
      summary: sent.summary,
      executed: true,
      content,
      reason: "",
      notBefore: null,
    };
  }

  /** Reports the earliest moment an action may leave this workspace. */
  async sendWindow(ctx: AbortSignal | undefined, scope: Scope): Promise<Date> {
    this.requireScope(scope);

    const window = await this.window(scope);
    return window.open ? this.clock() : window.next;
  }

  /**
   * Reports whether actions may leave now, and when they may next.
   *
   * The hours are read per call rather than cached, so changing them or the
   * timezone recomputes the hold instead of leaving a stale one behind.
   */
  private async window(scope: Scope): Promise<{ open: boolean; next: Date; zone: string }> {
    const hours = this.deps.hours;
    if (!hours) {
      return { open: true, next: this.clock(), zone: "" };
    }

    let working: WorkingHours;
    try {
      working = await hours.workingHours(scope.workspaceId);
    } catch (err) {
      this.deps.logger?.warn(
        { workspace_id: scope.workspaceId, err },
        "could not read the workspace's working hours; treating the window as open",
      );
      return { open: true, next: this.clock(), zone: "" };
    }

    return openWindow(working, this.clock());
  }

  /** Returns the workspace's drafts, newest first. */
  async drafts(ctx: AbortSignal | undefined, scope: Scope, filter: Filter): Promise<Draft[]> {
    this.requireScope(scope);
    if (filter.status !== "" && !knownStatus(filter.status)) {
      throw wrap(ErrInvalidInput, JSON.stringify(filter.status));
    }

    return this.deps.drafts.listDrafts(scope, filter);
  }

  /** Returns one draft. */
  async draft(ctx: AbortSignal | undefined, scope: Scope, id: string): Promise<Draft> {
    this.requireScope(scope);
    // The id reached here through a path segment, so a zero uuid is an id the
    // caller sent rather than a missing one: it names no draft, and the store's
    // not-found answer is the honest one (#480).
    if (isMissingID(id)) {
      throw wrap(ErrInvalidInput, "draft id is required");
    }

    return this.deps.drafts.getDraft(scope, id);
  }

  /** Returns a draft's trail, oldest first. */
  async revisions(ctx: AbortSignal | undefined, scope: Scope, draftId: string): Promise<Revision[]> {
    this.requireScope(scope);

    // Reading the draft first confirms the caller may see it: a draft id from
    // another workspace answers not found rather than an empty list.
    await this.deps.drafts.getDraft(scope, draftId);

    return this.deps.drafts.revisions(scope, draftId);
  }

  /**
   * Returns the effective policy for every action kind that reaches outside
   * Bolu.
   *
   * A kind the workspace never set a policy for is reported as the conservative
   * default rather than omitted: the screen then shows what is actually in
   * force, and there is no kind that silently has no policy.
   */
  async policies(ctx: AbortSignal | undefined, scope: Scope): Promise<Policy[]> {
    this.requireScope(scope);

    const stored = await this.deps.drafts.policies(scope);
    const kinds = await this.deps.drafts.actionKinds();

    const byKind = new Map<string, Policy>();
    for (const policy of stored) {
      byKind.set(policy.actionKind, policy);
    }

    const policies: Policy[] = [];
    const seen = new Set<string>();
    for (const kind of kinds) {
      seen.add(kind);
      const policy = byKind.get(kind);
      policies.push(policy ?? defaultPolicy(kind));
    }

    // A stored policy for an action kind the catalogue no longer ships is still
    // reported: dropping it would hide a decision the workspace made.
    for (const policy of stored) {
      if (!seen.has(policy.actionKind)) {
        policies.push(policy);
      }
    }

    return policies;
  }

  /** Stores a workspace's decision about one action kind. */
  async setPolicy(ctx: AbortSignal | undefined, scope: Scope, req: PolicyRequest): Promise<Policy> {
    this.requireScope(scope);

    const kind = req.actionKind.trim();
    if (kind === "") {
      throw wrap(ErrInvalidInput, "action kind is required");
    }
    if (req.mode !== "always_draft" && req.mode !== "auto") {
      throw wrap(ErrUnknownMode, JSON.stringify(req.mode));
    }
    if (req.maxAmount !== null && req.maxAmount < 0) {
      throw wrap(ErrInvalidInput, "the ceiling cannot be negative");
    }

    const policy = await this.deps.drafts.setPolicy(scope, {
      actionKind: kind,
      mode: req.mode,
      maxAmount: req.maxAmount,
      updatedBy: scope.userId ?? "",
      createdAt: new Date(0),
      updatedAt: new Date(0),
    });

    await this.recordAudit(scope, {
      action: "policy.updated",
      objectType: "approval_policy",
      objectId: policy.actionKind,
      actorUserId: scope.userId ?? "",
      actorAgentId: "",
      metadata: {
        action_kind: policy.actionKind,
        mode: policy.mode,
        max_amount: policy.maxAmount,
      },
    });

    return policy;
  }

  /** Returns the trail the filter names, newest first. */
  async audit(ctx: AbortSignal | undefined, scope: Scope, filter: AuditFilter): Promise<AuditRecord[]> {
    this.requireScope(scope);

    return this.deps.drafts.auditTrail(scope, filter);
  }

  /**
   * Reads the policy in force for one action kind.
   *
   * An action kind the workspace never set a policy for is `always_draft`. That
   * is the product's first design principle, and it is enforced here rather
   * than by hoping every workspace has a row: a missing row is the safe answer,
   * not an error and not an implicit yes.
   */
  private async effectivePolicy(scope: Scope, actionKind: string): Promise<Policy> {
    try {
      return await this.deps.drafts.policy(scope, actionKind);
    } catch (err) {
      if (is(err, ErrPolicyNotFound)) {
        return defaultPolicy(actionKind);
      }
      throw err;
    }
  }

  /**
   * Records one entry. A trail that could not be written is reported rather
   * than failing the action: the decision already happened, and refusing it
   * after the fact would leave the draft in a state nobody decided.
   */
  private async recordAudit(scope: Scope, entry: AuditEntry): Promise<void> {
    try {
      await this.deps.drafts.recordAudit(scope, entry);
    } catch (err) {
      this.deps.logger?.warn({ action: entry.action, err }, "could not record an approval audit entry");
    }
  }

  /** Publishes a draft's state. */
  private async emit(
    ctx: AbortSignal | undefined,
    scope: Scope,
    draft: Draft,
    eventType: string,
  ): Promise<void> {
    const sink = this.deps.events;
    if (!sink) {
      return;
    }

    const payload = {
      draft_id: draft.id,
      task_id: draft.taskId,
      agent_id: draft.agentId,
      action_kind: draft.actionKind,
      status: draft.status,
      summary: draft.summary,
      title: draft.title,
      revision: draft.revision,
      not_before: draft.notBefore ? draft.notBefore.toISOString() : "",
      blocked: draft.blockedReason,
    };

    try {
      await sink.emit(ctx, {
        workspaceId: scope.workspaceId,
        type: eventType,
        actorAgentId: draft.agentId,
        actorUserId: draft.decidedBy,
        conversationId: draft.conversationId,
        taskId: draft.taskId,
        draftId: draft.id,
        payload,
      });
    } catch (err) {
      this.deps.logger?.warn({ type: eventType, err }, "draft event published only to the store");
    }

    await this.notify(scope, eventType, draft);
  }

  /**
   * Wakes the deciding person's phones. Only a draft that just appeared is worth
   * a notification: a decision the caller made themselves needs no alarm, and
   * `draft.decided` fires for holds and sends as well, which would ring for work
   * that is already done.
   */
  private async notify(scope: Scope, eventType: string, draft: Draft): Promise<void> {
    const notifier = this.deps.notifications;
    if (!notifier || eventType !== EventDraftCreated) {
      return;
    }

    try {
      await notifier.send(scope, {
        type: eventType,
        taskId: draft.taskId,
        draftId: draft.id,
      });
    } catch (err) {
      // The notifier is documented not to throw; this is the guard that keeps a
      // push from ever turning a recorded draft into a failed request.
      this.deps.logger?.warn({ draft_id: draft.id, err }, "draft notification was not sent");
    }
  }

  private newDraft(req: Request): NewDraft {
    return {
      taskId: req.taskId,
      agentId: req.agentId,
      conversationId: req.conversationId,
      actionKind: req.actionKind,
      title: req.title,
      summary: summaryOf(req),
      payload: req.payload,
      idempotencyKey: idempotencyKey(req),
    };
  }

  private validate(scope: Scope, req: Request): void {
    this.requireScope(scope);
    if (isNil(req.taskId)) {
      throw wrap(ErrInvalidInput, "task id is required");
    }
    if (isNil(req.agentId)) {
      throw wrap(ErrInvalidInput, "agent id is required");
    }
    if (req.actionKind.trim() === "") {
      throw wrap(ErrInvalidInput, "action kind is required");
    }
  }

  private requireScope(scope: Scope): void {
    if (isZeroScope(scope)) {
      throw wrap(ErrInvalidInput, "workspace is required");
    }
  }
}

/**
 * The one line a card shows. A caller that supplied one keeps it; otherwise the
 * title stands in, because a card with no description is worse than a short one.
 */
export function summaryOf(req: Request): string {
  let summary = req.summary.trim();
  if (summary === "") {
    summary = req.title.trim();
  }

  const runes = Array.from(summary);
  if (runes.length <= SummaryMaxRunes) {
    return summary;
  }
  return runes.slice(0, SummaryMaxRunes - 1).join("") + "…";
}

/**
 * Identifies one proposal.
 *
 * It is derived from what the proposal is rather than generated, so a retried
 * activity — the same task proposing the same action with the same arguments —
 * resolves to the draft already opened instead of a second one. A payload that
 * differs is a different proposal, which is why the arguments are part of it.
 */
export function idempotencyKey(req: Request): string {
  let payload = JSON.stringify(req.payload ?? {}).trim();
  if (payload === "") {
    payload = "{}";
  }
  return req.taskId + ":" + req.actionKind + ":" + hashPayload(payload);
}

/** The moment a held action may leave, as Go's `02 Jan 15:04` in the workspace's zone. */
function holdStamp(at: Date, zone: string): string {
  const parts = new Intl.DateTimeFormat("en-GB", {
    timeZone: zone || "UTC",
    day: "2-digit",
    month: "short",
    hour: "2-digit",
    minute: "2-digit",
    hourCycle: "h23",
  }).formatToParts(at);
  const read = (type: Intl.DateTimeFormatPartTypes): string =>
    parts.find((part) => part.type === type)?.value ?? "";
  return `${read("day")} ${read("month")} ${read("hour")}:${read("minute")}`;
}

/** The statuses the schema allows. */
function knownStatus(status: string): boolean {
  return (
    status === StatusPending ||
    status === StatusApproved ||
    status === StatusRevise ||
    status === StatusSent ||
    status === StatusCanceled
  );
}
