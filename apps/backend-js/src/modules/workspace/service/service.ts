/**
 * The workspace use cases: onboarding, member management, and invitations.
 *
 * It depends on the domain contracts and the platform primitives only.
 *
 * Ported 1:1 from internal/modules/workspace/service/service.go.
 */

import { nopLogger, type Logger } from "../../../platform/logger.ts";
import type { AuditEntry } from "../domain/ports.ts";
import {
  RoleOwner,
  canAdminister,
  canManageMembers,
  hashToken,
  isValidRole,
  wrap,
  type Auditor,
  type Invitation,
  type Mailer,
  type Member,
  type OnboardRequest,
  type OnboardResult,
  type PlanLimits,
  type Repository,
  type Scope,
  type Service as ServicePort,
  type Team,
  type Tokens,
  type UpdateRequest,
  type Workspace,
  ErrCannotRemoveYourself,
  ErrForbidden,
  ErrInvitationInvalid,
  ErrInvalidInput,
  ErrInvalidRole,
  ErrNotMember,
  ErrPlanLimit,
} from "../domain/workspace.ts";

/** The workspace settings. */
export interface Config {
  /** How long an invitation link stays valid, in milliseconds. */
  invitationTTL?: number;
  /** The base URL used to build the invitation link. */
  frontendURL?: string;
  /** Used when the onboarding form omits one. */
  defaultTimezone?: string;
  /** Injectable so tests can control expiry. */
  clock?: () => Date;
  /** Reports a trail that could not be written. Optional. */
  logger?: Logger;
}

/** The service dependencies. */
export interface ServiceDeps {
  repository: Repository;
  /** Delivers invitation links. Optional. */
  mailer?: Mailer | null;
  /** Generates the random invitation token and its hash. */
  tokens: Tokens;
  /** Reports how many members the workspace's package allows. Optional. */
  limits?: PlanLimits | null;
  /** Records membership and role changes in the workspace's trail. Optional. */
  auditor?: Auditor | null;
  config?: Config;
}

/** The reply languages a workspace may choose. */
const languageIndonesian = "id";
const validLanguages: Record<string, true> = { id: true, en: true, both: true };

/** The concrete service behind `ServicePort`. */
export class Service implements ServicePort {
  private readonly repo: Repository;
  private readonly mail: Mailer | null;
  private readonly tokens: Tokens;
  private limits: PlanLimits | null;
  private auditor: Auditor | null;
  private readonly cfg: Required<Pick<Config, "invitationTTL" | "frontendURL" | "defaultTimezone" | "clock" | "logger">>;

  constructor(deps: ServiceDeps) {
    this.repo = deps.repository;
    this.mail = deps.mailer ?? null;
    this.tokens = deps.tokens;
    this.limits = deps.limits ?? null;
    this.auditor = deps.auditor ?? null;
    this.cfg = {
      invitationTTL: deps.config?.invitationTTL && deps.config.invitationTTL > 0
        ? deps.config.invitationTTL
        : 7 * 24 * 60 * 60 * 1000,
      frontendURL: deps.config?.frontendURL ?? "",
      defaultTimezone: deps.config?.defaultTimezone || "Asia/Jakarta",
      clock: deps.config?.clock ?? (() => new Date()),
      logger: deps.config?.logger ?? nopLogger,
    };
  }

  /** Installs the package's member cap after construction. */
  usePlanLimits(limits: PlanLimits): void {
    this.limits = limits;
  }

  /** Installs the trail writer after construction. */
  useAuditor(auditor: Auditor): void {
    this.auditor = auditor;
  }

