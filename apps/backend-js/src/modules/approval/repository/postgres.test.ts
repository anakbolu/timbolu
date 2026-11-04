/**
 * Pure text gate for `src/modules/approval/repository/postgres.ts` (#722).
 *
 * Production runs `apps/backend-js` without executing repository SQL against a
 * live Postgres instance in `bun test`. In `getDraft` (and every other `drafts`
 * read/write), the `workspace_id = ${scope.workspaceId}` predicate in the `WHERE`
 * clause is the application-layer tenant isolation guard preventing a caller who
 * guesses a draft UUID from reading or mutating another workspace's draft.
 *
 * Why we parse per tagged-template SQL literal (`tx\`...\`` / `sql\`...\``) mapped
 * to its enclosing repository method and `-- name:` / `// name:` query label:
 * splitting on tagged-template boundaries isolates each individual SQL statement
 * executed by the driver regardless of TypeScript line wrapping, indentation, or
 * helper closures (`this.read`, `this.write`, `this.translateWrite`), while still
 * attributing any violation to both the repository method name and the sqlc query
 * name so a developer immediately sees which query lost its tenant filter.
 */

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "bun:test";

const REPOSITORY_DIR = dirname(fileURLToPath(import.meta.url));
export const POSTGRES_TS_PATH = join(REPOSITORY_DIR, "postgres.ts");

/**
 * Queries touching `drafts` that are explicitly allowed to omit a `WHERE`
 * `workspace_id` filter, mapped to the mandatory justification for why they are
 * safe across tenants.
 *
 * Today this allowlist is intentionally empty: every single query on `drafts`
 * is tenant-scoped and must filter by `workspace_id` in its own `WHERE` clause.
 * Following the `UNSCOPED_WORKSPACE_ID_ALLOWLIST` pattern in `migrations.test.ts`,
 * any future exception must be registered here with a non-empty reason rather
 * than silently bypassing the gate.
 */
export const UNSCOPED_DRAFTS_QUERY_ALLOWLIST: Record<string, string> = {};

/** One tagged-template SQL query extracted from `postgres.ts`. */
export interface ExtractedSqlQuery {
  /** Enclosing `Repository` method name (e.g. `getDraft`, `decide`, `distinguish`). */
  methodName: string;
  /** Optional `// name: <QueryName>` label immediately preceding the query. */
  queryName: string | null;
  /** Human-readable identifier combining method and query name (`getDraft (GetDraft)`). */
  label: string;
  /** 1-based line number where the tagged template starts in `postgres.ts`. */
  line: number;
  /** Raw template content with `${...}` interpolations preserved as `${expr}`. */
  rawSql: string;
}

/**
 * Strips SQL comments (`-- ...` and `/* ... *\/`) and single-quoted string
 * literals so keywords inside comments or string constants cannot spoof a
 * `WHERE workspace_id = ...` clause.
 */
function stripSqlCommentsAndStrings(sql: string): string {
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
      i++;
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
      out += "''";
      continue;
    }
    out += sql[i++];
  }

  return out;
}

/**
 * Replaces parenthesised sub-expressions `( ... )` at depth >= 1 that contain
 * a nested `SELECT` (subqueries) with `(__SUBQUERY__)`, while preserving
 * non-SELECT parenthesised boolean groups at the outer query level.
 *
 * This ensures a `workspace_id` filter buried inside a subquery (e.g.
 * `WHERE d.id = ${id} AND d.task_id IN (SELECT id FROM tasks WHERE workspace_id = ...)`)
 * cannot satisfy the requirement that the `drafts` query itself filters by
 * `workspace_id` in its own `WHERE` clause.
 */
function maskSubqueries(sql: string): string {
  let out = "";
  let i = 0;
  const n = sql.length;

  while (i < n) {
    if (sql[i] === "(") {
      let depth = 1;
      let j = i + 1;
      while (j < n && depth > 0) {
        if (sql[j] === "(") depth++;
        else if (sql[j] === ")") depth--;
        j++;
      }
      const inner = sql.slice(i + 1, j - 1);
      if (/\bSELECT\b/i.test(inner)) {
        out += "(__SUBQUERY__)";
      } else {
        out += `(${maskSubqueries(inner)})`;
      }
      i = j;
      continue;
    }
    out += sql[i++];
  }

  return out;
}

