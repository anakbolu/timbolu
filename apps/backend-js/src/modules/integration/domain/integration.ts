/**
 * The integration contract: the account a workspace connected, the token that
 * lets a Bolu act on it, and the grant that says which Bolu may.
 *
 * Two rules live here. A token is only ever handled sealed — the domain has no
 * field for a plaintext one, so a caller cannot hold it by accident. And a tool
 * is only offered to a Bolu whose grant covers it: a read-only grant keeps the
 * tools that cannot change anything outside Bolu, and nothing else.
 *
 * Ported 1:1 from internal/modules/integration/domain/integration.go.
 */

import type { CallLimiter, Registry, Result, SchemaValidator, ToolSpec } from "./adapter.ts";
import type { Logger } from "../../../platform/logger.ts";
import type { MCPServer, MCPServerInput, MCPClientFactory, MCPRepository, MCPTool } from "./mcp.ts";
import type { AuthURL, CompleteAuthRequest, OAuthState, StateCodec, StateStore } from "./oauth.ts";
import type { EnqueueInput, Queue, QueuedEvent, RefCodec, WebhookRequest } from "./webhook.ts";

/** An account whose token works. */
export const StatusConnected = "connected";
/** An account whose refresh failed. */
export const StatusNeedsRelogin = "needs_relogin";
/** An account somebody removed or never finished connecting. */
export const StatusDisconnected = "disconnected";

/** Read-only: the Bolu may look, never change. */
export const PermissionRead = "read";
/** Lets the Bolu act, subject to the approval gate. */
export const PermissionReadWrite = "read_write";

/** The record does not exist in this workspace. */
export const ErrNotFound = new Error("integration: not found");
/** No key-management key is configured, so no token could be sealed. */
export const ErrNoCipher = new Error("integration: no encryption key configured, so tokens cannot be stored");
/** A caller supplied something the schema would refuse. */
export const ErrInvalidInput = new Error("integration: invalid input");
/** A caller asked for a status the schema does not have. */
export const ErrUnknownStatus = new Error("integration: unknown connection status");
/** A caller asked for a permission the schema does not have. */
export const ErrUnknownPermission = new Error("integration: unknown grant permission");
/** The workspace has spent the calls this integration is allowed for now. */
export const ErrRateLimited = new Error("integration: the call limit for this integration is reached");
/** The connection has nothing usable stored. */
export const ErrNoToken = new Error("integration: the connection has no usable token");
/** The workspace's package does not allow another connection. */
export const ErrPlanLimit = new Error("integration: paket workspace tidak mengizinkan koneksi baru");

/** Records what a person did, for the trail. */
export interface Auditor {
  record(ctx: unknown, scope: Scope, entry: AuditEntry): Promise<void>;
}

/** One thing that happened, for the record. */
export interface AuditEntry {
  action: string;
  objectType: string;
  objectId: string;
  actorUserId: string;
  metadata: Record<string, unknown>;
}

/** Reports how many connections a workspace's package allows. */
export interface PlanLimits {
  maxIntegrations(workspaceId: string): Promise<number>;
}

/** The tenant every call is bound to. */
export interface Scope {
  userId?: string;
  workspaceId: string;
  /** Who the call is being made for, when it is made for somebody. */
  agentId?: string;
  taskId?: string;
}

/** A connection this deployment holds the credential for. */
export interface LinkInput {
  app: string;
  accountLabel: string;
  scopes: string[];
}

/** One connected account. It carries no token. */
export interface Integration {
  id: string;
  app: string;
  accountLabel: string;
  status: string;
  statusReason: string;
  scopes: string[];
  expiresAt: Date | null;
  lastRefreshedAt: Date | null;
  createdAt: Date;
  updatedAt: Date;
}

/** A usable credential pair. It only ever exists in memory. */
export interface Token {
  accessToken: string;
  refreshToken: string;
  expiresAt: Date | null;
  scopes: string[];
}

/** What a completed authorisation produces. */
export interface ConnectInput {
  app: string;
  accountLabel: string;
  scopes: string[];
  accessToken: string;
  refreshToken: string;
  expiresAt: Date | null;
}

