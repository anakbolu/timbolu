/**
 * The OAuth half of the flow: starting a connection, finishing it, and keeping
 * a token alive.
 *
 * Ported 1:1 from internal/modules/integration/service/oauth.go.
 */

import { randomBytes } from "node:crypto";
import {
  ErrExchangeFailed,
  ErrInvalidState,
  ErrRefreshFailed,
  OAuthRedirectURI,
  stateExpired,
  type AccountNamer,
  type AuthURL,
  type CompleteAuthRequest,
  type OAuthAdapter,
  type OAuthState,
  type OAuthTokens,
} from "../domain/oauth.ts";
import { ErrUnknownAdapter } from "../domain/adapter.ts";
import {
  ErrInvalidInput,
  adopt,
  connected,
  errorMessage,
  is,
  wrap,
  type Integration,
  type Scope,
} from "../domain/integration.ts";
import type { IntegrationService } from "./service.ts";

/** How long a person has to finish a provider's consent screen. */
export const StateTTL = 10 * 60 * 1000;

/**
 * Starts a connection flow.
 *
 * The state it returns is signed and single-use. Signed so the callback knows
 * which workspace and which person it belongs to without a session; single-use
 * so a replayed callback cannot store a second credential.
 */
export async function beginAuth(
  service: IntegrationService,
  ctx: unknown,
  scope: Scope,
  app: string,
  redirectUri: string,
  returnTo: string,
): Promise<AuthURL> {
  const trimmed = app.trim();
  if (trimmed === "") {
    throw wrap(ErrInvalidInput, "an app is required");
  }
  // A redirect the provider cannot resolve is a flow that sends the browser
  // somewhere that is not this deployment.
  const redirect = redirectUri.trim();
  if (redirect === "" || !redirect.includes("://")) {
    throw wrap(ErrInvalidInput, "the redirect URI must be absolute");
  }
  if (isNil(scope.workspaceId) || isNil(scope.userId)) {
    throw wrap(ErrInvalidInput, "a workspace and a person are required");
  }
  if (!service.deps.states || !service.deps.stateStore) {
    throw wrap(ErrInvalidInput, "no OAuth state codec is configured");
  }

  const adapter = service.oauthAdapter(trimmed);
  if (!adapter) {
    throw new Error(`${ErrUnknownAdapter.message}: ${JSON.stringify(trimmed)}`);
  }

  const state: OAuthState = {
    workspaceId: scope.workspaceId,
    userId: scope.userId ?? NIL,
    app: trimmed,
    nonce: newNonce(),
    returnTo: safeReturnTo(returnTo),
    expiresAt: new Date(service.now().getTime() + StateTTL),
  };

  // Stored before the value is handed out, so a callback that arrives
  // immediately still finds the state it is about to spend.
  await service.deps.stateStore.saveState(state);

  let signed: string;
  try {
    signed = await service.deps.states.sign(state);
  } catch (err) {
    throw new Error(`integration: sign state: ${errorMessage(err)}`);
  }

  return { url: adapter.authCodeUrl(signed, redirect), state: signed };
}

/**
 * Finishes a flow.
 *
 * The order matters and is the whole security story: verify the signature to
 * learn the tenant, spend the nonce so a replay cannot also succeed, and only
 * then talk to the provider.
 */
export async function completeAuth(
  service: IntegrationService,
  ctx: unknown,
  req: CompleteAuthRequest,
): Promise<Integration> {
  if (!service.deps.states || !service.deps.stateStore) {
    throw wrap(ErrInvalidInput, "no OAuth state codec is configured");
  }
  if (req.state.trim() === "") {
    throw wrap(ErrInvalidState, "no state");
  }
  if (req.code.trim() === "") {
    throw wrap(ErrInvalidState, "no authorization code");
  }

  let state: OAuthState;
  try {
    state = await service.deps.states.verify(req.state);
  } catch (err) {
    // The platform codec reports its own type; the handler switches on the
    // module's sentinel.
    throw adopt(ErrInvalidState, err);
  }
  if (stateExpired(state, service.now())) {
    throw wrap(ErrInvalidState, "it has expired");
  }

  const adapter = service.oauthAdapter(state.app);
  if (!adapter) {
    throw new Error(`${ErrUnknownAdapter.message}: ${JSON.stringify(state.app)}`);
  }

  await service.deps.stateStore.consumeState(state.workspaceId, state.nonce, service.now());

  const redirect = req.redirectUri.trim() === "" ? OAuthRedirectURI : req.redirectUri;

  let tokens: OAuthTokens;
  try {
    tokens = await adapter.exchange(ctx, req.code, redirect);
  } catch (err) {
    throw wrap(ErrExchangeFailed, errorMessage(err));
  }
  if (tokens.accessToken.trim() === "") {
    throw wrap(ErrExchangeFailed, "the provider returned no access token");
  }

  const scope: Scope = {
    userId: state.userId,
    workspaceId: state.workspaceId,
    agentId: NIL,
    taskId: NIL,
  };

  const account = await service.accountLabel(ctx, adapter, tokens);

  return service.connect(ctx, scope, {
    app: state.app,
    accountLabel: account,
    scopes: tokens.scopes,
    accessToken: tokens.accessToken,
    refreshToken: tokens.refreshToken,
    expiresAt: tokens.expiresAt,
  });
}

