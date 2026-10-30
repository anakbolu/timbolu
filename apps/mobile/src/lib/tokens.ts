/**
 * Where the session's two halves live.
 *
 * The access token stays in memory only: it is short-lived, and a copy on disk
 * would outlive the session it belongs to. The refresh token is the opposite —
 * it has to survive the app being killed, or every cold start becomes a sign-in
 * — so it goes to the platform keystore through `expo-secure-store`.
 */

import * as SecureStore from "expo-secure-store";

const REFRESH_TOKEN_KEY = "bolu.refresh_token";

/**
 * How long before its expiry an access token is treated as spent. A call sent
 * just inside the last moment arrives after it, so the window is the cost of a
 * round trip rather than zero.
 */
const REFRESH_SKEW_MS = 30_000;

let accessToken: string | null = null;
let accessTokenExpiresAt: number | null = null;

export function getAccessToken(): string | null {
  return accessToken;
}

/**
 * `expiresAt` is the `access_expires_at` the session carried. An unparsable
 * date is treated as unknown rather than spent: the API stays the authority, and
 * a broken clock should not turn into a refresh on every request.
 */
export function setAccessToken(token: string | null, expiresAt?: string | null): void {
  accessToken = token;
  accessTokenExpiresAt = token && expiresAt ? Date.parse(expiresAt) : null;
  if (accessTokenExpiresAt !== null && Number.isNaN(accessTokenExpiresAt)) {
    accessTokenExpiresAt = null;
  }
}

/** Whether a call sent now would arrive after the token's expiry. */
export function tokenIsSpent(): boolean {
  if (!accessToken) return true;
  if (accessTokenExpiresAt === null) return false;
  return Date.now() >= accessTokenExpiresAt - REFRESH_SKEW_MS;
}

/**
 * A keystore that refuses to answer is read as "no token". That is the safe
 * direction: the app asks for a sign-in instead of retrying a broken read on
 * every screen.
 */
export async function readRefreshToken(): Promise<string | null> {
  try {
    return await SecureStore.getItemAsync(REFRESH_TOKEN_KEY);
  } catch {
    return null;
  }
}

export async function saveRefreshToken(token: string): Promise<void> {
  try {
    await SecureStore.setItemAsync(REFRESH_TOKEN_KEY, token);
  } catch {
    // A keystore write that fails costs the next cold start a sign-in, not this
    // session: the access token in memory already carries it.
  }
}

export async function clearRefreshToken(): Promise<void> {
  try {
    await SecureStore.deleteItemAsync(REFRESH_TOKEN_KEY);
  } catch {
    // Nothing left to do; the in-memory token is cleared by the caller.
  }
}
