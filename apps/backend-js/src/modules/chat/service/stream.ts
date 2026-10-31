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
import type { Logger } from "../../../platform/logger.ts";

/** Bounds one replay query, so a long absence is paged rather than one read. */
const replayChunk = 500;

/** What the stream needs. */
export interface StreamDeps {
  events: EventStore;
  publisher: EventPublisher | null;
  /** Where a live frame that cannot be read is reported. Optional in tests. */
  logger?: Logger | null;
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
  let skipped = 0;
  for await (const frame of live) {
    if (signal?.aborted) return;

    // The live path is the only one that comes back through JSON, and JSON has
    // no Date: `createdAt` arrives as the RFC3339 string the publisher wrote.
    // Handing that to a formatter that calls `toISOString()` throws, which used
    // to end the whole stream on the first live event — the backlog read from
    // the table kept working, so the stream looked "connected but silent".
    const event = reviveEvent(frame);
    if (event === null) {
      skipped++;
      if (skipped === 1) {
        deps.logger?.warn({ frame }, "chat: unreadable live event skipped");
      }
      continue;
    }

    if (event.id <= last) {
      // Already delivered from the backlog; the two paths overlap by design.
      continue;
    }
    yield event;
    last = event.id;
  }
}

/**
 * Rebuilds one event that came back over the wire.
 *
 * It returns null rather than throwing for a frame it cannot read: one broken
 * payload must not cost the client every event after it. `createdAt` is the
 * only field JSON cannot carry as-is, and it is required by every formatter
 * downstream.
 */
function reviveEvent(raw: unknown): Event | null {
  if (raw === null || typeof raw !== "object") {
    return null;
  }

  const source = raw as Record<string, unknown>;
  const createdAt = source.createdAt instanceof Date ? source.createdAt : new Date(String(source.createdAt));
  if (typeof source.id !== "number" || typeof source.type !== "string" || Number.isNaN(createdAt.getTime())) {
    return null;
  }

  return {
    id: source.id,
    workspaceId: typeof source.workspaceId === "string" ? source.workspaceId : "",
    type: source.type,
    actorAgentId: typeof source.actorAgentId === "string" ? source.actorAgentId : "",
    actorUserId: typeof source.actorUserId === "string" ? source.actorUserId : "",
    conversationId: typeof source.conversationId === "string" ? source.conversationId : "",
    taskId: typeof source.taskId === "string" ? source.taskId : "",
    draftId: typeof source.draftId === "string" ? source.draftId : "",
    payload: source.payload,
    createdAt,
  };
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
