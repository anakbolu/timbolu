import { describe, expect, test } from "bun:test";
import { Hono } from "hono";
import type { AppContext, AppEnv } from "../../../platform/context.ts";
import type { Database, Scope as DbScope, Tx } from "../../../platform/database/index.ts";
import { nopLogger } from "../../../platform/logger.ts";
import { ErrAttachmentNotFound, type Attachment } from "../domain/conversation.ts";
import type { ObjectStore, Scope } from "../domain/ports.ts";
import type { Service } from "../domain/service.ts";
import { PostgresRepository } from "../repository/postgres.ts";
import { Service as ChatService } from "../service/service.ts";
import {
  AttachmentMaxBytes,
  AttachmentMaxRequestBytes,
  AttachmentMultipartOverheadBytes,
  Handler,
  routes,
} from "./handler.ts";
describe("Content Origin Policy", () => {
  test("withContent sets frame-ancestors correctly", () => {
    const service = {} as Service;
    const handler = new Handler(service, nopLogger);
    handler.withContent({ frameAncestors: ["https://app.example.com"] });
    
    const policy = handler.contentPolicy();
    expect(policy).toContain("frame-ancestors https://app.example.com");
    expect(policy).not.toContain("frame-ancestors 'self'");
  });

  test("withContent default is 'self'", () => {
    const service = {} as Service;
    const handler = new Handler(service, nopLogger);
    const policy = handler.contentPolicy();
    expect(policy).toContain("frame-ancestors 'self'");
  });
});

const conversationId = "11111111-1111-1111-1111-111111111111";
const workspaceId = "22222222-2222-2222-2222-222222222222";
const userId = "33333333-3333-3333-3333-333333333333";

