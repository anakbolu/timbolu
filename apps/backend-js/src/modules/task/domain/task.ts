/**
 * The task runtime contract: the task a Bolu runs, the steps it took, the tools
 * it may call, and the ports the workflow needs for everything that is not
 * deterministic.
 *
 * A task is a durable unit of work. It survives a worker restart, it can wait
 * for a human for hours, and every round it takes is recorded — which is what
 * lets the activity feed and the history read the same truth.
 *
 * Ported 1:1 from internal/modules/task/domain/task.go.
 *
 * Naming: the Go structs that cross the Temporal boundary have no `json` tags,
 * so their wire keys are the Go field names (`TaskID`, `WorkspaceID`, …). Those
 * types keep PascalCase fields here, because an in-flight task recorded by the
 * Go worker must replay through this code. The types that never leave the
 * process (`Task`, `Step`, `Scope`, …) use the camelCase convention the rest of
 * this port follows.
 */

import type { CostTable as LLMCostTable, Gateway as LLMGateway } from "../../llm/domain/gateway.ts";
import type {
  Role,
  ToolCall as LLMToolCall,
  ToolResult as LLMToolResult,
  Usage as LLMUsage,
} from "../../llm/domain/provider.ts";
import type { Usage } from "./usage.ts";

export type { Usage } from "./usage.ts";

/** Status values, matching the check constraint on `tasks.status`. */
export const StatusQueued = "queued";
export const StatusRunning = "running";
export const StatusWaitingApproval = "waiting_approval";
export const StatusSucceeded = "succeeded";
export const StatusFailed = "failed";
export const StatusCanceled = "canceled";

/** Triggers, matching the check constraint on `tasks.trigger`. */
export const TriggerChat = "chat";
export const TriggerRoutine = "routine";
export const TriggerWebhook = "webhook";
export const TriggerHandoff = "handoff";

/** Step kinds, matching the check constraint on `task_steps.kind`. */
export const StepThink = "think";
export const StepToolCall = "tool_call";
export const StepToolResult = "tool_result";
export const StepDraft = "draft";
export const StepFinal = "final";

/** The zero uuid, as Go's `uuid.Nil`. */
export const NilUUID = "00000000-0000-0000-0000-000000000000";

/** Errors the service and the handlers map onto responses. */
export const ErrTaskNotFound = new Error("task: not found");
export const ErrInvalidInput = new Error("task: invalid input");
export const ErrUnknownTrigger = new Error("task: unknown trigger");
export const ErrUnknownStep = new Error("task: unknown step kind");
export const ErrStepLimitReached = new Error("task: step limit reached");
export const ErrTokenLimitReached = new Error("task: token limit reached");
export const ErrCostLimitReached = new Error("task: daily cost limit reached");
export const ErrDepthLimitReached = new Error("task: handoff chain is too deep");
export const ErrCrossWorkspaceHandoff = new Error("task: handoff must stay in the workspace");
export const ErrToolNotAvailable = new Error("task: tool is not available");
export const ErrDraftNoSender = new Error("task: no executor can carry this action out");
export const ErrDraftNotApproved = new Error("task: the draft is not approved");
export const ErrRevisionLimitReached = new Error(
  "tugas berhenti: aksi ini terlalu banyak dikembalikan untuk direvisi",
);
export const ErrOutcomeUnclaimed = new Error("task: no outcome sink claimed this task");

/**
 * An error carrying the sentinel a caller switches on, so `is` reproduces Go's
 * `errors.Is` for the messages `fmt.Errorf("%w: …")` produced.
 */
export class WrappedError extends Error {
  constructor(
    readonly sentinel: Error,
    message: string,
  ) {
    super(message);
    this.name = "WrappedError";
  }
}

/** Wraps a sentinel with a detail, rendering the message as Go's `%w: detail`. */
export function wrap(sentinel: Error, detail: string): WrappedError {
  return new WrappedError(sentinel, `${sentinel.message}: ${detail}`);
}

/** True when `err` is the sentinel or wraps it. */
export function is(err: unknown, sentinel: Error): boolean {
  let current: unknown = err;
  for (let guard = 0; guard < 32; guard++) {
    if (current === sentinel) return true;
    if (!(current instanceof Error)) return false;
    if (current instanceof WrappedError) {
      current = current.sentinel;
      continue;
    }
    current = "cause" in current ? current.cause : undefined;
  }
  return false;
}

