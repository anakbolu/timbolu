/**
 * The workspace data access.
 *
 * Every method runs inside the caller's scope, so Row Level Security filters
 * the statements even if a query forgets a filter. Onboarding and accepting an
 * invitation each run in one transaction: a workspace must never exist without
 * its owner membership and team, and an invitation must never be consumed
 * without the membership it grants.
 *
 * Ported 1:1 from internal/modules/workspace/repository/postgres.go, with
 * repository/sqlcgen/*.go folded in: each query keeps its name as a comment and
 * its SQL text unchanged.
 */

import { setWorkspace, type Database, type Tx } from "../../../platform/database/index.ts";
import {
  ErrInvitationAlreadySent,
  ErrInvitationEmailMatch,
  ErrInvitationInvalid,
  ErrLastOwner,
  ErrMemberNotFound,
  ErrNotMember,
  ErrWorkspaceNotFound,
  RoleOwner,
  type Invitation,
  type Member,
  type NewInvitation,
  type OnboardRequest,
  type OnboardResult,
  type Repository,
  type Scope,
  type Team,
  type UpdateRequest,
  type Workspace,
} from "../domain/workspace.ts";

/** The team every workspace starts with. */
const teamBolu = "Tim Bolu";

/** Gives a new workspace the conservative approval policies, inside onboarding. */
export interface PolicyProvisioner {
  seedPolicies(tx: Tx, workspaceId: string): Promise<void>;
}

/** Gives a new workspace the package it starts on, inside onboarding. */
export interface SubscriptionProvisioner {
  startTrial(tx: Tx, workspaceId: string): Promise<void>;
}

/** Records that an account finished onboarding, inside the same transaction. */
export interface OnboardedMarker {
  markOnboardedInTx(tx: Tx, userId: string): Promise<void>;
}

/** The window a workspace lets actions leave in. */
export interface WorkingHours {
  start: string;
  end: string;
  timezone: string;
  /** ISO weekdays the window applies to (1 = Monday). */
  days: number[];
}

/** One row of `workspaces`. */
interface WorkspaceRow {
  id: string;
  name: string;
  business_field: string;
  timezone: string;
  language: string;
  work_hours_start: string;
  work_hours_end: string;
  created_at: Date;
  updated_at: Date;
  owner_user_id: string | null;
  work_days: number[];
}

/** One row of `teams`. */
interface TeamRow {
  id: string;
  workspace_id: string;
  name: string;
  kind: string;
  created_at: Date;
  updated_at: Date;
}

/** One row of `invitations`. */
interface InvitationRow {
  id: string;
  workspace_id: string;
  email: string;
  role: string;
  token_hash: string;
  invited_by: string | null;
  expires_at: Date;
  accepted_at: Date | null;
  accepted_by: string | null;
  created_at: Date;
}

/** One member joined to its account. */
interface MemberRow {
  id: string;
  workspace_id: string;
  user_id: string;
  role: string;
  created_at: Date;
  updated_at: Date;
  email: string;
  email_verified_at: Date | null;
}

/** The `time` column arrives as text; the domain wants "HH:MM". */
function formatClock(value: string | null | undefined): string {
  if (!value) return "";
  const match = /^(\d{2}):(\d{2})/.exec(value);
  return match ? `${match[1]}:${match[2]}` : "";
}

function toWorkspace(row: WorkspaceRow, role: string): Workspace {
  return {
    id: row.id,
    name: row.name,
    businessField: row.business_field,
    timezone: row.timezone,
    language: row.language,
    workHoursStart: formatClock(row.work_hours_start),
    workHoursEnd: formatClock(row.work_hours_end),
    workDays: row.work_days ?? [],
    role,
    createdAt: row.created_at,
  };
}

function toTeams(rows: TeamRow[]): Team[] {
  return rows.map((row) => ({ id: row.id, name: row.name, kind: row.kind }));
}

function toMember(row: MemberRow): Member {
  return {
    userId: row.user_id,
    email: row.email,
    role: row.role,
    emailVerified: row.email_verified_at !== null,
    createdAt: row.created_at,
  };
}

