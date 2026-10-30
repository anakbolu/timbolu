/**
 * The push use case's rules, without a database or the network.
 *
 * These cases pin the decisions the service owns rather than the modules it
 * calls: which devices are worth sending to, what happens to a token Expo says is
 * gone, and that an account with no phones causes no request at all. The Expo
 * client and the device registry are fakes, so a case that fails here is a rule
 * that changed, not a stub that drifted.
 */

import { describe, expect, test } from "bun:test";

import { EventTaskFinished, EventTaskStarted } from "../../chat/domain/event.ts";
import type { Device } from "../../device/domain/device.ts";
import { Client as ExpoClient } from "../expo/client.ts";
import type { Message, Result } from "../expo/client.ts";
import { Service, buildNotifier, type DeviceRegistry, type Sender } from "./service.ts";

const userId = "22222222-2222-2222-2222-222222222222";
const workspaceId = "11111111-1111-1111-1111-111111111111";
const taskId = "44444444-4444-4444-4444-444444444444";

const scope = { userId, workspaceId };

function device(token: string): Device {
  return {
    id: "33333333-3333-3333-3333-333333333333",
    userId,
    workspaceId,
    platform: "ios",
    token,
    appVersion: "1.0.0",
    lastSeenAt: new Date("2026-10-30T09:00:00Z"),
    createdAt: new Date("2026-10-30T09:00:00Z"),
  };
}

/** Answers with a fixed list of devices and records what was removed. */
class FakeDevices implements DeviceRegistry {
  readonly removed: Array<{ scope: unknown; token: string }> = [];
  readonly listed: unknown[] = [];
  failure: Error | null = null;

  constructor(private readonly devices: Device[]) {}

  async listTokens(received: unknown): Promise<Device[]> {
    this.listed.push(received);
    if (this.failure) throw this.failure;
    return this.devices;
  }

  async remove(received: unknown, token: string): Promise<void> {
    this.removed.push({ scope: received, token });
  }
}

/** Records the batches it was handed and answers with a prepared result. */
class FakeSender implements Sender {
  readonly batches: Message[][] = [];
  result: Result = { sent: 0, rejected: 0, unregistered: [] };
  failure: Error | null = null;

  async send(messages: Message[]): Promise<Result> {
    this.batches.push(messages);
    if (this.failure) throw this.failure;
    return this.result;
  }
}

function newService(devices: Device[]): {
  registry: FakeDevices;
  sender: FakeSender;
  service: Service;
} {
  const registry = new FakeDevices(devices);
  const sender = new FakeSender();
  return { registry, sender, service: new Service({ devices: registry, expo: sender }) };
}

