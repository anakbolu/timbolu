/**
 * The API's serve options are proven by a real handshake, because that is the
 * only check that sees a missing `websocket` handler: the upgrade route
 * compiles either way, and without the handler Bun throws
 * `ERR_INVALID_ARG_TYPE`, which the error handler turns into a 500.
 */

import { afterEach, describe, expect, test } from "bun:test";
import { Hono } from "hono";
import { upgradeWebSocket } from "hono/bun";
import { serveOptions } from "./serve-options.ts";

const servers: Bun.Server<unknown>[] = [];

afterEach(async () => {
  await Promise.all(servers.splice(0).map((server) => server.stop(true)));
});

/** Boots the options the API uses, on a port the kernel picks. */
function listen(app: Hono): Bun.Server<unknown> {
  const server = Bun.serve(serveOptions({ hostname: "127.0.0.1", port: 0, fetch: app.fetch }));
  servers.push(server);
  return server;
}

/** Opens one socket and resolves with the first frame the server sends. */
function firstFrame(url: string): Promise<string> {
  const socket = new WebSocket(url);
  return new Promise<string>((resolve, reject) => {
    socket.onmessage = (event) => {
      socket.close();
      resolve(String(event.data));
    };
    // A refused upgrade leaves the promise pending: the test's own timeout is
    // the failure, and it names this file rather than a guessed duration.
    socket.onerror = () => reject(new Error("the handshake failed"));
  });
}

describe("api serve options", () => {
  test("an upgrade route opens a socket", async () => {
    const app = new Hono();
    app.get("/socket", (c) =>
      upgradeWebSocket(c, {
        onOpen: (_event, ws) => ws.send("ping"),
      }),
    );

    const server = listen(app);

    expect(await firstFrame(`ws://127.0.0.1:${server.port}/socket`)).toBe("ping");
  });

  test("the event stream keeps Bun's idle timeout disabled", () => {
    const options = serveOptions({ hostname: "127.0.0.1", port: 0, fetch: () => new Response(null) });

    expect(options.idleTimeout).toBe(0);
  });
});