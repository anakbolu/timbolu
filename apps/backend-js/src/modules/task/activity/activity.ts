/**
 * The side-effecting half of the task runtime: everything that touches a
 * database, a model, or an integration.
 *
 * Each method is a plain function the worker registers, so a test registers a
 * stub under the same name and the workflow cannot tell the difference. The
 * workflow never calls any of this directly: it goes through Temporal, so a
 * restart re-runs the activities that did not finish and skips the ones that did.
 *
 * Ported 1:1 from internal/modules/task/activity/activity.go.
 */

import { ApplicationFailure } from "@temporalio/common";
import * as attachtext from "../../../platform/attachtext/attachtext.ts";
import type { ChatRequest, ChatResponse, Message as LLMMessage, Tool as LLMTool } from "../../../platform/llm/registry.ts";
import { ErrUnsupported, isLLMError, PurposeAgent } from "../../../platform/llm/registry.ts";
import { ToolChoiceAuto } from "../../llm/domain/provider.ts";
import type { Logger } from "../../../platform/logger.ts";
import type { Tracer } from "../../../platform/telemetry/telemetry.ts";
import { SpanStatusCode } from "../../../platform/telemetry/telemetry.ts";
import type {
  AgentDirectory,
  AgentNamer,
  AttachmentReader,
  Budget,
  CostTable,
  Draft,
  DraftExecution,
  DraftGate,
  EventSink,
  Gateway,
  Limits,
  MemoryRecall,
  Metrics,
  OutcomeSink,
  ReplyWriter,
  Scope,
  StepRepository,
  Task,
  TaskRepository,
  ToolExecutor,
  ToolRegistry,
} from "../domain/task.ts";
import {
  DraftCanceled,
  ErrDraftNoSender,
  ErrDraftNotApproved,
  ErrInvalidInput,
  ErrOutcomeUnclaimed,
  ErrToolNotAvailable,
  StatusCanceled,
  StatusFailed,
  StatusSucceeded,
  StatusWaitingApproval,
  StepFinal,
  StepToolCall,
  TriggerHandoff,
  is,
  normaliseLimits,
  wrap,
} from "../domain/task.ts";
import { RenderBlocks, WireID } from "../domain/render.ts";
import type {
  Beat,
  CompleteRequest,
  Context,
  DraftCarryOutcome,
  DraftCarryRequest,
  DraftOutcome,
  HandoffRequest,
  HandoffStarted,
  ModelReply,
  ModelRequest,
  PreparedHandoff,
  ResumeRequest,
  StepRequest,
  TaskInput,
  TaskOutcome,
  ToolOutcome,
  ToolRequest,
} from "../workflow/workflow.ts";
import {
  EventTaskFinished,
  EventTaskRunning,
  EventTaskStarted,
  EventTaskStep,
  EventTaskWaiting,
  MessageComplete,
  MessageFailed,
} from "../workflow/workflow.ts";
import { TaskEvent } from "../workflow/prompt.ts";
import { HandoffTool, draftTitle, firstLine as firstLineOf } from "./helpers.ts";
import { appOfTool, durationOf, retry } from "./metrics.ts";
import { attachmentTurns, attachMessage, carriedImages, saysNothing } from "./attachments.ts";
import { end, span, taskLogger, toolSpan } from "./tracing.ts";

/** How much of a conversation a task is given. */
const historyLimit = 30;

/** The activity dependencies. Every one is optional except the repositories. */
export interface Deps {
  tasks: TaskRepository;
  steps: StepRepository;
  agents?: AgentDirectory | null;
  tools?: ToolRegistry | null;
  executor?: ToolExecutor | null;
  drafts?: DraftGate | null;
  gateway?: Gateway | null;
  costs?: CostTable | null;
  budget?: Budget | null;
  events?: EventSink | null;
  reply?: ReplyWriter | null;
  /** Reads the content of a message's files. Optional. */
  attachments?: AttachmentReader | null;
  /** Bounds what a round's attachments contribute to the prompt. */
  attachmentLimits?: attachtext.Limits;
  memory?: MemoryRecall | null;
  outcomes?: OutcomeSink | null;
  namer?: AgentNamer | null;
  limits?: Limits;
  logger?: Logger | null;
  tracer?: Tracer | null;
  metrics?: Metrics | null;
  clock?: (() => Date) | null;
}

