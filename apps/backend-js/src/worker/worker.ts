/**
 * Builds the activity maps the Temporal workers register.
 *
 * Each module's `Activities` class names its methods exactly as the Go activity
 * registration names them (`LoadTaskContext`, `CallModel`, `RunTool`, …), so the
 * map is derived from the prototype rather than hand-listed — a hand-written list
 * drifts the moment an activity is added, and a missing name fails at runtime in
 * the middle of a task instead of at startup.
 */

import { NativeConnection, Worker } from "@temporalio/worker";
import type { Config } from "../platform/config/config.ts";
import type { Logger } from "../platform/logger.ts";
import type { Database } from "../platform/database/index.ts";

import { Activities as TaskActivities } from "../modules/task/activity/activity.ts";
import { PostgresRepository as TaskRepository } from "../modules/task/repository/postgres.ts";
import { Activities as MemoryActivities } from "../modules/memory/activity/activity.ts";
import { Activities as RoutineActivities } from "../modules/routine/activity/activity.ts";
import { fromConfig as vcomputerActivities } from "../modules/vcomputer/temporal/activities.ts";

/** Collects the public methods of an activities instance into a name → function map. */
export function activityMap(instance: object): Record<string, (...args: any[]) => any> {
  const out: Record<string, (...args: any[]) => any> = {};
  let proto = Object.getPrototypeOf(instance);
  while (proto && proto !== Object.prototype) {
    for (const name of Object.getOwnPropertyNames(proto)) {
      if (name === "constructor") continue;
      const value = (instance as any)[name];
      if (typeof value === "function" && !(name in out)) {
        out[name] = value.bind(instance);
      }
    }
    proto = Object.getPrototypeOf(proto);
  }
  return out;
}

export interface WorkerDeps {
  config: Config;
  log: Logger;
  db: Database | null;
}

/**
 * Builds every activity the two workers register. A module without a database is
 * skipped: its activities are then absent, and a workflow that reaches for one
 * fails with a clear "activity not registered" rather than a null dereference.
 */
export function buildActivities(deps: WorkerDeps): Record<string, (...args: any[]) => any> {
  const activities: Record<string, (...args: any[]) => any> = {};

  if (deps.db) {
    const repo = new TaskRepository(deps.db);
    Object.assign(
      activities,
      activityMap(
        new TaskActivities({
          tasks: repo,
          steps: repo,
          limits: {
            maxSteps: deps.config.tasks.maxSteps,
            maxTokens: deps.config.tasks.maxTokens,
            maxHandoffDepth: deps.config.tasks.maxHandoffDepth,
            maxToolResultBytes: deps.config.tasks.maxToolResultBytes,
          } as any,
          logger: deps.log,
        } as any),
      ),
    );

    Object.assign(activities, activityMap(new MemoryActivities({ db: deps.db, config: deps.config, log: deps.log } as any)));
    Object.assign(activities, activityMap(new RoutineActivities({ db: deps.db, config: deps.config, log: deps.log } as any)));
  }

  // The virtual computer needs no database: it is an HTTP client to the control
  // plane, and an unconfigured address makes every activity answer
  // "not configured" — which is the documented behaviour, not an error.
  Object.assign(activities, activityMap(vcomputerActivities(deps.config.vComputer as any, deps.log)));

  return activities;
}

/** Opens a worker connection and returns a Worker that has not started yet. */
export async function newWorker(
  deps: WorkerDeps,
  taskQueue: string,
  workflowsPath: string,
): Promise<Worker> {
  const address = deps.config.temporal.hostPort || "localhost:7233";
  const connection = await NativeConnection.connect({
    address,
    ...(deps.config.temporal.tls ? { tls: true } : {}),
    ...(deps.config.temporal.apiKey ? { apiKey: deps.config.temporal.apiKey } : {}),
  });

  return Worker.create({
    connection,
    namespace: deps.config.temporal.namespace || "default",
    taskQueue,
    workflowsPath,
    activities: buildActivities(deps),
    // A workflow that calls an activity the worker did not register is a
    // deployment mistake; failing loudly at startup beats failing mid-task.
    maxCachedWorkflows: 100,
  });
}