/** The message of an unknown thrown value. */
export function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/** Limits bound what one task may spend. Zero means the configured default. */
export interface Limits {
  /** How many rounds of the model-and-tools loop a task may take. */
  MaxSteps: number;
  /** How many tokens one task may spend, input and output together. */
  MaxTokens: number;
  /** How deep a chain of handoffs may nest. */
  MaxHandoffDepth: number;
  /** Truncates a tool's answer. */
  MaxToolResultBytes: number;
  /** Bounds how many times a person may send one action back. */
  MaxRevisions: number;
}

/** The bounds a deployment runs with when the configuration says nothing. */
export const DefaultLimits: Limits = {
  MaxSteps: 12,
  MaxTokens: 120_000,
  MaxHandoffDepth: 2,
  MaxToolResultBytes: 16 * 1024,
  MaxRevisions: 3,
};

/** Fills the zero fields from the defaults, so a partially configured deployment still has bounds. */
export function normaliseLimits(l: Limits): Limits {
  return {
    MaxSteps: l.MaxSteps > 0 ? l.MaxSteps : DefaultLimits.MaxSteps,
    MaxTokens: l.MaxTokens > 0 ? l.MaxTokens : DefaultLimits.MaxTokens,
    MaxHandoffDepth: l.MaxHandoffDepth < 0 ? DefaultLimits.MaxHandoffDepth : l.MaxHandoffDepth,
    MaxToolResultBytes:
      l.MaxToolResultBytes > 0 ? l.MaxToolResultBytes : DefaultLimits.MaxToolResultBytes,
    MaxRevisions: l.MaxRevisions > 0 ? l.MaxRevisions : DefaultLimits.MaxRevisions,
  };
}

/** The tenant identity a query runs as. */
export interface Scope {
  userId?: string | null;
  workspaceId?: string | null;
}

/** Reports whether the scope carries no identity. */
export function scopeIsZero(s: Scope): boolean {
  return !s.workspaceId;
}

/** One unit of work a Bolu runs. */
export interface Task {
  id: string;
  workspaceId: string;
  agentId: string;
  parentTaskId: string;
  conversationId: string;
  trigger: string;
  title: string;
  status: string;
  /** The conversation message this task's answer fills. */
  replyMessageId: string;
  workflowId: string;
  temporalRunId: string;
  /** How far down a chain of handoffs this task is. A task a user started is zero. */
  depth: number;
  /** What the task did, in one line. It is what survives after the raw steps are pruned. */
  summary: string;
  /** Explains a task that stopped without finishing, in the user's words. */
  stoppedReason: string;
  /** What a parked task is waiting for. */
  waitingReason: string;
  /** The approval a parked task is waiting on. */
  waitingDraftId: string;
  stepCount: number;
  inputTokens: number;
  outputTokens: number;
  costMicros: number;
  heartbeatAt: Date | null;
  startedAt: Date | null;
  finishedAt: Date | null;
  createdAt: Date;
  updatedAt: Date;
}

/** What the task spent, which is what the limits compare against. */
export function totalTokens(t: Task): number {
  return t.inputTokens + t.outputTokens;
}

/** Reports whether the task still occupies a Bolu: queued, running, or parked on a decision. */
export function taskLive(t: Task): boolean {
  switch (t.status) {
    case StatusQueued:
    case StatusRunning:
    case StatusWaitingApproval:
      return true;
    default:
      return false;
  }
}

/** How long a task may go without a heartbeat before it is reported as stuck. */
export const STUCK_AFTER_MS = 5 * 60 * 1000;

/** How the UI describes a live task. */
export type Health = "running" | "waiting" | "stuck" | "done";

export const HealthRunning: Health = "running";
export const HealthWaiting: Health = "waiting";
export const HealthStuck: Health = "stuck";
export const HealthDone: Health = "done";

/** Reports how a task looks right now. It is derived, never stored. */
export function taskHealth(t: Task, now: Date): Health {
  if (!taskLive(t)) {
    return HealthDone;
  }
  if (t.status === StatusWaitingApproval) {
    return HealthWaiting;
  }
  if (t.heartbeatAt === null || now.getTime() - t.heartbeatAt.getTime() > STUCK_AFTER_MS) {
    if (t.status === StatusQueued && now.getTime() - t.createdAt.getTime() <= STUCK_AFTER_MS) {
      return HealthRunning;
    }
    return HealthStuck;
  }
  return HealthRunning;
}

