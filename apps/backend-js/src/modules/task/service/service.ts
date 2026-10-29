/**
 * The task runtime use cases: opening a task from a trigger, reading its history
 * and steps, and stopping one that has not finished.
 *
 * The engine is a port. This package decides what a task is — its trigger, its
 * bounds — and where it runs is the container's business, which is what makes
 * dispatch testable without Temporal.
 *
 * Ported 1:1 from internal/modules/task/service/service.go.
 */

import type { Logger } from "../../../platform/logger.ts";
import { nopLogger } from "../../../platform/logger.ts";
import type {
  AgentDirectory,
  AgentRef,
  Budget,
  EventSink,
  Filter,
  Finish,
  Limits,
  Metrics,
  NewTask,
  Scope,
  Step,
  StepRepository,
  Task,
  TaskRepository,
} from "../domain/task.ts";
import {
  AgentStateEvent,
  ErrDepthLimitReached,
  ErrInvalidInput,
  ErrUnknownTrigger,
  StatusCanceled,
  StatusFailed,
  StatusQueued,
  StatusRunning,
  StatusSucceeded,
  StatusWaitingApproval,
  TriggerChat,
  TriggerRoutine,
  TriggerWebhook,
  normaliseLimits,
  scopeIsZero,
  taskLive,
  wrap,
} from "../domain/task.ts";
import type { DispatchRequest, Service as ServicePort, Starter } from "../domain/service.ts";
import { Payload, taskEvent } from "./wire.ts";

/** Bounds a task title, which is shown in the feed. */
export const TitleMaxRunes = 160;

/**
 * The set a client may open a task with. `handoff` is not in it: a chain of
 * handoffs is opened by the runtime, and letting a client claim that trigger
 * would make the chain look like something a person asked for.
 */
export const TriggerWhitelist: string[] = [TriggerChat, TriggerRoutine, TriggerWebhook];

/** The service dependencies. */
export interface Deps {
  tasks: TaskRepository;
  steps: StepRepository;
  starter?: Starter | null;
  /** Validates the Bolu the task runs as. Optional. */
  agents?: AgentDirectory | null;
  /** Publishes what a task is doing. Optional. */
  events?: EventSink | null;
  /** Refuses a new task a workspace can no longer afford. */
  budget?: Budget | null;
  limits?: Limits;
  logger?: Logger | null;
  clock?: (() => Date) | null;
  /** Reports what this deployment accepted. Optional. */
  metrics?: Metrics | null;
}

/** The task runtime implementation. */
export class Service implements ServicePort {
  private readonly limits: Limits;
  private readonly clock: () => Date;

  constructor(private readonly deps: Deps) {
    this.limits = normaliseLimits(deps.limits ?? emptyLimits());
    this.clock = deps.clock ?? (() => new Date());
  }

  /** Opens a task for a trigger and hands it to the durable runtime. */
  async dispatch(
    ctx: AbortSignal | undefined,
    scope: Scope,
    req: DispatchRequest,
  ): Promise<Task> {
    requireScope(scope);
    if (!req.agentId) {
      throw wrap(ErrInvalidInput, "agent id is required");
    }
    if (!clientTrigger(req.trigger)) {
      throw wrap(ErrUnknownTrigger, JSON.stringify(req.trigger));
    }
    if (req.depth < 0 || req.depth > this.limits.MaxHandoffDepth) {
      throw wrap(ErrDepthLimitReached, `depth ${req.depth}`);
    }

    let title = req.title.trim();
    if (title === "") {
      title = titleFrom(req.prompt);
    }
    if (runeCount(title) > TitleMaxRunes) {
      throw wrap(ErrInvalidInput, `title is longer than ${TitleMaxRunes} characters`);
    }

    if (this.deps.agents) {
      const agent = await this.deps.agents.agent(ctx, scope, req.agentId);
      // A resting Bolu takes no new work. The switch is the user's, and a task
      // that started anyway would contradict it.
      if (!this.deps.agents.active(agent)) {
        throw wrap(ErrInvalidInput, `${agent.Name} sedang istirahat`);
      }
    }

    // The workspace's allowance is asked before the task is opened, so a
    // workspace with nothing left is *held* rather than opened and failed.
    if (this.deps.budget) {
      await this.deps.budget.allow(ctx, scope.workspaceId ?? "");
    }

    const newTask: NewTask = {
      agentId: req.agentId,
      parentTaskId: req.parentTaskId,
      conversationId: req.conversationId,
      replyMessageId: req.replyMessageId,
      trigger: req.trigger,
      title,
      depth: req.depth,
    };
    const task = await this.deps.tasks.create(ctx, scope, newTask);

    if (!this.deps.starter) {
      // No engine: the task is recorded and left queued rather than reported as
      // started, which is the honest answer for a deployment without Temporal.
      return task;
    }

    let result;
    try {
      result = await this.deps.starter.start(ctx, scope, {
        taskId: task.id,
        agentId: req.agentId,
        conversationId: req.conversationId,
        replyMessageId: req.replyMessageId,
        title,
        prompt: req.prompt,
        depth: req.depth,
      });
    } catch (err) {
      // A task nothing will run must not sit queued forever looking pending, so
      // the failure is recorded on the task itself.
      await this.fail(ctx, scope, task, `tidak bisa dijalankan: ${messageOf(err)}`);
      throw new Error(`task: start workflow: ${messageOf(err)}`, { cause: err });
    }

    const started = await this.deps.tasks.start(ctx, scope, task.id, result.workflowId, result.runId);

    // Counted once the engine accepted it, so the depth is work that will run
    // rather than work this call created and then failed to hand over.
    this.deps.metrics?.taskDispatched(ctx);

    await this.emitTask(ctx, scope, started, EventTaskStarted);
    return started;
  }

