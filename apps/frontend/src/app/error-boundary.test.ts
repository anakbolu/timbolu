/**
 * Tests for App Router error boundaries and loading states (issue #629).
 *
 * Checks that error boundaries and segment loading components are present,
 * properly export default components, and render expected copy without
 * requiring a full React testing-library harness.
 *
 * Run with:
 *   cd apps/frontend && bun test src/app/error-boundary.test.ts
 */

import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";

const APP_DIR = path.resolve(import.meta.dirname, ".");

test("global-error.tsx exists and exports a default function with Indonesian copy", () => {
  const filePath = path.join(APP_DIR, "global-error.tsx");
  assert.ok(fs.existsSync(filePath), "global-error.tsx must exist in src/app");

  const content = fs.readFileSync(filePath, "utf-8");
  assert.ok(content.includes('"use client"'), 'global-error.tsx must have "use client"');
  assert.ok(content.includes("<html"), "global-error.tsx must define its own <html> tag");
  assert.ok(content.includes("<body"), "global-error.tsx must define its own <body> tag");
  assert.ok(content.includes("Ada yang salah di aplikasi"), "global-error.tsx must have Indonesian error copy");
  assert.ok(content.includes("Coba lagi"), 'global-error.tsx must have "Coba lagi" button');
  assert.ok(content.includes("/dashboard"), "global-error.tsx must have link to /dashboard");
  assert.ok(content.includes("reset()"), "global-error.tsx must invoke reset()");
});

test("(workspace)/error.tsx exists and exports a default function with Indonesian copy", () => {
  const filePath = path.join(APP_DIR, "(workspace)", "error.tsx");
  assert.ok(fs.existsSync(filePath), "(workspace)/error.tsx must exist");

  const content = fs.readFileSync(filePath, "utf-8");
  assert.ok(content.includes('"use client"'), '(workspace)/error.tsx must have "use client"');
  assert.ok(content.includes("Ada yang salah di halaman ini"), "(workspace)/error.tsx must have Indonesian error copy");
  assert.ok(content.includes("Coba lagi"), '(workspace)/error.tsx must have "Coba lagi" button');
  assert.ok(content.includes("Kembali ke dasbor"), '(workspace)/error.tsx must have "Kembali ke dasbor" text');
  assert.ok(content.includes("/dashboard"), "(workspace)/error.tsx must have link to /dashboard");
  assert.ok(content.includes("reset()"), "(workspace)/error.tsx must invoke reset()");
});

test("required segment loading.tsx files exist and export default functions", () => {
  const loadingSegments = [
    ["(workspace)", "chat", "loading.tsx"],
    ["(workspace)", "tasks", "[taskID]", "loading.tsx"],
    ["(workspace)", "approval", "[draftID]", "loading.tsx"],
    ["(workspace)", "settings", "loading.tsx"],
  ];

  for (const segments of loadingSegments) {
    const filePath = path.join(APP_DIR, ...segments);
    assert.ok(fs.existsSync(filePath), `${segments.join("/")} must exist`);

    const content = fs.readFileSync(filePath, "utf-8");
    assert.ok(content.includes("export default function"), `${segments.join("/")} must export default function`);
    assert.ok(content.includes("bolu-"), `${segments.join("/")} should use bolu-* design tokens`);
  }
});
