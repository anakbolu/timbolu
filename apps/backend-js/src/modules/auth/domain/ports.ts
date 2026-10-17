/**
 * The ports the auth service depends on. They are declared here, in the domain,
 * and implemented by the platform packages; the container is the only place
 * that knows which implementation is wired in.
 *
 * Ported 1:1 from internal/modules/auth/domain/ports.go.
 */

import { hashToken } from "../../../platform/authn/index.ts";

/** What an access token asserts. */
export interface AccessClaims {
  userId: string;
  sessionId: string;
}

/** Signs and verifies access tokens. */
export interface TokenIssuer {
  issue(userID: string, sessionID: string): Promise<{ token: string; expiresAt: Date }>;
  verify(token: string): Promise<AccessClaims>;
}

/** Signs and verifies the OAuth state value. */
export interface StateSigner {
  sign(): Promise<string>;
  verify(state: string): Promise<void>;
}

/** Delivers a transactional email. */
export interface Mailer {
  send(to: string, subject: string, body: string): Promise<void>;
}

/** The identity Google asserts for a signed-in user. */
export interface GoogleUser {
  subject: string;
  email: string;
  emailVerified: boolean;
}

/** The Google code-flow client. */
export interface GoogleProvider {
  /** Builds the consent URL for a state value. */
  authCodeURL(state: string): string;
  /** Trades the authorization code for the user's identity. */
  exchange(code: string): Promise<GoogleUser>;
}

/** The outcome of one throttled attempt. */
export interface ThrottleResult {
  allowed: boolean;
  /** Milliseconds until the next token, mirroring Go's `time.Duration`. */
  retryAfter: number;
}

/** Limits repeated login attempts per key. */
export interface LoginThrottle {
  allow(key: string, capacity: number, refill: number): Promise<ThrottleResult>;
  reset(key: string): Promise<void>;
}

/**
 * Hashes and verifies account passwords, and enforces the password policy. The
 * argon2id implementation lives in the platform layer.
 */
export interface Passwords {
  hash(password: string): Promise<string>;
  verify(hash: string, password: string): Promise<boolean>;
  validate(password: string): Promise<void>;
}

/** Generates one-time tokens and the hash to store. */
export interface Tokens {
  generate(): { raw: string; hash: string };
}

/**
 * Hashes a one-time token for storage. The token is high-entropy random, so
 * SHA-256 is enough; only the hash is persisted. Re-exported from the platform
 * authn package rather than re-implemented.
 */
export { hashToken };
