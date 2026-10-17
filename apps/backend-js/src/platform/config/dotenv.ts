/**
 * `.env` loading.
 *
 * Ported from packages/platform/config/dotenv.go. A missing file is not an
 * error: docker-compose and CI supply the variables directly. Bun reads `.env`
 * automatically at process start; this function exists so a binary that starts
 * with a different working directory (or after `chdir`) can load one
 * explicitly, and it never overwrites a variable the process already has.
 */

import { existsSync, readFileSync } from "node:fs";

/**
 * Loads a `.env` file when one is present next to the working directory.
 * A missing file is not an error.
 */
export function loadDotEnv(): void {
  const path = `${process.cwd()}/.env`;
  if (!existsSync(path)) return;

  let content: string;
  try {
    content = readFileSync(path, "utf8");
  } catch {
    // Go's godotenv returns a *PathError only when the file exists but cannot
    // be read; the loader then surfaces it. Match that: a present-but-unreadable
    // file is an error, an absent one is not.
    throw new Error(`open ${path}: permission denied`);
  }

  for (const [key, value] of parseDotEnv(content)) {
    if (process.env[key] === undefined) process.env[key] = value;
  }
}

/**
 * Parses dotenv syntax: `KEY=value`, optional `export` prefix, single or double
 * quotes, `#` comments on unquoted lines, and the usual backslash escapes
 * inside double quotes.
 */
export function parseDotEnv(content: string): Array<[string, string]> {
  const out: Array<[string, string]> = [];

  for (const rawLine of content.split(/\r?\n/)) {
    const line = rawLine.trim();
    if (line === "" || line.startsWith("#")) continue;

    const withoutExport = line.startsWith("export ") ? line.slice("export ".length).trim() : line;
    const eq = withoutExport.indexOf("=");
    if (eq <= 0) continue;

    const key = withoutExport.slice(0, eq).trim();
    if (!/^[A-Za-z_][A-Za-z0-9_.]*$/.test(key)) continue;

    let value = withoutExport.slice(eq + 1).trim();

    if (value.startsWith("'")) {
      const end = value.indexOf("'", 1);
      value = end === -1 ? value.slice(1) : value.slice(1, end);
    } else if (value.startsWith('"')) {
      const end = value.lastIndexOf('"');
      value = end <= 0 ? value.slice(1) : value.slice(1, end);
      value = value
        .replace(/\\n/g, "\n")
        .replace(/\\r/g, "\r")
        .replace(/\\t/g, "\t")
        .replace(/\\"/g, '"')
        .replace(/\\\\/g, "\\");
    } else {
      const comment = value.indexOf(" #");
      if (comment !== -1) value = value.slice(0, comment).trim();
      if (value.startsWith("#")) value = "";
    }

    out.push([key, value]);
  }

  return out;
}
