/**
 * Pure text gate for `src/migrations/` (#672).
 *
 * Production runs `apps/backend-js` without executing Postgres migrations in CI,
 * so a truncated statement, a dropped tenant table in the bootstrap RLS loop, or
 * a permissive `USING (true)` policy would otherwise pass `bun test` unnoticed.
 */

import { readdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "bun:test";

const MIGRATIONS_DIR = dirname(fileURLToPath(import.meta.url));
const INIT_MIGRATION = "000001_init.up.sql";

/**
 * Tables that carry a `workspace_id` column by design WITHOUT Row Level Security.
 *
 * Four are deployment-wide routing/sweep queues read before any tenant scope can
 * be opened (`whatsapp_directory`, `billing_directory`, `billing_payment_references`,
 * `retention_targets`), and `data_erasures` is an operator audit log written after
 * the workspace row itself has been deleted. Any new unscoped table carrying
 * `workspace_id` must be explicitly justified and registered here.
 */
const UNSCOPED_WORKSPACE_ID_ALLOWLIST = new Set<string>([
  "whatsapp_directory",
  "billing_directory",
  "billing_payment_references",
  "retention_targets",
  "data_erasures",
]);

/** Valid PostgreSQL top-level statement starters used in schema migrations. */
const VALID_STATEMENT_STARTERS = new Set<string>([
  "CREATE",
  "ALTER",
  "DROP",
  "INSERT",
  "UPDATE",
  "DELETE",
  "WITH",
  "DO",
  "COMMENT",
  "SELECT",
  "TRUNCATE",
  "SET",
  "GRANT",
  "REVOKE",
]);

/** Loads all `.sql` files from `src/migrations/` into an in-memory map. */
export function loadMigrationFilesFromDisk(dir: string = MIGRATIONS_DIR): Record<string, string> {
  const files: Record<string, string> = {};
  for (const entry of readdirSync(dir).sort()) {
    if (entry.endsWith(".sql")) {
      files[entry] = readFileSync(join(dir, entry), "utf8");
    }
  }
  return files;
}

/**
 * Strips SQL line (`-- ...`) and block (`/* ... *\/`) comments while preserving
 * single-quoted and dollar-quoted string literals so trailing comments after the
 * final `;` do not masquerade as missing semicolons, and comments mentioning SQL
 * fragments do not trigger false positives.
 */
function stripSqlComments(sql: string, stripInsideDollarQuotes: boolean = false): string {
  let out = "";
  let i = 0;
  const n = sql.length;

  while (i < n) {
    if (sql[i] === "-" && sql[i + 1] === "-") {
      while (i < n && sql[i] !== "\n") i++;
      continue;
    }
    if (sql[i] === "/" && sql[i + 1] === "*") {
      i += 2;
      while (i < n && !(sql[i] === "*" && sql[i + 1] === "/")) i++;
      i += 2;
      out += " ";
      continue;
    }
    if (sql[i] === "'") {
      const start = i++;
      while (i < n) {
        if (sql[i] === "'" && sql[i + 1] === "'") {
          i += 2;
        } else if (sql[i] === "'") {
          i++;
          break;
        } else {
          i++;
        }
      }
      out += sql.slice(start, i);
      continue;
    }
    if (sql[i] === "$") {
      const tagMatch = /^\$([A-Za-z0-9_]*)\$/.exec(sql.slice(i));
      if (tagMatch) {
        const tag = tagMatch[0];
        const endIdx = sql.indexOf(tag, i + tag.length);
        if (endIdx === -1) {
          out += sql.slice(i);
          break;
        }
        const inner = sql.slice(i + tag.length, endIdx);
        out += tag + (stripInsideDollarQuotes ? stripSqlComments(inner, false) : inner) + tag;
        i = endIdx + tag.length;
        continue;
      }
    }
    out += sql[i++];
  }

  return out;
}

/**
 * Splits a SQL script into top-level semicolon-terminated statements while
 * respecting comments, single quotes, dollar-quoted `DO $$ ... $$` bodies, and
 * parenthesised blocks.
 */
function splitTopLevelStatements(sql: string): {
  statements: string[];
  trailing: string;
  unterminatedError: string | null;
} {
  const statements: string[] = [];
  let i = 0;
  let start = 0;
  let parenDepth = 0;
  const n = sql.length;

  while (i < n) {
    if (sql[i] === "-" && sql[i + 1] === "-") {
      while (i < n && sql[i] !== "\n") i++;
      continue;
    }
    if (sql[i] === "/" && sql[i + 1] === "*") {
      i += 2;
      while (i < n && !(sql[i] === "*" && sql[i + 1] === "/")) {
        if (i + 1 >= n) {
          return { statements, trailing: "", unterminatedError: "komentar blok /* tidak ditutup" };
        }
        i++;
      }
      i += 2;
      continue;
    }
    if (sql[i] === "'") {
      i++;
      let closed = false;
      while (i < n) {
        if (sql[i] === "'" && sql[i + 1] === "'") {
          i += 2;
        } else if (sql[i] === "'") {
          i++;
          closed = true;
          break;
        } else {
          i++;
        }
      }
      if (!closed) {
        return { statements, trailing: "", unterminatedError: "literal string '...' tidak ditutup" };
      }
      continue;
    }
    if (sql[i] === "$") {
      const tagMatch = /^\$([A-Za-z0-9_]*)\$/.exec(sql.slice(i));
      if (tagMatch) {
        const tag = tagMatch[0];
        const endIdx = sql.indexOf(tag, i + tag.length);
        if (endIdx === -1) {
          return { statements, trailing: "", unterminatedError: `blok dollar-quote ${tag} tidak ditutup` };
        }
        i = endIdx + tag.length;
        continue;
      }
    }
    if (sql[i] === "(") {
      parenDepth++;
      i++;
      continue;
    }
    if (sql[i] === ")") {
      parenDepth--;
      if (parenDepth < 0) {
        return { statements, trailing: "", unterminatedError: "kurung tutup ')' tanpa pasangan" };
      }
      i++;
      continue;
    }
    if (sql[i] === ";" && parenDepth === 0) {
      statements.push(sql.slice(start, i + 1));
      start = i + 1;
    }
    i++;
  }

  if (parenDepth !== 0) {
    return { statements, trailing: "", unterminatedError: "kurung buka '(' tidak ditutup" };
  }

  return { statements, trailing: sql.slice(start), unterminatedError: null };
}

/** Extracts the table list inside `FOREACH ... IN ARRAY ARRAY[...]` in `000001_init.up.sql`. */
function extractInitRlsLoopTables(sql: string): { foundLoop: boolean; tables: string[] } {
  const clean = stripSqlComments(sql, true);
  const match = /FOREACH\s+\w+\s+IN\s+ARRAY\s+ARRAY\s*\[([\s\S]*?)\]/i.exec(clean);
  if (!match) {
    return { foundLoop: false, tables: [] };
  }
  const tables: string[] = [];
  for (const item of match[1].matchAll(/'([^']+)'/g)) {
    tables.push(item[1]);
  }
  return { foundLoop: true, tables };
}

