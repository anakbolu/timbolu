/**
 * The chat screen's live feed: one SSE connection to `/api/events/stream`.
 *
 * The web client (`apps/frontend/src/lib/realtime.ts`) negotiates between a
 * WebSocket and SSE and polls the backlog while the stream is slow. A phone
 * needs none of that: SSE is the documented fallback for a client that cannot
 * hold a socket, and `expo/fetch` streams a response body, so this client opens
 * the one transport it can keep. What does have to match the contract is the
 * cursor: every frame's `id:` advances the in-memory position, and a reconnect
 * sends it as `last_event_id` so the server replays exactly what was missed.
 *
 * The token and the workspace travel in the query string because the stream
 * route authenticates from there (`requireStreamAuth`), not from a header.
 *
 * Being on screen is part of the contract, so `AppState` is watched here rather
 * than in the screen: a socket does not survive the background, and reconnecting
 * there is battery and data spent on nobody. The cursor stays in memory across
 * the pause, so the connection opened on return asks the server to replay from
 * exactly where the phone left off.
 */

import { fetch as expoFetch } from "expo/fetch";
import { AppState } from "react-native";

import { API_URL, getActiveWorkspaceId, refreshSession } from "@/lib/api";
import type { ChatMessage, MessageBlock, MessageStatus } from "@/lib/api";
import { getAccessToken, tokenIsSpent } from "@/lib/tokens";

/**
 * The wait before each reconnect while the stream keeps failing.
 *
 * A short sequence first, because a dropped connection is usually a moment's
 * noise; then a long fixed wait, because a route that is genuinely down must not
 * turn this screen into a spinner that costs battery.
 */
const RECONNECT_DELAYS_MS = [500, 1_000, 2_000, 4_000, 8_000];

/** The wait once the fast budget is spent — a backoff, not a poll. */
const SLOW_RETRY_MS = 30_000;

/** Spread retries so many phones of a sick deployment do not return together. */
const SLOW_RETRY_JITTER_MS = 15_000;

/**
 * How long an open connection must survive before it counts as healthy.
 *
 * An open proves the route answered, not that it stays open: production once
 * answered `200` and closed 32 ms later. A connection that dies sooner than this
 * keeps the failure count moving, so a server that accepts and hangs up cannot
 * be retried every half second forever.
 */
const HEALTHY_AFTER_MS = 5_000;

/** One parsed SSE block: its header id, its name, and its joined data lines. */
type StreamFrame = {
  id?: number;
  event?: string;
  data: string;
};

/**
 * How one connection attempt ended.
 *
 * `interrupted` separates "we cut this short" — a pause, a resume, a close —
 * from "the route failed". Only the second belongs in the reconnect budget:
 * being asleep is not evidence that the server is sick.
 */
type OpenOutcome = { healthy: boolean; interrupted: boolean };

/** The event types that change a thread. Everything else only moves the cursor. */
const MESSAGE_EVENT_TYPES: Record<string, true> = { "message.new": true, "message.updated": true };

const MESSAGE_STATUSES: Record<string, true> = {
  complete: true,
  streaming: true,
  partial: true,
  failed: true,
};

function isMessageStatus(value: unknown): value is MessageStatus {
  return typeof value === "string" && MESSAGE_STATUSES[value] === true;
}

/** A message's instant, in milliseconds; unparsable or absent reads as 0. */
function messageTime(message: ChatMessage): number {
  const parsed = Date.parse(message.created_at);
  return Number.isNaN(parsed) ? 0 : parsed;
}

/**
 * Puts one streamed message into a newest-first thread.
 *
 * The list is rendered `inverted`, so index zero is the bottom and a message is
 * inserted by its own timestamp rather than assumed to be the newest: a
 * reconnect replays from the cursor and can carry something older than the page
 * already on screen. An update to a message that is already there — the
 * placeholder that becomes the answer — replaces it in place, which is what
 * keeps the bubble from flashing and the scroll from jumping.
 *
 * A message older than the loaded page is dropped: the page is the window this
 * screen opened with, and the workspace backlog reaches further back than it.
 */
export function insertMessage(current: ChatMessage[], incoming: ChatMessage): ChatMessage[] {
  const index = current.findIndex((message) => message.message_id === incoming.message_id);
  const oldest = current.length === 0 ? null : messageTime(current[current.length - 1]);
  if (index === -1 && oldest !== null && messageTime(incoming) < oldest) return current;

  const next = current.slice();
  if (index === -1) {
    next.push(incoming);
  } else {
    next[index] = incoming;
  }
  // `Array.prototype.sort` is stable, so two messages of the same instant keep
  // the order they arrived in.
  return next.sort((left, right) => messageTime(right) - messageTime(left));
}

