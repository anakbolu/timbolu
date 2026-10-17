/**
 * One client's view of the workspace stream: the backlog it missed, then
 * everything live.
 *
 * The order of the two steps is what makes the stream complete. The subscription
 * is made before the backlog is read, so an event that happens in between is in
 * the backlog (it is written before it is published) and a duplicate is dropped
 * by its id rather than shown twice.
 *
 * Ported 1:1 from internal/modules/chat/service/stream.go.
 */

import { isNil } from "../domain/conversation.ts";
import type { Event, EventPublisher, EventStore } from "../domain/event.ts";
import type { Scope } from "../domain/ports.ts";

/** Bounds one replay query, so a long absence is paged rather than one read. */
const replayChunk = 500;

/** What the stream needs. */
export interface StreamDeps {
  events: EventStore;
  publisher: EventPublisher | null;
}

/**
 * Returns one client's view of the workspace stream: the backlog it missed, then
 * everything live. The iterable ends when the signal aborts.
 */
export async function* streamEvents(
  deps: StreamDeps,
  scope: Scope,
  afterId: number,
  signal?: AbortSignal,
): AsyncGenerator<Event> {
  if (isNil(scope.workspaceId)) {
    throw new Error("chat: invalid input: workspace is required");
  }
  if (deps.publisher === null) {
    throw new Error("chat: the live stream is not configured");
  }

  // Subscribe before reading the backlog: the publisher's confirmation makes
  // every later publish ordered after this point, so nothing slips between the
  // two steps.
  const live = await deps.publisher.subscribe(scope.workspaceId, signal);

  const backlog = await backlogEvents(deps.events, scope.workspaceId, afterId);

  let last = afterId;
  for (const event of backlog) {
    if (signal?.aborted) return;
    yield event;
    last = event.id;
  }

  // The buffer is the publisher's own; this loop only has to drop what the
  // backlog already delivered.
  for await (const event of live) {
    if (signal?.aborted) return;
    if (event.id <= last) {
      // Already delivered from the backlog; the two paths overlap by design.
      continue;
    }
    yield event;
    last = event.id;
  }
}

/**
 * Reads everything after an id, in pages, so a long absence does not turn into
 * one unbounded query.
 */
async function backlogEvents(events: EventStore, workspaceId: string, afterId: number): Promise<Event[]> {
  const out: Event[] = [];
  let cursor = afterId;

  for (;;) {
    const page = await events.sinceEvents(workspaceId, cursor, replayChunk);
    if (page.length === 0) {
      return out;
    }

    out.push(...page);
    cursor = page[page.length - 1].id;
    if (page.length < replayChunk) {
      return out;
    }

    // A hard stop: a client asking for a backlog of a million events is either a
    // bug or an attack, and neither should hold a connection.
    if (out.length >= replayChunk * 8) {
      return out;
    }
  }
}
