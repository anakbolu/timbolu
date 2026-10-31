/**
 * Throwaway repro for the dead live stream (#504), second attempt: drive the
 * *real* `Handler.sse` through Hono and `Bun.serve` on an ephemeral port, with
 * a fake service whose iterable yields one event now and one after 1200 ms.
 *
 * The first version rebuilt the body inline and delivered both chunks, so the
 * hypothesis "an async start() that never resolves stops the pump" is not the
 * whole story. This one exercises the handler's own code path.
 *
 * Run: bun run scripts/repro-sse.ts
 */

import { Hono } from "hono";
import { Handler } from "../src/modules/chat/handler/handler.ts";
import type { AppEnv } from "../src/platform/context.ts";
import type { Logger } from "../src/platform/logger.ts";
import type { Service } from "../src/modules/chat/domain/service.ts";

const HEARTBEAT = 300;

function liveEvent(id: number): never {
  return {
    id,
    workspaceId: "w",
    type: "message.new",
    actorAgentId: "",
    actorUserId: "",
    conversationId: "",
    taskId: "",
    draftId: "",
    payload: {},
    createdAt: new Date(),
  } as never;
}

/** What a subscriber actually receives: the publisher's JSON round trip. */
function wire(event: never): never {
  return JSON.parse(JSON.stringify(event)) as never;
}

function fakeEvents(): AsyncIterable<never> {
  return {
    async *[Symbol.asyncIterator]() {
      yield wire(liveEvent(1));
      const later = Promise.withResolvers<void>();
      setTimeout(later.resolve, 1200);
      await later.promise;
      yield wire(liveEvent(2));
      // Keep the generator alive like the live subscription would.
      await Promise.withResolvers<void>().promise;
    },
  };
}

const service = {
  stream: () => Promise.resolve(fakeEvents()),
} as unknown as Service;

const log = {
  info: () => {},
  warn: () => {},
  error: (fields: unknown, msg?: string) => console.log("log.error", fields, msg),
  debug: () => {},
  child: () => log,
} as unknown as Logger;

const handler = new Handler(service, log).withHeartbeat(HEARTBEAT);

const app = new Hono<AppEnv>();
app.use("*", async (c, next) => {
  c.set("scope", { userId: "u", workspaceId: "w" });
  await next();
});
app.get("/stream", (c) => handler.sse(c));

const server = Bun.serve({ port: 0, idleTimeout: 0, fetch: app.fetch });

const started = performance.now();
const response = await fetch(`http://127.0.0.1:${server.port}/stream`);
const reader = response.body!.getReader();
const decoder = new TextDecoder();
const chunks: Array<{ at: number; text: string }> = [];

const deadline = setTimeout(() => void reader.cancel(), 4000);
try {
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    chunks.push({ at: Math.round(performance.now() - started), text: decoder.decode(value) });
  }
} catch (err) {
  console.log("read failed:", err);
}
clearTimeout(deadline);
server.stop(true);

for (const chunk of chunks) {
  console.log(`${String(chunk.at).padStart(5)}ms  ${JSON.stringify(chunk.text)}`);
}
const sawSecondEvent = chunks.some((chunk) => chunk.text.includes("id: 2"));
console.log(`\nchunks=${chunks.length} second-event=${sawSecondEvent ? "DELIVERED" : "MISSING"}`);
process.exit(sawSecondEvent ? 0 : 1);
