/**
 * expo-router routes a URL by matching its path against file names. That is
 * wrong for this app: `bolu://chat/<conversation id>` would land on
 * `chat/[agentID]`, which expects an agent id and would open (or create) a
 * thread the link never named.
 *
 * `null` means "the router stays where it is", and the app starts at the gate as
 * usual; `src/lib/deep-link.ts` decides the real destination and the root layout
 * drives the navigation. Every other URL keeps the router's own handling, so a
 * path without a screen file still ends at `+not-found`.
 */
export function redirectSystemPath({ path }: { path: string; initial: boolean }): string | null {
  return path.toLowerCase().startsWith("bolu://") ? null : path;
}
