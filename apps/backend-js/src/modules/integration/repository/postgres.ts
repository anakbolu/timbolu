/**
 * Postgres-backed implementation of the integration ports.
 *
 * Every statement runs inside the caller's tenant scope, so Row Level Security
 * filters it even when a statement forgets its workspace filter. The token
 * columns are read by one statement that exists to open them, so listing the
 * workspace's connections cannot carry a ciphertext into a response by
 * accident.
 *
 * Ported 1:1 from internal/modules/integration/repository/postgres.go, with
 * repository/sqlcgen/*.go folded in: each query keeps its name as a comment and
 * its SQL text unchanged.
 */

import { createHash } from "node:crypto";
import type { Database, Tx } from "../../../platform/database/index.ts";
import {
  ErrInvalidInput,
  ErrNotFound,
  type ConnectInput,
  type Grant,
  type Integration,
  type Scope,
  type StoredToken,
  type WrappedKey,
} from "../domain/integration.ts";
import {
  ErrDeliveryExists,
  type EnqueueInput,
  type QueuedEvent,
} from "../domain/webhook.ts";
import { ErrMCPServerNotFound, type MCPServer, type MCPServerInput, type MCPTool } from "../domain/mcp.ts";
import { ErrInvalidState, type OAuthState } from "../domain/oauth.ts";

/** One row of `integrations`, as the driver returns it. */
interface IntegrationRow {
  id: string;
  workspace_id: string;
  app: string;
  account_label: string;
  status: string;
  scopes: string[] | null;
  access_token_encrypted: Uint8Array | null;
  refresh_token_encrypted: Uint8Array | null;
  token_expires_at: Date | null;
  created_at: Date;
  updated_at: Date;
  status_reason: string;
  last_refreshed_at: Date | null;
}

/** One row of `integrations`, token columns only. */
interface IntegrationTokenRow {
  access_token_encrypted: Uint8Array | null;
  refresh_token_encrypted: Uint8Array | null;
  token_expires_at: Date | null;
  scopes: string[] | null;
}

/** One row of `mcp_servers`. */
interface MCPServerRow {
  id: string;
  workspace_id: string;
  app: string;
  label: string;
  endpoint: string;
  credential_encrypted: Uint8Array | null;
  enabled: boolean;
  last_error: string;
  tools: Uint8Array | string | null;
  tool_count: number;
  created_at: Date;
  updated_at: Date;
}

/** One row of `webhook_events`. */
interface WebhookEventRow {
  id: string;
  workspace_id: string;
  integration_id: string | null;
  app: string;
  delivery_id: string;
  event: string;
  payload: Uint8Array;
  received_at: Date;
  processed_at: Date | null;
  attempts: number;
  last_error: string;
}

/** One row of `agent_grants`. */
interface AgentGrantRow {
  id: string;
  workspace_id: string;
  agent_id: string;
  integration_id: string;
  permission: string;
  created_at: Date;
  updated_at: Date;
}

export class PostgresRepository {
  constructor(private readonly db: Database) {}

  /** The workspace's connections, newest app first. */
  async list(scope: Scope): Promise<Integration[]> {
    // name: ListIntegrations
    const rows = await this.read(scope, (tx) => tx<IntegrationRow[]>`
      SELECT * FROM integrations
      WHERE workspace_id = ${scope.workspaceId}
      ORDER BY app, account_label
    `);
    return rows.map(toIntegration);
  }

  /** One connection. */
  async get(scope: Scope, id: string): Promise<Integration> {
    // name: GetIntegration
    const rows = await this.read(scope, (tx) => tx<IntegrationRow[]>`
      SELECT * FROM integrations
      WHERE workspace_id = ${scope.workspaceId} AND id = ${id}
    `);
    if (rows.length === 0) {
      throw ErrNotFound;
    }
    return toIntegration(rows[0]);
  }

