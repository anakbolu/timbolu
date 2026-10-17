/**
 * Installs the workspace's package as the member cap.
 *
 * It exists for the reason the registry's does: the tenancy services are
 * assembled before the billing module that knows what a package allows, and all
 * of that happens before any request is served. A workspace service left
 * unwired caps nothing.
 *
 * Ported 1:1 from internal/modules/workspace/service/limits.go.
 */

import type { PlanLimits, Service } from "../domain/workspace.ts";
import { Service as ConcreteService } from "./service.ts";

/** Installs the package's member cap on an already-built service. */
export function usePlanLimits(target: Service, limits: PlanLimits): void {
  if (target instanceof ConcreteService) {
    target.usePlanLimits(limits);
  }
}
