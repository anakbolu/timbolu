import { describe, expect, test } from "bun:test";
import { Hono } from "hono";
import type { AppContext, AppEnv } from "../../../platform/context.ts";
import { nopLogger } from "../../../platform/logger.ts";
import type { Attachment } from "../domain/conversation.ts";
import type { Service } from "../domain/service.ts";
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
