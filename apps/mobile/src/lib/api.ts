/**
 * API client for the Bolu backend, shaped after `apps/frontend/src/lib/api.ts`.
 *
 * Three details differ from the web client, and each one is load-bearing:
 * - there is no cookie jar we can read, so a renewal presents its refresh token
 *   in the body and takes the rotated one back from the body
 *   (`docs/api.md`, *Authentication*);
 * - the access token lives in memory only and the refresh token in the platform
 *   keystore, so a cold start begins by asking /auth/refresh;
 * - the workspace is chosen once per run from `GET /workspaces` rather than
 *   remembered on disk, because this scaffold has no storage beyond the keystore
 *   (see `docs/mobile.md`).
 */

import {
  clearRefreshToken,
  getAccessToken,
  readRefreshToken,
  saveRefreshToken,
  setAccessToken,
  tokenIsSpent,
} from "@/lib/tokens";

const API_URL = (process.env.EXPO_PUBLIC_API_URL ?? "https://timbolu.com").replace(/\/+$/, "");

export type ApiError = {
  ok: false;
  status: number;
  message: string;
  /** The API's machine-readable reason, which a screen acts on rather than shows. */
  code?: string;
  details?: unknown;
};

export type ApiSuccess<T> = {
  ok: true;
  status: number;
  data: T;
  meta?: Record<string, unknown>;
};

export type ApiResult<T> = ApiSuccess<T> | ApiError;

export type User = {
  id: string;
  email: string;
  email_verified: boolean;
  onboarded: boolean;
  has_password: boolean;
  has_google: boolean;
  /** The platform role, not the workspace one. */
  role: string;
};

export type Session = {
  access_token: string;
  access_expires_at: string;
  refresh_expires_at: string;
  user: User;
  /**
   * Present only when the refresh token was presented in the body, which is the
   * only path this client uses. A browser reads the rotated token from its
   * cookie instead.
   */
  refresh_token?: string;
};

export type Workspace = {
  id: string;
  name: string;
  business_field: string;
  timezone: string;
  language: string;
  role?: string;
  created_at: string;
};

export type Agent = {
  id: string;
  team_id: string;
  team_name: string;
  name: string;
  role: string;
  persona: string;
  tone: string;
  shape: string;
  color: string;
  /** The stored switch: active or resting. */
  status: string;
  /** Derived from tasks and drafts, never stored. */
  display_status: string;
  tools: string[];
  created_at: string;
};

export type TextBlock = { type: "text"; markdown: string; title?: string };

/**
 * Any block the API may store. The scaffold renders only `text`; the other five
 * types are named here so a renderer added later narrows rather than casts.
 */
export type MessageBlock = { type: string } & Record<string, unknown>;

export type MessageStatus = "complete" | "streaming" | "partial" | "failed";

export type ChatMessage = {
  message_id: string;
  conversation_id: string;
  agent_id?: string;
  user_id?: string;
  blocks: MessageBlock[];
  task_id?: string;
  status: MessageStatus;
  finish_reason?: string;
  created_at: string;
};

export type ChatParticipant = {
  id: string;
  agent_id?: string;
  user_id?: string;
  name: string;
  role: string;
  is_agent: boolean;
};

export type ChatConversation = {
  id: string;
  kind: "direct" | "group" | "thread";
  title: string;
  participants: ChatParticipant[];
  message_count: number;
  last_activity_at: string;
  created_at: string;
};

export type MessagePage = {
  messages: ChatMessage[];
  next_cursor?: string;
  has_more: boolean;
};

export type SendMessageResult = {
  message: ChatMessage;
  /** One placeholder per Bolu that will answer, in the order they were picked. */
  reply_message_ids?: string[];
  responders?: ChatParticipant[];
  routed: boolean;
};

type Envelope<T> = {
  success: boolean;
  message: string;
  data?: T;
  meta?: Record<string, unknown>;
  error?: { code?: string; details?: unknown };
};