  /** Stores a connection, keyed by app and account label. */
  async upsert(scope: Scope, input: ConnectInput, sealed: StoredToken): Promise<Integration> {
    // name: UpsertIntegration
    const rows = await this.readWrite(scope, (tx) => tx<IntegrationRow[]>`
      INSERT INTO integrations (
          workspace_id, app, account_label, status, status_reason, scopes,
          access_token_encrypted, refresh_token_encrypted, token_expires_at, last_refreshed_at
      ) VALUES (
          ${scope.workspaceId}, ${input.app}, ${input.accountLabel}, 'connected', '',
          ${scopesOr(input.scopes)}, ${sealed.accessTokenEncrypted},
          ${sealed.refreshTokenEncrypted}, ${sealed.expiresAt}, now()
      )
      ON CONFLICT (workspace_id, app, account_label) DO UPDATE SET
          status = 'connected',
          status_reason = '',
          scopes = EXCLUDED.scopes,
          access_token_encrypted = EXCLUDED.access_token_encrypted,
          refresh_token_encrypted = EXCLUDED.refresh_token_encrypted,
          token_expires_at = EXCLUDED.token_expires_at,
          last_refreshed_at = now(),
          updated_at = now()
      RETURNING *
    `);
    return toIntegration(rows[0]);
  }

  /** The stored token of a connection, still sealed. */
  async sealed(scope: Scope, id: string): Promise<StoredToken> {
    // name: GetIntegrationToken
    const rows = await this.read(scope, (tx) => tx<IntegrationTokenRow[]>`
      SELECT access_token_encrypted, refresh_token_encrypted, token_expires_at, scopes
      FROM integrations
      WHERE workspace_id = ${scope.workspaceId} AND id = ${id}
    `);
    if (rows.length === 0) {
      throw ErrNotFound;
    }
    const row = rows[0];
    return {
      accessTokenEncrypted: row.access_token_encrypted ?? new Uint8Array(),
      refreshTokenEncrypted: row.refresh_token_encrypted ?? new Uint8Array(),
      expiresAt: row.token_expires_at,
      scopes: row.scopes ?? [],
    };
  }

  /** Replaces the sealed token after a refresh. */
  async saveRefreshed(scope: Scope, id: string, sealed: StoredToken): Promise<void> {
    // name: SaveRefreshedToken
    const rows = await this.readWrite(scope, (tx) => tx<IntegrationRow[]>`
      UPDATE integrations
      SET access_token_encrypted = ${sealed.accessTokenEncrypted},
          refresh_token_encrypted = ${sealed.refreshTokenEncrypted},
          token_expires_at = ${sealed.expiresAt},
          last_refreshed_at = now(),
          status = 'connected',
          status_reason = '',
          updated_at = now()
      WHERE workspace_id = ${scope.workspaceId} AND id = ${id}
      RETURNING *
    `);
    if (rows.length === 0) {
      throw ErrNotFound;
    }
  }

  /** Records a status a person may have to act on. */
  async setStatus(scope: Scope, id: string, status: string, reason: string): Promise<void> {
    // name: SetIntegrationStatus
    const rows = await this.readWrite(scope, (tx) => tx<IntegrationRow[]>`
      UPDATE integrations
      SET status = ${status}, status_reason = ${reason}, updated_at = now()
      WHERE workspace_id = ${scope.workspaceId} AND id = ${id}
      RETURNING *
    `);
    if (rows.length === 0) {
      throw ErrNotFound;
    }
  }

  /** Clears the stored token and marks the row disconnected. */
  async disconnect(scope: Scope, id: string): Promise<void> {
    // name: DisconnectIntegration
    const rows = await this.readWrite(scope, (tx) => tx<IntegrationRow[]>`
      UPDATE integrations
      SET status = 'disconnected',
          status_reason = '',
          access_token_encrypted = NULL,
          refresh_token_encrypted = NULL,
          token_expires_at = NULL,
          updated_at = now()
      WHERE workspace_id = ${scope.workspaceId} AND id = ${id}
      RETURNING *
    `);
    if (rows.length === 0) {
      throw ErrNotFound;
    }
  }

