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
 *   reached as soon as the transport is up and a slow HTTP replay can no longer
 *   delay it.
 */

import {
  AGENT_STATES as CONTRACT_AGENT_STATES,
  CHAT_EVENT_TYPES as CONTRACT_CHAT_EVENT_TYPES,
  REALTIME_EVENT_TYPES as CONTRACT_REALTIME_EVENT_TYPES,
} from "@timbolu/contracts/events";

import { getAccessToken, listEvents, type ActivityEvent } from "./api";

/**
 * The event types the chat thread renders while a reply is still streaming.
 *
 * The names come from the package rather than from a list here: `agent.state` is
 * in it because a Bolu says what it is doing through its own state, which is the
 * only status the stream carries while an answer is still being prepared.
 */
export const CHAT_EVENT_TYPES: readonly string[] = CONTRACT_CHAT_EVENT_TYPES;

/**
 * The two stream events no web screen renders: `task.step` is a task's internal
 * progress, and `notification.pending` is the notification worker's hook. The
 * package names what the stream carries; which of those this client subscribes
 * to is this client's decision, so the difference is kept here — and
 * `make contracts-check` fails if this set and the script's disagree.
 */
const WEB_INTERNAL_EVENT_TYPES: readonly string[] = [
  "task.step",
  "notification.pending",
];

/**
 * Every event type the app acts on: the package's list minus the internal ones.
 *
 * The SSE fallback subscribes to each of them by name: the server names its
 * frames, and EventSource fires `onmessage` only for unnamed ones, so a named
 * frame with no listener of its own is a frame nobody sees.
 */
export const REALTIME_EVENT_TYPES: readonly string[] = CONTRACT_REALTIME_EVENT_TYPES.filter(
  (type) => !WEB_INTERNAL_EVENT_TYPES.includes(type),
);

/**
 * The states a Bolu reports. Re-exported with the event names so a screen takes
 * the stream's vocabulary from this module instead of reaching into the package
 * for one constant and keeping a copy of the values.
 */
export const AGENT_STATES: readonly string[] = CONTRACT_AGENT_STATES;

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

export type RealtimeStatus = "connecting" | "live" | "reconnecting" | "closed";

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
  let attempts = 0;
  let stopped = false;

  function setStatus(next: RealtimeStatus) {
    status = next;
    handlers.onStatus?.(next);
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
    for (let page = 0; page < REPLAY_MAX_PAGES; page++) {
      if (stopped) {
        return;
      }
      const result = await listEvents(workspaceId, lastId, REPLAY_LIMIT);
      if (!result.ok || result.data.length === 0) {
        return;
      }
      for (const event of result.data) {
        deliver(event);
      }
      if (result.data.length < REPLAY_LIMIT) {
        return;
      }
    }
  }

  function scheduleReconnect() {
    if (stopped) {
      return;
    }
    const delay = RECONNECT_DELAYS_MS[Math.min(attempts, RECONNECT_DELAYS_MS.length - 1)];
    attempts++;
    setStatus("reconnecting");
    timer = window.setTimeout(() => start(true), delay);
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

    candidate.onopen = () => {
      attempts = 0;
      setStatus("live");
    };
    candidate.onmessage = (message) => {
      const event = parseEvent(message.data);
      if (event) {
        deliver(event);
      }
    };
    candidate.onerror = () => candidate.close();
    candidate.onclose = () => {
      socket = null;
      scheduleReconnect();
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
      attempts = 0;
      setStatus("live");
    };

    // The frames are named, and `onmessage` only fires for unnamed ones, so
    // every type the app acts on needs a listener of its own. The frame header
    // carries the id, which is the fallback when the body has none.
    const handle = (raw: Event) => {
      const message = raw as MessageEvent<string>;
      const header = message.lastEventId ? Number(message.lastEventId) : undefined;
      const event = parseEvent(message.data, header);
      if (event) {
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
      source = null;
      scheduleReconnect();
    };
  }

  function start(reconnected: boolean) {
    if (stopped) {
      return;
    }
    setStatus(reconnected ? "reconnecting" : "connecting");

    if (reconnected) {
      void replay().catch(() => {
        // A failed replay is not fatal: the connection is live, and the next
        // reconnect tries the backlog again.
      });
    }

    if (stopped) {
      return;
    }
    if (sseForced() || !openSocket()) {
      openSSE();
    }
  }

  start(false);

  return {
    close() {
      stopped = true;
      clearTimeout(timer);
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
