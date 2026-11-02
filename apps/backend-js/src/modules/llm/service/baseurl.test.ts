import { describe, expect, test } from "bun:test";

import type { AgentStore, Store } from "../domain/gateway.ts";
import type { Provider } from "../domain/provider.ts";
import {
  assertSafeBaseUrl,
  checkBaseUrl,
  isPrivateOrReservedIP,
  MaxBaseUrlLength,
} from "./baseurl.ts";
import { Service } from "./service.ts";

describe("baseurl validation", () => {
  const dummyLookup = async (_host: string) => [
    { address: "93.184.216.34", family: 4 },
  ];

  test("empty string or whitespace is allowed (uses default deployment)", async () => {
    const empty = await checkBaseUrl("");
    expect(empty).toEqual({ ok: true, value: "" });

    const spaces = await checkBaseUrl("   ");
    expect(spaces).toEqual({ ok: true, value: "" });

    const asserted = await assertSafeBaseUrl("  ");
    expect(asserted).toBe("");
  });

  test("valid public https endpoint is accepted and trimmed", async () => {
    const res = await checkBaseUrl("  https://api.openai.com/v1  ", {
      lookup: dummyLookup,
    });
    expect(res).toEqual({ ok: true, value: "https://api.openai.com/v1" });

    const asserted = await assertSafeBaseUrl("https://api.openai.com/v1", {
      lookup: dummyLookup,
    });
    expect(asserted).toBe("https://api.openai.com/v1");
  });

  test("rejects cloud metadata IP 169.254.169.254", async () => {
    const res = await checkBaseUrl("http://169.254.169.254/", {
      allowHttp: true,
    });
    expect(res.ok).toBe(false);
    if (!res.ok) {
      expect(res.reason).toContain("private or reserved");
    }

    await expect(
      assertSafeBaseUrl("http://169.254.169.254/", { allowHttp: true }),
    ).rejects.toThrow();
  });

  test("rejects localhost hostname and port", async () => {
    const res = await checkBaseUrl("http://localhost:8080", {
      allowHttp: true,
    });
    expect(res.ok).toBe(false);
    if (!res.ok) {
      expect(res.reason).toContain("reserved internal or private name");
    }
  });

  test("rejects loopback and RFC 1918 private IPv4 literals", async () => {
    const targets = [
      "http://127.0.0.1",
      "http://127.0.0.2:8000",
      "http://10.1.2.3",
      "http://192.168.1.1",
      "http://172.16.0.1",
      "http://172.31.255.254",
      "http://0.0.0.0",
    ];

    for (const target of targets) {
      const res = await checkBaseUrl(target, { allowHttp: true });
      expect(res.ok).toBe(false);
      if (!res.ok) {
        expect(res.reason).toContain("private or reserved");
      }
    }
  });

  test("rejects IPv6 loopback, link-local, ULA, and IPv4-mapped", async () => {
    const targets = [
      "http://[::1]/",
      "http://[::]/",
      "http://[fe80::1]/",
      "http://[fc00::1]/",
      "http://[fd12:3456:789a::1]/",
      "http://[::ffff:127.0.0.1]/",
      "http://[::ffff:10.0.0.1]/",
      "http://[::ffff:192.168.1.1]/",
    ];

    for (const target of targets) {
      const res = await checkBaseUrl(target, { allowHttp: true });
      expect(res.ok).toBe(false);
      if (!res.ok) {
        expect(res.reason).toContain("private or reserved");
      }
    }
  });

  test("rejects CGNAT and multicast IP literals", async () => {
    const targets = [
      "http://100.64.0.1/",
      "http://100.127.255.254/",
      "http://224.0.0.1/",
      "http://239.255.255.255/",
    ];

    for (const target of targets) {
      const res = await checkBaseUrl(target, { allowHttp: true });
      expect(res.ok).toBe(false);
      if (!res.ok) {
        expect(res.reason).toContain("private or reserved");
      }
    }
  });

  test("rejects dotless hostnames that are not IP literals", async () => {
    const res = await checkBaseUrl("https://internal-svc/");
    expect(res.ok).toBe(false);
    if (!res.ok) {
      expect(res.reason).toContain("reserved internal or private name");
    }
  });

  test("rejects reserved internal domains without DNS resolution", async () => {
    const hosts = [
      "https://foo.localhost",
      "https://service.local",
      "https://backend.internal",
      "https://metadata.google.internal",
    ];

    for (const host of hosts) {
      const res = await checkBaseUrl(host);
      expect(res.ok).toBe(false);
      if (!res.ok) {
        expect(res.reason).toContain("reserved internal or private name");
      }
    }
  });

  test("rejects hostname that resolves to private IP via fake lookup", async () => {
    const fakeLookup = async () => [
      { address: "127.0.0.1", family: 4 },
    ];

    const res = await checkBaseUrl("https://evil.example.com", {
      lookup: fakeLookup,
    });
    expect(res.ok).toBe(false);
    if (!res.ok) {
      expect(res.reason).toContain("resolved to private or reserved IP address");
    }
  });

  test("rejects hostname when DNS resolution fails", async () => {
    const failingLookup = async () => {
      throw new Error("ENOTFOUND");
    };

    const res = await checkBaseUrl("https://nonexistent.domain.example", {
      lookup: failingLookup,
    });
    expect(res.ok).toBe(false);
    if (!res.ok) {
      expect(res.reason).toContain("could not resolve host");
    }
  });

  test("rejects unsupported protocols (ftp, file, gopher)", async () => {
    const ftp = await checkBaseUrl("ftp://example.com");
    expect(ftp.ok).toBe(false);
    if (!ftp.ok) {
      expect(ftp.reason).toContain("unsupported scheme");
    }

    const file = await checkBaseUrl("file:///etc/passwd");
    expect(file.ok).toBe(false);
    if (!file.ok) {
      expect(file.reason).toContain("unsupported scheme");
    }
  });

  test("http scheme: rejected when NODE_ENV=production, accepted when allowHttp=true", async () => {
    const origEnv = process.env.NODE_ENV;
    try {
      process.env.NODE_ENV = "production";
      const prodRes = await checkBaseUrl("http://example.com", {
        lookup: dummyLookup,
      });
      expect(prodRes.ok).toBe(false);
      if (!prodRes.ok) {
        expect(prodRes.reason).toContain("http: scheme is not allowed in production");
      }

      const devRes = await checkBaseUrl("http://example.com", {
        allowHttp: true,
        lookup: dummyLookup,
      });
      expect(devRes.ok).toBe(true);
    } finally {
      process.env.NODE_ENV = origEnv;
    }
  });

  test("rejects malformed URLs or excessively long strings", async () => {
    const invalid = await checkBaseUrl("not-a-url");
    expect(invalid.ok).toBe(false);
    if (!invalid.ok) {
      expect(invalid.reason).toContain("not a valid URL");
    }

    const tooLong = "https://example.com/" + "a".repeat(MaxBaseUrlLength);
    const longRes = await checkBaseUrl(tooLong);
    expect(longRes.ok).toBe(false);
    if (!longRes.ok) {
      expect(longRes.reason).toContain("exceeds max length");
    }
  });

  test("isPrivateOrReservedIP unit coverage", () => {
    expect(isPrivateOrReservedIP("127.0.0.1")).toBe(true);
    expect(isPrivateOrReservedIP("10.0.0.1")).toBe(true);
    expect(isPrivateOrReservedIP("172.16.0.1")).toBe(true);
    expect(isPrivateOrReservedIP("172.32.0.1")).toBe(false);
    expect(isPrivateOrReservedIP("192.168.0.1")).toBe(true);
    expect(isPrivateOrReservedIP("169.254.1.1")).toBe(true);
    expect(isPrivateOrReservedIP("100.64.0.1")).toBe(true);
    expect(isPrivateOrReservedIP("100.128.0.1")).toBe(false);
    expect(isPrivateOrReservedIP("0.0.0.0")).toBe(true);
    expect(isPrivateOrReservedIP("224.0.0.1")).toBe(true);
    expect(isPrivateOrReservedIP("240.0.0.1")).toBe(true);
    expect(isPrivateOrReservedIP("8.8.8.8")).toBe(false);
    expect(isPrivateOrReservedIP("1.1.1.1")).toBe(false);

    expect(isPrivateOrReservedIP("::1")).toBe(true);
    expect(isPrivateOrReservedIP("::")).toBe(true);
    expect(isPrivateOrReservedIP("fe80::1")).toBe(true);
    expect(isPrivateOrReservedIP("fc00::1")).toBe(true);
    expect(isPrivateOrReservedIP("fd00::1")).toBe(true);
    expect(isPrivateOrReservedIP("ff02::1")).toBe(true);
    expect(isPrivateOrReservedIP("2001:4860:4860::8888")).toBe(false);
  });
});

