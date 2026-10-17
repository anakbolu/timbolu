/**
 * The health module data access: a real query against Postgres.
 *
 * Ported from internal/modules/health/repository/postgres.go and the folded
 * sqlc query in repository/sqlcgen (health.sql).
 */

import type { Database } from "../../../platform/database/index.ts";
import type { Repository as RepositoryPort } from "../domain/health.ts";

/** The row of the `Ping` query. */
export interface PingRow {
  ok: number;
}

/**
 * Repository is the Postgres-backed implementation of the domain contract.
 *
 * A null database is allowed: Ping then reports that the database is not
 * configured instead of panicking, which is what lets the API boot without
 * Postgres during local development.
 */
export class Repository implements RepositoryPort {
  private readonly db: Database | null;

  constructor(db: Database | null) {
    this.db = db;
  }

  /** Verifies the database answers a real query. */
  async ping(): Promise<void> {
    if (!this.db) {
      throw new Error("database is not configured");
    }

    try {
      await this.db.ping();
    } catch (err) {
      throw new Error(`ping database: ${(err as Error).message}`);
    }

    try {
      await ping(this.db);
    } catch (err) {
      throw new Error(`query database: ${(err as Error).message}`);
    }
  }
}

/** name: Ping :one — proves the database is reachable. */
export async function ping(db: Database): Promise<number> {
  const rows = await db.sql<PingRow[]>`
    SELECT 1::int AS ok
  `;
  return rows[0]?.ok ?? 0;
}
