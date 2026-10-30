/**
 * The event → notification mapping.
 *
 * A push is a promise about a screen, so these cases pin the two things that
 * would be wrong silently: an event that must not notify, and a link that points
 * at an id the event does not carry. The copy itself is not asserted word for
 * word — a test that pins a sentence only makes rewording harder — except where
 * the title is what tells the person which screen is waiting.
 */

import { describe, expect, test } from "bun:test";

import {
  EventDraftCreated,
  EventMessageNew,
  EventNotification,
  EventRoutineRun,
  EventTaskFinished,
  EventTaskStarted,
  EventTaskWaiting,
} from "../../chat/domain/event.ts";
import { notificationFor } from "./push.ts";

const taskId = "44444444-4444-4444-4444-444444444444";
const draftId = "55555555-5555-5555-5555-555555555555";
const routineId = "66666666-6666-6666-6666-666666666666";

describe("notificationFor", () => {
  test("a finished task opens the task", () => {
    const notification = notificationFor({ type: EventTaskFinished, taskId });
    expect(notification?.deepLink).toBe(`bolu://task/${taskId}`);
    expect(notification?.title).toBe("Tugas selesai");
  });

  test("a waiting task opens the draft it is parked on", () => {
    const notification = notificationFor({ type: EventTaskWaiting, taskId, draftId });
    expect(notification?.deepLink).toBe(`bolu://approval/${draftId}`);
  });

  test("a waiting task without a draft falls back to the task", () => {
    const notification = notificationFor({ type: EventTaskWaiting, taskId, draftId: "" });
    expect(notification?.deepLink).toBe(`bolu://task/${taskId}`);
  });

  test("a waiting task with neither id notifies nobody", () => {
    expect(notificationFor({ type: EventTaskWaiting })).toBeNull();
  });

  test("a new draft opens the approval inbox on the draft", () => {
    const notification = notificationFor({ type: EventDraftCreated, draftId, taskId });
    expect(notification?.deepLink).toBe(`bolu://approval/${draftId}`);
  });

  test("a routine run opens the routine", () => {
    const notification = notificationFor({ type: EventRoutineRun, routineId });
    expect(notification?.deepLink).toBe(`bolu://routine/${routineId}`);
  });

  test("a pending notification opens its draft", () => {
    const notification = notificationFor({ type: EventNotification, draftId });
    expect(notification?.deepLink).toBe(`bolu://approval/${draftId}`);
  });

  test("every mapped event carries a deep link and a title", () => {
    const events = [
      { type: EventTaskFinished, taskId },
      { type: EventTaskWaiting, draftId },
      { type: EventDraftCreated, draftId },
      { type: EventRoutineRun, routineId },
      { type: EventNotification, draftId },
    ];
    for (const event of events) {
      const notification = notificationFor(event);
      expect(notification?.deepLink.startsWith("bolu://")).toBe(true);
      expect(notification?.title.trim()).not.toBe("");
      expect(notification?.body.trim()).not.toBe("");
    }
  });

  test("an event outside the five is quiet, not an error", () => {
    expect(notificationFor({ type: EventTaskStarted, taskId })).toBeNull();
    expect(notificationFor({ type: EventMessageNew, taskId })).toBeNull();
    expect(notificationFor({ type: "something.else" })).toBeNull();
  });
});
