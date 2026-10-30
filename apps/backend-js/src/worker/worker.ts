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
import type { Container } from "../container/container.ts";
import {
  agentDirectoryForTask,
  memoryRecallForTask,
  taskAttachmentReader,
  taskBudget,
  taskDraftGate,
  taskEventSink,
  taskOutcomeSink,
  taskReplyWriter,
  taskToolExecutor,
  taskToolRegistry,
} from "../container/adapters.ts";

import { Activities as TaskActivities } from "../modules/task/activity/activity.ts";
import { PostgresRepository as TaskRepository } from "../modules/task/repository/postgres.ts";
import { Activities as MemoryActivities } from "../modules/memory/activity/activity.ts";
import { Activities as RoutineActivities } from "../modules/routine/activity/activity.ts";
import { fromConfig as vcomputerActivities } from "../modules/vcomputer/temporal/activities.ts";

/** One registered activity: Temporal calls it with the workflow's arguments. */
export type Activity = (...args: never[]) => unknown;

/** Collects the public methods of an activities instance into a name → function map. */
export function activityMap(instance: object): Record<string, Activity> {
  const out: Record<string, Activity> = {};
  let proto = Object.getPrototypeOf(instance);
  while (proto && proto !== Object.prototype) {
    for (const name of Object.getOwnPropertyNames(proto)) {
      if (name === "constructor") continue;
      const value = (instance as Record<string, unknown>)[name];
      if (typeof value === "function" && !(name in out)) {
        out[name] = value.bind(instance) as Activity;
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
  /** The process's assembly point: every port an activity needs is built from it. */
  container: Container;
}

/**
 * Builds every activity the two workers register. A module without a database is
 * skipped: its activities are then absent, and a workflow that reaches for one
 * fails with a clear "activity not registered" rather than a null dereference.
 */
export function buildActivities(deps: WorkerDeps): Record<string, Activity> {
  const activities: Record<string, Activity> = {};
  const c = deps.container;

  if (deps.db) {
    const repo = new TaskRepository(deps.db);

    // The task runtime's ports. Each reads the service that owns the fact, and
    // each is null when that service is absent — the activities then report the
    // dependency as not configured instead of dereferencing nothing.
    const agents = agentDirectoryForTask(c.agentService);
    const tools = taskToolRegistry(c.agentService, c.integrationService);

    Object.assign(
      activities,
      activityMap(
        new TaskActivities({
          tasks: repo,
          steps: repo,
          agents,
          namer: agents,
          tools,
          executor: taskToolExecutor(c.integrationService, tools),
          drafts: taskDraftGate(c.approvalService),
          memory: memoryRecallForTask(c.memoryService),
          gateway: c.llmService,
          costs: c.costs,
          budget: taskBudget(c.llmService),
          events: c.chatService ? taskEventSink(c.chatService) : null,
          reply:
            c.chatService && c.chatRepo
              ? taskReplyWriter(c.chatRepo, c.chatService)
              : null,
          attachments: taskAttachmentReader(c.chatService),
          attachmentLimits: {
            maxFileBytes: deps.config.attachmentContext.maxFileBytes,
            maxCharsPerFile: deps.config.attachmentContext.maxCharsPerFile,
            maxCharsTotal: deps.config.attachmentContext.maxCharsTotal,
            maxImages: deps.config.attachmentContext.maxImages,
            maxImageBytes: deps.config.attachmentContext.maxImageBytes,
          },
          outcomes: taskOutcomeSink(c.routineService),
          notifications: c.pushService,
          limits: {
            MaxSteps: deps.config.tasks.maxSteps,
            MaxTokens: deps.config.tasks.maxTokens,
            MaxHandoffDepth: deps.config.tasks.maxHandoffDepth,
            MaxToolResultBytes: deps.config.tasks.maxToolResultBytes,
            MaxRevisions: 0,
          },
          logger: deps.log,
        }),
      ),
    );

    // Memory extraction and routine runs ride on the same worker as the task
    // runtime. Each reads its module's service; the service, not the database,
    // is what knows how to do the work.
    Object.assign(
      activities,
      activityMap(new MemoryActivities({ memories: c.memoryService, logger: deps.log })),
    );
    Object.assign(
      activities,
      activityMap(new RoutineActivities({ routines: c.routineService, logger: deps.log })),
    );
  }

  // The virtual computer needs no database: it is an HTTP client to the control
  // plane, and an unconfigured address makes every activity answer
  // "not configured" — which is the documented behaviour, not an error.
  Object.assign(activities, activityMap(vcomputerActivities(deps.config.vComputer, deps.log)));

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
