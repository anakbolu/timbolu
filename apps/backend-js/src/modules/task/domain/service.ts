/**
 * The task runtime use case contract.
 *
 * Ported 1:1 from internal/modules/task/domain/service.go.
 */

import type { Filter, Scope, Step, Task } from "./task.ts";

/** The task runtime use case contract. */
export interface Service {
  /**
   * Opens a task for a trigger and hands it to the durable runtime. It returns
   * as soon as the workflow owns the task: the work happens out of process,
   * which is what makes it survive the request and a restart.
   */
  dispatch(ctx: AbortSignal | undefined, scope: Scope, req: DispatchRequest): Promise<Task>;
  get(ctx: AbortSignal | undefined, scope: Scope, id: string): Promise<Task>;
  /** Returns the tasks of the workspace, newest first. */
  history(ctx: AbortSignal | undefined, scope: Scope, filter: Filter): Promise<Task[]>;
  /** Returns a task's recorded rounds, in order. */
  steps(ctx: AbortSignal | undefined, scope: Scope, taskId: string): Promise<Step[]>;
  /** Returns the tasks this one handed work to. */
  children(ctx: AbortSignal | undefined, scope: Scope, taskId: string): Promise<Task[]>;
  /** Stops a task that has not finished. */
  cancel(ctx: AbortSignal | undefined, scope: Scope, id: string): Promise<Task>;
}

/** One task to open. */
export interface DispatchRequest {
  agentId: string;
  /** The thread the task answers, when it came from chat. */
  conversationId: string;
  /** The placeholder the answer fills. */
  replyMessageId: string;
  trigger: string;
  title: string;
  /** The text the task was asked to act on. */
  prompt: string;
  /** Set when another task handed this work over. */
  parentTaskId: string;
  /** How far down a handoff chain this task is. */
  depth: number;
}

/**
 * What the workflow needs to begin.
 *
 * Every field is an id: the workflow loads the task, the Bolu, and the
 * conversation itself, so a restart re-reads the current state rather than
 * replaying a snapshot taken at start.
 */
export interface StartInput {
  taskId: string;
  agentId: string;
  conversationId: string;
  replyMessageId: string;
  title: string;
  prompt: string;
  depth: number;
}

/** Identifies the running workflow. */
export interface StartResult {
  workflowId: string;
  runId: string;
}

/**
 * Starts the durable workflow that runs a task.
 *
 * It is a port rather than a direct call because the engine is infrastructure:
 * the service decides what a task is, and this decides where it runs.
 */
export interface Starter {
  start(ctx: AbortSignal | undefined, scope: Scope, input: StartInput): Promise<StartResult>;
  /** Delivers a decision to a running task. */
  signal(
    ctx: AbortSignal | undefined,
    scope: Scope,
    taskId: string,
    name: string,
    payload: unknown,
  ): Promise<void>;
}
