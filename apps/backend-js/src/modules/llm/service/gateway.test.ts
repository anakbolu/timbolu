import { describe, expect, test } from "bun:test";

import type { AgentStore, ProviderConfig, Store } from "../domain/gateway.ts";
import type {
  ChatRequest,
  Provider,
  StreamEvent,
  Usage,
  UsageEntry,
  UsageRecorder,
} from "../domain/provider.ts";
import { Service } from "./service.ts";

const scope = { userId: "user-1", workspaceId: "workspace-1", agentId: "" };

const request: ChatRequest = {
  model: "",
  system: "",
  retrieved: "",
  messages: [{ role: "user", text: "hello", images: [], toolCalls: [], toolResults: [] }],
  tools: [],
  toolChoice: "auto",
  toolName: "",
  maxTokens: 0,
  temperature: null,
  stopSequences: [],
  metadata: { workspaceId: scope.workspaceId, agentId: "", taskId: "task-1", purpose: "agent" },
};

const config: ProviderConfig = {
  id: "provider-1",
  workspaceId: scope.workspaceId,
  name: "Test provider",
  adapter: "openai",
  baseUrl: "https://example.test/v1",
  model: "test-model",
  apiKey: "",
  priority: 0,
  maxTokens: 0,
  isDefault: true,
  contextTokens: 0,
  enabled: true,
  createdAt: new Date(0),
  updatedAt: new Date(0),
};

const usage: Usage = {
  inputTokens: 12,
  outputTokens: 7,
  cacheReadTokens: 2,
  cacheWriteTokens: 1,
};

function event(type: StreamEvent["type"], values: Partial<StreamEvent> = {}): StreamEvent {
  return {
    type,
    text: "",
    toolCall: null,
    usage: null,
    finishReason: "unknown",
    err: null,
    ...values,
  };
}

function fixture(events: StreamEvent[], failRecord?: Error) {
  const entries: UsageEntry[] = [];
  const order: string[] = [];
  let upstreamClosed = false;
  const recorder: UsageRecorder = {
    async record(_ctx, entry) {
      order.push("record");
      if (failRecord) throw failRecord;
      entries.push(entry);
    },
  };
  const store: Store = {
    async list() {
      return [];
    },
    async get() {
      throw new Error("get is not used by this test");
    },
    async create(_ctx, stored) {
      return stored;
    },
    async update(_ctx, stored) {
      return stored;
    },
    async delete() {},
  };
  const agents: AgentStore = {
    async agentModel() {
      return {};
    },
    async setAgentModel() {},
  };
  const provider: Provider = {
    name: () => config.name,
    model: () => config.model,
    capabilities: () => ({
      tools: true,
      vision: true,
      streaming: true,
      promptCaching: true,
      parallelToolCalls: true,
      embeddings: true,
      maxContextTokens: 100_000,
    }),
    async chat() {
      throw new Error("chat is not used by this test");
    },
    async stream() {
      return (async function* () {
        try {
          yield* events;
        } finally {
          upstreamClosed = true;
        }
      })();
    },
    async embed() {
      throw new Error("embed is not used by this test");
    },
  };
  const service = new Service({
    store,
    agents,
    factory: { build: () => provider },
    box: null,
    recorder,
    reader: null,
    counter: null,
    quota: null,
    costs: null,
    default: config,
    embeddingModel: "",
    auditor: null,
    logger: null,
    tracer: null,
    metrics: null,
    settings: { quotaEnabled: false, chainLimit: 0 },
  });

  return { service, entries, order, upstreamClosed: () => upstreamClosed };
}