  /** Removes the row. */
  async delete(scope: Scope, id: string): Promise<void> {
    // name: DeleteIntegration
    const rows = await this.readWrite(scope, (tx) => tx<{ id: string }[]>`
      DELETE FROM integrations
      WHERE workspace_id = ${scope.workspaceId} AND id = ${id}
      RETURNING id
    `);
    if (rows.length === 0) {
      throw ErrNotFound;
    }
  }

  /** The workspace's wrapped data key, or ErrNotFound when it has none yet. */
  async dataKey(scope: Scope): Promise<WrappedKey> {
    // name: GetWorkspaceKey
    const rows = await this.read(scope, (tx) => tx<{ wrapped_data_key: Uint8Array; kms_key_id: string }[]>`
      SELECT wrapped_data_key, kms_key_id FROM workspace_keys
      WHERE workspace_id = ${scope.workspaceId}
    `);
    if (rows.length === 0) {
      throw ErrNotFound;
    }
    return { keyId: rows[0].kms_key_id, bytes: rows[0].wrapped_data_key };
  }

  /** Stores the wrapped data key, replacing an existing wrap. */
  async saveDataKey(scope: Scope, wrapped: WrappedKey): Promise<void> {
    // name: UpsertWorkspaceKey
    await this.translateWrite("save workspace key", () =>
      this.readWrite(scope, (tx) => tx`
        INSERT INTO workspace_keys (workspace_id, wrapped_data_key, kms_key_id)
        VALUES (${scope.workspaceId}, ${wrapped.bytes}, ${wrapped.keyId})
        ON CONFLICT (workspace_id) DO UPDATE SET
            wrapped_data_key = EXCLUDED.wrapped_data_key,
            kms_key_id = EXCLUDED.kms_key_id,
            rotated_at = CASE
                WHEN workspace_keys.kms_key_id <> EXCLUDED.kms_key_id THEN now()
                ELSE workspace_keys.rotated_at
            END
        RETURNING *
      `),
    );
  }

  /** Records an OAuth state as usable until it expires. */
  async saveState(state: OAuthState): Promise<void> {
    // name: SaveOAuthState
    await this.translateWrite("save oauth state", () =>
      this.readWrite({ userId: state.userId, workspaceId: state.workspaceId }, (tx) => tx`
        INSERT INTO oauth_states (nonce_hash, workspace_id, user_id, app, return_to, expires_at)
        VALUES (${hashNonce(state.nonce)}, ${state.workspaceId}, ${state.userId}, ${state.app},
                ${state.returnTo}, ${state.expiresAt})
        ON CONFLICT (nonce_hash) DO NOTHING
      `),
    );
  }

  /**
   * Spends a state's nonce. The single statement is the guarantee: the second of
   * two callbacks carrying the same state finds no row, because the first set
   * `consumed_at`.
   */
  async consumeState(workspaceId: string, nonce: string, now: Date): Promise<void> {
    // name: ConsumeOAuthState
    const rows = await this.readWrite({ workspaceId }, (tx) => tx<{ user_id: string }[]>`
      UPDATE oauth_states
      SET consumed_at = now()
      WHERE nonce_hash = ${hashNonce(nonce)}
        AND workspace_id = ${workspaceId}
        AND consumed_at IS NULL
        AND expires_at > ${now}
      RETURNING user_id, app, return_to
    `);
    if (rows.length === 0) {
      // Already spent, never existed, or expired: all the same answer.
      throw ErrInvalidState;
    }
  }

