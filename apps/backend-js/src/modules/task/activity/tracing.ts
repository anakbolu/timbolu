/**
 * The task runtime's spans and its log fields.
 *
 * The Temporal interceptor already gives every activity a span of its own. What
 * is added here is the semantics on top of that skeleton: which Bolu, which task,
 * which tool, which model, and how much it cost.
 *
 * Two rules hold across all of it. **The identifiers and the counts travel; the
 * content does not.** And **the log fields are on every line a task writes**:
 * workspace_id, agent_id, task_id, and workflow_id, so a line can be followed
 * from the trace to the log and back.
 *
 * Ported 1:1 from internal/modules/task/activity/tracing.go.
 */

import * as activity from "@temporalio/activity";
import type { Logger } from "../../../platform/logger.ts";
import { nopLogger } from "../../../platform/logger.ts";
import type { AttributeValue, Span, Tracer } from "../../../platform/telemetry/telemetry.ts";
import { SpanStatusCode, noopTracer } from "../../../platform/telemetry/telemetry.ts";
import type { Task } from "../domain/task.ts";

/** What the tracing helpers need from the activity set. */
export interface TracingDeps {
  logger?: Logger | null;
  tracer?: Tracer | null;
}

/** Opens the span for one step of a task. */
export function span(
  deps: TracingDeps,
  name: string,
  task: Task,
  extra: Record<string, AttributeValue> = {},
): Span {
  const tracer = deps.tracer ?? noopTracer();
  const attributes: Record<string, AttributeValue> = {
    workspace_id: task.workspaceId,
    task_id: task.id,
    agent_id: task.agentId,
    task_depth: task.depth,
    ...extra,
  };
  const workflowId = workflowIDOf();
  if (workflowId !== "") {
    attributes.workflow_id = workflowId;
  }

  return tracer.startSpan(name, { attributes });
}

/**
 * Reads the workflow execution this activity belongs to.
 *
 * It is read from Temporal rather than passed through the request: the workflow
 * id is what a person quotes when they report a task that misbehaved, and the
 * runtime already knows it.
 */
export function workflowIDOf(): string {
  if (!activityContextActive()) {
    // A unit test calling the activity directly is not inside a workflow, and
    // asking Temporal for the execution would throw.
    return "";
  }

  return activity.Context.current().info.workflowExecution?.workflowId ?? "";
}

/** Reports whether this code is running inside a Temporal activity. */
export function activityContextActive(): boolean {
  try {
    activity.Context.current();
    return true;
  } catch {
    return false;
  }
}

/**
 * The logger a task's lines are written through.
 *
 * The four fields are always together and always in the same names, which is the
 * point: a log search for one task's lines is one query.
 */
export function taskLogger(deps: TracingDeps, task: Task): Logger {
  const base = deps.logger ?? nopLogger;
  const fields: Record<string, string> = {
    workspace_id: task.workspaceId,
    agent_id: task.agentId,
    task_id: task.id,
  };
  const workflowId = workflowIDOf();
  if (workflowId !== "") {
    fields.workflow_id = workflowId;
  }
  return base.child(fields);
}

/**
 * Opens the span for one tool call.
 *
 * The tool's arguments are never attributes. A tool call is where a customer's
 * data enters the run, and a trace is a store the product's own retention does
 * not reach.
 */
export function toolSpan(deps: TracingDeps, task: Task, name: string): Span {
  return span(deps, `tool ${name}`, task, { tool_name: name });
}

/** Records the error, if any, and closes the span. */
export function end(spanValue: Span, err: unknown): void {
  if (err !== null && err !== undefined) {
    spanValue.recordException(err);
    spanValue.setStatus({ code: SpanStatusCode.ERROR, message: "the step failed" });
  }
  spanValue.end();
}
