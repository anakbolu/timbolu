/**
 * The task runtime's Temporal seam.
 *
 * Ported from apps/backend/internal/container/task.go (`taskStarter`). The task
 * service decides what a task is; this decides where it runs, which is why it is
 * a port rather than a direct call.
 *
 * The workflow id is deterministic — `task-<id>` — which is what makes starting
 * the same task twice harmless: Temporal answers "already started" instead of
 * running it twice.
 */

import { Client, Connection, WorkflowNotFoundError } from "@temporalio/client";
import type { Config } from "../platform/config/config.ts";
import type { Logger } from "../platform/logger.ts";
import { AgentTaskWorkflow, WorkflowName } from "../modules/task/workflow/workflow.ts";

/** The deterministic id of one task's workflow. */
export function workflowId(taskId: string): string {
  return `task-${taskId}`;
}

export interface TemporalStarter {
  start(ctx: AbortSignal | undefined, scope: any, input: any): Promise<{ workflowId: string; runId: string }>;
  signal(ctx: AbortSignal | undefined, scope: any, taskId: string, name: string, payload: unknown): Promise<void>;
}

/**
 * Builds the starter. It returns null when Temporal is not configured, which is
 * the same shape as a deployment without a task runtime: tasks are recorded and
 * stay queued rather than the process refusing to start.
 */
export async function newTemporalStarter(
  config: Config,
  log: Logger,
): Promise<TemporalStarter | null> {
  const address = config.temporal.hostPort;
  if (!address) {
    log.warn("the task runtime is not configured: tasks are recorded and stay queued");
    return null;
  }

  let connection: Connection;
  try {
    connection = await Connection.connect({
      address,
      ...(config.temporal.tls ? { tls: true } : {}),
      ...(config.temporal.apiKey ? { apiKey: config.temporal.apiKey } : {}),
    });
  } catch (err) {
    log.error({ err }, "temporal is unreachable: tasks are recorded and stay queued");
    return null;
  }

  const client = new Client({ connection, namespace: config.temporal.namespace || "default" });
  const taskQueue = config.temporal.taskQueueAgent || "agent";

  return {
    async start(_ctx, scope, input) {
      const handle = await client.workflow.start(WorkflowName, {
        taskQueue,
        workflowId: workflowId(input.taskId),
        args: [
          {
            TaskID: input.taskId,
            WorkspaceID: scope.workspaceId,
            AgentID: input.agentId,
            ConversationID: input.conversationId ?? "",
            ReplyMessageID: input.replyMessageId ?? "",
            Title: input.title,
            Prompt: input.prompt,
            Depth: input.depth ?? 0,
          },
        ],
      });
      return { workflowId: handle.workflowId, runId: handle.firstExecutionRunId };
    },

    async signal(_ctx, _scope, taskId, name, payload) {
      try {
        const handle = client.workflow.getHandle(workflowId(taskId));
        await handle.signal(name, payload);
      } catch (err) {
        // A workflow that already finished answers "not found", which is not an
        // error: the decision arrived after the task stopped caring.
        if (err instanceof WorkflowNotFoundError) return;
        throw err;
      }
    },
  };
}

export { AgentTaskWorkflow };
