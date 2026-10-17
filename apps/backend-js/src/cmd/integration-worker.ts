/**
 * Command integration-worker runs the Temporal worker that talks to the outside
 * world: OAuth token refresh, provider API calls, and webhook delivery.
 *
 * Ported from apps/backend/cmd/integration-worker/main.go. It is a separate
 * process from the agent worker because the two have different failure modes —
 * a provider outage must not stop a task from running — and separate queues let a
 * deployment scale them apart.
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

  defaultServiceName(config.telemetry, "bolu-integration-worker");

  const taskQueue = config.temporal.taskQueueIntegration || "integration";
  const worker = await newWorker({ config, log, db: container.db }, taskQueue, WORKFLOWS_PATH);

  log.info({ taskQueue, temporal: config.temporal.hostPort }, "integration worker running");

  const shutdown = async (signal: string): Promise<void> => {
    log.info({ signal }, "shutting down integration worker");
    await worker.shutdown();
    await container.close();
    process.exit(0);
  };
  process.on("SIGINT", () => void shutdown("SIGINT"));
  process.on("SIGTERM", () => void shutdown("SIGTERM"));

  await worker.run();
}

run().catch((err: unknown) => {
  console.error("integration-worker:", err);
  process.exit(1);
});
