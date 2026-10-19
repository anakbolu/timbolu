/**
 * The integration use cases: connecting an account, opening its token where the
 * token is about to be used, and saying which Bolu may use it.
 *
 * Two rules the class exists for. A token is sealed on the way in and opened
 * only on the path that is about to be used, so nothing else in the process
 * ever holds a plaintext one. And a grant is checked on the way out: a
 * read-only grant keeps the tools that cannot change anything outside Bolu, and
 * drops the rest.
 *
 * Ported 1:1 from internal/modules/integration/service/service.go. Go spreads
 * the same receiver's methods over several files; here each of those files
 * exports functions that take the service, and the methods below delegate to
 * them, so the file boundaries still name the same concerns.
 */

import { nopLogger } from "../../../platform/logger.ts";
import {
  ErrInvalidInput,
  ErrNoCipher,
  ErrNoToken,
  ErrPlanLimit,
  ErrUnknownPermission,
  NilUUID,
  StatusDisconnected,
  errorMessage,
  isNilId,
  validateConnectInput,
  validPermission,
  wrap,
  type AuditEntry,
  type ConnectInput,
  type Deps,
  type Grant,
  type Integration,
  type LinkInput,
  type RefreshInput,
  type Scope,
  type Service,
  type Token,
} from "../domain/integration.ts";
import {
  RateLimitedError,
  isConnectionless,
  isCredentialled,
  type Adapter,
  type Registry,
  type Result,
  type ToolSpec,
} from "../domain/adapter.ts";
import type { CompleteAuthRequest, AuthURL, OAuthAdapter, OAuthTokens } from "../domain/oauth.ts";
import type { TriggerAdapter, WebhookRequest, QueuedEvent } from "../domain/webhook.ts";
import type { MCPServer, MCPServerInput, MCPTool } from "../domain/mcp.ts";
import { linkConnection } from "./link.ts";
import { accountLabelFor, beginAuth, completeAuth, refreshConnection, refreshExpiring } from "./oauth.ts";
import {
  claimPendingEvents,
  pendingWebhooks,
  receiveWebhook,
  startTriggers,
  webhookFailed,
  webhookProcessed,
  webhookUrl,
} from "./webhook.ts";
import {
  addMCPServer,
  executeMCP,
  listMCPServers,
  mcpCredential,
  mcpTools,
  recordMCPFailure,
  refreshMCPServer,
  refreshMCPTools,
  removeMCPServer,
} from "./mcp.ts";

/** How long before its stated expiry a token is treated as expired. */
export const RefreshMargin = 2 * 60 * 1000;

/** The concrete service behind `ServicePort`. */
export class IntegrationService implements Service {
  readonly deps: Deps;

  constructor(deps: Deps) {
    this.deps = {
      ...deps,
      logger: deps.logger ?? nopLogger,
      clock: deps.clock ?? (() => new Date()),
    };
  }

  /** The workspace's connections. */
  async list(_ctx: unknown, scope: Scope): Promise<Integration[]> {
    return this.deps.repository.list(scope);
  }

  /** One connection. */
  async get(_ctx: unknown, scope: Scope, id: string): Promise<Integration> {
    if (isNilId(id)) {
      throw wrap(ErrInvalidInput, "an integration id is required");
    }
    return this.deps.repository.get(scope, id);
  }

  /**
   * Refuses a connection the workspace's package does not allow.
   *
   * Re-authorising an account the workspace already has is not a new
   * connection, so the accounts it already holds are subtracted before the
   * package's allowance is asked.
   */
  private async checkConnectionLimit(scope: Scope, input: ConnectInput): Promise<void> {
    if (!this.deps.limits) {
      return;
    }

    const limit = await this.deps.limits.maxIntegrations(scope.workspaceId);
    if (limit <= 0) {
      return;
    }

    const existing = await this.deps.repository.list(scope);

    let held = 0;
    for (const connection of existing) {
      if (connection.app === input.app && connection.accountLabel === input.accountLabel) {
        // The same account, so this is a re-authorisation rather than a new
        // connection.
        return;
      }
      held++;
    }

    if (held >= limit) {
      throw wrap(
        ErrPlanLimit,
        `paket ini mengizinkan ${limit} koneksi; putuskan salah satu atau naikkan paket`,
      );
    }
  }

