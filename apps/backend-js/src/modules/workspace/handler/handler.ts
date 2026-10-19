/**
 * Exposes the workspace module over HTTP. It talks to the domain service
 * interface only.
 *
 * Ported 1:1 from internal/modules/workspace/handler/handler.go.
 */

import type { Hono } from "hono";
import {
  currentEmail,
  currentUserId,
  isUuid,
  role as callerRole,
  scope as requestScope,
  type AppContext,
  type AppEnv,
} from "../../../platform/context.ts";
import type { Logger } from "../../../platform/logger.ts";
import { created, error, ok } from "../../../platform/response.ts";
import {
  ErrCannotRemoveYourself,
  ErrForbidden,
  ErrInvitationAlreadySent,
  ErrInvitationEmailMatch,
  ErrInvitationInvalid,
  ErrInvalidInput,
  ErrInvalidRole,
  ErrLastOwner,
  ErrMemberNotFound,
  ErrNotMember,
  ErrPlanLimit,
  ErrWorkspaceNotFound,
  NilUUID,
  errorMessage,
  is,
  type Member,
  type OnboardResult,
  type Scope,
  type Service,
  type Team,
  type Workspace,
} from "../domain/workspace.ts";

interface onboardRequest {
  name: string;
  business_field: string;
  timezone: string;
}

interface updateWorkspaceRequest {
  name: string;
  business_field: string;
  timezone: string;
  language: string;
  work_hours_start: string;
  work_hours_end: string;
  work_days: number[];
}

interface setRoleRequest {
  role: string;
}

interface inviteRequest {
  email: string;
  role: string;
}

interface acceptInvitationRequest {
  token: string;
}

interface workspacePayload {
  id: string;
  name: string;
  business_field: string;
  timezone: string;
  language: string;
  role?: string;
  work_hours_start: string;
  work_hours_end: string;
  work_days: number[] | null;
  created_at: string;
}

interface teamPayload {
  id: string;
  name: string;
  kind: string;
}

interface memberPayload {
  user_id: string;
  email: string;
  role: string;
  email_verified: boolean;
  joined_at: string;
}

interface invitationPayload {
  id: string;
  email: string;
  role: string;
  expires_at: string;
  invited_at: string;
  workspace_id?: string;
}

interface onboardPayload {
  workspace: workspacePayload;
  teams: teamPayload[];
  created: boolean;
}

/** Runs when an update moved the workspace's timezone or working window. */
export type ClockChangeCallback = (workspaceId: string) => void | Promise<void>;

/** Serves the workspace endpoints. */
export class Handler {
  private afterClockChange: ClockChangeCallback | null = null;

  constructor(
    private readonly service: Service,
    private readonly log: Logger,
  ) {}

  /** Installs the callback that runs after the workspace's clock moves. */
  withClockChange(fn: ClockChangeCallback): this {
    this.afterClockChange = fn;
    return this;
  }

  /** Creates the caller's workspace with its owner membership and default team. */
  onboard = async (c: AppContext): Promise<Response> => {
    const body = await readBody<onboardRequest>(c);
    if (!body) return error(c, 400, "invalid request body", "invalid_request");

    try {
      const result = await this.service.onboard(currentUserId(c) ?? NilUUID, {
        name: body.name ?? "",
        businessField: body.business_field ?? "",
        timezone: body.timezone ?? "",
      });
      return ok(c, "workspace ready", toOnboardPayload(result));
    } catch (err) {
      return this.fail(c, "onboard", err);
    }
  };

  /** Every workspace the caller belongs to. */
  list = async (c: AppContext): Promise<Response> => {
    try {
      const workspaces = await this.service.workspaces(currentUserId(c) ?? NilUUID);
      return ok(c, "ok", workspaces.map(toWorkspacePayload));
    } catch (err) {
      return this.fail(c, "list workspaces", err);
    }
  };

  /** The active workspace. */
  current = async (c: AppContext): Promise<Response> => {
    try {
      const workspace = await this.service.current(scope(c));
      workspace.role = callerRole(c);
      return ok(c, "ok", toWorkspacePayload(workspace));
    } catch (err) {
      return this.fail(c, "current workspace", err);
    }
  };

  /** Changes the workspace profile. Owner only. */
  update = async (c: AppContext): Promise<Response> => {
    const body = await readBody<updateWorkspaceRequest>(c);
    if (!body) return error(c, 400, "invalid request body", "invalid_request");

    // The window in force before the write, so a rename does not reschedule
    // every routine of the workspace.
    let before: Workspace | null = null;
    try {
      before = await this.service.current(scope(c));
    } catch {
      before = null;
    }

    try {
      const workspace = await this.service.update(scope(c), callerRole(c), {
        name: body.name ?? "",
        businessField: body.business_field ?? "",
        timezone: body.timezone ?? "",
        language: body.language ?? "",
        workHoursStart: body.work_hours_start ?? "",
        workHoursEnd: body.work_hours_end ?? "",
        workDays: body.work_days ?? [],
      });
      workspace.role = callerRole(c);

      if (before && this.afterClockChange && clockMoved(before, workspace)) {
        await this.afterClockChange(workspace.id);
      }

      return ok(c, "workspace updated", toWorkspacePayload(workspace));
    } catch (err) {
      return this.fail(c, "update workspace", err);
    }
  };