/** The set the worker registers. */
export class Activities {
  private readonly limits: Limits;

  constructor(private readonly deps: Deps) {
    this.limits = normaliseLimits(deps.limits ?? emptyLimits());

    // The worker reads an implementation set with `Object.entries`, which sees
    // own properties only: an unbound prototype method would be invisible and
    // every call would answer "not registered". Binding once here keeps the
    // class the thing a worker is handed.
    this.LoadTaskContext = this.LoadTaskContext.bind(this);
    this.CallModel = this.CallModel.bind(this);
    this.RunTool = this.RunTool.bind(this);
    this.RequestDraft = this.RequestDraft.bind(this);
    this.CarryOutDraft = this.CarryOutDraft.bind(this);
    this.RecordStep = this.RecordStep.bind(this);
    this.BeatTask = this.BeatTask.bind(this);
    this.CompleteTask = this.CompleteTask.bind(this);
    this.PrepareHandoff = this.PrepareHandoff.bind(this);
    this.ResumeTask = this.ResumeTask.bind(this);
    this.MarkHandoffStarted = this.MarkHandoffStarted.bind(this);
  }

  /** The activity set's clock, which a test pins. */
  now(): Date {
    return this.deps.clock ? this.deps.clock() : new Date();
  }

  /**
   * Loads everything one round needs.
   *
   * It runs once per round rather than once per task: a task that waited for an
   * approval for hours must see the persona and the tools as they are now.
   */
  async LoadTaskContext(input: TaskInput): Promise<Context> {
    const task = await this.load(input);

    const scope = scopeOf(task);
    const agents = this.deps.agents;
    if (!agents) {
      throw new Error("task: load agent: the agent registry is not configured");
    }
    const agent = await agents.agent(undefined, scope, task.agentId);

    // The tools a Bolu may call are the grant's answer, not its wish list.
    let tools = this.deps.tools ? await this.deps.tools.tools(undefined, scope, task.agentId) : [];
    // Handing work to another Bolu is part of the product rather than an
    // integration, so the tool is offered alongside them.
    tools = [...tools, HandoffTool()];

    const loaded: Context = {
      Agent: {
        ID: agent.ID,
        Name: agent.Name,
        Role: agent.Role,
        Persona: agent.Persona,
        Tone: agent.Tone,
        Stored: agent.Stored,
        Display: agent.Display,
        Tools: agent.Tools,
        DefaultModel: agent.DefaultModel,
      },
      Tools: tools,
      History: [],
      Limits: this.limits,
      Depth: task.depth,
      Memory: { Stable: "", Retrieved: "", Used: 0 },
    };

    if (task.conversationId !== "" && this.deps.reply) {
      const history = await this.deps.reply.history(undefined, scope, task.conversationId, historyLimit);
      loaded.History = history.map((message) => ({
        ID: message.ID,
        ConversationID: message.ConversationID,
        AgentID: message.AgentID,
        UserID: message.UserID,
        Blocks: message.Blocks,
        Attachments: message.Attachments,
      }));
    }

    // Memory is loaded once per task rather than per round: the retrieved block
    // then stays identical across the rounds of one task, which is what lets the
    // provider's cache hit on the second round.
    //
    // A failure here is logged rather than fatal. A Bolu that answers without its
    // memory is a worse answer; a Bolu that refuses to answer at all is broken.
    if (this.deps.memory) {
      try {
        const recall = await this.deps.memory.recall(undefined, scope, {
          agentId: task.agentId,
          taskId: task.id,
          query: recallQuery(input, loaded.History),
        });
        loaded.Memory = recall;
      } catch (err) {
        taskLogger(this.deps, task).warn(
          { err: messageOf(err) },
          "memory could not be loaded for this task; it will answer without it",
        );
      }
    }

    return loaded;
  }

