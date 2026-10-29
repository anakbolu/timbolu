"use client";

/**
 * The realtime client: one connection per tab, WebSocket first with a
 * Server-Sent Events fallback, and a resume that fills whatever it missed.
 *
 * Two details make the feed whole rather than merely live:
 * - every frame carries the event id, so a reconnect asks for everything after
 *   the last id it saw instead of hoping nothing happened, and that id is kept
 *   in sessionStorage so a reload resumes where the last one stopped instead of
 *   replaying the workspace's history over the screen;
 * - the connection opens first and the backlog follows it — the server sends
 *   everything after `last_event_id` on the connection itself — so "live" is
 *   reached as soon as the transport proves itself and a slow HTTP replay can
 *   no longer delay it.
 */

import { getAccessToken, listEvents, type ActivityEvent } from "./api";

/**
 * The event names this client acts on are listed here rather than imported from
 * `packages/contracts/events`: the image is built with `apps/frontend` as its
 * build context, so nothing outside this directory exists inside it and a
 * cross-package import fails the build (#328). `make contracts-check` still
 * rejects a name the docs do not have, so the copy cannot drift silently.
 */
export const CHAT_EVENT_TYPES: readonly string[] = [
  "message.new",
  "message.updated",
  "router.decision",
  // A Bolu says what it is doing through its own state, which is the only
  // status the stream carries while an answer is still being prepared.
  "agent.state",
];

/**
 * Every event type the app acts on, named once.
 *
 * The SSE fallback subscribes to each of them by name: the server names its
 * frames, and EventSource fires `onmessage` only for unnamed ones, so a named
 * frame with no listener of its own is a frame nobody sees.
 */
export const REALTIME_EVENT_TYPES: readonly string[] = [
  ...CHAT_EVENT_TYPES,
  "draft.created",
  "draft.decided",
  "task.started",
  "task.finished",
  "task.waiting_approval",
  "task.running",
  "routine.run",
];

/** Where the last delivered event id per workspace is kept. */
const LAST_EVENT_ID_PREFIX = "bolu.last_event_id.";

/** The key that forces the SSE fallback, for testing it against a real backend. */
const TRANSPORT_KEY = "bolu.realtime";

/**
 * readLastEventId is the resume point a page load starts from.
 *
 * sessionStorage rather than localStorage: the cursor is only meaningful while
 * this tab's view of the workspace is being rebuilt, and a second tab must be
 * free to resume from where it left off rather than from this one's.
 */
export function readLastEventId(workspaceId: string): number {
  if (typeof window === "undefined") {
    return 0;
  }

  try {
    const raw = window.sessionStorage.getItem(LAST_EVENT_ID_PREFIX + workspaceId);
    const value = raw === null ? Number.NaN : Number(raw);
    return Number.isFinite(value) && value > 0 ? value : 0;
  } catch {
    // Storage can be denied outright (private mode, blocked cookies). A missing
    // cursor costs a backlog replay, which is what 0 asks for anyway.
    return 0;
  }
}

function writeLastEventId(workspaceId: string, id: number) {
  try {
    window.sessionStorage.setItem(LAST_EVENT_ID_PREFIX + workspaceId, String(id));
  } catch {
    // As above: the cursor is an optimisation, not the feed.
  }
}

/**
 * parseEvent reads one streamed frame.
 *
 * The frame comes from the network, so it is validated rather than asserted: a
 * malformed one is dropped instead of reaching a renderer as a half-built
 * object. Only the fields the client acts on are checked.
 *
 * `fallbackId` is the frame header's id, used when the body carries none: a
 * named SSE frame puts its id on the `id:` line rather than in the JSON.
 */
function parseEvent(raw: unknown, fallbackId?: number): ActivityEvent | null {
  if (typeof raw !== "string") {
    return null;
  }

  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch {
    return null;
  }

  if (typeof value !== "object" || value === null) {
    return null;
  }

  const candidate = value as Record<string, unknown>;
  const id = typeof candidate.id === "number" ? candidate.id : fallbackId;
  if (typeof id !== "number" || !Number.isFinite(id) || typeof candidate.type !== "string") {
    return null;
  }

  return { ...candidate, id } as ActivityEvent;
}

