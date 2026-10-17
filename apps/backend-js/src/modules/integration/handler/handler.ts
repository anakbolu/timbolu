/**
 * The integration hub over HTTP: the connected accounts, the grants that say
 * which Bolu may use them, and the two halves of an authorization flow.
 *
 * The callback is the only endpoint here that runs without a session. It
 * arrives as a browser redirect from the provider, so it learns its tenant from
 * the signed state rather than from a header, and it answers with a redirect to
 * the app instead of a JSON body — a person is watching it happen.
 *
 * Ported 1:1 from internal/modules/integration/handler/handler.go.
 */

import type { Hono } from "hono";
import {
  currentUserId,
  isUuid,
  workspaceUUID,
  type AppContext,
  type AppEnv,
} from "../../../platform/context.ts";
import type { Logger } from "../../../platform/logger.ts";
import { created, error, ok } from "../../../platform/response.ts";
import {
  ErrInvalidInput,
  ErrNoCipher,
  ErrNoToken,
  ErrNotFound,
  ErrPlanLimit,
  ErrUnknownPermission,
  ErrUnknownStatus,
  is,
  type Grant,
  type Integration,
  type Scope,
  type Service,
} from "../domain/integration.ts";
import { ErrMCPServerNotFound, ErrMCPUnavailable, type MCPServer } from "../domain/mcp.ts";
import { ErrDeliveryExists, ErrNoWebhook, ErrSignatureInvalid } from "../domain/webhook.ts";
import { ErrExchangeFailed, ErrInvalidState, OAuthRedirectURI } from "../domain/oauth.ts";
import { ErrInvalidSpec, ErrUnknownAdapter, ErrUnknownTool, isOAuthAdapter } from "../domain/adapter.ts";

/** Where the handler sends a person. */
export interface Config {
  /** The app's origin, which is where a finished flow returns to. */
  frontendUrl: string;
  /** The public origin providers redirect back to. */
  redirectBase: string;
}

/** Serves the integration endpoints. */
export class Handler {
  private readonly frontendUrl: string;
  private readonly redirectBase: string;

  constructor(
    private readonly service: Service,
    private readonly log: Logger,
    cfg: Config,
  ) {
    this.frontendUrl = cfg.frontendUrl.replace(/\/+$/, "");
    this.redirectBase = cfg.redirectBase.replace(/\/+$/, "");
  }

  // ------------------------------------------------------------- the routes

  /** Receives a delivery from a provider. */
  webhook = async (c: AppContext): Promise<Response> => {
    const app = (c.req.param("app") ?? "").trim();

    const headers: Record<string, string> = {};
    for (const [key, value] of Object.entries(c.req.header())) {
      headers[key.toLowerCase()] = value;
    }

    // The body is read as bytes: a signature is computed over what was sent,
    // and a parsed body would not verify.
    const body = new Uint8Array(await c.req.arrayBuffer());

    try {
      const accepted = await this.service.receiveWebhook(c, app, {
        app,
        headers,
        body,
        query: c.req.query(),
        receivedAt: new Date(),
      });
      this.log.info({ app, events: accepted }, "a webhook delivery was accepted");
      return ok(c, "accepted", { accepted });
    } catch (err) {
      return this.webhookFail(c, app, err);
    }
  };

  /**
   * Answers a provider.
   *
   * The codes are chosen for what a provider does with them: 401 for a request
   * that is not theirs, so it stops; 400 for one that is theirs but unreadable;
   * and 200 for a duplicate, because retrying is what it would otherwise do.
   */
  private webhookFail(c: AppContext, app: string, err: unknown): Response {
    if (is(err, ErrSignatureInvalid)) {
      this.log.warn({ app, err }, "a webhook was refused");
      return error(c, 401, "invalid signature", "invalid_signature");
    }
    if (is(err, ErrDeliveryExists)) {
      // Already queued, which is a success from the provider's point of view.
      return ok(c, "already accepted", { accepted: 0 });
    }
    if (is(err, ErrNoWebhook) || is(err, ErrUnknownAdapter) || is(err, ErrInvalidInput)) {
      return error(c, 400, messageOf(err), "invalid_input");
    }
    this.log.error({ app, err }, "a webhook could not be queued");
    return error(c, 500, "internal server error", "internal_error");
  }