/** Detects tables declared or altered to carry a `workspace_id` column in a single `.up.sql` file. */
function extractWorkspaceIdTablesInFile(statements: string[]): string[] {
  const tables: string[] = [];

  for (const rawStmt of statements) {
    const stmt = stripSqlComments(rawStmt, false).trim();

    const createMatch = /^CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*\(([\s\S]*)\)\s*;$/i.exec(
      stmt,
    );
    if (createMatch) {
      const [, tableName, body] = createMatch;
      if (/(?:^|,)\s*workspace_id\s+[A-Za-z]/im.test(body)) {
        tables.push(tableName);
      }
      continue;
    }

    const alterMatch = /^ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?([A-Za-z_][A-Za-z0-9_]*)\b([\s\S]*);$/i.exec(
      stmt,
    );
    if (alterMatch) {
      const [, tableName, rest] = alterMatch;
      if (/\bADD\s+(?:COLUMN\s+)?(?:IF\s+NOT\s+EXISTS\s+)?workspace_id\b/i.test(rest)) {
        tables.push(tableName);
      }
    }
  }

  return tables;
}

/** Extracts tables that receive `ENABLE ROW LEVEL SECURITY`, `FORCE ROW LEVEL SECURITY`, and `CREATE POLICY`. */
function extractExplicitRlsInFile(sql: string): {
  enabled: Set<string>;
  forced: Set<string>;
  policies: Set<string>;
} {
  const clean = stripSqlComments(sql, true);
  const enabled = new Set<string>();
  const forced = new Set<string>();
  const policies = new Set<string>();

  for (const m of clean.matchAll(
    /ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?([A-Za-z_][A-Za-z0-9_]*)\s+ENABLE\s+ROW\s+LEVEL\s+SECURITY\b/gi,
  )) {
    enabled.add(m[1]);
  }
  for (const m of clean.matchAll(
    /ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?([A-Za-z_][A-Za-z0-9_]*)\s+FORCE\s+ROW\s+LEVEL\s+SECURITY\b/gi,
  )) {
    forced.add(m[1]);
  }
  for (const m of clean.matchAll(
    /CREATE\s+POLICY\s+[A-Za-z_][A-Za-z0-9_]*\s+ON\s+([A-Za-z_][A-Za-z0-9_]*)\b/gi,
  )) {
    policies.add(m[1]);
  }

  return { enabled, forced, policies };
}

