/**
 * The workspace module's ports: the identity a query runs as, and the small
 * cross-module seams the service depends on.
 *
 * Ported 1:1 from internal/modules/workspace/domain/ports.go. The type lives in
 * the domain so the service can pass it through without importing the database
 * package; the repository translates it.
 */

import { createHash } from "node:crypto";

/** The identity a query runs as. RLS reads both parts. */
export interface Scope {
  userId: string;
  workspaceId: string;
}

/** The zero uuid, as Go's `uuid.Nil`. */
const NilUUID = "00000000-0000-0000-0000-000000000000";

/** True when the scope carries no identity at all. */
export function isZeroScope(scope: Scope | null | undefined): boolean {
  if (!scope) return true;
  return (!scope.userId || scope.userId === NilUUID) && (!scope.workspaceId || scope.workspaceId === NilUUID);
}

/** Delivers a transactional email. */
export interface Mailer {
  send(ctx: unknown, to: string, subject: string, body: string): Promise<void>;
}

/**
 * Hashes an invitation token for storage. Only the hash is persisted, so a
 * database leak does not hand over usable invitations.
 */
export function hashToken(raw: string): string {
  return createHash("sha256").update(raw, "utf8").digest("hex");
}

/** Generates one-time tokens and the hash to store. */
export interface Tokens {
  generate(): { raw: string; hash: string };
}

/** One thing that happened, for the record. */
export interface AuditEntry {
  action: string;
  objectType: string;
  objectId: string;
  actorUserId: string;
  metadata?: Record<string, unknown>;
}

/** Records what a person did, for the trail the product answers "who changed this" with. */
export interface Auditor {
  record(ctx: unknown, scope: Scope, entry: AuditEntry): Promise<void>;
}

/** Reports how many members a workspace's package allows. */
export interface PlanLimits {
  maxMembers(ctx: unknown, workspaceId: string): Promise<number>;
}

/** Reports the current time, so expiry rules can be tested. */
export type Clock = () => Date;