  /** The workspace's connections. */
  list = async (c: AppContext): Promise<Response> => {
    try {
      const connections = await this.service.list(c, requestScopeOf(c));
      return ok(c, "ok", connections.map(toPayload));
    } catch (err) {
      return this.fail(c, "list integrations", err);
    }
  };

  /** One connection. */
  get = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("integrationID");
    if (!isUuid(id)) {
      return error(c, 400, "integrationID is not a uuid", "invalid_input");
    }

    try {
      const connection = await this.service.get(c, requestScopeOf(c), id);
      return ok(c, "ok", toPayload(connection));
    } catch (err) {
      return this.fail(c, "get integration", err);
    }
  };

  /**
   * Lists the applications this deployment speaks, and what each offers.
   *
   * It is what the Integrations screen shows before anything is connected.
   */
  adapters = async (c: AppContext): Promise<Response> => {
    const registry = this.service.registry();
    if (!registry) {
      return ok(c, "ok", [] as AdapterPayload[]);
    }

    const payload: AdapterPayload[] = [];
    for (const app of registry.apps()) {
      const entry: AdapterPayload = {
        app,
        oauth: false,
        tools: registry.toolsFor(app).map((tool) => ({
          name: tool.name,
          label: tool.label,
          description: tool.description,
          needs_human: tool.label === "write_external",
        })),
      };

      const adapter = registry.adapter(app);
      if (adapter) {
        const triggers = adapter.triggers();
        if (triggers.length > 0) {
          entry.triggers = triggers.map((trigger) => ({
            kind: trigger.kind,
            event: trigger.event,
            description: trigger.description,
          }));
        }
        if (isOAuthAdapter(adapter)) {
          entry.oauth = true;
          entry.default_scopes = adapter.defaultScopes();
        }
      }
      payload.push(entry);
    }

    return ok(c, "ok", payload);
  };

  /**
   * Starts an authorization flow and answers with the provider's consent URL.
   *
   * It answers JSON rather than redirecting: the app authenticates with a
   * bearer token held in memory, so a plain navigation here would carry no
   * credential and would be refused.
   */
  connect = async (c: AppContext): Promise<Response> => {
    const app = (c.req.param("app") ?? "").trim();
    if (app === "") {
      return error(c, 400, "app is required", "invalid_input");
    }

    try {
      const auth = await this.service.beginAuth(
        c,
        requestScopeOf(c),
        app,
        this.callbackUri(c),
        c.req.query("return_to") ?? "",
      );
      return ok(c, "ok", { url: auth.url, state: auth.state });
    } catch (err) {
      return this.fail(c, "begin authorization", err);
    }
  };

  /**
   * Finishes an authorization flow and returns the person to the app.
   *
   * Every outcome is a redirect, because a person is watching a browser tab.
   */
  callback = async (c: AppContext): Promise<Response> => {
    // The provider reports a refusal as an error parameter rather than a code.
    const providerErr = c.req.query("error");
    if (providerErr) {
      this.log.info(
        { error: providerErr, description: c.req.query("error_description") },
        "a person declined the authorization",
      );
      return this.redirect(c, "/dashboard/integrations", "declined", providerErr);
    }

    try {
      const connection = await this.service.completeAuth(c, {
        code: c.req.query("code") ?? "",
        state: c.req.query("state") ?? "",
        redirectUri: this.callbackUri(c),
      });
      this.log.info({ app: connection.app, account: connection.accountLabel }, "an integration was connected");
      return this.redirect(c, "/dashboard/integrations", "connected", connection.app);
    } catch (err) {
      const reason = classify(err);
      this.log.warn({ reason, err }, "an authorization callback failed");
      return this.redirect(c, "/dashboard/integrations", "error", reason);
    }
  };

  /** Clears a connection's stored tokens. */
  disconnect = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("integrationID");
    if (!isUuid(id)) {
      return error(c, 400, "integrationID is not a uuid", "invalid_input");
    }

    try {
      await this.service.disconnect(c, requestScopeOf(c), id);
      return ok(c, "the account was disconnected");
    } catch (err) {
      return this.fail(c, "disconnect integration", err);
    }
  };

  /** Removes a connection. */
  delete = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("integrationID");
    if (!isUuid(id)) {
      return error(c, 400, "integrationID is not a uuid", "invalid_input");
    }

    try {
      await this.service.delete(c, requestScopeOf(c), id);
      return ok(c, "the connection was removed");
    } catch (err) {
      return this.fail(c, "delete integration", err);
    }
  };

  /** Trades a stored refresh token for a new access token. */
  refresh = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("integrationID");
    if (!isUuid(id)) {
      return error(c, 400, "integrationID is not a uuid", "invalid_input");
    }

    try {
      const connection = await this.service.refreshConnection(c, requestScopeOf(c), id);
      return ok(c, "ok", toPayload(connection));
    } catch (err) {
      return this.fail(c, "refresh integration", err);
    }
  };

  /** Lists the integrations one Bolu may use. */
  grants = async (c: AppContext): Promise<Response> => {
    const agentId = c.req.param("agentID");
    if (!isUuid(agentId)) {
      return error(c, 400, "agentID is not a uuid", "invalid_input");
    }

    try {
      const grants = await this.service.grants(c, requestScopeOf(c), agentId);
      return ok(c, "ok", grants.map(toGrantPayload));
    } catch (err) {
      return this.fail(c, "list grants", err);
    }
  };

  /** Grants or updates one Bolu's permission on one integration. */
  setGrant = async (c: AppContext): Promise<Response> => {
    const agentId = c.req.param("agentID");
    if (!isUuid(agentId)) {
      return error(c, 400, "agentID is not a uuid", "invalid_input");
    }
    const integrationId = c.req.param("integrationID");
    if (!isUuid(integrationId)) {
      return error(c, 400, "integrationID is not a uuid", "invalid_input");
    }

    const body = await c.req.json().catch(() => null);
    if (!body || typeof body !== "object") {
      return error(c, 400, "the body is not readable", "invalid_input");
    }
    const permission = "permission" in body && typeof body.permission === "string" ? body.permission : "";

    try {
      const grant = await this.service.setGrant(c, requestScopeOf(c), agentId, integrationId, permission);
      return ok(c, "the grant was saved", toGrantPayload(grant));
    } catch (err) {
      return this.fail(c, "set grant", err);
    }
  };

  /** Removes one grant. */
  deleteGrant = async (c: AppContext): Promise<Response> => {
    const agentId = c.req.param("agentID");
    if (!isUuid(agentId)) {
      return error(c, 400, "agentID is not a uuid", "invalid_input");
    }
    const integrationId = c.req.param("integrationID");
    if (!isUuid(integrationId)) {
      return error(c, 400, "integrationID is not a uuid", "invalid_input");
    }

    try {
      await this.service.deleteGrant(c, requestScopeOf(c), agentId, integrationId);
      return ok(c, "the grant was removed");
    } catch (err) {
      return this.fail(c, "delete grant", err);
    }
  };

  /** The servers the workspace added. */
  listMCPServers = async (c: AppContext): Promise<Response> => {
    try {
      const servers = await this.service.listMCPServers(c, requestScopeOf(c));
      return ok(c, "ok", servers.map(toMCPServerPayload));
    } catch (err) {
      return this.fail(c, "list MCP servers", err);
    }
  };

  /** Registers a server and asks it what it offers. */
  addMCPServer = async (c: AppContext): Promise<Response> => {
    const body = await c.req.json().catch(() => null);
    if (!body || typeof body !== "object") {
      return error(c, 400, "the body is not readable", "invalid_input");
    }

    const request: MCPServerRequest = {
      app: stringField(body, "app"),
      label: stringField(body, "label"),
      endpoint: stringField(body, "endpoint"),
      credential: stringField(body, "credential"),
      // A server nobody enabled is one somebody added but does not want used
      // yet, which is the sensible reading of an absent field.
      enabled: "enabled" in body && typeof body.enabled === "boolean" ? body.enabled : true,
    };

    try {
      const server = await this.service.addMCPServer(c, requestScopeOf(c), request);
      return created(c, "the server was added", toMCPServerPayload(server));
    } catch (err) {
      return this.fail(c, "add MCP server", err);
    }
  };

  /** Asks one server what it offers. */
  refreshMCPServer = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("serverID");
    if (!isUuid(id)) {
      return error(c, 400, "serverID is not a uuid", "invalid_input");
    }

    try {
      const server = await this.service.refreshMCPServer(c, requestScopeOf(c), id);
      return ok(c, "ok", toMCPServerPayload(server));
    } catch (err) {
      return this.fail(c, "refresh MCP server", err);
    }
  };

  /** Removes one. */
  removeMCPServer = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("serverID");
    if (!isUuid(id)) {
      return error(c, 400, "serverID is not a uuid", "invalid_input");
    }

    try {
      await this.service.removeMCPServer(c, requestScopeOf(c), id);
      return ok(c, "the server was removed");
    } catch (err) {
      return this.fail(c, "remove MCP server", err);
    }
  };

  // ---------------------------------------------------------------- helpers

  /** Sends the browser back into the app with the outcome in the query. */
  private redirect(c: AppContext, path: string, status: string, detail: string): Response {
    let target = `${this.frontendUrl}${path}?integration=${status}`;
    if (detail !== "") {
      target += `&detail=${urlEscape(detail)}`;
    }
    return c.redirect(target, 302);
  }

  /**
   * The address this deployment registers with providers. It has to match the
   * one the authorization request used, because providers check.
   */
  private callbackUri(c: AppContext): string {
    if (this.redirectBase !== "") {
      return this.redirectBase + OAuthRedirectURI;
    }
    return new URL(c.req.url).origin + OAuthRedirectURI;
  }

  /** Maps the module's sentinels onto responses. */
  private fail(c: AppContext, action: string, err: unknown): Response {
    if (is(err, ErrNotFound)) {
      return error(c, 404, "not found", "not_found");
    }
    if (is(err, ErrMCPServerNotFound)) {
      return error(c, 404, "the MCP server was not found", "not_found");
    }
    if (is(err, ErrMCPUnavailable)) {
      return error(c, 502, "the MCP server could not be reached", "mcp_unavailable");
    }
    if (is(err, ErrNoCipher)) {
      return error(c, 503, "no encryption key is configured, so tokens cannot be stored", "integration_not_configured");
    }
    if (is(err, ErrNoToken)) {
      return error(c, 409, "the connection has no usable token; reconnect it", "integration_needs_relogin");
    }
    if (is(err, ErrPlanLimit)) {
      // 409 rather than 403: the answer is to buy a bigger package, not to ask
      // somebody else.
      return error(c, 409, planLimitMessage(err, ErrPlanLimit), "plan_limit_reached");
    }
    if (
      is(err, ErrInvalidInput) ||
      is(err, ErrUnknownPermission) ||
      is(err, ErrUnknownStatus) ||
      is(err, ErrUnknownAdapter) ||
      is(err, ErrUnknownTool) ||
      is(err, ErrInvalidSpec)
    ) {
      return error(c, 400, messageOf(err), "invalid_input");
    }
    this.log.error({ err }, `${action} failed`);
    return error(c, 500, "internal server error", "internal_error");
  }
}

