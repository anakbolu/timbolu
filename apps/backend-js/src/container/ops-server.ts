/**
 * The operations listener: the surface a process with no HTTP API of its own
 * answers probes and scrapes on.
 *
 * Ported from apps/backend/internal/container/ops_server.go.
 *
 * The API answers `/health` and `/ready` through its own router, because it
 * already has a listener and a route table. A worker has neither — it polls
 * Temporal and records most of the product's measures — so without this an
 * orchestrator has nothing to probe:
 *
 *   - `/health` is liveness. It never touches a dependency, and it answers as long
 *     as the process is running, which is the question a restart is decided on.
 *   - `/ready` is readiness. It checks every configured dependency and answers 503
 *     while one is down, which is the question a rollout is decided on. A worker
 *     that cannot reach Postgres must not be told it is ready: a rollout that
 *     continued would put more tasks on a process that cannot run them.
 *
 * It is off unless the deployment names an address, so a deployment whose workers
 * have no probe is unchanged. What it serves is deliberately not the API: a worker
 * that grew a general HTTP surface would be a second API with none of the guards.
 */

import { Hono } from "hono";
import type { AppEnv } from "../platform/context.ts";
import type { Logger } from "../platform/logger.ts";
import type { Handler as HealthHandler } from "../modules/health/handler/handler.ts";

/** Splits `:9101` or `127.0.0.1:9101` into a hostname and a port. */
function splitAddress(address: string): [string, number] {
  const trimmed = address.replace(/^tcp:\/\//, "");
  const idx = trimmed.lastIndexOf(":");
  if (idx === -1) return ["0.0.0.0", Number(trimmed) || 9101];
  const host = trimmed.slice(0, idx) || "0.0.0.0";
  const port = Number(trimmed.slice(idx + 1)) || 9101;
  return [host, port];
}

/** A started operations listener, or null when the deployment did not ask for one. */
export interface OpsServer {
  stop(): void;
}

/**
 * Starts the operations listener. Returns null when no address is configured.
 *
 * A port that is already taken is reported and swallowed: the process's job is to
 * run tasks, and refusing to start because a monitoring port was busy would turn a
 * monitoring problem into an outage.
 */
export function startOpsServer(
  address: string,
  health: HealthHandler | null,
  log: Logger,
): OpsServer | null {
  if (address === "") return null;

  if (health === null) {
    log.warn({ address }, "the operations endpoint has no health service; not starting it");
    return null;
  }

  const app = new Hono<AppEnv>();
  // The same handlers the API serves, so the two surfaces cannot disagree about
  // what "ready" means.
  app.get("/health", health.live);
  app.get("/ready", health.ready);

  const [hostname, port] = splitAddress(address);

  let server: ReturnType<typeof Bun.serve>;
  try {
    server = Bun.serve({ hostname, port, fetch: app.fetch });
  } catch (err) {
    log.error(
      { address, err },
      "the operations endpoint could not start; the process runs without it",
    );
    return null;
  }

  log.info({ address: `http://${hostname}:${port}` }, "serving the operations endpoint");
  return { stop: () => server.stop() };
}