export type RealtimeStatus = "connecting" | "live" | "reconnecting" | "slow" | "closed";

export type RealtimeHandlers = {
  onEvent: (event: ActivityEvent) => void;
  onStatus?: (status: RealtimeStatus) => void;
};

export type RealtimeConnection = {
  close: () => void;
  /** lastEventId is what a reconnect resumes from. */
  lastEventId: () => number;
  status: () => RealtimeStatus;
};

const RECONNECT_DELAYS_MS = [500, 1000, 2000, 4000, 8000];
const REPLAY_LIMIT = 200;
/** How many backlog pages a single reconnect reads before giving up on the past. */
const REPLAY_MAX_PAGES = 8;

/**
 * How long an open connection must survive before it counts as healthy.
 *
 * `onopen` says the route answered, not that the stream stays up: production
 * answered 200 and closed 32 ms later, and resetting the retry budget on that
 * open is what kept the client reconnecting forever.
 */
const HEALTHY_AFTER_MS = 5000;

/** Consecutive failures before the status stops promising a fast reconnect. */
const FAILURES_BEFORE_SLOW = 3;

/** The first wait between retries once the client is on the slow path. */
const SLOW_RETRY_MS = 30_000;

/** Extra wait per slow retry, so many tabs do not return to a sick server together. */
const SLOW_RETRY_JITTER_MS = 15_000;

/** How long a replay that moved the cursor not at all is skipped before retrying. */
const REPLAY_STALL_MS = 60_000;

/**
 * How many socket handshakes may fail before the client stops asking for one.
 *
 * `new WebSocket` succeeds even when the handshake never will, so a proxy that
 * does not upgrade the route used to retry the socket forever and never reach
 * SSE. Two failures tell a flaky network apart from a route that is not there.
 */
const SOCKET_FAILURES_BEFORE_SSE = 2;

/**
 * How long a handshake may stay unopened before it counts as a failure.
 *
 * A browser can keep a CONNECTING socket quiet long past the point where the
 * user needs an answer, and waiting on an event that never comes is what left
 * production on "reconnecting" indefinitely.
 */
const SOCKET_OPEN_TIMEOUT_MS = 4000;

function apiBase(): string {
  return process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:8080";
}

/**
 * sseForced takes the socket out of the picture, so the fallback can be
 * exercised against a real backend: `localStorage['bolu.realtime'] = 'sse'`.
 */
function sseForced(): boolean {
  try {
    return window.localStorage.getItem(TRANSPORT_KEY) === "sse";
  } catch {
    return false;
  }
}

/**
 * connect opens the stream for one workspace.
 *
 * The transport opens first and the backlog follows it: the server sends
 * everything after `last_event_id` on the connection, so waiting on an HTTP
 * replay before opening would only delay "live". The replay is kept for a
 * reconnect — in the background, from the last delivered id — and `deliver`
 * drops every id it has already seen, so the two sources cannot double up.
 */