type RequestOptions = {
  method?: string;
  body?: unknown;
  workspace?: string | null;
  authenticated?: boolean;
  timeoutMs?: number;
  timeoutMessage?: string;
};

/**
 * The workspace requests are scoped to. It is resolved once per app run, because
 * the tenant header needs an id and this scaffold has nowhere to remember a
 * choice across cold starts.
 */
let activeWorkspaceId: string | null = null;

export function getActiveWorkspaceId(): string | null {
  return activeWorkspaceId;
}

/**
 * The headers one call carries. `authenticated` is not decoration: a renewal
 * must not present the very token the API just refused, or the refusal it is
 * trying to answer would be repeated to the endpoint that replaces it.
 */
function apiHeaders(workspace: string | null, authenticated: boolean): Record<string, string> {
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  const token = authenticated ? getAccessToken() : null;
  if (token) headers.Authorization = `Bearer ${token}`;
  if (workspace) headers["X-Workspace-Id"] = workspace;
  return headers;
}

/**
 * The API's own words are English and name a state ("email or password is
 * incorrect"), so the three answers a person can act on are restated in the
 * screen's language. Anything else keeps the server's explanation, which is more
 * useful than a generic apology.
 */
function loginMessage(code: string | undefined, fallback: string | undefined): string {
  switch (code) {
    case "invalid_credentials":
      return "Email atau kata sandi salah.";
    case "email_not_verified":
      return "Email belum diverifikasi. Buka tautan verifikasi di kotak masukmu.";
    case "too_many_attempts":
      return "Terlalu banyak percobaan masuk. Tunggu sebentar lalu coba lagi.";
    default:
      return fallback ?? "Gagal masuk. Coba lagi sebentar.";
  }
}

/** Normalises `{success, message, data, meta}` into a result a screen branches on. */
async function parseEnvelope<T>(response: Response): Promise<ApiResult<T>> {
  const text = await response.text();
  let envelope: Envelope<T> | null = null;
  if (text) {
    try {
      envelope = JSON.parse(text) as Envelope<T>;
    } catch {
      envelope = null;
    }
  }

  if (!response.ok || !envelope?.success) {
    return {
      ok: false,
      status: response.status,
      message: envelope?.message ?? "Terjadi kesalahan. Coba lagi sebentar.",
      code: envelope?.error?.code,
      details: envelope?.error?.details,
    };
  }

  return { ok: true, status: response.status, data: envelope.data as T, meta: envelope.meta };
}

/** Performs one API call with the bearer token and a workspace header. */
async function request<T>(path: string, options: RequestOptions = {}): Promise<ApiResult<T>> {
  const authenticated = options.authenticated !== false;

  // The deadline is an abort signal rather than a race against a timer, because
  // the socket is what has to be let go: a server that accepts the connection
  // and never answers would otherwise keep the button pending indefinitely.
  //
  // No handle is kept for the deadline: the `finally` aborts the same controller,
  // and aborting a settled request is a no-op, so a late timer cannot do anything
  // but be collected.
  const controller = new AbortController();
  if (options.timeoutMs !== undefined) setTimeout(() => controller.abort(), options.timeoutMs);

  try {
    const response = await authorizedFetch(
      `${API_URL}/api${path}`,
      {
        method: options.method ?? "GET",
        body: options.body === undefined ? undefined : JSON.stringify(options.body),
        signal: controller.signal,
      },
      () => apiHeaders(options.workspace ?? null, authenticated),
      authenticated,
    );
    return await parseEnvelope<T>(response);
  } catch {
    if (controller.signal.aborted) {
      return {
        ok: false,
        status: 0,
        code: "timeout",
        message: options.timeoutMessage ?? "Server terlalu lama menjawab. Coba lagi.",
      };
    }
    return {
      ok: false,
      status: 0,
      message: "Tidak bisa menghubungi server. Cek koneksimu lalu coba lagi.",
    };
  } finally {
    controller.abort();
  }
}

