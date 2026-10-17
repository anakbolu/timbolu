/**
 * The key that protects every workspace's data key.
 *
 * Ported from packages/platform/kms/kms.go. This is the outer half of the
 * envelope; the inner half is the data key that encrypts the tokens.
 *
 * A wrap records the id of the key that sealed it, so a value sealed under a
 * retired key still opens after a rotation.
 */

import { randomBytes } from "node:crypto";
import { Box, ErrInvalidCiphertext, KeySize } from "../crypto/secretbox";

/** The length of a workspace data key. */
export const DataKeySize = KeySize;

/** No key-management key is configured. */
export const ErrNoKey = new Error("kms: no key-management key configured");
/** A wrap names a key this deployment no longer holds. */
export const ErrUnknownKey = new Error("kms: the key that wrapped this value is not configured");

/** A data key sealed by a key-management key, with the id of that key. */
export interface Wrapped {
  keyId: string;
  bytes: Uint8Array;
}

/** Seals and opens data keys. */
export interface Manager {
  wrap(workspaceId: string, dataKey: Uint8Array): Wrapped;
  unwrap(workspaceId: string, wrapped: Wrapped): Uint8Array;
  activeKeyId(): string;
}

/** Wraps data keys with keys held in the process environment. */
export class Local implements Manager {
  private readonly active: Box;
  /** Keys a previous rotation used. They open, never seal. */
  private readonly retired: Map<string, Box>;
  private readonly activeID: string;

  private constructor(active: Box, activeId: string, retired: Map<string, Box>) {
    this.active = active;
    this.activeID = activeId;
    this.retired = retired;
  }

  /** Builds a manager from the active key and any retired ones. */
  static newLocal(activeId: string, activeKey: string, retired: Record<string, string>): Local {
    if (activeKey.trim() === "") {
      throw ErrNoKey;
    }

    let box: Box;
    try {
      box = Box.fromString(activeKey);
    } catch (err) {
      throw new Error(`kms: active key: ${(err as Error).message}`);
    }

    const keys = new Map<string, Box>();
    for (const [id, encoded] of Object.entries(retired)) {
      const trimmedId = id.trim();
      if (trimmedId === "") {
        throw new Error("kms: a retired key needs an id");
      }
      try {
        keys.set(trimmedId, Box.fromString(encoded));
      } catch (err) {
        throw new Error(`kms: retired key "${trimmedId}": ${(err as Error).message}`);
      }
    }

    const trimmedActiveId = activeId.trim();
    if (trimmedActiveId === "") {
      throw new Error("kms: the active key needs an id");
    }

    return new Local(box, trimmedActiveId, keys);
  }

  /**
   * Builds a manager from the configured key and retired list. A missing key is
   * not an error: a deployment without one runs with integrations disabled.
   */
  static newLocalFromConfig(masterKey: string, masterKeyId: string, retired: string): Local | null {
    if (masterKey.trim() === "") {
      return null;
    }

    const older = parseRetired(retired);

    let id = masterKeyId.trim();
    if (id === "") {
      // A single-key deployment needs no ceremony: the id only has to be
      // stable, and the key's fingerprint is stable without being secret.
      let box: Box;
      try {
        box = Box.fromString(masterKey);
      } catch (err) {
        throw new Error(`kms: master key: ${(err as Error).message}`);
      }
      id = `local-${box.fingerprint}`;
    }

    return Local.newLocal(id, masterKey, older);
  }

  /** Seals a data key under the active key. */
  wrap(workspaceId: string, dataKey: Uint8Array): Wrapped {
    if (dataKey.length !== DataKeySize) {
      throw new Error(`kms: data key must be ${DataKeySize} bytes (got ${dataKey.length})`);
    }

    let sealed: Uint8Array;
    try {
      sealed = this.active.encryptWith(Buffer.from(dataKey).toString("latin1"), associated(workspaceId));
    } catch (err) {
      throw new Error(`kms: wrap data key: ${(err as Error).message}`);
    }

    return { keyId: this.activeID, bytes: sealed };
  }

  /** Opens a data key. A wrap made by a retired key opens too. */
  unwrap(workspaceId: string, wrapped: Wrapped): Uint8Array {
    const box = wrapped.keyId === this.activeID ? this.active : this.retired.get(wrapped.keyId);
    if (!box) {
      throw new Error(`${ErrUnknownKey.message}: "${wrapped.keyId}"`);
    }

    let plaintext: string;
    try {
      plaintext = box.decryptWith(wrapped.bytes, associated(workspaceId));
    } catch (err) {
      throw new Error(`kms: unwrap data key: ${(err as Error).message}`);
    }

    // Go converts the decrypted bytes back to a byte slice. `latin1` is the
    // lossless byte-to-char round-trip used on the way in.
    const key = Buffer.from(plaintext, "latin1");
    if (key.length !== DataKeySize) {
      throw new Error(`kms: unwrap data key: ${ErrInvalidCiphertext.message}`);
    }
    return key;
  }

  /** The id a new wrap records. */
  activeKeyId(): string {
    return this.activeID;
  }

  /** The keys that only open, for diagnostics. Sorted for stable logs. */
  retiredIds(): string[] {
    return Array.from(this.retired.keys()).sort();
  }
}

/** Returns a fresh random data key. */
export function newDataKey(): Uint8Array {
  return randomBytes(DataKeySize);
}

/**
 * Reads the `id:key` pairs. A key that is itself base64 contains no colon, so
 * splitting on the first one is unambiguous.
 */
function parseRetired(encoded: string): Record<string, string> {
  const trimmed = encoded.trim();
  if (trimmed === "") {
    return {};
  }

  const keys: Record<string, string> = {};
  for (const pair of trimmed.split(",")) {
    const entry = pair.trim();
    if (entry === "") {
      continue;
    }

    const cut = entry.indexOf(":");
    if (cut < 0) {
      throw new Error(`kms: retired key "${entry}" must be \`id:key\``);
    }
    const id = entry.slice(0, cut);
    const key = entry.slice(cut + 1);
    if (id.trim() === "" || key.trim() === "") {
      throw new Error(`kms: retired key "${entry}" must be \`id:key\``);
    }
    keys[id.trim()] = key.trim();
  }

  return keys;
}

/** The data every wrap is bound to. Binding the workspace blocks row swaps. */
function associated(workspaceId: string): Uint8Array {
  return Buffer.from(`workspace-keys:${workspaceId}`, "utf8");
}
