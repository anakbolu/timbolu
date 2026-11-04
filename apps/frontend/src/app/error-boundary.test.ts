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
import type { ReactElement, ReactNode } from "react";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
// `bun:test` ships no types for the Next build's `tsc` run; the module only exists under `bun test`.
// @ts-ignore -- typed by bun's runner, not by the frontend's tsconfig
import { mock } from "bun:test";
import RootError from "./error";
import WorkspaceError from "./(workspace)/error";

// next/font/google exports loader functions in Next.js builds, but under bun test
// the virtual module must be stubbed before importing global-error.tsx.
// In ESM, static imports evaluate before top-level statements, so mock.module must run
// before global-error.tsx is loaded via dynamic import.
mock.module("next/font/google", () => ({
  Figtree: () => ({ variable: "--font-figtree" }),
  Fredoka: () => ({ variable: "--font-fredoka" }),
}));

const APP_DIR = path.resolve(import.meta.dirname, ".");

type ElementNode = ReactElement<{
  children?: ReactNode;
  onClick?: () => void;
  href?: string;
  [key: string]: unknown;
}>;

/**
 * Renders a React component using react-dom/server while capturing its top-level
 * ReactElement tree. Using a capture wrapper ensures React's server-side dispatcher
 * is properly initialized for components using client hooks like useState.
 */
function renderAndInspect<P extends object>(
  Component: (props: P) => ReactElement | null,
  props: P
): { html: string; vnode: ElementNode | null } {
  let vnode: ElementNode | null = null;
  function Capture(p: P) {
    vnode = Component(p) as ElementNode | null;
    return vnode;
  }
  const html = renderToStaticMarkup(createElement(Capture, props));
  return { html, vnode };
}

function findInTree(
  node: ReactNode,
  predicate: (el: ElementNode) => boolean
): ElementNode[] {
  const matches: ElementNode[] = [];
  function walk(current: ReactNode) {
    if (!current || typeof current !== "object") return;
    if ("type" in current && "props" in current) {
      const el = current as ElementNode;
      if (predicate(el)) {
        matches.push(el);
      }
      if (el.props?.children) {
        const children = Array.isArray(el.props.children)
          ? el.props.children
          : [el.props.children];
        for (const child of children) {
          walk(child);
        }
      }
    }
  }
  walk(node);
  return matches;
}

test("global-error.tsx renders root HTML document with Indonesian copy and functioning recovery handler", async () => {
  const filePath = path.join(APP_DIR, "global-error.tsx");
  assert.ok(fs.existsSync(filePath), "global-error.tsx must exist in src/app");

  // Dynamic import required here: mock.module("next/font/google") above must register
  // before global-error.tsx evaluates its top-level Figtree/Fredoka calls.
  const { default: GlobalError } = await import("./global-error");
  let resetCalled = false;
  let retryCalled = false;
  const { html, vnode } = renderAndInspect(GlobalError, {
    error: new Error("boom"),
    reset() {
      resetCalled = true;
    },
    retry() {
      retryCalled = true;
    },
  });

  assert.ok(vnode !== null, "GlobalError must return a render tree, not null");
  assert.ok(html.length > 0, "GlobalError rendered HTML must not be empty");
  assert.ok(html.includes("<html"), "GlobalError must define its own <html> tag");
  assert.ok(html.includes("<body"), "GlobalError must define its own <body> tag");
  assert.ok(html.includes("Ada yang salah di aplikasi"), "GlobalError must render Indonesian error copy");
  assert.ok(html.includes("Coba lagi"), 'GlobalError must render "Coba lagi" button');
  assert.ok(html.includes("/dashboard"), "GlobalError must render link to /dashboard");

  const buttons = findInTree(vnode, (el) => el.type === "button");
  assert.ok(buttons.length > 0, "GlobalError must render at least one button");
  const retryBtn = buttons.find((btn) => {
    const text = typeof btn.props.children === "string" ? btn.props.children : "";
    return text.includes("Coba lagi");
  });
  assert.ok(retryBtn, 'GlobalError must have a button with "Coba lagi" text');
  assert.equal(typeof retryBtn.props.onClick, "function", "Coba lagi button must have an onClick handler");

  retryBtn.props.onClick!();
  assert.ok(
    resetCalled || retryCalled,
    "Clicking Coba lagi must invoke the recovery handler (reset or retry)"
  );
});

