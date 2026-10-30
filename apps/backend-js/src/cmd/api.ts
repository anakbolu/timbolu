/**
 * Command api runs the HTTP and WebSocket surface: it accepts every user
 * request, opens the realtime connection, and starts long-running work on
 * Temporal instead of doing it in the request.
 *
 * Ported from apps/backend/cmd/api/main.go.
 */

import { loadDotEnv } from "../platform/config/dotenv.ts";
import { defaultServiceName } from "../platform/config/config.ts";
import { newContainer } from "../container/container.ts";
import { registerRoutes } from "../container/handler.ts";
import { newApp } from "../container/http.ts";
import { serveOptions } from "./serve-options.ts";
import { requestContext } from "../platform/middleware/request_context.ts";
import { tracing } from "../platform/middleware/tracing.ts";

const SHUTDOWN_TIMEOUT_MS = 10_000;

async function run(): Promise<void> {
  await loadDotEnv();

  const container = await newContainer();
  const { config, log } = container;

  // This binary names itself in the traces, so the API and the workers are
  // separate services in a collector rather than one process that appears
  // everywhere.
  defaultServiceName(config.telemetry, "bolu-api");

  const address = config.http.address || ":8080";
  const [host, port] = splitAddress(address);

  const app = newApp({
    log,
    cors: { origins: config.application.corsOrigins ?? [] },
    tracing: () => tracing(),
    requestContext: () => requestContext(log),
  });

  registerRoutes(app, container);

  log.info(
    {
      environment: config.application.environment,
      address,
      database: container.db !== null,
      redis: container.redis !== null,
    },
    "starting api",
  );

  // The options carry the websocket handler the realtime routes upgrade into;
  // without it every handshake throws and the error handler answers 500.
  const server = Bun.serve(serveOptions({ hostname: host, port, fetch: app.fetch }));

  log.info({ url: `http://${host}:${port}` }, "api listening");

  // The billing sweep is a loop in this process rather than a durable workflow,
  // because nothing in it waits: each pass recomputes the allowance from the
  // times and the ledger, so a pass that never ran is corrected by the next one.
  // The retention pass shares the process for the same reason. A deployment sets
  // the interval to 0 to switch either off.
  startLoop("billing sweep", config.billing.sweepInterval, log, async () => {
    if (!container.billingService) return;
    await container.billingService.sweep();
  });

  startLoop("retention", config.retention.interval, log, async () => {
    if (!container.retentionService) return;
    await container.retentionService.run();
  });

  const shutdown = async (signal: string): Promise<void> => {
    log.info({ signal }, "shutting down api");
    server.stop();
    await container.close();
    process.exit(0);
  };

  process.on("SIGINT", () => void shutdown("SIGINT"));
  process.on("SIGTERM", () => void shutdown("SIGTERM"));
}

/**
 * Runs one pass immediately, then every interval until the process stops.
 *
 * The first pass is immediate on purpose: a deployment that has just started is
 * exactly when a correction is most likely to be needed. A pass that throws is
 * logged and the loop continues — a sweep is a correction, not a transaction, so
 * one failure must not end it.
 */
function startLoop(name: string, intervalMs: number, log: any, pass: () => Promise<void>): void {
  if (!intervalMs || intervalMs <= 0) {
    log.info({ loop: name }, "loop is disabled");
    return;
  }

  let running = false;
  const tick = async (): Promise<void> => {
    if (running) return;
    running = true;
    try {
      await pass();
    } catch (err) {
      log.error({ err, loop: name }, "pass failed; the next one will correct it");
    } finally {
      running = false;
    }
  };

  void tick();
  const timer = setInterval(() => void tick(), intervalMs);
  // The loop must not hold the process open on its own.
  timer.unref?.();
}

/** Splits `:8080` or `127.0.0.1:8080` into a hostname and a port. */
function splitAddress(address: string): [string, number] {
  const trimmed = address.replace(/^tcp:\/\//, "");
  const idx = trimmed.lastIndexOf(":");
  if (idx === -1) return ["0.0.0.0", Number(trimmed) || 8080];
  const host = trimmed.slice(0, idx) || "0.0.0.0";
  const port = Number(trimmed.slice(idx + 1)) || 8080;
  return [host, port];
}

run().catch((err: unknown) => {
  console.error("api:", err);
  process.exit(1);
});

export { SHUTDOWN_TIMEOUT_MS };
