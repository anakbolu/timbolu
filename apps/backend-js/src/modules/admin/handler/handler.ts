/**
 * The operator surface over HTTP: the accounts a deployment has, and the one
 * decision an admin makes about them.
 *
 * Ported from internal/modules/admin/handler/handler.go.
 *
 * The container guards the group with the platform-admin middleware, so every
 * request that reaches it is already known to be an admin. The handler still
 * takes the caller's id from the session rather than from the body — a request
 * that names its own actor could promote itself — and the service re-reads that
 * account's role, so the surface has two locks rather than one.
 */

import type { Hono } from "hono";
import type { AppContext, AppEnv } from "../../../platform/context.ts";
import { currentUserId, isUuid } from "../../../platform/context.ts";
import type { Logger } from "../../../platform/logger.ts";
import { error, ok, okWithMeta, unavailable } from "../../../platform/response.ts";
import type { Account, Filter, Service as ServicePort } from "../domain/admin.ts";
import { AdminError, ErrInvalidInput, ZeroUUID, accountIsAdmin, normaliseFilter } from "../domain/admin.ts";

/** Serves the operator surface. */
export class Handler {
  private readonly service: ServicePort;
  private readonly log: Logger;

  constructor(service: ServicePort, log: Logger) {
    this.service = service;
    this.log = log;
  }

  /** Answers what the deployment's accounts add up to. */
  overview = async (c: AppContext): Promise<Response> => {
    try {
      const stats = await this.service.overview();
      return ok(c, "ok", {
        total: stats.total,
        admins: stats.admins,
        users: stats.users,
        verified: stats.verified,
        onboarded: stats.onboarded,
        google_accounts: stats.googleAccounts,
        new_last_7_days: stats.newLast7Days,
      });
    } catch (err) {
      return this.fail(c, "overview", err);
    }
  };

  /** Answers a page of accounts. */
  users = async (c: AppContext): Promise<Response> => {
    const filter: Filter = {
      search: c.req.query("q") ?? "",
      role: c.req.query("role") ?? "",
      limit: queryInt(c, "limit", 0),
      offset: queryInt(c, "offset", 0),
    };

    try {
      const [accounts, total] = await this.service.list(filter);
      const payload = accounts.map(toAccountPayload);

      // The filter comes back with the page, so a screen that shows "3 of 41"
      // does not have to remember what it asked for.
      const normalised = normaliseFilter(filter);
      return okWithMeta(c, "ok", payload, {
        count: payload.length,
        total,
        limit: normalised.limit,
        offset: normalised.offset,
        role: normalised.role,
        search: normalised.search,
      });
    } catch (err) {
      return this.fail(c, "list users", err);
    }
  };

  /** Promotes or demotes an account. */
  changeRole = async (c: AppContext): Promise<Response> => {
    const targetID = c.req.param("userID");
    if (!isUuid(targetID)) {
      return error(c, 400, "user id is not a uuid", "invalid_input");
    }

    const parsed: unknown = await c.req.json().catch(() => null);
    const role = typeof parsed === "object" && parsed !== null && "role" in parsed ? parsed.role : undefined;
    if (typeof role !== "string") {
      return error(c, 400, "invalid request body", "invalid_request");
    }

    try {
      const account = await this.service.changeRole({
        actorID: currentUserId(c) ?? ZeroUUID,
        targetID,
        role,
      });

      const message = accountIsAdmin(account) ? "peran diubah menjadi admin" : "peran diubah menjadi user";
      return ok(c, message, toAccountPayload(account));
    } catch (err) {
      return this.fail(c, "change role", err);
    }
  };

  /** Maps the module's errors onto responses. */
  private fail(c: AppContext, operation: string, err: unknown): Response {
    if (err instanceof AdminError) {
      switch (err.kind) {
        case "not_admin":
          return error(c, 403, "hanya admin platform yang boleh membuka halaman ini", "forbidden");
        case "account_not_found":
          return error(c, 404, "akun tidak ditemukan", "user_not_found");
        case "last_admin":
          return error(c, 409, "admin terakhir tidak bisa diturunkan", "last_admin");
        case "self_demotion":
          return error(c, 409, "tidak bisa menurunkan peran akun sendiri", "self_demotion");
        case "not_configured":
          return unavailable(c, "halaman admin tidak dikonfigurasi di deployment ini", "admin_not_configured");
        case "invalid_input":
          return error(c, 400, err.message, "invalid_input");
      }
    }

    this.log.error({ operation, err }, "admin request failed");
    return error(c, 500, "internal server error", "internal_error");
  }
}

/** One account as the operator screen reads it. */
interface AccountPayload {
  id: string;
  email: string;
  role: string;
  email_verified: boolean;
  onboarded: boolean;
  has_google: boolean;
  has_password: boolean;
  workspaces: number;
  created_at: string;
}

function toAccountPayload(account: Account): AccountPayload {
  return {
    id: account.id,
    email: account.email,
    role: account.role,
    email_verified: account.emailVerified,
    onboarded: account.onboarded,
    has_google: account.hasGoogle,
    has_password: account.hasPassword,
    workspaces: account.workspaces,
    // Go: `CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")`, and an empty
    // string when the time is zero.
    created_at: Number.isNaN(account.createdAt.getTime())
      ? ""
      : account.createdAt.toISOString().replace(/\.\d{3}Z$/, "Z"),
  };
}

/** Fiber's `c.QueryInt(key, default)`: an unparseable value answers the default. */
function queryInt(c: AppContext, key: string, fallback: number): number {
  const raw = c.req.query(key);
  if (raw === undefined || raw === "") return fallback;
  const value = Number.parseInt(raw, 10);
  return Number.isNaN(value) ? fallback : value;
}

/** Registers the endpoints. Mirrors Go `Routes(group, h)`. */
export function routes(group: Hono<AppEnv>, h: Handler): void {
  group.get("/admin/overview", h.overview);
  group.get("/admin/users", h.users);
  group.post("/admin/users/:userID/role", h.changeRole);
}