/**
 * Finds `USING (true)` or `WITH CHECK (true)` (case- and whitespace-insensitive,
 * including optional parenthesised/cast variants like `(true)` or `((true))`) and
 * attributes each hit to the policy's target table (or dynamic RLS loop tables).
 */
function detectWeakRlsPolicies(fileName: string, sql: string, loopTables: string[]): string[] {
  const errors: string[] = [];
  const clean = stripSqlComments(sql, true);
  const weakClauseRe = /\b(USING|WITH\s+CHECK)\s*\(\s*\(*\s*true\s*\)*\s*\)/gi;

  // 1. Static `CREATE POLICY <name> ON <table> ...;` statements.
  for (const policyMatch of clean.matchAll(
    /CREATE\s+POLICY\s+([A-Za-z_][A-Za-z0-9_]*)\s+ON\s+([A-Za-z_][A-Za-z0-9_]*)([\s\S]*?);/gi,
  )) {
    const [, policyName, tableName, body] = policyMatch;
    for (const weak of body.matchAll(weakClauseRe)) {
      const clause = weak[1].replace(/\s+/g, " ").toUpperCase();
      errors.push(
        `${fileName}: policy RLS lemah '${clause} (true)' ditolak pada tabel '${tableName}' (policy '${policyName}')`,
      );
    }
  }

  // 2. Dynamic `CREATE POLICY` inside `DO $$ ... EXECUTE format('CREATE POLICY ...') $$`.
  for (const dynMatch of clean.matchAll(
    /EXECUTE\s+format\s*\(\s*'([^']*CREATE\s+POLICY[^']*)'([\s\S]*?)\)\s*;/gi,
  )) {
    const [, formatTemplate, formatArgs] = dynMatch;
    for (const weak of formatTemplate.matchAll(weakClauseRe)) {
      const clause = weak[1].replace(/\s+/g, " ").toUpperCase();
      const targetDesc =
        loopTables.length > 0
          ? `loop RLS tabel [${loopTables.join(", ")}]`
          : formatArgs.trim() || "dynamic table";
      errors.push(`${fileName}: policy RLS lemah '${clause} (true)' ditolak pada ${targetDesc}`);
    }
  }

  // 3. Fallback if `USING (true)` / `WITH CHECK (true)` appears outside standard forms.
  if (errors.length === 0) {
    for (const weak of clean.matchAll(weakClauseRe)) {
      const clause = weak[1].replace(/\s+/g, " ").toUpperCase();
      errors.push(`${fileName}: policy RLS lemah '${clause} (true)' ditolak pada tabel tidak dikenal`);
    }
  }

  return errors;
}