  /** Performs one model call. */
  async CallModel(request: ModelRequest): Promise<ModelReply> {
    if (!this.deps.gateway) {
      throw new Error("task: the model gateway is not configured");
    }

    const task = await this.load({ TaskID: request.TaskID, WorkspaceID: request.WorkspaceID } as TaskInput);
    const scope = scopeOf(task);

    // The round's span, with the model call the gateway makes hanging under it.
    // The prompt and the answer are not attributes: the round's size and its cost
    // are what a trace is read for.
    const roundSpan = span(this.deps, "task.model_round", task, {
      history_messages: request.Messages.length,
      tool_count: request.Tools.length,
    });

    try {
      // The budget is asked before every call rather than once per task.
      if (this.deps.budget) {
        await this.deps.budget.allow(undefined, scope.workspaceId ?? "");
      }

      // The files of the round are read here rather than carried in the request.
      const attachments = await attachmentTurns(
        this.deps,
        undefined,
        scope,
        task,
        request.Messages.map((message) => ({
          Role: message.Role as "user" | "assistant" | "system" | "tool",
          Text: message.Text,
          ToolCalls: message.ToolCalls,
          ToolResults: message.ToolResults,
          Attachments: message.Attachments,
        })),
      );

      // A chain without vision refuses a request that carries a picture. Answering
      // without them is the difference between a reply that names the photo and no
      // reply at all, so it is tried once, with the pictures said to be unreadable.
      let vision = true;
      let response: ChatResponse;
      try {
        response = await this.modelCall(scope, task, request, attachments, vision);
      } catch (err) {
        if (!vision || !carriedImages(attachments) || !isLLMError(err, ErrUnsupported)) {
          throw err;
        }
        taskLogger(this.deps, task).warn(
          { err: messageOf(err) },
          "the model chain cannot read images; answering without them",
        );
        vision = false;
        response = await this.modelCall(scope, task, request, attachments, vision);
      }

      roundSpan.setAttributes({
        model: response.model,
        provider: response.provider,
        input_tokens: response.usage.inputTokens,
        output_tokens: response.usage.outputTokens,
        tool_calls: response.toolCalls.length,
      });

      return {
        Text: response.text,
        Usage: {
          InputTokens: response.usage.inputTokens,
          OutputTokens: response.usage.outputTokens,
          CacheReadTokens: response.usage.cacheReadTokens,
          CacheWriteTokens: response.usage.cacheWriteTokens,
        },
        Model: response.model,
        FinishReason: response.finishReason,
        ToolCalls: response.toolCalls.map((call) => ({
          CallID: call.id,
          Name: call.name,
          Arguments: call.arguments,
        })),
      };
    } catch (err) {
      end(roundSpan, err);
      throw err;
    }
  }

  /**
   * Executes one tool call.
   *
   * A `write_external` tool never reaches here: the workflow routes it to the
   * draft gate, so this only runs what is safe to run unattended.
   */
  async RunTool(request: ToolRequest): Promise<ToolOutcome> {
    retry(this.deps.metrics, "RunTool");

    const task = await this.load({ TaskID: request.TaskID, WorkspaceID: request.WorkspaceID } as TaskInput);
    const scope = scopeOf(task);

    // The tool's span, named for the tool rather than for the call. It opens
    // before the checks below rather than after them, so a call this deployment
    // could not serve is *visible* as a span that failed.
    const toolSpanValue = toolSpan(this.deps, task, request.Name);
    const started = this.now();
    let result = { content: undefined as unknown, isError: false };
    let failure: unknown = null;

    try {
      if (!this.deps.executor) {
        failure = wrap(ErrToolNotAvailable, request.Name);
        throw failure;
      }

      result = await this.deps.executor.execute(undefined, scope, {
        taskId: task.id,
        agentId: task.agentId,
        callId: request.CallID,
        name: request.Name,
        arguments: request.Arguments,
        conversationId: task.conversationId,
      });

      // An error the tool reported is not a failure of the runtime: the model is
      // told what went wrong and decides what to do, so the span carries the fact
      // without failing.
      const encoded = JSON.stringify(result.content ?? {});
      toolSpanValue.setAttributes({
        result_bytes: new TextEncoder().encode(encoded).length,
        is_error: result.isError,
      });
      if (result.isError) {
        toolSpanValue.setStatus({ code: SpanStatusCode.ERROR, message: "the tool reported an error" });
      }

      // A tool that answers with a megabyte would be re-sent to the model on every
      // later round, which is the costliest way to run out of tokens.
      const content =
        new TextEncoder().encode(encoded).length > this.limits.MaxToolResultBytes
          ? truncateContent(result.content, this.limits.MaxToolResultBytes)
          : result.content;

      return { Name: request.Name, Label: "", Content: content, IsError: result.isError, Draft: null };
    } finally {
      // The call is recorded however it ends: the integration travels with it,
      // and a call this deployment refused is a call that never reached the
      // provider, which is exactly what a failure chart should show.
      this.deps.metrics?.toolCall(
        undefined,
        appOfTool(request.Name),
        failure !== null || result.isError,
        (this.now().getTime() - started.getTime()) / 1000,
      );
      end(toolSpanValue, failure);
    }
  }