/** What a provider returned for a refresh. */
export interface RefreshInput {
  accessToken: string;
  refreshToken: string;
  expiresAt: Date | null;
  scopes: string[];
}

/** Which Bolu may use an integration, and how far. */
export interface Grant {
  agentId: string;
  integrationId: string;
  permission: string;
}

/** A data key sealed by the key-management key, with the id of the key that sealed it. */
export interface WrappedKey {
  keyId: string;
  bytes: Uint8Array;
}

/** Seals and opens the per-workspace data keys. */
export interface KeyManager {
  wrap(ctx: unknown, workspaceId: string, dataKey: Uint8Array): Promise<WrappedKey>;
  unwrap(ctx: unknown, workspaceId: string, wrapped: WrappedKey): Promise<Uint8Array>;
  activeKeyId(): string;
}

/** Seals and opens a token with the workspace's data key. */
export interface TokenCipher {
  seal(ctx: unknown, workspaceId: string, plaintext: string): Promise<Uint8Array>;
  open(ctx: unknown, workspaceId: string, ciphertext: Uint8Array): Promise<string>;
  rotate(ctx: unknown, workspaceId: string): Promise<void>;
}

/** What the repository holds: sealed values and their metadata. */
export interface StoredToken {
  accessTokenEncrypted: Uint8Array;
  refreshTokenEncrypted: Uint8Array;
  expiresAt: Date | null;
  scopes: string[];
}

/** The storage the service needs. */
export interface Repository {
  list(scope: Scope): Promise<Integration[]>;
  get(scope: Scope, id: string): Promise<Integration>;
  upsert(scope: Scope, input: ConnectInput, sealed: StoredToken): Promise<Integration>;
  sealed(scope: Scope, id: string): Promise<StoredToken>;
  saveRefreshed(scope: Scope, id: string, sealed: StoredToken): Promise<void>;
  setStatus(scope: Scope, id: string, status: string, reason: string): Promise<void>;
  disconnect(scope: Scope, id: string): Promise<void>;
  delete(scope: Scope, id: string): Promise<void>;

  dataKey(scope: Scope): Promise<WrappedKey>;
  saveDataKey(scope: Scope, wrapped: WrappedKey): Promise<void>;

  saveState(state: OAuthState): Promise<void>;
  consumeState(workspaceId: string, nonce: string, now: Date): Promise<void>;

  grants(scope: Scope, agentId: string): Promise<Grant[]>;
  setGrant(scope: Scope, agentId: string, integrationId: string, permission: string): Promise<Grant>;
  deleteGrant(scope: Scope, agentId: string, integrationId: string): Promise<void>;
}

/** The module's public contract. */
export interface Service {
  list(ctx: unknown, scope: Scope): Promise<Integration[]>;
  get(ctx: unknown, scope: Scope, id: string): Promise<Integration>;
  connect(ctx: unknown, scope: Scope, input: ConnectInput): Promise<Integration>;
  link(ctx: unknown, scope: Scope, input: LinkInput): Promise<Integration>;
  token(ctx: unknown, scope: Scope, id: string): Promise<Token>;
  refresh(ctx: unknown, scope: Scope, id: string, input: RefreshInput): Promise<Integration>;
  needsRelogin(ctx: unknown, scope: Scope, id: string, reason: string): Promise<Integration>;
  disconnect(ctx: unknown, scope: Scope, id: string): Promise<void>;
  delete(ctx: unknown, scope: Scope, id: string): Promise<void>;

  rotateDataKey(ctx: unknown, scope: Scope): Promise<void>;

  grants(ctx: unknown, scope: Scope, agentId: string): Promise<Grant[]>;
  setGrant(ctx: unknown, scope: Scope, agentId: string, integrationId: string, permission: string): Promise<Grant>;
  deleteGrant(ctx: unknown, scope: Scope, agentId: string, integrationId: string): Promise<void>;
  allowedPermissions(ctx: unknown, scope: Scope, agentId: string): Promise<Map<string, string>>;