/** One recorded round of a task. */
export interface Step {
  id: string;
  taskId: string;
  seq: number;
  kind: string;
  toolName: string;
  toolLabel: string;
  /** The JSON document as the runtime recorded it. */
  input: unknown;
  output: unknown;
  inputTokens: number;
  /** The model's tokens for this step, which only a think step spends. */
  outputTokens: number;
  /** Marks a step whose raw payload the retention job removed. */
  pruned: boolean;
  createdAt: Date;
}

/** The heartbeat a running task records. */
export interface Progress {
  taskId: string;
  inputTokens: number;
  outputTokens: number;
  costMicros: number;
  stepCount: number;
}

/** The terminal state of a task. */
export interface Finish {
  taskId: string;
  status: string;
  summary: string;
  stoppedReason: string;
  inputTokens: number;
  outputTokens: number;
  costMicros: number;
  stepCount: number;
}

/** Narrows a task history read. */
export interface Filter {
  /** Limits the read to one Bolu. */
  agentId: string;
  /** Limits the read to the children of one task. */
  parentTaskId: string;
  /** Limits the read to one status; empty means every status. */
  status: string;
  /** Limits the read to tasks that have not finished. */
  live: boolean;
  limit: number;
}

/** The page a client gets when it does not ask for one. */
export const DefaultHistoryLimit = 50;
/** Bounds one page. */
export const MaxHistoryLimit = 200;

/** The persistence contract for tasks. */
export interface TaskRepository {
  create(ctx: AbortSignal | undefined, scope: Scope, task: NewTask): Promise<Task>;
  get(ctx: AbortSignal | undefined, scope: Scope, id: string): Promise<Task>;
  list(ctx: AbortSignal | undefined, scope: Scope, filter: Filter): Promise<Task[]>;
  start(
    ctx: AbortSignal | undefined,
    scope: Scope,
    id: string,
    workflowId: string,
    runId: string,
  ): Promise<Task>;
  progress(ctx: AbortSignal | undefined, scope: Scope, progress: Progress): Promise<void>;
  park(
    ctx: AbortSignal | undefined,
    scope: Scope,
    id: string,
    reason: string,
    draftId: string,
  ): Promise<Task>;
  resume(ctx: AbortSignal | undefined, scope: Scope, id: string): Promise<Task>;
  finish(ctx: AbortSignal | undefined, scope: Scope, finish: Finish): Promise<Task>;
  cancel(ctx: AbortSignal | undefined, scope: Scope, id: string, reason: string): Promise<Task>;
  children(ctx: AbortSignal | undefined, scope: Scope, taskId: string): Promise<Task[]>;
}

/** The persistence contract for steps. */
export interface StepRepository {
  append(ctx: AbortSignal | undefined, scope: Scope, step: Step): Promise<Step>;
  listSteps(ctx: AbortSignal | undefined, scope: Scope, taskId: string): Promise<Step[]>;
}

/** Removes the raw payload of steps past their window. */
export interface StepPruner {
  pruneContents(
    ctx: AbortSignal | undefined,
    scope: Scope,
    olderThan: Date,
    limit: number,
  ): Promise<StepPrune>;
}

/** Bounds one bulk prune. */
export const DefaultPruneLimit = 500;

/** What one bulk prune did. */
export interface StepPrune {
  pruned: number;
  remaining: number;
}

/** A task to insert. */
export interface NewTask {
  agentId: string;
  parentTaskId: string;
  conversationId: string;
  replyMessageId: string;
  trigger: string;
  title: string;
  depth: number;
}

/** The minimum a task needs to know about the Bolu running it. */
export interface AgentRef {
  ID: string;
  Name: string;
  Role: string;
  Persona: string;
  Tone: string;
  /** The registry's own switch and its derived status. */
  Stored: string;
  Display: string;
  Tools: string[];
  DefaultModel: Record<string, unknown>;
}

/** The stored status that takes no new work. */
export const Resting = "resting";

/** Reports whether the Bolu accepts new work. */
export function agentActive(a: AgentRef): boolean {
  return a.Stored !== Resting;
}