/**
 * Splits a boolean predicate expression at top-level `OR` operators (outside any
 * parentheses). If the expression has a top-level `OR`, a tenant check on only one
 * branch (`d.id = ${id} OR d.workspace_id = ${scope.workspaceId}`) would allow
 * rows matching the other branch to leak across workspaces.
 */
function splitTopLevelOrBranches(whereExpr: string): string[] {
  const branches: string[] = [];
  let depth = 0;
  let lastStart = 0;
  const n = whereExpr.length;
  let i = 0;

  while (i < n) {
    const ch = whereExpr[i];
    if (ch === "(") {
      depth++;
      i++;
      continue;
    }
    if (ch === ")") {
      depth = Math.max(0, depth - 1);
      i++;
      continue;
    }
    if (depth === 0) {
      const prevChar = i > 0 ? whereExpr[i - 1] : " ";
      if (!/[A-Za-z0-9_$]/.test(prevChar)) {
        const orMatch = /^OR(?![A-Za-z0-9_$])/i.exec(whereExpr.slice(i));
        if (orMatch) {
          branches.push(whereExpr.slice(lastStart, i).trim());
          i += orMatch[0].length;
          lastStart = i;
          continue;
        }
      }
    }
    i++;
  }

  branches.push(whereExpr.slice(lastStart).trim());
  return branches.filter((b) => b.length > 0);
}

/**
 * Splits a boolean predicate expression at top-level `AND` operators (outside any
 * parentheses) into its conjuncts.
 */
function splitTopLevelAndConjuncts(whereExpr: string): string[] {
  const conjuncts: string[] = [];
  let depth = 0;
  let lastStart = 0;
  const n = whereExpr.length;
  let i = 0;

  while (i < n) {
    const ch = whereExpr[i];
    if (ch === "(") {
      depth++;
      i++;
      continue;
    }
    if (ch === ")") {
      depth = Math.max(0, depth - 1);
      i++;
      continue;
    }
    if (depth === 0) {
      const prevChar = i > 0 ? whereExpr[i - 1] : " ";
      if (!/[A-Za-z0-9_$]/.test(prevChar)) {
        const andMatch = /^AND(?![A-Za-z0-9_$])/i.exec(whereExpr.slice(i));
        if (andMatch) {
          conjuncts.push(whereExpr.slice(lastStart, i).trim());
          i += andMatch[0].length;
          lastStart = i;
          continue;
        }
      }
    }
    i++;
  }

  conjuncts.push(whereExpr.slice(lastStart).trim());
  return conjuncts.filter((c) => c.length > 0);
}

/**
 * Extracts every `tx\`...\`` or `sql\`...\`` tagged-template SQL literal from the
 * TypeScript source of `postgres.ts`, pairing each with its enclosing method name
 * and `// name: ...` comment.
 */