test("(workspace)/error.tsx renders Indonesian copy, dashboard link, and invokes recovery handler", async () => {
  const filePath = path.join(APP_DIR, "(workspace)", "error.tsx");
  assert.ok(fs.existsSync(filePath), "(workspace)/error.tsx must exist");

  // WorkspaceError is statically imported at the top of this file

  let resetCalled = false;
  let retryCalled = false;
  const { html, vnode } = renderAndInspect(WorkspaceError, {
    error: new Error("boom"),
    reset() {
      resetCalled = true;
    },
    retry() {
      retryCalled = true;
    },
  });

  assert.ok(vnode !== null, "WorkspaceError must return a render tree, not null");
  assert.ok(html.length > 0, "WorkspaceError rendered HTML must not be empty");
  assert.ok(html.includes("Ada yang salah di halaman ini"), "WorkspaceError must render Indonesian error copy");
  assert.ok(html.includes("Coba lagi"), 'WorkspaceError must render "Coba lagi" button');
  assert.ok(html.includes("Kembali ke dasbor"), 'WorkspaceError must render "Kembali ke dasbor" text');
  assert.ok(html.includes("/dashboard"), "WorkspaceError must render link to /dashboard");

  const buttons = findInTree(vnode, (el) => el.type === "button");
  assert.ok(buttons.length > 0, "WorkspaceError must render at least one button");
  const retryBtn = buttons.find((btn) => {
    const text = typeof btn.props.children === "string" ? btn.props.children : "";
    return text.includes("Coba lagi");
  });
  assert.ok(retryBtn, 'WorkspaceError must have a button with "Coba lagi" text');
  assert.equal(typeof retryBtn.props.onClick, "function", "Coba lagi button must have an onClick handler");

  retryBtn.props.onClick!();
  assert.ok(
    resetCalled || retryCalled,
    "Clicking Coba lagi must invoke the recovery handler (reset or retry)"
  );
});

test("root error.tsx renders Indonesian copy, home link, and invokes recovery handler", async () => {
  const filePath = path.join(APP_DIR, "error.tsx");
  assert.ok(fs.existsSync(filePath), "error.tsx must exist in src/app");

  // RootError is statically imported at the top of this file

  let resetCalled = false;
  let retryCalled = false;
  const { html, vnode } = renderAndInspect(RootError, {
    error: new Error("boom"),
    reset() {
      resetCalled = true;
    },
    retry() {
      retryCalled = true;
    },
  });

  assert.ok(vnode !== null, "RootError must return a render tree, not null");
  assert.ok(html.length > 0, "RootError rendered HTML must not be empty");
  assert.ok(html.includes("Ada yang salah di halaman ini"), "RootError must render Indonesian error copy");
  assert.ok(html.includes("Coba lagi"), 'RootError must render "Coba lagi" button');
  assert.ok(html.includes("Kembali ke beranda"), 'RootError must render "Kembali ke beranda" text');
  assert.ok(html.includes('href="/"'), 'RootError must render link to home "/"');

  const buttons = findInTree(vnode, (el) => el.type === "button");
  assert.ok(buttons.length > 0, "RootError must render at least one button");
  const retryBtn = buttons.find((btn) => {
    const text = typeof btn.props.children === "string" ? btn.props.children : "";
    return text.includes("Coba lagi");
  });
  assert.ok(retryBtn, 'RootError must have a button with "Coba lagi" text');
  assert.equal(typeof retryBtn.props.onClick, "function", "Coba lagi button must have an onClick handler");

  retryBtn.props.onClick!();
  assert.ok(
    resetCalled || retryCalled,
    "Clicking Coba lagi must invoke the recovery handler (reset or retry)"
  );

  // Also verify RootError works when only reset() is supplied (retry is optional)
  let fallbackResetCalled = false;
  const fallback = renderAndInspect(RootError, {
    error: new Error("boom"),
    reset() {
      fallbackResetCalled = true;
    },
  });
  const fallbackBtn = findInTree(fallback.vnode, (el) => el.type === "button")[0];
  fallbackBtn.props.onClick!();
  assert.ok(fallbackResetCalled, "Clicking Coba lagi must invoke reset() when retry is omitted");
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
