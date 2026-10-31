/**
 * The health module use cases. Depends only on the module's domain contracts.
 *
 * Ported from internal/modules/health/service/service.go.
 */

import type {
  Checker,
  Component,
  RealtimeProbe,
  Report,
  Repository,
} from "../domain/health.ts";
import { StatusNotConfigured, StatusOK, StatusUnavailable } from "../domain/health.ts";

/** What the handler calls. */
export interface ServicePort {
  liveness(): Report;
  readiness(): Promise<Report>;
}

/**
 * The health service. Both dependencies are optional: a null dependency is
 * reported as not_configured instead of failing the probe, which keeps the API
 * usable while Postgres or Redis is down.
 */
export class Service implements ServicePort {
  private readonly database: Repository | null;
  private readonly redis: Checker | null;
  private readonly realtime: RealtimeProbe | null;

  constructor(
    database: Repository | null,
    redis: Checker | null,
    realtime: RealtimeProbe | null = null,
  ) {
    this.database = database;
    this.redis = redis;
    this.realtime = realtime;
  }

  /** Reports the process state without touching any dependency. */
  liveness(): Report {
    return { status: StatusOK };
  }

  /** Checks every configured dependency and reports per-component state. */
  async readiness(): Promise<Report> {
    const database = await check("database", this.database);
    const redis =
      this.redis === null
        ? { name: "redis", status: StatusNotConfigured }
        : await check("redis", this.redis);
    const components: Component[] = [
      database,
      redis,
      await checkRealtime(this.realtime),
    ];

    // Only database and redis decide the overall status. A dead live path must
    // be visible, not fatal: restarting an API that still serves requests would
    // turn a silent realtime outage into a restart loop. Realtime is reported
    // so it can be read from /api/ready instead.
    const report: Report = { status: StatusOK, components };
    for (const component of [database, redis]) {
      if (component.status === StatusUnavailable) {
        report.status = StatusUnavailable;
        break;
      }
    }
    return report;
  }
}

async function check(name: string, checker: Checker | null): Promise<Component> {
  if (checker === null) {
    return { name, status: StatusNotConfigured };
  }
  try {
    await checker.ping();
  } catch (err) {
    return { name, status: StatusUnavailable, error: (err as Error).message };
  }
  return { name, status: StatusOK };
}

async function checkRealtime(probe: RealtimeProbe | null): Promise<Component> {
  if (probe === null) {
    return { name: "realtime", status: StatusNotConfigured };
  }
  try {
    await probe.probe();
  } catch (err) {
    return { name: "realtime", status: StatusUnavailable, error: (err as Error).message };
  }
  return { name: "realtime", status: StatusOK };
}