function toInvitation(row: InvitationRow, workspaceName: string): Invitation {
  return {
    id: row.id,
    workspaceId: row.workspace_id,
    workspaceName,
    email: row.email,
    role: row.role,
    expiresAt: row.expires_at,
    createdAt: row.created_at,
  };
}

/** True for the Postgres unique violation. */
function isUniqueViolation(err: unknown): boolean {
  if (typeof err !== "object" || err === null || !("code" in err)) return false;
  return err.code === "23505";
}

export class PostgresRepository implements Repository {
  private policies: PolicyProvisioner | null = null;
  private subscriptions: SubscriptionProvisioner | null = null;
  private onboarded: OnboardedMarker | null = null;

  constructor(private readonly db: Database) {}

  /** Installs the approval repository as the policy provisioner. */
  setPolicyProvisioner(provisioner: PolicyProvisioner): void {
    this.policies = provisioner;
  }

  /** Installs the billing repository as the trial provisioner. */
  setSubscriptionProvisioner(provisioner: SubscriptionProvisioner): void {
    this.subscriptions = provisioner;
  }

  /** Installs the auth repository as the onboarding marker. */
  setOnboardedMarker(marker: OnboardedMarker): void {
    this.onboarded = marker;
  }

  /**
   * Creates the workspace, its owner membership and the default team in one
   * transaction. Idempotent: a caller who already owns a workspace gets that
   * workspace back instead of a second one.
   */
  async onboard(userId: string, req: OnboardRequest): Promise<OnboardResult> {
    return this.db.inScope({ userId }, async (tx) => {
      // name: GetOwnedWorkspaceForUser
      const existing = await tx<WorkspaceRow[]>`
        SELECT id, name, business_field, timezone, language, work_hours_start, work_hours_end, created_at, updated_at, owner_user_id, work_days FROM workspaces
        WHERE owner_user_id = ${userId}
        ORDER BY created_at
        LIMIT 1
      `;

      if (existing.length > 0) {
        const row = existing[0];
        await setWorkspace(tx, row.id);
        // Heal an account that onboarded before the marker was wired.
        await this.markOnboarded(tx, userId);

        // name: ListTeams
        const teams = await tx<TeamRow[]>`
          SELECT id, workspace_id, name, kind, created_at, updated_at FROM teams WHERE workspace_id = ${row.id} ORDER BY name
        `;

        return {
          workspace: toWorkspace(row, RoleOwner),
          teams: toTeams(teams),
          created: false,
        };
      }

      // name: CreateWorkspace
      const createdRows = await tx<WorkspaceRow[]>`
        INSERT INTO workspaces (name, business_field, timezone, owner_user_id)
        VALUES (${req.name}, ${req.businessField}, ${req.timezone}, ${userId})
        RETURNING id, name, business_field, timezone, language, work_hours_start, work_hours_end, created_at, updated_at, owner_user_id, work_days
      `;
      const created = createdRows[0];

      // The rows that follow belong to the new workspace, so the tenant
      // context is activated before they are written.
      await setWorkspace(tx, created.id);

      // name: CreateMember
      await tx`
        INSERT INTO members (workspace_id, user_id, role)
        VALUES (${created.id}, ${userId}, ${RoleOwner})
        RETURNING id, workspace_id, user_id, role, created_at, updated_at
      `;

      // One team is created, because a Bolu has to belong to one. What is
      // deliberately not created is a Bolu: the workspace starts empty.
      // name: CreateTeam
      const teamRows = await tx<TeamRow[]>`
        INSERT INTO teams (workspace_id, name, kind)
        VALUES (${created.id}, ${teamBolu}, ${"bolu"})
        RETURNING id, workspace_id, name, kind, created_at, updated_at
      `;

      // The conservative approval policies, so no action kind reaches the
      // outside world without a row the workspace can see and change.
      if (this.policies) {
        await this.policies.seedPolicies(tx, created.id);
      }

      // The trial package, so the new workspace has an allowance before it can
      // spend anything.
      if (this.subscriptions) {
        await this.subscriptions.startTrial(tx, created.id);
      }

      await this.markOnboarded(tx, userId);

      return {
        workspace: toWorkspace(created, RoleOwner),
        teams: toTeams(teamRows),
        created: true,
      };
    });
  }

