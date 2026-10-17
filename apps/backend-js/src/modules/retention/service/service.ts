/**
 * The retention pass.
 *
 * One pass over the fleet, and the shape of it follows from what it must not
 * do:
 *
 * - It removes *content* and never rows. A task step keeps its kind, its tool,
 *   its token counts and when it ran, so the history a person reads is the same
 *   afterwards; only the input and the output — a copy of a conversation — go.
 * - It runs per workspace rather than in one statement. Every table it prunes
 *   is protected by Row Level Security, so there is no such thing as "prune
 *   every tenant" without opening a workspace scope, and opening one per tenant
 *   is what makes the isolation real rather than a filter somebody has to
 *   remember.
 * - It is a loop rather than a durable workflow, for the reason the billing
 *   sweep is one: nothing in it waits. `retention_targets.next_prune_at` is
 *   what makes a missed pass a pass that is still due, so a process that was
 *   down for a day catches up on its next run instead of skipping the day.
 * - One workspace that cannot be pruned does not stop the pass. The rest of the
 *   fleet is waiting, and this one is due again on the interval.
 *
 * Ported 1:1 from internal/modules/retention/service/service.go; the erasure
 * methods Go declares in `erasure.go` live in `./erasure.ts` because
 * TypeScript cannot spread one class across files.
 */

import { nopLogger, type Logger } from "../../../platform/logger.ts";
import {
  DefaultLimit,
  DefaultStepWindow,
  ErrNoRepository,
  type AttachmentPruner,
  type Policy,
  type Repository,
  type RunResult,
  type Scope,
  type Service as ServicePort,
  type StepPruner,
} from "../domain/retention.ts";
import type {
  ErasureRepository,
  ErasureResult,
  Erasure,
  ErasureRequest,
  ObjectEraser,
  WorkspaceEraser,
} from "../domain/erasure.ts";
import { erase, erasures, findErasure } from "./erasure.ts";

/** The service dependencies. */
export interface Deps {
  repository: Repository | null;
  /**
   * Prunes a task's raw step contents. Optional: without it the pass removes
   * attachments only, and says so rather than reporting a silent zero.
   */
  steps?: StepPruner | null;
  /** Removes attachments past their window, row and object together. */
  attachments?: AttachmentPruner | null;
  logger?: Logger | null;
  config?: Config | null;
  /** The source of "now", so the windows are testable. */
  clock?: (() => Date) | null;
  /**
   * The record of what was removed on request: the evidence a person who asked
   * has a right to be answered with. Optional: without it an erasure request
   * is refused rather than done silently.
   */
  erasures?: ErasureRepository | null;
  /**
   * Removes a workspace and everything that cascades from it, and reads the two
   * names the erasure record keeps. Optional, for the same reason.
   */
  workspaces?: WorkspaceEraser | null;
  /** Removes every object a workspace owns. Optional, for the same reason. */
  objects?: ObjectEraser | null;
}

/** The policy the service applies. */
export interface Config {
  /** How long a task step's raw payload is kept, in milliseconds. Zero: thirty days. */
  stepWindow?: number;
  /**
   * How long an attachment is kept, in milliseconds. Zero keeps attachments as
   * long as the message that carries them, and the pass then removes none.
   */
  attachmentWindow?: number;
  /** How far forward a workspace's next pass is scheduled, in milliseconds. */
  interval?: number;
  /** Bounds how many steps and how many attachments one workspace's pass removes. */
  limit?: number;
  /** Bounds how many workspaces one pass looks at. */
  sweepLimit?: number;
}

/** How long a workspace waits between passes. */
export const DefaultInterval = 24 * 60 * 60 * 1000;

/** Bounds how many workspaces one pass looks at. */
export const DefaultSweepLimit = 200;

/** The dependencies with their absent optionals resolved. */
export interface ResolvedDeps {
  repository: Repository | null;
  steps: StepPruner | null;
  attachments: AttachmentPruner | null;
  logger: Logger;
  clock: () => Date;
  erasures: ErasureRepository | null;
  workspaces: WorkspaceEraser | null;
  objects: ObjectEraser | null;
}

/**
 * A pass that failed part way.
 *
 * Go returns the partial result alongside the error; the counts are on the
 * error so the caller can log both, which is what the container's
 * `pruneContent` does.
 */
export class RetentionRunError extends Error {
  readonly result: RunResult;