/** Registers the endpoints a member of the workspace may use. */
export function routes(group: Hono<AppEnv>, h: Handler): void {
  group.get("/integrations", h.list);
  group.get("/integrations/adapters", h.adapters);
  group.get("/integrations/:integrationID", h.get);
  group.post("/integrations/:integrationID/disconnect", h.disconnect);
  group.delete("/integrations/:integrationID", h.delete);
  group.post("/integrations/:integrationID/refresh", h.refresh);
  group.get("/integrations/:app/connect", h.connect);
  group.get("/agents/:agentID/grants", h.grants);
  group.put("/agents/:agentID/grants/:integrationID", h.setGrant);
  group.delete("/agents/:agentID/grants/:integrationID", h.deleteGrant);

  // The MCP servers a workspace added. They are per workspace rather than per
  // connection, which is why they are their own set of routes.
  group.get("/mcp/servers", h.listMCPServers);
  group.post("/mcp/servers", h.addMCPServer);
  group.post("/mcp/servers/:serverID/refresh", h.refreshMCPServer);
  group.delete("/mcp/servers/:serverID", h.removeMCPServer);
}

/**
 * Registers the endpoints a provider reaches directly.
 *
 * They are public by necessity: the provider sends a browser to the first and a
 * server to the second, and neither carries a session. What makes them safe is
 * what each carries instead — a signed, single-use state for the redirect, and
 * a signed reference plus the adapter's own signature for the webhook.
 */
