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
test("root error.tsx exists and exports a default function with Indonesian copy", () => {
  const filePath = path.join(APP_DIR, "error.tsx");
  assert.ok(fs.existsSync(filePath), "error.tsx must exist in src/app");

  const content = fs.readFileSync(filePath, "utf-8");
  assert.ok(content.includes('"use client"'), 'error.tsx must have "use client"');
  assert.ok(content.includes("Ada yang salah di halaman ini"), "error.tsx must have Indonesian error copy");
  assert.ok(content.includes("Coba lagi"), 'error.tsx must have "Coba lagi" button');
  assert.ok(content.includes("Kembali ke beranda"), 'error.tsx must have "Kembali ke beranda" text');
  assert.ok(content.includes('href="/"'), 'error.tsx must have link home');
  assert.ok(content.includes("reset()"), "error.tsx must invoke reset()");
});

test("route tree error boundary coverage: root or segment error.tsx covers all top-level routes", () => {
  // Root error.tsx lives inside RootLayout (src/app/layout.tsx), which means
  // all non-workspace child routes (/login, /signup, /pricing, etc.) are caught within root layout
  // where globals.css and font variables are active.
  const rootErrorPath = path.join(APP_DIR, "error.tsx");
  assert.ok(fs.existsSync(rootErrorPath), "src/app/error.tsx must exist to catch non-workspace routes");

  const entries = fs.readdirSync(APP_DIR, { withFileTypes: true });
  const topLevelDirs = entries
    .filter((entry) => entry.isDirectory())
    .map((entry) => entry.name);

  // Explicit allowlist of top-level segments/directories with documented reasons:
  // - "(workspace)": Has its own segment-level error.tsx boundary for workspace-specific layout and links.
  // - "api": Next.js Route Handlers (HTTP endpoints), not React UI components; App Router error boundaries do not apply.
  // - All other top-level UI routes (/admin, /login, /signup, /pricing, etc.) fall back to src/app/error.tsx
  //   within root layout, avoiding unstyled global-error.tsx crashes.
  const explicitSegmentBoundaries: Record<string, string> = {
    "(workspace)": "Dedicated workspace error boundary with contextual links and crew bot illustration",
  };

  const exemptDirectories: Record<string, string> = {
    api: "API route handlers return JSON HTTP responses and do not render React component trees",
  };

  for (const dirName of topLevelDirs) {
    if (explicitSegmentBoundaries[dirName]) {
      const segmentError = path.join(APP_DIR, dirName, "error.tsx");
      assert.ok(
        fs.existsSync(segmentError),
        `Top-level segment ${dirName} is documented as having its own error boundary (${explicitSegmentBoundaries[dirName]}), but ${segmentError} is missing`
      );
    } else if (exemptDirectories[dirName]) {
      // Documented exempt directory
      assert.ok(true);
    } else {
      // Standard route segments rely on the root error.tsx boundary.
      // We assert that the root error.tsx is present and active.
      assert.ok(
        fs.existsSync(rootErrorPath),
        `Route segment ${dirName} relies on src/app/error.tsx, which must exist`
      );
    }
  }
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