  constructor(result: RunResult, cause: Error) {
    super(cause.message, { cause });
    this.name = "RetentionRunError";
    this.result = result;
  }
}

/** What one workspace's pass removed. */
interface Pruned {
  steps: number;
  attachments: number;
  bytes: number;
}

/** The retention implementation. */
export class Service implements ServicePort {
  readonly deps: ResolvedDeps;
  readonly settings: Config;

  constructor(deps: Deps, config: Config = {}) {
    this.deps = {
      repository: deps.repository ?? null,
      steps: deps.steps ?? null,
      attachments: deps.attachments ?? null,
      logger: deps.logger ?? nopLogger,
      clock: deps.clock ?? (() => new Date()),
      erasures: deps.erasures ?? null,
      workspaces: deps.workspaces ?? null,
      objects: deps.objects ?? null,
    };
    this.settings = config;
  }

  /**
   * What the deployment's settings mean in the terms the pass works in.
   *
   * A zero attachment window means "keep them with their message" — it is not
   * a window of zero seconds, which would delete every attachment on the next
   * pass. The distinction is the whole reason the value is expressed as a
   * duration rather than as a day count.
   */
  policy(): Policy {
    const window = this.settings.stepWindow ?? 0;
    const limit = this.settings.limit ?? 0;

    return {
      stepWindow: window > 0 ? window : DefaultStepWindow,
      attachmentWindow: this.settings.attachmentWindow ?? 0,
      limit: limit > 0 ? limit : DefaultLimit,
    };
  }

  /** Makes one pass over every workspace that is due. */
  async run(signal?: AbortSignal): Promise<RunResult> {
    const result: RunResult = {
      workspaces: 0,
      steps_pruned: 0,
      attachments_pruned: 0,
      bytes_freed: 0,
    };

    const repository = this.deps.repository;
    if (!repository) throw ErrNoRepository;

    const now = this.deps.clock();

    // The run is opened before any pruning: a pass that dies half way leaves a
    // row saying it started and never finished, which is exactly the state an
    // operator needs to see.
    const runID = await repository.startRun();

    let due: string[];
    try {
      due = await repository.dueTargets(now, this.sweepLimit());
    } catch (err) {
      // A pass that cannot even read its queue failed as a whole, so it has
      // nothing to report per workspace and the run is closed with the reason.
      await this.close(runID, result, asError(err));
      throw err;
    }

    let failure: Error | null = null;

    for (const workspaceId of due) {
      if (signal?.aborted) {
        failure = asError(signal.reason ?? new Error("retention: the pass was cancelled"));
        break;
      }

      const scope: Scope = { workspaceId };
      result.workspaces++;

      const pruned = await this.pruneWorkspace(scope, now);
      result.steps_pruned += pruned.out.steps;
      result.attachments_pruned += pruned.out.attachments;
      result.bytes_freed += pruned.out.bytes;
      if (pruned.failure) {
        // One workspace must not stop the pass, and it must not stop the run
        // from being closed either — the other workspaces are waiting.
        failure = pruned.failure;
        this.warn("could not prune a workspace's content", workspaceId, pruned.failure);
      }

      // The next pass is scheduled whether or not this one worked, so a
      // workspace that failed is retried on the interval rather than in a tight
      // loop — and a pass that never ran is caught by the next one rather than
      // lost.
      try {
        await repository.touchTarget(scope, new Date(now.getTime() + this.interval()));
      } catch (err) {
        this.warn("could not schedule a workspace's next prune", workspaceId, asError(err));
      }
    }

    await this.close(runID, result, failure);

    if (failure) throw new RetentionRunError(result, failure);
    return result;
  }