/**
 * The renewal this app is currently running.
 *
 * The presented refresh token is revoked when it is spent, so two renewals
 * racing would each rotate and the loser would revoke the winner's session. A
 * screen that reads through `Promise.all` asks several times at once; callers
 * share one attempt instead.
 */
let refreshInFlight: Promise<ApiResult<Session>> | null = null;

/**
 * Exchanges the stored refresh token for a new access token and saves the
 * rotated token the response carries.
 *
 * A response with no rotated token keeps the one presented. That cannot happen
 * against this API — the body path always returns one — and losing the session
 * to protect an invariant the server already holds would be the worse answer.
 */
export function refreshSession(): Promise<ApiResult<Session>> {
  if (!refreshInFlight) {
    refreshInFlight = (async (): Promise<ApiResult<Session>> => {
      const presented = await readRefreshToken();
      if (!presented) {
        setAccessToken(null);
        return { ok: false, status: 401, code: "no_refresh_token", message: "Sesi berakhir. Silakan masuk lagi." };
      }

      const result = await request<Session>("/auth/refresh", {
        method: "POST",
        body: { token: presented },
        authenticated: false,
      });

      if (result.ok) {
        setAccessToken(result.data.access_token, result.data.access_expires_at);
        await saveRefreshToken(result.data.refresh_token ?? presented);
      } else {
        // A refused renewal is the answer "this session is over"; keeping the
        // token would make every later screen retry a dead one.
        setAccessToken(null);
        await clearRefreshToken();
      }
      return result;
    })().finally(() => {
      refreshInFlight = null;
    });
  }
  return refreshInFlight;
}

/**
 * Sends one request with the bearer token, renewing before a call whose token is
 * spent and once more when the API refuses one.
 *
 * The access token lives fifteen minutes while a phone stays in a pocket far
 * longer, so without this the first tap after a break would answer with the
 * sign-in screen instead of the workspace.
 */
async function authorizedFetch(
  url: string,
  init: RequestInit,
  headers: () => Record<string, string>,
  authenticated: boolean,
): Promise<Response> {
  let renewed = false;
  if (authenticated && tokenIsSpent()) {
    renewed = true;
    await refreshSession();
  }

  let response = await fetch(url, { ...init, headers: headers() });

  if (response.status === 401 && authenticated && !renewed) {
    const renewal = await refreshSession();
    if (renewal.ok) {
      response = await fetch(url, { ...init, headers: headers() });
    }
  }

  return response;
}

/**
 * The refresh token a sign-in leaves in `Set-Cookie`.
 *
 * `POST /auth/login` hands a browser its session as an httpOnly cookie and never
 * repeats the refresh token in the body, and this client has no readable cookie
 * jar. The response header is the only channel left. A platform that hides it
 * yields `null`, which costs a sign-in on the next cold start and nothing else —
 * `docs/mobile.md` states that limitation.
 */
function refreshTokenFromSetCookie(setCookie: string | null): string | null {
  if (!setCookie) return null;
  // Two cookies are set on this response (`bolu_refresh` and, on some
  // deployments, a clearsite companion), and a comma inside an `Expires` value
  // is not a separator, so each candidate is split on `;` and never on `,`.
  for (const part of setCookie.split(/,(?=[^;=]+=)/)) {
    const [pair] = part.trim().split(";");
    const index = pair.indexOf("=");
    if (index < 0) continue;
    if (pair.slice(0, index).trim() !== "bolu_refresh") continue;
    const value = pair.slice(index + 1).trim();
    if (value !== "") return value;
  }
  return null;
}

/**
 * The answer to "which workspace is this run scoped to?" — three different
 * things, kept apart because a screen acts differently on each: the account has
 * one, the account has none yet (onboarding lives on the web), or the question
 * could not be asked. Folding the last two into a single `null` would tell a
 * person whose phone is offline to create a workspace they already have.
 */
export type WorkspaceLookup =
  | { kind: "workspace"; workspaceId: string }
  | { kind: "no-workspace" }
  | { kind: "error"; error: ApiError };

