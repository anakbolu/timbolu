/**
 * Where a parsed link lands in this build, and what the landing screen says.
 *
 * A task link now has a screen of its own (`app/task/[taskID].tsx`). The other
 * three still land on the Bolu list: chat needs a conversation-by-id screen
 * (`chat/[agentID]` opens from an *agent* id), approval's screen exists but its
 * link is mapped separately, and routine has no screen at all. The sentences live
 * here so the same news is not written three times, and so a later issue can swap
 * one branch for a real screen without hunting for copy.
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

/**
 * A destination is either the Bolu list with a sentence, or a detail screen that
 * reads its id from the route. `notice: null` clears a banner an earlier link
 * left behind — the screen it lands on says everything that needs saying.
 */
export type LinkDestination =
  | { kind: "list"; notice: string }
  | { kind: "task"; taskID: string; notice: null };

export function destinationFor(target: LinkTarget): LinkDestination {
  // Rule 5: an unknown path falls to the list, not to an error screen. A bad id
  // is the same news for the person holding the phone — the id is a schema
  // detail, not a state they can act on.
  if (target.kind === "fallback") return { kind: "list", notice: UNKNOWN_LINK_HINT };

  // The screen decides what a missing or foreign task reads like
  // (`task_not_found` and `invalid_input` are the same "Tugas tidak ditemukan"
  // there), so the link itself carries no notice.
  if (target.kind === "task") return { kind: "task", taskID: target.taskID, notice: null };

  return { kind: "list", notice: MISSING_SCREEN_NOTICE };
}