  execute(ctx: unknown, scope: Scope, integrationId: string, tool: string, args: string): Promise<Result>;

  registry(): Registry | null;

  beginAuth(
    ctx: unknown,
    scope: Scope,
    app: string,
    redirectUri: string,
    returnTo: string,
  ): Promise<AuthURL>;
  completeAuth(ctx: unknown, req: CompleteAuthRequest): Promise<Integration>;
  refreshConnection(ctx: unknown, scope: Scope, id: string): Promise<Integration>;
  refreshExpiring(ctx: unknown, scope: Scope): Promise<Integration[]>;

  webhookUrl(ctx: unknown, scope: Scope, integrationId: string, baseUrl: string): Promise<string>;
  receiveWebhook(ctx: unknown, app: string, request: WebhookRequest): Promise<number>;
  pendingWebhooks(ctx: unknown, scope: Scope, limit: number): Promise<QueuedEvent[]>;
  claimPendingEvents(ctx: unknown, scope: Scope, event: string, limit: number): Promise<QueuedEvent[]>;
  webhookProcessed(ctx: unknown, scope: Scope, id: string): Promise<void>;
  webhookFailed(ctx: unknown, scope: Scope, id: string, reason: string): Promise<void>;

  addMCPServer(ctx: unknown, scope: Scope, input: MCPServerInput): Promise<MCPServer>;
  listMCPServers(ctx: unknown, scope: Scope): Promise<MCPServer[]>;
  refreshMCPServer(ctx: unknown, scope: Scope, id: string): Promise<MCPServer>;
  removeMCPServer(ctx: unknown, scope: Scope, id: string): Promise<void>;
  mcpTools(ctx: unknown, scope: Scope): Promise<ToolSpec[]>;
}

/** Starts the routines watching an event. */
export interface EventTriggers {
  fire(ctx: unknown, scope: Scope, event: string): Promise<number>;
}

/** The set of collaborators the concrete service is built from. */
export interface Deps {
  repository: Repository;
  cipher: TokenCipher | null;
  logger: Logger;
  clock: (() => Date) | null;
  limits: PlanLimits | null;
  auditor: Auditor | null;
  registry: Registry | null;
  validator: SchemaValidator | null;
  states: StateCodec | null;
  stateStore: StateStore | null;
  refs: RefCodec | null;
  queue: Queue | null;
  triggers: EventTriggers | null;
  limiter: CallLimiter | null;
  mcp: MCPRepository | null;
  mcpClients: MCPClientFactory | null;
}

/** The pieces of the service the OAuth, webhook and MCP files share. */
export type { EnqueueInput, MCPClientFactory, MCPRepository, MCPTool };

/** Tool labels, mirroring the task runtime's. */
export const LabelRead = "read";
export const LabelWriteInternal = "write_internal";
export const LabelWriteExternal = "write_external";

/** Reports whether a permission is one the schema stores. */
export function validPermission(permission: string): boolean {
  return permission === PermissionRead || permission === PermissionReadWrite;
}

/** Reports whether a status is one the schema stores. */
export function validStatus(status: string): boolean {
  return status === StatusConnected || status === StatusNeedsRelogin || status === StatusDisconnected;
}

/** True when an integration can be used right now. */
export function connected(integration: Integration): boolean {
  return integration.status === StatusConnected;
}

/** True when a person has to re-authorise. */
export function needsRelogin(integration: Integration): boolean {
  return integration.status === StatusNeedsRelogin;
}

/**
 * Reports whether a grant covers a tool of the given label.
 *
 * A read-only grant keeps the tools that cannot change anything outside Bolu —
 * `read` and `write_internal` both stay inside. Anything that reaches out is
 * dropped, which is the whole point of granting read-only.
 */
export function grantAllows(grant: Grant, label: string): boolean {
  if (grant.permission === PermissionReadWrite) {
    return true;
  }
  return label === LabelRead || label === LabelWriteInternal;
}

/** Refuses a link input the schema would reject. */
export function validateLinkInput(input: LinkInput): Error | null {
  if (input.app.trim() === "") {
    return new Error("integration: an app is required");
  }
  if (input.accountLabel.trim() === "") {
    return new Error("integration: an account label is required");
  }
  return null;
}