  /** Returns one task. */
  async get(ctx: AbortSignal | undefined, scope: Scope, id: string): Promise<Task> {
    requireScope(scope);
    if (!id) {
      throw wrap(ErrInvalidInput, "task id is required");
    }
    return this.deps.tasks.get(ctx, scope, id);
  }

  /** Returns the tasks of the workspace, newest first. */
  async history(ctx: AbortSignal | undefined, scope: Scope, filter: Filter): Promise<Task[]> {
    requireScope(scope);
    if (filter.status !== "" && !knownStatus(filter.status)) {
      throw wrap(ErrInvalidInput, JSON.stringify(filter.status));
    }
    return this.deps.tasks.list(ctx, scope, filter);
  }

  /** Returns a task's recorded rounds, in order. */
  async steps(ctx: AbortSignal | undefined, scope: Scope, taskId: string): Promise<Step[]> {
    requireScope(scope);

    // Reading the task first confirms the caller may see it: a task id from
    // another workspace answers "not found" rather than an empty list.
    await this.deps.tasks.get(ctx, scope, taskId);
    return this.deps.steps.listSteps(ctx, scope, taskId);
  }

  /** Returns the tasks this one handed work to. */
  async children(ctx: AbortSignal | undefined, scope: Scope, taskId: string): Promise<Task[]> {
    requireScope(scope);
    return this.deps.tasks.children(ctx, scope, taskId);
  }

  /**
   * Stops a task that has not finished.
   *
   * The status is written first and the running workflow is told second. A
   * worker that is down therefore cannot leave a task running: the workflow
   * reads its own status after every round and stops, and the signal only makes
   * it stop sooner.
   */
  async cancel(ctx: AbortSignal | undefined, scope: Scope, id: string): Promise<Task> {
    requireScope(scope);

    const task = await this.deps.tasks.get(ctx, scope, id);
    if (!taskLive(task)) {
      throw wrap(ErrInvalidInput, "tugas sudah selesai");
    }

    const canceled = await this.deps.tasks.cancel(ctx, scope, id, "dihentikan oleh pengguna");

    if (this.deps.starter && task.workflowId !== "") {
      try {
        await this.deps.starter.signal(ctx, scope, id, SignalCancel, null);
      } catch (err) {
        this.log().warn(
          { task_id: id, err: messageOf(err) },
          "could not tell the running task it was canceled; its status will stop it",
        );
      }
    }

    await this.emitTask(ctx, scope, canceled, EventTaskFinished);
    // Canceling is the other way a run stops, and it stops from the API process
    // rather than from the worker: the roster is told here so a Bolu whose
    // workflow is already gone does not read as busy forever.
    await this.emitAgentState(ctx, scope, canceled);
    return canceled;
  }

