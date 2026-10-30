#!/usr/bin/env node
/**
 * Fails when the route audit in `docs/deep-links.md` drifts from the pages that
 * actually exist under `apps/frontend/src/app`.
 *
 * The audit table is the contract, so it is parsed rather than duplicated here:
 * adding a screen without a row (or a row without a screen) fails the gate.
 *
 * Row status:
 * - `ada`       the named `page.tsx` exists, resolves to the row's path, and renders
 *               a screen (a page whose body is only `redirect()` counts as moved).
 * - `pindah`    the named `page.tsx` exists, resolves to the row's path, and contains
 *               only `redirect()` to the path the row names.
 * - `tidak ada` no `page.tsx` serves that path shape, including `[param]` pages.
 *
 * A glob row (`/admin/**`) must name every page under its prefix. The well-known
 * `components` block in the same document must be covered by `ada` rows: an exact
 * path needs its own row, and a `/*` glob must not hide an undocumented page.
 */

import { readFileSync, readdirSync, statSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const DOC = "docs/deep-links.md";
const APP = "apps/frontend/src/app";
const read = (path) => readFileSync(join(root, path), "utf8");
const appPath = (path) => join(root, APP, path);

function stripComments(source) {
  return source.replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/[^\n]*/g, "");
}

function isFile(path) {
  try {
    return statSync(path).isFile();
  } catch {
    return false;
  }
}

/** Every `page.tsx` under the app directory, relative to it. */
function pageFiles(directory = appPath(""), prefix = "") {
  const files = [];
  for (const entry of readdirSync(directory)) {
    const absolute = join(directory, entry);
    const relative = prefix === "" ? entry : `${prefix}/${entry}`;
    if (statSync(absolute).isDirectory()) {
      files.push(...pageFiles(absolute, relative));
      continue;
    }
    if (entry === "page.tsx") {
      files.push(relative);
    }
  }
  return files.sort();
}

/** The URL shape a page file serves: route groups vanish, `[param]` is a wildcard. */
function routePattern(file) {
  return file
    .split("/")
    .slice(0, -1)
    .filter((segment) => !/^\(.+\)$/.test(segment))
    .map((segment) => {
      if (/^\[\[?\.\.\..+\]\]?$/.test(segment)) {
        return "**";
      }
      return /^\[.+\]$/.test(segment) ? "*" : segment;
    });
}

/** The URL shape a row's path names: `<task_id>` and friends are wildcards. */
function pathPattern(path) {
  return path
    .split("/")
    .filter(Boolean)
    .map((segment) => (/^<[^>]+>$/.test(segment) ? "*" : segment));
}

/** `*` on either side matches one segment; `**` matches zero or more. */
function matches(pattern, route, i = 0, j = 0) {
  if (j < route.length && route[j] === "**") {
    return (
      matches(pattern, route, i, j + 1) ||
      (i < pattern.length && matches(pattern, route, i + 1, j))
    );
  }
  if (i === pattern.length) {
    return j === route.length;
  }
  if (j >= route.length) {
    return false;
  }
  if (pattern[i] === "*" || route[j] === "*") {
    return matches(pattern, route, i + 1, j + 1);
  }
  return pattern[i] === route[j] && matches(pattern, route, i + 1, j + 1);
}

function hasPrefix(route, prefix, strict) {
  if (route.length < prefix.length + (strict ? 1 : 0)) {
    return false;
  }
  return prefix.every((segment, index) => route[index] === segment);
}

