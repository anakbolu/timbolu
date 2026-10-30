# Realtime: two transports, and what the server must get right

The workspace event feed is carried by two endpoints, and the order between them is a client
decision while the handshake itself is a server one. This document covers the transport layer:
which endpoint opens first, when the client gives up on it, what `apps/backend-js` has to configure
for the upgrade to happen at all, and how to prove from outside that it does. The event types, the
payloads, and the meaning of each client status are in [api.md](./api.md#the-stream) and
[api.md](./api.md#status-realtime-di-klien); this file links there instead of restating them.

## The two transports and the order between them

`GET /api/events/socket` upgrades to a WebSocket and is the primary transport. `GET
/api/events/stream` answers with Server-Sent Events and is the fallback for a client that cannot
hold a socket open. Both read the same cursor and carry the same event payloads in the same order,
so a tab that changes transport does not lose its place in the feed.

`connect()` in [`apps/frontend/src/lib/realtime.ts`](../apps/frontend/src/lib/realtime.ts) opens
exactly one connection per tab and prefers the socket: it asks for a WebSocket whenever an access
token is available and the runtime has one, and it reaches for `EventSource` only when either is
missing or when the socket has already been given up on — or when the `bolu.realtime` key forces it,
described below.

**When the client stops asking for a socket.** Constructing a `WebSocket` proves nothing — the
object exists whether or not the route answers `101` — so the client measures the handshake instead.
A handshake that stays unopened for `SOCKET_OPEN_TIMEOUT_MS = 4000` counts as one failure, and after
`SOCKET_FAILURES_BEFORE_SSE = 2` consecutive failed handshakes the client sets its `preferSSE` latch
for the rest of the session. The latch is what stops a route the proxy does not upgrade from costing
a fresh pair of handshakes on every retry; the attempt budget is reset when it flips, so the first SSE
attempt does not inherit the socket's backoff.

**When a stream counts as healthy.** `onopen` is not one answer for both transports. A WebSocket
handshake that completes is a `101` upgrade, so the transport is alive and the status turns `live` at
that instant without waiting for the clock (#475) — but it does not reset the retry budgets. An
`EventSource` `onopen` proves only a `200`: production answered that and closed 32 ms later (#314), so
SSE earns `live` only after surviving `HEALTHY_AFTER_MS = 5000`, or earlier if it delivers a frame
first. Either way a transport that dies before the clock is a failure, and the retry budget is not
reset. The five `RealtimeStatus` values and the labels the chat screen renders are documented in
[api.md](./api.md#status-realtime-di-klien).

**Why a reconnect is cheap.** Both URLs carry `last_event_id`, taken from `readLastEventId()` in the
same client file. The cursor lives in `sessionStorage` under `LAST_EVENT_ID_PREFIX` plus the workspace
id — `bolu.last_event_id.<workspaceId>` — and is written by `deliver()` at the moment an id advances
it, so a reload resumes from the last event that tab actually saw instead of replaying the
workspace's history over the screen. `sessionStorage` rather than `localStorage` is deliberate: a
second tab keeps its own resume point. A cursor of `0` — no stored value, storage denied, or a fresh
workspace — asks for the backlog from the beginning.

The server reads the cursor in two places, header first: the `Last-Event-ID` header, then the
`last_event_id` query parameter, ignoring an empty or non-positive value. SSE frames are named
(`id:`, `event:`, `data:`) because the client listens per event type — `EventSource.onmessage` fires
only for unnamed frames. The socket sends the same payload as a JSON message per event, plus a
`{"type":"ping"}` heartbeat frame. The SSE response opens with a `: connected` comment line so the
head reaches the client before there is anything to replay.

To exercise the fallback against a real backend, set `localStorage['bolu.realtime'] = 'sse'` before
the page loads: `sseForced()` reads that key and skips the socket entirely.

## What the server must get right

Production serves `timbolu.com/api/**` from the Bun tree under `apps/backend-js` (see
[deployment.md](./deployment.md#what-production-actually-runs)), and the upgrade depends on one option
in that process.

**`Bun.serve` must be given the `websocket` handler from `hono/bun`.** `apps/backend-js/src/cmd/api.ts`
builds the server, and the `hono/bun` `upgradeWebSocket` that `handler.ts` returns does not implement
a handshake itself: it calls `server.upgrade(request, {data: …})` and leaves the frames to the
`websocket` object. Bun only accepts the upgrade when the server was started with that object — its
own error is `To enable websocket support, set the "websocket" object in Bun.serve({})` — so
`api.ts` has to import the `websocket` export from `hono/bun` alongside the handler and pass it in
the options.

**`idleTimeout: 0` stays.** The SSE route is a response that lives as long as the tab does, and Bun's
default idle timeout would cut it. Zero disables the timeout, which is what the stream needs.

**The consequence of getting the wiring wrong is a `500`, not a quiet fallback.** `server.upgrade()`
throws when `websocket` is missing, the throw happens inside the route before any `Response` exists,
and the app-wide handler in `apps/backend-js/src/container/http.ts` turns an uncaught error into the
standard envelope: `500` with `{"error":{"code":"internal_error"}}`. The browser then sees a failed
handshake rather than a `101`, spends `SOCKET_FAILURES_BEFORE_SSE` attempts on it, and finishes the
page load on SSE — which works, and is exactly why the defect is easy to miss. It is paid once per
page load, because `preferSSE` lives in memory and does not survive a reload, while the cursor does.
Production was in exactly this state (#471): the handshake answered `500` and the feed stayed
alive over SSE, so nothing on the screen said the primary transport was gone.

The same requirement covers the WhatsApp QR stream: `apps/backend-js/src/modules/whatsapp/handler/stream.ts`
also returns `upgradeWebSocket`, so a server without the `websocket` object breaks that socket too.

**Both stream paths authenticate from the query string.** `requireStreamAuth` in
`apps/backend-js/src/container/middleware-auth.ts` is registered on the two chat stream paths and on
the WhatsApp link socket, and on no other route. It accepts a bearer header or `?token=`, and reads
the workspace from the `X-Workspace-Id` header or `?workspace_id=`. The query form is not a
convenience for scripts: a browser cannot set a header on a `WebSocket` or an `EventSource` request,
so the token travels in the URL for both transports. A missing token answers `401 unauthenticated`, a
malformed workspace `400 invalid_workspace`, and a non-member `403 not_a_member` — before the upgrade
is attempted.

## Verifying the handshake from outside

These commands need only `curl`; they show what the deployment answers, not what the code intends.
Set the three variables first (the token is an access token from the web app's own session, and the
workspace id is a UUID):

```bash
BASE=${BASE:-https://timbolu.com/api}
TOKEN=…
WORKSPACE=…
```

**The upgrade itself.** Send the four headers a browser's handshake carries, over HTTP/1.1, with the
fixed key from RFC 6455 so the accept value is predictable:

```bash
curl --http1.1 -i --max-time 5 \
  -H 'Connection: Upgrade' \
  -H 'Upgrade: websocket' \
  -H 'Sec-WebSocket-Version: 13' \
  -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
  "$BASE/events/socket?workspace_id=$WORKSPACE&token=$TOKEN"
```

The first line must be `HTTP/1.1 101 Switching Protocols`, followed by `Upgrade: websocket`,
`Connection: Upgrade`, and `Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=`. `curl` waits on the
open connection until `--max-time` expires and exits with status 28; that is expected, and the
headers already printed are the evidence.

| Answer | Means |
| --- | --- |
| `101 Switching Protocols` | The route is mounted and `Bun.serve` has the `websocket` handler |
| `500` with `internal_error` | The route is mounted but `server.upgrade()` threw — the `websocket` option is missing from the server (#471) |
| `404` with `not_found` | No route answered: the chat module is not mounted, or the path is wrong |
| `401` / `403` | Auth reached the route and refused it; fix the token or the workspace before reading the handshake |

**The same URL without an upgrade.** A plain GET is refused deliberately rather than upgraded, and it
must not be the `500` above:

```bash
curl -i "$BASE/events/socket?workspace_id=$WORKSPACE&token=$TOKEN"
```

Expect `426 Upgrade Required` with `{"success":false,"message":"this endpoint speaks WebSocket; use
/events/stream for SSE","error":{"code":"upgrade_required"}}`. A `426` here together with a `101`
above is a healthy deployment; a `500` on either command means the server wiring is wrong.

**The SSE route.** `-N` disables buffering so the head is read as it is flushed:

```bash
curl -N -i --max-time 5 "$BASE/events/stream?workspace_id=$WORKSPACE&token=$TOKEN"
```

The response is `200` with `Content-Type: text/event-stream`, and the first frame is `: connected`.
Frames that follow are `: keep-alive` comments at the heartbeat interval, then named events as
`id:` / `event:` / `data:`. Add `-H 'Last-Event-ID: <id>'` to ask for the backlog after a specific
id and watch the replay arrive before the heartbeat. `curl` again exits 28 when `--max-time` expires.

**The fallback end to end.** With the socket blocked — a proxy that does not upgrade, or the
`bolu.realtime` key above — the page must still end up `live` over SSE: the status reaches it once
the transport survives `HEALTHY_AFTER_MS` or delivers one frame (there is no handshake to trust on
this path), and events keep arriving. If the page instead stays on `reconnecting` or goes `slow`
while `/events/stream` answers `: connected`, the problem is on the SSE side rather than the socket.

## Where the rest lives

- [api.md](./api.md#the-stream) — the event types, what a frame carries, and the `after_id` replay.
- [api.md](./api.md#status-realtime-di-klien) — the five client statuses and the retry cadences that
  produce them.
- [deployment.md](./deployment.md#what-production-actually-runs) — which process serves `timbolu.com/api`
  and how to tell it apart from the Go tree.
- [`apps/frontend/src/lib/realtime.ts`](../apps/frontend/src/lib/realtime.ts) — the client, which is
  the authority for every constant named here.