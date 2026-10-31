/**
 * expo-router routes a URL by matching its path against file names. That is
 * wrong for this app: `bolu://chat/<conversation id>` would land on
 * `chat/[agentID]`, which expects an agent id and would open (or create) a
 * thread the link never named. The same applies to the universal links the app
 * claims (`https://timbolu.com/tasks/<id>`), whose paths are web paths, not
 * route files.
 *
 * `null` means "the router stays where it is", and the app starts at the gate as
 * usual; `src/lib/deep-link.ts` decides the real destination and the root layout
 * drives the navigation. Every other URL keeps the router's own handling, so a
 * foreign host, a browser-owned path like `/login`, or a path without a screen
 * file still ends at the router or `+not-found`.
 */
import { isAppLink } from "@/lib/deep-link";

export function redirectSystemPath({ path }: { path: string; initial: boolean }): string | null {
  return isAppLink(path) ? null : path;
}