describe("chat attachment upload Content-Length precheck (#777)", () => {
  test("Content-Length melebihi AttachmentMaxRequestBytes -> 413 dan parseBody() tidak dipanggil", async () => {
    let parseBodyCalls = 0;
    let attachCalls = 0;
    const service = {
      attach: async () => {
        attachCalls++;
        throw new Error("attach should not be called");
      },
    } as unknown as Service;

    const handler = new Handler(service, nopLogger);
    const oversizedLength = AttachmentMaxBytes + AttachmentMultipartOverheadBytes + 1;
    const ctx = {
      req: {
        param: (name: string) => (name === "conversationID" ? conversationId : ""),
        header: (name: string) =>
          name.toLowerCase() === "content-length" ? String(oversizedLength) : undefined,
        parseBody: async () => {
          parseBodyCalls++;
          return {};
        },
      },
      get: (key: string) => (key === "scope" ? { userId, workspaceId } : undefined),
      json: (body: unknown, status?: number) =>
        new Response(JSON.stringify(body), {
          status,
          headers: { "Content-Type": "application/json" },
        }),
    } as unknown as AppContext;

    const res = await handler.attach(ctx);
    expect(res.status).toBe(413);
    const payload = (await res.json()) as {
      success: boolean;
      message: string;
      error?: { code: string };
    };
    expect(payload.success).toBe(false);
    expect(payload.error?.code).toBe("file_too_large");
    expect(payload.message).toBe(`the file is larger than ${AttachmentMaxBytes} bytes`);
    expect(parseBodyCalls).toBe(0);
    expect(attachCalls).toBe(0);
  });

  test("rute HTTP POST /conversations/:conversationID/attachments menolak Content-Length besar dengan 413 tanpa memanggil parseBody()", async () => {
    let parseBodyCalls = 0;
    let streamReadCalls = 0;
    let attachCalls = 0;
    const service = {
      attach: async () => {
        attachCalls++;
        throw new Error("attach should not be called");
      },
    } as unknown as Service;

    const app = new Hono<AppEnv>();
    app.use("*", async (c, next) => {
      c.set("scope", { userId, workspaceId });
      const originalParseBody = c.req.parseBody.bind(c.req);
      c.req.parseBody = (async (...args: Parameters<typeof originalParseBody>) => {
        parseBodyCalls++;
        return originalParseBody(...args);
      }) as typeof c.req.parseBody;
      return next();
    });
    routes(app, new Handler(service, nopLogger));

    const bodyStream = new ReadableStream<Uint8Array>(
      {
        pull(controller) {
          streamReadCalls++;
          controller.enqueue(new Uint8Array([1, 2, 3]));
          controller.close();
        },
      },
      { highWaterMark: 0 },
    );

    const res = await app.request(`/conversations/${conversationId}/attachments`, {
      method: "POST",
      headers: {
        "Content-Type": "multipart/form-data; boundary=----BoluBoundary",
        "Content-Length": String(AttachmentMaxRequestBytes + 100),
      },
      body: bodyStream,
      duplex: "half",
    });

    expect(res.status).toBe(413);
    const payload = (await res.json()) as { success: boolean; error?: { code: string } };
    expect(payload.success).toBe(false);
    expect(payload.error?.code).toBe("file_too_large");
    expect(parseBodyCalls).toBe(0);
    expect(streamReadCalls).toBe(0);
    expect(attachCalls).toBe(0);
  });

  test("pemeriksaan lapis kedua file.size > AttachmentMaxBytes tetap menolak 413 bila Content-Length masih di bawah batas amplop multipart", async () => {
    let parseBodyCalls = 0;
    let attachCalls = 0;
    const service = {
      attach: async () => {
        attachCalls++;
        return {} as Attachment;
      },
    } as unknown as Service;

    const handler = new Handler(service, nopLogger);
    const oversizedFile = new File(["x"], "laporan.pdf", { type: "application/pdf" });
    Object.defineProperty(oversizedFile, "size", { value: AttachmentMaxBytes + 1 });

    const ctx = {
      req: {
        param: (name: string) => (name === "conversationID" ? conversationId : ""),
        header: (name: string) =>
          name.toLowerCase() === "content-length" ? String(AttachmentMaxBytes + 512) : undefined,
        parseBody: async () => {
          parseBodyCalls++;
          return { file: oversizedFile };
        },
      },
      get: (key: string) => (key === "scope" ? { userId, workspaceId } : undefined),
      json: (body: unknown, status?: number) =>
        new Response(JSON.stringify(body), {
          status,
          headers: { "Content-Type": "application/json" },
        }),
    } as unknown as AppContext;

    const res = await handler.attach(ctx);
    expect(res.status).toBe(413);
    const payload = (await res.json()) as { success: boolean; error?: { code: string } };
    expect(payload.success).toBe(false);
    expect(payload.error?.code).toBe("file_too_large");
    expect(parseBodyCalls).toBe(1);
    expect(attachCalls).toBe(0);
  });
});

