/**
 * The shared Redis client used for realtime pub/sub, rate-limit buckets and the
 * live quota counter.
 *
 * Ported from packages/platform/redis/redis.go. A Redis outage is not fatal to
 * the process: the container starts without a client, and the modules that need
 * one fall back to "no throttling" rather than failing a request.
 */

import { Redis } from "ioredis";

/** Wraps the Redis client. */
export class Client {
  private readonly client: Redis;

  private constructor(client: Redis) {
    this.client = client;
  }

  /**
   * Connects to Redis and verifies the connection with a ping.
   *
   * The connection is opened lazily, so importing this module never touches the
   * network: a bad or unreachable URL surfaces here, to the caller, as a
   * rejection it can log and continue without.
   */
  static async create(url: string): Promise<Client> {
    if (url === "") {
      throw new Error("redis url is required");
    }

    const parsed = parseUrl(url);
    const client = new Redis({
      ...parsed,
      lazyConnect: true,
      maxRetriesPerRequest: 1,
      // Reconnect in the background; a Redis that comes back must not need a
      // process restart.
      retryStrategy: (times) => Math.min(times * 200, 2000),
    });

    try {
      await client.connect();
      await client.ping();
    } catch (err) {
      client.disconnect();
      throw new Error(`ping redis: ${(err as Error).message}`);
    }

    return new Client(client);
  }

  /** The underlying client, for repositories and services. */
  raw(): Redis {
    return this.client;
  }

  /** Reports whether Redis is reachable; used by the readiness probe. */
  async ping(): Promise<void> {
    await this.client.ping();
  }

  /** Releases the client. */
  async close(): Promise<void> {
    await this.client.quit().catch(() => {
      this.client.disconnect();
    });
  }
}

/** Parses a `redis://` or `rediss://` URL into ioredis options. */
function parseUrl(url: string): {
  host: string;
  port: number;
  username?: string;
  password?: string;
  db: number;
  tls?: Record<string, never>;
} {
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    throw new Error(`parse redis url: invalid url ${url}`);
  }

  if (parsed.protocol !== "redis:" && parsed.protocol !== "rediss:") {
    throw new Error(`parse redis url: unsupported scheme ${parsed.protocol}`);
  }

  const port = parsed.port === "" ? 6379 : Number.parseInt(parsed.port, 10);
  if (!Number.isInteger(port) || port <= 0 || port > 65535) {
    throw new Error(`parse redis url: invalid port ${parsed.port}`);
  }

  const path = parsed.pathname.replace(/^\//, "");
  const db = path === "" ? 0 : Number.parseInt(path, 10);
  if (!Number.isInteger(db) || db < 0) {
    throw new Error(`parse redis url: invalid database ${path}`);
  }

  return {
    host: parsed.hostname,
    port,
    username: parsed.username === "" ? undefined : decodeURIComponent(parsed.username),
    password: parsed.password === "" ? undefined : decodeURIComponent(parsed.password),
    db,
    tls: parsed.protocol === "rediss:" ? {} : undefined,
  };
}
