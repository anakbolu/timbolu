/**
 * Authentication primitives: password hashing, one-time token generation, and
 * access-token issuing and verification.
 *
 * Ported from apps/backend/internal/platform/authn. Nothing here knows about
 * HTTP or the database.
 *
 * **Hashing is bcrypt for new passwords, with argon2id still verified.** The Go
 * deployment wrote argon2id, and 15 accounts in the live database hold one, so a
 * bcrypt-only verifier would lock every existing user out. `verifyPassword`
 * therefore dispatches on the stored format:
 *
 *   $argon2id$…  → @node-rs/argon2   (what Go wrote; Bun.password rejects its
 *                                     unpadded base64, so this library stays)
 *   $2a$/$2b$…   → Bun.password      (built in, no native module)
 *
 * New hashes use bcrypt, which is what `Bun.password` implements natively and
 * what the product asked for.
 */

import { createHash, randomBytes, timingSafeEqual } from "node:crypto";
import { hash as argonHash, verify as argonVerify } from "@node-rs/argon2";
import { SignJWT, jwtVerify } from "jose";

// Kept identical to the Go constants: an old hash carries its own parameters in
// the PHC string, but these are what a fresh argon2id hash would use.
export const ARGON_MEMORY_KIB = 64 * 1024;
export const ARGON_ITERATIONS = 3;
export const ARGON_PARALLELISM = 2;
export const ARGON_SALT_LENGTH = 16;
export const ARGON_KEY_LENGTH = 32;

/** bcrypt's cost factor. 12 is the current OWASP recommendation. */
export const BCRYPT_COST = 12;

/** The stored hash is not in a format this build can verify. */
export class InvalidHashError extends Error {
  constructor() {
    super("authn: password hash is not in a recognised format");
    this.name = "InvalidHashError";
  }
}

const PHC = /^\$argon2id\$v=19\$m=(\d+),t=(\d+),p=(\d+)\$[A-Za-z0-9+/]+\$[A-Za-z0-9+/]+$/;
const BCRYPT = /^\$2[abxy]?\$\d{2}\$[./A-Za-z0-9]{53}$/;

/** Derives a bcrypt hash. This is what a new password is stored as. */
export async function hashPassword(password: string): Promise<string> {
  if (!password) throw new Error("authn: password is required");
  return Bun.password.hash(password, { algorithm: "bcrypt", cost: BCRYPT_COST });
}

/**
 * Reports whether `password` matches the encoded hash, in constant time.
 *
 * The format decides the verifier, so a hash written by the Go deployment still
 * verifies after the switch to bcrypt.
 */
export async function verifyPassword(encoded: string, password: string): Promise<boolean> {
  if (!encoded) throw new InvalidHashError();

  if (PHC.test(encoded)) {
    try {
      return await argonVerify(encoded, password);
    } catch (err: any) {
      if (String(err?.code ?? "").includes("INVALID_ENCODING")) throw new InvalidHashError();
      return false;
    }
  }

  if (BCRYPT.test(encoded)) {
    try {
      return await Bun.password.verify(password, encoded);
    } catch {
      return false;
    }
  }

  throw new InvalidHashError();
}

/** True when the stored value is an argon2id PHC string. */
export function isArgon2Hash(encoded: string | null | undefined): boolean {
  return !!encoded && PHC.test(encoded);
}

/** True when the stored value is a bcrypt hash. */
export function isBcryptHash(encoded: string | null | undefined): boolean {
  return !!encoded && BCRYPT.test(encoded);
}

/**
 * True when a stored hash should be replaced on the next successful sign-in:
 * an argon2id hash is still valid but is no longer what this build writes.
 */
export function needsRehash(encoded: string | null | undefined): boolean {
  return isArgon2Hash(encoded);
}

/** Returns a fresh random token and the hash to store. */
export function generateToken(): { raw: string; hash: string } {
  const buf = randomBytes(32);
  const raw = buf.toString("base64url");
  return { raw, hash: hashToken(raw) };
}

/**
 * Hashes a token for lookup. SHA-256 is enough because the token is
 * high-entropy random, unlike a password.
 */
export function hashToken(raw: string): string {
  return createHash("sha256").update(raw, "utf8").digest("hex");
}

/** Constant-time string comparison. */
export function safeEqual(a: string, b: string): boolean {
  const ba = Buffer.from(a, "utf8");
  const bb = Buffer.from(b, "utf8");
  if (ba.length !== bb.length) return false;
  return timingSafeEqual(ba, bb);
}

/** What an access token asserts. */
export interface Claims {
  userId: string;
  sessionId: string;
  expiresAt: Date;
  issuedAt: Date;
}

/**
 * Signs and verifies access tokens with a shared secret (HS256). The claims
 * match the Go issuer: `sub` is the user id, `jti` is the session id, so
 * signing out or resetting a password invalidates the token immediately.
 */
export class Issuer {
  private readonly key: Uint8Array;
  private readonly ttlSeconds: number;

  constructor(secret: string, ttlSeconds: number) {
    if (!secret) throw new Error("authn: jwt secret is required");
    if (!(ttlSeconds > 0)) throw new Error("authn: access token ttl must be positive");
    this.key = new TextEncoder().encode(secret);
    this.ttlSeconds = ttlSeconds;
  }

  /** How long a newly issued token lives. */
  get ttl(): number {
    return this.ttlSeconds;
  }

  /** Signs an access token for the user and their current session. */
  async issue(userId: string, sessionId: string): Promise<{ token: string; expiresAt: Date }> {
    if (!userId) throw new Error("authn: user id is required");
    const now = Math.floor(Date.now() / 1000);
    const exp = now + this.ttlSeconds;

    const token = await new SignJWT({})
      .setProtectedHeader({ alg: "HS256", typ: "JWT" })
      .setSubject(userId)
      .setJti(sessionId)
      .setIssuedAt(now)
      .setExpirationTime(exp)
      .sign(this.key);

    return { token, expiresAt: new Date(exp * 1000) };
  }

  /** Checks the signature and the expiry and returns the claims. */
  async verify(token: string): Promise<Claims> {
    if (!token) throw new Error("authn: access token is required");

    const { payload } = await jwtVerify(token, this.key, { algorithms: ["HS256"] });

    const userId = typeof payload.sub === "string" ? payload.sub : "";
    const sessionId = typeof payload.jti === "string" ? payload.jti : "";
    if (!userId) throw new Error("authn: access token subject is not a user id");
    if (!sessionId) throw new Error("authn: access token has no session id");

    return {
      userId,
      sessionId,
      expiresAt: new Date((payload.exp ?? 0) * 1000),
      issuedAt: new Date((payload.iat ?? 0) * 1000),
    };
  }
}