describe("chat attachment read authorization (#650)", () => {
  const attachmentId = "44444444-4444-4444-4444-444444444444";
  const messageId = "55555555-5555-5555-5555-555555555555";
  const authorUserId = "66666666-6666-6666-6666-666666666666";
  const participantUserId = "77777777-7777-7777-7777-777777777777";
  const outsiderUserId = "88888888-8888-8888-8888-888888888888";

  function buildRepoWithAttachment() {
    const queries: Array<{ sql: string; values: unknown[] }> = [];

    const attachmentRow = {
      id: attachmentId,
      workspace_id: workspaceId,
      message_id: messageId,
      storage_key: `attachments/workspaces/${workspaceId}/objects/${attachmentId}/nota.pdf`,
      filename: "nota.pdf",
      content_type: "application/pdf",
      byte_size: 4,
      checksum_sha256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
      created_at: new Date("2026-01-01T00:00:00Z"),
    };

    const txMock = ((strings: TemplateStringsArray, ...values: unknown[]) => {
      const sql = strings.join("$").replace(/\s+/g, " ").trim();
      queries.push({ sql, values });

      // Evaluate the same SQL predicates the repository sends to Postgres:
      // [id, workspace_id, author_user_id, participant_user_id]
      const [queriedId, queriedWorkspaceId, queriedAuthorId, queriedParticipantId] =
        values as string[];
      const isTargetAttachment =
        queriedId === attachmentRow.id && queriedWorkspaceId === attachmentRow.workspace_id;
      const isAuthor = queriedAuthorId === authorUserId;
      const isParticipant = queriedParticipantId === participantUserId;

      if (isTargetAttachment && (isAuthor || isParticipant)) {
        return Promise.resolve([attachmentRow]);
      }
      return Promise.resolve([]);
    }) as unknown as Tx;

    const fakeDb = {
      inScope: async <T>(_scope: DbScope, fn: (tx: Tx) => Promise<T>): Promise<T> => fn(txMock),
      inScopeRead: async <T>(_scope: DbScope, fn: (tx: Tx) => Promise<T>): Promise<T> =>
        fn(txMock),
    } as unknown as Database;

    return { repo: new PostgresRepository(fakeDb), queries };
  }

  test("open() menolak anggota workspace yang bukan penulis pesan maupun peserta percakapan dengan ErrAttachmentNotFound", async () => {
    const { repo, queries } = buildRepoWithAttachment();

    const outsiderScope: Scope = { userId: outsiderUserId, workspaceId };
    await expect(repo.open(outsiderScope, attachmentId)).rejects.toBe(ErrAttachmentNotFound);

    expect(queries).toHaveLength(1);
    expect(queries[0].sql).toContain("JOIN messages m");
    expect(queries[0].sql).toContain("m.author_user_id =");
    expect(queries[0].sql).toContain("FROM conversation_participants p");
    expect(queries[0].values).toEqual([
      attachmentId,
      workspaceId,
      outsiderUserId,
      outsiderUserId,
    ]);
  });

  test("open() menolak scope tanpa userId dengan ErrAttachmentNotFound tanpa menjalankan query", async () => {
    const { repo, queries } = buildRepoWithAttachment();

    await expect(repo.open({ userId: "", workspaceId }, attachmentId)).rejects.toBe(
      ErrAttachmentNotFound,
    );
    expect(queries).toHaveLength(0);
  });

  test("open() mengizinkan penulis pesan dan peserta percakapan membaca lampiran", async () => {
    const { repo } = buildRepoWithAttachment();

    const byAuthor = await repo.open({ userId: authorUserId, workspaceId }, attachmentId);
    expect(byAuthor.id).toBe(attachmentId);
    expect(byAuthor.filename).toBe("nota.pdf");

    const byParticipant = await repo.open(
      { userId: participantUserId, workspaceId },
      attachmentId,
    );
    expect(byParticipant.id).toBe(attachmentId);
  });

  test("Service.attachment(), attachmentContent(), dan GET /attachments/:id mengembalikan 404 attachment_not_found untuk percakapan milik user lain", async () => {
    const { repo } = buildRepoWithAttachment();
    let storageGetCalls = 0;
    const storage: ObjectStore = {
      put: async () => {
        throw new Error("unexpected put");
      },
      get: async () => {
        storageGetCalls++;
        return new Uint8Array([1, 2, 3, 4]);
      },
      delete: async () => {},
      list: async () => [],
      driver: () => "memory",
    };

    const service = new ChatService({
      conversations: repo,
      messages: repo,
      attachments: repo,
      events: repo,
      publisher: null,
      agents: {
        agent: async () => {
          throw new Error("unexpected agent lookup");
        },
        agents: async () => [],
        active: () => false,
      },
      responder: {
        reply: async () => {
          throw new Error("unexpected reply");
        },
      },
      router: null,
      tasks: null,
      memory: null,
      storage,
      logger: nopLogger,
    });

    const outsiderScope: Scope = { userId: outsiderUserId, workspaceId };
    await expect(service.attachment(outsiderScope, attachmentId)).rejects.toBe(
      ErrAttachmentNotFound,
    );
    await expect(service.attachmentContent(outsiderScope, attachmentId)).rejects.toBe(
      ErrAttachmentNotFound,
    );
    expect(storageGetCalls).toBe(0);

    const app = new Hono<AppEnv>();
    app.use("*", async (c, next) => {
      c.set("scope", outsiderScope);
      return next();
    });
    routes(app, new Handler(service, nopLogger));

    const res = await app.request(`/attachments/${attachmentId}`);
    expect(res.status).toBe(404);
    const body = (await res.json()) as { success: boolean; error?: { code: string } };
    expect(body.success).toBe(false);
    expect(body.error?.code).toBe("attachment_not_found");
    expect(storageGetCalls).toBe(0);
  });
});
