/**
 * Tests for the Bolu API client.
 *
 * Pinned regressions:
 * 1. Tenancy header: every workspace-scoped API request (GET, POST, PATCH, DELETE)
 *    must carry the `X-Workspace-Id` header with the exact workspace ID passed (#725).
 * 2. Unscoped requests (e.g. currentUser, listWorkspaces) must NOT send `X-Workspace-Id`
 *    (neither empty string nor "undefined").
 * 3. 401 response: an authenticated request encountering 401 must trigger session renewal
 *    via POST /auth/refresh and retry with the refreshed token, rather than returning raw 401.
 * 4. 402 response: read-only mode returns 402, which must be passed through as ApiError
 *    with status === 402 without being swallowed or altered (#642).
 *
 * Run it with Bun:
 *   cd apps/frontend && bun test src/lib/api.test.ts
 */

import assert from "node:assert/strict";
import { afterEach, beforeEach, describe, it } from "node:test";

import {
  clearActiveWorkspaceId,
  currentWorkspace,
  currentUser,
  ensureActiveWorkspace,
  getActiveWorkspaceId,
  invite,
  listWorkspaces,
  removeMember,
  setActiveWorkspaceId,
  setAccessToken,
  updateWorkspace,
} from "./api";

type RecordedRequest = {
  url: string;
  method: string;
  headers: Record<string, string>;
  body: string | undefined;
};

const originalFetch = globalThis.fetch;
let recordedRequests: RecordedRequest[] = [];
let fetchHandler: (url: string, init?: RequestInit) => Promise<Response> | Response;

const localStorageStore = new Map<string, string>();
const mockLocalStorage: Storage = {
  getItem(key: string): string | null {
    return localStorageStore.get(key) ?? null;
  },
  setItem(key: string, value: string): void {
    localStorageStore.set(key, String(value));
  },
  removeItem(key: string): void {
    localStorageStore.delete(key);
  },
  clear(): void {
    localStorageStore.clear();
  },
  key(index: number): string | null {
    return Array.from(localStorageStore.keys())[index] ?? null;
  },
  get length(): number {
    return localStorageStore.size;
  },
};

const globalWithWindow = globalThis as unknown as {
  window?: {
    localStorage?: Storage;
  };
};

if (typeof globalWithWindow.window === "undefined") {
  globalWithWindow.window = {
    localStorage: mockLocalStorage,
  };
} else if (!globalWithWindow.window.localStorage) {
  Object.defineProperty(globalWithWindow.window, "localStorage", {
    value: mockLocalStorage,
    writable: true,
    configurable: true,
  });
}
function setMockFetch(handler: (url: string, init?: RequestInit) => Promise<Response> | Response) {
  fetchHandler = handler;
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.toString() : input.url;
    const method = init?.method ?? "GET";
    const rawHeaders = init?.headers;
    const headers: Record<string, string> = {};

    if (rawHeaders) {
      if (rawHeaders instanceof Headers) {
        rawHeaders.forEach((val, key) => {
          headers[key] = val;
        });
      } else if (Array.isArray(rawHeaders)) {
        for (const [key, val] of rawHeaders) {
          headers[key] = val;
        }
      } else {
        Object.assign(headers, rawHeaders);
      }
    }

    recordedRequests.push({
      url,
      method,
      headers,
      body: typeof init?.body === "string" ? init.body : undefined,
    });

    return fetchHandler(url, init);
  }) as typeof fetch;
}

function jsonResponse(data: unknown, status = 200, statusText = "OK"): Response {
  return new Response(JSON.stringify(data), {
    status,
    statusText,
    headers: {
      "Content-Type": "application/json",
      Date: new Date().toUTCString(),
    },
  });
}