  /** The teams of the active workspace. */
  teams = async (c: AppContext): Promise<Response> => {
    try {
      const teams = await this.service.teams(scope(c));
      return ok(c, "ok", teams.map(toTeamPayload));
    } catch (err) {
      return this.fail(c, "list teams", err);
    }
  };

  /** The members of the active workspace. */
  members = async (c: AppContext): Promise<Response> => {
    try {
      const members = await this.service.members(scope(c));
      return ok(c, "ok", members.map(toMemberPayload));
    } catch (err) {
      return this.fail(c, "list members", err);
    }
  };

  /** Changes a member's role. */
  setRole = async (c: AppContext): Promise<Response> => {
    const userId = c.req.param("userID") ?? "";
    if (!isUuid(userId)) {
      return error(c, 400, "invalid user id", "invalid_request");
    }

    const body = await readBody<setRoleRequest>(c);
    if (!body) return error(c, 400, "invalid request body", "invalid_request");

    try {
      const member = await this.service.setRole(scope(c), callerRole(c), userId, body.role ?? "");
      return ok(c, "role updated", toMemberPayload(member));
    } catch (err) {
      return this.fail(c, "set role", err);
    }
  };

  /** Removes a member from the active workspace. */
  removeMember = async (c: AppContext): Promise<Response> => {
    const userId = c.req.param("userID") ?? "";
    if (!isUuid(userId)) {
      return error(c, 400, "invalid user id", "invalid_request");
    }

    try {
      await this.service.removeMember(scope(c), callerRole(c), currentUserId(c) ?? NilUUID, userId);
      return ok(c, "member removed");
    } catch (err) {
      return this.fail(c, "remove member", err);
    }
  };

  /** The pending invitations. */
  invitations = async (c: AppContext): Promise<Response> => {
    try {
      const invitations = await this.service.invitations(scope(c));
      return ok(
        c,
        "ok",
        invitations.map(
          (invitation): invitationPayload => ({
            id: invitation.id,
            email: invitation.email,
            role: invitation.role,
            expires_at: invitation.expiresAt.toISOString(),
            invited_at: invitation.createdAt.toISOString(),
          }),
        ),
      );
    } catch (err) {
      return this.fail(c, "list invitations", err);
    }
  };

  /** Sends an invitation to join the active workspace. */
  invite = async (c: AppContext): Promise<Response> => {
    const body = await readBody<inviteRequest>(c);
    if (!body) return error(c, 400, "invalid request body", "invalid_request");

    try {
      const invitation = await this.service.invite(
        scope(c),
        callerRole(c),
        currentUserId(c) ?? NilUUID,
        body.email ?? "",
        body.role ?? "",
      );
      return created(c, "invitation sent", {
        id: invitation.id,
        email: invitation.email,
        role: invitation.role,
        expires_at: invitation.expiresAt.toISOString(),
        invited_at: invitation.createdAt.toISOString(),
        workspace_id: invitation.workspaceId,
      } satisfies invitationPayload);
    } catch (err) {
      return this.fail(c, "invite", err);
    }
  };

  /** Deletes a pending invitation. */
  revokeInvitation = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("invitationID") ?? "";
    if (!isUuid(id)) {
      return error(c, 400, "invalid invitation id", "invalid_request");
    }

    try {
      await this.service.revokeInvitation(scope(c), callerRole(c), id);
      return ok(c, "invitation revoked");
    } catch (err) {
      return this.fail(c, "revoke invitation", err);
    }
  };

  /** Joins the caller to the workspace they were invited to. */
  acceptInvitation = async (c: AppContext): Promise<Response> => {
    const body = await readBody<acceptInvitationRequest>(c);
    if (!body) return error(c, 400, "invalid request body", "invalid_request");

    try {
      const workspace = await this.service.acceptInvitation(
        body.token ?? "",
        currentUserId(c) ?? NilUUID,
        currentEmail(c),
      );
      return ok(c, "invitation accepted", toWorkspacePayload(workspace));
    } catch (err) {
      return this.fail(c, "accept invitation", err);
    }
  };

  private fail(c: AppContext, operation: string, err: unknown): Response {
    if (is(err, ErrNotMember)) {
      return error(c, 403, "you are not a member of this workspace", "not_a_member");
    }
    if (is(err, ErrForbidden)) {
      return error(c, 403, "your role does not allow this action", "forbidden");
    }
    if (is(err, ErrLastOwner)) {
      return error(c, 409, "a workspace must keep at least one owner", "last_owner");
    }
    if (is(err, ErrCannotRemoveYourself)) {
      return error(c, 409, "you cannot remove your own membership", "cannot_remove_yourself");
    }
    if (is(err, ErrMemberNotFound)) {
      return error(c, 404, "member not found", "member_not_found");
    }
    if (is(err, ErrInvitationAlreadySent)) {
      return error(c, 409, "an invitation for this address is already pending", "invitation_pending");
    }
    if (is(err, ErrInvitationInvalid)) {
      return error(c, 400, "invitation is invalid, expired, or already accepted", "invalid_invitation");
    }
    if (is(err, ErrInvitationEmailMatch)) {
      return error(c, 403, "sign in with the invited email address to accept", "invitation_email_mismatch");
    }
    if (is(err, ErrInvalidInput)) {
      return error(c, 400, errorMessage(err), "invalid_input");
    }
    if (is(err, ErrInvalidRole)) {
      return error(c, 400, "unknown role", "invalid_role");
    }
    if (is(err, ErrPlanLimit)) {
      // 409 rather than 403: nothing is forbidden about who is asking, and the
      // answer is to buy a bigger package rather than to ask somebody else.
      return error(c, 409, planLimitMessage(err), "plan_limit_reached");
    }
    if (is(err, ErrWorkspaceNotFound)) {
      return error(c, 404, "workspace not found", "workspace_not_found");
    }
    this.log.error({ operation, err }, "workspace request failed");
    return error(c, 500, "internal server error", "internal_error");
  }
}

