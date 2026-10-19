/**
 * Installs the trail writer a model configuration change is recorded through.
 *
 * Ported from internal/modules/llm/service/audit.go. It exists because the
 * gateway is assembled before the approval module's repository is wired into
 * the container. Left unwired, a provider change happens and leaves no line.
 */

import type { Gateway } from "../domain/gateway.ts";
import type { Auditor } from "../domain/provider.ts";
import { Service } from "./service.ts";

export function useAuditor(target: Gateway, auditor: Auditor): void {
  if (target instanceof Service) {
    target.deps.auditor = auditor;
  }
}
