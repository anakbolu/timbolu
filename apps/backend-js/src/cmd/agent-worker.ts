/**
 * Command agent-worker runs the Temporal worker that executes agent tasks: the
 * LLM loop, tool calls, approvals, and handoffs.
 *
 * Ported from apps/backend/cmd/agent-worker/main.go. The workflows and activities
 * are built by the container, so the worker and the API share one assembly point:
 * the API starts a task and signals it, and this process is where it runs.
 */

import { fileURLToPath } from "node:url";
import { loadDotEnv } from "../platform/config/dotenv.ts";
import { defaultServiceName } from "../platform/config/config.ts";
import { newContainer } from "../container/container.ts";
import { newWorker } from "../worker/worker.ts";

const WORKFLOWS_PATH = fileURLToPath(new URL("../worker/workflows.ts", import.meta.url));

async function run(): Promise<void> {
  loadDotEnv();

  const container = await newContainer();
  const { config, log } = container;

  // This binary names itself in the traces, so the API and the workers are
  // separate services in a collector rather than one process that appears
  // everywhere.
  defaultServiceName(config.telemetry, "bolu-agent-worker");

  const taskQueue = config.temporal.taskQueueAgent || "agent";
  const worker = await newWorker({ config, log, db: container.db }, taskQueue, WORKFLOWS_PATH);

  log.info({ taskQueue, temporal: config.temporal.hostPort }, "agent worker running");

  const shutdown = async (signal: string): Promise<void> => {
    log.info({ signal }, "shutting down agent worker");
    await worker.shutdown();
    await container.close();
    process.exit(0);
  };
  process.on("SIGINT", () => void shutdown("SIGINT"));
  process.on("SIGTERM", () => void shutdown("SIGTERM"));

  await worker.run();
}

run().catch((err: unknown) => {
  console.error("agent-worker:", err);
  process.exit(1);
});