  /** Stores a completed authorisation, sealing the tokens first. */
  async connect(ctx: unknown, scope: Scope, input: ConnectInput): Promise<Integration> {
    const failure = validateConnectInput(input);
    if (failure) {
      throw wrap(ErrInvalidInput, failure.message);
    }
    if (!this.deps.cipher) {
      throw ErrNoCipher;
    }
    await this.checkConnectionLimit(scope, input);

    let access: Uint8Array;
    try {
      access = await this.deps.cipher.seal(ctx, scope.workspaceId, input.accessToken);
    } catch (err) {
      throw new Error(`integration: seal access token: ${errorMessage(err)}`);
    }

    let refresh: Uint8Array = new Uint8Array();
    if (input.refreshToken !== "") {
      try {
        refresh = await this.deps.cipher.seal(ctx, scope.workspaceId, input.refreshToken);
      } catch (err) {
        throw new Error(`integration: seal refresh token: ${errorMessage(err)}`);
      }
    }

    const stored = await this.deps.repository.upsert(scope, input, {
      accessTokenEncrypted: access,
      refreshTokenEncrypted: refresh,
      expiresAt: input.expiresAt,
      scopes: input.scopes,
    });

    // The app and the account, never the token.
    this.deps.logger.info(
      { app: stored.app, account: stored.accountLabel, scopes: stored.scopes.length },
      "integration connected",
    );
    return stored;
  }

  /**
   * Opens the stored token of a connection. This is the only path that
   * decrypts, and it is called where the token is about to be used.
   */
  async token(ctx: unknown, scope: Scope, id: string): Promise<Token> {
    if (isNilId(id)) {
      throw wrap(ErrInvalidInput, "an integration id is required");
    }

    const integration = await this.deps.repository.get(scope, id);

    const sealed = await this.deps.repository.sealed(scope, id);
    if (!sealed.accessTokenEncrypted || sealed.accessTokenEncrypted.length === 0) {
      throw wrap(ErrNoToken, `(status ${integration.status})`);
    }
    if (!this.deps.cipher) {
      throw ErrNoCipher;
    }

    let access: string;
    try {
      access = await this.deps.cipher.open(ctx, scope.workspaceId, sealed.accessTokenEncrypted);
    } catch (err) {
      throw new Error(`integration: open access token: ${errorMessage(err)}`);
    }

    let refresh = "";
    if (sealed.refreshTokenEncrypted && sealed.refreshTokenEncrypted.length > 0) {
      try {
        refresh = await this.deps.cipher.open(ctx, scope.workspaceId, sealed.refreshTokenEncrypted);
      } catch (err) {
        throw new Error(`integration: open refresh token: ${errorMessage(err)}`);
      }
    }

    return {
      accessToken: access,
      refreshToken: refresh,
      expiresAt: sealed.expiresAt,
      scopes: sealed.scopes,
    };
  }

  /** Stores the result of a refresh the caller performed. */
  async refresh(ctx: unknown, scope: Scope, id: string, input: RefreshInput): Promise<Integration> {
    if (isNilId(id)) {
      throw wrap(ErrInvalidInput, "an integration id is required");
    }
    if (input.accessToken.trim() === "") {
      throw wrap(ErrInvalidInput, "a refreshed access token is required");
    }
    if (!this.deps.cipher) {
      throw ErrNoCipher;
    }

    let access: Uint8Array;
    try {
      access = await this.deps.cipher.seal(ctx, scope.workspaceId, input.accessToken);
    } catch (err) {
      throw new Error(`integration: seal access token: ${errorMessage(err)}`);
    }

    let refresh: Uint8Array = new Uint8Array();
    if (input.refreshToken !== "") {
      try {
        refresh = await this.deps.cipher.seal(ctx, scope.workspaceId, input.refreshToken);
      } catch (err) {
        throw new Error(`integration: seal refresh token: ${errorMessage(err)}`);
      }
    }

    await this.deps.repository.saveRefreshed(scope, id, {
      accessTokenEncrypted: access,
      refreshTokenEncrypted: refresh,
      expiresAt: input.expiresAt,
      scopes: input.scopes,
    });

    return this.deps.repository.get(scope, id);
  }

