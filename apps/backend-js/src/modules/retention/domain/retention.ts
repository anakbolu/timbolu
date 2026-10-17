/**
 * The retention contract: how long the deployment keeps what a customer said
 * and what a model answered, and which modules do the removing.
 *
 * The rule this module exists to express is one sentence long: a task's
 * *shape* is kept for good and its raw payload is not. What a step was — which
 * tool, how many tokens, when it ran — is what the task history and the cost
 * report are built from; the input and the output are a copy of a conversation,
 * which is the part with a clock on it. A retention rule that removed whole
 * rows would take the history with it, and the deployment would have to choose
 * between keeping records and keeping less data.
 *
 * This module owns the schedule and the policy and nothing else. The tables
 * being pruned belong to the task and chat modules, so they are the modules
 * that prune them: the step bulk-update is the task repository's, and an
 * attachment takes its row *and* its object storage key with it, which is the
 * chat module's business because it is the module that owns the bucket path.
 * The container adapts the two ports below onto those implementations.
 *
 * Ported 1:1 from internal/modules/retention/domain/retention.go.
 */

import type {
  Erasure,
  ErasureRequest,
  ErasureResult,
  ErasureService,
} from "./erasure.ts";

/** The canonical nil UUID, Go's `uuid.Nil`. */
export const NilUUID = "00000000-0000-0000-0000-000000000000";

/** A request that cannot be honoured. */
export const ErrInvalidInput = new Error("retention: invalid input");
/** A deployment without a database. */
export const ErrNoRepository = new Error("retention: no repository configured");

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

/**
 * The tenant a pass runs as.
 *
 * It carries no user on purpose. The job is the deployment acting on its own
 * schedule, not a person asking for something, so the scope a pruned statement
 * runs in is a workspace and nothing else — and Row Level Security, which keys
 * on the workspace, is what keeps one tenant's sweep out of another's rows.
 */
export interface Scope {
  workspaceId: string;
}

/** How long each kind of content is kept. */
export interface Policy {
  /** How long a task step's raw input and output are kept, in milliseconds. */
  stepWindow: number;
  /**
   * How long an attachment is kept, in milliseconds. Zero means attachments
   * are kept as long as the message that carries them, which is a deployment
   * that has no attachment policy — and a pass that then removes none.
   */
  attachmentWindow: number;
  /**
   * Bounds how many steps and how many attachments one workspace's pass
   * removes, so a deployment with years of history makes progress in slices
   * rather than in one unbounded statement.
   */
  limit: number;
}

/**
 * How long a task step's raw payload is kept when the deployment does not say
 * otherwise: thirty days.
 */
export const DefaultStepWindow = 30 * 24 * 60 * 60 * 1000;

/** Bounds one workspace's pass. */
export const DefaultLimit = 500;

/** What one workspace's step prune removed. */
export interface PrunedSteps {
  /** How many steps had their contents removed. */
  count: number;
  /**
   * How many steps of that workspace were already past the window and still
   * hold their contents, which is what tells an operator whether one pass kept
   * up with the backlog or whether the batch is too small.
   */
  remaining: number;
}

/** What one workspace's attachment sweep removed. */
export interface PrunedAttachments {
  /** How many attachments were removed, row and object together. */
  count: number;
  /** What object storage gave back, which is the number an operator watches. */
  bytesFreed: number;
}

/**
 * Removes the raw payload of a task's steps.
 *
 * The row stays: the step's kind, its tool, its sequence, its token counts and
 * when it ran are what a task history is made of, and they are not content.
 * Only `input` and `output` go, and the step is marked so a reader can tell an
 * empty payload from a pruned one.
 */
export interface StepPruner {
  pruneSteps(scope: Scope, olderThan: Date, limit: number): Promise<PrunedSteps>;
}

/**
 * Removes attachments past their window, row and object together.
 *
 * It is one port rather than two calls because the two halves must not drift:
 * a row removed without its object leaks storage forever, and an object
 * removed without its row leaves a message pointing at bytes that are gone.
 */
export interface AttachmentPruner {
  pruneAttachments(
    scope: Scope,
    olderThan: Date,
    limit: number,
  ): Promise<PrunedAttachments>;
}

/**
 * What the container calls: one pass, and the policy it applies — plus the
 * erasure half, which is the same policy answering a request instead of a
 * clock.
 *
 * `policy` is on the interface rather than kept private because "what does
 * this deployment actually keep" is a question an operator asks, and deriving
 * it in one place is what stops a configured zero from meaning three different
 * things in three call sites.
 */
export interface Service extends ErasureService {
  /** Makes one pass over every workspace that is due, and reports what it did. */
  run(signal?: AbortSignal): Promise<RunResult>;
  /** The window and the batch this deployment applies. */
  policy(): Policy;
}

/**
 * The storage this module owns: the queue of workspaces due a pass, and the
 * record of what each pass did.
 */
export interface Repository {
  /**
   * Returns the workspaces whose next pass is due, oldest first.
   *
   * It is the one read with no tenant scope, and it carries a workspace id and
   * a moment and nothing else: the sweep cannot open a scope until a row tells
   * it which one.
   */
  dueTargets(now: Date, limit: number): Promise<string[]>;
  /** Moves a workspace's next pass forward. */
  touchTarget(scope: Scope, next: Date): Promise<void>;

  /** Opens a run record and returns its id. */
  startRun(): Promise<number>;
  /** Closes a run record with what it did. */
  finishRun(id: number, result: RunResult, failure: Error | null): Promise<void>;
  /** Returns the newest runs, which is what a monitor reads. */
  runs(limit: number): Promise<Run[]>;
}

/** What one pass over the fleet did. */
export interface RunResult {
  workspaces: number;
  steps_pruned: number;
  attachments_pruned: number;
  bytes_freed: number;
}

/** One recorded pass. */
export interface Run {
  id: number;
  startedAt: Date;
  /** Null until the run closes, matching Go's zero `time.Time`. */
  finishedAt: Date | null;
  stepsPruned: number;
  attachments: number;
  bytesFreed: number;
  error: string;
}

/** Re-exported so a caller can import the whole contract from one file. */
export type { Erasure, ErasureRequest, ErasureResult, ErasureService };
