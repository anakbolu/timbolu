/**
 * The LLM gateway data access: the provider configuration, the usage ledger,
 * and the Bolu model override.
 *
 * Ported from internal/modules/llm/repository/postgres.go and the sqlc queries
 * in repository/sqlcgen (provider.sql, usage.sql, agent_override.sql). Every
 * statement runs inside the caller's tenant scope, so Row Level Security filters
 * it even if a query forgets its workspace filter. The API keys are stored as
 * ciphertext; this file never holds a plaintext secret except while opening one.
 */

import type { Database, Scope, Tx } from "../../../platform/database/index.ts";
import type {
  AgentOverride,
  AgentStore as AgentStorePort,
  SecretBox,
  StoredProvider,
  Store as StorePort,
} from "../domain/gateway.ts";
import {
  ErrDuplicateProvider,
  ErrInvalidRequest,
  ErrProviderNotFound,
  llmError,
} from "../domain/provider.ts";
import type {
  UsageByAgent,
  UsageByTask,
  UsageDaily,
  UsageEntry,
  UsageReport,
} from "../domain/provider.ts";
import { DefaultReportPage } from "../domain/provider.ts";

/** The row shape of `llm_providers`. */
export interface LlmProviderRow {
  id: string;
  workspace_id: string;
  name: string;
  adapter: string;
  base_url: string;
  model: string;
  api_key_encrypted: Uint8Array | null;
  priority: number;
  max_tokens: number;
  context_tokens: number;
  is_default: boolean;
  enabled: boolean;
  created_at: Date;
  updated_at: Date;
}

/** The row shape of `usage_daily`. */
export interface UsageDailyRow {
  workspace_id: string;
  day: Date;
  provider: string;
  model: string;
  purpose: string;
  calls: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  cost_micros: number;
}

export interface ListUsageByAgentRow {
  agent_id: string;
  agent_name: string;
  input_tokens: number;
  output_tokens: number;
  cost_micros: number;
  calls: number;
}

export interface ListUsageByTaskRow {
  task_id: string | null;
  task_title: string;
  task_status: string;
  input_tokens: number;
  output_tokens: number;
  cost_micros: number;
  calls: number;
}

export interface SumUsageSinceRow {
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  cost_micros: number;
  calls: number;
}

/** Postgres-backed implementation of the gateway ports. */
export class Repository implements StorePort {
  constructor(private readonly db: Database) {}

  /** Every provider of the workspace, the default first and then the fallback order. */
  async list(ctx: AbortSignal | undefined, workspaceId: string): Promise<StoredProvider[]> {
    return this.read(workspaceId, async (tx) => {
      // name: ListProviders
      const rows = await tx<LlmProviderRow[]>`
        SELECT id, workspace_id, name, adapter, base_url, model, api_key_encrypted, priority, max_tokens, context_tokens, is_default, enabled, created_at, updated_at FROM llm_providers
        WHERE workspace_id = ${workspaceId}
        ORDER BY is_default DESC, priority, created_at
      `;
      return rows.map(toStoredProvider);
    });
  }

  /** One provider, or ErrProviderNotFound. */
  async get(
    ctx: AbortSignal | undefined,
    workspaceId: string,
    id: string,
  ): Promise<StoredProvider> {
    return this.read(workspaceId, async (tx) => {
      // name: GetProvider
      const rows = await tx<LlmProviderRow[]>`
        SELECT id, workspace_id, name, adapter, base_url, model, api_key_encrypted, priority, max_tokens, context_tokens, is_default, enabled, created_at, updated_at FROM llm_providers WHERE id = ${id} AND workspace_id = ${workspaceId}
      `;
      const row = rows[0];
      if (!row) throw ErrProviderNotFound;
      return toStoredProvider(row);
    });
  }

