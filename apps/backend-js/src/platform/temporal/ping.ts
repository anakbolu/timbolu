/**
 * The worker connectivity probe.
 *
 * Ported from apps/backend/internal/platform/temporal/ping.go. The names are
 * frozen: a running deployment replays and schedules by name, and the Go worker
 * registers these functions under their Go identifiers, so the TypeScript
 * registrations carry the same strings explicitly.
 */

import { proxyActivities, type ActivityOptions } from "@temporalio/workflow";

/** The name the Go worker registers the workflow under. */
export const WORKER_PING_WORKFLOW = "WorkerPingWorkflow";

/** The name the Go worker registers the activity under. */
export const WORKER_PING_ACTIVITY = "WorkerPingActivity";

const activityOptions: ActivityOptions = {
  startToCloseTimeout: "10s",
};

/**
 * Runs one activity on the worker's own task queue and returns the namespace it
 * ran against. Deployment verification and the Temporal testsuite both use it,
 * so a worker can be proven reachable without running an agent task.
 */
export async function WorkerPingWorkflow(name: string): Promise<string> {
  const { WorkerPingActivity: ping } = proxyActivities<{
    WorkerPingActivity: (name: string) => Promise<string>;
  }>(activityOptions);

  return ping(name);
}

/** Answers the probe with the worker identity that ran it. */
export async function WorkerPingActivity(name: string): Promise<string> {
  const who = name === "" ? "worker" : name;
  return `${who} pong`;
}
