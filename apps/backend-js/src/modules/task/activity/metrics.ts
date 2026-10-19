/**
 * The task runtime's own measures: how much work the deployment accepted, what
 * it cost, and how often an attempt had to be repeated.
 *
 * Two of these are here rather than anywhere else because this is where the fact
 * is born. A retry is only visible to the attempt that *is* the retry, and a
 * draft's readiness is only known to the step that produced it.
 *
 * Ported 1:1 from internal/modules/task/activity/metrics.go. The recorder itself
 * is a port (`domain.Metrics`): this file only decides *when* a measurement is
 * taken.
 */

import * as activity from "@temporalio/activity";
import type { Metrics, Task } from "../domain/task.ts";
import { activityContextActive } from "./tracing.ts";

/**
 * Records an activity attempt past the first.
 *
 * The attempt count comes from Temporal rather than from a tally here: a retry
 * caused by a worker that died mid-activity never re-enters this code as a first
 * attempt, so counting "how many times have I been called" would miss exactly the
 * case that matters.
 */
export function retry(metrics: Metrics | null | undefined, name: string): void {
  if (!metrics || !activityContextActive()) {
    return;
  }

  const attempt = activity.Context.current().info.attempt;
  if (attempt > 1) {
    metrics.activityRetry(undefined, name, attempt);
  }
}

/**
 * How long a task ran.
 *
 * It prefers the two stored moments over any clock read here, because a task
 * spans processes and restarts: the row's own start and finish are the run an
 * operator is looking at.
 */
export function durationOf(task: Task, fallback: Date): number {
  if (task.startedAt === null) {
    return 0;
  }

  const finished = task.finishedAt ?? fallback;
  if (finished.getTime() < task.startedAt.getTime()) {
    return 0;
  }

  return (finished.getTime() - task.startedAt.getTime()) / 1000;
}

/**
 * Names the integration a tool belongs to, from the tool's own name.
 *
 * Tool names are `<app>.<operation>` by convention, which the registry enforces
 * rather than a caller: the name is a constant in the code, so the prefix is a
 * bounded value and safe as a label.
 */
export function appOfTool(name: string): string {
  const dot = name.indexOf(".");
  if (dot > 0) {
    return name.slice(0, dot);
  }
  return name;
}