  /**
   * Inserts a provider. When it is the default, the previous default is cleared
   * in the same transaction so the workspace always has exactly one.
   */
  async create(
    ctx: AbortSignal | undefined,
    stored: StoredProvider,
  ): Promise<StoredProvider> {
    return this.write(stored.workspaceId, async (tx) => {
      if (stored.isDefault) {
        await clearDefaults(tx, stored.workspaceId, null);
      }

      try {
        // name: CreateProvider
        const rows = await tx<LlmProviderRow[]>`
          INSERT INTO llm_providers (
              workspace_id, name, adapter, base_url, model, api_key_encrypted,
              priority, max_tokens, context_tokens, is_default, enabled
          )
          VALUES (${stored.workspaceId}, ${stored.name}, ${stored.adapter}, ${stored.baseUrl}, ${stored.model}, ${stored.encryptedKey}, ${stored.priority}, ${stored.maxTokens}, ${stored.contextTokens}, ${stored.isDefault}, ${stored.enabled})
          RETURNING id, workspace_id, name, adapter, base_url, model, api_key_encrypted, priority, max_tokens, context_tokens, is_default, enabled, created_at, updated_at
        `;
        return toStoredProvider(rows[0]!);
      } catch (err) {
        throw translateWriteError(err, "create");
      }
    });
  }

  /** Replaces a provider. A null encryptedKey keeps the stored secret. */
  async update(
    ctx: AbortSignal | undefined,
    stored: StoredProvider,
  ): Promise<StoredProvider> {
    return this.write(stored.workspaceId, async (tx) => {
      if (stored.isDefault) {
        await clearDefaults(tx, stored.workspaceId, stored.id);
      }

      let rows: LlmProviderRow[];
      try {
        // name: UpdateProvider
        rows = await tx<LlmProviderRow[]>`
          UPDATE llm_providers
          SET name = ${stored.name},
              adapter = ${stored.adapter},
              base_url = ${stored.baseUrl},
              model = ${stored.model},
              api_key_encrypted = CASE
                  WHEN ${stored.encryptedKey}::bytea IS NULL THEN api_key_encrypted
                  ELSE NULLIF(${stored.encryptedKey}::bytea, ''::bytea)
              END,
              priority = ${stored.priority},
              max_tokens = ${stored.maxTokens},
              context_tokens = ${stored.contextTokens},
              is_default = ${stored.isDefault},
              enabled = ${stored.enabled},
              updated_at = now()
          WHERE id = ${stored.id} AND workspace_id = ${stored.workspaceId}
          RETURNING id, workspace_id, name, adapter, base_url, model, api_key_encrypted, priority, max_tokens, context_tokens, is_default, enabled, created_at, updated_at
        `;
      } catch (err) {
        throw translateWriteError(err, "update");
      }

      const row = rows[0];
      if (!row) throw ErrProviderNotFound;
      return toStoredProvider(row);
    });
  }

  /** Removes a provider. */
  async delete(
    ctx: AbortSignal | undefined,
    workspaceId: string,
    id: string,
  ): Promise<void> {
    await this.write(workspaceId, async (tx) => {
      // name: DeleteProvider
      const result = await tx`
        DELETE FROM llm_providers WHERE id = ${id} AND workspace_id = ${workspaceId}
      `;
      if (result.count === 0) throw ErrProviderNotFound;
    });
  }

  /**
   * Appends one row to the usage ledger. It is the only write path of a
   * provider call's cost.
   */
  async record(ctx: AbortSignal | undefined, entry: UsageEntry): Promise<void> {
    await this.write(entry.workspaceId, async (tx) => {
      // name: InsertUsageEntry
      await tx`
        INSERT INTO usage_ledger (
            workspace_id, agent_id, task_id, provider, model, purpose,
            input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_micros
        )
        VALUES (${entry.workspaceId}, ${optionalUUID(entry.agentId)}, ${optionalUUID(entry.taskId)}, ${entry.provider}, ${entry.model}, ${entry.purpose}, ${entry.usage.inputTokens}, ${entry.usage.outputTokens}, ${entry.usage.cacheReadTokens}, ${entry.usage.cacheWriteTokens}, ${entry.costMicros})
        RETURNING id, created_at
      `;
    });
  }

  /**
   * The cost recorded since a moment, in micro-rupiah. It reads the daily
   * aggregation rather than the ledger.
   */
  async costSince(
    ctx: AbortSignal | undefined,
    workspaceId: string,
    since: Date,
  ): Promise<number> {
    const days = await this.daily(ctx, workspaceId, since);
    let total = 0;
    for (const day of days) total += day.costMicros;
    return total;
  }

