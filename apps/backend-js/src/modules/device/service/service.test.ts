/**
 * The device registry's rules, without a database.
 *
 * The upsert itself is SQL's job, so these cases pin the half the service owns:
 * what reaches the repository, and which bodies never reach it at all. A
 * registration that was trimmed twice must arrive as the same token, because the
 * unique index is `(user_id, token)` and a stray space would be a second row.
 */

import { describe, expect, test } from "bun:test";

import {
  ErrInvalidInput,
  ErrNotFound,
  TOKEN_MAX_LENGTH,
  is,
  type Device,
  type RegisterRequest,
  type Repository,
  type Scope,
} from "../domain/device.ts";
import { Service } from "./service.ts";

const userId = "22222222-2222-2222-2222-222222222222";
const workspaceId = "11111111-1111-1111-1111-111111111111";

const scope: Scope = { userId, workspaceId };

/** Records what the service asked for, and answers as the stored row. */
class RecordingRepository implements Repository {
  readonly registered: Array<{ scope: Scope; request: RegisterRequest }> = [];
  readonly removed: Array<{ scope: Scope; token: string }> = [];

  async register(received: Scope, request: RegisterRequest): Promise<Device> {
    this.registered.push({ scope: received, request });
    return {
      id: "33333333-3333-3333-3333-333333333333",
      userId: received.userId ?? "",
      workspaceId: received.workspaceId,
      platform: request.platform,
      token: request.token,
      appVersion: request.appVersion,
      lastSeenAt: new Date("2026-10-30T09:00:00Z"),
      createdAt: new Date("2026-10-30T09:00:00Z"),
    };
  }

  async remove(received: Scope, token: string): Promise<void> {
    this.removed.push({ scope: received, token });
    throw ErrNotFound;
  }
}

function newService(): { repository: RecordingRepository; service: Service } {
  const repository = new RecordingRepository();
  return { repository, service: new Service({ repository }) };
}

describe("register", () => {
  test("hands the repository the trimmed token on every launch", async () => {
    const { repository, service } = newService();
    const request: RegisterRequest = {
      token: "  ExponentPushToken[abc123]  ",
      platform: " ios ",
      appVersion: " 1.0.0 ",
    };

    await service.register(scope, request);
    await service.register(scope, request);

    // Two calls rather than one row: "registering twice keeps one row" is the
    // upsert's promise, and the service's half is sending the same token both
    // times so the conflict is actually reached.
    expect(repository.registered).toHaveLength(2);
    expect(repository.registered[0]?.request).toEqual({
      token: "ExponentPushToken[abc123]",
      platform: "ios",
      appVersion: "1.0.0",
    });
    expect(repository.registered[1]?.request).toEqual(repository.registered[0]?.request);
    expect(repository.registered[0]?.scope).toEqual(scope);
  });

  test("refuses a platform the app does not run on", async () => {
    const { repository, service } = newService();

    const err = await service
      .register(scope, { token: "ExponentPushToken[abc123]", platform: "web", appVersion: "" })
      .catch((thrown: unknown) => thrown);

    expect(is(err, ErrInvalidInput)).toBe(true);
    expect(repository.registered).toHaveLength(0);
  });

  test("refuses an empty token and one longer than a push address", async () => {
    const { repository, service } = newService();
    const cases: RegisterRequest[] = [
      { token: "   ", platform: "android", appVersion: "" },
      { token: "a".repeat(TOKEN_MAX_LENGTH + 1), platform: "android", appVersion: "" },
    ];

    for (const request of cases) {
      const err = await service.register(scope, request).catch((thrown: unknown) => thrown);
      expect(is(err, ErrInvalidInput)).toBe(true);
    }
    expect(repository.registered).toHaveLength(0);
  });

  test("refuses a scope with no workspace", async () => {
    const { repository, service } = newService();

    const err = await service
      .register(
        { userId, workspaceId: "" },
        { token: "ExponentPushToken[abc123]", platform: "ios", appVersion: "" },
      )
      .catch((thrown: unknown) => thrown);

    expect(is(err, ErrInvalidInput)).toBe(true);
    expect(repository.registered).toHaveLength(0);
  });
});

describe("remove", () => {
  test("passes another account's token through as not found", async () => {
    const { repository, service } = newService();
    const other: Scope = { userId: "44444444-4444-4444-4444-444444444444", workspaceId };

    const err = await service.remove(other, "ExponentPushToken[abc123]").catch((thrown: unknown) => thrown);

    // Not rewritten into a refusal: whether somebody else registered the token is
    // not something this endpoint tells.
    expect(err).toBe(ErrNotFound);
    expect(repository.removed).toEqual([{ scope: other, token: "ExponentPushToken[abc123]" }]);
  });
});
