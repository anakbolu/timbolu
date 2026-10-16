"use client";

import { useSyncExternalStore } from "react";

import { readResolvedTheme, readTheme, subscribeTheme, type ResolvedTheme, type Theme } from "@/lib/theme";

/**
 * useTheme reports the theme the person picked, and re-renders when it changes.
 *
 * It is the same `useSyncExternalStore` the mobile hook uses, for the same
 * reason: `localStorage` and the machine's own preference are the store, and the
 * snapshot is read at render time rather than copied into state one render late.
 *
 * The server snapshot is "system", which is also what the inline script in the
 * root layout leaves in the document when nothing has been chosen — so the first
 * client render agrees with the markup, and a chosen theme arrives with the
 * subscription rather than as a mismatch.
 */
export function useTheme(): Theme {
  return useSyncExternalStore(subscribeTheme, readTheme, () => "system" as const);
}

/**
 * useResolvedTheme is the same store answered as light or dark.
 *
 * It exists for the two places that cannot read a CSS variable: ECharts and
 * Mermaid paint outside the cascade and need a colour, not a token. Everything
 * drawn by CSS uses the token and does not need this.
 *
 * The subscription is `subscribeTheme`, which also watches the machine's own
 * preference — so a page following the system repaints its charts when the
 * system flips, and a page that chose does not.
 */
export function useResolvedTheme(): ResolvedTheme {
  return useSyncExternalStore(subscribeTheme, readResolvedTheme, () => "light" as const);
}
