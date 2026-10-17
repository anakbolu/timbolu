/**
 * The wire shapes of the task runtime: one task, one step, and the event that
 * carries a task's state to the activity feed.
 *
 * The payload is built in one place so a task read from the history endpoint and
 * the same task arriving on the stream describe themselves identically.
 *
 * Ported 1:1 from internal/modules/task/service/wire.go.
 */

import { WireID } from "../domain/render.ts";
import type { Event, Scope, Step, Task } from "../domain/task.ts";
import { taskHealth } from "../domain/task.ts";

/** The wire shape of a task event, and of the task endpoints. */
export interface PayloadShape {
  task_id: string;
  agent_id: string;
  parent_task_id?: string;
  conversation_id?: string;
  trigger: string;
  title: string;
  status: string;
  /** Derived from the heartbeat: running, waiting, stuck, or done. */
  health: string;
  /** How far down a handoff chain the task is. */
  depth: number;
  summary?: string;
  stopped_reason?: string;
  waiting_reason?: string;
  waiting_draft_id?: string;
  workflow_id?: string;
  temporal_run_id?: string;
  step_count: number;
  input_tokens: number;
  output_tokens: number;
  cost_micros: number;
  started_at?: string;
  finished_at?: string;
  heartbeat_at?: string;
  created_at: string;
}

/** The wire shape of one recorded round. */
export interface StepPayload {
  step_id: string;
  seq: number;
  kind: string;
  tool_name?: string;
  tool_label?: string;
  input?: unknown;
  output?: unknown;
  pruned: boolean;
  created_at: string;
  input_tokens: number;
  output_tokens: number;
}

/**
 * Formats a moment the way Go's `time.RFC3339Nano` does: the fractional second
 * is written only when it is non-zero, and its trailing zeros are trimmed.
 */
export function wireTime(value: Date | null): string {
  if (value === null) return "";

  const iso = value.toISOString();
  const dot = iso.indexOf(".");
  if (dot < 0) return iso;

  const fraction = iso.slice(dot + 1, iso.length - 1).replace(/0+$/, "");
  return fraction === "" ? `${iso.slice(0, dot)}Z` : `${iso.slice(0, dot)}.${fraction}Z`;
}

/** Renders one task for a client. */
export function Payload(task: Task, now: Date): PayloadShape {
  const payload: PayloadShape = {
    task_id: task.id,
    agent_id: WireID(task.agentId),
    trigger: task.trigger,
    title: task.title,
    status: task.status,
    health: taskHealth(task, now),
    depth: task.depth,
    step_count: task.stepCount,
    input_tokens: task.inputTokens,
    output_tokens: task.outputTokens,
    cost_micros: task.costMicros,
    created_at: wireTime(task.createdAt),
  };

  if (task.parentTaskId) payload.parent_task_id = WireID(task.parentTaskId);
  if (task.conversationId) payload.conversation_id = WireID(task.conversationId);
  if (task.summary) payload.summary = task.summary;
  if (task.stoppedReason) payload.stopped_reason = task.stoppedReason;
  if (task.waitingReason) payload.waiting_reason = task.waitingReason;
  if (task.waitingDraftId) payload.waiting_draft_id = WireID(task.waitingDraftId);
  if (task.workflowId) payload.workflow_id = task.workflowId;
  if (task.temporalRunId) payload.temporal_run_id = task.temporalRunId;
  if (task.startedAt) payload.started_at = wireTime(task.startedAt);
  if (task.finishedAt) payload.finished_at = wireTime(task.finishedAt);
  if (task.heartbeatAt) payload.heartbeat_at = wireTime(task.heartbeatAt);

  return payload;
}

/**
 * Renders one step for a client. A pruned step keeps its shape and loses its
 * contents, which is what makes the history readable after the retention job runs.
 */
export function StepWire(step: Step): StepPayload {
  const payload: StepPayload = {
    step_id: step.id,
    seq: step.seq,
    kind: step.kind,
    pruned: step.pruned,
    created_at: wireTime(step.createdAt),
    input_tokens: step.inputTokens,
    output_tokens: step.outputTokens,
  };

  if (step.toolName) payload.tool_name = step.toolName;
  if (step.toolLabel) payload.tool_label = step.toolLabel;
  if (!step.pruned) {
    payload.input = step.input;
    payload.output = step.output;
  }
  return payload;
}

/** Builds one event for the activity stream. */
export function taskEvent(
  scope: Scope,
  task: Task,
  eventType: string,
  payload: PayloadShape,
): Event {
  return {
    workspaceId: scope.workspaceId ?? "",
    type: eventType,
    actorAgentId: task.agentId,
    actorUserId: "",
    conversationId: task.conversationId,
    taskId: task.id,
    draftId: "",
    payload,
  };
}
