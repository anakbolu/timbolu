/**
 * The durable agent loop: one task is one Temporal workflow that calls the
 * model, runs the tools it asks for, records every round, and stops on a bound
 * rather than hanging.
 *
 * This file is deliberately free of I/O. Everything that touches the network, a
 * database, or the clock happens in an activity, because a workflow is replayed
 * from its history after a worker restart: a non-deterministic step would make
 * the replay disagree with the run and the task would be stuck forever.
 *
 * Ported 1:1 from internal/modules/task/workflow/workflow.go.
 */

import {
  condition,
  defineSignal,
  proxyActivities,
  setHandler,
  sleep,
  startChild,
  type ActivityOptions,
  type RetryPolicy,
} from "@temporalio/workflow";
import type { AgentRef, Block, Tool } from "../domain/task.ts";
import {
  DraftCanceled,
  LabelRead,
  StatusCanceled,
  StatusFailed,
  StatusSucceeded,
  StepDraft,
  StepFinal,
  StepThink,
  StepToolCall,
  StepToolResult,
  ToolHandoff,
  ErrRevisionLimitReached,
  wrap,
} from "../domain/task.ts";
import type { Usage } from "../domain/usage.ts";
import {
  AnswerBlocks,
  SystemPrompt,
  Turns,
  assistantTurn,
  handoffInstructions,
  handoffTarget,
  marshalQuiet,
  toolTurn,
} from "./prompt.ts";

/** What the worker registers and what a start request names. */
export const WorkflowName = "AgentTaskWorkflow";

/**
 * Activity names, referenced by string so the worker registers them under the
 * names a running deployment's history already carries.
 */
export const ActivityLoadContext = "LoadTaskContext";
export const ActivityCallModel = "CallModel";
export const ActivityRunTool = "RunTool";
export const ActivityRequestDraft = "RequestDraft";
export const ActivityRecordStep = "RecordStep";
export const ActivityBeat = "BeatTask";
export const ActivityComplete = "CompleteTask";
export const ActivityPrepareHandoff = "PrepareHandoff";
export const ActivityMarkHandoffStarted = "MarkHandoffStarted";
export const ActivityResume = "ResumeTask";
export const ActivityCarryOutDraft = "CarryOutDraft";

/** Signal names a running task accepts. */
export const SignalCancel = "cancel";
export const SignalApproval = "approval";

/**
 * The signal definitions, named exactly as the Go workflow names its channels.
 * A signal that arrived before the handler was installed is buffered by the SDK
 * and delivered on replay, which is what the Go channels gave for free.
 */
export const cancelSignal = defineSignal(SignalCancel);
export const approvalSignal = defineSignal<[ApprovalSignal]>(SignalApproval);

// --------------------------------------------------------------- time bounds

/** A model call can legitimately take minutes; a step write must not. */
const contextTimeout = 30_000;
const modelTimeout = 5 * 60_000;
const toolTimeout = 2 * 60_000;
const draftTimeout = 30_000;
const recordTimeout = 30_000;
const beatTimeout = 30_000;
const completeTimeout = 60_000;
const handoffTimeout = 30_000;
/** Bounds one child task's whole life, because the parent waits for the answer. */
const childTimeout = 60 * 60_000;
/** Bounds one wait for the workspace to open. */
const maxHoldWait = 12 * 60 * 60_000;

/** The retry policy every activity gets unless it says otherwise. */
export const DefaultRetry: RetryPolicy = {
  initialInterval: "1s",
  backoffCoefficient: 2,
  maximumInterval: "30s",
  maximumAttempts: 4,
};

/** The policy for running a tool: a short retry is worth it, but not many times. */
export const ToolRetry: RetryPolicy = {
  initialInterval: "2s",
  backoffCoefficient: 2,
  maximumInterval: "20s",
  maximumAttempts: 3,
};

/** What starts a task. Every field is an id, so a restart re-reads the current state. */
export interface TaskInput {
  TaskID: string;
  /** Scopes every read an activity makes. */
  WorkspaceID: string;
  AgentID: string;
  ConversationID: string;
  ReplyMessageID: string;
  Title: string;
  Prompt: string;
  Depth: number;
}

/** What a finished task reports. */
export interface TaskOutcome {
  Status: string;
  Summary: string;
  Reason: string;
  Steps: number;
  Tokens: number;
}

