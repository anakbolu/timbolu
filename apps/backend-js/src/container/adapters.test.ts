import { describe, expect, test } from "bun:test";
import { authStateSigner, chatTaskStarter } from "./adapters.ts";
import { Codec } from "../platform/oauthstate/state.ts";

interface DispatchedTask {
  agentId: string;
  conversationId: string;
  replyMessageId: string;
  trigger: string;
  title: string;
  prompt: string;
  parentTaskId: string;
  depth: number;
}

function fakeTaskService() {
  const dispatched: DispatchedTask[] = [];
  return {
    dispatched,
    dispatch: async (
      _ctx: undefined,
      _scope: { userId?: string; workspaceId?: string },
      req: DispatchedTask,
    ) => {
      dispatched.push(req);
      return { id: "fake-task-id" };
    },
  };
}

describe("chatTaskStarter", () => {
  test("teks 400 karakter memotong judul <= 160 rune dengan akhiran ellipsis dan prompt utuh", async () => {
    const fake = fakeTaskService();
    const starter = chatTaskStarter(fake);

    // 400 karakter tanpa baris baru
    const fullText = "a".repeat(400);
    const scope = { userId: "user-1", workspaceId: "workspace-1" };
    const taskId = await starter.start(scope, {
      agentId: "agent-1",
      conversationId: "conv-1",
      messageId: "msg-1",
      trigger: "chat",
      title: fullText,
    });

    expect(taskId).toBe("fake-task-id");
    expect(fake.dispatched.length).toBe(1);
    const recorded = fake.dispatched[0];
    expect(Array.from(recorded.title).length).toBeLessThanOrEqual(160);
    expect(Array.from(recorded.title).length).toBe(160);
    expect(recorded.title.endsWith("…")).toBe(true);
    expect(recorded.prompt).toBe(fullText);
  });

  test("teks pendek menghasilkan judul sama persis tanpa akhiran ellipsis", async () => {
    const fake = fakeTaskService();
    const starter = chatTaskStarter(fake);

    const shortText = "halo";
    const scope = { userId: "user-1", workspaceId: "workspace-1" };
    await starter.start(scope, {
      agentId: "agent-1",
      conversationId: "conv-1",
      messageId: "msg-1",
      trigger: "chat",
      title: shortText,
    });

    expect(fake.dispatched.length).toBe(1);
    const recorded = fake.dispatched[0];
    expect(recorded.title).toBe("halo");
    expect(recorded.title.endsWith("…")).toBe(false);
    expect(recorded.prompt).toBe(shortText);
  });

  test("teks multi-baris mengambil judul dari baris pertama", async () => {
    const fake = fakeTaskService();
    const starter = chatTaskStarter(fake);

    const multiLineText = "Baris pertama yang menjadi judul\nBaris kedua detail instruksi\nBaris ketiga tambahan";
    const scope = { userId: "user-1", workspaceId: "workspace-1" };
    await starter.start(scope, {
      agentId: "agent-1",
      conversationId: "conv-1",
      messageId: "msg-1",
      trigger: "chat",
      title: multiLineText,
    });

    expect(fake.dispatched.length).toBe(1);
    const recorded = fake.dispatched[0];
    expect(recorded.title).toBe("Baris pertama yang menjadi judul");
    expect(recorded.prompt).toBe(multiLineText);
  });
});

describe("authStateSigner", () => {
  const secretKey = "a-very-long-signing-key-that-exceeds-32-chars";
  const codec = Codec.create(secretKey);

  test("sign() and verify() succeed with valid state", async () => {
    const signer = authStateSigner(codec, 10);
    expect(signer).not.toBeNull();
    const state = await signer!.sign();
    expect(typeof state).toBe("string");
    await expect(signer!.verify(state)).resolves.toBeUndefined();
  });

  test("verify() throws when signature is tampered", async () => {
    const signer = authStateSigner(codec, 10);
    expect(signer).not.toBeNull();
    const state = await signer!.sign();
    const cut = state.indexOf(".");
    const broken = state.slice(0, cut + 1) + (state[cut + 1] === "a" ? "b" : "a") + state.slice(cut + 2);
    await expect(signer!.verify(broken)).rejects.toThrow();
  });
});