  /**
   * Prunes one workspace's steps and attachments.
   *
   * The two are independent: a deployment may run with a storage driver but no
   * attachment policy, or the reverse, and one of them failing must not skip
   * the other. So each is attempted and its error collected, rather than the
   * first failure returning.
   */
  private async pruneWorkspace(
    scope: Scope,
    now: Date,
  ): Promise<{ out: Pruned; failure: Error | null }> {
    const policy = this.policy();
    const out: Pruned = { steps: 0, attachments: 0, bytes: 0 };
    const failures: Error[] = [];

    if (this.deps.steps && policy.stepWindow > 0) {
      try {
        const steps = await this.deps.steps.pruneSteps(
          scope,
          new Date(now.getTime() - policy.stepWindow),
          policy.limit,
        );
        out.steps = steps.count;
        if (steps.remaining > 0) {
          // Not a failure, and worth saying: a workspace with more past the
          // window than one batch removes is a deployment whose batch is too
          // small for its history, and the backlog only grows.
          this.warn("a workspace still has prunable steps after its pass", scope.workspaceId, null, {
            remaining: steps.remaining,
            limit: policy.limit,
          });
        }
      } catch (err) {
        failures.push(asError(err));
      }
    }

    if (this.deps.attachments && policy.attachmentWindow > 0) {
      try {
        const attachments = await this.deps.attachments.pruneAttachments(
          scope,
          new Date(now.getTime() - policy.attachmentWindow),
          policy.limit,
        );
        out.attachments = attachments.count;
        out.bytes = attachments.bytesFreed;
      } catch (err) {
        failures.push(asError(err));
      }
    }

    if (failures.length === 0) return { out, failure: null };
    return {
      out,
      failure: failures.length === 1 ? failures[0] : new AggregateError(failures, "retention: prune failed"),
    };
  }

  /**
   * Writes what the run did, and reports a failure it could not record.
   *
   * A run log that cannot be written is not swallowed: it is the only durable
   * trace the job leaves, so losing it is a failure of the job itself.
   */
  private async close(runID: number, result: RunResult, failure: Error | null): Promise<void> {
    try {
      await this.deps.repository?.finishRun(runID, result, failure);
    } catch (err) {
      this.warn("could not record the retention run", null, asError(err));
    }
  }

  /** Takes the deployment's schedule or the default. */
  private interval(): number {
    const configured = this.settings.interval ?? 0;
    return configured > 0 ? configured : DefaultInterval;
  }

  /** Takes the deployment's bound or the default. */
  private sweepLimit(): number {
    const configured = this.settings.sweepLimit ?? 0;
    return configured > 0 ? configured : DefaultSweepLimit;
  }

  /** Logs one failure, and stays quiet when there is no logger to write to. */
  warn(
    message: string,
    workspaceId: string | null,
    err: Error | null,
    fields: Record<string, unknown> = {},
  ): void {
    const payload: Record<string, unknown> = { ...fields };
    if (workspaceId) payload.workspace = workspaceId;
    if (err) payload.err = err;
    this.deps.logger.warn(payload, message);
  }

  // ---------------------------------------------------------------- erasure

  /** Removes a workspace's data on request. */
  async erase(request: ErasureRequest): Promise<ErasureResult> {
    return erase(this, request);
  }

  /** Returns the newest erasures, which is what an auditor reads. */
  async erasures(limit: number): Promise<Erasure[]> {
    return erasures(this, limit);
  }

  /** Answers whether one workspace's data was erased. */
  async findErasure(workspaceId: string): Promise<Erasure> {
    return findErasure(this, workspaceId);
  }
}

/** Coerces anything thrown into an `Error`. */
export function asError(err: unknown): Error {
  return err instanceof Error ? err : new Error(String(err));
}

/**
 * Runs the retention pass on an interval until the signal aborts.
 *
 * Ported from the container's `RunRetention`: it is a loop in the process
 * rather than a durable workflow, because nothing in it waits, and every state
 * it looks at is derived from the times and the contents.
 * `retention_targets.next_prune_at` is what makes that true — a pass that never
 * ran is still a due pass, and it is picked up as soon as the process is back.
 *
 * An interval of zero disables the job, which is what local development uses.
 */
export async function runRetention(
  service: ServicePort,
  options: { interval: number; signal?: AbortSignal; logger?: Logger | null },
): Promise<void> {
  const log = options.logger ?? nopLogger;
  if (options.interval <= 0) {
    log.info("retention is disabled");
    return;
  }

  const pass = async (): Promise<void> => {
    try {
      const result = await service.run(options.signal);
      if (result.workspaces === 0) return;
      log.info(result, "the retention pass ran");
    } catch (err) {
      if (options.signal?.aborted) return;
      // A pass that failed part way still reports what it managed, so the log
      // line says both.
      const result = err instanceof RetentionRunError ? err.result : null;
      log.error({ ...result, err }, "the retention pass failed");
    }
  };

  await pass();

  await new Promise<void>((resolve) => {
    const timer = setInterval(() => {
      void pass();
    }, options.interval);
    options.signal?.addEventListener(
      "abort",
      () => {
        clearInterval(timer);
        resolve();
      },
      { once: true },
    );
  });
}