  /**
   * Turns a write_external call into a draft a human decides on, and parks the
   * task on it.
   */
  async RequestDraft(request: ToolRequest): Promise<DraftOutcome> {
    retry(this.deps.metrics, "RequestDraft");

    const task = await this.load({ TaskID: request.TaskID, WorkspaceID: request.WorkspaceID } as TaskInput);
    const scope = scopeOf(task);

    if (!this.deps.drafts) {
      // Without an approval gate the only safe answer is to refuse: running the
      // action directly would skip the product's central guardrail.
      return {
        DraftID: "",
        Status: DraftCanceled,
        Note: "gerbang persetujuan tidak dikonfigurasi, jadi aksi ini tidak dijalankan",
        Title: request.Name,
        Auto: false,
        Content: null,
      };
    }

    const draft: Draft = await this.deps.drafts.requestDraft(undefined, scope, {
      taskId: task.id,
      agentId: task.agentId,
      conversationId: task.conversationId,
      actionKind: request.Name,
      title: draftTitle(request),
      payload: request.Arguments,
    });

    // How long the bot took to produce something a person can read, measured from
    // the moment the task opened rather than from this round.
    if (this.deps.metrics && task.startedAt !== null) {
      this.deps.metrics.draftReady(undefined, (this.now().getTime() - task.startedAt.getTime()) / 1000);
    }

    // A policy that allowed the action unattended means there is nobody to wait for.
    if (draft.auto) {
      await this.emit(scope, task, EventTaskStep);
      return {
        DraftID: draft.id,
        Status: draft.status,
        Note: draft.note,
        Title: draft.title,
        Auto: true,
        Content: draft.content,
      };
    }

    // The task is parked in the store before the workflow waits, so a worker that
    // dies during the wait resumes the wait rather than losing the task.
    const parked = await this.deps.tasks.park(
      undefined,
      scope,
      task.id,
      `menunggu persetujuan: ${draftTitle(request)}`,
      draft.id,
    );
    await this.emit(scope, parked, EventTaskWaiting);

    return {
      DraftID: draft.id,
      Status: draft.status,
      Note: draft.note,
      Title: draft.title,
      Auto: false,
      Content: null,
    };
  }