  /** Records the account inside the onboarding transaction. */
  private async markOnboarded(tx: Tx, userId: string): Promise<void> {
    if (!this.onboarded) return;
    await this.onboarded.markOnboardedInTx(tx, userId);
  }

  /**
   * The window a workspace lets actions leave in.
   *
   * It runs unscoped by user rather than through the tenant helpers: the caller
   * is the approval runtime, which already knows the workspace.
   */
  async workingHours(workspaceId: string): Promise<WorkingHours> {
    return this.db.inScopeRead({ userId: crypto.randomUUID(), workspaceId }, async (tx) => {
      // name: GetWorkspaceHours
      const rows = await tx<{ timezone: string; work_hours_start: string; work_hours_end: string; work_days: number[] }[]>`
        SELECT timezone, work_hours_start, work_hours_end, work_days
        FROM workspaces WHERE id = ${workspaceId}
      `;
      if (rows.length === 0) throw ErrWorkspaceNotFound;

      const row = rows[0];
      return {
        start: formatClock(row.work_hours_start),
        end: formatClock(row.work_hours_end),
        timezone: row.timezone,
        days: row.work_days ?? [],
      };
    });
  }

  /** The workspaces the user belongs to, with their role. */
  async workspaces(userId: string): Promise<Workspace[]> {
    return this.db.inScopeRead({ userId }, async (tx) => {
      // name: ListWorkspacesForUser
      const rows = await tx<(WorkspaceRow & { role: string })[]>`
        SELECT w.id, w.name, w.business_field, w.timezone, w.language, w.work_hours_start, w.work_hours_end, w.created_at, w.updated_at, w.owner_user_id, w.work_days, m.role
        FROM workspaces w
        JOIN members m ON m.workspace_id = w.id
        WHERE m.user_id = ${userId}
        ORDER BY w.created_at
      `;
      return rows.map((row) => ({
        id: row.id,
        name: row.name,
        businessField: row.business_field,
        timezone: row.timezone,
        language: row.language,
        workHoursStart: formatClock(row.work_hours_start),
        workHoursEnd: formatClock(row.work_hours_end),
        workDays: row.work_days ?? [],
        role: row.role,
        createdAt: row.created_at,
      }));
    });
  }

  /** One workspace inside the caller's scope. */
  async workspace(scope: Scope): Promise<Workspace> {
    return this.read(scope, async (tx) => {
      // name: GetWorkspace
      const rows = await tx<WorkspaceRow[]>`
        SELECT id, name, business_field, timezone, language, work_hours_start, work_hours_end, created_at, updated_at, owner_user_id, work_days FROM workspaces WHERE id = ${scope.workspaceId}
      `;
      if (rows.length === 0) throw ErrWorkspaceNotFound;
      return toWorkspace(rows[0], "");
    });
  }

  /** Changes the workspace profile. */
  async updateWorkspace(scope: Scope, req: UpdateRequest): Promise<Workspace> {
    return this.write(scope, async (tx) => {
      // name: UpdateWorkspace
      const rows = await tx<WorkspaceRow[]>`
        UPDATE workspaces
        SET name = ${req.name},
            business_field = ${req.businessField},
            timezone = ${req.timezone},
            language = ${req.language},
            work_hours_start = ${req.workHoursStart},
            work_hours_end = ${req.workHoursEnd},
            work_days = ${req.workDays},
            updated_at = now()
        WHERE id = ${scope.workspaceId}
        RETURNING id, name, business_field, timezone, language, work_hours_start, work_hours_end, created_at, updated_at, owner_user_id, work_days
      `;
      if (rows.length === 0) throw ErrWorkspaceNotFound;
      return toWorkspace(rows[0], "");
    });
  }

