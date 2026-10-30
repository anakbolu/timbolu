/**
 * The `Bun.serve` options the API listens with, in one place so the test that
 * performs a real handshake drives the same options production uses.
 */

import { type BunWebSocketData, websocket } from "hono/bun";

interface ServeOptionsInput {
  hostname: string;
  port: number;
  fetch: (request: Request, server: Bun.Server<BunWebSocketData>) => Response | Promise<Response>;
}

/**
 * `websocket` is the reason this is a function rather than an object literal.
 * `upgradeWebSocket` from `hono/bun` hands the connection to `server.upgrade()`,
 * and Bun refuses that call unless a handler was passed to `Bun.serve`:
 * `TypeError [ERR_INVALID_ARG_TYPE]: To enable websocket support, set the
 * "websocket" object in Bun.serve({})`. Nothing in the route's own types says
 * so — it compiles, and only a real upgrade fails, which is how
 * `/api/events/socket` answered 500 in production.
 *
 * `idleTimeout: 0` rides along because the SSE stream is a long-lived response
 * and Bun's default timeout would cut it; a test that boots these options is
 * booting the stream's options too.
 */
export function serveOptions(opts: ServeOptionsInput) {
  return { ...opts, idleTimeout: 0, websocket } satisfies Bun.Serve.Options<BunWebSocketData>;
}