describe("LLM gateway stream", () => {
  test("forwards provider events in order and records usage before done", async () => {
    const providerEvents = [
      event("text", { text: "Hello " }),
      event("text", { text: "world" }),
      event("tool_call", {
        toolCall: { id: "call-1", name: "render_chart", arguments: { title: "Sales" } },
      }),
      event("usage", { usage }),
      event("done", { finishReason: "stop" }),
    ];
    const f = fixture(providerEvents);
    const stream = await f.service.stream(undefined, scope, request);
    const received: StreamEvent[] = [];

    for await (const item of stream) {
      received.push(item);
      f.order.push(item.type);
      if (item.type === "done") expect(f.entries).toHaveLength(1);
    }

    expect(received).toEqual(providerEvents);
    expect(received.map((item) => item.text).join("")).toBe("Hello world");
    expect(f.entries).toHaveLength(1);
    expect(f.entries[0].usage).toEqual(usage);
    expect(f.order).toEqual(["text", "text", "tool_call", "usage", "record", "done"]);
    expect(f.upstreamClosed()).toBe(true);
  });

  test("emits an error instead of done when recording usage fails", async () => {
    const f = fixture(
      [event("text", { text: "answer" }), event("usage", { usage }), event("done")],
      new Error("ledger unavailable"),
    );
    const stream = await f.service.stream(undefined, scope, request);
    const received: StreamEvent[] = [];

    for await (const item of stream) {
      received.push(item);
      f.order.push(item.type);
    }

    expect(received.map((item) => item.type)).toEqual(["text", "usage", "error"]);
    expect(received.at(-1)?.err).toBeInstanceOf(Error);
    expect(f.order).toEqual(["text", "usage", "record", "error"]);
    expect(f.entries).toHaveLength(0);
    expect(f.upstreamClosed()).toBe(true);
  });

  test("records once and reports an error when the provider closes without a terminal event", async () => {
    const f = fixture([event("text", { text: "partial" }), event("usage", { usage })]);
    const stream = await f.service.stream(undefined, scope, request);
    const received: StreamEvent[] = [];

    for await (const item of stream) {
      received.push(item);
      f.order.push(item.type);
    }

    expect(received.map((item) => item.type)).toEqual(["text", "usage", "error"]);
    expect(received.at(-1)?.err).toBeInstanceOf(Error);
    expect(f.entries).toHaveLength(1);
    expect(f.entries[0].usage).toEqual(usage);
    expect(f.order).toEqual(["text", "usage", "record", "error"]);
    expect(f.upstreamClosed()).toBe(true);
  });

  test("forwards provider errors after recording usage exactly once", async () => {
    const providerError = new Error("provider failed");
    const f = fixture([event("usage", { usage }), event("error", { err: providerError })]);
    const stream = await f.service.stream(undefined, scope, request);
    const received: StreamEvent[] = [];

    for await (const item of stream) {
      received.push(item);
      f.order.push(item.type);
    }

    expect(received.map((item) => item.type)).toEqual(["usage", "error"]);
    expect(received.at(-1)?.err).toBe(providerError);
    expect(f.entries).toHaveLength(1);
    expect(f.order).toEqual(["usage", "record", "error"]);
    expect(f.upstreamClosed()).toBe(true);
  });

  test("closes the provider iterator without yielding after the caller aborts", async () => {
    const f = fixture([event("text", { text: "not delivered" }), event("done")]);
    const controller = new AbortController();
    const stream = await f.service.stream(controller.signal, scope, request);
    controller.abort();
    const received: StreamEvent[] = [];

    for await (const item of stream) received.push(item);

    expect(received).toEqual([]);
    expect(f.entries).toHaveLength(0);
    expect(f.upstreamClosed()).toBe(true);
  });

  test("closes the provider iterator when the consumer stops after a partial answer", async () => {
    const f = fixture([
      event("text", { text: "first" }),
      event("text", { text: "second" }),
      event("done"),
    ]);
    const stream = await f.service.stream(undefined, scope, request);
    const received: StreamEvent[] = [];

    for await (const item of stream) {
      received.push(item);
      break;
    }

    expect(received.map((item) => item.text)).toEqual(["first"]);
    expect(f.entries).toHaveLength(0);
    expect(f.upstreamClosed()).toBe(true);
  });
});
