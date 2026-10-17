/**
 * The health module use cases. Depends only on the module's domain contracts.
 *
 * Ported from internal/modules/health/service/service.go.
 */

import type { Checker, Component, Report, Repository } from "../domain/health.ts";
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

  constructor(database: Repository | null, redis: Checker | null) {
    this.database = database;
    this.redis = redis;
  }

  /** Reports the process state without touching any dependency. */
  liveness(): Report {
    return { status: StatusOK };
  }

  /** Checks every configured dependency and reports per-component state. */
  async readiness(): Promise<Report> {
    const components: Component[] = [await check("database", this.database)];

    if (this.redis === null) {
      components.push({ name: "redis", status: StatusNotConfigured });
    } else {
      components.push(await check("redis", this.redis));
    }

    const report: Report = { status: StatusOK, components };
    for (const component of components) {
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