  /** The teams of the active workspace. */
  async teams(scope: Scope): Promise<Team[]> {
    return this.read(scope, async (tx) => {
      // name: ListTeams
      const rows = await tx<TeamRow[]>`
        SELECT id, workspace_id, name, kind, created_at, updated_at FROM teams WHERE workspace_id = ${scope.workspaceId} ORDER BY name
      `;
      return toTeams(rows);
    });
  }

  /** The caller's role in the workspace, or ErrNotMember. */
  async membership(scope: Scope, userId: string): Promise<string> {
    return this.read(scope, async (tx) => {
      // name: GetMembership
      const rows = await tx<{ role: string }[]>`
        SELECT id, workspace_id, user_id, role, created_at, updated_at FROM members WHERE workspace_id = ${scope.workspaceId} AND user_id = ${userId}
      `;
      if (rows.length === 0) throw ErrNotMember;
      return rows[0].role;
    });
  }

  /** The members of the active workspace. */
  async members(scope: Scope): Promise<Member[]> {
    return this.read(scope, async (tx) => {
      // name: ListMembers
      const rows = await tx<MemberRow[]>`
        SELECT m.id, m.workspace_id, m.user_id, m.role, m.created_at, m.updated_at, u.email, u.email_verified_at
        FROM members m
        JOIN users u ON u.id = m.user_id
        WHERE m.workspace_id = ${scope.workspaceId}
        ORDER BY m.created_at
      `;
      return rows.map(toMember);
    });
  }

  /** Changes a member's role, refusing to demote the last owner. */
  async setRole(scope: Scope, userId: string, role: string): Promise<Member> {
    return this.write(scope, async (tx) => {
      // name: GetMembership
      const current = await tx<{ role: string }[]>`
        SELECT id, workspace_id, user_id, role, created_at, updated_at FROM members WHERE workspace_id = ${scope.workspaceId} AND user_id = ${userId}
      `;
      if (current.length === 0) throw ErrMemberNotFound;

      if (current[0].role === RoleOwner && role !== RoleOwner) {
        // name: CountOwners
        const owners = await tx<{ count: number }[]>`
          SELECT count(*)::bigint FROM members WHERE workspace_id = ${scope.workspaceId} AND role = 'owner'
        `;
        if (owners[0].count <= 1) throw ErrLastOwner;
      }

      // name: UpdateMemberRole
      const updated = await tx`
        UPDATE members
        SET role = ${role}, updated_at = now()
        WHERE workspace_id = ${scope.workspaceId} AND user_id = ${userId}
        RETURNING id, workspace_id, user_id, role, created_at, updated_at
      `;
      if (updated.count === 0) throw ErrWorkspaceNotFound;

      // name: GetMemberDetail
      const detail = await tx<MemberRow[]>`
        SELECT m.id, m.workspace_id, m.user_id, m.role, m.created_at, m.updated_at,
               u.email, u.email_verified_at
        FROM members m
        JOIN users u ON u.id = m.user_id
        WHERE m.workspace_id = ${scope.workspaceId} AND m.user_id = ${userId}
      `;
      if (detail.length === 0) throw ErrWorkspaceNotFound;

      return toMember(detail[0]);
    });
  }

  /** Deletes a membership, refusing to remove the last owner. */
  async removeMember(scope: Scope, userId: string): Promise<void> {
    await this.write(scope, async (tx) => {
      // name: GetMembership
      const current = await tx<{ role: string }[]>`
        SELECT id, workspace_id, user_id, role, created_at, updated_at FROM members WHERE workspace_id = ${scope.workspaceId} AND user_id = ${userId}
      `;
      if (current.length === 0) throw ErrMemberNotFound;

      if (current[0].role === RoleOwner) {
        // name: CountOwners
        const owners = await tx<{ count: number }[]>`
          SELECT count(*)::bigint FROM members WHERE workspace_id = ${scope.workspaceId} AND role = 'owner'
        `;
        if (owners[0].count <= 1) throw ErrLastOwner;
      }

      // name: DeleteMember
      const removed = await tx`
        DELETE FROM members WHERE workspace_id = ${scope.workspaceId} AND user_id = ${userId}
      `;
      if (removed.count === 0) throw ErrMemberNotFound;
    });
  }