  /**
   * Creates the workspace, makes the caller its owner, and creates the default
   * team. Submitting twice returns the same workspace.
   */
  async onboard(userId: string, req: OnboardRequest): Promise<OnboardResult> {
    if (!userId || userId === "00000000-0000-0000-0000-000000000000") {
      throw ErrNotMember;
    }

    const name = req.name.trim();
    if (name === "") {
      throw wrap(ErrInvalidInput, "business name is required");
    }

    const timezone = req.timezone.trim() || this.cfg.defaultTimezone;
    if (!isKnownTimezone(timezone)) {
      throw wrap(ErrInvalidInput, `unknown timezone "${timezone}"`);
    }

    // The repository creates the workspace, the owner membership, and the
    // default team in one transaction.
    return this.repo.onboard(userId, {
      name,
      businessField: req.businessField.trim(),
      timezone,
    });
  }

  /** The workspaces the caller belongs to. */
  async workspaces(userId: string): Promise<Workspace[]> {
    return this.repo.workspaces(userId);
  }

  /** The active workspace. */
  async current(scope: Scope): Promise<Workspace> {
    return this.repo.workspace(scope);
  }

  /**
   * Changes the workspace profile. Only an owner may do it.
   *
   * Every field is validated here rather than in the handler, because the same
   * values decide when the approval gate lets an action leave.
   */
  async update(scope: Scope, actorRole: string, req: UpdateRequest): Promise<Workspace> {
    if (!canAdminister(actorRole)) {
      throw ErrForbidden;
    }

    const name = req.name.trim();
    if (name === "") {
      throw wrap(ErrInvalidInput, "business name is required");
    }

    const timezone = req.timezone.trim() || this.cfg.defaultTimezone;
    if (!isKnownTimezone(timezone)) {
      throw wrap(ErrInvalidInput, `unknown timezone "${timezone}"`);
    }

    const language = req.language.trim() || languageIndonesian;
    if (!validLanguages[language]) {
      throw wrap(ErrInvalidInput, `unknown reply language "${language}"`);
    }

    const start = normaliseClock(req.workHoursStart);
    const end = normaliseClock(req.workHoursEnd);
    if (start === end) {
      throw wrap(ErrInvalidInput, "working hours start and end are the same");
    }

    const days = normaliseDays(req.workDays);

    return this.repo.updateWorkspace(scope, {
      name,
      businessField: req.businessField.trim(),
      timezone,
      language,
      workHoursStart: start,
      workHoursEnd: end,
      workDays: days,
    });
  }

  /** The teams of the active workspace. */
  async teams(scope: Scope): Promise<Team[]> {
    return this.repo.teams(scope);
  }

  /** The members of the active workspace. */
  async members(scope: Scope): Promise<Member[]> {
    return this.repo.members(scope);
  }

  /**
   * Changes a member's role. Owners and admins may manage members, but only an
   * owner may grant the owner role or change another owner.
   */
  async setRole(scope: Scope, actorRole: string, userId: string, role: string): Promise<Member> {
    if (!canManageMembers(actorRole)) {
      throw ErrForbidden;
    }
    if (!isValidRole(role)) {
      throw ErrInvalidRole;
    }
    if (role === RoleOwner && !canAdminister(actorRole)) {
      throw ErrForbidden;
    }
    await this.guardOwnerTarget(scope, actorRole, userId);

    const member = await this.repo.setRole(scope, userId, role);

    await this.audit(scope, {
      action: "member.role_changed",
      objectType: "member",
      objectId: userId,
      actorUserId: scope.userId ?? "",
      metadata: { role },
    });

    return member;
  }

  /**
   * Deletes a membership. A member cannot remove anyone, an admin cannot
   * remove an owner, and nobody can remove the last owner.
   */
  async removeMember(scope: Scope, actorRole: string, actorId: string, userId: string): Promise<void> {
    if (!canManageMembers(actorRole)) {
      throw ErrForbidden;
    }
    if (actorId === userId) {
      throw ErrCannotRemoveYourself;
    }
    await this.guardOwnerTarget(scope, actorRole, userId);

    // The role is read before the row goes, because after it the trail can only
    // say that somebody left. It is read only when something will record it.
    let held = "";
    if (this.auditor) {
      held = await this.roleOf(scope, userId);
    }

    await this.repo.removeMember(scope, userId);

    await this.audit(scope, {
      action: "member.removed",
      objectType: "member",
      objectId: userId,
      actorUserId: scope.userId ?? "",
      metadata: { had_role: held },
    });
  }

