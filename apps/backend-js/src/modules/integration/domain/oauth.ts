/**
 * The OAuth half of the adapter contract. The flow itself belongs to the
 * integration module — issuing and consuming a single-use state, storing what
 * the provider returned, and marking a connection that stopped working — while
 * the provider's endpoints belong to the adapter that speaks it.
 *
 * Ported 1:1 from internal/modules/integration/domain/oauth.go.
 */

import type { Adapter } from "./adapter.ts";

/** The state the provider sent back is not one this deployment issued, or was already spent, or has expired. */
export const ErrInvalidState = new Error("integration: the authorization state is not valid");
/** The provider refused the code. */
export const ErrExchangeFailed = new Error("integration: the provider refused the authorization code");
/** An application that is not connected through OAuth was asked to start an authorization flow. */
export const ErrNoOAuth = new Error("integration: this application is not connected through OAuth");
/** The provider refused the refresh token. */
export const ErrRefreshFailed = new Error("integration: the provider refused the refresh token");

/** What a provider returns from an exchange or a refresh. */
export interface OAuthTokens {
  accessToken: string;
  refreshToken: string;
  expiresAt: Date | null;
  scopes: string[];
}

/** An adapter whose application is connected through OAuth. */
export interface OAuthAdapter extends Adapter {
  /** The provider's consent URL for a state and a redirect. */
  authCodeUrl(state: string, redirectUri: string): string;
  /** Trades a code for tokens. */
  exchange(ctx: unknown, code: string, redirectUri: string): Promise<OAuthTokens>;
  /** Trades a refresh token for a new access token. */
  refresh(ctx: unknown, refreshToken: string): Promise<OAuthTokens>;
  /** The narrowest scope set that makes the adapter's read tools work. */
  defaultScopes(): string[];
}

/**
 * An OAuth adapter that can name the account a flow connected. Declared
 * separately because Go reaches it with an anonymous interface assertion.
 */
export interface AccountNamer {
  accountLabel(ctx: unknown, tokens: OAuthTokens): Promise<string>;
}

/** The payload a state value carries. */
export interface OAuthState {
  workspaceId: string;
  userId: string;
  app: string;
  nonce: string;
  returnTo: string;
  expiresAt: Date;
}

/** Signs and verifies a state payload. */
export interface StateCodec {
  sign(state: OAuthState): Promise<string>;
  verify(value: string): Promise<OAuthState>;
}

/** Remembers which nonces are still unspent. */
export interface StateStore {
  saveState(state: OAuthState): Promise<void>;
  consumeState(workspaceId: string, nonce: string, now: Date): Promise<void>;
}

/** Where a connection flow starts. */
export interface AuthURL {
  url: string;
  state: string;
}

/** A provider's callback. */
export interface CompleteAuthRequest {
  code: string;
  state: string;
  redirectUri: string;
}

/** The path this deployment registers with providers. */
export const OAuthRedirectURI = "/api/integrations/oauth/callback";

/** One thing an application reported. */
export interface TriggerEvent {
  app: string;
  event: string;
  integrationId: string;
  /** The provider's own id for this delivery. */
  eventId: string;
  /** The provider's own body, as raw JSON text. */
  payload: string;
}

/** Reports whether the state is past its life. `!ExpiresAt.After(now)`. */
export function stateExpired(state: OAuthState, now: Date): boolean {
  return state.expiresAt.getTime() <= now.getTime();
}