  /** Stores a pending invitation. */
  async createInvitation(scope: Scope, invitation: NewInvitation): Promise<Invitation> {
    return this.write(scope, async (tx) => {
      // name: CreateInvitation
      let rows: InvitationRow[];
      try {
        rows = await tx<InvitationRow[]>`
          INSERT INTO invitations (workspace_id, email, role, token_hash, invited_by, expires_at)
          VALUES (${scope.workspaceId}, ${invitation.email}, ${invitation.role}, ${invitation.tokenHash}, ${invitation.invitedBy}, ${invitation.expiresAt})
          RETURNING id, workspace_id, email, role, token_hash, invited_by, expires_at, accepted_at, accepted_by, created_at
        `;
      } catch (err) {
        if (isUniqueViolation(err)) throw ErrInvitationAlreadySent;
        throw err;
      }
      return toInvitation(rows[0], "");
    });
  }

  /** The pending invitations of the active workspace. */
  async invitations(scope: Scope): Promise<Invitation[]> {
    return this.read(scope, async (tx) => {
      // name: ListInvitations
      const rows = await tx<InvitationRow[]>`
        SELECT id, workspace_id, email, role, token_hash, invited_by, expires_at, accepted_at, accepted_by, created_at FROM invitations
        WHERE workspace_id = ${scope.workspaceId} AND accepted_at IS NULL
        ORDER BY created_at DESC
      `;
      return rows.map((row) => toInvitation(row, ""));
    });
  }

  /** Deletes a pending invitation. */
  async revokeInvitation(scope: Scope, id: string): Promise<void> {
    await this.write(scope, async (tx) => {
      // name: DeleteInvitation
      const removed = await tx`
        DELETE FROM invitations WHERE workspace_id = ${scope.workspaceId} AND id = ${id}
      `;
      if (removed.count === 0) throw ErrInvitationInvalid;
    });
  }

  /** Consumes the invitation and creates the membership in one transaction. */
  async acceptInvitation(tokenHash: string, userId: string, email: string): Promise<Workspace> {
    return this.db.inScope({ userId }, async (tx) => {
      // name: GetInvitationByTokenHash
      const invitationRows = await tx<
        (InvitationRow & { workspace_name: string })[]
      >`
        SELECT i.id, i.workspace_id, i.email, i.role, i.token_hash, i.invited_by, i.expires_at, i.accepted_at, i.accepted_by, i.created_at, coalesce(w.name, '')::text AS workspace_name
        FROM invitations i
        LEFT JOIN workspaces w ON w.id = i.workspace_id
        WHERE i.token_hash = ${tokenHash}
      `;
      if (invitationRows.length === 0) throw ErrInvitationInvalid;

      const invitation = invitationRows[0];
      if (invitation.accepted_at !== null || invitation.expires_at.getTime() < Date.now()) {
        throw ErrInvitationInvalid;
      }
      if (invitation.email.toLowerCase() !== email.toLowerCase()) {
        throw ErrInvitationEmailMatch;
      }

      await setWorkspace(tx, invitation.workspace_id);

      // name: GetMembership
      const membership = await tx<{ role: string }[]>`
        SELECT id, workspace_id, user_id, role, created_at, updated_at FROM members WHERE workspace_id = ${invitation.workspace_id} AND user_id = ${userId}
      `;

      // Already a member: accepting again is not an error, the invitation is
      // simply consumed.
      if (membership.length > 0) {
        // name: AcceptInvitation
        await tx`
          UPDATE invitations
          SET accepted_at = now(), accepted_by = ${userId}
          WHERE id = ${invitation.id} AND accepted_at IS NULL
          RETURNING id, workspace_id, email, role, token_hash, invited_by, expires_at, accepted_at, accepted_by, created_at
        `;
        return this.readWorkspaceIn(tx, invitation.workspace_id, invitation.role);
      }

      // name: CreateMember
      await tx`
        INSERT INTO members (workspace_id, user_id, role)
        VALUES (${invitation.workspace_id}, ${userId}, ${invitation.role})
        RETURNING id, workspace_id, user_id, role, created_at, updated_at
      `;

      // name: AcceptInvitation
      const accepted = await tx`
        UPDATE invitations
        SET accepted_at = now(), accepted_by = ${userId}
        WHERE id = ${invitation.id} AND accepted_at IS NULL
        RETURNING id, workspace_id, email, role, token_hash, invited_by, expires_at, accepted_at, accepted_by, created_at
      `;
      if (accepted.count === 0) throw ErrWorkspaceNotFound;

      return this.readWorkspaceIn(tx, invitation.workspace_id, invitation.role);
    });
  }