  /**
   * The period's totals with the per-Bolu and per-task breakdowns. The three
   * reads are one method because they are one answer.
   */
  async report(
    ctx: AbortSignal | undefined,
    workspaceId: string,
    since: Date,
    limit: number,
  ): Promise<UsageReport> {
    const report: UsageReport = {
      periodStartedAt: new Date(0),
      totals: {
        calls: 0,
        inputTokens: 0,
        outputTokens: 0,
        cacheReadTokens: 0,
        cacheWriteTokens: 0,
        costMicros: 0,
      },
      byAgent: [],
      byTask: [],
    };

    await this.read(workspaceId, async (tx) => {
      // name: SumUsageSince
      const totalsRows = await tx<SumUsageSinceRow[]>`
        SELECT COALESCE(sum(input_tokens), 0)::bigint       AS input_tokens,
               COALESCE(sum(output_tokens), 0)::bigint      AS output_tokens,
               COALESCE(sum(cache_read_tokens), 0)::bigint  AS cache_read_tokens,
               COALESCE(sum(cache_write_tokens), 0)::bigint AS cache_write_tokens,
               COALESCE(sum(cost_micros), 0)::bigint        AS cost_micros,
               count(*)::bigint                             AS calls
        FROM usage_ledger
        WHERE workspace_id = ${workspaceId} AND created_at >= ${since}
      `;
      const totals = totalsRows[0]!;
      report.totals = {
        calls: totals.calls,
        inputTokens: totals.input_tokens,
        outputTokens: totals.output_tokens,
        cacheReadTokens: totals.cache_read_tokens,
        cacheWriteTokens: totals.cache_write_tokens,
        costMicros: totals.cost_micros,
      };

      // name: ListUsageByAgent
      const agents = await tx<ListUsageByAgentRow[]>`
        SELECT COALESCE(a.id, '00000000-0000-0000-0000-000000000000'::uuid) AS agent_id,
               COALESCE(a.name, '')                                          AS agent_name,
               sum(u.input_tokens)::bigint                                   AS input_tokens,
               sum(u.output_tokens)::bigint                                  AS output_tokens,
               sum(u.cost_micros)::bigint                                    AS cost_micros,
               count(*)::bigint                                              AS calls
        FROM usage_ledger u
        LEFT JOIN agents a ON a.id = u.agent_id AND a.workspace_id = u.workspace_id
        WHERE u.workspace_id = ${workspaceId} AND u.created_at >= ${since}
        GROUP BY 1, 2
        ORDER BY 5 DESC, 3 DESC
        LIMIT ${boundedLimit(limit)}
      `;
      report.byAgent = agents.map(
        (row): UsageByAgent => ({
          agentId: row.agent_id,
          agentName: row.agent_name,
          calls: row.calls,
          inputTokens: row.input_tokens,
          outputTokens: row.output_tokens,
          costMicros: row.cost_micros,
        }),
      );

      // name: ListUsageByTask
      const tasks = await tx<ListUsageByTaskRow[]>`
        SELECT u.task_id,
               COALESCE(t.title, '')           AS task_title,
               COALESCE(t.status, '')          AS task_status,
               sum(u.input_tokens)::bigint     AS input_tokens,
               sum(u.output_tokens)::bigint    AS output_tokens,
               sum(u.cost_micros)::bigint      AS cost_micros,
               count(*)::bigint                AS calls
        FROM usage_ledger u
        LEFT JOIN tasks t ON t.id = u.task_id AND t.workspace_id = u.workspace_id
        WHERE u.workspace_id = ${workspaceId} AND u.created_at >= ${since} AND u.task_id IS NOT NULL
        GROUP BY 1, 2, 3
        ORDER BY 6 DESC, 4 DESC
        LIMIT ${boundedLimit(limit)}
      `;
      report.byTask = tasks.map(
        (row): UsageByTask => ({
          taskId: row.task_id ?? "",
          taskTitle: row.task_title,
          taskStatus: row.task_status,
          calls: row.calls,
          inputTokens: row.input_tokens,
          outputTokens: row.output_tokens,
          costMicros: row.cost_micros,
        }),
      );
    });

    return report;
  }