export function extractRepositoryQueries(source: string): ExtractedSqlQuery[] {
  const queries: ExtractedSqlQuery[] = [];

  // Track method definitions inside `class Repository`:
  // e.g. `async getDraft(`, `private async distinguish(`.
  const methodRegex =
    /^\s*(?:(?:private|public|protected)\s+)?(?:async\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*\([^;]*?\)\s*(?::\s*[^{]+)?\{/gm;
  const methods: Array<{ name: string; index: number }> = [];
  for (const match of source.matchAll(methodRegex)) {
    const name = match[1];
    if (name && name !== "constructor" && name !== "if" && name !== "for" && name !== "while" && name !== "switch" && name !== "catch") {
      methods.push({ name, index: match.index ?? 0 });
    }
  }

  // Match tagged template openings: `tx<...>\`` or `tx\`` or `sql<...>\`` or `sql\``
  const tagRegex = /\b(?:tx|sql)\s*(?:<[^>`]*>)?\s*`/g;
  for (const match of source.matchAll(tagRegex)) {
    const tagStart = match.index ?? 0;
    const templateStart = tagStart + match[0].length;

    // Scan until matching closing backtick, respecting `${ ... }` interpolations.
    let i = templateStart;
    const n = source.length;
    while (i < n && source[i] !== "`") {
      if (source[i] === "\\") {
        i += 2;
        continue;
      }
      if (source[i] === "$" && source[i + 1] === "{") {
        i += 2;
        let braceDepth = 1;
        while (i < n && braceDepth > 0) {
          if (source[i] === "{") braceDepth++;
          else if (source[i] === "}") braceDepth--;
          i++;
        }
        continue;
      }
      i++;
    }

    const rawSql = source.slice(templateStart, i);
    const line = source.slice(0, tagStart).split("\n").length;

    let methodName = "<top-level>";
    for (const m of methods) {
      if (m.index <= tagStart) {
        methodName = m.name;
      } else {
        break;
      }
    }

    // Look back up to 350 characters before the tag for `// name: <QueryName>`.
    const lookbackWindow = source.slice(Math.max(0, tagStart - 350), tagStart);
    const nameMatches = [...lookbackWindow.matchAll(/\/\/\s*name:\s*([A-Za-z0-9_]+)/g)];
    const queryName =
      nameMatches.length > 0 ? (nameMatches[nameMatches.length - 1]?.[1] ?? null) : null;

    const label = queryName ? `${methodName} (${queryName})` : methodName;
    queries.push({ methodName, queryName, label, line, rawSql });
  }

  return queries;
}

/**
 * Checks whether a cleaned SQL statement touches the `drafts` table (rather than
 * `draft_revisions` or another table).
 */
function touchesDraftsTable(cleanSql: string): {
  touchesDrafts: boolean;
  draftsAlias: string | null;
} {
  // Match `FROM drafts [AS] <alias>`, `JOIN drafts [AS] <alias>`, `UPDATE drafts [AS] <alias>`, `INTO drafts`
  const fromUpdateMatch = /\b(?:FROM|JOIN|UPDATE)\s+drafts(?:\s+(?:AS\s+)?([A-Za-z_][A-Za-z0-9_]*))?\b/i.exec(
    cleanSql,
  );
  if (fromUpdateMatch) {
    const candidateAlias = fromUpdateMatch[1] ?? null;
    const reserved: Record<string, true> = {
      WHERE: true,
      SET: true,
      JOIN: true,
      INNER: true,
      LEFT: true,
      RIGHT: true,
      ON: true,
      ORDER: true,
      GROUP: true,
      LIMIT: true,
      RETURNING: true,
      VALUES: true,
      SELECT: true,
    };
    const alias =
      candidateAlias && !reserved[candidateAlias.toUpperCase()] ? candidateAlias : null;
    return { touchesDrafts: true, draftsAlias: alias };
  }

  if (/\bINSERT\s+INTO\s+drafts\b/i.test(cleanSql)) {
    return { touchesDrafts: true, draftsAlias: null };
  }

  if (/\bDELETE\s+FROM\s+drafts\b/i.test(cleanSql)) {
    return { touchesDrafts: true, draftsAlias: null };
  }

  return { touchesDrafts: false, draftsAlias: null };
}

/**
 * Extracts the outer query's `WHERE` clause body (after masking subqueries), stopping
 * at trailing top-level SQL clauses (`ORDER BY`, `GROUP BY`, `LIMIT`, `OFFSET`,
 * `ON CONFLICT`, `RETURNING`, `FOR UPDATE`, `FOR SHARE`).
 */
function extractOuterWhereClause(maskedSql: string): string | null {
  const whereMatch = /\bWHERE\b([\s\S]*)$/i.exec(maskedSql);
  if (!whereMatch) {
    return null;
  }
  const afterWhere = whereMatch[1] ?? "";
  const tailStop =
    /\b(?:ORDER\s+BY|GROUP\s+BY|HAVING|LIMIT|OFFSET|ON\s+CONFLICT|RETURNING|FOR\s+(?:UPDATE|SHARE))\b/i.exec(
      afterWhere,
    );
  const body = tailStop ? afterWhere.slice(0, tailStop.index) : afterWhere;
  return body.trim();
}

/**
 * Checks whether a single `WHERE` conjunct is a valid equality filter binding
 * `workspace_id` to `${scope.workspaceId}` (or `${workspaceId}`) on the `drafts`
 * table (or its joined `tasks` source in `INSERT INTO drafts ... SELECT ... FROM tasks t`).
 */
function isWorkspaceIdConjunct(
  conjunct: string,
  isInsertSelectFromTasks: boolean,
  draftsAlias: string | null,
): boolean {
  // Strip outer redundant parentheses around a single conjunct if it contains no top-level OR.
  let expr = conjunct.trim();
  while (expr.startsWith("(") && expr.endsWith(")")) {
    const inner = expr.slice(1, -1).trim();
    if (splitTopLevelOrBranches(inner).length === 1) {
      expr = inner;
    } else {
      break;
    }
  }

  // Reject any conjunct that still has a top-level OR inside it (e.g. `(d.id = ${id} OR d.workspace_id = ${scope.workspaceId})`).
  if (splitTopLevelOrBranches(expr).length > 1) {
    return false;
  }

  // Allowed column references:
  // - If `drafts` is aliased as `d`, require `d.workspace_id` (or `drafts.workspace_id`), not an unrelated table alias.
  // - If `drafts` has no alias (e.g. `UPDATE drafts ... WHERE workspace_id = ...`), allow `workspace_id` or `drafts.workspace_id`.
  // - If `INSERT INTO drafts ... SELECT ... FROM tasks t`, allow `t.workspace_id` or `tasks.workspace_id` or `workspace_id`.
  let allowedQualifiers: string[];
  if (isInsertSelectFromTasks) {
    allowedQualifiers = ["t\\.", "tasks\\.", ""];
  } else if (draftsAlias) {
    allowedQualifiers = [`${draftsAlias}\\.`, "drafts\\."];
  } else {
    allowedQualifiers = ["drafts\\.", ""];
  }

  const qualifierPattern = `(?:${allowedQualifiers.join("|")})`;
  const forwardPattern = new RegExp(
    `^${qualifierPattern}workspace_id\\s*=\\s*\\$\\{\\s*(?:scope\\.workspaceId|workspaceId)\\s*\\}$`,
    "i",
  );
  const reversePattern = new RegExp(
    `^\\$\\{\\s*(?:scope\\.workspaceId|workspaceId)\\s*\\}\\s*=\\s*${qualifierPattern}workspace_id$`,
    "i",
  );

  return forwardPattern.test(expr) || reversePattern.test(expr);
}

/**
 * Validates all SQL queries in `postgres.ts` that touch the `drafts` table.
 * Returns a list of human-readable violation messages (empty when all gates pass).
 */
export function checkApprovalDraftQueries(
  source: string,
  allowlist: Record<string, string> = UNSCOPED_DRAFTS_QUERY_ALLOWLIST,
): string[] {
  const errors: string[] = [];
  const queries = extractRepositoryQueries(source);

  if (queries.length === 0) {
    return ["postgres.ts: tidak ditemukan query SQL tagged-template (`tx` / `sql`)"];
  }

  const draftsQueries: ExtractedSqlQuery[] = [];

  for (const q of queries) {
    const cleanSql = stripSqlCommentsAndStrings(q.rawSql);
    const { touchesDrafts, draftsAlias } = touchesDraftsTable(cleanSql);
    if (!touchesDrafts) {
      continue;
    }

    draftsQueries.push(q);

    // Check if the query is in the explicit allowlist.
    const allowKey =
      q.queryName && q.queryName in allowlist
        ? q.queryName
        : q.methodName in allowlist
          ? q.methodName
          : null;
    if (allowKey !== null) {
      const reason = allowlist[allowKey]?.trim() ?? "";
      if (reason.length === 0) {
        errors.push(
          `postgres.ts:${q.line} [${q.label}]: terdaftar di UNSCOPED_DRAFTS_QUERY_ALLOWLIST tetapi alasan (reason) kosong`,
        );
      }
      continue;
    }

    const maskedSql = maskSubqueries(cleanSql);
    const outerWhere = extractOuterWhereClause(maskedSql);

    if (!outerWhere) {
      errors.push(
        `postgres.ts:${q.line} [${q.label}]: query yang menyentuh tabel 'drafts' wajib memiliki klausa WHERE dengan filter workspace_id (klausa WHERE tidak ditemukan di query utama)`,
      );
      continue;
    }

    // Check if the query filters by draft id (`d.id = ${...}` or `id = ${...}`).
    const filtersByDraftId = /\b(?:[A-Za-z_][A-Za-z0-9_]*\.)?id\s*=\s*\$\{[^}]+\}/i.test(
      cleanSql,
    );
    const filtersByDraftIdInOuterWhere =
      /\b(?:[A-Za-z_][A-Za-z0-9_]*\.)?id\s*=\s*\$\{[^}]+\}/i.test(outerWhere);

    // Reject top-level OR in the outer WHERE clause so `WHERE d.id = ${id} OR d.workspace_id = ...` fails immediately.
    const topOrBranches = splitTopLevelOrBranches(outerWhere);
    if (topOrBranches.length > 1) {
      errors.push(
        `postgres.ts:${q.line} [${q.label}]: klausa WHERE pada query 'drafts' tidak boleh menggunakan operator OR tingkat atas yang dapat melewati filter workspace_id`,
      );
      continue;
    }

    const isInsertSelectFromTasks =
      /\bINSERT\s+INTO\s+drafts\b/i.test(maskedSql) && /\bFROM\s+tasks\b/i.test(maskedSql);

    const conjuncts = splitTopLevelAndConjuncts(outerWhere);
    const hasValidWorkspaceConjunct = conjuncts.some((c) =>
      isWorkspaceIdConjunct(c, isInsertSelectFromTasks, draftsAlias),
    );

    if (!hasValidWorkspaceConjunct) {
      const idNote =
        filtersByDraftId || filtersByDraftIdInOuterWhere
          ? " pada query yang sama dengan filter id"
          : "";
      errors.push(
        `postgres.ts:${q.line} [${q.label}]: query 'drafts' kehilangan filter wajib 'workspace_id = \${scope.workspaceId}' di klausa WHERE utama${idNote}`,
      );
      continue;
    }

    // If the query uses an id parameter (`d.id = ${...}`), verify the `id` predicate
    // and the `workspace_id` predicate both live in the same outer WHERE clause.
    if (filtersByDraftId && !filtersByDraftIdInOuterWhere) {
      errors.push(
        `postgres.ts:${q.line} [${q.label}]: filter id dan filter workspace_id wajib berada di klausa WHERE pada query yang sama`,
      );
    }
  }

  if (draftsQueries.length === 0) {
    errors.push("postgres.ts: tidak ditemukan satu pun query yang menyentuh tabel 'drafts'");
  }

  // Guard against stale entries in UNSCOPED_DRAFTS_QUERY_ALLOWLIST.
  const knownNames = new Set<string>();
  for (const q of draftsQueries) {
    knownNames.add(q.methodName);
    if (q.queryName) knownNames.add(q.queryName);
  }
  for (const key of Object.keys(allowlist)) {
    if (!knownNames.has(key)) {
      errors.push(
        `UNSCOPED_DRAFTS_QUERY_ALLOWLIST: entri '${key}' tidak cocok dengan metode atau query 'drafts' mana pun di postgres.ts`,
      );
    }
  }

  return errors;
}

describe("approval repository SQL tenant-isolation gate (#722)", () => {
  const diskSource = readFileSync(POSTGRES_TS_PATH, "utf8");

  test("seluruh query 'drafts' di postgres.ts memiliki filter workspace_id di klausa WHERE yang sama", () => {
    const errors = checkApprovalDraftQueries(diskSource);
    expect(errors).toEqual([]);

    // Verify all expected drafts methods are discovered and checked.
    const queries = extractRepositoryQueries(diskSource);
    const draftsMethodNames = queries
      .filter((q) => touchesDraftsTable(stripSqlCommentsAndStrings(q.rawSql)).touchesDrafts)
      .map((q) => q.methodName);

    expect(draftsMethodNames).toEqual([
      "createDraft",
      "getDraft",
      "listDrafts",
      "decide",
      "markSent",
      "hold",
      "setCardMessage",
      "distinguish",
    ]);
  });

  test("mutasi #722: menghapus 'AND d.workspace_id = ${scope.workspaceId}' dari getDraft memerahkan gate dengan menyebut getDraft (GetDraft)", () => {
    const mutated = diskSource.replace(
      "WHERE d.id = ${id} AND d.workspace_id = ${scope.workspaceId}",
      "WHERE d.id = ${id}",
    );
    expect(mutated).not.toBe(diskSource);

    const errors = checkApprovalDraftQueries(mutated);
    expect(errors.length).toBeGreaterThan(0);
    expect(
      errors.some(
        (e) =>
          e.includes("getDraft (GetDraft)") &&
          e.includes("workspace_id") &&
          e.includes("filter id"),
      ),
    ).toBe(true);
  });

  test("varian OR: mengubah AND menjadi 'WHERE d.id = ${id} OR d.workspace_id = ${scope.workspaceId}' memerahkan gate", () => {
    const mutated = diskSource.replace(
      "WHERE d.id = ${id} AND d.workspace_id = ${scope.workspaceId}",
      "WHERE d.id = ${id} OR d.workspace_id = ${scope.workspaceId}",
    );
    expect(mutated).not.toBe(diskSource);

    const errors = checkApprovalDraftQueries(mutated);
    expect(errors.length).toBeGreaterThan(0);
    expect(errors.some((e) => e.includes("getDraft (GetDraft)") && e.includes("OR"))).toBe(true);

    // Also reject parenthesised OR inside an AND clause.
    const mutatedParenOr = diskSource.replace(
      "WHERE d.id = ${id} AND d.workspace_id = ${scope.workspaceId}",
      "WHERE (d.id = ${id} OR d.workspace_id = ${scope.workspaceId})",
    );
    const parenOrErrors = checkApprovalDraftQueries(mutatedParenOr);
    expect(parenOrErrors.some((e) => e.includes("getDraft (GetDraft)"))).toBe(true);
  });

  test("varian subquery: memindahkan klausa workspace_id ke subquery lain memerahkan gate", () => {
    const mutated = diskSource.replace(
      "WHERE d.id = ${id} AND d.workspace_id = ${scope.workspaceId}",
      "WHERE d.id = ${id} AND d.task_id IN (SELECT id FROM tasks WHERE workspace_id = ${scope.workspaceId})",
    );
    expect(mutated).not.toBe(diskSource);

    const errors = checkApprovalDraftQueries(mutated);
    expect(errors.length).toBeGreaterThan(0);
    expect(
      errors.some((e) => e.includes("getDraft (GetDraft)") && e.includes("workspace_id")),
    ).toBe(true);
  });

  test("varian alias salah: memfilter t.workspace_id tetapi menghapus d.workspace_id pada SELECT FROM drafts d memerahkan gate", () => {
    const mutated = diskSource.replace(
      "WHERE d.id = ${id} AND d.workspace_id = ${scope.workspaceId}",
      "WHERE d.id = ${id} AND t.workspace_id = ${scope.workspaceId}",
    );
    expect(mutated).not.toBe(diskSource);

    const errors = checkApprovalDraftQueries(mutated);
    expect(errors.length).toBeGreaterThan(0);
    expect(errors.some((e) => e.includes("getDraft (GetDraft)"))).toBe(true);
  });

  test("menghapus filter workspace_id dari createDraft, listDrafts, decide, markSent, hold, setCardMessage, atau distinguish menyebut metode yang rusak", () => {
    const cases: Array<{ target: string; replacement: string; expectedLabel: string }> = [
      {
        target: "WHERE t.id = ${draft.taskId}\n          AND t.workspace_id = ${scope.workspaceId}\n          AND t.agent_id = ${draft.agentId}",
        replacement: "WHERE t.id = ${draft.taskId}\n          AND t.agent_id = ${draft.agentId}",
        expectedLabel: "createDraft (CreateDraft)",
      },
      {
        target: "WHERE d.workspace_id = ${scope.workspaceId}\n          AND (${optionalText(filter.status)}",
        replacement: "WHERE (${optionalText(filter.status)}",
        expectedLabel: "listDrafts (ListDrafts)",
      },
      {
        target: "WHERE id = ${decision.draftId}\n          AND workspace_id = ${scope.workspaceId}\n          AND status = 'pending'",
        replacement: "WHERE id = ${decision.draftId}\n          AND status = 'pending'",
        expectedLabel: "decide (DecideDraft)",
      },
      {
        target: "WHERE id = ${id} AND workspace_id = ${scope.workspaceId} AND status = 'approved'",
        replacement: "WHERE id = ${id} AND status = 'approved'",
        expectedLabel: "markSent (MarkDraftSent)",
      },
      {
        target: "WHERE id = ${id}\n          AND workspace_id = ${scope.workspaceId}\n          AND status = 'approved'",
        replacement: "WHERE id = ${id}\n          AND status = 'approved'",
        expectedLabel: "hold (HoldDraft)",
      },
      {
        target: "WHERE id = ${id} AND workspace_id = ${scope.workspaceId}\n        RETURNING *",
        replacement: "WHERE id = ${id}\n        RETURNING *",
        expectedLabel: "setCardMessage (SetDraftCard)",
      },
      {
        target: "SELECT d.id FROM drafts d\n      JOIN tasks t ON t.id = d.task_id\n      WHERE d.id = ${id} AND d.workspace_id = ${scope.workspaceId}",
        replacement: "SELECT d.id FROM drafts d\n      JOIN tasks t ON t.id = d.task_id\n      WHERE d.id = ${id}",
        expectedLabel: "[distinguish]",
      },
    ];
    for (const c of cases) {
      const mutated = diskSource.replace(c.target, c.replacement);
      expect(mutated).not.toBe(diskSource);
      const errors = checkApprovalDraftQueries(mutated);
      expect(errors.some((e) => e.includes(c.expectedLabel))).toBe(true);
    }
  });

  test("allowlist eksplisit menolak alasan kosong dan menolak nama query fiktif", () => {
    const mutated = diskSource.replace(
      "WHERE d.id = ${id} AND d.workspace_id = ${scope.workspaceId}",
      "WHERE d.id = ${id}",
    );

    // Empty reason in allowlist is rejected.
    const emptyReasonErrors = checkApprovalDraftQueries(mutated, { GetDraft: "   " });
    expect(
      emptyReasonErrors.some(
        (e) => e.includes("GetDraft") && e.includes("UNSCOPED_DRAFTS_QUERY_ALLOWLIST"),
      ),
    ).toBe(true);

    // Non-empty reason in allowlist permits the specific query.
    const validAllowErrors = checkApprovalDraftQueries(mutated, {
      GetDraft: "Hypothetical cross-tenant system sweep with explicit justification",
    });
    expect(validAllowErrors).toEqual([]);

    // Stale/unknown allowlist key is rejected.
    const staleErrors = checkApprovalDraftQueries(diskSource, {
      NonExistentDraftQuery: "Some reason",
    });
    expect(
      staleErrors.some(
        (e) =>
          e.includes("NonExistentDraftQuery") && e.includes("UNSCOPED_DRAFTS_QUERY_ALLOWLIST"),
      ),
    ).toBe(true);
  });
});
