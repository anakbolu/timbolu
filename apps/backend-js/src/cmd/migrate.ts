/**
 * Applies, reverts, or reports the schema version using the migrations in
 * `src/migrations/`.
 *
 * Ported from apps/backend/internal/platform/migrate/migrate.go and
 * apps/backend/tools/migrate/main.go. It replaces golang-migrate with the
 * behaviour this repository relies on:
 *
 *   - the version lives in `schema_migrations (version bigint primary key,
 *     dirty boolean not null)`, the table golang-migrate creates, so the Go and
 *     TypeScript runners agree on what is applied;
 *   - every migration runs in one transaction together with its version row, so
 *     a failed statement leaves neither half applied;
 *   - a dirty row is refused rather than guessed at, exactly as golang-migrate
 *     refuses `Up`/`Down` on a dirty database;
 *   - the advisory lock is the same one golang-migrate takes, so the two
 *     runners cannot migrate at the same time.
 *
 * Usage: bun run src/cmd/migrate.ts [up|down|version] [--down <n>] [--to <v>] [--url <url>]
 */

import { readdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import postgres from "postgres";
import type { Sql } from "../platform/database/index.ts";

/** Where the `.sql` files live, resolved from this file rather than the cwd. */
const MIGRATIONS_DIR = join(dirname(fileURLToPath(import.meta.url)), "..", "migrations");

/** The table golang-migrate creates and reads. */
const MIGRATIONS_TABLE = "schema_migrations";

/** `database.NilVersion`: no migration has been applied. */
const NIL_VERSION = -1;

/** golang-migrate's advisory-lock salt. */
const ADVISORY_LOCK_SALT = 1486364155;

/** One `.up.sql` / `.down.sql` pair. */
interface Migration {
  version: number;
  name: string;
  up: string;
  down: string | null;
}

/** The applied version, as the database records it. */
interface Version {
  version: number;
  dirty: boolean;
}

/** The command line, after parsing. */
interface Command {
  action: "up" | "down" | "version";
  url: string;
  downSteps: number | null;
  to: number | null;
}

function usageError(message: string): never {
  throw new Error(`${message}: use up, down, version, --down <n>, or --to <version>`);
}

function parseArgs(argv: string[]): Command {
  let url = "";
  let action: "up" | "down" | "version" | null = null;
  let downSteps: number | null = null;
  let to: number | null = null;

  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    const [flag, inline] = arg.startsWith("--") || arg.startsWith("-") ? arg.split("=", 2) : [arg, undefined];
    const value = (): string => {
      const next = inline ?? argv[++i];
      if (next === undefined) usageError(`${flag} needs a value`);
      return next;
    };

    switch (flag) {
      case "-url":
      case "--url":
        url = value();
        break;
      case "--down":
      case "-down": {
        const text = value();
        const n = Number(text);
        if (!Number.isInteger(n) || n <= 0) throw new Error(`--down needs a positive number, got ${JSON.stringify(text)}`);
        downSteps = n;
        action ??= "down";
        break;
      }
      case "--to":
      case "-to": {
        const text = value();
        const n = Number(text);
        if (!Number.isInteger(n) || n < 0) throw new Error(`--to needs a version number, got ${JSON.stringify(text)}`);
        to = n;
        break;
      }
      case "up":
      case "down":
      case "version":
        if (action !== null && action !== flag) throw usageError(`conflicting commands ${action} and ${flag}`);
        action = flag;
        break;
      default:
        throw usageError(`unknown argument ${JSON.stringify(arg)}`);
    }
  }

  return {
    action: action ?? "up",
    url,
    downSteps,
    to,
  };
}

