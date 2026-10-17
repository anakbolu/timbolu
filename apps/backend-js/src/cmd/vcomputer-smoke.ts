/**
 * Command vcomputer-smoke runs one virtual computer end to end: it starts a worker
 * on a queue of its own, executes the smoke workflow on it, and stops the sandbox
 * the run made.
 *
 * Ported from apps/backend/cmd/vcomputer-smoke/main.go. It is a development tool
 * and it is not deployed. What it is for is the seam no unit test covers: a real
 * Temporal server, a real control plane, a real sandbox, and the activities this
 * backend registers against them.
 *
 * It is not the agent worker. The worker here lives for one run and registers
 * nothing but the virtual computer's activities, so a run says nothing about
 * whether the agent worker's task loop works.
 *
 *   VCOMPUTER_CONTROL_URL=http://localhost:8090 VCOMPUTER_CONTROL_TOKEN=... \
 *     bun run src/cmd/vcomputer-smoke.ts --bot bot-1 --url https://example.com
 *
 * The run exits non-zero on the first failure and prints what the workflow
 * observed as JSON, so a person can read it and a script can diff it.
 */

import { fileURLToPath } from "node:url";
import { Client, Connection } from "@temporalio/client";
import { loadDotEnv } from "../platform/config/dotenv.ts";
import { load as loadConfig } from "../platform/config/init.ts";
import { createLogger } from "../platform/logger.ts";
import { activityMap } from "../worker/worker.ts";
import { fromConfig as vcomputerActivities } from "../modules/vcomputer/temporal/activities.ts";
import { SmokeWorkflowName } from "../modules/vcomputer/workflow/smoke.ts";

/** A smoke run is a person watching it; a sandbox that never answers must not hang them. */
const RUN_TIMEOUT_MS = 5 * 60 * 1000;

const WORKFLOWS_PATH = fileURLToPath(new URL("../worker/workflows.ts", import.meta.url));

interface Flags {
  temporal: string;
  namespace: string;
  queue: string;
  bot: string;
  url: string;
  clickX: number;
  clickY: number;
  keepSession: boolean;
  out: string;
}

function envOr(key: string, fallback: string): string {
  const value = process.env[key];
  return value === undefined || value === "" ? fallback : value;
}

function parseFlags(argv: string[]): Flags {
  const get = (name: string, fallback: string): string => {
    const i = argv.indexOf(`--${name}`);
    if (i !== -1 && argv[i + 1] !== undefined) return argv[i + 1];
    const eq = argv.find((a) => a.startsWith(`--${name}=`));
    return eq ? eq.slice(name.length + 3) : fallback;
  };
  const has = (name: string): boolean => argv.includes(`--${name}`);

  return {
    temporal: get("temporal", envOr("TEMPORAL_HOST_PORT", "127.0.0.1:7233")),
    namespace: get("namespace", envOr("TEMPORAL_NAMESPACE", "default")),
    // The queue is its own rather than the agent worker's, so a smoke run cannot be
    // picked up by a deployed worker and cannot leave anything on a queue that
    // carries real work.
    queue: get("queue", "vcomputer-smoke"),
    bot: get("bot", ""),
    url: get("url", ""),
    clickX: Number(get("click-x", "0")) || 0,
    clickY: Number(get("click-y", "0")) || 0,
    keepSession: has("keep-session"),
    out: get("out", ""),
  };
}

async function run(): Promise<void> {
  loadDotEnv();
  const flags = parseFlags(process.argv.slice(2));

  if (!flags.bot) throw new Error("--bot is required: name the bot whose virtual computer to drive");
  if (!flags.url) throw new Error("--url is required: name the page to point the browser at");
  if (flags.out && !flags.keepSession) {
    throw new Error("--out needs --keep-session: the capture is asked of a sandbox that would otherwise be down");
  }

  const config = await loadConfig();
  const log = createLogger({ level: config.logging.level });

  const activities = activityMap(vcomputerActivities(config.vComputer as any, log));
  const { Worker, NativeConnection } = await import("@temporalio/worker");

  const connection = await NativeConnection.connect({ address: flags.temporal });
  const worker = await Worker.create({
    connection,
    namespace: flags.namespace,
    taskQueue: flags.queue,
    workflowsPath: WORKFLOWS_PATH,
    activities,
  });

  const client = new Client({
    connection: await Connection.connect({ address: flags.temporal }),
    namespace: flags.namespace,
  });

  const deadline = new AbortController();
  const timer = setTimeout(() => deadline.abort(), RUN_TIMEOUT_MS);

  try {
    // The worker runs only for this one workflow: a run that outlived its task
    // would keep polling a queue nobody meant to serve.
    await worker.runUntil(async () => {
      const handle = await client.workflow.start(SmokeWorkflowName, {
        taskQueue: flags.queue,
        workflowId: `vcomputer-smoke-${Date.now()}`,
        args: [
          {
            botId: flags.bot,
            url: flags.url,
            clickX: flags.clickX,
            clickY: flags.clickY,
            keepSession: flags.keepSession,
            out: flags.out,
          },
        ],
      });

      const result = await handle.result();
      process.stdout.write(`${JSON.stringify(result, null, 2)}\n`);
    });
  } finally {
    clearTimeout(timer);
    await worker.shutdown();
    await connection.close();
  }
}

run().catch((err: unknown) => {
  console.error(`vcomputer-smoke: ${err instanceof Error ? err.message : String(err)}`);
  process.exit(1);
});