  /** Per-day usage totals since a date, most recent first. */
  async daily(
    ctx: AbortSignal | undefined,
    workspaceId: string,
    since: Date,
  ): Promise<UsageDaily[]> {
    return this.read(workspaceId, async (tx) => {
      // name: ListDailyUsage
      const rows = await tx<UsageDailyRow[]>`
        SELECT workspace_id, day, provider, model, purpose, calls, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_micros FROM usage_daily
        WHERE workspace_id = ${workspaceId} AND day >= ${since}
        ORDER BY day DESC, provider, model
      `;
      return rows.map(
        (row): UsageDaily => ({
          day: row.day,
          provider: row.provider,
          model: row.model,
          purpose: row.purpose,
          calls: row.calls,
          inputTokens: row.input_tokens,
          outputTokens: row.output_tokens,
          cacheReadTokens: row.cache_read_tokens,
          cacheWriteTokens: row.cache_write_tokens,
          costMicros: row.cost_micros,
        }),
      );
    });
  }

  /** The tokens the ledger recorded since a moment. */
  async sumSince(
    ctx: AbortSignal | undefined,
    workspaceId: string,
    since: Date,
  ): Promise<number> {
    return this.read(workspaceId, async (tx) => {
      // name: SumTokensSince
      const rows = await tx<{ tokens: number }[]>`
        SELECT COALESCE(sum(input_tokens + output_tokens), 0)::bigint AS tokens
        FROM usage_ledger
        WHERE workspace_id = ${workspaceId} AND created_at >= ${since}
      `;
      return rows[0]?.tokens ?? 0;
    });
  }

  private async read<T>(workspaceId: string, fn: (tx: Tx) => Promise<T>): Promise<T> {
    return this.db.inScopeRead(scopeOf(workspaceId), fn);
  }

  private async write<T>(workspaceId: string, fn: (tx: Tx) => Promise<T>): Promise<T> {
    return this.db.inScope(scopeOf(workspaceId), fn);
  }
}

/**
 * Reads and writes the model override of one Bolu. The override lives in
 * agents.default_model, which the agent module owns.
 */
export class AgentStore implements AgentStorePort {
  constructor(
    private readonly db: Database,
    private readonly box: SecretBox | null,
  ) {}

  /** Reads the override of one Bolu. */
  async agentModel(
    ctx: AbortSignal | undefined,
    workspaceId: string,
    agentId: string,
  ): Promise<AgentOverride> {
    return this.read(workspaceId, async (tx) => {
      // name: GetAgentModelOverride
      const rows = await tx<{ default_model: unknown }[]>`
        SELECT default_model FROM agents
        WHERE id = ${agentId} AND workspace_id = ${workspaceId} AND deleted_at IS NULL
      `;
      const row = rows[0];
      if (!row) throw ErrProviderNotFound;
      return this.decode(row.default_model);
    });
  }

  /** Stores the override of one Bolu. */
  async setAgentModel(
    ctx: AbortSignal | undefined,
    workspaceId: string,
    agentId: string,
    override: AgentOverride,
  ): Promise<void> {
    const encoded = JSON.stringify(this.encode(override));

    await this.write(workspaceId, async (tx) => {
      // name: SetAgentModelOverride
      const result = await tx`
        UPDATE agents
        SET default_model = ${encoded}::jsonb, updated_at = now()
        WHERE id = ${agentId} AND workspace_id = ${workspaceId} AND deleted_at IS NULL
      `;
      if (result.count === 0) throw ErrProviderNotFound;
    });
  }

  private encode(override: AgentOverride): StoredOverride {
    const stored: StoredOverride = {
      // Go marshals uuid.UUID as an array, which omitempty never drops, so the
      // zero id is written out rather than omitted.
      provider_id: override.provider_id || ZeroUUID,
      adapter: override.adapter,
      base_url: override.base_url,
      model: override.model,
      max_tokens: override.max_tokens,
    };

    if (override.api_key) {
      if (!this.box) {
        throw llmError(ErrInvalidRequest, "no encryption key is configured");
      }
      stored.api_key_encrypted = Buffer.from(this.box.encrypt(override.api_key)).toString("base64");
    }

    return stored;
  }

