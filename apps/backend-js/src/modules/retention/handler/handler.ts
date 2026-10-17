/**
 * The erasure endpoints: a workspace owner asking for their data to be
 * removed, and reading back the record of what was removed.
 *
 * Two endpoints and no more, and the shape is deliberate. There is no "schedule
 * an erasure" and no "cancel" — the right it implements is a *request* whose
 * answer is either "done" or "here is why not", and a queue in between would be
 * a deployment keeping data after being asked not to. There is also no way to
 * erase somebody else's workspace: the route reads the active workspace from
 * the request, so the tenant middleware is the authority rather than a
 * parameter a caller could edit.
 *
 * Ported 1:1 from internal/modules/retention/handler/handler.go.
 */

import type { Hono } from "hono";
import {
  currentUserId,
  isUuid,
  workspaceUUID,
  type AppContext,
  type AppEnv,
} from "../../../platform/context.ts";
import type { Logger } from "../../../platform/logger.ts";
import { error, ok } from "../../../platform/response.ts";
import {
  ErrInvalidInput,
  ErrNoRepository,
  NilUUID,
  is,
  type Erasure,
  type ErasureService,
} from "../domain/retention.ts";
import { ErrErasureNotFound } from "../domain/erasure.ts";

/** Serves the erasure endpoints. */
export class Handler {
  constructor(
    private readonly service: ErasureService,
    private readonly log: Logger,
  ) {}

  /**
   * Removes the active workspace's data and answers with the record.
   *
   * The response is the evidence, and it is returned in the same call that does
   * the work rather than being fetched afterwards: the workspace no longer
   * exists when this returns, so there is nothing left to address a later read
   * to.
   */
  erase = async (c: AppContext): Promise<Response> => {
    const workspaceId = workspaceUUID(c);
    if (!isUuid(workspaceId)) {
      return error(c, 400, "workspace id is not a uuid", "invalid_input");
    }

    let result;
    try {
      result = await this.service.erase({
        workspaceId,
        requestedBy: currentUserId(c) ?? NilUUID,
      });
    } catch (err) {
      return this.fail(c, "erase workspace", err);
    }

    return ok(c, "data workspace dihapus", {
      workspace_id: workspaceId,
      objects: result.objects,
      rows: result.rows,
    });
  };

  /**
   * Returns the workspace's erasure record, when there is one.
   *
   * It exists for the case the POST cannot cover: a deployment where the
   * request was made and the caller kept a copy of the id, and a person who has
   * to answer "was this erased". A workspace whose data is gone answers
   * `erasure_not_found` here — the active workspace no longer resolves, so the
   * tenant middleware stops the request before this runs, which is the honest
   * outcome rather than a 404 from the handler.
   */
  show = async (c: AppContext): Promise<Response> => {
    const workspaceId = workspaceUUID(c);
    if (!isUuid(workspaceId)) {
      return error(c, 400, "workspace id is not a uuid", "invalid_input");
    }

    let erasure: Erasure;
    try {
      erasure = await this.service.findErasure(workspaceId);
    } catch (err) {
      return this.fail(c, "read erasure", err);
    }

    return ok(c, "ok", erasurePayload(erasure));
  };

  /** Maps a service error onto a response. */
  private fail(c: AppContext, operation: string, err: unknown): Response {
    if (is(err, ErrErasureNotFound)) {
      return error(
        c,
        404,
        "belum ada permintaan hapus data yang selesai untuk workspace ini",
        "erasure_not_found",
      );
    }
    if (is(err, ErrNoRepository) || is(err, ErrInvalidInput)) {
      return error(
        c,
        503,
        "penghapusan data tidak bisa dijalankan di deployment ini: jalur hapus belum lengkap",
        "erasure_unavailable",
      );
    }

    this.log.error({ operation, err }, "erasure failed");

    return error(
      c,
      500,
      "penghapusan data gagal dan tidak ada yang setengah dihapus: coba lagi atau hubungi kami",
      "erasure_failed",
    );
  }
}

/** The record as a person reads it. */
function erasurePayload(erasure: Erasure): Record<string, unknown> {
  return {
    workspace_id: erasure.workspaceId,
    workspace_name: erasure.workspaceName,
    requested_by: erasure.requestedBy,
    requested_by_email: erasure.requestedByMail,
    requested_at: erasure.requestedAt,
    completed_at: erasure.completedAt,
    objects_deleted: erasure.objects,
    rows_deleted: erasure.rows,
    complete: erasure.completedAt !== null,
  };
}

/**
 * Registers the endpoints a workspace owner may call, mirroring Go
 * `OwnerRoutes`. The container applies the tenant middleware and an owner check
 * to this group; the person is taken from the session rather than from the
 * body, because who asked is part of the record.
 */
export function ownerRoutes(group: Hono<AppEnv>, h: Handler): void {
  group.post("/workspaces/current/erasure", h.erase);
  group.get("/workspaces/current/erasure", h.show);
}
