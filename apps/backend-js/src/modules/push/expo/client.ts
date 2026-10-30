/**
 * The Expo Push API client: one request per hundred messages, and no failure
 * that reaches the caller.
 *
 * Expo accepts at most 100 messages per request, so an account with more phones
 * than that is several requests rather than one larger body. A push is a
 * courtesy on top of a business action that has already happened: a dead
 * network, a missing access token, or Expo answering 500 must be a log line,
 * never an exception that unwinds a task or an approval. The result still says
 * what happened, so the caller can prune the tokens Expo reports as gone.
 *
 * The endpoint, the fetch and the access token are injected because the tests
 * must not reach the network, and `EXPO_PUSH_URL` lets a deployment point at a
 * proxy without a code change.
 */

import type { Logger } from "../../../platform/logger.ts";

/** Where Expo accepts a batch, unless the deployment overrides it. */
export const DefaultEndpoint = "https://exp.host/--/api/v2/push/send";

/** The most messages one request may carry, per Expo's contract. */
export const BatchSize = 100;

/** One message in Expo's wire format. */
export interface Message {
  to: string;
  title: string;
  body: string;
  sound?: "default";
  data?: Record<string, unknown>;
}

/** One per-message answer from Expo, in the order the messages were sent. */
export interface Ticket {
  status?: string;
  id?: string;
  message?: string;
  details?: { error?: string } | null;
}

/** The body Expo answers with; `errors` is present when the request itself failed. */
interface ResponseBody {
  data?: Ticket[] | null;
  errors?: Array<{ code?: string; message?: string }> | null;
}

/** What a send did, and which tokens are no longer worth keeping. */
export interface Result {
  /** Messages Expo accepted. */
  sent: number;
  /** Messages Expo refused, or that never got an answer. */
  rejected: number;
  /** Tokens Expo answered `DeviceNotRegistered` for, so the caller can delete them. */
  unregistered: string[];
}

/** The client dependencies. Every one is optional so production is the default. */
export interface Deps {
  fetch?: typeof fetch;
  url?: string;
  accessToken?: string | null;
  logger?: Logger | null;
}

/** The Expo Push API client. */
export class Client {
  constructor(private readonly deps: Deps = {}) {}

  /** Sends every message, in batches Expo accepts, and never throws. */
  async send(messages: Message[]): Promise<Result> {
    const result: Result = { sent: 0, rejected: 0, unregistered: [] };
    for (let start = 0; start < messages.length; start += BatchSize) {
      await this.sendBatch(messages.slice(start, start + BatchSize), result);
    }
    return result;
  }

  private async sendBatch(batch: Message[], result: Result): Promise<void> {
    const doFetch = this.deps.fetch ?? fetch;
    const endpoint = this.deps.url ?? process.env.EXPO_PUSH_URL ?? DefaultEndpoint;

    const headers: Record<string, string> = {
      Accept: "application/json",
      "Content-Type": "application/json",
    };
    // Expo only wants the header when the project has an access token; sending
    // an empty Bearer would be a credential the request does not have.
    const accessToken = (this.deps.accessToken ?? process.env.EXPO_ACCESS_TOKEN ?? "").trim();
    if (accessToken !== "") {
      headers.Authorization = `Bearer ${accessToken}`;
    }

    let response: Response;
    try {
      response = await doFetch(endpoint, {
        method: "POST",
        headers,
        body: JSON.stringify(batch),
      });
    } catch (err) {
      this.deps.logger?.warn({ err, count: batch.length }, "expo push request failed");
      result.rejected += batch.length;
      return;
    }

    if (!response.ok) {
      this.deps.logger?.warn(
        { status: response.status, count: batch.length },
        "expo push answered an error",
      );
      result.rejected += batch.length;
      return;
    }

    let body: ResponseBody;
    try {
      body = (await response.json()) as ResponseBody;
    } catch (err) {
      this.deps.logger?.warn({ err, count: batch.length }, "expo push answer was not json");
      result.rejected += batch.length;
      return;
    }

    const tickets = Array.isArray(body.data) ? body.data : [];
    if (tickets.length === 0) {
      // No ticket is not a success: the batch was sent and Expo said nothing
      // about it, so every message in it is unaccounted for.
      this.deps.logger?.warn(
        { count: batch.length, errors: body.errors ?? null },
        "expo push answered no tickets",
      );
      result.rejected += batch.length;
      return;
    }

    tickets.forEach((ticket, index) => {
      if (ticket.status === "ok") {
        result.sent++;
        return;
      }

      result.rejected++;
      if (ticket.details?.error !== "DeviceNotRegistered") {
        return;
      }
      // Tickets arrive in the order the messages were sent, which is the only
      // way to know which address Expo wants forgotten.
      const message = batch[index];
      if (message) {
        result.unregistered.push(message.to);
      }
    });
  }
}