describe("API client headers and tenancy (#725)", () => {
  beforeEach(() => {
    recordedRequests = [];
    setAccessToken("test-access-token", new Date(Date.now() + 600_000).toISOString());
    setActiveWorkspaceId("ws-local-storage");
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
    setAccessToken(null);
    clearActiveWorkspaceId();
  });

  it("sends X-Workspace-Id matching workspaceId on GET (currentWorkspace)", async () => {
    setMockFetch(() =>
      jsonResponse({
        success: true,
        message: "ok",
        data: {
          id: "ws-123",
          name: "Workspace 123",
          business_field: "Tech",
          timezone: "Asia/Jakarta",
          language: "id",
          work_hours_start: "09:00",
          work_hours_end: "17:00",
          work_days: [1, 2, 3, 4, 5],
          created_at: new Date().toISOString(),
        },
      }),
    );

    const result = await currentWorkspace("ws-123");
    assert.equal(result.ok, true);
    assert.equal(recordedRequests.length, 1);
    const req = recordedRequests[0];
    assert.equal(req.method, "GET");
    assert.equal(req.headers["X-Workspace-Id"], "ws-123");
  });

  it("sends X-Workspace-Id matching workspaceId on POST (invite)", async () => {
    setMockFetch(() =>
      jsonResponse({
        success: true,
        message: "ok",
        data: {
          id: "inv-1",
          email: "colleague@example.com",
          role: "member",
          expires_at: new Date().toISOString(),
          invited_at: new Date().toISOString(),
          workspace_id: "ws-456",
        },
      }),
    );

    const result = await invite("ws-456", "colleague@example.com", "member");
    assert.equal(result.ok, true);
    assert.equal(recordedRequests.length, 1);
    const req = recordedRequests[0];
    assert.equal(req.method, "POST");
    assert.equal(req.headers["X-Workspace-Id"], "ws-456");
  });

  it("sends X-Workspace-Id matching workspaceId on PATCH (updateWorkspace)", async () => {
    setMockFetch(() =>
      jsonResponse({
        success: true,
        message: "ok",
        data: {
          id: "ws-789",
          name: "Workspace Updated",
          business_field: "Retail",
          timezone: "Asia/Jakarta",
          language: "id",
          work_hours_start: "08:00",
          work_hours_end: "16:00",
          work_days: [1, 2, 3, 4, 5],
          created_at: new Date().toISOString(),
        },
      }),
    );

    const result = await updateWorkspace("ws-789", {
      name: "Workspace Updated",
      business_field: "Retail",
      timezone: "Asia/Jakarta",
    });
    assert.equal(result.ok, true);
    assert.equal(recordedRequests.length, 1);
    const req = recordedRequests[0];
    assert.equal(req.method, "PATCH");
    assert.equal(req.headers["X-Workspace-Id"], "ws-789");
  });

  it("sends X-Workspace-Id matching workspaceId on DELETE (removeMember)", async () => {
    setMockFetch(() =>
      jsonResponse({
        success: true,
        message: "ok",
        data: null,
      }),
    );

    const result = await removeMember("ws-delete-test", "user-xyz");
    assert.equal(result.ok, true);
    assert.equal(recordedRequests.length, 1);
    const req = recordedRequests[0];
    assert.equal(req.method, "DELETE");
    assert.equal(req.headers["X-Workspace-Id"], "ws-delete-test");
  });

  it("does NOT send X-Workspace-Id on unscoped requests (currentUser)", async () => {
    setMockFetch(() =>
      jsonResponse({
        success: true,
        message: "ok",
        data: {
          id: "user-1",
          email: "user@example.com",
          email_verified: true,
          onboarded: true,
          has_password: true,
          has_google: false,
          role: "user",
        },
      }),
    );

    const result = await currentUser();
    assert.equal(result.ok, true);
    assert.equal(recordedRequests.length, 1);
    const req = recordedRequests[0];
    assert.equal("X-Workspace-Id" in req.headers, false);
    assert.equal(req.headers["X-Workspace-Id"], undefined);
  });

  it("does NOT send X-Workspace-Id on unscoped requests (listWorkspaces)", async () => {
    setMockFetch(() =>
      jsonResponse({
        success: true,
        message: "ok",
        data: [],
      }),
    );

    const result = await listWorkspaces();
    assert.equal(result.ok, true);
    assert.equal(recordedRequests.length, 1);
    const req = recordedRequests[0];
    assert.equal("X-Workspace-Id" in req.headers, false);
    assert.equal(req.headers["X-Workspace-Id"], undefined);
  });

  it("triggers session renewal via POST /auth/refresh on 401 and retries request", async () => {
    let callCount = 0;
    setMockFetch((url) => {
      if (url.endsWith("/auth/refresh")) {
        return jsonResponse({
          success: true,
          message: "ok",
          data: {
            access_token: "refreshed-token",
            access_expires_at: new Date(Date.now() + 600_000).toISOString(),
            refresh_expires_at: new Date(Date.now() + 86400_000).toISOString(),
            user: {
              id: "u1",
              email: "u1@example.com",
              email_verified: true,
              onboarded: true,
              has_password: true,
              has_google: false,
              role: "user",
            },
          },
        });
      }

      callCount++;
      if (callCount === 1) {
        return jsonResponse(
          {
            success: false,
            message: "token expired",
            error: { code: "unauthorized" },
          },
          401,
          "Unauthorized",
        );
      }

      return jsonResponse({
        success: true,
        message: "ok",
        data: {
          id: "ws-renew",
          name: "Renewed Workspace",
          business_field: "Tech",
          timezone: "Asia/Jakarta",
          language: "id",
          work_hours_start: "09:00",
          work_hours_end: "17:00",
          work_days: [1, 2, 3, 4, 5],
          created_at: new Date().toISOString(),
        },
      });
    });

    const result = await currentWorkspace("ws-renew");
    assert.equal(result.ok, true);
    if (result.ok) {
      assert.equal(result.data.name, "Renewed Workspace");
    }

    assert.equal(recordedRequests.length, 3);
    assert.match(recordedRequests[0].url, /\/api\/workspaces\/current$/);
    assert.equal(recordedRequests[0].headers.Authorization, "Bearer test-access-token");

    assert.match(recordedRequests[1].url, /\/api\/auth\/refresh$/);
    assert.equal(recordedRequests[1].method, "POST");

    assert.match(recordedRequests[2].url, /\/api\/workspaces\/current$/);
    assert.equal(recordedRequests[2].headers.Authorization, "Bearer refreshed-token");
    assert.equal(recordedRequests[2].headers["X-Workspace-Id"], "ws-renew");
  });

  it("passes 402 through as ApiError with status === 402 without altering it (#642)", async () => {
    setMockFetch(() =>
      jsonResponse(
        {
          success: false,
          message: "ruang kerja sedang dalam mode baca saja",
          error: { code: "workspace_read_only" },
        },
        402,
        "Payment Required",
      ),
    );

    const result = await updateWorkspace("ws-readonly", {
      name: "New Name",
      business_field: "Tech",
      timezone: "Asia/Jakarta",
    });

    assert.equal(result.ok, false);
    if (!result.ok) {
      assert.equal(result.status, 402);
      assert.equal(result.code, "workspace_read_only");
      assert.equal(result.message, "ruang kerja sedang dalam mode baca saja");
    }
  });

  it("returns stored workspace id when listWorkspaces fails (!result.ok)", async () => {
    setActiveWorkspaceId("ws-existing");
    setMockFetch((url) => {
      if (url.includes("/api/auth/refresh")) {
        return jsonResponse({
          success: true,
          message: "ok",
          data: {
            user: { id: "u-1", email: "test@example.com", name: "Test", email_verified: true, created_at: "" },
            token: "refreshed-token",
            expires_at: new Date(Date.now() + 600_000).toISOString(),
          },
        });
      }
      if (url.includes("/api/workspaces")) {
        return jsonResponse({ success: false, message: "service unavailable" }, 503);
      }
      return jsonResponse({ success: false, message: "not found" }, 404);
    });

    const result = await ensureActiveWorkspace();
    assert.equal(result, "ws-existing");
    assert.equal(getActiveWorkspaceId(), "ws-existing");
  });

  it("clears stored workspace id and returns null when listWorkspaces succeeds with empty list (#637)", async () => {
    setActiveWorkspaceId("ws-stale-deleted");
    setMockFetch((url) => {
      if (url.includes("/api/auth/refresh")) {
        return jsonResponse({
          success: true,
          message: "ok",
          data: {
            user: { id: "u-1", email: "test@example.com", name: "Test", email_verified: true, created_at: "" },
            token: "refreshed-token",
            expires_at: new Date(Date.now() + 600_000).toISOString(),
          },
        });
      }
      if (url.includes("/api/workspaces")) {
        return jsonResponse({ success: true, message: "ok", data: [] });
      }
      return jsonResponse({ success: false, message: "not found" }, 404);
    });

    const result = await ensureActiveWorkspace();
    assert.equal(result, null);
    assert.equal(getActiveWorkspaceId(), null);
  });

  it("returns stored workspace id when listWorkspaces contains it", async () => {
    setActiveWorkspaceId("ws-2");
    setMockFetch((url) => {
      if (url.includes("/api/auth/refresh")) {
        return jsonResponse({
          success: true,
          message: "ok",
          data: {
            user: { id: "u-1", email: "test@example.com", name: "Test", email_verified: true, created_at: "" },
            token: "refreshed-token",
            expires_at: new Date(Date.now() + 600_000).toISOString(),
          },
        });
      }
      if (url.includes("/api/workspaces")) {
        return jsonResponse({
          success: true,
          message: "ok",
          data: [
            { id: "ws-1", name: "Workspace 1", business_field: "Tech", timezone: "Asia/Jakarta", language: "id", work_hours_start: "09:00", work_hours_end: "17:00", work_days: [1, 2, 3, 4, 5], created_at: "" },
            { id: "ws-2", name: "Workspace 2", business_field: "Tech", timezone: "Asia/Jakarta", language: "id", work_hours_start: "09:00", work_hours_end: "17:00", work_days: [1, 2, 3, 4, 5], created_at: "" },
          ],
        });
      }
      return jsonResponse({ success: false, message: "not found" }, 404);
    });

    const result = await ensureActiveWorkspace();
    assert.equal(result, "ws-2");
    assert.equal(getActiveWorkspaceId(), "ws-2");
  });
});
