"use client";

import { useSyncExternalStore } from "react";

const WIDE_BREAKPOINT = 1280;
const QUERY = `(min-width: ${WIDE_BREAKPOINT}px)`;

/**
 * useIsWide reports whether the viewport has room for a third column: the chat's
 * thread panel beside the sidebar and the conversation.
 *
 * It is the same subscription as `useIsMobile`, asked the other question. The
 * server snapshot is `false`, so the panel is a drawer until the browser says
 * there is room for it, rather than a column that collapses after hydration.
 */
export function useIsWide(): boolean {
  return useSyncExternalStore(subscribe, isWide, () => false);
}

function subscribe(onChange: () => void): () => void {
  const query = window.matchMedia(QUERY);
  query.addEventListener("change", onChange);
  return () => query.removeEventListener("change", onChange);
}

function isWide(): boolean {
  return window.matchMedia(QUERY).matches;
}