/** One round's input: the Bolu, the tools it may call, and the conversation so far. */
export interface Context {
  Agent: AgentRef;
  Tools: Tool[];
  History: {
    ID: string;
    ConversationID: string;
    AgentID: string;
    UserID: string;
    Blocks: Block[];
    Attachments: { ID: string; Filename: string; ContentType: string; ByteSize: number }[];
  }[];
  /** The bounds the service configured, so the workflow and the service cannot disagree. */
  Limits: { MaxSteps: number; MaxTokens: number; MaxHandoffDepth: number; MaxToolResultBytes: number; MaxRevisions: number };
  Depth: number;
  /** What the Bolu remembers and what the search found for this task. */
  Memory: { Stable: string; Retrieved: string; Used: number };
}

/** One model call. */
export interface ModelRequest {
  TaskID: string;
  WorkspaceID: string;
  AgentID: string;
  System: string;
  Retrieved: string;
  Messages: {
    Role: string;
    Text: string;
    ToolCalls: { id: string; name: string; arguments: unknown }[];
    ToolResults: { callId: string; content: string; isError: boolean }[];
    Attachments: { ID: string; Filename: string; ContentType: string; ByteSize: number }[];
  }[];
  Tools: Tool[];
}

/** What the model answered. */
export interface ModelReply {
  Text: string;
  ToolCalls: ToolCall[];
  Usage: Usage;
  Model: string;
  FinishReason: string;
}

/** One tool the model asked for. */
export interface ToolCall {
  CallID: string;
  Name: string;
  Arguments: unknown;
}

/** One tool call for an activity to run. */
export interface ToolRequest {
  TaskID: string;
  WorkspaceID: string;
  CallID: string;
  Name: string;
  Arguments: unknown;
}

/** One round for an activity to record. */
export interface StepRequest {
  TaskID: string;
  WorkspaceID: string;
  Record: StepRecord;
}

/** The terminal state for an activity to write. */
export interface CompleteRequest {
  TaskID: string;
  WorkspaceID: string;
  Status: string;
  Reason: string;
  InputTokens: number;
  OutputTokens: number;
  CostMicros: number;
  StepCount: number;
  /** The answer, which the reply writer stores in the conversation. */
  Blocks: Block[];
}

/** Records the workflow a child task runs under. */
export interface HandoffStarted {
  TaskID: string;
  WorkspaceID: string;
  WorkflowID: string;
  RunID: string;
}

/** Asks for an approved action to be carried out. */
export interface DraftCarryRequest {
  TaskID: string;
  WorkspaceID: string;
  DraftID: string;
}

/** What carrying out an approved action produced. */
export interface DraftCarryOutcome {
  Status: string;
  /** What the action answered, when it ran. */
  Content: unknown;
  /** Says the action is waiting for the workspace's working hours. */
  Held: boolean;
  /** RFC3339, because a `Date` does not survive the payload converter. */
  NotBefore: string;
  /**
   * How long until `NotBefore`, in milliseconds, measured by the activity.
   *
   * It exists because the workflow may not read a clock: the Go loop computes
   * `NotBefore.Sub(workflow.Now())`, and the only clock this side may consult is
   * the one an activity already had when it answered.
   */
  WaitMs: number;
  /** Explains a hold in the user's words. */
  Reason: string;
}

/** Clears a parked task's waiting state. */
export interface ResumeRequest {
  TaskID: string;
  WorkspaceID: string;
}

/** Event names a task publishes. */
export const EventTaskStarted = "task.started";
export const EventTaskStep = "task.step";
export const EventTaskRunning = "task.running";
export const EventTaskWaiting = "task.waiting_approval";
export const EventTaskFinished = "task.finished";

/** Message statuses for the reply the task writes. */
export const MessageComplete = "complete";
export const MessageFailed = "failed";

/** What running a tool produced. */
export interface ToolOutcome {
  Name: string;
  Label: string;
  Content: unknown;
  IsError: boolean;
  /** Set when the tool needed approval instead of running. */
  Draft: DraftOutcome | null;
}

/** The approval a write_external tool asked for. */
export interface DraftOutcome {
  DraftID: string;
  Status: string;
  Note: string;
  Title: string;
  /** Says the policy allowed the action to go out without a person. */
  Auto: boolean;
  /** What an action that already ran answered. */
  Content: unknown;
}