  /** Marks a connection whose refresh failed, keeping its data. */
  async needsRelogin(_ctx: unknown, scope: Scope, id: string, reason: string): Promise<Integration> {
    if (isNilId(id)) {
      throw wrap(ErrInvalidInput, "an integration id is required");
    }

    const trimmed = reason.trim() === "" ? "penyedia menolak token; sambungkan ulang" : reason.trim();

    await this.deps.repository.setStatus(scope, id, "needs_relogin", trimmed);

    this.deps.logger.warn({ integration: id, reason: trimmed }, "integration needs a fresh login");
    return this.deps.repository.get(scope, id);
  }

  /** Clears the stored tokens. */
  async disconnect(_ctx: unknown, scope: Scope, id: string): Promise<void> {
    if (isNilId(id)) {
      throw wrap(ErrInvalidInput, "an integration id is required");
    }
    return this.deps.repository.disconnect(scope, id);
  }

  /** Removes the connection. */
  async delete(_ctx: unknown, scope: Scope, id: string): Promise<void> {
    if (isNilId(id)) {
      throw wrap(ErrInvalidInput, "an integration id is required");
    }
    return this.deps.repository.delete(scope, id);
  }

  /** Re-seals the workspace's data key under the active key-management key. */
  async rotateDataKey(ctx: unknown, scope: Scope): Promise<void> {
    if (!this.deps.cipher) {
      throw ErrNoCipher;
    }
    try {
      await this.deps.cipher.rotate(ctx, scope.workspaceId);
    } catch (err) {
      throw new Error(`integration: rotate data key: ${errorMessage(err)}`);
    }
  }

  /**
   * Runs one tool call against one connection.
   *
   * The order is the point: look the tool up, check the connection speaks that
   * application, validate the arguments, and only then open the token.
   */
  async execute(
    ctx: unknown,
    scope: Scope,
    integrationId: string,
    tool: string,
    args: string,
  ): Promise<Result> {
    const found = this.deps.registry ? this.deps.registry.lookup(tool) : null;
    if (!found) {
      // No native adapter offers it, so it may belong to a server this
      // workspace added.
      return executeMCP(this, ctx, scope, tool, args);
    }
    const { spec, adapter } = found;
    const app = spec.name.slice(0, spec.name.indexOf("."));

    // A tool that belongs to the workspace itself runs with no connection at
    // all.
    if (isNilId(integrationId)) {
      if (!isConnectionless(adapter) || adapter.needsConnection()) {
        throw wrap(ErrInvalidInput, "an integration id is required");
      }
      return this.runConnectionless(ctx, spec, adapter, scope, args);
    }

    // The tool has to belong to the application the connection is for.
    const integration = await this.deps.repository.get(scope, integrationId);
    if (integration.app !== app) {
      throw wrap(ErrInvalidInput, `${JSON.stringify(tool)} is not a ${integration.app} tool`);
    }

    if (this.deps.validator) {
      await this.deps.validator.validate(spec.schema, args);
    }

    // The limit is checked before the token is opened.
    if (this.deps.limiter) {
      const decision = await this.deps.limiter.allow(ctx, app, integrationId);
      if (!decision.allowed) {
        this.deps.logger.info(
          { app, integration: integrationId, per_minute: decision.perMinute, retry_after: decision.retryAfter },
          "an integration call was held",
        );
        throw new RateLimitedError(app, decision.perMinute, decision.retryAfter);
      }
    }

    // Opened here, used immediately, and never stored on the result. An adapter
    // that declares it needs no token is called without one.
    let token: Token = { accessToken: "", refreshToken: "", expiresAt: null, scopes: [] };
    if (!isCredentialled(adapter) || adapter.needsToken()) {
      token = await this.token(ctx, scope, integrationId);
    }

    try {
      return await adapter.execute(ctx, {
        scope,
        integrationId,
        tool: spec.name,
        arguments: args,
        token,
      });
    } catch (err) {
      throw new Error(`integration: ${spec.name}: ${errorMessage(err)}`);
    }
  }

  /**
   * Runs a tool that needs no connection.
   *
   * The rate limiter is skipped deliberately: its bucket is keyed by the
   * connection, so there is nothing to pace a workspace's own catalogue
   * against. Everything else is the same path as a connected call.
   */
  private async runConnectionless(
    ctx: unknown,
    spec: ToolSpec,
    adapter: Adapter,
    scope: Scope,
    args: string,
  ): Promise<Result> {
    if (this.deps.validator) {
      await this.deps.validator.validate(spec.schema, args);
    }

    try {
      return await adapter.execute(ctx, {
        scope,
        integrationId: NilUUID,
        tool: spec.name,
        arguments: args,
        token: { accessToken: "", refreshToken: "", expiresAt: null, scopes: [] },
      });
    } catch (err) {
      throw new Error(`integration: ${spec.name}: ${errorMessage(err)}`);
    }
  }