/** Refuses a connect input the schema or the flow would reject. */
export function validateConnectInput(input: ConnectInput): Error | null {
  if (input.app.trim() === "") {
    return new Error("integration: an app is required");
  }
  if (input.accessToken.trim() === "") {
    return new Error("integration: an access token is required");
  }
  if (input.accountLabel.trim() === "") {
    return new Error("integration: an account label is required");
  }
  return null;
}

/** Keeps enough of a secret to tell two apart while never showing one. */
export function redact(secret: string): string {
  if (secret === "") {
    return "(empty)";
  }
  if (secret.length <= 4) {
    return "(redacted)";
  }
  return `(redacted:${secret.slice(0, 2)}…${secret.slice(-2)})`;
}

/**
 * Renders a token without its secrets, as `Token.String` does in Go.
 *
 * Go's `%v` on a `[]string` prints `[a b]`, and the expiry is RFC3339 or
 * `(none)`; the rendering is kept so a log line reads the same after the port.
 */
export function tokenString(token: Token): string {
  return `integration.Token{access: ${redact(token.accessToken)}, refresh: ${redact(token.refreshToken)}, expires: ${expiryOf(token.expiresAt)}, scopes: [${token.scopes.join(" ")}]}`;
}

function expiryOf(at: Date | null): string {
  if (!at) {
    return "(none)";
  }
  return at.toISOString().replace(/\.\d{3}Z$/, "Z");
}

/** Reports whether the access token is past its expiry, with a margin. */
export function tokenExpired(token: Token, now: Date, marginMs: number): boolean {
  if (!token.expiresAt) {
    return false;
  }
  return token.expiresAt.getTime() <= now.getTime() + marginMs;
}

/** The zero uuid, as Go's `uuid.Nil`. */
export const NilUUID = "00000000-0000-0000-0000-000000000000";

/** True for the zero uuid, which is how "nobody" is spelled here. */
export function isNilId(id: string | null | undefined): boolean {
  return !id || id === NilUUID;
}

/**
 * An error carrying the sentinel a caller switches on, so `is` reproduces Go's
 * `errors.Is` for the messages `fmt.Errorf("%w: ...")` produced.
 */
export class WrappedError extends Error {
  constructor(
    readonly sentinel: Error,
    message: string,
  ) {
    super(message);
    this.name = "WrappedError";
  }
}

/** Wraps a sentinel with a detail, rendering the message as Go's `%w: detail` does. */
export function wrap(sentinel: Error, detail: string): WrappedError {
  return new WrappedError(sentinel, `${sentinel.message}: ${detail}`);
}

/** Wraps a sentinel around another error. */
export function wrapError(sentinel: Error, err: unknown): WrappedError {
  return new WrappedError(sentinel, `${sentinel.message}: ${errorMessage(err)}`);
}

/**
 * Gives an error the module's sentinel while keeping its own message.
 *
 * The platform codec reports a bad state with its own error type, exactly as
 * the Go codec reports the domain sentinel; this is where the two are made the
 * same, so a caller can switch on one value.
 */
export function adopt(sentinel: Error, err: unknown): WrappedError {
  if (is(err, sentinel)) {
    return err instanceof WrappedError ? err : new WrappedError(sentinel, errorMessage(err));
  }
  return new WrappedError(sentinel, errorMessage(err));
}

/** The message of an unknown thrown value. */
export function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/**
 * True when `err` is the sentinel or wraps it, matching Go's `errors.Is` over
 * the `%w` chains this module builds.
 */
export function is(err: unknown, sentinel: Error): boolean {
  let current: unknown = err;
  for (let guard = 0; guard < 32; guard++) {
    if (current === sentinel) {
      return true;
    }
    if (!(current instanceof Error)) {
      return false;
    }
    if (current instanceof WrappedError) {
      current = current.sentinel;
      continue;
    }
    current = "cause" in current ? current.cause : undefined;
  }
  return false;
}
