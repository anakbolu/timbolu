/**
 * The operator surface's storage: the account list and the numbers on the
 * overview.
 *
 * Ported from internal/modules/admin/repository/postgres.go and the folded sqlc
 * queries in repository/sqlcgen (admin.sql).
 *
 * Every statement here runs outside a tenant scope, and that is what these
 * statements are: they read `users`, the one table in the schema that belongs to
 * the deployment rather than to a workspace, and which carries no Row Level
 * Security for exactly that reason. Nothing here reads a tenant table.
 */

import type { Database } from "../../../platform/database/index.ts";
import type { Account, Filter, Stats } from "../domain/admin.ts";
import { AdminError, ErrAccountNotFound, ErrLastAdmin, ErrNotConfigured, normaliseFilter } from "../domain/admin.ts";

/** The row shape of `users`, as every query below returns it. */
export interface UserRow {
  id: string;
  email: string;
  password_hash: string;
  email_verified_at: Date | null;
  google_subject: string | null;
  created_at: Date;
  updated_at: Date;
  onboarded_at: Date | null;
  role: string;
}

/** The row shape of the workspace-count query. */
export interface WorkspaceCountRow {
  user_id: string;
  workspace_count: number;
}

/** The row shape of the stats query. */
export interface UserStatsRow {
  total: number;
  admins: number;
  users: number;
  verified: number;
  onboarded: number;
  google_accounts: number;
  new_last_7_days: number;
}

/** Repository is the Postgres-backed implementation of the domain contract. */
export class Repository {
  private readonly db: Database | null;

  constructor(db: Database | null) {
    this.db = db;
  }

  /** The pool, or ErrNotConfigured when the deployment has no database. */
  private pool(): Database {
    if (!this.db) throw ErrNotConfigured;
    return this.db;
  }

  /** A page of accounts, newest first. */
  async users(filter: Filter): Promise<Account[]> {
    const db = this.pool();
    const f = normaliseFilter(filter);
    const rows = await db.sql<UserRow[]>`
      SELECT id, email, password_hash, email_verified_at, google_subject, created_at, updated_at, onboarded_at, role FROM users
      WHERE (${f.search}::text = '' OR lower(email) LIKE '%' || lower(${f.search}::text) || '%')
        AND (${f.role}::text = '' OR role = ${f.role}::text)
      ORDER BY created_at DESC
      LIMIT ${f.limit} OFFSET ${f.offset}
    `;
    return rows.map(toAccount);
  }

  /** How many accounts the filter matches. */
  async countUsers(filter: Filter): Promise<number> {
    const db = this.pool();
    const rows = await db.sql<{ count: number }[]>`
      SELECT count(*) FROM users
      WHERE (${filter.search}::text = '' OR lower(email) LIKE '%' || lower(${filter.search}::text) || '%')
        AND (${filter.role}::text = '' OR role = ${filter.role}::text)
    `;
    return rows[0]?.count ?? 0;
  }

  /** What the accounts add up to. */
  async stats(): Promise<Stats> {
    const db = this.pool();
    const rows = await db.sql<UserStatsRow[]>`
      SELECT count(*) AS total,
             count(*) FILTER (WHERE role = 'admin') AS admins,
             count(*) FILTER (WHERE role = 'user') AS users,
             count(*) FILTER (WHERE email_verified_at IS NOT NULL) AS verified,
             count(*) FILTER (WHERE onboarded_at IS NOT NULL) AS onboarded,
             count(*) FILTER (WHERE google_subject IS NOT NULL) AS google_accounts,
             count(*) FILTER (WHERE created_at >= now() - interval '7 days') AS new_last_7_days
      FROM users
    `;
    const row = rows[0];
    if (!row) throw new Error("admin: user stats: no row");
    return {
      total: row.total,
      admins: row.admins,
      users: row.users,
      verified: row.verified,
      onboarded: row.onboarded,
      googleAccounts: row.google_accounts,
      newLast7Days: row.new_last_7_days,
    };
  }

  /** Reads one account. A missing id is ErrAccountNotFound. */
  async accountByID(id: string): Promise<Account> {
    const db = this.pool();
    const rows = await db.sql<UserRow[]>`
      SELECT id, email, password_hash, email_verified_at, google_subject, created_at, updated_at, onboarded_at, role FROM users WHERE id = ${id}
    `;
    const row = rows[0];
    if (!row) throw new AdminError(ErrAccountNotFound.kind, `${ErrAccountNotFound.message}: ${id}`);
    return toAccount(row);
  }

  /** Makes an account a platform admin. */
  async promote(id: string): Promise<Account> {
    const db = this.pool();
    const rows = await db.sql<UserRow[]>`
      UPDATE users
      SET role = 'admin', updated_at = now()
      WHERE id = ${id}
      RETURNING id, email, password_hash, email_verified_at, google_subject, created_at, updated_at, onboarded_at, role
    `;
    const row = rows[0];
    if (!row) throw new AdminError(ErrAccountNotFound.kind, `${ErrAccountNotFound.message}: ${id}`);
    return toAccount(row);
  }

  /**
   * Takes the platform role away. The statement refuses the last admin, so no
   * rows here is that refusal rather than a missing account: the service reads
   * the account before it calls this, and the statement's own conditions are the
   * two a concurrent demotion would slip between.
   */
  async demote(id: string): Promise<Account> {
    const db = this.pool();
    const rows = await db.sql<UserRow[]>`
      UPDATE users
      SET role = 'user', updated_at = now()
      WHERE users.id = ${id}
        AND users.role = 'admin'
        AND (SELECT count(*) FROM users AS admins WHERE admins.role = 'admin') > 1
      RETURNING id, email, password_hash, email_verified_at, google_subject, created_at, updated_at, onboarded_at, role
    `;
    const row = rows[0];
    if (!row) throw new AdminError(ErrLastAdmin.kind, `${ErrLastAdmin.message}: ${id}`);
    return toAccount(row);
  }

  /** Per account id, how many workspaces it belongs to. */
  async workspaceCounts(ids: string[]): Promise<Record<string, number>> {
    const counts: Record<string, number> = {};
    if (ids.length === 0) return counts;

    const db = this.pool();
    const rows = await db.sql<WorkspaceCountRow[]>`
      SELECT m.user_id, count(*) AS workspace_count
      FROM members m
      WHERE m.user_id = ANY(${ids}::uuid[])
      GROUP BY m.user_id
    `;
    for (const row of rows) counts[row.user_id] = row.workspace_count;
    return counts;
  }
}

/** Row → domain account. */
export function toAccount(row: UserRow): Account {
  return {
    id: row.id,
    email: row.email,
    role: row.role,
    emailVerified: row.email_verified_at !== null,
    onboarded: row.onboarded_at !== null,
    hasGoogle: row.google_subject !== null && row.google_subject !== "",
    hasPassword: row.password_hash !== "",
    createdAt: row.created_at,
    workspaces: 0,
  };
}