/**
 * Reads the namespace one app run works in: the first workspace the account
 * belongs to.
 *
 * Every tenant-scoped call needs `X-Workspace-Id`, and this scaffold has nowhere
 * to remember a chosen one, so the first is the default — which is what an
 * account with a single workspace expects to see anyway.
 *
 * `refresh` skips the cached id and asks `GET /workspaces` again. That is what a
 * "Coba lagi" button needs after the workspace was created in a browser: the
 * account's membership changed outside this run, so only a fresh answer knows it.
 */
async function resolveWorkspace(refresh: boolean): Promise<WorkspaceLookup> {
  if (!refresh && activeWorkspaceId) {
    return { kind: "workspace", workspaceId: activeWorkspaceId };
  }
  const result = await request<Workspace[]>("/workspaces");
  if (!result.ok) return { kind: "error", error: result };

  const first = result.data[0];
  if (!first) {
    // A workspace can disappear between calls (it was removed on the web), so a
    // stale id must not survive an answer that says there is nothing to scope to.
    activeWorkspaceId = null;
    return { kind: "no-workspace" };
  }

  activeWorkspaceId = first.id;
  return { kind: "workspace", workspaceId: activeWorkspaceId };
}

/**
 * The workspace id a tenant-scoped call carries, or `null` when there is none
 * to carry. Callers that only need "can this call proceed?" -- the chat calls --
 * use this; the Bolu list asks `resolveWorkspace` directly so it can say which
 * of the two empty reasons applies.
 */
async function ensureWorkspace(): Promise<string | null> {
  const found = await resolveWorkspace(false);
  return found.kind === "workspace" ? found.workspaceId : null;
}

/** What a screen needs once a session exists: the account and its workspace. */
export type SignedIn = { session: Session; workspaceId: string | null };

/**
 * Signs in, keeps the refresh token, and resolves the workspace.
 *
 * The workspace is resolved here rather than in a screen so the first read after
 * signing in cannot race the tenant header into existence.
 */
