/**
 * The Redis token bucket used for login attempts and per-integration API
 * budgets.
 *
 * Ported from internal/platform/ratelimit/ratelimit.go. The bucket is atomic (a
 * Lua script), so parallel workers cannot each spend the last token.
 *
 * Fail-open: a Redis outage must not stop the product. A bucket that cannot be
 * read reports "allowed" and the caller loses pacing rather than the work. A
 * limiter that was never configured is a configuration error and throws, so the
 * caller decides whether that is fatal — the Go callers treat it as allowed too.
 */

import type { Redis } from "ioredis";
import type { Client } from "../redis/redis.ts";

/**
 * Refills the bucket according to elapsed time and then tries to take one
 * token. It returns `{allowed, remaining_millis_until_next_token}`.
 *
 * KEYS[1] bucket key
 * ARGV[1] capacity
 * ARGV[2] refill tokens per second
 * ARGV[3] now in unix milliseconds
 */
const BUCKET_SCRIPT = `
local capacity = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local now = tonumber(ARGV[3])

local state = redis.call('HMGET', KEYS[1], 'tokens', 'updated')
local tokens = tonumber(state[1])
local updated = tonumber(state[2])

if tokens == nil then
  tokens = capacity
  updated = now
end

local elapsed = math.max(0, now - updated) / 1000
tokens = math.min(capacity, tokens + elapsed * rate)

local allowed = 0
local retry_after = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
else
  retry_after = math.ceil((1 - tokens) / rate * 1000)
end

redis.call('HSET', KEYS[1], 'tokens', tokens, 'updated', now)
-- Buckets are idle most of the time; keep them only as long as a full refill
-- needs, with a floor so a slow bucket still survives a burst.
local ttl = math.ceil(capacity / rate * 1000) + 1000
redis.call('PEXPIRE', KEYS[1], ttl)

return {allowed, retry_after}
`;

/** Receives the errors that are swallowed by the fail-open behaviour. */
export type ErrorHandler = (error: Error) => void;

/** The outcome of one attempt. */
export interface Result {
  allowed: boolean;
  /** Milliseconds until the next token, zero when the attempt was allowed. */
  retryAfter: number;
}

/** Spends tokens from Redis buckets. */
export class Limiter {
  private readonly client: Client | null;
  private readonly onError: ErrorHandler;

  /**
   * A null client is allowed: `allow` then reports that limiting is not
   * configured, and the caller decides whether that is fatal.
   */
  constructor(client: Client | null, onError: ErrorHandler = () => undefined) {
    this.client = client;
    this.onError = onError;
  }

  /**
   * Takes one token from the bucket for `key`. `capacity` is the burst size and
   * `refill` is the steady rate in tokens per second.
   */
  async allow(
    ctx: AbortSignal | undefined,
    key: string,
    capacity: number,
    refill: number,
  ): Promise<Result> {
    if (this.client === null) {
      throw new Error("ratelimit: redis is not configured");
    }
    if (capacity <= 0) {
      throw new Error("ratelimit: capacity must be positive");
    }
    if (refill <= 0) {
      throw new Error("ratelimit: refill rate must be positive");
    }

    let raw: unknown;
    try {
      raw = await this.client.raw().eval(
        BUCKET_SCRIPT,
        1,
        key,
        String(capacity),
        String(refill),
        String(Date.now()),
      );
    } catch (err) {
      // A limiter that cannot answer must not stop the work: Redis being
      // unreachable is a reason to lose pacing, not a reason to lose the task.
      this.onError(err instanceof Error ? err : new Error(String(err)));
      return { allowed: true, retryAfter: 0 };
    }

    if (!Array.isArray(raw) || raw.length !== 2) {
      throw new Error(`ratelimit: unexpected bucket result ${JSON.stringify(raw)}`);
    }

    const allowed = parseInteger(raw[0]);
    const retryMillis = parseInteger(raw[1]);
    return { allowed: allowed === 1, retryAfter: retryMillis };
  }

  /**
   * Clears a bucket, used after a successful login so a legitimate user is not
   * punished for earlier typos.
   */
  async reset(ctx: AbortSignal | undefined, key: string): Promise<void> {
    if (this.client === null) {
      throw new Error("ratelimit: redis is not configured");
    }
    try {
      await this.client.raw().del(key);
    } catch (err) {
      this.onError(err instanceof Error ? err : new Error(String(err)));
    }
  }
}

/** Mirrors the Go `toInt64`, which accepts a Lua integer or a decimal string. */
function parseInteger(value: unknown): number {
  if (typeof value === "number" && Number.isInteger(value)) {
    return value;
  }
  if (typeof value === "string") {
    const parsed = Number.parseInt(value, 10);
    if (Number.isFinite(parsed)) {
      return parsed;
    }
    throw new Error(`ratelimit: parse ${JSON.stringify(value)}`);
  }
  throw new Error(`ratelimit: unexpected value type ${typeof value}`);
}