/** Reads the migration files, keyed by version and ordered. */
function loadMigrations(): Migration[] {
  const byVersion = new Map<number, Migration>();

  for (const file of readdirSync(MIGRATIONS_DIR)) {
    const match = /^(\d+)_(.+)\.(up|down)\.sql$/.exec(file);
    if (!match) continue;

    const version = Number(match[1]);
    const name = match[2];
    const direction = match[3];
    const text = readFileSync(join(MIGRATIONS_DIR, file), "utf8");

    const existing = byVersion.get(version) ?? { version, name, up: "", down: null };
    if (direction === "up") existing.up = text;
    else existing.down = text;
    byVersion.set(version, existing);
  }

  return [...byVersion.values()].sort((a, b) => a.version - b.version);
}

/** Reads the applied version; `NIL_VERSION` when nothing has been applied. */
async function currentVersion(sql: Sql): Promise<Version> {
  const rows = await sql<{ version: number; dirty: boolean }[]>`
    SELECT version, dirty FROM schema_migrations LIMIT 1
  `;
  const row = rows[0];
  if (!row) return { version: NIL_VERSION, dirty: false };
  return { version: Number(row.version), dirty: Boolean(row.dirty) };
}

/** Creates the table if it is absent, as golang-migrate does. */
async function ensureTable(sql: Sql): Promise<void> {
  await sql.unsafe(
    `CREATE TABLE IF NOT EXISTS ${MIGRATIONS_TABLE} (version bigint not null primary key, dirty boolean not null)`,
  );
}

/** Records one applied version, replacing the single row. */
async function setVersion(sql: Sql, version: number): Promise<void> {
  await sql`TRUNCATE ${sql(MIGRATIONS_TABLE)}`;
  if (version >= 0) {
    await sql`INSERT INTO ${sql(MIGRATIONS_TABLE)} (version, dirty) VALUES (${version}, false)`;
  }
}

/**
 * golang-migrate's advisory lock id: crc32 of the schema, table and database
 * names joined with NULs, multiplied by the salt.
 */
function advisoryLockId(databaseName: string, schemaName: string, tableName: string): number {
  let crc = 0xffffffff;
  for (const byte of Buffer.from(`${schemaName}\x00${tableName}\x00${databaseName}`, "utf8")) {
    crc ^= byte;
    for (let bit = 0; bit < 8; bit++) {
      crc = crc & 1 ? (crc >>> 1) ^ 0xedb88320 : crc >>> 1;
    }
  }
  const sum = (crc ^ 0xffffffff) >>> 0;
  return Math.imul(sum, ADVISORY_LOCK_SALT) >>> 0;
}

/** Reports the applied version the way the Go tool does. */
async function report(sql: Sql): Promise<void> {
  const { version, dirty } = await currentVersion(sql);
  if (version === NIL_VERSION) {
    console.log("migrate: schema is empty");
    return;
  }
  console.log(`migrate: version ${version} (dirty=${dirty})`);
}

/** Runs one migration file and records its version in the same transaction. */
async function applyOne(sql: Sql, migration: Migration, direction: "up" | "down", target: number): Promise<void> {
  const body = direction === "up" ? migration.up : migration.down;
  if (body === null) {
    throw new Error(`no ${direction} migration found for version ${migration.version}`);
  }

  await sql.begin(async (tx) => {
    // `unsafe` without parameters uses the simple query protocol, so a file
    // with many statements and `DO $$ … $$` blocks runs as one script inside
    // this transaction rather than needing a splitter.
    await tx.unsafe(body);
    await tx`TRUNCATE ${tx(MIGRATIONS_TABLE)}`;
    if (target >= 0) {
      await tx`INSERT INTO ${tx(MIGRATIONS_TABLE)} (version, dirty) VALUES (${target}, false)`;
    }
  });
}

async function migrateUp(sql: Sql, migrations: Migration[], to: number | null): Promise<number> {
  const { version, dirty } = await currentVersion(sql);
  if (dirty) throw new Error(`migrate up: database is dirty at version ${version}`);

  const pending = migrations.filter(
    (m) => m.version > version && (to === null || m.version <= to),
  );
  if (to !== null && !migrations.some((m) => m.version === to)) {
    throw new Error(`migrate up: no migration found for version ${to}`);
  }
  if (pending.length === 0) {
    console.log("migrate: no pending migrations");
    return version;
  }

  for (const migration of pending) {
    try {
      await applyOne(sql, migration, "up", migration.version);
    } catch (err) {
      throw new Error(`migrate up: ${migration.version}_${migration.name}: ${(err as Error).message}`);
    }
    console.log(`migrate: applied ${migration.version}_${migration.name}`);
  }
  return pending[pending.length - 1]?.version ?? version;
}

