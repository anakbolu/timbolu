/**
 * The draft endpoints' id rules at the wire: an id the caller sent that names
 * no draft answers 404, and only an id that is not a uuid answers 400 (#480).
 *
 * The service is the real one, because the rule being pinned is its own; the
 * repository is a fake, because whether Postgres stores the row is not what
 * these cases ask. A zero uuid is the case that regressed: it was read as
 * "the request has no id" and answered 400 before the store ever saw it.
 */

import { describe, expect, test } from "bun:test";
import { Hono } from "hono";

import type { AppEnv } from "../../../platform/context.ts";
import { nopLogger } from "../../../platform/logger.ts";
import {
  ErrDraftNotFound,
  NilUUID,
  StatusPending,
  type Decision,
  type Draft,
  type Repository,
  type Scope,
} from "../domain/approval.ts";
import { Service } from "../service/service.ts";
import { Handler, routes } from "./handler.ts";

const workspaceId = "11111111-1111-1111-1111-111111111111";
const userId = "22222222-2222-2222-2222-222222222222";
const draftId = "33333333-3333-3333-3333-333333333333";

const stored: Draft = {
  id: draftId,
  workspaceId,
  taskId: "44444444-4444-4444-4444-444444444444",
  agentId: "55555555-5555-5555-5555-555555555555",
  conversationId: NilUUID,
  actionKind: "whatsapp.send",
  title: "Kirim penawaran",
  summary: "",
  payload: {},
  status: StatusPending,
  revision: 0,
  decidedBy: "",
  decidedAt: null,
  decisionNote: "",
  notBefore: null,
  blockedReason: "",
  sentAt: null,
  cardMessageId: NilUUID,
  idempotencyKey: "",
  createdAt: new Date(0),
  updatedAt: new Date(0),
};

function notUsed(method: string): never {
  throw new Error(`${method} is not part of the draft path under test`);
}

/** Answers only the rows `stored` stands for; every other id is not found. */
class FakeDrafts implements Repository {
  readonly asked: string[] = [];

  async getDraft(_scope: Scope, id: string): Promise<Draft> {
    this.asked.push(id);
    if (id !== draftId) throw ErrDraftNotFound;
    return stored;
  }

  async decide(_scope: Scope, decision: Decision): Promise<Draft> {
    if (decision.draftId !== draftId) throw ErrDraftNotFound;
    return { ...stored, status: decision.status };
  }

  createDraft(): never { return notUsed("createDraft"); }
  listDrafts(): never { return notUsed("listDrafts"); }
  markSent(): never { return notUsed("markSent"); }
  hold(): never { return notUsed("hold"); }
  setCardMessage(): never { return notUsed("setCardMessage"); }
  appendRevision(): never { return notUsed("appendRevision"); }
  revisions(): never { return notUsed("revisions"); }
  policies(): never { return notUsed("policies"); }
  policy(): never { return notUsed("policy"); }
  setPolicy(): never { return notUsed("setPolicy"); }
  actionKinds(): never { return notUsed("actionKinds"); }
  recordAudit(): never { return notUsed("recordAudit"); }
  auditTrail(): never { return notUsed("auditTrail"); }
}

/** Mounts the routes behind the identity the container's guards would have set. */
function newApp(drafts: Repository): Hono<AppEnv> {
  const app = new Hono<AppEnv>();
  app.use("*", async (c, next) => {
    c.set("userId", userId);
    c.set("scope", { userId, workspaceId });
    await next();
  });
  routes(app, new Handler(new Service({ drafts }), nopLogger));
  return app;
}

interface Envelope {
  data?: { draft_id?: string };
  error?: { code: string };
}

async function decode(res: Response): Promise<Envelope> {
  return (await res.json()) as Envelope;
}

describe("GET /drafts/:draftID", () => {
  test("a zero uuid names no draft, so it answers 404 draft_not_found", async () => {
    const drafts = new FakeDrafts();

    const res = await newApp(drafts).request("/drafts/" + NilUUID);

    expect(res.status).toBe(404);
    expect((await decode(res)).error?.code).toBe("draft_not_found");
    // The id reached the store: the 404 is its answer, not a rejected request.
    expect(drafts.asked).toEqual([NilUUID]);
  });

  test("a value that is not a uuid is still a bad request", async () => {
    const drafts = new FakeDrafts();

    const res = await newApp(drafts).request("/drafts/xyz");

    expect(res.status).toBe(400);
    expect((await decode(res)).error?.code).toBe("invalid_input");
    expect(drafts.asked).toEqual([]);
  });

  test("a draft that exists is returned", async () => {
    const res = await newApp(new FakeDrafts()).request("/drafts/" + draftId);

    expect(res.status).toBe(200);
    expect((await decode(res)).data?.draft_id).toBe(draftId);
  });
});

describe("the rest of a zero uuid's draft paths", () => {
  test("its revision trail answers 404 rather than an empty list", async () => {
    const res = await newApp(new FakeDrafts()).request("/drafts/" + NilUUID + "/revisions");

    expect(res.status).toBe(404);
    expect((await decode(res)).error?.code).toBe("draft_not_found");
  });

  test("deciding it answers 404, not a bad request", async () => {
    const res = await newApp(new FakeDrafts()).request("/drafts/" + NilUUID + "/approve", {
      method: "POST",
    });

    expect(res.status).toBe(404);
    expect((await decode(res)).error?.code).toBe("draft_not_found");
  });
});