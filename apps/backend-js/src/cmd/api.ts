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

  const server = Bun.serve({
    hostname: host,
    port,
    fetch: app.fetch,
    // The live stream is a long-lived response: Bun's default idle timeout would
    // cut it. 0 disables the timeout, which is what the SSE route needs.
    idleTimeout: 0,
  });

  log.info({ url: `http://${host}:${port}` }, "api listening");

  const shutdown = async (signal: string): Promise<void> => {
    log.info({ signal }, "shutting down api");
    server.stop();
    await container.close();
    process.exit(0);
  };

  process.on("SIGINT", () => void shutdown("SIGINT"));
  process.on("SIGTERM", () => void shutdown("SIGTERM"));
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
