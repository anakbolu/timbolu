/**
 * The live transports, proven end to end: the handler's own `sse` and `socket`,
 * a real `Bun.serve` on a port the kernel picks, and a real client reading
 * chunks/frames with their arrival time.
 *
 * The regression pinned here is #504. A live event does not come from the table:
 * it comes back through Redis, and JSON has no `Date`, so `createdAt` arrives as
 * a string. The formatter then threw on the first live event, the stream closed,
 * and the only symptom was a connection that said `: connected` and then went
 * quiet — the backlog kept working, so every check that replayed history stayed
 * green.
 *
 * The publisher stub round-trips through JSON on purpose. That is the one
 * difference between the two paths, and mocking it away is how the bug survived.
 *
 * Real timers are inherent here: the claim is about bytes crossing a socket over
 * a real interval, which fake timers cannot drive. Every wait is bounded by the
 * condition it awaits, so a failure names the missing frame rather than a
 * duration.
 */

import { afterEach, describe, expect, test } from "bun:test";
import { Hono } from "hono";

import { serveOptions } from "../../../cmd/serve-options.ts";
import type { AppEnv } from "../../../platform/context.ts";
import type { Logger } from "../../../platform/logger.ts";
import type { Event, EventPublisher, EventStore } from "../domain/event.ts";
import type { Service } from "../domain/service.ts";
import { streamEvents } from "../service/stream.ts";
import { Handler } from "./handler.ts";

const servers: Bun.Server<unknown>[] = [];

afterEach(() => {
  for (const server of servers.splice(0)) {
    server.stop(true);
  }
});

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
    payload: { id },
    createdAt: new Date(0),
  };
}

/** Emits the given events at the given offsets, then stays open like a live subscription. */
class WirePublisher implements EventPublisher {
  constructor(private readonly schedule: Array<{ at: number; event: Event }>) {}

  async publish(): Promise<void> {
    throw new Error("the stream under test never publishes");
  }

  async subscribe(_workspaceId: string, signal?: AbortSignal): Promise<AsyncIterable<Event>> {
    const schedule = this.schedule;
    return {
      async *[Symbol.asyncIterator]() {
        const started = performance.now();
        for (const { at, event: next } of schedule) {
          const wait = at - (performance.now() - started);
          if (wait > 0) {
            const delay = Promise.withResolvers<void>();
            setTimeout(delay.resolve, wait);
            await delay.promise;
          }
          if (signal?.aborted) return;
          // Exactly what a subscriber receives from Redis: the publisher's JSON.
          yield JSON.parse(JSON.stringify(next)) as Event;
        }

        const closed = Promise.withResolvers<void>();
        signal?.addEventListener("abort", () => closed.resolve(), { once: true });
        await closed.promise;
      },
    };
  }
}

const emptyStore: EventStore = {
  async appendEvent(event: Event): Promise<Event> {
    return event;
  },
  async sinceEvents(): Promise<Event[]> {
    return [];
  },
  async latestEvents(): Promise<Event[]> {
    return [];
  },
};

const quietLog = {
  info: () => {},
  warn: () => {},
  error: () => {},
  debug: () => {},
  trace: () => {},
  fatal: () => {},
  child: () => quietLog,
  isLevelEnabled: () => false,
} as unknown as Logger;

/** Boots the real handler routes on the serve options production uses. */
function listen(publisher: EventPublisher, heartbeat: number): Bun.Server<unknown> {
  const service = {
    stream: (scope: { userId: string; workspaceId: string }, afterId: number, signal?: AbortSignal) =>
      Promise.resolve(streamEvents({ events: emptyStore, publisher }, scope, afterId, signal)),
    // The two transports read nothing else off the service.
  } as unknown as Service;

  const handler = new Handler(service, quietLog).withHeartbeat(heartbeat);

  const app = new Hono<AppEnv>();
  app.use("*", async (c, next) => {
    c.set("scope", { userId: "user-1", workspaceId: "ws-1" });
    await next();
  });
  app.get("/events/stream", (c) => handler.sse(c));
  app.get("/events/socket", (c) => handler.socket(c));

  const server = Bun.serve(serveOptions({ hostname: "127.0.0.1", port: 0, fetch: app.fetch }));
  servers.push(server);
  return server;
}

interface Chunk {
  at: number;
  text: string;
}

/**
 * Reads the SSE response until `done` holds for the text so far, and returns
 * every chunk with its arrival time. A `timeout` ends the read anyway, so a
 * condition that never holds fails on the assertion rather than hanging.
 */
