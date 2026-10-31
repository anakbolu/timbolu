/**
 * Publishes the activity stream over Redis.
 *
 * Ported from internal/platform/pubsub/pubsub.go. Redis is the fast path, not
 * the record: an event is written to `activity_events` before it is published
 * here, so a subscriber that misses a publish still finds the row when it
 * reconnects.
 */

import type { Redis } from "ioredis";
import type { Client } from "../redis/redis.ts";

/**
 * Namespaces the per-workspace channels, so one tenant's events are one channel
 * and a subscriber never filters another tenant's traffic.
 */
export const CHANNEL_PREFIX = "ws:";

/** The Redis channel of one workspace. */
export function channel(workspaceID: string): string {
  return CHANNEL_PREFIX + workspaceID;
}

/**
 * One row of the activity stream, as it travels over Redis.
 *
 * The Go `chatdomain.Event` carries no JSON tags, so Go marshals its field names
 * verbatim: these keys are PascalCase on the wire and must stay that way. The
 * type belongs in the chat domain once that module is ported; it is declared
 * here because this package is what publishes it.
 */
export interface Event {
  ID: number;
  WorkspaceID: string;
  Type: string;
  ActorAgentID: string;
  ActorUserID: string;
  ConversationID: string;
  TaskID: string;
  DraftID: string;
  Payload: unknown;
  /** RFC3339, because Go marshals `time.Time` that way. */
  CreatedAt: string;
}

/** How many live events may wait for a slow consumer before the producer waits. */
const BUFFER = 64;

/** Publishes and subscribes to the workspace channels. */
export class Publisher {
  private readonly client: Client | null;

  /**
   * A null client is allowed: publishing then reports that the fast path is
   * unavailable, which the event sink treats as a warning rather than a
   * failure, because the row is already written.
   */
  constructor(client: Client | null) {
    this.client = client;
  }

  /** Sends one event to the workspace channel. */
  async publish(workspaceID: string, event: Event): Promise<void> {
    let payload: string;
    try {
      payload = JSON.stringify(event);
    } catch (err) {
      throw new Error(`pubsub: encode event: ${errorText(err)}`);
    }
    await this.publishChannel(channel(workspaceID), payload);
  }

  /**
   * Publishes one raw payload to an arbitrary channel.
   *
   * Workspace events go through here too; the readiness probe is the only
   * caller that needs a channel outside the `ws:` namespace.
   */
  async publishChannel(name: string, payload: string): Promise<void> {
    if (this.client === null) {
      throw new Error("pubsub: redis is not configured");
    }
    try {
      await this.client.raw().publish(name, payload);
    } catch (err) {
      throw new Error(`pubsub: publish: ${errorText(err)}`);
    }
  }

  /**
   * Returns the live events of one workspace.
   *
   * The subscription is confirmed before this resolves, so a publish that
   * happens after `subscribe` returned is guaranteed to arrive. The returned
   * iterable ends when the signal aborts; breaking out of the `for await`
   * releases the dedicated Redis connection, so a disconnected client cannot
   * leak a subscription.
   */
  async subscribe(
    workspaceID: string,
    signal?: AbortSignal,
  ): Promise<AsyncIterable<Event>> {
    const name = channel(workspaceID);
    return this.stream(await this.open(name, signal), name, signal, (payload) => {
      // One unreadable payload must not end the subscription for every event
      // after it.
      try {
        return JSON.parse(payload) as Event;
      } catch {
        return null;
      }
    });
  }

  /** Subscribes to an arbitrary channel and yields its raw payloads. */
  async subscribeChannel(
    name: string,
    signal?: AbortSignal,
  ): Promise<AsyncIterable<string>> {
    return this.stream(await this.open(name, signal), name, signal, (payload) => payload);
  }

  /** Confirms one subscription on a connection of its own. */
  private async open(name: string, signal: AbortSignal | undefined): Promise<Redis> {
    if (this.client === null) {
      throw new Error("pubsub: redis is not configured");
    }
    if (signal?.aborted) {
      throw new Error("pubsub: subscribe: context canceled");
    }

    // A subscribed connection cannot run ordinary commands, so the
    // subscription gets its own connection rather than borrowing the shared
    // one.
    const subscriber = this.client.raw().duplicate();

    try {
      if (subscriber.status === "wait") {
        await subscriber.connect();
      }
      await subscriber.subscribe(name);
    } catch (err) {
      subscriber.disconnect();
      throw new Error(`pubsub: subscribe: ${errorText(err)}`);
    }

    return subscriber;
  }