function parseRows(source) {
  const heading = source.indexOf("## Audit rute");
  if (heading === -1) {
    throw new Error(`${DOC} has no '## Audit rute' section`);
  }

  const lines = source.slice(heading).split("\n");
  const header = lines.findIndex((line) =>
    /^\|\s*Path\s*\|\s*Berkas halaman\s*\|\s*Status\s*\|$/.test(line.trim()),
  );
  if (header === -1) {
    throw new Error(`${DOC}'s audit section has no '| Path | Berkas halaman | Status |' table`);
  }

  const rows = [];
  for (let index = header + 2; index < lines.length; index += 1) {
    const line = lines[index].trim();
    if (!line.startsWith("|")) {
      break;
    }

    const cells = line.replace(/^\||\|$/g, "").split("|").map((cell) => cell.trim());
    if (cells.length !== 3) {
      throw new Error(`${DOC} line ${index + 1}: expected 3 cells, found ${cells.length}`);
    }

    const pathCell = cells[0].match(/`([^`]+)`/);
    if (!pathCell) {
      throw new Error(`${DOC} line ${index + 1}: the Path cell is not in backticks`);
    }
    const path = pathCell[1].split("?")[0];
    const status = cells[2];

    const files = [...cells[1].matchAll(/`([^`]+)`/g)]
      .map((match) => match[1])
      .filter((value) => value.endsWith(".tsx") && !value.startsWith("/"));

    let kind;
    if (/^tidak ada\b/.test(status)) {
      kind = "absent";
    } else if (/^ada\b/.test(status)) {
      kind = "present";
    } else if (/^pindah\b/.test(status)) {
      kind = "moved";
    } else {
      throw new Error(
        `${DOC} line ${index + 1}: status '${status}' is not 'ada', 'pindah', or 'tidak ada'`,
      );
    }

    const glob = /\*{1,2}$/.test(path);
    if (glob && kind !== "present") {
      throw new Error(`${DOC} line ${index + 1}: only 'ada' rows may use a glob path`);
    }
    if (kind !== "absent" && files.length === 0) {
      throw new Error(`${DOC} line ${index + 1}: '${path}' names no page file`);
    }
    if (kind === "absent" && files.length > 0) {
      throw new Error(`${DOC} line ${index + 1}: '${path}' says 'tidak ada' but names a file`);
    }
    if (kind !== "absent" && files.some((file) => !file.endsWith("page.tsx"))) {
      throw new Error(`${DOC} line ${index + 1}: '${path}' does not name a page.tsx`);
    }

    rows.push({ path, kind, status, files, glob, line: index + 1 });
  }

  if (rows.length === 0) {
    throw new Error(`${DOC}'s audit table has no rows`);
  }
  return rows;
}

/** The `components` paths claimed by the AASA block in the same document. */
function wellKnownComponents(source) {
  const heading = "### `/.well-known/apple-app-site-association`";
  const start = source.indexOf(heading);
  if (start === -1) {
    throw new Error(`${DOC} has no AASA section`);
  }
  const next = source.indexOf("\n### ", start + heading.length);
  const section = source.slice(start, next === -1 ? undefined : next);
  const block = section.match(/```json\n([\s\S]*?)```/);
  if (!block) {
    throw new Error(`${DOC}'s AASA section has no json block`);
  }

  let document;
  try {
    document = JSON.parse(block[1]);
  } catch (error) {
    throw new Error(`${DOC}'s AASA block is not JSON: ${error.message}`);
  }
  return (document.applinks?.details ?? []).flatMap((detail) =>
    (detail.components ?? []).map((component) => component["/"]).filter(Boolean),
  );
}

let rows;
let pages;
let components;
try {
  const source = read(DOC);
  rows = parseRows(source);
  pages = pageFiles();
  components = wellKnownComponents(source);
} catch (error) {
  console.error(`deep-links: ${error.message}`);
  process.exit(1);
}

const routes = pages.map((file) => ({ file, pattern: routePattern(file) }));
const failures = [];
const fail = (path, reason) => {
  failures.push(`  ${path} — ${DOC} ${reason}`);
};