/**
 * Validates the complete set of migration files in memory and returns every
 * violation found. An empty array means the migration tree passes all gates.
 */
export function checkMigrations(files: Record<string, string>): string[] {
  const errors: string[] = [];
  const fileNames = Object.keys(files).sort();

  if (fileNames.length === 0) {
    return ["Tidak ada berkas migrasi ditemukan"];
  }

  interface ParsedEntry {
    versionRaw: string;
    version: number;
    name: string;
    direction: "up" | "down";
    fileName: string;
  }

  const upByVersion = new Map<string, ParsedEntry[]>();
  const downByVersion = new Map<string, ParsedEntry[]>();
  const allVersions = new Set<string>();

  for (const fileName of fileNames) {
    const match = /^(\d{6})_([a-z0-9_]+)\.(up|down)\.sql$/.exec(fileName);
    if (!match) {
      errors.push(
        `${fileName}: nama berkas migrasi tidak sesuai pola NNNNNN_<name>.(up|down).sql`,
      );
      continue;
    }

    const [, versionRaw, name, direction] = match as unknown as [string, string, string, "up" | "down"];
    const entry: ParsedEntry = {
      versionRaw,
      version: Number(versionRaw),
      name,
      direction,
      fileName,
    };

    allVersions.add(versionRaw);
    const bucket = direction === "up" ? upByVersion : downByVersion;
    const list = bucket.get(versionRaw) ?? [];
    list.push(entry);
    bucket.set(versionRaw, list);
  }

  // 1 & 2: Pairing (.up.sql <-> .down.sql), no duplicate versions, sequential 000001..N without gaps.
  const sortedVersionKeys = [...allVersions].sort((a, b) => Number(a) - Number(b));

  for (const vKey of sortedVersionKeys) {
    const ups = upByVersion.get(vKey) ?? [];
    const downs = downByVersion.get(vKey) ?? [];

    if (ups.length > 1) {
      errors.push(
        `Versi ${vKey} duplikat pada berkas .up.sql: ${ups.map((u) => u.fileName).join(", ")}`,
      );
    }
    if (downs.length > 1) {
      errors.push(
        `Versi ${vKey} duplikat pada berkas .down.sql: ${downs.map((d) => d.fileName).join(", ")}`,
      );
    }
    if (ups.length === 1 && downs.length === 0) {
      errors.push(`${ups[0].fileName}: tidak punya pasangan ${vKey}_${ups[0].name}.down.sql`);
    }
    if (downs.length === 1 && ups.length === 0) {
      errors.push(`${downs[0].fileName}: .down.sql tanpa pasangan ${vKey}_${downs[0].name}.up.sql`);
    }
    if (ups.length === 1 && downs.length === 1 && ups[0].name !== downs[0].name) {
      errors.push(
        `Versi ${vKey}: nama .up.sql (${ups[0].fileName}) dan .down.sql (${downs[0].fileName}) berbeda`,
      );
    }
  }

  for (let i = 0; i < sortedVersionKeys.length; i++) {
    const expected = String(i + 1).padStart(6, "0");
    const actual = sortedVersionKeys[i];
    if (actual !== expected) {
      errors.push(
        `Urutan versi migrasi melompat atau tidak mulai dari 000001: mengharapkan ${expected}, menemukan ${actual}`,
      );
      break;
    }
  }

  // 3, 4 & 5: Non-empty `.up.sql` ending in `;`, valid top-level SQL statements, RLS coverage, and no `USING (true)`.
  const initSql = files[INIT_MIGRATION];
  let initLoopTables: string[] = [];
  if (initSql === undefined) {
    errors.push(`Berkas bootstrap ${INIT_MIGRATION} tidak ditemukan`);
  } else {
    const parsedLoop = extractInitRlsLoopTables(initSql);
    if (!parsedLoop.foundLoop || parsedLoop.tables.length === 0) {
      errors.push(`${INIT_MIGRATION}: loop RLS ARRAY[...] tidak ditemukan atau kosong`);
    }
    initLoopTables = parsedLoop.tables;
  }
  const initLoopSet = new Set(initLoopTables);

  const workspaceIdTables = new Map<string, string>(); // table -> defining file
  const rlsEnabledTables = new Set<string>(initLoopTables);
  const rlsForcedTables = new Set<string>(initLoopTables);
  const rlsPolicyTables = new Set<string>(initLoopTables);

  const upFiles = fileNames.filter((f) => f.endsWith(".up.sql")).sort();
  for (const fileName of upFiles) {
    const raw = files[fileName] ?? "";
    if (raw.trim().length === 0) {
      errors.push(`${fileName}: berkas .up.sql kosong`);
      continue;
    }

    const strippedForTail = stripSqlComments(raw, false).trim();
    if (strippedForTail.length === 0) {
      errors.push(`${fileName}: berkas .up.sql hanya berisi komentar tanpa pernyataan SQL`);
      continue;
    }
    if (!strippedForTail.endsWith(";")) {
      errors.push(`${fileName}: berkas .up.sql tidak berakhir dengan ';' (SQL terpotong)`);
    }

    const { statements, trailing, unterminatedError } = splitTopLevelStatements(raw);
    if (unterminatedError !== null) {
      errors.push(`${fileName}: SQL rusak (${unterminatedError})`);
      continue;
    }

    const trailingClean = stripSqlComments(trailing, false).trim();
    if (trailingClean.length > 0) {
      errors.push(`${fileName}: pernyataan SQL terakhir tidak ditutup dengan ';'`);
    }

    for (const stmt of statements) {
      const cleanStmt = stripSqlComments(stmt, false).trim();
      if (cleanStmt === ";") {
        errors.push(`${fileName}: ditemukan pernyataan SQL kosong ';'`);
        continue;
      }
      const starter = /^([A-Za-z_]+)/.exec(cleanStmt)?.[1]?.toUpperCase();
      if (!starter || !VALID_STATEMENT_STARTERS.has(starter)) {
        const preview = cleanStmt.replace(/\s+/g, " ").slice(0, 60);
        errors.push(`${fileName}: pernyataan SQL tingkat atas tidak valid: "${preview}"`);
      }
    }

    const declaredWorkspaceTables = extractWorkspaceIdTablesInFile(statements);
    for (const table of declaredWorkspaceTables) {
      if (!workspaceIdTables.has(table)) {
        workspaceIdTables.set(table, fileName);
      }
      if (fileName === INIT_MIGRATION && !initLoopSet.has(table)) {
        errors.push(
          `${INIT_MIGRATION}: tabel '${table}' punya kolom workspace_id tetapi tidak ada di ARRAY[...] loop RLS`,
        );
      }
    }

    const explicitRls = extractExplicitRlsInFile(raw);
    for (const t of explicitRls.enabled) rlsEnabledTables.add(t);
    for (const t of explicitRls.forced) rlsForcedTables.add(t);
    for (const t of explicitRls.policies) rlsPolicyTables.add(t);

    const fileLoopTables = fileName === INIT_MIGRATION ? initLoopTables : [];
    errors.push(...detectWeakRlsPolicies(fileName, raw, fileLoopTables));
  }

  // Every `workspace_id` table across all migrations must either be in 000001's loop,
  // carry explicit ENABLE + FORCE + CREATE POLICY in its migration, or be in the
  // documented unscoped routing/erasure allowlist.
  for (const [table, definedIn] of workspaceIdTables.entries()) {
    if (UNSCOPED_WORKSPACE_ID_ALLOWLIST.has(table)) continue;
    if (initLoopSet.has(table)) continue;

    const missingParts: string[] = [];
    if (!rlsEnabledTables.has(table)) missingParts.push("ENABLE ROW LEVEL SECURITY");
    if (!rlsForcedTables.has(table)) missingParts.push("FORCE ROW LEVEL SECURITY");
    if (!rlsPolicyTables.has(table)) missingParts.push("CREATE POLICY");

    if (missingParts.length > 0) {
      errors.push(
        `${definedIn}: tabel '${table}' punya kolom workspace_id tetapi tidak ada di loop RLS ${INIT_MIGRATION} maupun kebijakan RLS lengkap (kurang: ${missingParts.join(", ")})`,
      );
    }
  }

  // Guard against dead names left in the 000001 loop that have no `workspace_id` table.
  for (const loopTable of initLoopTables) {
    if (!workspaceIdTables.has(loopTable)) {
      errors.push(
        `${INIT_MIGRATION}: tabel '${loopTable}' ada di ARRAY[...] loop RLS tetapi tidak ditemukan definisi tabel berkolom workspace_id`,
      );
    }
  }

  return errors;
}