  /** Turns one confirmed subscription into an async iterable that owns it. */
  private stream<T>(
    subscriber: Redis,
    name: string,
    signal: AbortSignal | undefined,
    decode: (payload: string) => T | null,
  ): AsyncIterable<T> {
    const queue: T[] = [];
    const waiters: Array<() => void> = [];
    let notify: (() => void) | null = null;
    let closed = false;
    // Delivery is serialised through one promise chain so the buffer keeps the
    // order Redis sent, even when a slow consumer fills it.
    let chain: Promise<void> = Promise.resolve();

    const wake = (): void => {
      const resume = notify;
      notify = null;
      resume?.();
    };
    const onMessage = (incoming: string, payload: string): void => {
      if (incoming !== name) return;
      const event = decode(payload);
      // One unreadable payload must not end the subscription for every event
      // after it.
      if (event === null) return;
      chain = chain.then(async () => {
        while (queue.length >= BUFFER && !closed) {
          const slot = Promise.withResolvers<void>();
          waiters.push(slot.resolve);
          await slot.promise;
        }
        if (closed) return;
        queue.push(event);
        wake();
      });
    };
    const onEnd = (): void => {
      closed = true;
      wake();
      while (waiters.length > 0) waiters.shift()?.();
    };
    const onAbort = onEnd;

    subscriber.on("message", onMessage);
    subscriber.on("end", onEnd);
    subscriber.on("close", onEnd);
    signal?.addEventListener("abort", onAbort, { once: true });
    // The signal may have fired while the subscription was being confirmed.
    if (signal?.aborted) onAbort();

    const release = (): void => {
      subscriber.off("message", onMessage);
      subscriber.off("end", onEnd);
      subscriber.off("close", onEnd);
      signal?.removeEventListener("abort", onAbort);
      subscriber
        .quit()
        .catch(() => {
          subscriber.disconnect();
        });
    };

    return {
      async *[Symbol.asyncIterator](): AsyncGenerator<T> {
        try {
          for (;;) {
            if (queue.length > 0) {
              yield queue.shift() as T;
              waiters.shift()?.();
              continue;
            }
            if (closed) return;
            const next = Promise.withResolvers<void>();
            notify = next.resolve;
            await next.promise;
          }
        } finally {
          release();
        }
      },
    };
  }
}

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/**
 * Proves the publish → subscribe path a live stream depends on, within a bound.
 *
 * A readiness probe that only pings Redis stays green when the two halves
 * disagree, which is exactly how a dead live stream goes unnoticed. This one
 * carries a frame across the same two calls the stream uses. The channel is
 * unique per call so two instances probing at once cannot steal each other's
 * frame, and it sits outside the `ws:` namespace so no workspace subscriber
 * ever sees it.
 */
export async function probeRoundTrip(publisher: Publisher, timeoutMs = 1000): Promise<void> {
  const name = `health:probe:${crypto.randomUUID()}`;
  const controller = new AbortController();
  let timedOut = false;
  const timer = setTimeout(() => {
    timedOut = true;
    controller.abort();
  }, timeoutMs);

  try {
    let frames: AsyncIterable<string>;
    try {
      // `subscribe` is confirmed before it resolves, so a publish after this
      // point cannot be missed.
      frames = await publisher.subscribeChannel(name, controller.signal);
    } catch (err) {
      if (timedOut) throw new Error("pubsub: probe timed out");
      throw err;
    }

    // A publish failure aborts the subscription, which is how the loop below
    // ends and releases its connection; the error is rethrown after the loop.
    let failure: unknown = null;
    const sent = publisher.publishChannel(name, "1").catch((err: unknown) => {
      failure = err;
      controller.abort();
    });

    let received = false;
    for await (const _frame of frames) {
      received = true;
      // Ending the loop here runs the iterable's cleanup, which closes the
      // dedicated connection.
      controller.abort();
    }
    await sent;

    if (failure !== null) throw failure;
    if (!received) {
      if (timedOut) throw new Error("pubsub: probe timed out");
      throw new Error("pubsub: probe: subscription ended before a frame arrived");
    }
  } finally {
    clearTimeout(timer);
    controller.abort();
  }
}
