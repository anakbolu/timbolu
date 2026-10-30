/**
 * The device data access: the Expo push addresses one account registered.
 *
 * Every statement runs inside the caller's scope, and the policy on
 * `device_tokens` is keyed on the user rather than the workspace, so a statement
 * that forgot its own user filter still cannot reach another account's address.
 * The workspace is set on the transaction as well because the row records it,
 * and the delete must not need a second query to decide whether a token exists.
 *
 * Ported 1:1 from internal/modules/device/repository/postgres.go and the sqlc
 * queries in repository/queries/device.sql (folded in below, one function per
 * named query with the query name kept as a comment).
 */

import type { Database, Scope as DatabaseScope, Tx } from "../../../platform/database/index.ts";
import {
  ErrInvalidInput,
  ErrNotFound,
  isNilID,
  wrap,
  type Device,
  type RegisterRequest,
  type Repository as RepositoryPort,
  type Scope,
} from "../domain/device.ts";

/** One row of `device_tokens`. */
interface DeviceTokenRow {
  id: string;
  user_id: string;
  workspace_id: string | null;
  platform: string;
  token: string;
  app_version: string | null;
  last_seen_at: Date;
  created_at: Date;
}

/** The Postgres-backed implementation of the device ports. */
export class Repository implements RepositoryPort {
  constructor(private readonly db: Database) {}

  /**
   * Stores a token, or updates the row the same account already has for it.
   *
   * The upsert is the whole of "registering twice does not add a row": a phone
   * re-registers on every launch, so the second registration updates the row it
   * already has and `created_at` stays the moment it first appeared.
   * `workspace_id` is deliberately absent from the update list — the address
   * belongs to the account, and switching workspaces must not rewrite where it
   * was first registered.
   */
  async register(scope: Scope, request: RegisterRequest): Promise<Device> {
    const userId = scope.userId;
    if (!userId) {
      throw wrap(ErrInvalidInput, "user is required");
    }

    return this.write(scope, async (tx) => {
      // name: RegisterDeviceToken
      const rows = await tx<DeviceTokenRow[]>`
        INSERT INTO device_tokens (user_id, workspace_id, platform, token, app_version, last_seen_at)
        VALUES (
            ${userId},
            ${optionalID(scope.workspaceId)}::uuid,
            ${request.platform},
            ${request.token},
            ${optionalString(request.appVersion)},
            now()
        )
        ON CONFLICT (user_id, token) DO UPDATE
        SET platform = EXCLUDED.platform,
            app_version = EXCLUDED.app_version,
            last_seen_at = now()
        RETURNING id, user_id, workspace_id, platform, token, app_version, last_seen_at, created_at
      `;

      const row = rows[0];
      if (!row) {
        throw wrap(ErrInvalidInput, "register device");
      }
      return toDevice(row);
    });
  }

  /**
   * Removes one of the caller's tokens.
   *
   * A statement that matched nothing is reported as ErrNotFound. That is the
   * whole of "another account's token is invisible": the policy already hid the
   * row, and this turns the empty result into the answer the handler gives.
   */
  async remove(scope: Scope, token: string): Promise<void> {
    const userId = scope.userId;
    if (!userId) {
      throw wrap(ErrInvalidInput, "user is required");
    }

    await this.write(scope, async (tx) => {
      // name: DeleteDeviceToken
      const result = await tx`
        DELETE FROM device_tokens
        WHERE user_id = ${userId} AND token = ${token}
      `;
      if (result.count === 0) {
        throw ErrNotFound;
      }
    });
  }

  /**
   * Every address the caller registered.
   *
   * Ordered by `last_seen_at` because the newest address is the one a phone is
   * most likely still running: the order is what lets a sender stop early when
   * it has to. There is no pagination — an account has one row per phone, and a
   * list that needed pages would mean a registry that has already gone wrong.
   */
  async listTokens(scope: Scope): Promise<Device[]> {
    const userId = scope.userId;
    if (!userId) {
      throw wrap(ErrInvalidInput, "user is required");
    }

    return this.read(scope, async (tx) => {
      // name: ListDeviceTokensByUser
      const rows = await tx<DeviceTokenRow[]>`
        SELECT id, user_id, workspace_id, platform, token, app_version, last_seen_at, created_at
        FROM device_tokens
        WHERE user_id = ${userId}
        ORDER BY last_seen_at DESC
      `;
      return rows.map(toDevice);
    });
  }

  // -------------------------------------------------------------- helpers

  private async read<T>(scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    return this.db.inScopeRead(databaseScope(scope), fn);
  }

  private async write<T>(scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    return this.db.inScope(databaseScope(scope), fn);
  }
}

function databaseScope(scope: Scope): DatabaseScope {
  return { userId: scope.userId ?? null, workspaceId: scope.workspaceId };
}

/**
 * Translates a stored row, where an absent workspace and an absent app version
 * are null.
 */
function toDevice(row: DeviceTokenRow): Device {
  return {
    id: row.id,
    userId: row.user_id,
    workspaceId: row.workspace_id ?? "",
    platform: row.platform,
    token: row.token,
    appVersion: row.app_version ?? "",
    lastSeenAt: row.last_seen_at,
    createdAt: row.created_at,
  };
}

/**
 * Writes an id that may be absent. A zero uuid in the database would be a
 * reference to a row nobody has, so absent is null.
 */
function optionalID(id: string | null | undefined): string | null {
  return isNilID(id) ? null : (id as string);
}

/**
 * Writes a value that may be absent. An empty app version is "the phone did not
 * say" rather than a version called "".
 */
function optionalString(value: string | null | undefined): string | null {
  return value ? value : null;
}