/** Reads the Bolu of a workspace. */
export interface AgentDirectory {
  agent(ctx: AbortSignal | undefined, scope: Scope, id: string): Promise<AgentRef>;
  active(ref: AgentRef): boolean;
}

/** Resolves a Bolu by the name the model knows. */
export interface AgentNamer {
  byName(ctx: AbortSignal | undefined, scope: Scope, name: string): Promise<AgentRef>;
}

/** One turn of the conversation a task answers. */
export interface Message {
  ID: string;
  ConversationID: string;
  AgentID: string;
  UserID: string;
  Blocks: Block[];
  Attachments: AttachmentRef[];
}

/** Names one file of a message without carrying it. */
export interface AttachmentRef {
  ID: string;
  Filename: string;
  ContentType: string;
  ByteSize: number;
}

/** A file as the reader returned it. */
export interface AttachmentContent {
  filename: string;
  contentType: string;
  content: Uint8Array;
}

/** Reads the content of a message's attachment. */
export interface AttachmentReader {
  readAttachment(
    ctx: AbortSignal | undefined,
    scope: Scope,
    attachmentId: string,
  ): Promise<AttachmentContent>;
}

/**
 * One content block of a message.
 *
 * `Body` is the JSON document encoded as base64, because the Go field is a
 * `[]byte` and that is how encoding/json puts it on the Temporal wire. The
 * helpers in `render.ts` decode it.
 */
export interface Block {
  Type: string;
  Body: string;
}

/** One message of the conversation the agent reads. */
export interface Turn {
  Role: Role;
  Text: string;
  /** What the assistant asked for, on an assistant turn. */
  ToolCalls: LLMToolCall[];
  /** What those calls answered, on a tool turn. */
  ToolResults: LLMToolResult[];
  /** The files of the message this turn came from, on a user turn. */
  Attachments: AttachmentRef[];
}

/** Reports whether a Bolu wrote the message. */
export function messageFromAgent(m: Message): boolean {
  return m.AgentID !== "" && m.AgentID !== NilUUID;
}

/** Writes a task's answer into the conversation it came from. */
export interface ReplyWriter {
  history(
    ctx: AbortSignal | undefined,
    scope: Scope,
    conversationId: string,
    limit: number,
  ): Promise<Message[]>;
  write(ctx: AbortSignal | undefined, scope: Scope, write: WriteReply): Promise<void>;
}

/** The body of an answer at one moment. */
export interface WriteReply {
  messageId: string;
  blocks: Block[];
  status: string;
  finishReason: string;
  taskId: string;
}

/** Is told how a task ended. */
export interface OutcomeSink {
  report(ctx: AbortSignal | undefined, scope: Scope, report: OutcomeReport): Promise<void>;
}

/** One finished task's outcome. */
export interface OutcomeReport {
  taskId: string;
  status: string;
  summary: string;
  reason: string;
}

/** Describes one entry of the activity stream. */
export interface Event {
  workspaceId: string;
  type: string;
  actorAgentId: string;
  actorUserId: string;
  conversationId: string;
  taskId: string;
  draftId: string;
  payload: unknown;
}

/** Publishes what a task is doing. */
export interface EventSink {
  emit(ctx: AbortSignal | undefined, event: Event): Promise<void>;
}

/** One capability a task may call. */
export interface Tool {
  Name: string;
  Integration: string;
  Description: string;
  /** The JSON schema as the registry stores it. */
  Schema: unknown;
  /** `read` never needs a human; `write_external` always does. */
  Label: string;
}

/** The tool that asks another Bolu to do part of the work. */
export const ToolHandoff = "handoff";

/** Tool labels, matching the check constraint on `tool_catalog.label`. */
export const LabelRead = "read";
export const LabelWriteInternal = "write_internal";
export const LabelWriteExternal = "write_external";

/** Reports whether a tool reaches outside Bolu. */
export function toolNeedsApproval(t: Tool): boolean {
  return t.Label === LabelWriteExternal;
}

/** Renders a tool for the model. */
export function asLLMTool(t: Tool): {
  name: string;
  description: string;
  schema: unknown;
  label: string;
} {
  return { name: t.Name, description: t.Description, schema: t.Schema, label: t.Label };
}