export async function login(email: string, password: string): Promise<ApiResult<SignedIn>> {
  // Sign-in is a person watching a button, so it may not hang: the deadline is
  // what turns a backend that never answers into something the form can explain.
  const controller = new AbortController();
  setTimeout(() => controller.abort(), 20_000);

  try {
    const response = await fetch(`${API_URL}/api/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ email, password }),
      signal: controller.signal,
    });

    const setCookie = response.headers.get("set-cookie");
    const result = await parseEnvelope<Session>(response);

    if (!result.ok) {
      return {
        ok: false,
        status: result.status,
        message: loginMessage(result.code, result.message),
        code: result.code,
      };
    }

    setAccessToken(result.data.access_token, result.data.access_expires_at);
    const refresh = refreshTokenFromSetCookie(setCookie);
    if (refresh) await saveRefreshToken(refresh);

    activeWorkspaceId = null;
    const workspaceId = await ensureWorkspace();
    return { ok: true, status: result.status, data: { session: result.data, workspaceId } };
  } catch {
    return {
      ok: false,
      status: 0,
      code: controller.signal.aborted ? "timeout" : undefined,
      message: controller.signal.aborted
        ? "Server terlalu lama menjawab. Coba lagi."
        : "Tidak bisa menghubungi server. Cek koneksimu lalu coba lagi.",
    };
  } finally {
    controller.abort();
  }
}

/**
 * Restores the session a previous run left behind, or reports that there is
 * none. A cold start calls this before it decides which screen to show.
 */
export async function restoreSession(): Promise<ApiResult<SignedIn>> {
  const refresh = await refreshSession();
  if (!refresh.ok) return refresh;

  const workspaceId = await ensureWorkspace();
  return { ok: true, status: refresh.status, data: { session: refresh.data, workspaceId } };
}

/**
 * Ends the session on both ends. The refresh token is named in the body because
 * there is no cookie to revoke with, and it is deleted whatever the server
 * answers: a phone that keeps a token it has told the server to forget is the
 * one failure worth preventing here.
 */
export async function logout(): Promise<void> {
  const token = await readRefreshToken();
  if (token) {
    await request<null>("/auth/logout", { method: "POST", body: { token }, authenticated: false });
  }
  setAccessToken(null);
  await clearRefreshToken();
  activeWorkspaceId = null;
}

const NO_WORKSPACE: ApiError = {
  ok: false,
  status: 404,
  code: "no_workspace",
  message: "Akun ini belum punya workspace.",
};

/**
 * What `GET /agents` means to a screen. `agents: []` is a workspace that exists
 * but has no Bolu yet; `no-workspace` is an account that has nothing to look
 * inside. They need different sentences and different buttons, so they are not
 * one `ApiResult` whose error text happens to differ.
 */
export type AgentListResult =
  | { kind: "agents"; agents: Agent[] }
  | { kind: "no-workspace" }
  | { kind: "error"; error: ApiError };

/**
 * The Bolu in the active workspace, flat and in the registry's own order.
 *
 * `refreshWorkspace` asks `GET /workspaces` again before reading. The no-workspace
 * screen passes it on "Coba lagi": the workspace is created in a browser, so the
 * app only learns about it by asking rather than trusting this run's cache.
 */
export async function listAgents(
  options: { refreshWorkspace?: boolean } = {},
): Promise<AgentListResult> {
  const found = await resolveWorkspace(options.refreshWorkspace === true);
  if (found.kind !== "workspace") return found;

  const result = await request<Agent[]>("/agents", { workspace: found.workspaceId });
  if (!result.ok) return { kind: "error", error: result };
  return { kind: "agents", agents: result.data };
}

/**
 * The 1:1 thread with one Bolu, created on first open. The server makes this
 * idempotent, so a chat screen may call it on every mount.
 */
export async function openDirectConversation(agentId: string): Promise<ApiResult<ChatConversation>> {
  const workspaceId = await ensureWorkspace();
  if (!workspaceId) return NO_WORKSPACE;
  return request<ChatConversation>("/conversations/direct", {
    method: "POST",
    body: { agent_id: agentId },
    workspace: workspaceId,
  });
}

/** One page of history, newest first. */
export async function listMessages(
  conversationId: string,
  cursor?: string,
  limit = 40,
): Promise<ApiResult<MessagePage>> {
  const workspaceId = await ensureWorkspace();
  if (!workspaceId) return NO_WORKSPACE;
  const query = `?limit=${limit}${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`;
  return request<MessagePage>(`/conversations/${conversationId}/messages${query}`, {
    workspace: workspaceId,
  });
}

/**
 * Stores one message and asks the Bolu to answer it.
 *
 * `reply: true` is what opens the task that produces the answer; without it the
 * message would sit in the thread unanswered.
 */
export async function sendMessage(conversationId: string, text: string): Promise<ApiResult<SendMessageResult>> {
  const workspaceId = await ensureWorkspace();
  if (!workspaceId) return NO_WORKSPACE;
  return request<SendMessageResult>(`/conversations/${conversationId}/messages`, {
    method: "POST",
    body: { text, reply: true },
    workspace: workspaceId,
    timeoutMs: 20_000,
    timeoutMessage: "Server terlalu lama menjawab. Pesanmu mungkin sudah terkirim — cek obrolannya.",
  });
}

/**
 * The readable text of one message.
 *
 * A body is a list of typed blocks and the scaffold renders only `text`; the
 * other five types are counted rather than dropped silently, so a message never
 * looks empty when it carried a table or a draft.
 */
export function messageText(blocks: MessageBlock[]): string {
  const parts: string[] = [];
  let others = 0;
  for (const block of blocks) {
    if (block.type === "text" && typeof block.markdown === "string") {
      parts.push(block.markdown);
    } else {
      others += 1;
    }
  }
  if (others > 0) {
    parts.push(`(${others} bagian lain belum bisa ditampilkan di app ini)`);
  }
  return parts.join("\n\n").trim();
}

export { API_URL };
