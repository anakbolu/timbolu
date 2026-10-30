/**
 * Exposes the device registry over HTTP: registering a phone's push address, and
 * removing it when the phone signs out.
 *
 * Both routes are the caller's own rows. The user comes from the bearer token
 * and the workspace from the tenant middleware, so a client never names either —
 * and a token belonging to another account is answered as "not found" rather
 * than as "forbidden", because whether it exists is not this endpoint's to tell.
 *
 * Ported 1:1 from internal/modules/device/handler/handler.go.
 */

import type { Hono } from "hono";
import { currentUserId, workspaceUUID, type AppContext, type AppEnv } from "../../../platform/context.ts";
import type { Logger } from "../../../platform/logger.ts";
import { created, error } from "../../../platform/response.ts";
import {
  ErrInvalidInput,
  ErrNotFound,
  errorMessage,
  is,
  isNilID,
  type Device,
  type Scope,
  type Service,
} from "../domain/device.ts";

/** The body a registration carries. Every field is optional: the service is what refuses an absent one. */
interface RegisterBody {
  token?: string;
  platform?: string;
  app_version?: string;
}

/** Serves the device endpoints. */
export class Handler {
  constructor(
    private readonly service: Service,
    private readonly log: Logger,
  ) {}

  /**
   * Stores a phone's push address, updating the row a repeated token already
   * has.
   */
  register = async (c: AppContext): Promise<Response> => {
    const body = await readBody<RegisterBody>(c, {
      token: "string",
      platform: "string",
      app_version: "string",
    });
    if (!body) {
      return error(c, 400, "invalid request body", "invalid_request");
    }

    let device: Device;
    try {
      device = await this.service.register(requestScope(c), {
        token: body.token ?? "",
        platform: body.platform ?? "",
        appVersion: body.app_version ?? "",
      });
    } catch (err) {
      return this.fail(c, "register device", err);
    }

    return created(c, "perangkat terdaftar", devicePayload(device));
  };

  /**
   * Removes one of the caller's tokens.
   *
   * 204 rather than an envelope: the row is gone, and there is nothing about it
   * left to say. A token that is not the caller's is 404, which is the same
   * answer as a token nobody registered.
   */
  unregister = async (c: AppContext): Promise<Response> => {
    try {
      await this.service.remove(requestScope(c), c.req.param("token") ?? "");
    } catch (err) {
      return this.fail(c, "delete device", err);
    }

    return c.body(null, 204);
  };

  /** Maps the module's errors onto responses. */
  private fail(c: AppContext, operation: string, err: unknown): Response {
    if (is(err, ErrNotFound)) {
      return error(c, 404, "perangkat tidak ditemukan", "not_found");
    }
    if (is(err, ErrInvalidInput)) {
      return error(c, 422, userMessage(err, ErrInvalidInput), "validation_failed");
    }

    this.log.error({ operation, err: errorMessage(err) }, "device request failed");
    return error(c, 500, "permintaan gagal", "internal_error");
  }
}

/**
 * Registers the endpoints every member of the workspace may use.
 *
 * A device belongs to the person holding it rather than to the workspace, so
 * there is no role to check: a member registering their phone is the only thing
 * this surface is for.
 */
export function routes(group: Hono<AppEnv>, h: Handler): void {
  group.post("/devices", h.register);
  group.delete("/devices/:token", h.unregister);
}

/** The tenant the request runs as. */
function requestScope(c: AppContext): Scope {
  return { userId: currentUserId(c) ?? "", workspaceId: workspaceUUID(c) ?? "" };
}

interface DevicePayload {
  device_id: string;
  workspace_id?: string;
  platform: string;
  token: string;
  app_version?: string;
  last_seen_at: string;
  created_at: string;
}

/** Renders a registered device, with the values that may be absent left out. */
function devicePayload(device: Device): DevicePayload {
  const payload: DevicePayload = {
    device_id: device.id,
    platform: device.platform,
    token: device.token,
    last_seen_at: rfc3339(device.lastSeenAt),
    created_at: rfc3339(device.createdAt),
  };
  if (!isNilID(device.workspaceId)) payload.workspace_id = device.workspaceId;
  if (device.appVersion !== "") payload.app_version = device.appVersion;
  return payload;
}

/**
 * Go's `time.RFC3339` for a UTC timestamp: seconds precision and a `Z`, which is
 * what the Go handler emits and what the app parses.
 */
function rfc3339(at: Date): string {
  return at.toISOString().replace(/\.\d{3}Z$/, "Z");
}

/**
 * Reads a JSON body the way Fiber's `BodyParser` does: an empty body and a
 * `null` body both mean "no fields", an unknown field is ignored, and a known
 * field of the wrong type is a parse error.
 */
async function readBody<T>(c: AppContext, shape: Record<string, "string">): Promise<T | null> {
  let raw: string;
  try {
    raw = await c.req.text();
  } catch {
    return null;
  }
  if (raw.trim() === "") {
    return {} as T;
  }

  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return null;
  }
  if (parsed === null) {
    return {} as T;
  }
  if (typeof parsed !== "object" || Array.isArray(parsed)) {
    return null;
  }

  const record = parsed as Record<string, unknown>;
  for (const key of Object.keys(shape)) {
    const value = record[key];
    if (value === undefined || value === null) {
      continue;
    }
    if (typeof value !== "string") {
      return null;
    }
  }
  return record as T;
}

/**
 * Renders a validation error for a person: the detail after the sentinel's own
 * text, because everything before it is this module's name.
 */
function userMessage(err: unknown, sentinel: Error): string {
  const message = errorMessage(err);
  const prefix = `${sentinel.message}: `;
  return message.startsWith(prefix) ? message.slice(prefix.length) : message;
}