/** One round to write to `task_steps`. */
export interface StepRecord {
  Kind: string;
  ToolName: string;
  ToolLabel: string;
  Input: unknown;
  Output: unknown;
  InputTokens: number;
  OutputTokens: number;
}

/** The checkpoint between rounds: a heartbeat with the running totals. */
export interface Beat {
  TaskID: string;
  WorkspaceID: string;
  StepCount: number;
  InputTokens: number;
  OutputTokens: number;
  /** Set when a bound was reached. */
  StopReason: string;
  /** The user-facing explanation. */
  StopDetail: string;
}

/** What a human decided about a draft. */
export interface ApprovalSignal {
  DraftID: string;
  Approved: boolean;
  Note: string;
}

/** Asks another Bolu to do part of the work. */
export interface HandoffRequest {
  ParentTaskID: string;
  WorkspaceID: string;
  AgentName: string;
  Instructions: string;
  Depth: number;
}

/** The child task a handoff opened, or the reason it was refused. */
export interface PreparedHandoff {
  Child: TaskInput;
  Refusal: string;
}

/** The child task's answer. */
export interface HandoffResult {
  ChildTaskID: string;
  Status: string;
  Summary: string;
  /** Set when the handoff was refused rather than run. */
  Refusal: string;
}

/** The activity set, as the workflow sees it through its proxies. */
export interface ActivitiesShape {
  LoadTaskContext(input: TaskInput): Promise<Context>;
  CallModel(request: ModelRequest): Promise<ModelReply>;
  RunTool(request: ToolRequest): Promise<ToolOutcome>;
  RequestDraft(request: ToolRequest): Promise<DraftOutcome>;
  RecordStep(request: StepRequest): Promise<void>;
  BeatTask(request: Beat): Promise<Beat>;
  CompleteTask(request: CompleteRequest): Promise<TaskOutcome>;
  PrepareHandoff(request: HandoffRequest): Promise<PreparedHandoff>;
  MarkHandoffStarted(request: HandoffStarted): Promise<void>;
  ResumeTask(request: ResumeRequest): Promise<void>;
  CarryOutDraft(request: DraftCarryRequest): Promise<DraftCarryOutcome>;
}

function options(startToCloseTimeout: number, retry: RetryPolicy): ActivityOptions {
  return { startToCloseTimeout, retry };
}

/** The activity set, as the workflow calls it. Names come from the interface. */
const loadActivities = proxyActivities<ActivitiesShape>(options(contextTimeout, DefaultRetry));
const modelActivities = proxyActivities<ActivitiesShape>(options(modelTimeout, DefaultRetry));
const toolActivities = proxyActivities<ActivitiesShape>(options(toolTimeout, ToolRetry));
const draftActivities = proxyActivities<ActivitiesShape>(options(draftTimeout, DefaultRetry));
const recordActivities = proxyActivities<ActivitiesShape>(options(recordTimeout, DefaultRetry));
const beatActivities = proxyActivities<ActivitiesShape>(options(beatTimeout, DefaultRetry));
const completeActivities = proxyActivities<ActivitiesShape>(options(completeTimeout, DefaultRetry));
const handoffActivities = proxyActivities<ActivitiesShape>(options(handoffTimeout, DefaultRetry));

/**
 * Runs one task to its end.
 *
 * A Bolu may think and read as freely as it likes, so reading tools run directly;
 * anything that reaches outside Bolu goes through a draft a human decides on.
 * Every round is recorded, and the loop stops on a bound with a reason a person
 * can read rather than on a timeout.
 */
export async function AgentTaskWorkflow(input: TaskInput): Promise<TaskOutcome> {
  const state = new RunState(input);

  // The handlers are installed before any work, so a cancel or an approval that
  // arrives while the first activity runs is buffered rather than lost.
  setHandler(approvalSignal, (signal) => {
    state.approvals.push(signal);
  });
  setHandler(cancelSignal, () => {
    state.canceled = true;
  });

  let loaded: Context;
  try {
    loaded = await loadActivities.LoadTaskContext(input);
  } catch (err) {
    return state.fail(`gagal menyiapkan tugas: ${messageOf(err)}`);
  }

  // The conversation starts from what was already said and grows by a turn per
  // round, so a tool's answer reaches the model on the next one.
  state.turns = Turns(loaded, input);

  while (state.steps < loaded.Limits.MaxSteps) {
    // The cancel check is here rather than at the top so it covers a signal that
    // arrived while the previous round ran.
    if (state.drainCancel()) {
      return state.finish(StatusCanceled, "dihentikan oleh pengguna");
    }

    try {
      const done = await state.oneRound(loaded);
      const beat = await state.beat();
      if (beat.StopReason !== "") {
        return state.finish(beat.StopReason, beat.StopDetail);
      }
      if (done) {
        return state.finish(StatusSucceeded, "");
      }
    } catch (err) {
      return state.fail(messageOf(err));
    }
  }

  // The loop ran its whole allowance without an answer, which is a stop the user
  // must be told about rather than a silent truncation.
  return state.finish(StatusFailed, "tugas berhenti karena mencapai batas langkah");
}