async function migrateDown(
  sql: Sql,
  migrations: Migration[],
  steps: number | null,
  to: number | null,
): Promise<number> {
  const { version, dirty } = await currentVersion(sql);
  if (dirty) throw new Error(`migrate down: database is dirty at version ${version}`);
  if (version === NIL_VERSION) {
    console.log("migrate: no migrations to revert");
    return version;
  }
  if (to !== null && !migrations.some((m) => m.version === to)) {
    throw new Error(`migrate down: no migration found for version ${to}`);
  }

  const applied = migrations.filter((m) => m.version <= version);
  const selected: Migration[] = [];
  for (let i = applied.length - 1; i >= 0; i--) {
    const migration = applied[i];
    if (to !== null ? migration.version <= to : steps !== null && selected.length >= steps) break;
    selected.push(migration);
  }
  if (steps !== null) selected.length = Math.min(selected.length, steps);

  if (selected.length === 0) {
    console.log("migrate: no migrations to revert");
    return version;
  }

  let target = version;
  for (const migration of selected) {
    // The version after reverting this one is the highest remaining applied
    // migration, or nil when none is left.
    const remaining = migrations.filter((m) => m.version < migration.version);
    target = remaining[remaining.length - 1]?.version ?? NIL_VERSION;
    try {
      await applyOne(sql, migration, "down", target);
    } catch (err) {
      throw new Error(`migrate down: ${migration.version}_${migration.name}: ${(err as Error).message}`);
    }
    console.log(`migrate: reverted ${migration.version}_${migration.name}`);
  }
  return target;
}

async function run(): Promise<void> {
  const command = parseArgs(process.argv.slice(2));
  const url = command.url !== "" ? command.url : (process.env.DATABASE_URL ?? "");
  if (url === "") {
    throw new Error("no database url: pass --url or set DATABASE_URL");
  }

  const migrations = loadMigrations();
  // max must be > 1: the advisory lock reserves one connection for the whole
  // run, and every statement below needs a second one. With max: 1 the pool
  // deadlocks waiting for the connection the lock is holding.
  const sql = postgres(url, { max: 4, connect_timeout: 10, onnotice: () => {} });

  try {
    const databaseName = decodeURIComponent(new URL(url).pathname.replace(/^\//, "")) || "postgres";
    const lockId = advisoryLockId(databaseName, "public", MIGRATIONS_TABLE);

    // A reserved connection holds the session lock, so every migration below is
    // serialised against the Go runner and against another copy of this one.
    // The lock is advisory and session-wide, so the pooled transactions that do
    // the work are still covered by it.
    const lock = await sql.reserve();
    try {
      await lock`SELECT pg_advisory_lock(${lockId})`;
      try {
        await ensureTable(sql);

        switch (command.action) {
          case "version":
            break;
          case "up": {
            const target = command.to ?? null;
            const before = (await currentVersion(sql)).version;
            const after = await migrateUp(sql, migrations, target);
            if (after !== before) console.log("migrate: up applied");
            break;
          }
          case "down": {
            await migrateDown(sql, migrations, command.downSteps, command.to ?? null);
            console.log("migrate: down reverted");
            break;
          }
        }

        await report(sql);
      } finally {
        await lock`SELECT pg_advisory_unlock(${lockId})`;
      }
    } finally {
      lock.release();
    }
  } finally {
    await sql.end({ timeout: 5 });
  }
}

run().catch((err: unknown) => {
  console.error(`migrate: ${(err as Error).message}`);
  process.exit(1);
});