  /** The grants of one Bolu. */
  async grants(scope: Scope, agentId: string): Promise<Grant[]> {
    // name: ListGrants
    const rows = await this.read(scope, (tx) => tx<AgentGrantRow[]>`
      SELECT * FROM agent_grants
      WHERE workspace_id = ${scope.workspaceId} AND agent_id = ${agentId}
      ORDER BY integration_id
    `);
    return rows.map((row) => ({
      agentId: row.agent_id,
      integrationId: row.integration_id,
      permission: row.permission,
    }));
  }

  /** Adds or updates one Bolu's permission on one integration. */
  async setGrant(scope: Scope, agentId: string, integrationId: string, permission: string): Promise<Grant> {
    // name: UpsertGrant
    const rows = await this.translateWrite("set grant", () =>
      this.readWrite(scope, (tx) => tx<AgentGrantRow[]>`
        INSERT INTO agent_grants (workspace_id, agent_id, integration_id, permission)
        VALUES (${scope.workspaceId}, ${agentId}, ${integrationId}, ${permission})
        ON CONFLICT (agent_id, integration_id) DO UPDATE SET
            permission = EXCLUDED.permission,
            updated_at = now()
        RETURNING *
      `),
    );
    return {
      agentId: rows[0].agent_id,
      integrationId: rows[0].integration_id,
      permission: rows[0].permission,
    };
  }

  /** Removes one grant. */
  async deleteGrant(scope: Scope, agentId: string, integrationId: string): Promise<void> {
    // name: DeleteGrant
    const rows = await this.readWrite(scope, (tx) => tx<{ id: string }[]>`
      DELETE FROM agent_grants
      WHERE workspace_id = ${scope.workspaceId} AND agent_id = ${agentId} AND integration_id = ${integrationId}
      RETURNING id
    `);
    if (rows.length === 0) {
      throw ErrNotFound;
    }
  }

  // ------------------------------------------------------------ webhook queue

  /**
   * Stores a verified delivery. A retry returns ErrDeliveryExists while the
   * endpoint still answers 200.
   */
  async enqueue(input: EnqueueInput): Promise<QueuedEvent> {
    // name: EnqueueWebhookEvent
    const rows = await this.translateWrite("enqueue webhook event", () =>
      this.readWrite({ workspaceId: input.workspaceId }, (tx) => tx<WebhookEventRow[]>`
        INSERT INTO webhook_events (workspace_id, integration_id, app, delivery_id, event, payload)
        VALUES (${input.workspaceId}, ${nullableId(input.integrationId)}, ${input.app},
                ${input.deliveryId}, ${input.event}, ${payloadOr(input.payload)})
        ON CONFLICT (workspace_id, app, delivery_id) DO NOTHING
        RETURNING id, workspace_id, integration_id, app, delivery_id, event, payload,
                  received_at, processed_at, attempts, last_error
      `),
    );
    if (rows.length === 0) {
      throw ErrDeliveryExists;
    }
    return toQueuedEvent(rows[0]);
  }

  /** The oldest unprocessed deliveries of a workspace. */
  async pending(scope: Scope, limit: number): Promise<QueuedEvent[]> {
    const page = limit <= 0 ? 50 : limit;
    // name: PendingWebhookEvents
    const rows = await this.read(scope, (tx) => tx<WebhookEventRow[]>`
      SELECT id, workspace_id, integration_id, app, delivery_id, event, payload,
             received_at, processed_at, attempts, last_error
      FROM webhook_events
      WHERE workspace_id = ${scope.workspaceId} AND processed_at IS NULL
      ORDER BY received_at, id
      LIMIT ${page}
    `);
    return rows.map(toQueuedEvent);
  }