describe("send", () => {
  test("sends one message per registered phone, with the deep link in data", async () => {
    const { sender, service } = newService([
      device("ExponentPushToken[one]"),
      device("ExpoPushToken[two]"),
    ]);
    sender.result = { sent: 2, rejected: 0, unregistered: [] };

    await service.send(scope, { type: EventTaskFinished, taskId });

    expect(sender.batches).toHaveLength(1);
    expect(sender.batches[0].map((message) => message.to)).toEqual([
      "ExponentPushToken[one]",
      "ExpoPushToken[two]",
    ]);
    expect(sender.batches[0][0].data).toEqual({ deepLink: `bolu://task/${taskId}` });
    expect(sender.batches[0][0].title).toBe("Tugas selesai");
  });

  test("an account with no phones sends nothing", async () => {
    const { sender, service } = newService([]);
    const result = await service.send(scope, { type: EventTaskFinished, taskId });
    expect(sender.batches).toHaveLength(0);
    expect(result).toBeNull();
  });

  test("skips a stored value that is not an Expo address instead of failing the batch", async () => {
    const { sender, service } = newService([
      device("ExponentPushToken[good]"),
      device("  "),
      device("apns:not-an-expo-token"),
      device("ExponentPushToken[good]"),
    ]);
    sender.result = { sent: 1, rejected: 0, unregistered: [] };

    await service.send(scope, { type: EventTaskFinished, taskId });

    expect(sender.batches[0].map((message) => message.to)).toEqual(["ExponentPushToken[good]"]);
  });

  test("deletes the token Expo reports as DeviceNotRegistered", async () => {
    const { registry, sender, service } = newService([device("ExponentPushToken[gone]")]);
    sender.result = { sent: 0, rejected: 1, unregistered: ["ExponentPushToken[gone]"] };

    await service.send(scope, { type: EventTaskFinished, taskId });

    expect(registry.removed).toHaveLength(1);
    expect(registry.removed[0].token).toBe("ExponentPushToken[gone]");
    expect(registry.removed[0].scope).toEqual(scope);
  });

  test("an event outside the five is quiet and reads no registry", async () => {
    const { registry, sender, service } = newService([device("ExponentPushToken[one]")]);
    const result = await service.send(scope, { type: EventTaskStarted, taskId });
    expect(result).toBeNull();
    expect(registry.listed).toHaveLength(0);
    expect(sender.batches).toHaveLength(0);
  });

  test("a scope without a user sends nothing", async () => {
    const { registry, sender, service } = newService([device("ExponentPushToken[one]")]);
    const result = await service.send({ workspaceId }, { type: EventTaskFinished, taskId });
    expect(result).toBeNull();
    expect(registry.listed).toHaveLength(0);
    expect(sender.batches).toHaveLength(0);
  });

  test("a registry failure is a log line, not an exception", async () => {
    const { registry, sender, service } = newService([]);
    registry.failure = new Error("database is down");
    const result = await service.send(scope, { type: EventTaskFinished, taskId });
    expect(result).toBeNull();
    expect(sender.batches).toHaveLength(0);
  });

  test("a sender failure is a log line, not an exception", async () => {
    const { registry, sender, service } = newService([device("ExponentPushToken[one]")]);
    sender.failure = new Error("expo is down");
    const result = await service.send(scope, { type: EventTaskFinished, taskId });
    expect(result).toBeNull();
    expect(registry.removed).toHaveLength(0);
  });

  test("a prune that fails does not lose the send result", async () => {
    const { registry, sender, service } = newService([device("ExponentPushToken[gone]")]);
    sender.result = { sent: 0, rejected: 1, unregistered: ["ExponentPushToken[gone]"] };
    registry.remove = async () => {
      throw new Error("already deleted");
    };

    const result = await service.send(scope, { type: EventTaskFinished, taskId });
    expect(result?.unregistered).toEqual(["ExponentPushToken[gone]"]);
  });
});

describe("the kill switch", () => {
  test("off reads no registry and makes no request, even with a registered phone", async () => {
    const registry = new FakeDevices([device("ExponentPushToken[one]")]);
    let calls = 0;
    const expo = new ExpoClient({
      fetch: (async () => {
        calls += 1;
        return new Response("{}");
      }) as unknown as typeof fetch,
    });
    const notifier = buildNotifier({ enabled: false, devices: registry, expo });

    const result = await notifier.send(scope, { type: EventTaskFinished, taskId });

    expect(result).toBeNull();
    expect(calls).toBe(0);
    expect(registry.listed).toHaveLength(0);
  });

  test("on still sends through the Expo client", async () => {
    const registry = new FakeDevices([device("ExponentPushToken[one]")]);
    const bodies: Message[][] = [];
    const expo = new ExpoClient({
      fetch: (async (_url: string, init: RequestInit) => {
        bodies.push(JSON.parse(String(init.body)) as Message[]);
        return new Response(JSON.stringify({ data: [{ status: "ok" }] }), {
          headers: { "Content-Type": "application/json" },
        });
      }) as unknown as typeof fetch,
    });
    const notifier = buildNotifier({ enabled: true, devices: registry, expo });

    const result = (await notifier.send(scope, { type: EventTaskFinished, taskId })) as Result;

    expect(bodies).toHaveLength(1);
    expect(bodies[0].map((message) => message.to)).toEqual(["ExponentPushToken[one]"]);
    expect(result).toEqual({ sent: 1, rejected: 0, unregistered: [] });
  });
});
