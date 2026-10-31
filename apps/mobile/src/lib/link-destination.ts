/**
 * Where a parsed link lands in this build, and what the landing screen says.
 *
 * Every target currently lands on the Bolu list: the app has no task, approval,
 * routine, or conversation-by-id screen yet (issue #499, "Di luar scope"). The
 * sentences live here so the same news is not written three times, and so a
 * later issue can swap one branch for a real screen without hunting for copy.
 */

import type { LinkTarget } from "@/lib/deep-link";

/** `+not-found` shows exactly this sentence, next to its way out. */
export const UNKNOWN_LINK_NOTICE = "Tautan ini tidak dikenal.";

/**
 * One notice for every resource whose screen is not in the app. Chat belongs
 * here too: `chat/[agentID]` opens a direct thread from an *agent* id, while a
 * `bolu://chat/<id>` link carries a *conversation* id. Handing one to the other
 * would open the wrong thread, so the app does not try.
 */
const MISSING_SCREEN_NOTICE =
  "Layar yang dituju tautan ini belum ada di app. Pilih Bolu di bawah untuk melanjutkan.";

const UNKNOWN_LINK_HINT = `${UNKNOWN_LINK_NOTICE} Pilih Bolu di bawah untuk melanjutkan.`;

export type LinkDestination = {
  pathname: "/(tabs)/bolu";
  notice: string;
};

export function destinationFor(target: LinkTarget): LinkDestination {
  // Rule 5: an unknown path falls to the list, not to an error screen. A bad id
  // is the same news for the person holding the phone — the id is a schema
  // detail, not a state they can act on.
  if (target.kind === "fallback") return { pathname: "/(tabs)/bolu", notice: UNKNOWN_LINK_HINT };

  return { pathname: "/(tabs)/bolu", notice: MISSING_SCREEN_NOTICE };
}