  /**
   * Reads the deliveries of one event kind and marks them taken in the same
   * statement, so two consumers watching one event cannot both act on one
   * delivery.
   */
  async claimEvents(scope: Scope, event: string, limit: number): Promise<QueuedEvent[]> {
    const page = limit <= 0 ? 20 : limit;
    // name: ClaimWebhookEvents
    const rows = await this.readWrite(scope, (tx) => tx<WebhookEventRow[]>`
      UPDATE webhook_events SET processed_at = now()
      WHERE id IN (
          SELECT pending.id FROM webhook_events AS pending
          WHERE pending.workspace_id = ${scope.workspaceId}
            AND pending.event = ${event}
            AND pending.processed_at IS NULL
          ORDER BY pending.received_at, pending.id
          LIMIT ${page}
          FOR UPDATE SKIP LOCKED
      )
      RETURNING id, workspace_id, integration_id, app, delivery_id, event, payload,
                received_at, processed_at, attempts, last_error
    `);
    return rows.map(toQueuedEvent);
  }

  /** Marks a delivery as acted on. */
  async processed(scope: Scope, id: string): Promise<void> {
    // name: MarkWebhookEventProcessed
    const rows = await this.readWrite(scope, (tx) => tx<{ id: string }[]>`
      UPDATE webhook_events
      SET processed_at = now()
      WHERE workspace_id = ${scope.workspaceId} AND id = ${id} AND processed_at IS NULL
      RETURNING id
    `);
    if (rows.length === 0) {
      throw ErrNotFound;
    }
  }

  /** Records an attempt that did not work. */
  async failed(scope: Scope, id: string, reason: string): Promise<void> {
    // name: FailWebhookEvent
    const rows = await this.readWrite(scope, (tx) => tx<{ id: string }[]>`
      UPDATE webhook_events
      SET attempts = attempts + 1, last_error = ${reason}
      WHERE workspace_id = ${scope.workspaceId} AND id = ${id}
      RETURNING id
    `);
    if (rows.length === 0) {
      throw ErrNotFound;
    }
  }

  // ------------------------------------------------------------ MCP servers

  /** The servers a workspace added. */
  async listMCPServers(scope: Scope): Promise<MCPServer[]> {
    // name: ListMCPServers
    const rows = await this.read(scope, (tx) => tx<MCPServerRow[]>`
      SELECT * FROM mcp_servers WHERE workspace_id = ${scope.workspaceId} ORDER BY app
    `);
    return rows.map(toMCPServer);
  }

  /** One server. */
  async getMCPServer(scope: Scope, id: string): Promise<MCPServer> {
    // name: GetMCPServer
    const rows = await this.read(scope, (tx) => tx<MCPServerRow[]>`
      SELECT * FROM mcp_servers WHERE workspace_id = ${scope.workspaceId} AND id = ${id}
    `);
    if (rows.length === 0) {
      throw ErrMCPServerNotFound;
    }
    return toMCPServer(rows[0]);
  }

  /** Stores a server, keyed by app. */
  async upsertMCPServer(
    scope: Scope,
    input: MCPServerInput,
    sealedCredential: Uint8Array | null,
  ): Promise<MCPServer> {
    // name: UpsertMCPServer
    const rows = await this.translateWrite("upsert MCP server", () =>
      this.readWrite(scope, (tx) => tx<MCPServerRow[]>`
        INSERT INTO mcp_servers (workspace_id, app, label, endpoint, credential_encrypted, enabled)
        VALUES (${scope.workspaceId}, ${input.app}, ${input.label}, ${input.endpoint},
                ${sealedCredential}, ${input.enabled})
        ON CONFLICT (workspace_id, app) DO UPDATE SET
            label = EXCLUDED.label,
            endpoint = EXCLUDED.endpoint,
            credential_encrypted = COALESCE(EXCLUDED.credential_encrypted, mcp_servers.credential_encrypted),
            enabled = EXCLUDED.enabled,
            last_error = '',
            updated_at = now()
        RETURNING *
      `),
    );
    return toMCPServer(rows[0]);
  }

