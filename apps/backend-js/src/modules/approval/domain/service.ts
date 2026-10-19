/**
 * The approval use case contract: the request a task makes, the outcome it gets
 * back, and the policy a manager stores.
 *
 * Ported 1:1 from internal/modules/approval/domain/service.go.
 */

import type { AuditFilter, AuditRecord, Clock, Draft, Filter, Policy, Revision, Scope } from "./approval.ts";

/** An action a task wants to take that reaches outside Bolu. */
export interface Request {
  taskId: string;
  agentId: string;
  conversationId: string;
  actionKind: string;
  title: string;
  summary: string;
  payload: unknown;
}

/**
 * Reads the value an action carries, which is what a policy's ceiling is
 * compared against.
 *
 * The field is a convention rather than a schema, because the action's own
 * shape belongs to its integration: a payload that states no amount is treated
 * as unbounded by `needsDraft`, which is the conservative reading.
 *
 * The first key present wins, and only a numeric JSON value counts — the same
 * keys and the same order as the Go port.
 */
export function requestAmount(payload: unknown): number | null {
  if (payload === null || typeof payload !== "object" || Array.isArray(payload)) {
    return null;
  }

  const record = payload as Record<string, unknown>;
  for (const key of ["amount", "total", "total_amount", "value"]) {
    if (!(key in record)) {
      continue;
    }
    const raw = record[key];
    if (typeof raw === "number" && Number.isFinite(raw)) {
      // Go's float64 -> int64 conversion truncates toward zero.
      return Math.trunc(raw);
    }
  }
  return null;
}

/** What happened to a proposal. */
export interface Outcome {
  /** Set when a person has to decide. */
  draftId: string;
  /** pending when it is waiting, sent when the policy carried it out. */
  status: string;
  title: string;
  summary: string;
  /** Says the action ran without a person, and content is what it answered. */
  executed: boolean;
  content: unknown;
  /** Explains a refusal or a hold, in the user's words. */
  reason: string;
  /** When a held action may leave. */
  notBefore: Date | null;
}

/** One policy to store. */
export interface PolicyRequest {
  actionKind: string;
  mode: string;
  /** The ceiling an auto policy may carry out unattended, in rupiah. Null means none. */
  maxAmount: number | null;
}

/**
 * Where the approval gate reports what people did with its drafts.
 *
 * It carries two values and nothing else: a draft's identity, its task, and its
 * workspace are unbounded — one time series per draft — and the per-draft
 * question is answered from the `drafts` table. What the fleet's charts need is
 * the *rate*: how often a person approves what the bot produced, and how often
 * they have to send it back first.
 */
export interface Metrics {
  /**
   * One decision. `outcome` is the deployment's own vocabulary — approved,
   * revised, canceled — and `revision` is whether this decision followed a
   * revision, which is the difference between "approved as written" and
   * "approved after being corrected".
   */
  draftDecided(ctx: AbortSignal | undefined, outcome: string, revision: boolean): void;
}

/** The approval use case contract. */
export interface Service {
  /** Stores a proposal for a person, or carries it out now when the policy allows. */
  request(ctx: AbortSignal | undefined, scope: Scope, req: Request): Promise<Outcome>;
  /** Records that a person allowed a draft. */
  approve(ctx: AbortSignal | undefined, scope: Scope, draftId: string, userId: string, note: string): Promise<Draft>;
  /** Records that a person sent a draft back, with the note the agent reads next round. */
  revise(ctx: AbortSignal | undefined, scope: Scope, draftId: string, userId: string, note: string): Promise<Draft>;
  /** Records that a person stopped a draft. */
  cancel(ctx: AbortSignal | undefined, scope: Scope, draftId: string, userId: string, note: string): Promise<Draft>;
  /** Executes an approved draft, holding it when the workspace is closed. */
  carryOut(ctx: AbortSignal | undefined, scope: Scope, draftId: string): Promise<Outcome>;
  /** The earliest moment an action may leave this workspace. */
  sendWindow(ctx: AbortSignal | undefined, scope: Scope): Promise<Date>;

  /** The workspace's drafts, newest first. */
  drafts(ctx: AbortSignal | undefined, scope: Scope, filter: Filter): Promise<Draft[]>;
  /** One draft. */
  draft(ctx: AbortSignal | undefined, scope: Scope, id: string): Promise<Draft>;
  /** A draft's trail, oldest first. */
  revisions(ctx: AbortSignal | undefined, scope: Scope, draftId: string): Promise<Revision[]>;

  /** The effective policy for every action kind that reaches outside Bolu. */
  policies(ctx: AbortSignal | undefined, scope: Scope): Promise<Policy[]>;
  /** Stores a workspace's decision about one action kind. */
  setPolicy(ctx: AbortSignal | undefined, scope: Scope, req: PolicyRequest): Promise<Policy>;

  /** The trail the filter names, newest first. */
  audit(ctx: AbortSignal | undefined, scope: Scope, filter: AuditFilter): Promise<AuditRecord[]>;
}

/** The clock type the service reads, so a test can move the working day. */
export type { Clock };
