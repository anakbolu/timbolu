/**
 * The async mail sender: a bounded queue and a fixed set of workers, so a
 * request that triggers an email never waits for an SMTP server.
 *
 * Ported from apps/backend/internal/platform/mail/async.go.
 */

import { formatLog, validate, type LogFunc, type Message, type Sender } from "./mail";

/** A queue of 256 with two workers absorbs a burst of sign-ups. */
export const defaultAsyncQueueSize = 256;
export const defaultAsyncWorkers = 2;
export const defaultAsyncAttemptTimeoutMs = 15_000;
export const defaultAsyncAttempts = 3;
export const defaultAsyncBackoffMs = 1_000;

/** Tunes an AsyncSender. Every field has a default. */
export interface AsyncOptions {
  /** How many messages may wait for a worker; a full queue drops and reports. */
  queueSize?: number;
  workers?: number;
  /** Bounds one delivery attempt, including the SMTP session. */
  attemptTimeout?: number;
  /** How many times one message is tried before it is given up on. */
  attempts?: number;
  /** The pause before the second attempt; each further attempt waits twice as long. */
  backoff?: number;
  /** Reports a dropped or failed delivery. Receives recipient and subject, never the body. */
  errorf?: LogFunc;
}

interface ResolvedOptions {
  queueSize: number;
  workers: number;
  attemptTimeout: number;
  attempts: number;
  backoff: number;
  errorf: LogFunc;
}

/**
 * Hands a message to a worker pool and returns at once.
 *
 * Delivery deliberately ignores the caller's context: the request is over, and
 * its context cancelled, by the time the message leaves. Each attempt gets a
 * detached signal of its own, bounded by attemptTimeout.
 */
export class AsyncSender implements Sender {
  private readonly inner: Sender;
  private readonly opts: ResolvedOptions;
  private readonly queue: Message[] = [];
  private closed = false;
  private readonly workers: Promise<void>[] = [];
  /** Workers parked on an empty queue, woken by a push or by close. */
  private waiters: Array<() => void> = [];

  constructor(inner: Sender, opts: AsyncOptions = {}) {
    this.inner = inner;
    this.opts = {
      queueSize: opts.queueSize && opts.queueSize > 0 ? opts.queueSize : defaultAsyncQueueSize,
      workers: opts.workers && opts.workers > 0 ? opts.workers : defaultAsyncWorkers,
      attemptTimeout:
        opts.attemptTimeout && opts.attemptTimeout > 0 ? opts.attemptTimeout : defaultAsyncAttemptTimeoutMs,
      attempts: opts.attempts && opts.attempts > 0 ? opts.attempts : defaultAsyncAttempts,
      backoff: opts.backoff && opts.backoff > 0 ? opts.backoff : defaultAsyncBackoffMs,
      errorf: opts.errorf ?? (() => {}),
    };

    for (let i = 0; i < this.opts.workers; i++) {
      this.workers.push(this.work());
    }
  }

  /**
   * Validates the message and queues it. A malformed message is a bug in the
   * request, so its error is returned; once queued the caller is done.
   */
  async send(message: Message, _signal?: AbortSignal): Promise<void> {
    validate(message);

    if (this.closed) {
      this.opts.errorf(
        formatLog("mail: dropped, sender is shutting down: to=%s subject=%q", message.to, message.subject),
      );
      return;
    }

    if (this.queue.length >= this.opts.queueSize) {
      // A full queue means the mail server is slower than the traffic. The
      // caller is never blocked for it, and the loss is visible in the log.
      this.opts.errorf(
        formatLog(
          "mail: queue is full (%d), dropped: to=%s subject=%q",
          this.opts.queueSize,
          message.to,
          message.subject,
        ),
      );
      return;
    }

    this.queue.push(message);
    this.notify();
  }

  /**
   * Stops accepting messages and waits for the queued ones to be delivered, or
   * for the signal to fire.
   */
  async close(signal?: AbortSignal): Promise<void> {
    this.closed = true;
    this.notify();

    const drained = Promise.all(this.workers).then(() => undefined);
    if (!signal) {
      await drained;
      return;
    }
    if (signal.aborted) {
      throw new Error("mail: close cancelled");
    }

    const cancelled = Promise.withResolvers<never>();
    const onAbort = () => cancelled.reject(new Error("mail: close cancelled"));
    signal.addEventListener("abort", onAbort, { once: true });
    try {
      await Promise.race([drained, cancelled.promise]);
    } finally {
      signal.removeEventListener("abort", onAbort);
    }
  }

  private notify(): void {
    const waiters = this.waiters;
    this.waiters = [];
    for (const wake of waiters) wake();
  }

  /** Delivers queued messages until the queue is closed and empty. */
  private async work(): Promise<void> {
    for (;;) {
      const message = this.queue.shift();
      if (message) {
        await this.deliver(message);
        continue;
      }
      if (this.closed) {
        return;
      }
      const { promise, resolve } = Promise.withResolvers<void>();
      this.waiters.push(resolve);
      await promise;
    }
  }

  /** Attempts one message until it is accepted or the attempts run out. */
  private async deliver(message: Message): Promise<void> {
    let backoff = this.opts.backoff;

    for (let attempt = 1; ; attempt++) {
      let err: Error | null = null;
      try {
        await this.inner.send(message, AbortSignal.timeout(this.opts.attemptTimeout));
      } catch (caught) {
        err = caught instanceof Error ? caught : new Error(String(caught));
      }

      if (!err) {
        return;
      }
      if (attempt >= this.opts.attempts) {
        this.opts.errorf(
          formatLog(
            "mail: delivery failed after %d attempts: to=%s subject=%q: %v",
            attempt,
            message.to,
            message.subject,
            err,
          ),
        );
        return;
      }
      this.opts.errorf(
        formatLog(
          "mail: attempt %d failed, retrying in %s: to=%s subject=%q: %v",
          attempt,
          backoff,
          message.to,
          message.subject,
          err,
        ),
      );

      const { promise, resolve } = Promise.withResolvers<void>();
      setTimeout(resolve, backoff);
      await promise;
      backoff *= 2;
    }
  }
}