  /** Records the tool list a server reported. */
  async saveMCPTools(scope: Scope, id: string, tools: MCPTool[]): Promise<void> {
    const encoded = encodeMCPTools(tools);
    // name: SaveMCPTools
    const rows = await this.translateWrite("save MCP tools", () =>
      this.readWrite(scope, (tx) => tx<MCPServerRow[]>`
        UPDATE mcp_servers
        SET tools = ${tx.json(encoded as any)}, tool_count = ${tools.length}, last_error = '', updated_at = now()
        WHERE workspace_id = ${scope.workspaceId} AND id = ${id}
        RETURNING *
      `),
    );
    if (rows.length === 0) {
      throw ErrMCPServerNotFound;
    }
  }

  /** The tool list a server last reported. */
  async mcpServerTools(scope: Scope, id: string): Promise<MCPTool[]> {
    const rows = await this.read(scope, (tx) => tx<MCPServerRow[]>`
      SELECT * FROM mcp_servers WHERE workspace_id = ${scope.workspaceId} AND id = ${id}
    `);
    if (rows.length === 0) {
      throw ErrMCPServerNotFound;
    }
    return decodeMCPTools(rows[0].tools);
  }

  /** Records why a server could not be reached. */
  async setMCPServerError(scope: Scope, id: string, reason: string): Promise<void> {
    // name: SetMCPServerError
    const rows = await this.translateWrite("set MCP server error", () =>
      this.readWrite(scope, (tx) => tx<MCPServerRow[]>`
        UPDATE mcp_servers
        SET last_error = ${reason}, updated_at = now()
        WHERE workspace_id = ${scope.workspaceId} AND id = ${id}
        RETURNING *
      `),
    );
    if (rows.length === 0) {
      throw ErrMCPServerNotFound;
    }
  }

  /** Removes one server. */
  async deleteMCPServer(scope: Scope, id: string): Promise<void> {
    // name: DeleteMCPServer
    const rows = await this.readWrite(scope, (tx) => tx<{ id: string }[]>`
      DELETE FROM mcp_servers WHERE workspace_id = ${scope.workspaceId} AND id = ${id}
      RETURNING id
    `);
    if (rows.length === 0) {
      throw ErrMCPServerNotFound;
    }
  }

  /** The sealed credential of a server. */
  async mcpServerCredential(scope: Scope, id: string): Promise<Uint8Array> {
    // name: GetMCPServerCredential
    const rows = await this.read(scope, (tx) => tx<{ credential_encrypted: Uint8Array | null }[]>`
      SELECT credential_encrypted FROM mcp_servers
      WHERE workspace_id = ${scope.workspaceId} AND id = ${id}
    `);
    if (rows.length === 0) {
      throw ErrMCPServerNotFound;
    }
    return rows[0].credential_encrypted ?? new Uint8Array();
  }

  // ---------------------------------------------------------------- helpers

  private read<T>(scope: Scope, fn: (tx: Tx) => PromiseLike<T>): Promise<T> {
    return this.db.inScopeRead({ userId: scope.userId ?? null, workspaceId: scope.workspaceId }, async (tx) => fn(tx));
  }

  private readWrite<T>(scope: Scope, fn: (tx: Tx) => PromiseLike<T>): Promise<T> {
    return this.db.inScope({ userId: scope.userId ?? null, workspaceId: scope.workspaceId }, async (tx) => fn(tx));
  }

  /**
   * Turns the schema's own refusals into the module's sentinels, so a caller
   * sees what it can act on rather than a driver error.
   */
  private async translateWrite<T>(action: string, run: () => Promise<T>): Promise<T> {
    try {
      return await run();
    } catch (err) {
      const code = errorCode(err);
      if (code === "23503") {
        // A foreign key failure means the row referenced is not in this
        // workspace, which RLS refuses too. Reporting it as invalid input keeps
        // the tenant boundary invisible.
        throw new Error(`${ErrInvalidInput.message}: a referenced row is not in this workspace`);
      }
      if (code === "23514") {
        // A check constraint: an unknown status or an unknown permission.
        throw new Error(`${ErrInvalidInput.message}: a value is outside the allowed set`);
      }
      throw new Error(`integration: ${action}: ${err instanceof Error ? err.message : String(err)}`);
    }
  }
}

