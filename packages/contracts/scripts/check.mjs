#!/usr/bin/env node
/**
 * Fails when the realtime event names drift apart.
 *
 * `docs/api.md` is the source of truth. Three code locations must agree with it:
 * `packages/contracts/events.ts`, the Bun constants under `apps/backend-js`
 * (the tree that serves production), and the Go constants under `apps/backend/internal`
 * (the reference tree). The frontend may name a subset — it lists only the events
 * its screens act on — but it must not invent a name the docs do not have.
 *
 * The parsers deliberately key on the event-name shape (`word.word`) and ignore
 * whitespace, ordering, and comments, so reformatting a file does not fail the
 * gate. A name that is actually gone still does.
 */

import { readFileSync, readdirSync, statSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "../../..");
const read = (path) => readFileSync(join(root, path), "utf8");
const EVENT_NAME = /^[a-z][a-z0-9_]*(?:\.[a-z][a-z0-9_]*)+$/;

/**
 * The two stream events the web client does not act on: `task.step` is a task's
 * internal progress, and `notification.pending` is the notification worker's
 * hook. Listing the exception here keeps it deliberate — every other docs event
 * must appear in the frontend, so adding one and forgetting the client fails.
 */
const CLIENT_IGNORED = new Set(["task.step", "notification.pending"]);

function namesIn(values) {
  return new Set(values.filter((value) => EVENT_NAME.test(value)));
}

function quotedNames(body) {
  const bare = body
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/\/\/[^\n]*/g, "");
  return [...bare.matchAll(/"([^"\n]+)"/g)].map((match) => match[1]);
}

/** The names in a `const NAME = [ ... ]` tuple, comments included. */
function tupleNames(source, name) {
  const match = source.match(
    new RegExp(`${name}(?:\\s*:[^=]+)?\\s*=\\s*\\[([\\s\\S]*?)\\]`),
  );
  if (!match) {
    throw new Error(`${name} was not found`);
  }
  return quotedNames(match[1]);
}

function docsNames() {
  const source = read("docs/api.md");
  const start = source.indexOf("### The stream");
  if (start === -1) {
    throw new Error("docs/api.md has no '### The stream' section");
  }

  const section = source.slice(start);
  const match = section.match(/\bTypes:([\s\S]*?)(?:\n[ \t]*\n|$)/);
  if (!match) {
    throw new Error("the stream section has no 'Types:' list");
  }

  const values = [...match[1].matchAll(/`([^`]+)`/g)].map((item) => item[1]);
  return namesIn(values);
}

function goNames() {
  const values = [];
  const walk = (directory) => {
    for (const entry of readdirSync(directory)) {
      const path = join(directory, entry);
      if (statSync(path).isDirectory()) {
        walk(path);
        continue;
      }
      if (!entry.endsWith(".go") || entry.endsWith("_test.go")) {
        continue;
      }
      const source = readFileSync(path, "utf8");
      for (const match of source.matchAll(
        /^\s*Event[A-Za-z0-9_]+\s*=\s*"([^"]+)"/gm,
      )) {
        values.push(match[1]);
      }
    }
  };
  walk(join(root, "apps/backend/internal"));
  return namesIn(values);
}

/**
 * The event names declared by the Bun backend (`apps/backend-js`).
 *
 * Production runs this tree (timbolu.com/api/**), not the Go reference tree.
 * Renaming or dropping an event constant here without updating docs/api.md or
 * the contracts must fail the gate so production does not emit events the
 * client and contracts know nothing about.
 */
function bunNames() {
  const path = "apps/backend-js/src/modules/chat/domain/event.ts";
  let source;
  try {
    source = read(path);
  } catch (error) {
    throw new Error(`cannot read ${path}: ${error.message}`);
  }
  const values = [];
  for (const match of source.matchAll(
    /^\s*export\s+const\s+Event[A-Za-z0-9_]+\s*=\s*"([^"]+)"/gm,
  )) {
    values.push(match[1]);
  }
  if (values.length === 0) {
    throw new Error(`${path} declares no 'export const Event<Name> = "..."' constants`);
  }
  return namesIn(values);
}

function contractsNames() {
  const source = read("packages/contracts/events.ts");
  const chat = namesIn(tupleNames(source, "CHAT_EVENT_TYPES"));
  const realtime = namesIn(tupleNames(source, "REALTIME_EVENT_TYPES"));
  for (const name of chat) {
    if (!realtime.has(name)) {
      throw new Error(`CHAT_EVENT_TYPES names ${name}, which REALTIME_EVENT_TYPES does not`);
    }
  }
  return new Set([...chat, ...realtime]);
}

/**
 * The chat names as the frontend sees them.
 *
 * The frontend used to keep its own copy of `CHAT_EVENT_TYPES` because the image
 * was built with `apps/frontend` as its context (#328). It now imports the shared
 * tuple (#343), so the names come from `packages/contracts/events.ts`; the rest
 * of the client's list is still declared locally and parsed here.
 */
const SHARED_CHAT_IMPORT =
  /import\s*\{[^}]*\bCHAT_EVENT_TYPES\b[^}]*\}\s*from\s*"@timbolu\/contracts\/events"/;

function frontendNames() {
  const source = read("apps/frontend/src/lib/realtime.ts");
  const chat = SHARED_CHAT_IMPORT.test(source)
    ? namesIn(tupleNames(read("packages/contracts/events.ts"), "CHAT_EVENT_TYPES"))
    : namesIn(tupleNames(source, "CHAT_EVENT_TYPES"));
  const realtime = namesIn(tupleNames(source, "REALTIME_EVENT_TYPES"));
  return new Set([...chat, ...realtime]);
}

function difference(left, right) {
  return [...left].filter((value) => !right.has(value)).sort();
}

function list(values) {
  return values.length === 0 ? "none" : values.join(", ");
}

let docs;
let contracts;
let bun;
let go;
let frontend;
try {
  docs = docsNames();
  contracts = contractsNames();
  go = goNames();
  bun = bunNames();
  frontend = frontendNames();
} catch (error) {
  console.error(`contracts: ${error.message}`);
  process.exit(1);
}

const failures = [];
for (const [label, names] of [
  ["packages/contracts/events.ts", contracts],
  ["apps/backend/internal", go],
  ["apps/backend-js/src/modules/chat/domain/event.ts", bun],
]) {
  const missing = difference(docs, names);
  const extra = difference(names, docs);
  if (missing.length > 0) {
    failures.push(`  ${label} is missing: ${list(missing)}`);
  }
  if (extra.length > 0) {
    failures.push(`  ${label} names events the docs do not: ${list(extra)}`);
  }
}

const unknown = difference(frontend, docs);
if (unknown.length > 0) {
  failures.push(
    `  apps/frontend/src/lib/realtime.ts names events the docs do not: ${list(unknown)}`,
  );
}

const expectedClient = difference(docs, CLIENT_IGNORED);
const missingClient = difference(expectedClient, frontend);
if (missingClient.length > 0) {
  failures.push(
    `  apps/frontend/src/lib/realtime.ts is missing: ${list(missingClient)}`,
  );
}

if (failures.length > 0) {
  console.error("contracts: event names disagree with docs/api.md");
  console.error(failures.join("\n"));
  process.exit(1);
}

const ignored = difference(docs, frontend);
const note = ignored.length === 0 ? "" : `; frontend omits ${list(ignored)} deliberately`;
console.log(`contracts: ${docs.size} event types agree${note}`);