export function connect(
  workspaceId: string,
  handlers: RealtimeHandlers,
  startAfterId = 0,
): RealtimeConnection {
  let lastId = startAfterId;
  let status: RealtimeStatus = "connecting";
  let socket: WebSocket | null = null;
  let source: EventSource | null = null;
  // window.setTimeout returns a number in the browser, so the handle needs no
  // utility type and clears unconditionally.
  let timer: number | undefined;
  let handshake: number | undefined;
  let attempts = 0;
  // Consecutive failures, counted until a stream proves itself healthy. Unlike
  // `attempts` it survives the switch from socket to SSE, because the status
  // describes the feed rather than one transport.
  let failures = 0;
  // The health clock of the transport currently open, if it has not proven
  // itself yet.
  let healthy: number | undefined;
  // The cursor a replay stalled at and when it did, so the same question is not
  // asked again over a backlog that did not move.
  let replayStalledFrom = 0;
  let replayStalledAt = 0;
  // Consecutive failed socket handshakes. A socket that proves itself resets
  // it, so a working transport is never abandoned over an old failure.
  let socketFailures = 0;
  // Once the socket has been given up on, the rest of the session stays on SSE:
  // retrying a route the proxy does not upgrade only costs the user a stream.
  let preferSSE = false;
  let stopped = false;

  function setStatus(next: RealtimeStatus) {
    status = next;
    handlers.onStatus?.(next);
  }

  /**
   * proven marks a stream that earned a reset: it survived HEALTHY_AFTER_MS or
   * delivered a frame. Only then is the status allowed to say "live", because a
   * connection that opens and dies a frame later is a failure, not a stream.
   */
  function proven() {
    clearTimeout(healthy);
    healthy = undefined;
    failures = 0;
    attempts = 0;
    socketFailures = 0;
    setStatus("live");
  }

  /** opened starts the health clock for a transport that just came up. */
  function opened() {
    clearTimeout(healthy);
    healthy = window.setTimeout(proven, HEALTHY_AFTER_MS);
  }

  /** lost stops the health clock; a close before it fires is a failure. */
  function lost() {
    clearTimeout(healthy);
    healthy = undefined;
  }

  function deliver(event: ActivityEvent) {
    if (event.id <= lastId) {
      return;
    }
    lastId = event.id;
    // The cursor is written where it is advanced, so a reload resumes from the
    // last event this tab actually saw.
    writeLastEventId(workspaceId, event.id);
    handlers.onEvent(event);
  }

  /**
   * replay fills a gap over HTTP, after a reconnect and in the background: the
   * connection is already open and carries the server's own backlog, so this is
   * the belt to that suspenders. It starts from the last delivered id — never
   * from 0 — and a page the connection has already overtaken is dropped by
   * `deliver` rather than applied out of order.
   */
  async function replay() {
    // A backlog that moved nothing will not move when asked again: production
    // asked for the same `after_id` six times in a row. Skip the next replay
    // until the cursor has advanced or long enough has passed to expect more.
    if (
      replayStalledAt !== 0 &&
      lastId === replayStalledFrom &&
      Date.now() - replayStalledAt < REPLAY_STALL_MS
    ) {
      return;
    }

    const from = lastId;
    for (let page = 0; page < REPLAY_MAX_PAGES; page++) {
      if (stopped) {
        break;
      }
      const result = await listEvents(workspaceId, lastId, REPLAY_LIMIT);
      if (!result.ok || result.data.length === 0) {
        break;
      }
      for (const event of result.data) {
        deliver(event);
      }
      if (result.data.length < REPLAY_LIMIT) {
        break;
      }
    }
    if (lastId === from) {
      replayStalledFrom = from;
      replayStalledAt = Date.now();
    } else {
      replayStalledAt = 0;
    }
  }

  function scheduleReconnect() {
    if (stopped) {
      return;
    }
    failures++;
    if (failures >= FAILURES_BEFORE_SLOW) {
      // Past a few failures a fast retry promises a stream the client cannot
      // deliver: say the updates are late and back off instead of spinning.
      setStatus("slow");
      timer = window.setTimeout(
        () => start(true),
        SLOW_RETRY_MS + Math.random() * SLOW_RETRY_JITTER_MS,
      );
      return;
    }
    const delay = RECONNECT_DELAYS_MS[Math.min(attempts, RECONNECT_DELAYS_MS.length - 1)];
    attempts++;
    setStatus("reconnecting");
    timer = window.setTimeout(() => start(true), delay);
  }

  /**
   * socketFailed records a handshake that never opened and decides whether to
   * keep asking for one. On the switch the attempt budget is reset: the socket's
   * backoff says nothing about SSE, and inheriting it would make the first SSE
   * attempt wait eight seconds after a socket that never even connected.
   */
  function socketFailed() {
    socketFailures++;
    if (socketFailures >= SOCKET_FAILURES_BEFORE_SSE) {
      preferSSE = true;
      attempts = 0;
    }
    scheduleReconnect();
  }

  function openSocket(): boolean {
    const token = getAccessToken();
    if (!token || typeof WebSocket === "undefined") {
      return false;
    }

    const url = new URL(`${apiBase()}/api/events/socket`, apiBase() || window.location.origin);
    url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
    url.searchParams.set("workspace_id", workspaceId);
    url.searchParams.set("token", token);
    url.searchParams.set("last_event_id", String(lastId));

    const candidate = new WebSocket(url.toString());
    socket = candidate;

    // Constructing a socket proves nothing; only `onopen` does. A handshake the
    // browser never completes is what a route the proxy does not upgrade looks
    // like, so the timeout spends the failure budget: drop the socket first so
    // its late close cannot schedule a second attempt, then count it.
    handshake = window.setTimeout(() => {
      socketFailures = SOCKET_FAILURES_BEFORE_SSE;
      socket = null;
      candidate.close();
      socketFailed();
    }, SOCKET_OPEN_TIMEOUT_MS);

    candidate.onopen = () => {
      if (socket !== candidate) {
        return;
      }
      clearTimeout(handshake);
      handshake = undefined;
      opened();
    };
    candidate.onmessage = (message) => {
      const event = parseEvent(message.data);
      if (event) {
        if (healthy !== undefined) {
          proven();
        }
        deliver(event);
      }
    };
    candidate.onerror = () => candidate.close();
    candidate.onclose = () => {
      // A late close from a socket this client already replaced must not
      // schedule a second reconnect on top of the live transport.
      if (socket !== candidate) {
        return;
      }
      clearTimeout(handshake);
      handshake = undefined;
      lost();
      socket = null;
      socketFailed();
    };

    return true;
  }

  function openSSE() {
    const token = getAccessToken();
    if (!token || typeof EventSource === "undefined") {
      scheduleReconnect();
      return;
    }

    // EventSource cannot set a header either, so the token travels in the query
    // like it does for the socket, and the workspace the same way.
    const url = new URL(`${apiBase()}/api/events/stream`, apiBase() || window.location.origin);
    url.searchParams.set("workspace_id", workspaceId);
    url.searchParams.set("token", token);
    url.searchParams.set("last_event_id", String(lastId));

    const candidate = new EventSource(url.toString());
    source = candidate;

    candidate.onopen = () => {
      if (source !== candidate) {
        return;
      }
      opened();
    };

    // The frames are named, and `onmessage` only fires for unnamed ones, so
    // every type the app acts on needs a listener of its own. The frame header
    // carries the id, which is the fallback when the body has none.
    const handle = (raw: Event) => {
      const message = raw as MessageEvent<string>;
      const header = message.lastEventId ? Number(message.lastEventId) : undefined;
      const event = parseEvent(message.data, header);
      if (event) {
        if (healthy !== undefined) {
          proven();
        }
        deliver(event);
      }
    };
    for (const type of REALTIME_EVENT_TYPES) {
      candidate.addEventListener(type, handle);
    }
    candidate.onmessage = handle;

    candidate.onerror = () => {
      // EventSource reconnects by itself, but without the query string it would
      // lose the resume point, so the client owns the reconnect.
      candidate.close();
      if (source !== candidate) {
        return;
      }
      lost();
      source = null;
      scheduleReconnect();
    };
  }

  function start(reconnected: boolean) {
    if (stopped) {
      return;
    }
    // A slow retry keeps the slow label while it runs: flicking back to
    // "reconnecting" would promise a fast stream this attempt cannot prove.
    if (status !== "slow") {
      setStatus(reconnected ? "reconnecting" : "connecting");
    }

    if (reconnected) {
      void replay().catch(() => {
        // A failed replay is not fatal: the stream carries its own backlog once
        // it comes up, and the next reconnect tries the replay again.
      });
    }

    if (stopped) {
      return;
    }
    if (sseForced() || preferSSE) {
      openSSE();
      return;
    }
    if (!openSocket()) {
      openSSE();
    }
  }

  start(false);

  return {
    close() {
      stopped = true;
      clearTimeout(timer);
      clearTimeout(handshake);
      lost();
      socket?.close();
      source?.close();
      socket = null;
      source = null;
      setStatus("closed");
    },
    lastEventId: () => lastId,
    status: () => status,
  };
}