/**
 * Trades a stored refresh token for a new access token.
 *
 * A provider that refuses is not an error the caller has to handle: the
 * connection is marked as needing a fresh login and the returned integration
 * says so.
 */
export async function refreshConnection(
  service: IntegrationService,
  ctx: unknown,
  scope: Scope,
  id: string,
): Promise<Integration> {
  const integration = await service.deps.repository.get(scope, id);

  const adapter = service.oauthAdapter(integration.app);
  if (!adapter) {
    throw new Error(`${ErrUnknownAdapter.message}: ${JSON.stringify(integration.app)}`);
  }

  const token = await service.token(ctx, scope, id);
  if (token.refreshToken.trim() === "") {
    // Nothing to trade.
    return service.needsRelogin(ctx, scope, id, "penyedia tidak memberi refresh token");
  }

  let refreshed: OAuthTokens;
  try {
    refreshed = await adapter.refresh(ctx, token.refreshToken);
  } catch (err) {
    service.deps.logger.warn(
      { app: integration.app, account: integration.accountLabel, err },
      "refreshing an integration failed",
    );
    return service.needsRelogin(ctx, scope, id, refreshReason(err));
  }

  // A provider that rotates the refresh token sends a new one; one that does
  // not sends an empty field, and the stored value stays.
  const nextRefresh = refreshed.refreshToken.trim() === "" ? token.refreshToken : refreshed.refreshToken;

  return service.refresh(ctx, scope, id, {
    accessToken: refreshed.accessToken,
    refreshToken: nextRefresh,
    expiresAt: refreshed.expiresAt,
    scopes: refreshed.scopes,
  });
}

/**
 * Refreshes every connection whose token is close to expiring.
 *
 * One connection failing does not stop the others: each is independent, and the
 * failure is already recorded on the row.
 */
export async function refreshExpiring(
  service: IntegrationService,
  ctx: unknown,
  scope: Scope,
): Promise<Integration[]> {
  const connections = await service.deps.repository.list(scope);

  const now = service.now();
  const out: Integration[] = [];

  for (const connection of connections) {
    if (!connected(connection)) {
      continue;
    }
    if (!expiresSoon(connection.expiresAt, now, REFRESH_MARGIN)) {
      continue;
    }

    try {
      out.push(await refreshConnection(service, ctx, scope, connection.id));
    } catch (err) {
      // A single connection that cannot be refreshed is already marked as
      // needing a login. The rest still get their turn.
      service.deps.logger.warn({ integration: connection.id, err }, "refreshing one connection failed");
    }
  }

  return out;
}

/**
 * Names the account a flow connected.
 *
 * The adapter decides, because only it knows where the provider states the
 * address. A provider that does not is named after the app and the moment.
 */
export async function accountLabelFor(
  service: IntegrationService,
  ctx: unknown,
  adapter: OAuthAdapter,
  tokens: OAuthTokens,
): Promise<string> {
  const namer = adapter as Partial<AccountNamer>;
  if (typeof namer.accountLabel === "function") {
    let label: string;
    try {
      label = await namer.accountLabel(ctx, tokens);
    } catch (err) {
      throw new Error(`integration: read the account label: ${errorMessage(err)}`);
    }
    if (label.trim() !== "") {
      return label.trim();
    }
  }

  // A fallback that is still unique per app, so two connections of the same app
  // cannot collide on the upsert key and overwrite each other.
  return `${adapter.app()}@${Math.floor(service.now().getTime() / 1000)}`;
}

/** Reports whether an expiry is within the margin. */
function expiresSoon(at: Date | null, now: Date, margin: number): boolean {
  if (!at) {
    return false;
  }
  return at.getTime() <= now.getTime() + margin;
}

/** Turns a provider's refusal into something a person can read. */
function refreshReason(err: unknown): string {
  if (is(err, ErrRefreshFailed)) {
    return "penyedia menolak refresh token; sambungkan ulang";
  }
  return "penyegaran token gagal; sambungkan ulang";
}

/** The unguessable part of a state value. */
function newNonce(): string {
  return randomBytes(32).toString("base64url");
}

/**
 * Keeps the callback from being usable as an open redirect. Only a path within
 * the app is allowed.
 */
function safeReturnTo(returnTo: string): string {
  const trimmed = returnTo.trim();
  if (trimmed === "") {
    return "/dashboard";
  }
  if (trimmed.startsWith("/") && !trimmed.startsWith("//")) {
    return trimmed;
  }
  return "/dashboard";
}

const NIL = "00000000-0000-0000-0000-000000000000";
const REFRESH_MARGIN = 2 * 60 * 1000;

/** True for the zero uuid. */
function isNil(id: string | null | undefined): boolean {
  return !id || id === NIL;
}

