import { describe, expect, test } from "bun:test";
import type { AgentOverride, AgentStore, Store } from "../domain/gateway.ts";
import type { Provider } from "../domain/provider.ts";
import { checkBaseUrl, isPrivateOrReservedIP } from "./baseurl.ts";
import { Service } from "./service.ts";

describe("baseurl validation", () => {
  const dummyLookup = async (_h: string) => [{ address: "93.184.216.34", family: 4 }];

  test("empty or whitespace is allowed (uses default deployment)", async () => {
    expect((await checkBaseUrl("")).ok).toBe(true);
    expect((await checkBaseUrl("   ")).ok).toBe(true);
  });

  test("valid public https endpoint is accepted and trimmed", async () => {
    const res = await checkBaseUrl("  https://api.openai.com/v1  ", { lookup: dummyLookup });
    expect(res).toEqual({ ok: true, value: "https://api.openai.com/v1" });
  });

  test("rejects cloud metadata, localhost, and private IPv4", async () => {
    const blocked = [
      "http://169.254.169.254/",
      "http://localhost:8080",
      "http://127.0.0.1",
      "http://10.1.2.3",
      "http://192.168.1.1",
    ];
    for (const url of blocked) {
      expect((await checkBaseUrl(url, { allowHttp: true })).ok).toBe(false);
    }
  });

  test("rejects IPv6 loopback, link-local, ULA, and IPv4-mapped", async () => {
    const blocked = [
      "http://[::1]/",
      "http://[::]/",
      "http://[fe80::1]/",
      "http://[fc00::1]/",
      "http://[fd12:3456:789a::1]/",
      "http://[::ffff:127.0.0.1]/",
    ];
    for (const url of blocked) {
      expect((await checkBaseUrl(url, { allowHttp: true })).ok).toBe(false);
    }
  });

  test("rejects dotless hostnames and reserved internal domains", async () => {
    for (const url of ["https://internal-svc/", "https://foo.internal/", "https://foo.local/"]) {
      expect((await checkBaseUrl(url)).ok).toBe(false);
    }
  });

  test("rejects hostname resolving to private IP via fake lookup", async () => {
    const fakeLookup = async () => [{ address: "127.0.0.1", family: 4 }];
    const res = await checkBaseUrl("https://safe-looking.com/v1", { lookup: fakeLookup });
    expect(res.ok).toBe(false);
  });

  test("rejects unsupported protocols and http in production", async () => {
    expect((await checkBaseUrl("ftp://example.com")).ok).toBe(false);
    expect((await checkBaseUrl("http://example.com", { allowHttp: false })).ok).toBe(false);
    expect((await checkBaseUrl("http://example.com", { allowHttp: true, lookup: dummyLookup })).ok).toBe(true);
  });

  test("isPrivateOrReservedIP unit coverage", () => {
    expect(isPrivateOrReservedIP("127.0.0.1")).toBe(true);
    expect(isPrivateOrReservedIP("10.0.0.1")).toBe(true);
    expect(isPrivateOrReservedIP("169.254.169.254")).toBe(true);
    expect(isPrivateOrReservedIP("::1")).toBe(true);
    expect(isPrivateOrReservedIP("fe80::1")).toBe(true);
    expect(isPrivateOrReservedIP("fc00::1")).toBe(true);
    expect(isPrivateOrReservedIP("8.8.8.8")).toBe(false);
    expect(isPrivateOrReservedIP("2001:4860:4860::8888")).toBe(false);
  });
});

describe("Service base_url SSRF validation integration", () => {
  function createService() {
    const store: Store = {
      list: async () => [],
      get: async () => { throw new Error(); },
      create: async (_c, s) => s,
      update: async (_c, s) => s,
      delete: async () => {},
    };
    let lastOverride: AgentOverride | undefined;
    const agents: AgentStore = {
      agentModel: async () => lastOverride ?? {},
      setAgentModel: async (_c, _w, _a, o) => { lastOverride = o; },
    };
    const provider: Provider = {
      name: () => "mock",
      model: () => "m",
      capabilities: () => ({
        tools: true, vision: true, streaming: true, promptCaching: true,
        parallelToolCalls: true, embeddings: true, maxContextTokens: 100,
      }),
      chat: async () => { throw new Error(); },
      stream: async () => { throw new Error(); },
      embed: async () => { throw new Error(); },
    };
    const service = new Service({
      store,
      agents,
      factory: { build: () => provider },
      box: null, recorder: null, reader: null, counter: null, quota: null, costs: null,
      default: {
        id: "d", workspaceId: "ws", name: "d", adapter: "openai", baseUrl: "",
        model: "m", apiKey: "", priority: 0, maxTokens: 0, isDefault: true,
        contextTokens: 0, enabled: true, createdAt: new Date(0), updatedAt: new Date(0),
      },
      embeddingModel: "", auditor: null, logger: null, tracer: null, metrics: null,
      settings: { quotaEnabled: false, chainLimit: 0 },
    });
    return { service };
  }

  const baseConfig = {
    id: "", workspaceId: "", name: "c", adapter: "openai" as const,
    baseUrl: "", model: "gpt-4o", apiKey: "", priority: 0, maxTokens: 0,
    isDefault: false, contextTokens: 0, enabled: true, createdAt: new Date(0), updatedAt: new Date(0),
  };
  const scope = { userId: "u", workspaceId: "ws", agentId: "a" };

  test("upsert rejects SSRF and allows empty baseUrl", async () => {
    const { service } = createService();
    await expect(service.upsert(undefined, scope, { ...baseConfig, baseUrl: "http://169.254.169.254/v1" }))
      .rejects.toThrow("private or reserved");
    const ok = await service.upsert(undefined, scope, { ...baseConfig, baseUrl: "  " });
    expect(ok.name).toBe("c");
  });

  test("setAgentModel rejects SSRF base_url and allows empty base_url", async () => {
    const { service } = createService();
    await expect(service.setAgentModel(undefined, scope, { adapter: "openai", base_url: "http://localhost:8080", model: "m" }))
      .rejects.toThrow("reserved internal or private name");
    await service.setAgentModel(undefined, scope, { adapter: "openai", base_url: "", model: "m" });
  });
});