for (const row of rows) {
  if (row.kind === "absent") {
    const pattern = pathPattern(row.path);
    const served = routes.filter((route) => matches(pattern, route.pattern));
    if (served.length > 0) {
      fail(
        row.path,
        `says 'tidak ada', but ${served.map((route) => route.file).join(", ")} serves it; update the row`,
      );
    }
    continue;
  }

  const missing = row.files.filter((file) => !isFile(appPath(file)));
  if (missing.length > 0) {
    fail(row.path, `says '${row.status}', but ${missing.join(", ")} does not exist`);
    continue;
  }

  if (row.glob) {
    const strict = !row.path.endsWith("/**");
    const prefix = row.path.replace(/\/\*{1,2}$/, "").split("/").filter(Boolean);
    const named = new Set(row.files);
    const unlisted = routes
      .filter((route) => hasPrefix(route.pattern, prefix, strict))
      .filter((route) => !named.has(route.file))
      .map((route) => route.file);
    if (unlisted.length > 0) {
      fail(
        row.path,
        `is a glob but does not list ${unlisted.join(", ")}; add the file to the row`,
      );
    }
    const outside = row.files.filter((file) => !hasPrefix(routePattern(file), prefix, false));
    if (outside.length > 0) {
      fail(row.path, `is a glob but names ${outside.join(", ")}, which is outside it`);
    }
    continue;
  }

  const pattern = pathPattern(row.path);
  const named = new Set(row.files);
  const misplaced = row.files.filter((file) => !matches(pattern, routePattern(file)));
  if (misplaced.length > 0) {
    fail(
      row.path,
      `names ${misplaced.join(", ")}, which serves /${routePattern(misplaced[0]).join("/")}; fix the row`,
    );
    continue;
  }

  const served = routes.filter((route) => matches(pattern, route.pattern));
  const undocumented = served.filter((route) => !named.has(route.file));
  if (undocumented.length > 0) {
    fail(
      row.path,
      `says '${row.status}', but ${undocumented.map((route) => route.file).join(", ")} also serves it`,
    );
    continue;
  }

  for (const file of row.files) {
    const body = stripComments(readFileSync(appPath(file), "utf8"));
    const redirect = body.match(/\bredirect\s*\(\s*["'`]([^"'`]+)["'`]/);
    const redirectOnly = Boolean(redirect) && !/\breturn\b/.test(body);

    if (row.kind === "present" && redirectOnly) {
      fail(row.path, `says 'ada', but ${file} is only redirect(${redirect[1]}); mark it 'pindah'`);
      break;
    }

    if (row.kind === "moved") {
      if (!redirectOnly) {
        fail(row.path, `says 'pindah', but ${file} does not only redirect; fix the row`);
        break;
      }
      const target = row.status.match(/pindah ke\s+`([^`]+)`/);
      if (target && target[1] !== redirect[1]) {
        fail(
          row.path,
          `says it moves to ${target[1]}, but ${file} redirects to ${redirect[1]}; fix the row`,
        );
        break;
      }
    }
  }
}

const adaPatterns = rows
  .filter((row) => row.kind === "present" && !row.glob)
  .map((row) => ({ path: row.path, pattern: pathPattern(row.path) }));

for (const component of components) {
  const glob = /\*{1,2}$/.test(component);
  if (!glob) {
    if (!adaPatterns.some((row) => matches(row.pattern, pathPattern(component)))) {
      fail(
        component,
        `lists it as a universal link, but no 'ada' row covers it; update the table or the block`,
      );
    }
    continue;
  }

  const strict = !component.endsWith("/**");
  const prefix = component.replace(/\/\*{1,2}$/, "").split("/").filter(Boolean);
  const covered = routes
    .filter((route) => hasPrefix(route.pattern, prefix, strict))
    .filter((route) => !adaPatterns.some((row) => matches(row.pattern, route.pattern)));
  if (covered.length > 0) {
    fail(
      component,
      `claims it in the well-known block, but ${covered.map((route) => route.file).join(", ")} has no 'ada' row`,
    );
    continue;
  }
  if (!adaPatterns.some((row) => hasPrefix(row.pattern, prefix, strict))) {
    fail(
      component,
      `claims it in the well-known block, but no 'ada' row is under it; update the table or the block`,
    );
  }
}

if (failures.length > 0) {
  console.error(
    `deep-links: ${rows.length} audit rows checked against ${APP}, ${failures.length} do not match`,
  );
  console.error(failures.join("\n"));
  process.exit(1);
}

console.log(`deep-links: ${rows.length} audit rows match ${APP}`);
