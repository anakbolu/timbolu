/**
 * The workspace and membership contracts: entities, roles, and the repository
 * and service interfaces.
 *
 * Ported 1:1 from internal/modules/workspace/domain/workspace.go.
 *
 * Errors are sentinels so the handler can reproduce Go's `errors.Is` mapping.
 * `ErrNotMember` is an AppError because the container's tenant guard branches
 * on `code: "not_a_member"` to answer 403.
 */

import { AppError } from "../../../platform/errors.ts";
import type { Scope } from "./ports.ts";

/**
 * Go keeps these in the same package, so every file may name them. Re-exporting
 * here keeps the same reach without a second import in each consumer.
 */
export type {
  AuditEntry,
  Auditor,
  Clock,
  Mailer,
  PlanLimits,
  Scope,
  Tokens,
} from "./ports.ts";
export { hashToken, isZeroScope } from "./ports.ts";

/** Roles inside a workspace. */
export const RoleOwner = "owner";
export const RoleAdmin = "admin";
export const RoleMember = "member";

/** The zero uuid, as Go's `uuid.Nil`. */
export const NilUUID = "00000000-0000-0000-0000-000000000000";

/** Errors returned by the service. */
export const ErrNotMember = new AppError(
  403,
  "workspace: you are not a member of this workspace",
  "not_a_member",
);
export const ErrForbidden = new Error("workspace: your role does not allow this action");
export const ErrLastOwner = new Error("workspace: a workspace must keep at least one owner");
export const ErrAlreadyMember = new Error("workspace: this user is already a member");
export const ErrInvitationInvalid = new Error("workspace: invitation is invalid, expired, or already accepted");
export const ErrInvitationEmailMatch = new Error("workspace: sign in with the invited email address to accept");
export const ErrWorkspaceNotFound = new Error("workspace: workspace not found");
/** The request itself is wrong (a blank name, an unknown timezone, a malformed address). */
export const ErrInvalidInput = new Error("workspace: invalid input");
export const ErrInvalidRole = new Error("workspace: unknown role");
export const ErrMemberNotFound = new Error("workspace: member not found");
/** The workspace's package does not allow another member. */
export const ErrPlanLimit = new Error("workspace: paket ini tidak mengizinkan anggota baru");
export const ErrCannotRemoveYourself = new Error("workspace: you cannot remove your own membership");
export const ErrInvitationAlreadySent = new Error("workspace: an invitation for this address is already pending");

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

/** Wraps a sentinel with a detail, rendering the message as Go's `%w: detail`. */
export function wrap(sentinel: Error, detail: string): WrappedError {
  return new WrappedError(sentinel, `${sentinel.message}: ${detail}`);
}

/** True when `err` is the sentinel or wraps it. */
export function is(err: unknown, sentinel: Error): boolean {
  let current: unknown = err;
  for (let guard = 0; guard < 32; guard++) {
    if (current === sentinel) return true;
    if (!(current instanceof Error)) return false;
    if (current instanceof WrappedError) {
      current = current.sentinel;
      continue;
    }
    current = "cause" in current ? current.cause : undefined;
  }
  return false;
}

/** The message of an unknown thrown value. */
export function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/** A tenant. `role` is the caller's role in it. */
export interface Workspace {
  id: string;
  name: string;
  businessField: string;
  timezone: string;
  language: string;
  /**
   * "HH:MM" in the workspace's own timezone. The approval gate holds an action
   * outside them.
   */
  workHoursStart: string;
  workHoursEnd: string;
  /**
   * The ISO weekdays the window applies to (1 = Monday). Null when the read
   * path does not select the column, which Go serialises as `null`.
   */
  workDays: number[] | null;
  role: string;
  createdAt: Date;
}

/** Groups the Bolu of one workspace. */
export interface Team {
  id: string;
  name: string;
  kind: string;
}

/** A workspace member with the account's address. */
export interface Member {
  userId: string;
  email: string;
  role: string;
  emailVerified: boolean;
  createdAt: Date;
}

/** A pending invitation to join a workspace. */
export interface Invitation {
  id: string;
  workspaceId: string;
  workspaceName: string;
  email: string;
  role: string;
  expiresAt: Date;
  createdAt: Date;
}

/** The onboarding form. */
export interface OnboardRequest {
  name: string;
  businessField: string;
  timezone: string;
}

/**
 * The outcome of onboarding. `created` is false when the caller already owned a
 * workspace, which makes a retried submit harmless.
 */
export interface OnboardResult {
  workspace: Workspace;
  teams: Team[];
  created: boolean;
}

/** Changes the workspace profile. */
export interface UpdateRequest {
  name: string;
  businessField: string;
  timezone: string;
  language: string;
  workHoursStart: string;
  workHoursEnd: string;
  workDays: number[];
}

/** An invitation to store. */
export interface NewInvitation {
  email: string;
  role: string;
  tokenHash: string;
  invitedBy: string;
  expiresAt: Date;
}

/** The persistence contract. */
export interface Repository {
  /** Creates the workspace, owner membership and default team in one transaction. */
  onboard(userId: string, req: OnboardRequest): Promise<OnboardResult>;
  workspaces(userId: string): Promise<Workspace[]>;
  workspace(scope: Scope): Promise<Workspace>;
  updateWorkspace(scope: Scope, req: UpdateRequest): Promise<Workspace>;
  teams(scope: Scope): Promise<Team[]>;
  membership(scope: Scope, userId: string): Promise<string>;
  members(scope: Scope): Promise<Member[]>;
  setRole(scope: Scope, userId: string, role: string): Promise<Member>;
  removeMember(scope: Scope, userId: string): Promise<void>;
  createInvitation(scope: Scope, invitation: NewInvitation): Promise<Invitation>;
  invitations(scope: Scope): Promise<Invitation[]>;
  revokeInvitation(scope: Scope, id: string): Promise<void>;
  acceptInvitation(tokenHash: string, userId: string, email: string): Promise<Workspace>;
}

/** The workspace use case contract. */
export interface Service {
  onboard(userId: string, req: OnboardRequest): Promise<OnboardResult>;
  workspaces(userId: string): Promise<Workspace[]>;
  current(scope: Scope): Promise<Workspace>;
  update(scope: Scope, actorRole: string, req: UpdateRequest): Promise<Workspace>;
  teams(scope: Scope): Promise<Team[]>;
  members(scope: Scope): Promise<Member[]>;
  setRole(scope: Scope, actorRole: string, userId: string, role: string): Promise<Member>;
  removeMember(scope: Scope, actorRole: string, actorId: string, userId: string): Promise<void>;
  invite(scope: Scope, actorRole: string, actorId: string, email: string, role: string): Promise<Invitation>;
  invitations(scope: Scope): Promise<Invitation[]>;
  revokeInvitation(scope: Scope, actorRole: string, id: string): Promise<void>;
  acceptInvitation(token: string, userId: string, email: string): Promise<Workspace>;
  resolve(workspaceId: string, userId: string): Promise<string>;
}

/** True for one of the three workspace roles. */
export function isValidRole(role: string): boolean {
  return role === RoleOwner || role === RoleAdmin || role === RoleMember;
}

/** True when a role may invite, re-role, or remove members. */
export function canManageMembers(role: string): boolean {
  return role === RoleOwner || role === RoleAdmin;
}

/** True when a role may change workspace settings and manage owners. */
export function canAdminister(role: string): boolean {
  return role === RoleOwner;
}
