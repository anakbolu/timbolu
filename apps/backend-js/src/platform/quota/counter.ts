/**
 * The live token spend of a workspace, kept in Redis.
 *
 * Ported from internal/platform/quota/counter.go. The ledger in Postgres is the
 * record of what was spent; this counter is the fast read the pre-flight check
 * makes before every call, so a workspace that exhausted its allowance is
 * rejected before the provider is billed.
 *
 * Because it is only a cache, a Redis outage fails open: a read answers zero and
 * a write is dropped rather than throwing, so an outage costs accuracy and not
 * availability. A counter that was never configured (no client) is a
 * configuration error and is reported as such.
 */

import type { Redis } from "ioredis";
import type { Client } from "../redis/redis.ts";

/** Namespaces the period counters, so they can be flushed together on rollover. */
const KEY_PREFIX = "quota:spent:";

/**
 * Namespaces the day counters, which answer a different question: how much a
 * workspace has spent today, in micro-rupiah.
 */
const COST_KEY_PREFIX = "quota:cost:";

/** Keeps a counter a little longer than the period it belongs to. */
const TTL_MILLIS = 40 * 24 * 60 * 60 * 1000;

/**
 * Increments the counter and gives it a lifetime if it has none. Both steps must
 * be atomic: a key that is incremented but never expires would leak one key per
 * workspace per period forever.
 */
const ADD_SCRIPT = `
local total = redis.call('INCRBY', KEYS[1], ARGV[1])
if redis.call('TTL', KEYS[1]) < 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return total
`;

/** Receives the errors that are swallowed by the fail-open behaviour. */
export type ErrorHandler = (error: Error) => void;

/** The calendar month and day in Jakarta, which is where the customers are. */
function jakartaDate(now: Date): { month: string; day: string } {
  const parts = new Intl.DateTimeFormat("en-CA", {
    timeZone: "Asia/Jakarta",
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
  }).formatToParts(now);

  const get = (type: Intl.DateTimeFormatPartTypes): string =>
    parts.find((part) => part.type === type)?.value ?? "";

  const year = get("year");
  const month = get("month");
  const day = get("day");
  return { month: `${year}-${month}`, day: `${year}-${month}-${day}` };
}

/** The Redis-backed live spend of a workspace. */
export class Counter {
  private readonly client: Client | null;
  private readonly onError: ErrorHandler;
  private clock: () => Date = () => new Date();

  /**
   * A null client is allowed: the counter then reports that it is not
   * configured, and the caller falls back to the ledger. `onError` observes the
   * Redis failures that are otherwise swallowed.
   */
  constructor(client: Client | null, onError: ErrorHandler = () => undefined) {
    this.client = client;
    this.onError = onError;
  }

  /** Returns the cost recorded for the workspace today, in micro-rupiah. */
  async spentCostToday(ctx: AbortSignal | undefined, workspaceID: string): Promise<number> {
    const redis = this.requireClient();
    try {
      const value = await redis.get(this.costKey(workspaceID));
      if (value === null) {
        // Nothing spent today, which is the normal state of a new day.
        return 0;
      }
      return parseTotal(value, "quota: read today's cost", this.onError);
    } catch (err) {
      // Fail open: an outage must not stop the call. The ledger still records
      // the truth, so the next successful read corrects the total.
      this.report(err);
      return 0;
    }
  }

  /** Records what one call cost, in micro-rupiah. */
  async addCost(
    ctx: AbortSignal | undefined,
    workspaceID: string,
    costMicros: number,
  ): Promise<void> {
    const redis = this.requireClient();
    if (costMicros <= 0) {
      // A call that cost nothing must not create a counter.
      return;
    }
    try {
      await redis.eval(
        ADD_SCRIPT,
        1,
        this.costKey(workspaceID),
        String(costMicros),
        String(TTL_MILLIS),
      );
    } catch (err) {
      this.report(err);
    }
  }

  /** Returns the tokens recorded for the workspace in the current period. */
  async spent(ctx: AbortSignal | undefined, workspaceID: string): Promise<number> {
    const redis = this.requireClient();
    try {
      const value = await redis.get(this.key(workspaceID));
      if (value === null) {
        // Nothing spent yet, which is the normal state of a new workspace.
        return 0;
      }
      return parseTotal(value, "quota: read spend", this.onError);
    } catch (err) {
      this.report(err);
      return 0;
    }
  }

  /** Records tokens spent by one call. */
  async add(ctx: AbortSignal | undefined, workspaceID: string, tokens: number): Promise<void> {
    const redis = this.requireClient();
    if (tokens <= 0) {
      // A call that reported no usage must not create a counter.
      return;
    }
    try {
      await redis.eval(
        ADD_SCRIPT,
        1,
        this.key(workspaceID),
        String(tokens),
        String(TTL_MILLIS),
      );
    } catch (err) {
      this.report(err);
    }
  }

  /** Clears the counter of a workspace, which the period rollover uses. */
  async reset(ctx: AbortSignal | undefined, workspaceID: string): Promise<void> {
    const redis = this.requireClient();
    try {
      await redis.del(this.key(workspaceID));
    } catch (err) {
      this.report(err);
    }
  }

  /**
   * The period the spend belongs to: the calendar month in Jakarta, which is
   * what the product sells today.
   */
  key(workspaceID: string): string {
    return `${KEY_PREFIX}${workspaceID}:${jakartaDate(this.clock()).month}`;
  }

  /**
   * The day the spend belongs to: the calendar day in Jakarta, which is where
   * the customers' day boundary falls.
   */
  costKey(workspaceID: string): string {
    return `${COST_KEY_PREFIX}${workspaceID}:${jakartaDate(this.clock()).day}`;
  }

  /** Replaces the clock, which lets a test cross a boundary without waiting. */
  setClock(clock: () => Date): void {
    this.clock = clock;
  }

  private requireClient(): Redis {
    if (this.client === null) {
      throw new Error("quota: redis is not configured");
    }
    return this.client.raw();
  }

  private report(err: unknown): void {
    this.onError(err instanceof Error ? err : new Error(String(err)));
  }
}

function parseTotal(value: string, what: string, onError: ErrorHandler): number {
  const total = Number.parseInt(value, 10);
  if (!Number.isFinite(total)) {
    // A key another process wrote by hand: treat it as unreadable rather than
    // letting NaN poison every comparison above.
    onError(new Error(`${what}: unreadable value ${value}`));
    return 0;
  }
  return total;
}