/** Reports the tools an agent may actually call. */
export interface ToolRegistry {
  tools(ctx: AbortSignal | undefined, scope: Scope, agentId: string): Promise<Tool[]>;
}

/** Runs one tool call. */
export interface ToolExecutor {
  execute(ctx: AbortSignal | undefined, scope: Scope, call: ToolCall): Promise<ToolResult>;
}

/** One tool the model asked for. */
export interface ToolCall {
  taskId: string;
  agentId: string;
  callId: string;
  name: string;
  arguments: unknown;
  conversationId: string;
}

/** What a tool answered. */
export interface ToolResult {
  /** The JSON document the model reads. */
  content: unknown;
  /** Marks an answer that reports a failure. */
  isError: boolean;
}

/** Hands one round the memory a Bolu answers with. */
export interface MemoryRecall {
  recall(ctx: AbortSignal | undefined, scope: Scope, request: RecallRequest): Promise<Recall>;
}

/** What one round asks the memory module for. */
export interface RecallRequest {
  agentId: string;
  taskId: string;
  query: string;
}

/** The memory one round is given. */
export interface Recall {
  Stable: string;
  Retrieved: string;
  Used: number;
}

/** Turns an action that touches the outside world into a draft a human decides on. */
export interface DraftGate {
  requestDraft(
    ctx: AbortSignal | undefined,
    scope: Scope,
    request: DraftRequest,
  ): Promise<Draft>;
  carryOut(ctx: AbortSignal | undefined, scope: Scope, draftId: string): Promise<DraftExecution>;
}

/** What carrying out an approved action produced. */
export interface DraftExecution {
  /** The draft's status afterwards: sent when it left, approved when held. */
  status: string;
  content: unknown;
  held: boolean;
  notBefore: Date | null;
  reason: string;
}

/** One action awaiting a decision. */
export interface DraftRequest {
  taskId: string;
  agentId: string;
  conversationId: string;
  actionKind: string;
  title: string;
  payload: unknown;
}

/** The decision a task waits on. */
export interface Draft {
  id: string;
  title: string;
  status: string;
  note: string;
  /** Says the policy allowed the action to go out without a person. */
  auto: boolean;
  content: unknown;
}

/** Draft statuses, matching the check constraint on `drafts.status`. */
export const DraftPending = "pending";
export const DraftApproved = "approved";
export const DraftRevise = "revise";
export const DraftSent = "sent";
export const DraftCanceled = "canceled";

/** Refuses work a workspace can no longer afford. */
export interface Budget {
  allow(ctx: AbortSignal | undefined, workspaceId: string): Promise<void>;
}

/** Turns a model's tokens into a cost. */
export type CostTable = LLMCostTable;

/** Renders this module's token counts for the price table. */
export function asLLMUsage(u: Usage): LLMUsage {
  return {
    inputTokens: u.InputTokens,
    outputTokens: u.OutputTokens,
    cacheReadTokens: u.CacheReadTokens,
    cacheWriteTokens: u.CacheWriteTokens,
  };
}

/** The model the workflow calls. */
export type Gateway = LLMGateway;

/** Where the task runtime reports what it did. */
export interface Metrics {
  taskDispatched(ctx: AbortSignal | undefined): void;
  taskSettled(ctx: AbortSignal | undefined): void;
  taskFinished(
    ctx: AbortSignal | undefined,
    trigger: string,
    status: string,
    seconds: number,
    inputTokens: number,
    outputTokens: number,
    costMicros: number,
  ): void;
  toolCall(ctx: AbortSignal | undefined, app: string, failed: boolean, seconds: number): void;
  draftReady(ctx: AbortSignal | undefined, seconds: number): void;
  draftDecided(ctx: AbortSignal | undefined, outcome: string, revision: boolean): void;
  activityRetry(ctx: AbortSignal | undefined, activity: string, attempt: number): void;
}

/** Persists what one model call cost. */
export interface UsageRecorder {
  record(ctx: AbortSignal | undefined, entry: UsageEntry): Promise<void>;
}

/** One row of the usage ledger. */
export interface UsageEntry {
  workspaceId: string;
  agentId: string;
  taskId: string;
  provider: string;
  model: string;
  purpose: string;
  usage: LLMUsage;
  costMicros: number;
  createdAt: Date;
}