  /** The adapters this deployment knows. */
  registry(): Registry | null {
    return this.deps.registry;
  }

  /** The grants of one Bolu. */
  async grants(_ctx: unknown, scope: Scope, agentId: string): Promise<Grant[]> {
    if (isNilId(agentId)) {
      throw wrap(ErrInvalidInput, "a Bolu id is required");
    }
    return this.deps.repository.grants(scope, agentId);
  }

  /** Grants or updates one Bolu's permission on one integration. */
  async setGrant(
    ctx: unknown,
    scope: Scope,
    agentId: string,
    integrationId: string,
    permission: string,
  ): Promise<Grant> {
    if (isNilId(agentId) || isNilId(integrationId)) {
      throw wrap(ErrInvalidInput, "a Bolu id and an integration id are required");
    }
    if (!validPermission(permission)) {
      throw wrap(ErrUnknownPermission, JSON.stringify(permission));
    }

    // The integration must be in this workspace and must actually work.
    const integration = await this.deps.repository.get(scope, integrationId);
    if (integration.status === StatusDisconnected) {
      throw wrap(ErrInvalidInput, "the connection is not authorised yet");
    }

    const grant = await this.deps.repository.setGrant(scope, agentId, integrationId, permission);

    await this.audit(ctx, scope, {
      action: "grant.set",
      objectType: "integration",
      objectId: integrationId,
      actorUserId: scope.userId ?? NilUUID,
      metadata: { agent_id: agentId, permission: grant.permission, app: integration.app },
    });

    return grant;
  }

  /**
   * Records one entry. A trail that could not be written is reported rather
   * than failing the action.
   */
  private async audit(ctx: unknown, scope: Scope, entry: AuditEntry): Promise<void> {
    if (!this.deps.auditor) {
      return;
    }
    try {
      await this.deps.auditor.record(ctx, scope, entry);
    } catch (err) {
      this.deps.logger.warn({ action: entry.action, err }, "could not record a grant audit entry");
    }
  }

  /** Removes one grant. */
  async deleteGrant(ctx: unknown, scope: Scope, agentId: string, integrationId: string): Promise<void> {
    if (isNilId(agentId) || isNilId(integrationId)) {
      throw wrap(ErrInvalidInput, "a Bolu id and an integration id are required");
    }
    // The grant is read first so the line can name what access was taken away,
    // and only when something will record it.
    let permission = "";
    let app = "";
    if (this.deps.auditor) {
      const existing = await this.deps.repository.grants(scope, agentId);
      for (const grant of existing) {
        if (grant.integrationId === integrationId) {
          permission = grant.permission;
          try {
            app = (await this.deps.repository.get(scope, integrationId)).app;
          } catch {
            // A connection that vanished leaves the line without an app.
          }
          break;
        }
      }
    }

    await this.deps.repository.deleteGrant(scope, agentId, integrationId);

    await this.audit(ctx, scope, {
      action: "grant.removed",
      objectType: "integration",
      objectId: integrationId,
      actorUserId: scope.userId ?? NilUUID,
      metadata: { agent_id: agentId, permission, app },
    });
  }

  /** The permission a Bolu holds on each integration it may use. */
  async allowedPermissions(_ctx: unknown, scope: Scope, agentId: string): Promise<Map<string, string>> {
    const grants = await this.deps.repository.grants(scope, agentId);
    const out = new Map<string, string>();
    for (const grant of grants) {
      out.set(grant.integrationId, grant.permission);
    }
    return out;
  }

  // ------------------------------------------------------- the other halves

  /** Records a connection whose credential this deployment holds itself. */
  link(ctx: unknown, scope: Scope, input: LinkInput): Promise<Integration> {
    return linkConnection(this, ctx, scope, input);
  }

  beginAuth(ctx: unknown, scope: Scope, app: string, redirectUri: string, returnTo: string): Promise<AuthURL> {
    return beginAuth(this, ctx, scope, app, redirectUri, returnTo);
  }

  completeAuth(ctx: unknown, req: CompleteAuthRequest): Promise<Integration> {
    return completeAuth(this, ctx, req);
  }