describe("Service base_url SSRF validation integration", () => {
  function createService() {
    const store: Store = {
      async list() {
        return [];
      },
      async get() {
        throw new Error("get is not used");
      },
      async create(_ctx, stored) {
        return stored;
      },
      async update(_ctx, stored) {
        return stored;
      },
      async delete() {},
    };
    let lastAgentOverride: Record<string, unknown> = {};
    const agents: AgentStore = {
      async agentModel() {
        return lastAgentOverride;
      },
      async setAgentModel(_ctx, _ws, _agent, override) {
        lastAgentOverride = override;
      },
    };
    const provider: Provider = {
      name: () => "mock",
      model: () => "mock-model",
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
        throw new Error("not used");
      },
      async stream() {
        throw new Error("not used");
      },
      async embed() {
        throw new Error("not used");
      },
    };
    const service = new Service({
      store,
      agents,
      factory: { build: () => provider },
      box: null,
      recorder: null,
      reader: null,
      counter: null,
      quota: null,
      costs: null,
      default: {
        id: "default",
        workspaceId: "ws-1",
        name: "default",
        adapter: "openai",
        baseUrl: "",
        model: "default-model",
        apiKey: "",
        priority: 0,
        maxTokens: 0,
        isDefault: true,
        contextTokens: 0,
        enabled: true,
        createdAt: new Date(0),
        updatedAt: new Date(0),
      },
      embeddingModel: "",
      auditor: null,
      logger: null,
      tracer: null,
      metrics: null,
      settings: { quotaEnabled: false, chainLimit: 0 },
    });
    return { service, agents };
  }

  test("upsert rejects internal SSRF base_url", async () => {
    const { service } = createService();
    const scope = { userId: "user-1", workspaceId: "ws-1", agentId: "" };

    await expect(
      service.upsert(undefined, scope, {
        id: "",
        workspaceId: "",
        name: "custom",
        adapter: "openai",
        baseUrl: "http://169.254.169.254/v1",
        model: "gpt-4o",
        apiKey: "",
        priority: 0,
        maxTokens: 0,
        isDefault: false,
        contextTokens: 0,
        enabled: true,
        createdAt: new Date(0),
        updatedAt: new Date(0),
      }),
    ).rejects.toThrow("private or reserved");
  });

  test("upsert allows empty baseUrl", async () => {
    const { service } = createService();
    const scope = { userId: "user-1", workspaceId: "ws-1", agentId: "" };

    const res = await service.upsert(undefined, scope, {
      id: "",
      workspaceId: "",
      name: "custom",
      adapter: "openai",
      baseUrl: "  ",
      model: "gpt-4o",
      apiKey: "",
      priority: 0,
      maxTokens: 0,
      isDefault: false,
      contextTokens: 0,
      enabled: true,
      createdAt: new Date(0),
      updatedAt: new Date(0),
    });
    expect(res.name).toBe("custom");
  });

  test("setAgentModel rejects SSRF base_url and allows empty base_url", async () => {
    const { service } = createService();
    const scope = { userId: "user-1", workspaceId: "ws-1", agentId: "agent-1" };

    await expect(
      service.setAgentModel(undefined, scope, {
        adapter: "openai",
        base_url: "http://localhost:8080",
        model: "gpt-4o",
      }),
    ).rejects.toThrow("reserved internal or private name");

    // Empty base_url passes
    await service.setAgentModel(undefined, scope, {
      adapter: "openai",
      base_url: "",
      model: "gpt-4o",
    });
  });
});
