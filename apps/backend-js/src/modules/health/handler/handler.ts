/**
 * The health module over HTTP. Talks to the domain service interface only.
 *
 * Ported from internal/modules/health/handler/handler.go. The two probes sit at
 * the root as well as under the API prefix, so container health checks and the
 * frontend can use the same host.
 */

import { Hono } from "hono";
import { snapshot as buildSnapshot } from "../../../platform/buildinfo/buildinfo.ts";
import type { AppContext, AppEnv } from "../../../platform/context.ts";
import type { Logger } from "../../../platform/logger.ts";
import { error, ok } from "../../../platform/response.ts";
import { StatusOK } from "../domain/health.ts";
import type { ServicePort } from "../service/service.ts";

/** Serves the probes. */
export class Handler {
  private readonly service: ServicePort;
  private readonly log: Logger;

  constructor(service: ServicePort, log: Logger) {
    this.service = service;
    this.log = log;
  }

  /**
   * Answers "is the process alive". Never touches a dependency, so a database
   * outage does not restart the container. It also reports the build identity,
   * which is how a deploy is verified from outside: a green workflow only means
   * the deploy hook was accepted. Mirrors Go `liveReport`.
   */
  live = async (c: AppContext): Promise<Response> => {
    return ok(c, "alive", { ...this.service.liveness(), ...buildSnapshot() });
  };

  /**
   * Answers "can this process serve traffic". Every configured dependency is
   * checked; a failure returns 503 with the per-component detail.
   */
  ready = async (c: AppContext): Promise<Response> => {
    const report = await this.service.readiness();
    if (report.status !== StatusOK) {
      const summary = (report.components ?? [])
        .map((component) => `${component.name}=${component.status}`)
        .join(" ");
      this.log.warn({ report: summary }, "readiness check failed");
      return error(c, 503, "not ready", "not_ready", report);
    }
    return ok(c, "ready", report);
  };
}

/**
 * Registers the probes on both the root path and the configured API prefix.
 * Mirrors Go `Routes(app, h, apiPrefix)`.
 */
export function routes(app: Hono<AppEnv>, h: Handler, apiPrefix: string): void {
  app.get("/health", h.live);
  app.get("/ready", h.ready);

  if (apiPrefix !== "" && apiPrefix !== "/") {
    const group = new Hono<AppEnv>();
    group.get("/health", h.live);
    group.get("/ready", h.ready);
    app.route(apiPrefix, group);
  }
}
