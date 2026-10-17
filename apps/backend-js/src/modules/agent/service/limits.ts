/**
 * Installs the workspace's package as the registry's cap.
 *
 * It exists because of the order things are built in: the registry is assembled
 * with the rest of the tenancy services, and the billing module that knows what
 * a package allows is assembled after them. A registry left unwired is a
 * registry whose own configured cap applies.
 *
 * Ported 1:1 from internal/modules/agent/service/limits.go.
 */

import type { PlanLimits, Service } from "../domain/agent.ts";
import { Service as ConcreteService } from "./service.ts";

/** Installs the plan's Bolu allowance on an already-built service. */
export function usePlanLimits(target: Service, limits: PlanLimits): void {
  if (target instanceof ConcreteService) {
    target.usePlanLimits(limits);
  }
}
