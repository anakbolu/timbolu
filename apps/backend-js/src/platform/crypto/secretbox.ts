/**
 * Secret helpers used at rest.
 *
 * Ported from packages/platform/crypto/secretbox.go. AES-256-GCM with a random
 * 12-byte nonce prepended to the ciphertext (nonce || ciphertext || tag). This
 * is byte-compatible with Go's crypto/cipher GCM Seal output, so values written
 * by the Go deployment decrypt here and vice versa.
 */

import { createCipheriv, createDecipheriv, createHash, randomBytes } from "node:crypto";

/** KeySize is the AES-256 key length. */
export const KeySize = 32;

const NONCE_SIZE = 12;
const TAG_SIZE = 16;

/** No encryption key was configured. */
export const ErrNoKey = new Error("crypto: no encryption key configured");
/** The configured key is not 32 bytes. */
export const ErrInvalidKey = new Error("crypto: encryption key must be 32 bytes");
/** The stored value is not a value this box wrote. */
export const ErrInvalidCiphertext = new Error("crypto: ciphertext is invalid");

/** Encrypts secrets with AES-256-GCM. */
export class Box {
  private readonly key: Buffer;
  /** Identifies the key without revealing it. */
  readonly fingerprint: string;

  constructor(key: Uint8Array) {
    if (key.length !== KeySize) {
      throw new Error(`${ErrInvalidKey.message} (got ${key.length} bytes)`);
    }
    this.key = Buffer.from(key);
    this.fingerprint = createHash("sha256").update(this.key).digest().subarray(0, 4).toString("hex");
  }

  /** Builds a box from a configured key. */
  static fromString(encoded: string): Box {
    return new Box(parseKey(encoded));
  }

  /** Seals a secret. The result is the nonce followed by the ciphertext. */
  encrypt(plaintext: string): Uint8Array {
    return this.encryptWith(plaintext, null);
  }

  /** Seals a secret bound to additional authenticated data. */
  encryptWith(plaintext: string, associated: Uint8Array | null): Uint8Array {
    const nonce = randomBytes(NONCE_SIZE);
    const cipher = createCipheriv("aes-256-gcm", this.key, nonce);
    if (associated && associated.length > 0) {
      cipher.setAAD(Buffer.from(associated));
    }
    const sealed = Buffer.concat([cipher.update(Buffer.from(plaintext, "utf8")), cipher.final()]);
    return Buffer.concat([nonce, sealed, cipher.getAuthTag()]);
  }

  /** Opens a value this box wrote. */
  decrypt(ciphertext: Uint8Array): string {
    return this.decryptWith(ciphertext, null);
  }

  /** Opens a value sealed with encryptWith, given the same additional data. */
  decryptWith(ciphertext: Uint8Array, associated: Uint8Array | null): string {
    const raw = Buffer.from(ciphertext);
    if (raw.length < NONCE_SIZE + TAG_SIZE) {
      throw ErrInvalidCiphertext;
    }

    const nonce = raw.subarray(0, NONCE_SIZE);
    const sealed = raw.subarray(NONCE_SIZE, raw.length - TAG_SIZE);
    const tag = raw.subarray(raw.length - TAG_SIZE);

    try {
      const decipher = createDecipheriv("aes-256-gcm", this.key, nonce);
      decipher.setAuthTag(tag);
      if (associated && associated.length > 0) {
        decipher.setAAD(Buffer.from(associated));
      }
      return Buffer.concat([decipher.update(sealed), decipher.final()]).toString("utf8");
    } catch (err) {
      const detail = err instanceof Error ? err.message : String(err);
      throw new Error(`${ErrInvalidCiphertext.message} (key ${this.fingerprint}): ${detail}`);
    }
  }
}

/**
 * Decodes a configured key in any of the accepted forms: base64 (standard or
 * URL-safe, padded), raw standard base64 without padding, hex, or 32 raw bytes.
 */
export function parseKey(encoded: string): Buffer {
  const trimmed = encoded.trim();
  if (trimmed === "") {
    throw ErrNoKey;
  }

  const standard = decodeBase64(trimmed, false, true);
  if (standard && standard.length === KeySize) {
    return standard;
  }
  const raw = decodeBase64(trimmed, false, false);
  if (raw && raw.length === KeySize) {
    return raw;
  }
  const url = decodeBase64(trimmed, true, true);
  if (url && url.length === KeySize) {
    return url;
  }

  if (/^[0-9a-fA-F]+$/.test(trimmed) && trimmed.length === KeySize * 2) {
    const hex = Buffer.from(trimmed, "hex");
    if (hex.length === KeySize) {
      return hex;
    }
  }

  // A raw 32-byte secret, which is what a developer puts in .env.
  const bytes = Buffer.from(trimmed, "utf8");
  if (bytes.length === KeySize) {
    return bytes;
  }

  throw new Error(`${ErrInvalidKey.message} (configure ${KeySize} bytes, base64 or hex)`);
}

/** Strict base64 decode, rejecting characters the alphabet does not contain. */
function decodeBase64(value: string, urlSafe: boolean, padding: boolean): Buffer | null {
  const alphabet = urlSafe ? /^[A-Za-z0-9_-]*={0,2}$/ : /^[A-Za-z0-9+/]*={0,2}$/;
  if (!alphabet.test(value)) {
    return null;
  }

  const hasPadding = value.includes("=");
  if (padding) {
    if (!hasPadding && value.length % 4 !== 0) {
      return null;
    }
    if (hasPadding && value.length % 4 !== 0) {
      return null;
    }
  } else if (hasPadding) {
    return null;
  }

  if (!padding && value.length % 4 === 1) {
    return null;
  }

  try {
    const decoded = Buffer.from(value, urlSafe ? "base64url" : "base64");
    // Round-trip, so a value with stray padding is not accepted silently.
    const reencoded = decoded.toString(urlSafe ? "base64url" : "base64");
    const normalised = value.replace(/=+$/, "");
    if (reencoded.replace(/=+$/, "") !== normalised) {
      return null;
    }
    return decoded;
  } catch {
    return null;
  }
}
