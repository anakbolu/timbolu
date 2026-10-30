/**
 * The push contract: which activity events become a notification, what it says,
 * and where a tap lands.
 *
 * The mapping lives here rather than at each emitter because a push is a
 * promise about a screen. `docs/deep-links.md` fixes the `bolu://` shapes, and
 * an emitter only knows the id it already has; letting every call site build a
 * title and a link is how two screens end up disagreeing about the same draft.
 *
 * An event outside the five mapped here is not an error. Most of the stream is
 * for a client that is already open, and `notificationFor` answers null so the
 * sender stays quiet. The copy is Indonesian and deliberately generic: the
 * payload carries an id, not the task's title, and a notification that guessed
 * at content would be wrong more often than it is useful.
 */

import {
  EventDraftCreated,
  EventNotification,
  EventRoutineRun,
  EventTaskFinished,
  EventTaskWaiting,
} from "../../chat/domain/event.ts";

/**
 * The ids an activity event can point a notification at. They are optional
 * because one event type carries only the ids it needs, and a missing id is
 * answered as "nothing to open" rather than as a link to an empty path.
 */
export interface Event {
  type: string;
  taskId?: string | null;
  draftId?: string | null;
  routineId?: string | null;
}

/** One notification, reduced to what a phone shows and what a tap opens. */
export interface Notification {
  title: string;
  body: string;
  /** A `bolu://` link from `docs/deep-links.md`, never a bare id. */
  deepLink: string;
}

/** The tenant a send runs as. The user owns the tokens; the workspace is context. */
export interface Scope {
  userId?: string | null;
  workspaceId?: string | null;
}

/**
 * What a module hands the push service. Keeping this interface in the domain
 * means an emitter depends on "something that can notify", not on the Expo
 * client or the device registry.
 */
export interface Notifier {
  send(scope: Scope, event: Event): Promise<unknown>;
}

/**
 * Maps one event onto the notification a phone should receive, or null when
 * the event is not one of the five worth waking somebody for.
 */
export function notificationFor(event: Event): Notification | null {
  switch (event.type) {
    case EventTaskFinished:
      return at("task", event.taskId, "Tugas selesai", "Bolu sudah menyelesaikan tugasnya.");

    case EventTaskWaiting:
      // A task parked on a draft is answered at the draft; a task waiting for
      // any other reason still has a task screen, and a link that opened the
      // inbox without a draft would point at nothing.
      return (
        at("approval", event.draftId, "Menunggu persetujuan", "Bolu menunggu persetujuanmu untuk melanjutkan.") ??
        at("task", event.taskId, "Menunggu persetujuan", "Bolu menunggu persetujuanmu untuk melanjutkan.")
      );

    case EventDraftCreated:
      return at("approval", event.draftId, "Draf baru", "Bolu membuat draf yang perlu kamu putuskan.");

    case EventRoutineRun:
      return at("routine", event.routineId, "Rutinitas berjalan", "Rutinitas Bolu sudah dijalankan.");

    case EventNotification:
      return at("approval", event.draftId, "Ada yang perlu diputuskan", "Bolu menunggu keputusanmu.");

    default:
      return null;
  }
}

/**
 * Builds the notification for one resource, or null when the id is absent. An
 * empty id is not a route, so there is nothing to open and nothing to say.
 */
function at(
  resource: string,
  id: string | null | undefined,
  title: string,
  body: string,
): Notification | null {
  const value = (id ?? "").trim();
  return value === "" ? null : { title, body, deepLink: `bolu://${resource}/${value}` };
}
