/**
 * Where a parsed link lands in this build, and what the landing screen says.
 *
 * Three of the four targets have a screen of their own: `task`, `approval` and
 * `routine` open `/task/[taskID]`, `/draft/[draftID]` and `/routine/[routineID]`.
 * Chat still lands on the Bolu list: `chat/[agentID]` opens a thread from an
 * *agent* id while `bolu://chat/<id>` carries a *conversation* id, so no route
 * can honour it. The sentence lives here so the same news is not written twice,
 * and so a later issue can swap that branch for a real screen without hunting
 * for copy.
 */

import type { LinkTarget } from "@/lib/deep-link";

/** `+not-found` shows exactly this sentence, next to its way out. */
export const UNKNOWN_LINK_NOTICE = "Tautan ini tidak dikenal.";

/**
 * Chat's sentence. `chat/[agentID]` opens a direct thread from an *agent* id,
 * while a `bolu://chat/<id>` link carries a *conversation* id. Handing one to
 * the other would open the wrong thread, so the app does not try.
 */
const CHAT_LINK_NOTICE =
  "Layar yang dituju tautan ini belum ada di app. Pilih Bolu di bawah untuk melanjutkan.";

const UNKNOWN_LINK_HINT = `${UNKNOWN_LINK_NOTICE} Pilih Bolu di bawah untuk melanjutkan.`;

/**
 * A destination is either the Bolu list with a sentence, or a detail screen that
 * reads its id from the route. `notice: null` clears a banner an earlier link
 * left behind — the screen it lands on says everything that needs saying.
 */
export type LinkDestination =
  | { kind: "list"; notice: string }
  | { kind: "task"; taskID: string; notice: null }
  | { kind: "draft"; draftID: string; notice: null }
  | { kind: "routine"; routineID: string; notice: null };

export function destinationFor(target: LinkTarget): LinkDestination {
  // Rule 5: an unknown path falls to the list, not to an error screen. A bad id
  // is the same news for the person holding the phone — the id is a schema
  // detail, not a state they can act on.
  if (target.kind === "fallback") return { kind: "list", notice: UNKNOWN_LINK_HINT };

  // Each screen decides what a missing or foreign id reads like
  // (`task_not_found`, `draft_not_found` and `routine_not_found` are all
  // "… tidak ditemukan" there), so the link itself carries no notice.
  if (target.kind === "task") return { kind: "task", taskID: target.taskID, notice: null };
  if (target.kind === "approval") {
    return { kind: "draft", draftID: target.draftID, notice: null };
  }
  if (target.kind === "routine") {
    return { kind: "routine", routineID: target.routineID, notice: null };
  }

  // Only chat is left: the list is the closest screen the app can honestly open.
  return { kind: "list", notice: CHAT_LINK_NOTICE };
}