export function callbackRoutes(group: Hono<AppEnv>, h: Handler): void {
  group.get("/integrations/oauth/callback", h.callback);
  group.post("/webhooks/:app", h.webhook);
}

/** Turns an error into a short code the screen can act on. */
function classify(err: unknown): string {
  if (is(err, ErrInvalidState)) {
    return "invalid_state";
  }
  if (is(err, ErrExchangeFailed)) {
    return "exchange_failed";
  }
  if (is(err, ErrUnknownAdapter)) {
    return "unknown_app";
  }
  return "failed";
}

/**
 * The service's own sentence, with this module's prefix removed, so what a
 * person reads is about the package rather than about which module noticed.
 */
function planLimitMessage(err: unknown, prefix: Error): string {
  const raw = messageOf(err);
  let message = raw;
  if (message.startsWith(prefix.message)) {
    message = message.slice(prefix.message.length);
  }
  message = message.replace(/^:/, "").trim();
  return message === "" ? prefix.message : message;
}

/** The tenant the request runs as. */
function requestScopeOf(c: AppContext): Scope {
  return { userId: currentUserId(c), workspaceId: workspaceUUID(c) ?? "" };
}

function messageOf(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

function stringField(body: object, key: string): string {
  if (key in body) {
    const value = (body as Record<string, unknown>)[key];
    if (typeof value === "string") {
      return value;
    }
  }
  return "";
}

/** Escapes a query value without pulling in a dependency for it. */
function urlEscape(value: string): string {
  let out = "";
  for (const byte of new TextEncoder().encode(value)) {
    if (
      (byte >= 0x30 && byte <= 0x39) ||
      (byte >= 0x41 && byte <= 0x5a) ||
      (byte >= 0x61 && byte <= 0x7a) ||
      byte === 0x2d ||
      byte === 0x5f ||
      byte === 0x2e ||
      byte === 0x7e
    ) {
      out += String.fromCharCode(byte);
    } else {
      out += `%${byte.toString(16).toUpperCase().padStart(2, "0")}`;
    }
  }
  return out;
}

// ------------------------------------------------------------------ payloads

interface IntegrationPayload {
  integration_id: string;
  app: string;
  account_label: string;
  status: string;
  status_reason?: string;
  scopes: string[];
  expires_at?: string;
  last_refreshed_at?: string;
}

interface AdapterPayload {
  app: string;
  oauth: boolean;
  default_scopes?: string[];
  tools: { name: string; label: string; description: string; needs_human: boolean }[];
  triggers?: { kind: string; event: string; description: string }[];
}

interface GrantPayload {
  integration_id: string;
  permission: string;
}

interface MCPServerRequest {
  app: string;
  label: string;
  endpoint: string;
  credential: string;
  enabled: boolean;
}

interface MCPServerPayload {
  id: string;
  app: string;
  label: string;
  endpoint: string;
  enabled: boolean;
  tool_count: number;
  last_error?: string;
}

function toGrantPayload(grant: Grant): GrantPayload {
  return { integration_id: grant.integrationId, permission: grant.permission };
}

function toMCPServerPayload(server: MCPServer): MCPServerPayload {
  const payload: MCPServerPayload = {
    id: server.id,
    app: server.app,
    label: server.label,
    endpoint: server.endpoint,
    enabled: server.enabled,
    tool_count: server.toolCount,
  };
  if (server.lastError !== "") {
    payload.last_error = server.lastError;
  }
  return payload;
}

/** Renders a connection for a client. */
function toPayload(connection: Integration): IntegrationPayload {
  const payload: IntegrationPayload = {
    integration_id: connection.id,
    app: connection.app,
    account_label: connection.accountLabel,
    status: connection.status,
    scopes: connection.scopes ?? [],
  };
  if (connection.statusReason !== "") {
    payload.status_reason = connection.statusReason;
  }
  if (connection.expiresAt) {
    payload.expires_at = rfc3339(connection.expiresAt);
  }
  if (connection.lastRefreshedAt) {
    payload.last_refreshed_at = rfc3339(connection.lastRefreshedAt);
  }
  return payload;
}

/** Formats a time the way Go's `Format("2006-01-02T15:04:05Z07:00")` does. */
function rfc3339(at: Date): string {
  return at.toISOString().replace(/\.\d{3}Z$/, "Z");
}