/** What the workflow accumulated. It is never shared with an activity. */
class RunState {
  steps = 0;
  inputTokens = 0;
  outputTokens = 0;
  canceled = false;
  answer = "";
  /** How many times a person has sent an action back, for this run. */
  revisions = 0;
  turns: ModelRequest["Messages"] = [];

  /** Approvals that have arrived, in order; an unmatched one stays for its round. */
  approvals: ApprovalSignal[] = [];

  constructor(readonly input: TaskInput) {}

  /** Reports whether a cancel signal has been seen. */
  drainCancel(): boolean {
    return this.canceled;
  }

  /** Asks the model, runs what it asked for, and records the round. */
  async oneRound(loaded: Context): Promise<boolean> {
    const request: ModelRequest = {
      TaskID: this.input.TaskID,
      WorkspaceID: this.input.WorkspaceID,
      AgentID: this.input.AgentID,
      System: SystemPrompt(loaded, this.input),
      Retrieved: loaded.Memory.Retrieved,
      Messages: this.turns,
      Tools: loaded.Tools,
    };

    let reply: ModelReply;
    try {
      reply = await modelActivities.CallModel(request);
    } catch (err) {
      throw new Error(`model gagal menjawab: ${messageOf(err)}`);
    }

    this.steps++;
    this.inputTokens += reply.Usage.InputTokens;
    this.outputTokens += reply.Usage.OutputTokens;

    // The assistant's own turn joins the conversation, with the calls it asked
    // for, so the results that follow have something to answer.
    this.turns = [...this.turns, assistantTurn(reply)];

    // The thinking round is recorded before its tools, so the order in the
    // history is the order the model produced.
    await recordActivities.RecordStep({
      TaskID: this.input.TaskID,
      WorkspaceID: this.input.WorkspaceID,
      Record: {
        Kind: StepThink,
        ToolName: "",
        ToolLabel: "",
        Input: request.Messages,
        Output: marshalQuiet({ text: reply.Text, tool_calls: reply.ToolCalls }),
        InputTokens: reply.Usage.InputTokens,
        OutputTokens: reply.Usage.OutputTokens,
      },
    });

    if (reply.ToolCalls.length === 0) {
      this.answer = reply.Text;
      await recordActivities.RecordStep({
        TaskID: this.input.TaskID,
        WorkspaceID: this.input.WorkspaceID,
        Record: {
          Kind: StepFinal,
          ToolName: "",
          ToolLabel: "",
          Input: null,
          Output: marshalQuiet({ text: reply.Text }),
          InputTokens: 0,
          OutputTokens: 0,
        },
      });
      return true;
    }

    for (const call of reply.ToolCalls) {
      const outcome = await this.runTool(loaded, call);

      // A policy that allowed the action unattended means there is no decision
      // to wait for: the gate already carried it out.
      if (outcome.Draft !== null && outcome.Draft.Auto) {
        outcome.Content = marshalQuiet({
          executed: true,
          content: outcome.Draft.Content ?? {},
        });
      }

      // A tool that touches the outside world becomes a draft, and the task
      // parks on it: the workflow waits for the decision rather than holding a
      // worker slot open.
      if (outcome.Draft !== null && !outcome.Draft.Auto) {
        const decision = await this.waitForApproval(outcome.Draft.DraftID);
        if (decision === null) {
          throw new Error("dibatalkan saat menunggu persetujuan");
        }
        await this.resume();

        if (decision.Approved) {
          // An approved action is carried out and the task ends there: the
          // person approved what to send, so asking the model to produce it
          // again would spend tokens to reach a decision already made.
          const sent = await this.carryOut(outcome.Draft.DraftID);
          this.answer = draftAnswer(outcome.Draft, sent);
          return true;
        }

        // A revision goes back to the model with the person's note. The tally is
        // bounded so the argument cannot outlast the task's allowance.
        this.revisions++;
        if (this.revisions > loaded.Limits.MaxRevisions) {
          throw wrap(
            ErrRevisionLimitReached,
            `${this.revisions} kali, batasnya ${loaded.Limits.MaxRevisions}`,
          );
        }

        outcome.Content = marshalQuiet({
          approved: false,
          note: decision.Note,
          revision: this.revisions,
        });
        outcome.IsError = true;
      }

      await recordActivities.RecordStep({
        TaskID: this.input.TaskID,
        WorkspaceID: this.input.WorkspaceID,
        Record: {
          Kind: StepToolCall,
          ToolName: call.Name,
          ToolLabel: outcome.Label,
          Input: call.Arguments,
          Output: null,
          InputTokens: 0,
          OutputTokens: 0,
        },
      });

      const resultStep: StepRecord = {
        Kind: StepToolResult,
        ToolName: call.Name,
        ToolLabel: outcome.Label,
        Input: null,
        Output: outcome.Content,
        InputTokens: 0,
        OutputTokens: 0,
      };
      if (outcome.Draft !== null) {
        resultStep.Kind = StepDraft;
        resultStep.Output = marshalQuiet({
          draft_id: outcome.Draft.DraftID,
          status: outcome.Draft.Status,
        });
      }

      await recordActivities.RecordStep({
        TaskID: this.input.TaskID,
        WorkspaceID: this.input.WorkspaceID,
        Record: resultStep,
      });

      // The result is what the model reads next round. A refusal or a failure is
      // a result like any other: the model is meant to react to it.
      this.turns = [...this.turns, toolTurn(call, outcome)];
    }

    return false;
  }