  private async readWorkspaceIn(tx: Tx, workspaceId: string, role: string): Promise<Workspace> {
    // name: GetWorkspace
    const rows = await tx<WorkspaceRow[]>`
      SELECT id, name, business_field, timezone, language, work_hours_start, work_hours_end, created_at, updated_at, owner_user_id, work_days FROM workspaces WHERE id = ${workspaceId}
    `;
    if (rows.length === 0) throw ErrWorkspaceNotFound;
    return toWorkspace(rows[0], role);
  }

  // ------------------------------------------------------------- erasure

  /** Reads a workspace's name before the row that holds it is deleted. */
  async workspaceName(id: string): Promise<string> {
    return this.db.inScopeRead({ workspaceId: id }, async (tx) => {
      // name: WorkspaceName
      const rows = await tx<{ name: string }[]>`
        SELECT name FROM workspaces WHERE id = ${id}
      `;
      if (rows.length === 0) throw ErrWorkspaceNotFound;
      return rows[0].name;
    });
  }

  /** Reads the address of the person who asked, for the erasure record. */
  async requesterEmail(userId: string): Promise<string> {
    // name: RequesterEmail
    const rows = await this.db.sql<{ email: string }[]>`
      SELECT email FROM users WHERE id = ${userId}
    `;
    if (rows.length === 0) throw ErrWorkspaceNotFound;
    return rows[0].email;
  }

  /**
   * Deletes a workspace and everything that references it, reporting how many
   * rows went.
   *
   * `app.audit_erase` is set for this transaction only: `audit_logs` refuses a
   * delete from anywhere else, and a session-level setting would outlive the
   * call.
   */
  async eraseWorkspace(id: string): Promise<number> {
    return this.db.inScope({ userId: id, workspaceId: id }, async (tx) => {
      await tx.unsafe("SELECT set_config('app.audit_erase', 'on', true)");

      // name: CountWorkspaceRows
      const counted = await tx<{ rows: number }[]>`
        SELECT COALESCE(sum(counted.rows), 0)::bigint AS rows
        FROM (
            SELECT (
                xpath(
                    '/row/c/text()',
                    query_to_xml(
                        format('SELECT count(*) AS c FROM %I.%I WHERE workspace_id = %L',
                               c.table_schema, c.table_name, ${id}::text),
                        false, true, ''
                    )
                )
            )[1]::text::bigint AS rows
            FROM information_schema.columns c
            JOIN information_schema.tables t
              ON t.table_schema = c.table_schema AND t.table_name = c.table_name
            WHERE c.column_name = 'workspace_id'
              AND c.table_schema = current_schema()
              AND t.table_type = 'BASE TABLE'
              AND t.table_name <> 'data_erasures'
        ) AS counted
      `;

      // name: DeleteWorkspace
      const deleted = await tx`
        DELETE FROM workspaces WHERE id = ${id}
      `;
      if (deleted.count === 0) throw ErrWorkspaceNotFound;

      return counted[0].rows + deleted.count;
    });
  }

  private async read<T>(scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    return this.db.inScopeRead({ userId: scope.userId, workspaceId: scope.workspaceId }, fn);
  }

  private async write<T>(scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    return this.db.inScope({ userId: scope.userId, workspaceId: scope.workspaceId }, fn);
  }
}