/** Renders a stored row without its sealed columns. */
function toIntegration(row: IntegrationRow): Integration {
  return {
    id: row.id,
    app: row.app,
    accountLabel: row.account_label,
    status: row.status,
    statusReason: row.status_reason,
    scopes: row.scopes ?? [],
    expiresAt: row.token_expires_at,
    lastRefreshedAt: row.last_refreshed_at,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

/** Renders a stored server. */
function toMCPServer(row: MCPServerRow): MCPServer {
  return {
    id: row.id,
    app: row.app,
    label: row.label,
    endpoint: row.endpoint,
    enabled: row.enabled,
    toolCount: Number(row.tool_count),
    lastError: row.last_error,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

/** Renders a stored delivery. */
function toQueuedEvent(row: WebhookEventRow): QueuedEvent {
  return {
    id: row.id,
    workspaceId: row.workspace_id,
    integrationId: row.integration_id ?? "",
    app: row.app,
    deliveryId: row.delivery_id,
    event: row.event,
    payload: row.payload,
    receivedAt: row.received_at,
    attempts: Number(row.attempts),
  };
}

/**
 * Renders the stored form of a state's nonce. The value handed to the provider
 * is never stored, so a leaked backup cannot be used to finish somebody's flow.
 */
function hashNonce(nonce: string): string {
  return createHash("sha256").update(nonce, "utf8").digest("hex");
}

/** Keeps an absent body as an empty object rather than null. */
function payloadOr(payload: Uint8Array): Uint8Array {
  if (!payload || payload.length === 0) {
    return new TextEncoder().encode("{}");
  }
  return payload;
}

/** Renders an id the column allows to be absent as NULL rather than as the zero uuid. */
function nullableId(id: string): string | null {
  return !id || id === "00000000-0000-0000-0000-000000000000" ? null : id;
}

/** Keeps an empty list as an empty array rather than null. */
function scopesOr(scopes: string[]): string[] {
  return scopes ?? [];
}

/** Encodes the tool list the way Go's `json.Marshal` renders the domain struct. */
function encodeMCPTools(tools: MCPTool[]): unknown {
  return tools.map((tool) => ({
    Name: tool.name,
    Label: tool.label,
    Description: tool.description,
    Schema: Buffer.from(tool.schema).toString("base64"),
    RemoteName: tool.remoteName,
  }));
}

/**
 * Decodes a stored tool list. A list this build cannot read is reported as no
 * tools rather than as an error: the next refresh replaces it.
 */
function decodeMCPTools(raw: Uint8Array | string | null): MCPTool[] {
  if (!raw) {
    return [];
  }
  const text = typeof raw === "string" ? raw : new TextDecoder().decode(raw);
  if (text.trim() === "") {
    return [];
  }

  let decoded: unknown;
  try {
    decoded = JSON.parse(text);
  } catch {
    return [];
  }
  if (!Array.isArray(decoded)) {
    return [];
  }

  return decoded.flatMap((entry) => {
    if (!entry || typeof entry !== "object") {
      return [];
    }
    const tool = entry as Partial<Record<"Name" | "Label" | "Description" | "Schema" | "RemoteName", unknown>>;
    return [
      {
        name: String(tool.Name ?? ""),
        label: String(tool.Label ?? ""),
        description: String(tool.Description ?? ""),
        schema:
          typeof tool.Schema === "string"
            ? new Uint8Array(Buffer.from(tool.Schema, "base64"))
            : new Uint8Array(tool.Schema instanceof Uint8Array ? tool.Schema : []),
        remoteName: String(tool.RemoteName ?? ""),
      },
    ];
  });
}

/** Reads a driver error's SQLSTATE, if it carries one. */
function errorCode(err: unknown): string | undefined {
  if (err && typeof err === "object" && "code" in err && typeof err.code === "string") {
    return err.code;
  }
  return undefined;
}
