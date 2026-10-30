/**
 * The Expo client's wire behaviour, with fetch stubbed.
 *
 * What is worth pinning here is what Expo's contract makes easy to get wrong:
 * the 100-message ceiling, the access token being optional, and a failure being
 * a result rather than an exception. The caller is a task that has already done
 * its work, so a thrown error from this client would be a business failure
 * caused by a push.
 */

import { describe, expect, test } from "bun:test";

import { BatchSize, Client, DefaultEndpoint, type Message } from "./client.ts";

// The client falls back to the deployment's env, so the cases below would
// change meaning on a machine that has an Expo project configured. Clearing
// both here makes the default the default.
delete process.env.EXPO_PUSH_URL;
delete process.env.EXPO_ACCESS_TOKEN;

/** One recorded request, as the stub saw it. */
interface Call {
  url: string;
  init: RequestInit;
}

/** A fetch stub answering each call with the next prepared response. */
function stubFetch(responses: Array<() => Response | Promise<Response>>): {
  calls: Call[];
  fetch: typeof fetch;
} {
  const calls: Call[] = [];
  const fn = (async (input: string | URL | Request, init?: RequestInit) => {
    calls.push({ url: String(input), init: init ?? {} });
    const next = responses.shift();
    if (!next) throw new Error("the client made more requests than the test prepared");
    return next();
  }) as unknown as typeof fetch;
  return { calls, fetch: fn };
}

function tickets(count: number, status = "ok", error?: string): Response {
  return new Response(
    JSON.stringify({
      data: Array.from({ length: count }, () => ({
        status,
        ...(status === "ok" ? { id: "ticket" } : { message: "no", details: { error } }),
      })),
    }),
    { status: 200, headers: { "Content-Type": "application/json" } },
  );
}

function message(to: string): Message {
  return { to, title: "Tugas selesai", body: "Bolu sudah selesai.", data: { deepLink: "bolu://task/1" } };
}

function messages(count: number): Message[] {
  return Array.from({ length: count }, (_, index) => message(`ExponentPushToken[t${index}]`));
}

describe("Client.send", () => {
  test("posts to Expo with the json headers", async () => {
    const stub = stubFetch([() => tickets(1)]);
    const result = await new Client({ fetch: stub.fetch }).send([message("ExponentPushToken[a]")]);

    expect(stub.calls).toHaveLength(1);
    expect(stub.calls[0].url).toBe(DefaultEndpoint);
    expect(stub.calls[0].init.method).toBe("POST");
    const headers = stub.calls[0].init.headers as Record<string, string>;
    expect(headers.Accept).toBe("application/json");
    expect(headers["Content-Type"]).toBe("application/json");
    // No access token configured means no Authorization at all: an empty Bearer
    // is a credential the request does not have.
    expect(headers.Authorization).toBeUndefined();
    expect(result).toEqual({ sent: 1, rejected: 0, unregistered: [] });
  });

  test("sends the access token when one is configured", async () => {
    const stub = stubFetch([() => tickets(1)]);
    await new Client({ fetch: stub.fetch, accessToken: "secret" }).send([message("ExponentPushToken[a]")]);
    const headers = stub.calls[0].init.headers as Record<string, string>;
    expect(headers.Authorization).toBe("Bearer secret");
  });

  test("the endpoint can be pointed elsewhere for a proxy", async () => {
    const stub = stubFetch([() => tickets(1)]);
    await new Client({ fetch: stub.fetch, url: "http://localhost:9999/send" }).send([message("ExponentPushToken[a]")]);
    expect(stub.calls[0].url).toBe("http://localhost:9999/send");
  });

  test("splits more than a hundred messages across requests", async () => {
    const stub = stubFetch([() => tickets(BatchSize), () => tickets(50)]);
    const result = await new Client({ fetch: stub.fetch }).send(messages(150));

    expect(stub.calls).toHaveLength(2);
    expect((JSON.parse(String(stub.calls[0].init.body)) as Message[]).length).toBe(BatchSize);
    expect((JSON.parse(String(stub.calls[1].init.body)) as Message[]).length).toBe(50);
    expect(result.sent).toBe(150);
    expect(result.rejected).toBe(0);
  });

  test("a network failure is reported, not thrown", async () => {
    const stub = stubFetch([
      () => {
        throw new Error("offline");
      },
    ]);
    const result = await new Client({ fetch: stub.fetch }).send(messages(2));
    expect(result).toEqual({ sent: 0, rejected: 2, unregistered: [] });
  });

  test("a non-2xx answer is reported, not thrown", async () => {
    const stub = stubFetch([() => new Response("nope", { status: 500 })]);
    const result = await new Client({ fetch: stub.fetch }).send(messages(3));
    expect(result.sent).toBe(0);
    expect(result.rejected).toBe(3);
  });

  test("a body that is not json is reported, not thrown", async () => {
    const stub = stubFetch([() => new Response("<html>", { status: 200 })]);
    const result = await new Client({ fetch: stub.fetch }).send(messages(1));
    expect(result.rejected).toBe(1);
  });

  test("names the tokens Expo answered DeviceNotRegistered for", async () => {
    const stub = stubFetch([
      () =>
        new Response(
          JSON.stringify({
            data: [
              { status: "ok", id: "ticket" },
              { status: "error", message: "gone", details: { error: "DeviceNotRegistered" } },
              { status: "error", message: "slow down", details: { error: "MessageRateExceeded" } },
            ],
          }),
          { status: 200 },
        ),
    ]);
    const batch = [message("ExponentPushToken[ok]"), message("ExponentPushToken[gone]"), message("ExponentPushToken[slow]")];
    const result = await new Client({ fetch: stub.fetch }).send(batch);

    expect(result.sent).toBe(1);
    expect(result.rejected).toBe(2);
    // The ticket index is the only way back to the address; the other error must
    // not be mistaken for a dead token.
    expect(result.unregistered).toEqual(["ExponentPushToken[gone]"]);
  });

  test("an answer with no tickets counts every message as rejected", async () => {
    const stub = stubFetch([() => new Response(JSON.stringify({ data: [] }), { status: 200 })]);
    const result = await new Client({ fetch: stub.fetch }).send(messages(2));
    expect(result.sent).toBe(0);
    expect(result.rejected).toBe(2);
  });

  test("sends nothing and asks for nothing when the list is empty", async () => {
    const stub = stubFetch([]);
    const result = await new Client({ fetch: stub.fetch }).send([]);
    expect(stub.calls).toHaveLength(0);
    expect(result).toEqual({ sent: 0, rejected: 0, unregistered: [] });
  });
});
