/**
 * The fallback chain: what the Bolu asked for, then the workspace
 * configuration, then the platform default.
 *
 * Ported from internal/modules/llm/service/resolver.go. The order is data:
 * `is_default` and `priority` come from the settings screen, and the Bolu
 * override only ever moves one provider to the front or changes the model.
 */

import type { AgentOverride, ProviderConfig, Scope } from "../domain/gateway.ts";
import { ErrNoProvider, llmError } from "../domain/provider.ts";
import { requireWorkspace, type Service } from "./service.ts";

/** The ordered list of providers to try, or ErrNoProvider. */
export async function resolve(
  s: Service,
  ctx: AbortSignal | undefined,
  scope: Scope,
): Promise<ProviderConfig[]> {
  requireWorkspace(scope);

  const stored = await s.deps.store.list(ctx, scope.workspaceId);

  const workspace: ProviderConfig[] = [];
  const byId = new Map<string, ProviderConfig>();
  for (const provider of stored) {
    if (!provider.enabled) continue;

    const cfg = s.open(provider);
    workspace.push(cfg);
    byId.set(cfg.id, cfg);
  }

  let override: AgentOverride = {};
  if (scope.agentId) {
    override = await s.deps.agents.agentModel(ctx, scope.workspaceId, scope.agentId);
  }

  let chain = applyOverride(s, scope.workspaceId, workspace, byId, override);
  if (chain.length === 0) {
    throw llmError(ErrNoProvider, "atur penyedia di Pengaturan → Model");
  }
  if (chain.length > s.settings.chainLimit) {
    // A long tail costs a round trip each before the caller gets an answer;
    // three providers is already generous for a provider outage.
    chain = chain.slice(0, s.settings.chainLimit);
  }

  return chain;
}

/** Applies the Bolu override to the workspace chain. */
function applyOverride(
  s: Service,
  workspaceId: string,
  workspace: ProviderConfig[],
  byId: Map<string, ProviderConfig>,
  override: AgentOverride,
): ProviderConfig[] {
  const base = withPlatformDefault(s, workspace);

  if (override.adapter && override.model) {
    // A provider that exists only for this Bolu: the workspace chain stays
    // behind it as the fallback.
    return [standalone(s, workspaceId, override), ...base];
  }

  if (override.provider_id) {
    const pinned = byId.get(override.provider_id);
    if (pinned) {
      const head = { ...pinned };
      if (override.max_tokens && override.max_tokens > 0) head.maxTokens = override.max_tokens;
      return [head, ...base.filter((cfg) => cfg.id !== pinned.id)];
    }
    // A pinned provider that was deleted or disabled is not an error the task
    // should die of: the chain falls back to the workspace order.
    return base;
  }

  if (override.model && base.length > 0) {
    // Only the model differs: same endpoint, cheaper or larger model.
    const head = { ...base[0]!, model: override.model };
    if (override.max_tokens && override.max_tokens > 0) head.maxTokens = override.max_tokens;
    return [head, ...base.slice(1)];
  }

  return base;
}

/**
 * Turns a Bolu's own configuration into a provider. The key and the context
 * window come from the override when it names them, and from the platform
 * default when it shares the adapter.
 */
function standalone(s: Service, workspaceId: string, override: AgentOverride): ProviderConfig {
  const cfg: ProviderConfig = {
    id: "",
    workspaceId,
    name: "Bolu",
    adapter: override.adapter ?? "",
    baseUrl: override.base_url ?? "",
    model: override.model ?? "",
    apiKey: override.api_key ?? "",
    priority: 0,
    maxTokens: override.max_tokens ?? 0,
    isDefault: false,
    contextTokens: 0,
    enabled: true,
    createdAt: new Date(0),
    updatedAt: new Date(0),
  };

  if (s.deps.default.adapter === override.adapter) {
    if (!cfg.apiKey) cfg.apiKey = s.deps.default.apiKey;
    if (!cfg.baseUrl) cfg.baseUrl = s.deps.default.baseUrl;
    if (!cfg.maxTokens) cfg.maxTokens = s.deps.default.maxTokens;
    cfg.contextTokens = s.deps.default.contextTokens;
  }

  return cfg;
}

/**
 * Appends the platform-level provider, which is what a workspace that
 * configured nothing of its own calls.
 *
 * It is deliberately not appended behind a workspace's own providers: a
 * workspace that chose its providers must not quietly spend the platform's key
 * as a last resort.
 */
function withPlatformDefault(s: Service, workspace: ProviderConfig[]): ProviderConfig[] {
  if (workspace.length > 0) return workspace;

  const def = s.deps.default;
  if (!def.adapter || !def.model) return workspace;
  return [def];
}
