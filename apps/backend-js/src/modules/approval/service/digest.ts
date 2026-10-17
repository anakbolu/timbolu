/**
 * The short digest that makes an idempotency key stable.
 *
 * Ported 1:1 from internal/modules/approval/service/digest.go. It is a digest
 * rather than the payload itself because the key is a database column with a
 * unique index: two proposals that differ in one character are different
 * proposals, and the digest keeps that distinction without storing the action's
 * arguments twice.
 */

import { createHash } from "node:crypto";

/** The first 8 bytes of SHA-256, hex encoded, exactly as Go's `hex.EncodeToString(sum[:8])`. */
export function hashPayload(payload: string): string {
  return createHash("sha256").update(payload, "utf8").digest("hex").slice(0, 16);
}
