/**
 * Installs the trail writer a membership change is recorded through.
 *
 * It exists for the reason usePlanLimits does: the tenancy services are
 * assembled before the approval module's repository is wired into the
 * container. A service left unwired records nothing — the change happens and
 * leaves no line.
 *
 * Ported 1:1 from internal/modules/workspace/service/audit.go.
 */

import type { Auditor, Service } from "../domain/workspace.ts";
import { Service as ConcreteService } from "./service.ts";

/** Installs the trail writer a membership change is recorded through. */
export function useAuditor(target: Service, auditor: Auditor): void {
  if (target instanceof ConcreteService) {
    target.useAuditor(auditor);
  }
}