async function readSse(url: string, done: (text: string) => boolean, timeout: number): Promise<Chunk[]> {
  const started = performance.now();
  const response = await fetch(url, { headers: { Accept: "text/event-stream" } });
  expect(response.headers.get("content-type")).toBe("text/event-stream");

  const reader = response.body!.getReader();
  const decoder = new TextDecoder();
  const chunks: Chunk[] = [];
  let text = "";

  const deadline = Promise.withResolvers<void>();
  const timer = setTimeout(deadline.resolve, timeout);
  const pumping = (async () => {
    for (;;) {
      const { done: ended, value } = await reader.read();
      if (ended) return;
      const piece = decoder.decode(value);
      text += piece;
      chunks.push({ at: performance.now() - started, text: piece });
      if (done(text)) return;
    }
  })();

  await Promise.race([pumping, deadline.promise]);
  clearTimeout(timer);
  await reader.cancel().catch(() => {});
  return chunks;
}

describe("the live SSE stream", () => {
  test("a live event survives the wire, and the one after it too", async () => {
    const server = listen(
      new WirePublisher([
        { at: 0, event: event(1) },
        { at: 1200, event: event(2) },
      ]),
      25_000,
    );

    const chunks = await readSse(
      `http://127.0.0.1:${server.port}/events/stream`,
      (text) => text.includes("id: 2"),
      3000,
    );
    const text = chunks.map((chunk) => chunk.text).join("");

    expect(text).toContain(": connected");
    expect(text).toContain("id: 1\nevent: message.new\ndata: ");
    expect(text).toContain("id: 2\nevent: message.new\ndata: ");
    // The stream must still be open after the first event, which is the half
    // that used to fail: the connection was closed by the failure, not by the
    // client.
    expect(chunks.length).toBeGreaterThan(1);
  });

  test("the second chunk arrives after the first, not with it", async () => {
    const server = listen(
      new WirePublisher([
        { at: 0, event: event(1) },
        { at: 1200, event: event(2) },
      ]),
      25_000,
    );

    const chunks = await readSse(
      `http://127.0.0.1:${server.port}/events/stream`,
      (text) => text.includes("id: 2"),
      3000,
    );
    const first = chunks.find((chunk) => chunk.text.includes("id: 1"));
    const second = chunks.find((chunk) => chunk.text.includes("id: 2"));

    expect(first).toBeDefined();
    expect(second).toBeDefined();
    // A buffered stream would hand both over at once; the client's live view
    // depends on the gap being real.
    expect(second!.at - first!.at).toBeGreaterThan(1000);
  });

  test("an idle stream keeps itself alive without an event to carry", async () => {
    const server = listen(new WirePublisher([]), 100);

    const chunks = await readSse(
      `http://127.0.0.1:${server.port}/events/stream`,
      (text) => text.includes(": keep-alive"),
      1000,
    );
    const text = chunks.map((chunk) => chunk.text).join("");

    expect(text).toContain(": connected");
    expect(text).toContain(": keep-alive");
  });

  test("one unreadable frame does not end the stream", async () => {
    const broken = { ...event(1), createdAt: "not a date" } as unknown as Event;
    const server = listen(
      new WirePublisher([
        { at: 0, event: broken },
        { at: 100, event: event(2) },
      ]),
      25_000,
    );

    const chunks = await readSse(
      `http://127.0.0.1:${server.port}/events/stream`,
      (text) => text.includes("id: 2"),
      1000,
    );
    const text = chunks.map((chunk) => chunk.text).join("");

    expect(text).not.toContain("id: 1");
    expect(text).toContain("id: 2");
  });
});

describe("the live WebSocket stream", () => {
  test("the heartbeat starts before the stream does, and the live event still arrives", async () => {
    // The stream opens only after 250 ms, so a ping that arrives first can only
    // come from a timer installed before the subscription resolved.
    const slow = new WirePublisher([{ at: 250, event: event(7) }]);
    const server = listen(slow, 100);

    const frames: Array<{ at: number; frame: string }> = [];
    const started = performance.now();
    const socket = new WebSocket(`ws://127.0.0.1:${server.port}/events/socket`);

    const settled = Promise.withResolvers<void>();
    socket.onmessage = (message) => {
      frames.push({ at: performance.now() - started, frame: String(message.data) });
      const seen = frames.map((entry) => entry.frame);
      if (seen.some((frame) => frame.includes('"id":7'))) settled.resolve();
    };
    socket.onerror = () => settled.resolve();

    const timer = setTimeout(settled.resolve, 2000);
    await settled.promise;
    clearTimeout(timer);
    socket.close();

    const ping = frames.find((entry) => entry.frame.includes('"type":"ping"'));
    const live = frames.find((entry) => entry.frame.includes('"id":7'));

    expect(ping).toBeDefined();
    expect(live).toBeDefined();
    expect(ping!.at).toBeLessThan(live!.at);
  });
});