  /** Records a task that could not be dispatched. */
  private async fail(ctx: AbortSignal | undefined, scope: Scope, task: Task, reason: string): Promise<void> {
    const finish: Finish = {
      taskId: task.id,
      status: StatusFailed,
      summary: "",
      stoppedReason: reason,
      inputTokens: 0,
      outputTokens: 0,
      costMicros: 0,
      stepCount: 0,
    };

    let failed: Task;
    try {
      failed = await this.deps.tasks.finish(ctx, scope, finish);
    } catch (err) {
      this.log().error(
        { task_id: task.id, err: messageOf(err) },
        "could not record a task that failed to start",
      );
      return;
    }

    await this.emitTask(ctx, scope, failed, EventTaskFinished);
    // The row was queued for a moment, which the derived status counts as work in
    // flight: the roster is told it is not.
    await this.emitAgentState(ctx, scope, failed);
  }

  /** Publishes a task's state, which is how the feed learns about it without polling. */
  private async emitTask(
    ctx: AbortSignal | undefined,
    scope: Scope,
    task: Task,
    eventType: string,
  ): Promise<void> {
    if (!this.deps.events) return;

    const payload = Payload(task, this.clock());
    try {
      await this.deps.events.emit(ctx, taskEvent(scope, task, eventType, payload));
    } catch (err) {
      // Not fatal: the event is written to the store first, so a publish that
      // fails costs the live views latency and never the event.
      this.log().warn({ type: eventType, err: messageOf(err) }, "task event published only to the store");
    }
  }

  /**
   * Publishes the state the registry derives for the Bolu that owns the task, so
   * an open roster follows the run without reloading.
   *
   * `task.finished` moves the feed and the task list; a Bolu's own state is read
   * from `agent.state`, which the chat module publishes when it hands a message
   * over. The runtime owes the same event on the way out, or a finished run leaves
   * the header saying "sedang bekerja" (#315).
   */
  private async emitAgentState(ctx: AbortSignal | undefined, scope: Scope, task: Task): Promise<void> {
    if (!this.deps.agents || !this.deps.events) return;

    let agent: AgentRef;
    try {
      agent = await this.deps.agents.agent(ctx, scope, task.agentId);
    } catch (err) {
      this.log().warn(
        { task_id: task.id, err: messageOf(err) },
        "could not read the Bolu's state for the stream",
      );
      return;
    }

    try {
      await this.deps.events.emit(ctx, AgentStateEvent(scope, task, agent.Display));
    } catch (err) {
      this.log().warn({ task_id: task.id, err: messageOf(err) }, "agent state published only to the store");
    }
  }

  private log(): Logger {
    return this.deps.logger ?? nopLogger;
  }
}

export type { Service as ServicePort } from "../domain/service.ts";

/** The signal name that stops a running task. */
export const SignalCancel = "cancel";

/** Event types the feed and the chat read. */
export const EventTaskStarted = "task.started";
export const EventTaskStep = "task.step";
export const EventTaskFinished = "task.finished";

function emptyLimits(): Limits {
  return {
    MaxSteps: 0,
    MaxTokens: 0,
    MaxHandoffDepth: 0,
    MaxToolResultBytes: 0,
    MaxRevisions: 0,
  };
}

function messageOf(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

function requireScope(scope: Scope): void {
  if (scopeIsZero(scope)) {
    throw wrap(ErrInvalidInput, "workspace is required");
  }
}

/**
 * Reports whether a caller may open a task with this trigger.
 *
 * `handoff` is deliberately not in the set: a chain of handoffs is opened by the
 * runtime, through the repository, and a client that could claim the trigger
 * would make the chain look like something a person asked for.
 */
function clientTrigger(trigger: string): boolean {
  return TriggerWhitelist.includes(trigger);
}

function knownStatus(status: string): boolean {
  switch (status) {
    case StatusQueued:
    case StatusRunning:
    case StatusWaitingApproval:
    case StatusSucceeded:
    case StatusFailed:
    case StatusCanceled:
      return true;
    default:
      return false;
  }
}

/** Counts code points, as Go's `len([]rune(…))` does. */
function runeCount(value: string): number {
  return Array.from(value).length;
}

/**
 * Takes a task's title from the text it was asked to act on, which is what a
 * user recognises in the feed.
 */
function titleFrom(prompt: string): string {
  const trimmed = prompt.trim();
  if (trimmed === "") {
    return "Tugas baru";
  }

  const newline = trimmed.indexOf("\n");
  const line = newline >= 0 ? trimmed.slice(0, newline) : trimmed;

  const title = line.trim();
  const runes = Array.from(title);
  if (runes.length <= TitleMaxRunes) {
    return title;
  }
  return `${runes.slice(0, TitleMaxRunes - 1).join("")}…`;
}