  /** Executes one tool call, or routes it to the approval gate. */
  private async runTool(loaded: Context, call: ToolCall): Promise<ToolOutcome> {
    // A handoff is the one tool the workflow performs itself: it opens a child
    // task and waits for the answer, which is what makes a chain traceable
    // rather than a conversation.
    if (call.Name === ToolHandoff) {
      return this.handoffTool(loaded, call);
    }

    const tool = loaded.Tools.find((candidate) => candidate.Name === call.Name);
    if (!tool) {
      // Nothing can run it, so the model is told rather than the task failing.
      return {
        Name: call.Name,
        Label: "",
        Content: marshalQuiet({ error: "alat tidak tersedia untuk Bolu ini" }),
        IsError: true,
        Draft: null,
      };
    }

    const request: ToolRequest = {
      TaskID: this.input.TaskID,
      WorkspaceID: this.input.WorkspaceID,
      CallID: call.CallID,
      Name: call.Name,
      Arguments: call.Arguments,
    };

    if (tool.Label === "write_external") {
      return this.requestDraft(tool, request);
    }

    let outcome: ToolOutcome;
    try {
      outcome = await toolActivities.RunTool(request);
    } catch (err) {
      // A tool that could not run at all is the model's problem to solve, not
      // the task's: it is reported back as a failed result.
      return {
        Name: call.Name,
        Label: tool.Label,
        Content: marshalQuiet({ error: messageOf(err) }),
        IsError: true,
        Draft: null,
      };
    }

    outcome.Name = call.Name;
    outcome.Label = tool.Label;
    return outcome;
  }

  /** Turns a write_external tool into a draft and parks the task. */
  private async requestDraft(tool: Tool, request: ToolRequest): Promise<ToolOutcome> {
    let draft: DraftOutcome;
    try {
      draft = await draftActivities.RequestDraft(request);
    } catch (err) {
      // A gate that could not be reached is a refused action, which the model reads.
      return {
        Name: request.Name,
        Label: tool.Label,
        Content: marshalQuiet({ error: messageOf(err) }),
        IsError: true,
        Draft: null,
      };
    }

    if (draft.Status === DraftCanceled) {
      // No gate is configured, so the action was refused rather than queued.
      return {
        Name: request.Name,
        Label: tool.Label,
        Content: marshalQuiet({ error: draft.Note }),
        IsError: true,
        Draft: null,
      };
    }

    return {
      Name: request.Name,
      Label: tool.Label,
      Content: marshalQuiet({ queued_for_approval: true }),
      IsError: false,
      Draft: draft,
    };
  }

