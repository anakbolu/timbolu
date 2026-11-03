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
 * The health service.
 *
 * The database is required for API operation: a missing or unreachable database
 * marks readiness as unavailable. Redis remains optional (not_configured is acceptable,
 * but an unreachable configured Redis marks readiness as unavailable). Realtime is
 * informative only.
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

    // Database is mandatory for the API process: not_configured or unavailable
    // both drop overall status to unavailable.
    // Redis is optional: not_configured is fine, but unavailable fails readiness.
    // Realtime is informative only (visible in components, never fails readiness).
    const report: Report = { status: StatusOK, components };
    if (database.status !== StatusOK || redis.status === StatusUnavailable) {
      report.status = StatusUnavailable;
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