  /** Reads one member's role for a trail entry, answering "" when unreadable. */
  private async roleOf(scope: Scope, userId: string): Promise<string> {
    try {
      const members = await this.repo.members(scope);
      for (const member of members) {
        if (member.userId === userId) return member.role;
      }
    } catch {
      // A trail line with no previous role is worse than none.
    }
    return "";
  }

  /**
   * Records one entry. A trail that could not be written is reported rather
   * than failing the change.
   */
  private async audit(scope: Scope, entry: AuditEntry): Promise<void> {
    if (!this.auditor) return;
    try {
      await this.auditor.record(undefined, scope, entry);
    } catch (err) {
      this.cfg.logger.warn({ action: entry.action, err }, "could not record a workspace audit entry");
    }
  }

  /**
   * Refuses an invitation the workspace's package does not allow.
   *
   * It counts the people who are there plus the invitations that are still
   * open, because an invitation is a seat somebody will take.
   */
  private async checkMemberLimit(scope: Scope): Promise<void> {
    if (!this.limits) return;

    const limit = await this.limits.maxMembers(undefined, scope.workspaceId ?? "");
    if (limit <= 0) return;

    const members = await this.repo.members(scope);
    const invitations = await this.repo.invitations(scope);

    if (members.length + invitations.length >= limit) {
      throw wrap(ErrPlanLimit, `paket ini mengizinkan ${limit} anggota; naikkan paket untuk menambah`);
    }
  }

  /**
   * Sends an invitation. Only owners and admins may invite, and the role they
   * may grant is capped by their own.
   */
  async invite(scope: Scope, actorRole: string, actorId: string, email: string, role: string): Promise<Invitation> {
    if (!canManageMembers(actorRole)) {
      throw ErrForbidden;
    }
    if (!isValidRole(role)) {
      throw ErrInvalidRole;
    }
    if (role === RoleOwner && !canAdminister(actorRole)) {
      throw ErrForbidden;
    }

    const normalized = normalizeEmail(email);
    await this.checkMemberLimit(scope);

    const { raw, hash } = this.tokens.generate();

    const invitation = await this.repo.createInvitation(scope, {
      email: normalized,
      role,
      tokenHash: hash,
      invitedBy: actorId,
      expiresAt: new Date(this.cfg.clock().getTime() + this.cfg.invitationTTL),
    });

    await this.sendInvitation(normalized, raw, invitation);

    await this.audit(scope, {
      action: "member.invited",
      objectType: "invitation",
      objectId: invitation.id,
      actorUserId: actorId,
      metadata: { role },
    });

    return invitation;
  }

  /** The pending invitations of the active workspace. */
  async invitations(scope: Scope): Promise<Invitation[]> {
    return this.repo.invitations(scope);
  }

  /** Deletes a pending invitation. */
  async revokeInvitation(scope: Scope, actorRole: string, id: string): Promise<void> {
    if (!canManageMembers(actorRole)) {
      throw ErrForbidden;
    }

    await this.repo.revokeInvitation(scope, id);

    await this.audit(scope, {
      action: "member.invitation_revoked",
      objectType: "invitation",
      objectId: id,
      actorUserId: scope.userId ?? "",
    });
  }

  /** Consumes an invitation for the signed-in account. */
  async acceptInvitation(token: string, userId: string, email: string): Promise<Workspace> {
    if (token.trim() === "") {
      throw ErrInvitationInvalid;
    }
    if (!userId || userId === "00000000-0000-0000-0000-000000000000") {
      throw ErrInvitationInvalid;
    }

    return this.repo.acceptInvitation(hashToken(token), userId, email.trim().toLowerCase());
  }

  /** Returns the caller's role, which is what the tenant middleware uses. */
  async resolve(workspaceId: string, userId: string): Promise<string> {
    if (!workspaceId || !userId) {
      throw ErrNotMember;
    }
    return this.repo.membership({ workspaceId, userId }, userId);
  }

