/**
 * The trail writer a grant change is recorded through.
 *
 * It exists for the reason the workspace module's does: the hub is assembled
 * before the approval module's repository is wired into the container, and all
 * of that happens before any request is served. Left unwired, a grant change
 * happens and leaves no line.
 *
 * Ported 1:1 from internal/modules/integration/service/audit.go.
 */

import type { Auditor } from "../domain/integration.ts";
import type { IntegrationService } from "./service.ts";

/** Installs the trail writer a grant change is recorded through. */
export function useAuditor(target: IntegrationService, auditor: Auditor): void {
  target.deps.auditor = auditor;
}