  refreshConnection(ctx: unknown, scope: Scope, id: string): Promise<Integration> {
    return refreshConnection(this, ctx, scope, id);
  }

  refreshExpiring(ctx: unknown, scope: Scope): Promise<Integration[]> {
    return refreshExpiring(this, ctx, scope);
  }

  webhookUrl(ctx: unknown, scope: Scope, integrationId: string, baseUrl: string): Promise<string> {
    return webhookUrl(this, ctx, scope, integrationId, baseUrl);
  }

  receiveWebhook(ctx: unknown, app: string, request: WebhookRequest): Promise<number> {
    return receiveWebhook(this, ctx, app, request);
  }

  pendingWebhooks(ctx: unknown, scope: Scope, limit: number): Promise<QueuedEvent[]> {
    return pendingWebhooks(this, ctx, scope, limit);
  }

  claimPendingEvents(ctx: unknown, scope: Scope, event: string, limit: number): Promise<QueuedEvent[]> {
    return claimPendingEvents(this, ctx, scope, event, limit);
  }

  webhookProcessed(ctx: unknown, scope: Scope, id: string): Promise<void> {
    return webhookProcessed(this, ctx, scope, id);
  }

  webhookFailed(ctx: unknown, scope: Scope, id: string, reason: string): Promise<void> {
    return webhookFailed(this, ctx, scope, id, reason);
  }

  addMCPServer(ctx: unknown, scope: Scope, input: MCPServerInput): Promise<MCPServer> {
    return addMCPServer(this, ctx, scope, input);
  }

  listMCPServers(ctx: unknown, scope: Scope): Promise<MCPServer[]> {
    return listMCPServers(this, ctx, scope);
  }

  refreshMCPServer(ctx: unknown, scope: Scope, id: string): Promise<MCPServer> {
    return refreshMCPServer(this, ctx, scope, id);
  }

  removeMCPServer(ctx: unknown, scope: Scope, id: string): Promise<void> {
    return removeMCPServer(this, ctx, scope, id);
  }

  mcpTools(ctx: unknown, scope: Scope): Promise<ToolSpec[]> {
    return mcpTools(this, ctx, scope);
  }

  // ------------------------------------------------- the pieces those need

  /** The clock, which a test can freeze. */
  now(): Date {
    return this.deps.clock ? this.deps.clock() : new Date();
  }

  /** Finds an adapter that is connected through OAuth. */
  oauthAdapter(app: string): OAuthAdapter | null {
    if (!this.deps.registry) {
      return null;
    }
    const adapter = this.deps.registry.adapter(app);
    if (!adapter) {
      return null;
    }
    const candidate = adapter as Partial<OAuthAdapter>;
    return typeof candidate.authCodeUrl === "function" ? (adapter as OAuthAdapter) : null;
  }

  /** Names the account a flow connected. */
  accountLabel(ctx: unknown, adapter: OAuthAdapter, tokens: OAuthTokens): Promise<string> {
    return accountLabelFor(this, ctx, adapter, tokens);
  }

  /** Hands each newly queued event kind to the routines watching it. */
  startTriggers(ctx: unknown, scope: Scope, events: Set<string>): Promise<void> {
    return startTriggers(this, ctx, scope, events);
  }

  /** Finds an adapter that receives pushes. */
  webhookAdapter(app: string): TriggerAdapter | null {
    if (!this.deps.registry) {
      return null;
    }
    const adapter = this.deps.registry.adapter(app);
    if (!adapter) {
      return null;
    }
    const candidate = adapter as Partial<TriggerAdapter>;
    return typeof candidate.verifyWebhook === "function" ? (adapter as TriggerAdapter) : null;
  }

  /** Asks a server what it offers and records the answer. */
  refreshMCPTools(ctx: unknown, scope: Scope, server: MCPServer, credential: string): Promise<MCPTool[]> {
    return refreshMCPTools(this, ctx, scope, server, credential);
  }

  /** Opens a server's stored credential. */
  mcpCredential(ctx: unknown, scope: Scope, id: string): Promise<string> {
    return mcpCredential(this, ctx, scope, id);
  }

  /** Notes why a server could not be reached, so a screen can say so. */
  recordMCPFailure(ctx: unknown, scope: Scope, id: string, err: unknown): Promise<void> {
    return recordMCPFailure(this, ctx, scope, id, err);
  }
}