/**
 * Replaces the thread with a freshly read page, keeping anything the stream
 * delivered while that read was in flight.
 *
 * The page is the server's truth and wins on every message it names; the only
 * case this adds is a frame that landed between the request and the answer,
 * which a plain assignment would drop and the thread would show as a message
 * that vanished until the next event.
 */
export function applyPage(current: ChatMessage[] | null, page: ChatMessage[]): ChatMessage[] {
  if (current === null) return page;
  // An empty page is a thread the server says has nothing; a message the stream
  // just delivered is not what an empty answer is about, so the screen keeps
  // what it has rather than blanking under a person mid-read.
  if (page.length === 0) return current;

  const known: Record<string, true> = {};
  for (const message of page) known[message.message_id] = true;

  const newest = messageTime(page[0]);
  let merged = page;
  for (const message of current) {
    if (known[message.message_id] === true) continue;
    if (messageTime(message) <= newest) continue;
    merged = insertMessage(merged, message);
  }
  return merged;
}

export type ChatStream = {
  /** Ends the stream and every timer it owns. Safe to call more than once. */
  close: () => void;
  /** The last event id delivered, for the next connection to resume from. */
  lastEventId: () => number;
  /**
   * Stops connecting until `resume()`, keeping the cursor.
   *
   * The stream watches `AppState` itself, so a screen does not have to call
   * this: it exists for a caller that already knows the screen is not visible
   * and would rather not wait for the platform's event.
   */
  pause: () => void;
  /**
   * Opens again at once, from `lastEventId()`, with the failure budget reset.
   *
   * A return from the background is not a retry: the route was never sick, so
   * the screen must not sit out a backoff that an outage reached before the
   * phone was put down.
   */
  resume: () => void;
};

/**
 * Opens the workspace stream and reports the messages of one conversation.
 *
 * `startAfterId` is the cursor the caller kept across a blur; within one
 * connection the cursor advances on every event, matching or not, because the
 * cursor is the workspace's position rather than this thread's. The same cursor
 * survives a background/foreground round trip inside this stream, so a screen
 * that keeps its focus never sees the connection gap.
 */