describe("SQL migrations gate (#672)", () => {
  const diskFiles = loadMigrationFilesFromDisk();

  test("seluruh berkas migrasi di disk lolos pemeriksaan pasangan, urutan, sintaks akhir, dan RLS", () => {
    const errors = checkMigrations(diskFiles);
    expect(errors).toEqual([]);
  });

  test("setiap NNNNNN_<name>.up.sql punya pasangan .down.sql dan tidak ada .down.sql yatim", () => {
    const withoutDown = { ...diskFiles };
    delete withoutDown["000015_whatsapp.down.sql"];
    const errMissingDown = checkMigrations(withoutDown);
    expect(errMissingDown.some((e) => e.includes("000015_whatsapp.up.sql"))).toBe(true);

    const orphanDown = {
      ...diskFiles,
      "000026_orphan.down.sql": "DROP TABLE IF EXISTS orphan;",
    };
    const errOrphanDown = checkMigrations(orphanDown);
    expect(errOrphanDown.some((e) => e.includes("000026_orphan.down.sql"))).toBe(true);
  });

  test("nomor versi wajib berurutan mulai 000001 tanpa lompatan dan tanpa duplikat", () => {
    const withGap = {
      ...diskFiles,
      "000027_skipped.up.sql": "SELECT 1;",
      "000027_skipped.down.sql": "SELECT 1;",
    };
    const errGap = checkMigrations(withGap);
    expect(errGap.some((e) => e.includes("000026") && e.includes("000027"))).toBe(true);

    const withoutFirst = { ...diskFiles };
    delete withoutFirst["000001_init.up.sql"];
    delete withoutFirst["000001_init.down.sql"];
    const errNoFirst = checkMigrations(withoutFirst);
    expect(errNoFirst.some((e) => e.includes("000001"))).toBe(true);

    const withDuplicate = {
      ...diskFiles,
      "000025_duplicate_name.up.sql": "SELECT 1;",
      "000025_duplicate_name.down.sql": "SELECT 1;",
    };
    const errDup = checkMigrations(withDuplicate);
    expect(errDup.some((e) => e.includes("000025") && e.includes("duplikat"))).toBe(true);
  });

  test("berkas .up.sql wajib non-kosong dan berakhir dengan ';'", () => {
    const emptyUp = {
      ...diskFiles,
      "000025_slack_github_connectors.up.sql": "   \n\t  ",
    };
    expect(
      checkMigrations(emptyUp).some((e) => e.includes("000025_slack_github_connectors.up.sql")),
    ).toBe(true);

    const truncatedUp = {
      ...diskFiles,
      "000025_slack_github_connectors.up.sql": "ALTER TABLE users ADD COLUMN broken TEXT",
    };
    expect(
      checkMigrations(truncatedUp).some(
        (e) => e.includes("000025_slack_github_connectors.up.sql") && e.includes("';'"),
      ),
    ).toBe(true);
  });

  test("regresi (a): menambahkan 'THIS IS NOT SQL AT ALL;' ke akhir 000001_init.up.sql memerahkan gate", () => {
    const mutated = {
      ...diskFiles,
      [INIT_MIGRATION]: `${diskFiles[INIT_MIGRATION]}\nTHIS IS NOT SQL AT ALL;\n`,
    };
    const errors = checkMigrations(mutated);
    expect(errors.length).toBeGreaterThan(0);
    expect(
      errors.some(
        (e) => e.includes(INIT_MIGRATION) && e.includes("THIS IS NOT SQL AT ALL"),
      ),
    ).toBe(true);
  });

  test("regresi (b): menghapus satu tabel dari array loop RLS di 000001_init.up.sql memerahkan penjaga RLS", () => {
    const initWithoutInvoices = diskFiles[INIT_MIGRATION].replace("'billing_invoices', ", "");
    expect(initWithoutInvoices).not.toBe(diskFiles[INIT_MIGRATION]);

    const mutated = {
      ...diskFiles,
      [INIT_MIGRATION]: initWithoutInvoices,
    };
    const errors = checkMigrations(mutated);
    expect(errors.length).toBeGreaterThan(0);
    expect(
      errors.some(
        (e) =>
          e.includes(INIT_MIGRATION) &&
          e.includes("billing_invoices") &&
          e.includes("loop RLS"),
      ),
    ).toBe(true);
  });

  test("menambah tabel baru berkolom workspace_id tanpa RLS gagal di penjaga RLS", () => {
    const mutated = {
      ...diskFiles,
      "000026_unprotected_tenant_table.up.sql": `
        CREATE TABLE unprotected_notes (
            id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
            workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
            body TEXT NOT NULL
        );
      `,
      "000026_unprotected_tenant_table.down.sql": "DROP TABLE IF EXISTS unprotected_notes;",
    };
    const errors = checkMigrations(mutated);
    expect(
      errors.some(
        (e) =>
          e.includes("000026_unprotected_tenant_table.up.sql") &&
          e.includes("unprotected_notes"),
      ),
    ).toBe(true);
  });

  test("pelemahan RLS 'USING (true)' atau 'WITH CHECK (true)' ditolak dengan menyebut berkas dan tabel", () => {
    // Mutation 1 from issue #672: weakening the bootstrap loop policy in 000001_init.up.sql.
    const weakenedBootstrap = diskFiles[INIT_MIGRATION].replace(
      "USING (workspace_id = app_current_workspace_id()) WITH CHECK (workspace_id = app_current_workspace_id())",
      "USING ( true ) WITH CHECK (\n  true\n)",
    );
    expect(weakenedBootstrap).not.toBe(diskFiles[INIT_MIGRATION]);

    const bootstrapErrors = checkMigrations({
      ...diskFiles,
      [INIT_MIGRATION]: weakenedBootstrap,
    });
    expect(
      bootstrapErrors.some(
        (e) =>
          e.includes(INIT_MIGRATION) &&
          e.includes("USING (true)") &&
          e.includes("members"),
      ),
    ).toBe(true);
    expect(
      bootstrapErrors.some(
        (e) =>
          e.includes(INIT_MIGRATION) &&
          e.includes("WITH CHECK (true)") &&
          e.includes("members"),
      ),
    ).toBe(true);

    // Weakening an explicit per-table policy in a later migration.
    const ordersFile = "000021_orders.up.sql";
    const weakenedOrders = diskFiles[ordersFile].replace(
      "CREATE POLICY orders_tenant_isolation ON orders\n    USING (workspace_id = app_current_workspace_id())\n    WITH CHECK (workspace_id = app_current_workspace_id());",
      "CREATE POLICY orders_tenant_isolation ON orders\n    USING (   true   )\n    WITH CHECK (workspace_id = app_current_workspace_id());",
    );
    expect(weakenedOrders).not.toBe(diskFiles[ordersFile]);

    const ordersErrors = checkMigrations({
      ...diskFiles,
      [ordersFile]: weakenedOrders,
    });
    expect(
      ordersErrors.some(
        (e) =>
          e.includes(ordersFile) &&
          e.includes("orders") &&
          e.includes("USING (true)"),
      ),
    ).toBe(true);
  });
});