  /**
   * Stops an admin from touching an owner: only an owner may re-role or remove
   * another owner.
   */
  private async guardOwnerTarget(scope: Scope, actorRole: string, userId: string): Promise<void> {
    if (canAdminister(actorRole)) return;

    const members = await this.repo.members(scope);
    for (const member of members) {
      if (member.userId === userId && member.role === RoleOwner) {
        throw ErrForbidden;
      }
    }
  }

  private async sendInvitation(email: string, token: string, invitation: Invitation): Promise<void> {
    if (!this.mail) return;

    const link = `${this.cfg.frontendURL.replace(/\/+$/, "")}/join?token=${token}`;
    const body =
      `Kamu diundang bergabung ke workspace Bolu sebagai ${invitation.role}.\n\n` +
      `Buka tautan ini untuk menerima:\n\n${link}\n\n` +
      `Tautan berlaku sampai ${formatInvitationExpiry(invitation.expiresAt)}.`;

    await this.mail.send(undefined, email, "Undangan bergabung ke Bolu", body);
  }
}

/** True for an IANA zone the runtime accepts, which is what `time.LoadLocation` asks. */
function isKnownTimezone(timezone: string): boolean {
  try {
    new Intl.DateTimeFormat("en-US", { timeZone: timezone });
    return true;
  } catch {
    return false;
  }
}

/**
 * Validates a "HH:MM" time of day and returns it zero-padded.
 *
 * The database column is `time`, so the string is checked here instead of being
 * handed to the driver.
 */
function normaliseClock(value: string): string {
  const trimmed = value.trim();
  const match = /^(\d+):(\d+)$/.exec(trimmed);
  if (!match) {
    throw wrap(ErrInvalidInput, "working hours must be HH:MM");
  }

  const hour = Number(match[1]);
  const minute = Number(match[2]);
  if (hour < 0 || hour > 23 || minute < 0 || minute > 59) {
    throw wrap(ErrInvalidInput, `"${trimmed}" is not a time of day`);
  }

  return `${String(hour).padStart(2, "0")}:${String(minute).padStart(2, "0")}`;
}

/**
 * Validates the working week and returns it sorted and deduplicated.
 *
 * An empty week is refused rather than treated as "every day".
 */
function normaliseDays(days: number[]): number[] {
  if (!days || days.length === 0) {
    throw wrap(ErrInvalidInput, "pick at least one working day");
  }

  const seen = new Set<number>();
  const out: number[] = [];
  for (const day of days) {
    if (day < 1 || day > 7) {
      throw wrap(ErrInvalidInput, `${day} is not a weekday`);
    }
    if (seen.has(day)) continue;
    seen.add(day);
    out.push(day);
  }
  out.sort((a, b) => a - b);

  return out;
}

function normalizeEmail(email: string): string {
  const trimmed = email.trim();
  if (trimmed === "") {
    throw wrap(ErrInvalidInput, "email is required");
  }
  if (!isEmail(trimmed)) {
    throw wrap(ErrInvalidInput, "email is not valid");
  }
  return trimmed.toLowerCase();
}

/** A practical `net/mail.ParseAddress` equivalent: one address, no display name. */
function isEmail(value: string): boolean {
  if (value.includes(" ") || value.includes("<") || value.includes(">")) return false;
  return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(value);
}

/** Renders an expiry as Go's `2 January 2006 15:04 MST`. */
function formatInvitationExpiry(at: Date): string {
  const months = [
    "January",
    "February",
    "March",
    "April",
    "May",
    "June",
    "July",
    "August",
    "September",
    "October",
    "November",
    "December",
  ];
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${at.getUTCDate()} ${months[at.getUTCMonth()]} ${at.getUTCFullYear()} ${pad(at.getUTCHours())}:${pad(
    at.getUTCMinutes(),
  )} UTC`;
}