  /**
   * Executes an approved action, waiting for the workspace to open.
   *
   * A hold is waited out on a Temporal timer rather than by re-checking: the
   * wait is durable, so a worker that dies during it resumes the wait.
   */
  private async carryOut(draftId: string): Promise<DraftCarryOutcome> {
    const request: DraftCarryRequest = {
      TaskID: this.input.TaskID,
      WorkspaceID: this.input.WorkspaceID,
      DraftID: draftId,
    };

    let outcome: DraftCarryOutcome;
    try {
      outcome = await draftActivities.CarryOutDraft(request);
    } catch (err) {
      throw new Error(`gagal menjalankan aksi yang disetujui: ${messageOf(err)}`);
    }

    // The action is held until the workspace opens. The wait is bounded by a
    // ceiling of its own so a workspace whose hours never open cannot pin the
    // task forever.
    while (outcome.Held) {
      // The wait is a number the activity measured, not a clock read here: a
      // duration read in the workflow would differ between the run and its replay.
      let delay = outcome.WaitMs;
      if (delay <= 0) {
        // The window opened while the round ran, so the next attempt sends.
        delay = 1_000;
      }
      if (delay > maxHoldWait) {
        delay = maxHoldWait;
      }

      await sleep(delay);

      try {
        outcome = await draftActivities.CarryOutDraft(request);
      } catch (err) {
        throw new Error(`gagal menjalankan aksi yang disetujui: ${messageOf(err)}`);
      }
    }

    return outcome;
  }

  /** Clears the parked state once the decision is in. */
  private async resume(): Promise<void> {
    try {
      await recordActivities.ResumeTask({ TaskID: this.input.TaskID, WorkspaceID: this.input.WorkspaceID });
    } catch (err) {
      throw new Error(`gagal melanjutkan tugas setelah keputusan: ${messageOf(err)}`);
    }
  }

  /**
   * Opens a child task for another Bolu and waits for its answer, returning it
   * in the shape every other tool answers in.
   */
  private async handoffTool(loaded: Context, call: ToolCall): Promise<ToolOutcome> {
    const result = await this.handoff(loaded, call);

    if (result.Refusal !== "") {
      return {
        Name: call.Name,
        Label: LabelRead,
        Content: marshalQuiet({ error: result.Refusal }),
        IsError: true,
        Draft: null,
      };
    }

    return {
      Name: call.Name,
      Label: LabelRead,
      Content: marshalQuiet({
        child_task_id: result.ChildTaskID,
        status: result.Status,
        summary: result.Summary,
      }),
      IsError: result.Status === StatusFailed,
      Draft: null,
    };
  }

  /**
   * Opens a child task for another Bolu and waits for its answer.
   *
   * The child is a Temporal child workflow rather than an activity, so the chain
   * is visible in the Temporal UI, the parent's wait survives a restart, and a
   * child that runs long does not hold the parent's activity slot.
   */
  private async handoff(loaded: Context, call: ToolCall): Promise<HandoffResult> {
    let prepared: PreparedHandoff;
    try {
      prepared = await handoffActivities.PrepareHandoff({
        ParentTaskID: this.input.TaskID,
        WorkspaceID: this.input.WorkspaceID,
        AgentName: handoffTarget(call.Arguments),
        Instructions: handoffInstructions(call.Arguments),
        Depth: loaded.Depth,
      });
    } catch (err) {
      throw new Error(`serah-terima gagal: ${messageOf(err)}`);
    }
    if (prepared.Refusal !== "") {
      return { ChildTaskID: "", Status: "", Summary: "", Refusal: prepared.Refusal };
    }
    const child = prepared.Child;

    let handle;
    try {
      handle = await startChild(AgentTaskWorkflow, {
        // The child runs under its own task id, which is what makes the chain
        // readable from Temporal without a lookup table.
        workflowId: `task-${child.TaskID}`,
        workflowExecutionTimeout: childTimeout,
        args: [child],
      });
    } catch (err) {
      throw new Error(`serah-terima gagal dimulai: ${messageOf(err)}`);
    }

    // The execution is recorded before the wait, so a restart mid-child still
    // shows which run the parent is waiting on.
    try {
      await recordActivities.MarkHandoffStarted({
        TaskID: child.TaskID,
        WorkspaceID: child.WorkspaceID,
        WorkflowID: handle.workflowId,
        RunID: handle.firstExecutionRunId,
      });
    } catch (err) {
      throw new Error(messageOf(err));
    }

    try {
      const outcome = await handle.result();
      return { ChildTaskID: child.TaskID, Status: outcome.Status, Summary: outcome.Summary, Refusal: "" };
    } catch (err) {
      // A child that failed is reported as a failed tool result, so the parent
      // can carry on with what it has rather than failing with it.
      return {
        ChildTaskID: child.TaskID,
        Status: StatusFailed,
        Summary: messageOf(err),
        Refusal: "",
      };
    }
  }

