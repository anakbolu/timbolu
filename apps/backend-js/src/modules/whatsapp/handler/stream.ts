/**
 * The QR code reaches the browser over its own socket.
 *
 * Why a socket rather than the workspace's event stream: a QR is a capability.
 * It is whoever scans it that links the number, so it must be visible to the
 * person who started the attempt and to nobody else — and the workspace stream
 * is exactly what every member receives. This endpoint is therefore per attempt,
 * authenticated as the person who started it, and it stops the moment the attempt
 * ends.
 *
 * The polling read stays as the way a screen recovers: a socket that drops
 * because a laptop slept must not mean starting over.
 *
 * Ported 1:1 from internal/modules/whatsapp/handler/stream.go.
 */

import { upgradeWebSocket } from "hono/bun";
import type { Hono } from "hono";
import type { WSContext } from "hono/ws";

import type { AppEnv, AppContext } from "../../../platform/context.ts";
import { currentUserId, scope as requestScope } from "../../../platform/context.ts";
import { error as errorEnvelope } from "../../../platform/response.ts";
import {
  ErrLinkExpired,
  ErrNotFound,
  ErrNotConfigured,
  LinkCode,
  LinkFailed,
  LinkWaiting,
  is,
  type Link,
  type Scope,
} from "../domain/whatsapp.ts";
import type { ServicePort } from "../domain/service.ts";
import type { Handler } from "./handler.ts";
import { wireLink } from "./handler.ts";

/** How often the attempt is read. A QR rotates every twenty seconds or so. */
const linkFrameInterval = 1000;

/** Bounds one attempt's stream. Ten minutes is several times the longest pairing. */
const linkFrameLimit = 10 * 60 * 1000;

/**
 * Registers the per-attempt socket. The container applies the middleware that
 * authenticates from the query string, as it does for the event stream.
 */
export function linkStreamRoutes(group: Hono<AppEnv>, h: Handler): void {
  group.get("/whatsapp/links/:linkID/socket", h.linkSocket);
}

/** Streams one link attempt's state until it ends. */
export async function linkSocket(
  c: AppContext,
  service: ServicePort,
): Promise<Response> {
  const upgrade = (c.req.header("Upgrade") ?? "").toLowerCase();
  if (upgrade !== "websocket") {
    return errorEnvelope(c, 426, "this endpoint speaks WebSocket", "upgrade_required");
  }

  // The scope and the attempt are read before the upgrade: once the connection
  // is hijacked, the request context no longer holds them.
  const linkScope = scopeOf(c);
  const linkID = c.req.param("linkID") ?? "";

  return upgradeWebSocket(c, {
    onOpen: (_event, ws) => {
      void pump(ws, service, linkScope, linkID);
    },
    onMessage: () => {
      // A client that goes away without closing cleanly is noticed by the write
      // failing, which ends the loop.
    },
    onClose: () => {},
    onError: () => {},
  });
}

/** Pumps one attempt's state onto an open socket until it ends. */
async function pump(
  ws: WSContext,
  service: ServicePort,
  linkScope: Scope,
  linkID: string,
): Promise<void> {
  const deadline = Date.now() + linkFrameLimit;
  let last = "";

  while (Date.now() < deadline) {
    let link: Link;
    try {
      link = await service.linkState(linkScope, linkID);
    } catch (err) {
      writeLinkError(ws, err);
      ws.close();
      return;
    }

    // A frame is written when something changed rather than every tick: the QR
    // is the same string until WhatsApp rotates it, and sending it repeatedly
    // would make a screen re-render a code somebody is midway through scanning.
    const encoded = JSON.stringify(wireLink(link));
    if (encoded !== last) {
      try {
        ws.send(encoded);
      } catch {
        return;
      }
      last = encoded;
    }

    if (link.state !== LinkWaiting && link.state !== LinkCode) {
      // The attempt is over, either way. The socket closes rather than waiting
      // for the client to decide it is done.
      ws.close(1000, link.state);
      return;
    }

    await sleep(linkFrameInterval);
  }

  ws.close(1000, "");
}

/** Waits one frame interval without keeping a callback around. */
function sleep(ms: number): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, ms);
  return promise;
}

/**
 * Tells the browser what went wrong in one frame, so a refusal is read rather
 * than inferred from a closed socket.
 */
function writeLinkError(ws: WSContext, err: unknown): void {
  let reason = "penautan ini tidak bisa dibaca lagi";
  if (is(err, ErrNotFound)) {
    reason = "penautan itu milik workspace lain";
  } else if (is(err, ErrLinkExpired)) {
    reason = "penautan ini sudah kedaluwarsa; minta kode baru";
  } else if (is(err, ErrNotConfigured)) {
    reason = "gateway WhatsApp belum dikonfigurasi di deployment ini";
  }

  try {
    ws.send(JSON.stringify({ link_id: "", state: LinkFailed, reason }));
  } catch {
    // The socket is already gone; the close below is all that is left.
  }
}

/** Reads the tenant the request runs in. */
function scopeOf(c: AppContext): Scope {
  const s = requestScope(c);
  return { userId: currentUserId(c) ?? s.userId ?? null, workspaceId: s.workspaceId ?? "" };
}