/** Registers the authenticated endpoints that do not need an active workspace. */
export function userRoutes(group: Hono<AppEnv>, h: Handler): void {
  group.get("/workspaces", h.list);
}

/** Registers the endpoints that create or join tenancy. */
export function verifiedRoutes(group: Hono<AppEnv>, h: Handler): void {
  group.post("/workspaces/onboard", h.onboard);
  group.post("/invitations/accept", h.acceptInvitation);
}

/** Registers the member and invitation management endpoints. */
export function managerRoutes(group: Hono<AppEnv>, h: Handler): void {
  group.patch("/workspaces/current/members/:userID", h.setRole);
  group.delete("/workspaces/current/members/:userID", h.removeMember);
  group.post("/workspaces/current/invitations", h.invite);
  group.delete("/workspaces/current/invitations/:invitationID", h.revokeInvitation);
}

/** Registers the workspace settings endpoints, which only an owner may change. */
export function ownerRoutes(group: Hono<AppEnv>, h: Handler): void {
  group.patch("/workspaces/current", h.update);
}

/** Registers the endpoints that operate on the active workspace. */
export function tenantRoutes(group: Hono<AppEnv>, h: Handler): void {
  group.get("/workspaces/current", h.current);
  group.get("/workspaces/current/teams", h.teams);
  group.get("/workspaces/current/members", h.members);
  group.get("/workspaces/current/invitations", h.invitations);
}

/** The module scope from the identity the tenant middleware stored. */
function scope(c: AppContext): Scope {
  const s = requestScope(c);
  return { userId: s.userId ?? NilUUID, workspaceId: s.workspaceId ?? NilUUID };
}

/**
 * Reports whether an update changed what a scheduled time means.
 *
 * Only the zone the wall-clock times are read in, and the window itself, move
 * a routine. A rename must not touch the engine at all.
 */
function clockMoved(before: Workspace, after: Workspace): boolean {
  if (before.timezone !== after.timezone) return true;
  if (before.workHoursStart !== after.workHoursStart || before.workHoursEnd !== after.workHoursEnd) return true;
  const beforeDays = before.workDays ?? [];
  const afterDays = after.workDays ?? [];
  if (beforeDays.length !== afterDays.length) return true;
  for (let i = 0; i < beforeDays.length; i++) {
    if (beforeDays[i] !== afterDays[i]) return true;
  }
  return false;
}

/** The package's own number, stripped of the sentinel prefix as Go does. */
function planLimitMessage(err: unknown): string {
  const message = errorMessage(err);
  return message.slice(message.indexOf(":") + 1).trim();
}

async function readBody<T>(c: AppContext): Promise<T | null> {
  try {
    return (await c.req.json()) as T;
  } catch {
    return null;
  }
}

function toWorkspacePayload(workspace: Workspace): workspacePayload {
  const payload: workspacePayload = {
    id: workspace.id,
    name: workspace.name,
    business_field: workspace.businessField,
    timezone: workspace.timezone,
    language: workspace.language,
    work_hours_start: workspace.workHoursStart,
    work_hours_end: workspace.workHoursEnd,
    work_days: workspace.workDays ?? null,
    created_at: workspace.createdAt.toISOString(),
  };
  if (workspace.role) {
    payload.role = workspace.role;
  }
  return payload;
}

function toTeamPayload(team: Team): teamPayload {
  return { id: team.id, name: team.name, kind: team.kind };
}

function toMemberPayload(member: Member): memberPayload {
  return {
    user_id: member.userId,
    email: member.email,
    role: member.role,
    email_verified: member.emailVerified,
    joined_at: member.createdAt.toISOString(),
  };
}

function toOnboardPayload(result: OnboardResult): onboardPayload {
  return {
    workspace: toWorkspacePayload(result.workspace),
    teams: result.teams.map(toTeamPayload),
    created: result.created,
  };
}