  /** Records a heartbeat and asks whether the task may continue. */
  async beat(): Promise<Beat> {
    return beatActivities.BeatTask({
      TaskID: this.input.TaskID,
      WorkspaceID: this.input.WorkspaceID,
      StepCount: this.steps,
      InputTokens: this.inputTokens,
      OutputTokens: this.outputTokens,
      StopReason: "",
      StopDetail: "",
    });
  }

  /** Writes the terminal state of the task. */
  async finish(status: string, reason: string): Promise<TaskOutcome> {
    const stored = await completeActivities.CompleteTask({
      TaskID: this.input.TaskID,
      WorkspaceID: this.input.WorkspaceID,
      Status: status,
      Reason: reason,
      InputTokens: this.inputTokens,
      OutputTokens: this.outputTokens,
      CostMicros: 0,
      StepCount: this.steps,
      Blocks: AnswerBlocks(this.answer, status),
    });

    // A throw here means the task's own state could not be written; the error
    // propagates rather than the workflow pretending it finished.
    stored.Steps = this.steps;
    stored.Tokens = this.inputTokens + this.outputTokens;
    if (stored.Status === "") stored.Status = status;
    if (stored.Reason === "") stored.Reason = reason;
    return stored;
  }

  /** Ends the task as failed, keeping the message the user needs. */
  async fail(reason: string): Promise<TaskOutcome> {
    return this.finish(StatusFailed, reason);
  }

  /**
   * Parks the workflow until a human decides.
   *
   * The wait has no timeout: a person may take hours, and that is the point of a
   * durable task. What keeps it safe is that the task is parked in the store, so
   * a restart resumes the wait rather than losing the task.
   *
   * Returns null when a cancel arrived instead of a decision.
   */
  private async waitForApproval(draftId: string): Promise<ApprovalSignal | null> {
    for (;;) {
      if (this.canceled) return null;

      // A decision about another draft is not this wait's answer. The Go
      // selector consumes it and selects again, and the port does the same: the
      // buffer is drained in arrival order rather than searched for a match.
      const next = this.approvals[0];
      if (next !== undefined) {
        this.approvals = this.approvals.slice(1);
        if (next.DraftID === draftId) return next;
        continue;
      }

      await condition(() => this.canceled || this.approvals.length > 0);
    }
  }
}

/**
 * What the task reports after carrying an approved action out.
 *
 * The action's own answer is preferred when it gave one, because it is the most
 * specific thing that happened; otherwise the draft's summary stands in.
 */
function draftAnswer(draft: DraftOutcome | null, sent: DraftCarryOutcome): string {
  if (sent.Content !== null && sent.Content !== undefined) {
    const payload = sent.Content;
    if (typeof payload === "object" && !Array.isArray(payload)) {
      for (const key of ["text", "message", "summary"]) {
        if (!(key in payload)) continue;
        const candidate: unknown = Reflect.get(payload, key);
        if (typeof candidate === "string" && candidate.trim() !== "") {
          return candidate.trim();
        }
      }
    }
  }

  if (draft !== null && draft.Title.trim() !== "") {
    return draft.Title;
  }
  return "";
}

/**
 * The message an operator reads.
 *
 * The SDK's `ActivityFailure` says only "Activity task failed" and keeps the
 * activity's own sentence in `cause`; Go's error carried that sentence in its
 * text, and the user-facing stop reasons are built from it. The chain is walked
 * to the deepest error for that reason.
 */
function messageOf(err: unknown): string {
  let current: unknown = err;
  for (let guard = 0; guard < 16; guard++) {
    if (!(current instanceof Error)) return String(current);
    if (!(current.cause instanceof Error)) return current.message;
    current = current.cause;
  }
  return current instanceof Error ? current.message : String(current);
}