export function connectChatStream(
  conversationId: string,
  onMessage: (message: ChatMessage) => void,
  startAfterId = 0,
): ChatStream {
  let lastId = startAfterId;
  let failures = 0;
  let stopped = false;

  /**
   * Whether the app is on screen.
   *
   * The loop must not connect while it is not: the OS kills a background
   * socket within seconds, and every attempt after that is battery and data
   * spent on a screen nobody is looking at. `unknown` (iOS before it has decided)
   * counts as visible so the first frame is not delayed by a guess.
   */
  let paused = AppState.currentState === "background" || AppState.currentState === "inactive";

  /**
   * The attempt in flight, how to end its body read, and the generation that
   * says which attempt is still wanted.
   *
   * Pausing, resuming, or closing bumps the generation; an attempt that was cut
   * short compares against it and reports `interrupted` instead of a failure,
   * so a resume opens again at once rather than after a backoff.
   */
  let attempt: AbortController | null = null;
  let cancelBody: (() => void) | null = null;
  let generation = 0;

  /** Everything parked on a timer or on the pause; interrupting wakes them all. */
  const waiters = new Set<() => void>();

  function wakeWaiters(): void {
    const parked = [...waiters];
    waiters.clear();
    for (const wake of parked) wake();
  }

  /**
   * Ends the attempt in flight and anything waiting behind it.
   *
   * The body is cancelled as well as the fetch aborted: a platform that lets
   * `read()` sit on a socket the OS already killed is exactly what makes a
   * resumed screen wait for the network timeout instead of opening at once.
   */
  function interrupt(): void {
    generation += 1;
    const controller = attempt;
    attempt = null;
    // The body first: cancelling the reader is what ends a `read()` parked on a
    // socket the OS already killed, and a body that is not wired to the abort
    // signal would otherwise only notice at the network timeout.
    const cancel = cancelBody;
    cancelBody = null;
    cancel?.();
    controller?.abort();
    wakeWaiters();
  }

  function pause(): void {
    if (stopped || paused) return;
    paused = true;
    interrupt();
  }

  function resume(): void {
    if (stopped || !paused) return;
    paused = false;
    // Being asleep is not a sick route: the budget starts over so the attempt
    // after returning is immediate instead of the 30-second wait an outage
    // reached before the phone was put down.
    failures = 0;
    interrupt();
  }

  const appState = AppState.addEventListener("change", (state) => {
    if (state === "active") resume();
    else if (state === "background" || state === "inactive") pause();
  });

  /**
   * Reads one SSE block into its fields. Comments (`: connected`,
   * `: keep-alive`) carry no data and are dropped here.
   */
  function parseFrame(block: string): StreamFrame | null {
    let id: number | undefined;
    let event: string | undefined;
    const data: string[] = [];

    for (const line of block.split("\n")) {
      if (line === "" || line.startsWith(":")) continue;
      const colon = line.indexOf(":");
      const field = colon === -1 ? line : line.slice(0, colon);
      let value = colon === -1 ? "" : line.slice(colon + 1);
      if (value.startsWith(" ")) value = value.slice(1);

      if (field === "id") {
        const parsed = Number(value);
        if (value !== "" && Number.isFinite(parsed) && parsed > 0) id = parsed;
      } else if (field === "event") {
        event = value;
      } else if (field === "data") {
        data.push(value);
      }
    }

    if (data.length === 0) return null;
    return { id, event, data: data.join("\n") };
  }

  /** Reads the JSON body of a frame, keeping the header id as the fallback. */
  function parseEvent(frame: StreamFrame): { id?: number; type?: string; payload: unknown } | null {
    let value: unknown;
    try {
      value = JSON.parse(frame.data);
    } catch {
      return null;
    }
    if (typeof value !== "object" || value === null) return null;

    const raw = value as Record<string, unknown>;
    const id = typeof raw.id === "number" && Number.isFinite(raw.id) ? raw.id : frame.id;
    const type = typeof raw.type === "string" ? raw.type : frame.event;
    return { id, type, payload: raw.payload };
  }

  /**
   * Reads the message an event carries, or null when it is not one this screen
   * can draw. The payload comes from the network, so every field it needs is
   * checked rather than asserted.
   */
  function readMessage(value: unknown): ChatMessage | null {
    if (typeof value !== "object" || value === null) return null;
    const raw = value as Record<string, unknown>;

    if (typeof raw.message_id !== "string" || raw.message_id === "") return null;
    if (typeof raw.conversation_id !== "string") return null;
    if (!isMessageStatus(raw.status)) return null;
    if (!Array.isArray(raw.blocks)) return null;

    const blocks = raw.blocks.filter((block): block is MessageBlock => {
      return typeof block === "object" && block !== null && "type" in block && typeof block.type === "string";
    });

    return {
      message_id: raw.message_id,
      conversation_id: raw.conversation_id,
      agent_id: typeof raw.agent_id === "string" && raw.agent_id !== "" ? raw.agent_id : undefined,
      user_id: typeof raw.user_id === "string" && raw.user_id !== "" ? raw.user_id : undefined,
      blocks,
      task_id: typeof raw.task_id === "string" && raw.task_id !== "" ? raw.task_id : undefined,
      status: raw.status,
      finish_reason: typeof raw.finish_reason === "string" ? raw.finish_reason : undefined,
      // A message without a timestamp would sort unpredictably; stamping it once,
      // here, keeps the thread's comparator a pure function of the messages.
      created_at:
        typeof raw.created_at === "string" && raw.created_at !== ""
          ? raw.created_at
          : new Date().toISOString(),
    };
  }

  /** A token that will still be valid when the connection arrives. */
  async function accessToken(): Promise<string | null> {
    if (tokenIsSpent()) {
      // The stream carries the token in its URL, so it cannot lean on
      // `authorizedFetch`'s renewal: it renews before opening instead.
      await refreshSession();
    }
    return getAccessToken();
  }

  /**
   * Waits `ms`, or until the stream is interrupted; `null` waits for the
   * interruption alone.
   *
   * The pause itself is such a wait: nothing may connect while the app is not
   * on screen, and `resume()` is what ends it.
   */
  function wait(ms: number | null): Promise<void> {
    const { promise, resolve } = Promise.withResolvers<void>();
    // A clearing closure rather than a handle: the timer is created after
    // `wake` exists, and with `ms === null` there is nothing to clear.
    let clear = () => {};
    // The waiter is removed when the wait ends on its own, or one reconnect per
    // backoff would leave an entry behind for the life of the screen.
    const wake = () => {
      clear();
      waiters.delete(wake);
      resolve();
    };
    if (ms !== null) {
      const timer = setTimeout(wake, ms);
      clear = () => clearTimeout(timer);
    }
    waiters.add(wake);
    return promise;
  }

  /**
   * Opens one connection and consumes frames until it ends.
   *
   * The outcome says whether the connection proved itself — it has to stay open
   * for `HEALTHY_AFTER_MS` — and whether we ended it ourselves. A frame alone
   * does not reset the budget here: a server that answers one replay frame and
   * hangs up would otherwise be reconnected every half second for the life of
   * the screen, which is the polling loop this screen must not have.
   */
  async function openOnce(): Promise<OpenOutcome> {
    // Taken before the token renewal, because a pause during that await must
    // not be followed by a connection nobody asked for.
    const mine = generation;
    const token = await accessToken();
    const workspaceId = getActiveWorkspaceId();
    if (token === null || workspaceId === null) {
      return { healthy: false, interrupted: generation !== mine };
    }
    if (generation !== mine) return { healthy: false, interrupted: true };

    const query =
      `workspace_id=${encodeURIComponent(workspaceId)}` +
      `&token=${encodeURIComponent(token)}` +
      `&last_event_id=${lastId}`;

    // Each attempt owns its abort controller so a pause can end exactly this
    // one; `mine` above is what tells the outcome whose decision ended it.
    const controller = new AbortController();
    attempt = controller;

    const response = await expoFetch(`${API_URL}/api/events/stream?${query}`, {
      method: "GET",
      headers: { Accept: "text/event-stream" },
      signal: controller.signal,
    });

    if (!response.ok || response.body === null) {
      void response.body?.cancel().catch(() => {});
      if (response.status === 401) {
        // The clock thought the token was alive but the server refused it. A
        // renewal now gives the next attempt a token that works.
        await refreshSession();
      }
      return { healthy: false, interrupted: generation !== mine };
    }

    const openedAt = Date.now();
    const body = response.body;
    const reader = body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";

    // Cancelling the reader is what ends a `read()` parked on a socket the OS
    // already killed; aborting the fetch alone does not always reach it.
    cancelBody = () => void reader.cancel().catch(() => {});

    try {
      for (;;) {
        if (stopped || generation !== mine) break;
        const { done, value } = await reader.read();
        if (done) break;

        // A chunk may split a frame, a line, or even `\r\n`; only whole blocks
        // are handed to the parser, and the rest stays in the buffer.
        buffer += decoder.decode(value, { stream: true });
        buffer = buffer.replace(/\r\n/g, "\n");

        for (let cut = buffer.indexOf("\n\n"); cut >= 0; cut = buffer.indexOf("\n\n")) {
          const block = buffer.slice(0, cut);
          buffer = buffer.slice(cut + 2);

          const frame = parseFrame(block);
          if (frame === null) continue;
          const event = parseEvent(frame);
          if (event === null) continue;

          if (event.id !== undefined) {
            // The server may replay from a point before this connection; ids at
            // or below the cursor were already applied.
            if (event.id <= lastId) continue;
            lastId = event.id;
          }

          if (event.type === undefined || MESSAGE_EVENT_TYPES[event.type] !== true) continue;
          const message = readMessage(event.payload);
          if (message === null || message.conversation_id !== conversationId) continue;

          onMessage(message);
        }
      }
    } finally {
      void reader.cancel().catch(() => {});
      // Only this attempt may clear the slot; after an interrupt it belongs to
      // nobody, and a later attempt fills it again.
      if (attempt === controller) {
        attempt = null;
        cancelBody = null;
      }
    }

    const interrupted = stopped || generation !== mine;
    return { healthy: !interrupted && Date.now() - openedAt >= HEALTHY_AFTER_MS, interrupted };
  }

  async function run(): Promise<void> {
    while (!stopped) {
      // Nothing connects while the app is not on screen; `resume()` ends this
      // wait, and the loop then opens at once because the budget was reset.
      if (paused) {
        await wait(null);
        continue;
      }

      let outcome: OpenOutcome;
      const mine = generation;
      try {
        outcome = await openOnce();
      } catch {
        // An abort is a pause or a close; every other failure goes through the
        // same backoff, because a phone cannot tell a dead route from a tunnel.
        attempt = null;
        cancelBody = null;
        outcome = { healthy: false, interrupted: stopped || generation !== mine };
      }
      if (stopped) return;
      if (outcome.interrupted) continue;

      failures = outcome.healthy ? 0 : failures + 1;
      let delay: number;
      if (failures === 0) {
        delay = RECONNECT_DELAYS_MS[0];
      } else if (failures <= RECONNECT_DELAYS_MS.length) {
        delay = RECONNECT_DELAYS_MS[failures - 1];
      } else {
        delay = SLOW_RETRY_MS + Math.random() * SLOW_RETRY_JITTER_MS;
      }
      await wait(delay);
    }
  }

  void run();

  return {
    close() {
      if (stopped) return;
      stopped = true;
      appState.remove();
      interrupt();
    },
    pause,
    resume,
    lastEventId: () => lastId,
  };
}