  private decode(raw: unknown): AgentOverride {
    if (raw === null || raw === undefined || raw === "") return {};

    // The column is JSONB, which postgres.js hands back already parsed; a
    // string or a byte array is what a driver without a JSON codec returns.
    const stored: StoredOverride =
      typeof raw === "string"
        ? (JSON.parse(raw) as StoredOverride)
        : raw instanceof Uint8Array
          ? (JSON.parse(Buffer.from(raw).toString("utf8")) as StoredOverride)
          : (raw as StoredOverride);
    const override: AgentOverride = {
      provider_id:
        stored.provider_id && stored.provider_id !== ZeroUUID ? stored.provider_id : undefined,
      adapter: stored.adapter || undefined,
      base_url: stored.base_url || undefined,
      model: stored.model || undefined,
      max_tokens: stored.max_tokens || undefined,
    };

    if (stored.api_key_encrypted) {
      if (!this.box) {
        throw llmError(ErrInvalidRequest, "no encryption key is configured");
      }
      override.api_key = this.box.decrypt(Buffer.from(stored.api_key_encrypted, "base64"));
    }

    return override;
  }

  private async read<T>(workspaceId: string, fn: (tx: Tx) => Promise<T>): Promise<T> {
    return this.db.inScopeRead(scopeOf(workspaceId), fn);
  }

  private async write<T>(workspaceId: string, fn: (tx: Tx) => Promise<T>): Promise<T> {
    return this.db.inScope(scopeOf(workspaceId), fn);
  }
}

/** The JSONB shape of agents.default_model. */
interface StoredOverride {
  provider_id?: string;
  adapter?: string;
  base_url?: string;
  model?: string;
  max_tokens?: number;
  /** Base64 of the sealed key, because JSONB cannot hold raw bytes. */
  api_key_encrypted?: string;
}

const ZeroUUID = "00000000-0000-0000-0000-000000000000";

/** At most one default exists, so a new default is set by clearing the others. */
async function clearDefaults(tx: Tx, workspaceId: string, keep: string | null): Promise<void> {
  // name: ClearDefaultProviders
  const result = await tx`
    UPDATE llm_providers
    SET is_default = false, updated_at = now()
    WHERE workspace_id = ${workspaceId} AND id <> ${keep ?? ZeroUUID} AND is_default
  `;
  // Exactly one row may be spared, and only when it is the row being updated.
  if (result.count > 1) {
    throw new Error(`llm: ${result.count} providers were the default, expected at most 1`);
  }
}

/** Takes the caller's page size or the module's default. */
function boundedLimit(limit: number): number {
  return limit <= 0 ? DefaultReportPage : limit;
}

function scopeOf(workspaceId: string): Scope {
  return { workspaceId };
}

function optionalUUID(id: string): string | null {
  return id ? id : null;
}

function toStoredProvider(row: LlmProviderRow): StoredProvider {
  return {
    id: row.id,
    workspaceId: row.workspace_id,
    name: row.name,
    adapter: row.adapter,
    baseUrl: row.base_url,
    model: row.model,
    apiKey: "",
    priority: row.priority,
    maxTokens: row.max_tokens,
    isDefault: row.is_default,
    contextTokens: row.context_tokens,
    enabled: row.enabled,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
    encryptedKey: row.api_key_encrypted,
  };
}

/** Maps a write failure onto the module's sentinels. */
function translateWriteError(err: unknown, action: string): Error {
  if (err instanceof Error && "code" in err && typeof err.code === "string") {
    const constraint =
      "constraint_name" in err && typeof err.constraint_name === "string" ? err.constraint_name : "";
    if (err.code === "23505") {
      // Either the name is taken or another provider became the default.
      return llmError(ErrDuplicateProvider, constraint);
    }
    if (err.code === "23514") {
      // The adapter check constraint.
      return llmError(ErrInvalidRequest, "unsupported adapter");
    }
  }
  return new Error(`llm: ${action} provider: ${err instanceof Error ? err.message : String(err)}`);
}
