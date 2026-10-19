/**
 * The durable half of a routine run: one workflow per tick, started by the
 * routine's schedule or by an event.
 *
 * It is deliberately thin. The schedule decides *when* a routine runs and
 * Temporal owns that; this workflow decides *what happens* on one tick, and it
 * does so by calling exactly one activity. Everything that touches a database,
 * a queue, or a clock lives in that activity, because a workflow replayed from
 * its history cannot do any of those.
 *
 * Ported 1:1 from internal/modules/routine/workflow/workflow.go.
 */

import { proxyActivities } from "@temporalio/workflow";
import type { RunInput } from "../domain/service.ts";

/**
 * What the worker registers and what the scheduler starts.
 *
 * The exported workflow function is named exactly this, because the TypeScript
 * worker registers a workflow by its function name while the scheduler starts
 * one by this string. They must agree.
 */
export const WorkflowName = "RoutineRunWorkflow";

/**
 * The activity name, referenced by string so the worker registers it without
 * the workflow sharing a function value with it.
 */
export const ActivityRun = "RunRoutine";

/** Bounds one tick, in milliseconds. The work itself becomes a task. */
const runTimeoutMs = 2 * 60 * 1000;

/** What one tick did. Field names are Go's, which is what the history carries. */
export interface RunResult {
  /** The run that was recorded. */
  RunID: string;
  /** `running` when a task was opened, `skipped`, or `failed`. */
  Status: string;
  /** Explains a skip or a failure in the user's words. */
  Summary: string;
}

/** The activity contract this workflow calls, named as the worker registers it. */
export interface RoutineActivities {
  RunRoutine(input: RunInput): Promise<RunResult>;
}

/**
 * Performs one tick of one routine.
 *
 * It returns the tick's outcome rather than retrying forever: a tick that could
 * not dispatch its task has recorded a failed run, and the *next* tick is the
 * schedule's job. That is what "a failure does not stop the following run"
 * means in practice.
 */
export async function RoutineRunWorkflow(input: RunInput): Promise<RunResult> {
  const activities = proxyActivities<RoutineActivities>({
    startToCloseTimeout: runTimeoutMs,
    // One attempt. The activity records a failed run when it cannot dispatch,
    // so retrying it would open a second run for one tick.
    retry: { maximumAttempts: 1 },
  });

  return activities.RunRoutine(input);
}
