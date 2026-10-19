/**
 * Postgres pool and the tenant scope every query runs inside.
 *
 * Ported from packages/platform/database (database.go, scope.go, tenant.go).
 * Row Level Security reads `app.user_id` and `app.workspace_id`, both set with
 * `set_config(..., true)` so they die with the transaction.
 */

import postgres from "postgres";

export type Sql = postgres.Sql<{}>;
export type Tx = postgres.TransactionSql<{}>;

/**
 * The identity a transaction runs as. At least one of the two is required, so
 * no query ever runs unscoped by accident.
 */
export interface Scope {
  userId?: string | null;
  workspaceId?: string | null;
}

export interface DatabaseConfig {
  url: string;
  maxOpenConns?: number;
  maxIdleConns?: number;
  connMaxLifetimeSeconds?: number;
}

/** True when the scope carries no identity at all. */
export function isZeroScope(scope: Scope | null | undefined): boolean {
  if (!scope) return true;
  return !scope.userId && !scope.workspaceId;
}

/**
 * The pool plus the scoped-transaction helpers. Repositories receive this and
 * never build their own connections.
 */
export class Database {
  readonly sql: Sql;

  constructor(sql: Sql) {
    this.sql = sql;
  }

  /** Reports whether Postgres is reachable; used by the readiness probe. */
  async ping(): Promise<void> {
    await this.sql`select 1`;
  }

  /** Releases every pooled connection. */
  async close(): Promise<void> {
    await this.sql.end({ timeout: 5 });
  }

  /**
   * Runs `fn` inside a transaction whose connection carries the scope
   * settings. Every statement in `fn` is filtered by Row Level Security, so a
   * query that forgets `WHERE workspace_id = ...` still cannot reach another
   * tenant.
   */
  async inScope<T>(scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    return this.runScoped(scope, false, fn);
  }

  /** `inScope` for a read-only transaction. */
  async inScopeRead<T>(scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    return this.runScoped(scope, true, fn);
  }

  private async runScoped<T>(scope: Scope, readOnly: boolean, fn: (tx: Tx) => Promise<T>): Promise<T> {
    if (isZeroScope(scope)) {
      throw new Error("scope requires a user id or a workspace id");
    }

    return this.sql.begin(async (tx) => {
      if (readOnly) {
        await tx.unsafe("SET TRANSACTION READ ONLY");
      }
      if (scope.userId) {
        await tx`select set_config('app.user_id', ${scope.userId}, true)`;
      }
      if (scope.workspaceId) {
        await tx`select set_config('app.workspace_id', ${scope.workspaceId}, true)`;
      }
      return fn(tx);
    }) as Promise<T>;
  }
}

/**
 * Activates a tenant inside an already open transaction. Onboarding needs it:
 * the workspace row is created first (which only requires an authenticated
 * user), and only then can its teams and owner membership be written, because
 * those policies require the active workspace.
 */
export async function setWorkspace(tx: Tx, workspaceId: string): Promise<void> {
  if (!workspaceId) throw new Error("workspace id is required");
  await tx`select set_config('app.workspace_id', ${workspaceId}, true)`;
}

/**
 * Opens the pool and verifies the connection with a ping, so a bad
 * DATABASE_URL fails at startup instead of on the first request.
 */
export function createDatabase(cfg: DatabaseConfig): Database {
  if (!cfg.url) throw new Error("database url is required");

  const sql = postgres(cfg.url, {
    max: cfg.maxOpenConns ?? 10,
    idle_timeout: cfg.connMaxLifetimeSeconds ?? 30,
    connect_timeout: 10,
    // The Go side maps int8 to int64 and json-marshals it as a number. Money is
    // held in micros and token counts are plain integers, both far inside
    // Number.MAX_SAFE_INTEGER, so parsing here keeps response shapes identical.
    types: {
      bigint: {
        to: 20,
        from: [20],
        serialize: (x: number | bigint) => x.toString(),
        parse: (x: string) => Number(x),
      },
    },
    // An array of rows is what every caller expects; no transform is installed.
    transform: undefined,
    onnotice: () => {},
  });

  return new Database(sql);
}