  /**
   * Executes an action a person approved.
   *
   * It reports a hold rather than sending when the workspace is closed, which is
   * what lets the workflow wait for the window to open.
   */
  async CarryOutDraft(request: DraftCarryRequest): Promise<DraftCarryOutcome> {
    let task = await this.load({ TaskID: request.TaskID, WorkspaceID: request.WorkspaceID } as TaskInput);
    const scope = scopeOf(task);

    const draftId = request.DraftID.trim();
    if (draftId === "") {
      throw wrap(ErrInvalidInput, "draft id is required");
    }

    if (!this.deps.drafts) {
      throw nonRetryable("the approval gate is not configured", "ApprovalMissing", ErrToolNotAvailable);
    }

    let result: DraftExecution;
    try {
      result = await this.deps.drafts.carryOut(undefined, scope, draftId);
    } catch (err) {
      // A missing executor or a draft nobody approved is a state, not a hiccup:
      // retrying it would spend the retry budget reaching the same answer.
      if (is(err, ErrDraftNoSender) || is(err, ErrDraftNotApproved)) {
        throw nonRetryable(messageOf(err), "ApprovalRejected", err);
      }
      throw err;
    }

    if (result.held) {
      // A held action is not a failure and not a step: the task is waiting, and it
      // says so in the store as well as on the wire. Without the park its status
      // would stay `running` while the workflow slept for hours.
      const reason = result.reason === "" ? "menunggu jam kerja" : result.reason;
      try {
        task = await this.deps.tasks.park(undefined, scope, task.id, reason, draftId);
      } catch {
        // The park is a nicety here: the hold is still reported, and the workflow
        // still waits.
      }

      await this.emit(scope, task, EventTaskWaiting);

      return {
        Status: result.status,
        Content: null,
        Held: true,
        NotBefore: result.notBefore ? result.notBefore.toISOString() : "",
        // Measured here, where a clock may be read: the workflow itself may not
        // consult one.
        WaitMs: result.notBefore ? result.notBefore.getTime() - this.now().getTime() : 0,
        Reason: reason,
      };
    }

    await this.emit(scope, task, EventTaskStep);

    return {
      Status: result.status,
      Content: result.content,
      Held: false,
      NotBefore: "",
      WaitMs: 0,
      Reason: "",
    };
  }

  /** Writes one round. */
  async RecordStep(request: StepRequest): Promise<void> {
    const task = await this.load({ TaskID: request.TaskID, WorkspaceID: request.WorkspaceID } as TaskInput);
    const scope = scopeOf(task);

    await this.deps.steps.append(undefined, scope, {
      id: "",
      taskId: task.id,
      seq: 0,
      kind: request.Record.Kind,
      toolName: request.Record.ToolName,
      toolLabel: request.Record.ToolLabel,
      input: request.Record.Input,
      output: request.Record.Output,
      inputTokens: request.Record.InputTokens,
      outputTokens: request.Record.OutputTokens,
      pruned: false,
      createdAt: this.now(),
    });

    await this.emit(scope, task, EventTaskStep);
  }

  /**
   * Records a heartbeat and answers whether the task may keep going.
   *
   * It is the one place a bound is decided, so "the task stopped because it hit a
   * limit" has a single implementation rather than one check per bound.
   */
  async BeatTask(request: Beat): Promise<Beat> {
    const task = await this.load({ TaskID: request.TaskID, WorkspaceID: request.WorkspaceID } as TaskInput);
    const scope = scopeOf(task);

    // A canceled task is discovered here, which is what stops a task whose worker
    // was down when the user pressed cancel.
    if (task.status === StatusCanceled) {
      request.StopReason = StatusCanceled;
      request.StopDetail = task.stoppedReason;
      return request;
    }

    const tokens = request.InputTokens + request.OutputTokens;
    if (tokens > this.limits.MaxTokens) {
      request.StopReason = StatusFailed;
      request.StopDetail = `tugas berhenti karena mencapai batas token (${tokens} dari ${this.limits.MaxTokens})`;
      return request;
    }

    if (request.StepCount >= this.limits.MaxSteps) {
      request.StopReason = StatusFailed;
      request.StopDetail = `tugas berhenti karena mencapai batas langkah (${this.limits.MaxSteps})`;
      return request;
    }

    // The workspace budget is asked here as well as before each model call: a task
    // that spent the allowance mid-round must not start another.
    if (this.deps.budget) {
      try {
        await this.deps.budget.allow(undefined, scope.workspaceId ?? "");
      } catch (err) {
        request.StopReason = StatusFailed;
        request.StopDetail = `tugas berhenti: ${messageOf(err)}`;
        return request;
      }
    }

    await this.deps.tasks.progress(undefined, scope, {
      taskId: task.id,
      inputTokens: request.InputTokens,
      outputTokens: request.OutputTokens,
      costMicros: this.cost(task, request),
      stepCount: request.StepCount,
    });

    await this.emit(scope, task, EventTaskRunning);
    return request;
  }

