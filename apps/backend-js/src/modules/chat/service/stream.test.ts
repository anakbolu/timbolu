/**
 * The stream's contract with the publisher is positional: the workspace first,
 * the abort signal second. Swapping them is invisible to the compiler because
 * the container builds the chat dependencies through `as any`, and at runtime it
 * turns the channel into `ws:[object AbortSignal]` and ends the stream right
 * after its head. These cases pin that order on both sides of the port.
 */

import { describe, expect, test } from "bun:test";

import type { Client } from "../../../platform/redis/redis.ts";
import { Publisher } from "../../../platform/pubsub/pubsub.ts";
import type { Event, EventPublisher, EventStore } from "../domain/event.ts";
import type { Scope } from "../domain/ports.ts";
import { streamEvents } from "./stream.ts";

const scope: Scope = { userId: "user-1", workspaceId: "ws-1" };

function event(id: number): Event {
  return {
    id,
    workspaceId: "ws-1",
    type: "message.new",
    actorAgentId: "agent-1",
    actorUserId: "user-1",
    conversationId: "conversation-1",
    taskId: "",
    draftId: "",
    payload: {},
    createdAt: new Date(0),
  };
}

/** Records how the stream called it, which is the thing under test. */
class RecordingPublisher implements EventPublisher {
  readonly calls: Array<{ workspaceId: string; signal: AbortSignal | undefined }> = [];

  constructor(private readonly live: Event[]) {}

  async publish(): Promise<void> {}

  async subscribe(workspaceId: string, signal?: AbortSignal): Promise<AsyncIterable<Event>> {
    this.calls.push({ workspaceId, signal });
    const live = this.live;
    return {
      async *[Symbol.asyncIterator]() {
        yield* live;
      },
    };
  }
}

class BacklogStore implements EventStore {
  constructor(private readonly events: Event[]) {}

  async appendEvent(event: Event): Promise<Event> {
    return event;
  }

  async sinceEvents(_workspaceId: string, afterId: number, limit: number): Promise<Event[]> {
    return this.events.filter((e) => e.id > afterId).slice(0, limit);
  }

  async latestEvents(): Promise<Event[]> {
    return [];
  }
}

describe("streamEvents", () => {
  test("subscribes with the workspace first and the signal second", async () => {
    const controller = new AbortController();
    // The live path replays id 2, which the backlog already delivered, so the
    // dedupe is exercised rather than assumed.
    const publisher = new RecordingPublisher([event(2), event(3)]);
    const store = new BacklogStore([event(1), event(2)]);

    const stream = streamEvents({ events: store, publisher }, scope, 0, controller.signal);
    const ids: number[] = [];
    for await (const e of stream) {
      ids.push(e.id);
    }

    expect(publisher.calls).toHaveLength(1);
    expect(publisher.calls[0].workspaceId).toBe("ws-1");
    expect(publisher.calls[0].signal).toBe(controller.signal);
    expect(ids).toEqual([1, 2, 3]);
  });
});

describe("Publisher.subscribe", () => {
  test("names the channel from the workspace, not from the signal", async () => {
    const subscribed: string[] = [];
    const subscriber = {
      status: "ready",
      connect: async () => {},
      subscribe: async (name: string) => {
        subscribed.push(name);
      },
      on: () => {},
      off: () => {},
      quit: async () => {},
      disconnect: () => {},
    };
    // `Client.raw()` returns ioredis's full `Redis` surface, which no fake can
    // satisfy structurally; this stub covers only the members `subscribe` touches.
    const client = { raw: () => ({ duplicate: () => subscriber }) } as unknown as Client;
    const controller = new AbortController();

    const live = await new Publisher(client).subscribe("ws-1", controller.signal);
    controller.abort();
    // Draining releases the dedicated connection the way a closed stream does.
    for await (const _ of live) break;

    expect(subscribed).toEqual(["ws:ws-1"]);
  });
});
