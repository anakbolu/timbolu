#!/usr/bin/env node
/**
 * Fails when the design tokens drift from the Tailwind theme.
 *
 * `apps/frontend/src/app/globals.css` is the source of truth while Tailwind
 * still reads it. Its `@theme` block must name exactly the tokens in
 * `tokens.json`, with exactly the same values — that JSON is what a style sheet
 * without Tailwind (the Expo app) reads.
 *
 * The parser keys on `--name: value;` declarations and ignores whitespace,
 * ordering, and comments, so reformatting the CSS does not fail the gate. A
 * token that is actually gone, extra, or changed still does.
 *
 * The `.dark` overrides are not compared: they redefine the same names, and
 * folding two values per name into one JSON entry would make the JSON ambiguous.
 */

import { readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "../../..");
const read = (path) => readFileSync(join(root, path), "utf8");

/** The declarations of the `@theme` block, with comments stripped. */
function themeDeclarations(source) {
  const start = source.indexOf("@theme");
  if (start === -1) {
    throw new Error("apps/frontend/src/app/globals.css has no @theme block");
  }

  let depth = 0;
  let end = -1;
  for (let index = source.indexOf("{", start); index < source.length; index += 1) {
    if (source[index] === "{") {
      depth += 1;
    } else if (source[index] === "}") {
      depth -= 1;
      if (depth === 0) {
        end = index;
        break;
      }
    }
  }
  if (end === -1) {
    throw new Error("the @theme block is not closed");
  }

  const body = source
    .slice(source.indexOf("{", start) + 1, end)
    .replace(/\/\*[\s\S]*?\*\//g, "");

  const declarations = new Map();
  for (const match of body.matchAll(/^\s*(--[a-z0-9-]+)\s*:\s*([^;]+);/gm)) {
    declarations.set(match[1], match[2].trim());
  }
  if (declarations.size === 0) {
    throw new Error("the @theme block declares no custom properties");
  }
  return declarations;
}

/** `{ color: { "bolu-bg": "#f5f6fb" } }` as `--color-bolu-bg` → `#f5f6fb`. */
function flatten(value, path = [], flattened = new Map()) {
  if (typeof value === "string") {
    if (path.length < 2) {
      throw new Error(
        `packages/design-tokens/tokens.json has a value outside a group: ${path.join(".")}`,
      );
    }
    flattened.set(`--${path.join("-")}`, value);
    return flattened;
  }
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(
      `packages/design-tokens/tokens.json has a non-string, non-object value at ${path.join(".") || "<root>"}`,
    );
  }
  for (const [key, child] of Object.entries(value)) {
    flatten(child, [...path, key], flattened);
  }
  return flattened;
}

function jsonDeclarations() {
  const tokens = JSON.parse(read("packages/design-tokens/tokens.json"));
  const declarations = flatten(tokens);
  if (declarations.size === 0) {
    throw new Error("packages/design-tokens/tokens.json declares no tokens");
  }
  return declarations;
}

function difference(left, right) {
  return [...left].filter((name) => !right.has(name)).sort();
}

function list(names) {
  return names.length === 0 ? "none" : names.join(", ");
}

let css;
let json;
try {
  css = themeDeclarations(read("apps/frontend/src/app/globals.css"));
  json = jsonDeclarations();
} catch (error) {
  console.error(`design-tokens: ${error.message}`);
  process.exit(1);
}

const missing = difference(css.keys(), json);
const extra = difference(json.keys(), css);
const changed = [...css.keys()]
  .filter((name) => json.has(name) && json.get(name) !== css.get(name))
  .sort();

const failures = [];
if (missing.length > 0) {
  failures.push(`  packages/design-tokens/tokens.json is missing: ${list(missing)}`);
}
if (extra.length > 0) {
  failures.push(
    `  packages/design-tokens/tokens.json names tokens the @theme block does not: ${list(extra)}`,
  );
}
for (const name of changed) {
  failures.push(
    `  ${name} differs: globals.css has \`${css.get(name)}\`, tokens.json has \`${json.get(name)}\``,
  );
}

if (failures.length > 0) {
  console.error("design-tokens: tokens.json disagrees with the @theme block in globals.css");
  console.error(failures.join("\n"));
  process.exit(1);
}

console.log(`design-tokens: ${css.size} tokens agree with the @theme block in globals.css`);