  /** Writes the terminal state and the answer. */
  async CompleteTask(request: CompleteRequest): Promise<TaskOutcome> {
    retry(this.deps.metrics, "CompleteTask");

    const task = await this.load({ TaskID: request.TaskID, WorkspaceID: request.WorkspaceID } as TaskInput);
    const scope = scopeOf(task);

    const stored = await this.deps.tasks.finish(undefined, scope, {
      taskId: task.id,
      status: request.Status,
      summary: await this.summary(scope, task),
      stoppedReason: request.Reason,
      inputTokens: request.InputTokens,
      outputTokens: request.OutputTokens,
      costMicros: request.CostMicros,
      stepCount: request.StepCount,
    });

    // The answer is written into the conversation, which is what makes a task
    // started from chat visible where it was asked for.
    if (this.deps.reply && task.conversationId !== "") {
      const status = stored.status === StatusSucceeded ? MessageComplete : MessageFailed;
      try {
        await this.deps.reply.write(undefined, scope, {
          messageId: task.replyMessageId,
          blocks: request.Blocks,
          status,
          finishReason: stored.stoppedReason,
          taskId: stored.id,
        });
      } catch (err) {
        // A task whose answer could not be written is still finished: saying
        // otherwise would leave it live forever.
        taskLogger(this.deps, stored).warn(
          { err: messageOf(err) },
          "could not write the task's answer into the conversation",
        );
      }
    }

    // The task's own numbers, which is what the depth comes back down from. They
    // are taken from the stored row rather than from the request.
    if (this.deps.metrics) {
      this.deps.metrics.taskSettled(undefined);
      this.deps.metrics.taskFinished(
        undefined,
        stored.trigger,
        stored.status,
        durationOf(stored, this.now()),
        stored.inputTokens,
        stored.outputTokens,
        stored.costMicros,
      );
    }

    await this.emit(scope, stored, EventTaskFinished);

    // Whoever opened this task is told how it ended. A task opened by chat has no
    // claimant, and that is not a failure.
    if (this.deps.outcomes) {
      try {
        await this.deps.outcomes.report(undefined, scope, {
          taskId: stored.id,
          status: stored.status,
          summary: stored.summary,
          reason: stored.stoppedReason,
        });
      } catch (err) {
        if (!is(err, ErrOutcomeUnclaimed)) {
          taskLogger(this.deps, stored).warn({ err: messageOf(err) }, "could not report the task's outcome");
        }
      }
    }

    return { Status: stored.status, Summary: stored.summary, Reason: stored.stoppedReason, Steps: 0, Tokens: 0 };
  }

  /**
   * Opens a child task for another Bolu and returns the input its workflow starts
   * with.
   *
   * A refusal is returned as a message rather than an error, because the model is
   * the one that has to react to it.
   */
  async PrepareHandoff(request: HandoffRequest): Promise<PreparedHandoff> {
    const parent = await this.load({
      TaskID: request.ParentTaskID,
      WorkspaceID: request.WorkspaceID,
    } as TaskInput);
    const scope = scopeOf(parent);

    const refuse = (reason: string): PreparedHandoff => ({ Child: emptyInput(), Refusal: reason });

    if (request.Depth >= this.limits.MaxHandoffDepth) {
      return refuse(
        `serah-terima ditolak: rantai sudah ${request.Depth} tingkat, batasnya ${this.limits.MaxHandoffDepth}`,
      );
    }

    if (!this.deps.namer || !this.deps.agents) {
      return refuse("serah-terima ditolak: registry tidak bisa mencari Bolu berdasarkan nama");
    }
    // The lookup is by name because that is what the model knows: it is told the
    // roster, not the ids.
    let target;
    try {
      target = await this.deps.namer.byName(undefined, scope, request.AgentName);
    } catch {
      return refuse(`serah-terima ditolak: tidak ada Bolu bernama ${JSON.stringify(request.AgentName)}`);
    }
    if (!this.deps.agents.active(target)) {
      return refuse(`serah-terima ditolak: ${target.Name} sedang istirahat`);
    }

    const instructions = request.Instructions.trim();
    if (instructions === "") {
      return refuse("serah-terima ditolak: instruksinya kosong");
    }

    const child = await this.deps.tasks.create(undefined, scope, {
      agentId: target.ID,
      parentTaskId: parent.id,
      conversationId: parent.conversationId,
      replyMessageId: "",
      trigger: TriggerHandoff,
      title: firstLineOf(instructions),
      depth: request.Depth + 1,
    });

    return {
      Child: {
        TaskID: child.id,
        WorkspaceID: scope.workspaceId ?? "",
        AgentID: target.ID,
        ConversationID: WireID(parent.conversationId),
        ReplyMessageID: "",
        Title: child.title,
        Prompt: instructions,
        Depth: child.depth,
      },
      Refusal: "",
    };
  }

