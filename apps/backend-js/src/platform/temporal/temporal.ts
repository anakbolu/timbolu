/**
 * The Temporal client shared by the API and the workers.
 *
 * Ported from apps/backend/internal/platform/temporal/temporal.go. The API
 * starts workflows and sends approval signals; the workers register workflows
 * and activities on their own task queues.
 *
 * One difference is forced by the SDK rather than chosen: the TypeScript client
 * has no per-client logger (the Go one sets `Options.Logger`). The SDK reads
 * `Runtime.logger`, which is process-wide, so the deployment installs the
 * adapter from `./logger.ts` there and the client is built without it.
 */

import { Client, Connection, WorkflowIdReusePolicy } from "@temporalio/client";
import type { ClientInterceptors } from "@temporalio/client";
import type { WorkflowStartOptions } from "@temporalio/client";

/** The health status Temporal reports when it can serve requests. */
const SERVING_STATUS = "SERVING";

/** Bound on the startup health check, in milliseconds. */
const HEALTH_CHECK_TIMEOUT_MS = 5_000;

/**
 * The workflow service name the health check is asked about. The Go SDK's
 * `CheckHealth` asks the same service, which is what makes a "SERVING" answer
 * mean the frontend is ready rather than merely the process being up.
 */
const WORKFLOW_SERVICE = "temporal.api.workflowservice.v1.WorkflowService";

/** Holds the Temporal connection settings. */
export interface Config {
  hostPort: string;
  namespace?: string;
  taskQueueAgent?: string;
  taskQueueIntegration?: string;
  /** Set for Temporal Cloud; self-hosted dev uses neither this nor `apiKey`. */
  tls?: boolean;
  apiKey?: string;
  /** Installed on the dialled client; the container passes the tracing interceptor. */
  interceptors?: ClientInterceptors;
}

/**
 * Wraps the Temporal client plus the task queues this deployment uses.
 *
 * A method on a null receiver answers the zero value in Go; here `undefined`
 * is the only way to express that, so every accessor accepts it and callers
 * that hold an optional client keep working unchanged.
 */
export class ClientHandle {
  private constructor(
    private readonly cli: Client,
    private readonly namespace: string,
    private readonly taskQueueAgent: string,
    private readonly taskQueueIntegration: string,
  ) {}

  /**
   * Dials Temporal. Fails fast when the endpoint is unreachable so a
   * misconfigured worker does not start half-alive.
   */
  static async connect(cfg: Config): Promise<ClientHandle> {
    if (!cfg.hostPort) {
      throw new Error("temporal host port is required");
    }

    const namespace = cfg.namespace || "default";

    const connection = await Connection.connect({
      address: cfg.hostPort,
      ...(cfg.tls ? { tls: true } : {}),
      ...(cfg.apiKey ? { apiKey: cfg.apiKey } : {}),
    });

    // A worker that starts "successfully" but cannot reach Temporal is worse
    // than one that refuses to start, so the connection is verified here.
    try {
      const answer = await connection.withDeadline(Date.now() + HEALTH_CHECK_TIMEOUT_MS, () =>
        connection.healthService.check({ service: WORKFLOW_SERVICE }),
      );
      if (String(answer.status) !== SERVING_STATUS) {
        throw new Error(`temporal health check at ${cfg.hostPort}: status is ${answer.status}`);
      }
    } catch (err) {
      await connection.close();
      throw err instanceof Error && err.message.startsWith("temporal health check")
        ? err
        : new Error(`temporal health check at ${cfg.hostPort}: ${String(err)}`);
    }

    const cli = new Client({
      connection,
      namespace,
      ...(cfg.interceptors ? { interceptors: cfg.interceptors } : {}),
    });

    return new ClientHandle(
      cli,
      namespace,
      cfg.taskQueueAgent || "bolu-agent",
      cfg.taskQueueIntegration || "bolu-integration",
    );
  }

  /** The underlying Temporal client. */
  client(): Client {
    return this.cli;
  }

  /** The namespace this deployment talks to. */
  Namespace(): string {
    return this.namespace;
  }

  /** The queue the agent worker polls. */
  TaskQueueAgent(): string {
    return this.taskQueueAgent;
  }

  /** The queue the integration worker polls. */
  TaskQueueIntegration(): string {
    return this.taskQueueIntegration;
  }

  /**
   * What a caller passes to start a workflow on the agent queue.
   *
   * The workflow id is the caller's, and it is what makes a restart resume the
   * work instead of opening a second copy of it: two starts with the same id and
   * the same run collide, which is exactly the protection a task needs.
   */
  StartOptions(workflowID: string): WorkflowStartOptions {
    return {
      workflowId: workflowID,
      taskQueue: this.TaskQueueAgent(),
      // A workflow id that already ran is refused rather than reused, so a
      // duplicate dispatch cannot silently replace a finished task's history.
      workflowIdReusePolicy: WorkflowIdReusePolicy.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
    };
  }

  /** Releases the client. */
  async close(): Promise<void> {
    await this.cli.connection.close();
  }
}
