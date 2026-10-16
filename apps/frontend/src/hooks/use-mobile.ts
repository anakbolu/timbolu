"use client";

import { useSyncExternalStore } from "react";

const MOBILE_BREAKPOINT = 768;
const QUERY = `(max-width: ${MOBILE_BREAKPOINT - 1}px)`;

/**
 * useIsMobile reports whether the viewport is narrower than the sidebar's mobile
 * breakpoint.
 *
 * It is `useSyncExternalStore` rather than a `useState` fed by an effect, which is
 * the same thing stated as a subscription: the media query is the external store,
 * `subscribe` is the listener, and the snapshot is read at render time instead of
 * being copied into state one render late. That copy is what the registry's
 * original does — it sets the value synchronously inside the effect — and it
 * costs a second render on every mount plus a window where the sidebar renders
 * the wrong variant.
 *
 * The server snapshot is `false`: there is no viewport to measure, and the
 * desktop layout is the one the shell is built around. React replaces it with the
 * real reading after hydration.
 */
export function useIsMobile(): boolean {
  return useSyncExternalStore(subscribe, isNarrow, () => false);
}

/** subscribe wires the media query's change events to React's re-render. */
function subscribe(onChange: () => void): () => void {
  const query = window.matchMedia(QUERY);
  query.addEventListener("change", onChange);
  return () => query.removeEventListener("change", onChange);
}

/** isNarrow reads the viewport, which is the store's current value. */
function isNarrow(): boolean {
  return window.innerWidth < MOBILE_BREAKPOINT;
}