  /** Clears a parked task's waiting state once the decision is in. */
  async ResumeTask(request: ResumeRequest): Promise<void> {
    const task = await this.load({ TaskID: request.TaskID, WorkspaceID: request.WorkspaceID } as TaskInput);

    if (task.status !== StatusWaitingApproval) {
      // The task is no longer parked, which is not a failure: a cancel that landed
      // while the decision was in flight already finished it.
      return;
    }

    const scope = scopeOf(task);
    const resumed = await this.deps.tasks.resume(undefined, scope, task.id);
    await this.emit(scope, resumed, EventTaskRunning);
  }

  /**
   * Records the workflow a child task runs under, so the chain is traceable from
   * the parent as well as in Temporal.
   */
  async MarkHandoffStarted(request: HandoffStarted): Promise<void> {
    const child = await this.load({ TaskID: request.TaskID, WorkspaceID: request.WorkspaceID } as TaskInput);
    const scope = scopeOf(child);

    const started = await this.deps.tasks.start(undefined, scope, child.id, request.WorkflowID, request.RunID);
    await this.emit(scope, started, EventTaskStarted);
  }

  /** Reads one task inside its workspace scope. */
  private async load(input: TaskInput): Promise<Task> {
    const workspaceId = input.WorkspaceID.trim();
    if (workspaceId === "") {
      throw wrap(ErrInvalidInput, "workspace id is required");
    }
    const taskId = input.TaskID.trim();
    if (taskId === "") {
      throw wrap(ErrInvalidInput, "task id is required");
    }

    return this.deps.tasks.get(undefined, { workspaceId, userId: null }, taskId);
  }

  /** Makes one attempt at the round's model call, with or without pictures. */
  private async modelCall(
    scope: Scope,
    task: Task,
    request: ModelRequest,
    attachments: Array<attachtext.Turn | null>,
    vision: boolean,
  ): Promise<ChatResponse> {
    const gateway = this.deps.gateway;
    if (!gateway) {
      throw new Error("task: the model gateway is not configured");
    }

    return gateway.chat(
      undefined,
      { workspaceId: scope.workspaceId ?? "", agentId: task.agentId, userId: "" },
      this.chatRequest(request, task, attachments, vision),
    );
  }

  /**
   * Renders one model call.
   *
   * `attachments` is aligned with `request.Messages`: it is what each message's
   * files contribute, already read, so the rendering here is pure.
   */
  private chatRequest(
    request: ModelRequest,
    task: Task,
    attachments: Array<attachtext.Turn | null>,
    vision: boolean,
  ): ChatRequest {
    const tools: LLMTool[] = request.Tools.map((tool) => ({
      name: tool.Name,
      description: tool.Description,
      schema: tool.Schema,
      label: tool.Label,
    }));

    // A turn that says nothing is left out rather than sent as a blank message.
    const messages: LLMMessage[] = [];
    for (let index = 0; index < request.Messages.length; index++) {
      const turn = request.Messages[index];
      if (!turn) continue;

      let message: LLMMessage = {
        role: turn.Role as LLMMessage["role"],
        text: turn.Text,
        images: [],
        toolCalls: turn.ToolCalls,
        toolResults: turn.ToolResults,
      };
      const rendered = attachments[index] ?? null;
      if (rendered) {
        message = attachMessage(message, rendered, vision);
      }
      if (saysNothing(message)) {
        continue;
      }
      messages.push(message);
    }

    return {
      model: "",
      system: request.System,
      retrieved: request.Retrieved,
      messages,
      tools,
      // The model decides: some tasks are prose, some need a tool.
      toolChoice: ToolChoiceAuto,
      toolName: "",
      maxTokens: 0,
      temperature: null,
      stopSequences: [],
      metadata: {
        workspaceId: task.workspaceId,
        agentId: task.agentId,
        taskId: task.id,
        purpose: PurposeAgent,
      },
    };
  }

