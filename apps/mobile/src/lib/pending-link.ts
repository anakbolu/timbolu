/**
 * The link the app still owes a screen.
 *
 * It lives outside React on purpose: a link can arrive before the session is
 * known, and the sign-in flow replaces whole screens before it can be used.
 * `docs/deep-links.md` § Auth saat cold start, rule 2: not being signed in
 * postpones the link, it does not discard it.
 *
 * Process death needs no storage: the platform re-delivers the launch URL to the
 * next cold start, which captures it again.
 */

import type { LinkTarget } from "@/lib/deep-link";

let pending: LinkTarget | null = null;
const pendingListeners = new Set<() => void>();

function announce(): void {
  for (const listener of pendingListeners) listener();
}

/** Called by the root listener for every link this build can open. */
export function setPendingLink(target: LinkTarget): void {
  pending = target;
  announce();
}

export function getPendingLink(): LinkTarget | null {
  return pending;
}

export function subscribePendingLink(listener: () => void): () => void {
  pendingListeners.add(listener);
  return () => {
    pendingListeners.delete(listener);
  };
}

/**
 * Take the link so it is used exactly once. Called only once the session is
 * signed in; taking it earlier would spend it at the login screen.
 */
export function takePendingLink(): LinkTarget | null {
  const target = pending;
  pending = null;
  announce();
  return target;
}

/**
 * What the landing screen says about the link it could not honour. A store
 * rather than a route parameter because the destination may already be mounted
 * when the link is consumed: a parameter would be a second navigation racing
 * the first.
 */
let notice: string | null = null;
const noticeListeners = new Set<() => void>();

export function setLinkNotice(next: string | null): void {
  if (next === notice) return;
  notice = next;
  for (const listener of noticeListeners) listener();
}

export function getLinkNotice(): string | null {
  return notice;
}

export function subscribeLinkNotice(listener: () => void): () => void {
  noticeListeners.add(listener);
  return () => {
    noticeListeners.delete(listener);
  };
}