  /** Writes the one line that survives the raw steps. */
  private async summary(scope: Scope, task: Task): Promise<string> {
    let steps;
    try {
      steps = await this.deps.steps.listSteps(undefined, scope, task.id);
    } catch {
      return "";
    }

    let tools = 0;
    let last = "";
    for (const step of steps) {
      switch (step.kind) {
        case StepToolCall:
          tools++;
          break;
        case StepFinal:
          if (step.output !== null && typeof step.output === "object" && "text" in step.output) {
            const text = step.output.text;
            if (typeof text === "string") last = text.trim();
          }
          break;
      }
    }

    if (last !== "") return firstLineOf(last);
    if (tools > 0) return `${steps.length} langkah, ${tools} alat dipakai`;
    return `${steps.length} langkah`;
  }

  /** Prices what the task spent, so its own total is recorded rather than inferred. */
  private cost(task: Task, beat: Beat): number {
    if (!this.deps.costs) {
      return task.costMicros;
    }
    return this.deps.costs.cost(task.title, {
      inputTokens: beat.InputTokens,
      outputTokens: beat.OutputTokens,
      cacheReadTokens: 0,
      cacheWriteTokens: 0,
    });
  }

  /** Publishes a task event, which is how the feed sees it. */
  private async emit(scope: Scope, task: Task, eventType: string): Promise<void> {
    if (!this.deps.events) {
      return;
    }

    const payload = {
      task_id: task.id,
      agent_id: WireID(task.agentId),
      status: task.status,
      trigger: task.trigger,
      title: task.title,
    };

    try {
      await this.deps.events.emit(undefined, TaskEvent(scope, task, eventType, payload));
    } catch (err) {
      taskLogger(this.deps, task).warn(
        { type: eventType, err: messageOf(err) },
        "task event published only to the store",
      );
    }
  }
}

/** The query a memory search runs against. */
function recallQuery(input: TaskInput, history: Context["History"]): string {
  const prompt = input.Prompt.trim();
  if (prompt !== "") {
    return prompt;
  }

  for (let index = history.length - 1; index >= 0; index--) {
    const message = history[index];
    if (!message) continue;
    const text = RenderBlocks(message.Blocks).trim();
    if (text !== "") {
      return text;
    }
  }

  return "";
}

function scopeOf(task: Task): Scope {
  return { workspaceId: task.workspaceId, userId: null };
}

function emptyInput(): TaskInput {
  return {
    TaskID: "",
    WorkspaceID: "",
    AgentID: "",
    ConversationID: "",
    ReplyMessageID: "",
    Title: "",
    Prompt: "",
    Depth: 0,
  };
}

function emptyLimits(): Limits {
  return { MaxSteps: 0, MaxTokens: 0, MaxHandoffDepth: 0, MaxToolResultBytes: 0, MaxRevisions: 0 };
}

function messageOf(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/** Bounds a tool's answer, keeping it a parseable document. */
function truncateContent(content: unknown, limit: number): unknown {
  const encoded = JSON.stringify(content ?? {});
  const bytes = new TextEncoder().encode(encoded);
  const preview = new TextDecoder("utf-8", { fatal: false }).decode(bytes.subarray(0, limit));
  return { truncated: true, bytes: bytes.length, preview };
}

/**
 * A configuration mistake Temporal must not retry.
 *
 * A missing approval gate or a draft nobody approved answers the same way every
 * time, so the attempts would only delay the message an operator needs.
 */
function nonRetryable(message: string, type: string, cause: unknown): ApplicationFailure {
  return ApplicationFailure.nonRetryable(message, type, undefined, cause);
}
