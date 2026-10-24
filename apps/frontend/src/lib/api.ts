/**
 * API client for the Bolu backend.
 *
 * Three details matter here:
 * - every request sends credentials, because the refresh token lives in an
 *   httpOnly cookie that JavaScript must not read;
 * - the access token is kept in memory only. A reload calls /auth/refresh,
 *   which reads the cookie and returns a fresh access token;
 * - the token outlives neither a break nor a second tab, so `authorizedFetch`
 *   renews before a call whose token is spent and once more when the API
 *   refuses one. Without that, a screen opened after an idle spell answers with
 *   the API's 401 instead of the workspace.
 */

import { SESSION_HINT_COOKIE, SESSION_HINT_MAX_AGE } from "@/lib/session-hint";

const API_URL = process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:8080";

export type ApiError = {
  ok: false;
  status: number;
  message: string;
  /** The API's machine-readable reason, which a screen acts on rather than shows. */
  code?: string;
  /** Whatever the API attached to that reason, when it names what went wrong. */
  details?: unknown;
};

export type ApiSuccess<T> = {
  ok: true;
  status: number;
  data: T;
  /**
   * The envelope's meta, when the endpoint sent one. Paginated reads put the
   * total and the page the filter became here, so a screen can render "3 of 41"
   * without remembering what it asked for.
   */
  meta?: Record<string, unknown>;
  /**
   * The `Date` header, as epoch milliseconds, when the response carried a
   * parsable one.
   *
   * It is the server's own clock, which is what a read that later has to compare
   * itself against a server-written timestamp needs: a client clock that is
   * minutes off would otherwise decide which of two records is the older one.
   */
  serverDate?: number;
};

export type ApiResult<T> = ApiSuccess<T> | ApiError;

export type User = {
  id: string;
  email: string;
  email_verified: boolean;
  onboarded: boolean;
  has_password: boolean;
  has_google: boolean;
  /**
   * The platform role: `admin` reaches /admin, `user` is everybody else. It is
   * not the workspace role — that one is on `Workspace.role` and answers what
   * the account may do inside one workspace. The screen uses this only to decide
   * whether to offer the link; the API checks the role again on every request,
   * so a stale value here costs nothing but a redirect.
   */
  role: string;
};

export type Session = {
  access_token: string;
  access_expires_at: string;
  refresh_expires_at: string;
  user: User;
};

export type Workspace = {
  id: string;
  name: string;
  business_field: string;
  timezone: string;
  language: string;
  role?: string;
  /** The window outbound actions may leave in, as "HH:MM" in `timezone`. */
  work_hours_start: string;
  work_hours_end: string;
  /** The ISO weekdays the window applies to, 1 = Monday. */
  work_days: number[];
  created_at: string;
};

export type Team = {
  id: string;
  name: string;
  kind: string;
};

export type Member = {
  user_id: string;
  email: string;
  role: string;
  email_verified: boolean;
  joined_at: string;
};

export type Invitation = {
  id: string;
  email: string;
  role: string;
  expires_at: string;
  invited_at: string;
  workspace_id?: string;
};

export type OnboardResult = {
  workspace: Workspace;
  teams: Team[];
  created: boolean;
};

/** The workspace the user picked, remembered across reloads. */
const WORKSPACE_KEY = "bolu.workspace_id";

let accessToken: string | null = null;
/** When the token above stops being accepted, as epoch milliseconds. */
let accessTokenExpiresAt: number | null = null;

/**
 * How long before its expiry a token is treated as spent.
 *
 * A call that leaves just inside the last moment would arrive after it, so the
 * window is the cost of a round trip rather than zero: renewing a little early
 * is cheaper than a 401 that has to be answered with a second call.
 */
const REFRESH_SKEW_MS = 30_000;

export function getAccessToken(): string | null {
  return accessToken;
}

/**
 * tokenIsSpent reports whether the token in memory is missing, or close enough
 * to its expiry that a call sent now would arrive after it.
 *
 * An unknown expiry is not spent: the API is still the authority, and refusing to
 * use a token because the clock was not recorded would turn a missing field into a
 * refresh loop.
 */
function tokenIsSpent(): boolean {
  if (!accessToken) {
    return true;
  }
  if (accessTokenExpiresAt === null) {
    return false;
  }
  return Date.now() >= accessTokenExpiresAt - REFRESH_SKEW_MS;
}

/**
 * setAccessToken is where a session is gained and lost, so it is also where the
 * edge gate is told about it.
 *
 * Every path runs through here — login, refresh, logout — so the hint cannot drift
 * from the token. Mirroring it in any of those three callers instead would leave
 * the fourth, whenever somebody adds one, updating the session without updating
 * the gate.
 *
 * The hint is not readable JavaScript state: it carries no secret and the API
 * never looks at it. It exists so the gate can answer before the dashboard shell
 * is sent, which the refresh cookie cannot do because the backend scopes it to
 * the auth endpoints on purpose.
 *
 * `expiresAt` is the `access_expires_at` the session carried. It is stored beside
 * the token rather than derived from it, because the only thing that knows when
 * the token dies is the issuer — a decoded `exp` would be a second answer to the
 * same question, and the client must not hold the JWT's payload to read it.
 */
export function setAccessToken(token: string | null, expiresAt?: string | null): void {
  accessToken = token;
  accessTokenExpiresAt = token && expiresAt ? Date.parse(expiresAt) : null;
  if (accessTokenExpiresAt !== null && Number.isNaN(accessTokenExpiresAt)) {
    // An unparsable date is treated as unknown, which keeps the token usable and
    // leaves the API as the authority rather than trusting a broken clock.
    accessTokenExpiresAt = null;
  }

  if (typeof document === "undefined") {
    return;
  }

  // A hint with no expiry outlives the refresh cookie it stands for, and a stale
  // one only costs the gate a guess it can recover from: the layout still
  // redirects once the refresh fails. Deleting on the way out is what makes
  // logging out visible at the edge at all.
  if (token === null) {
    document.cookie = `${SESSION_HINT_COOKIE}=; Max-Age=0; Path=/; SameSite=Lax`;
    return;
  }

  document.cookie = `${SESSION_HINT_COOKIE}=1; Max-Age=${SESSION_HINT_MAX_AGE}; Path=/; SameSite=Lax`;
}

export function getActiveWorkspaceId(): string | null {
  if (typeof window === "undefined") {
    return null;
  }
  return window.localStorage.getItem(WORKSPACE_KEY);
}

export function setActiveWorkspaceId(workspaceId: string): void {
  if (typeof window === "undefined") {
    return;
  }
  window.localStorage.setItem(WORKSPACE_KEY, workspaceId);
}

export function clearActiveWorkspaceId(): void {
  if (typeof window === "undefined") {
    return;
  }
  window.localStorage.removeItem(WORKSPACE_KEY);
}

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
  /** Sends the active workspace header, for tenant-scoped endpoints. */
  workspace?: string | null;
  /** Sends the in-memory access token as a bearer token. */
  authenticated?: boolean;
  /**
   * How long the call may take before it is abandoned, in milliseconds.
   *
   * Only the calls a person is waiting on a button for need one: a fetch has no
   * deadline of its own, so a backend that accepts the connection and then never
   * answers leaves the button saying "Mendaftarkan…" for as long as the browser
   * keeps the socket open. Omitting it keeps the old behaviour — a screen's
   * background reads are not on anybody's clock.
   */
  timeoutMs?: number;
};

/**
 * apiHeaders builds the headers one call carries.
 *
 * It is shared with the few reads that do not go through `request` — the CSV export, which wants a
 * file rather than a parsed envelope — so the two cannot drift about how a token and a workspace
 * travel.
 */
function apiHeaders(workspace: string | null): Record<string, string> {
  const headers: Record<string, string> = { "Content-Type": "application/json" };

  if (accessToken) {
    headers.Authorization = `Bearer ${accessToken}`;
  }
  if (workspace) {
    headers["X-Workspace-Id"] = workspace;
  }

  return headers;
}

/**
 * refreshInFlight is the renewal one tab is currently running.
 *
 * The refresh cookie is single-use — the server revokes the session it was spent
 * on — so two renewals racing would each rotate the token and the loser would
 * revoke the winner's. A screen that loads through `Promise.all` asks several
 * times at once, and without this the answer would be a session destroyed by its
 * own tab. Callers share one attempt instead.
 */
let refreshInFlight: Promise<ApiResult<Session>> | null = null;

/**
 * refreshSession exchanges the refresh cookie for a new access token, at most one
 * attempt at a time.
 *
 * This is *the* question "is my session still alive", so its answer is what the
 * token and the gate are set from — including the answer "no". A failed renewal
 * that left the hint standing would keep admitting a browser whose session is
 * gone, and the gate would be advice rather than a gate.
 *
 * Callers share one attempt because the cookie is single-use: two racing
 * renewals would each rotate the session and the second would revoke the first.
 * Screens that load through `Promise.all` — and the automatic renewal below —
 * therefore collapse into one exchange.
 */
export function refreshSession(): Promise<ApiResult<Session>> {
  if (!refreshInFlight) {
    refreshInFlight = request<Session>("/auth/refresh", { method: "POST", authenticated: false })
      .then((result) => {
        setAccessToken(result.ok ? result.data.access_token : null, result.ok ? result.data.access_expires_at : null);
        return result;
      })
      .finally(() => {
        refreshInFlight = null;
      });
  }
  return refreshInFlight;
}

/**
 * authorizedFetch sends one request with the bearer token, renewing the session
 * when the token in hand is spent and once more when the API refuses it.
 *
 * This is what keeps an idle tab signed in. The access token lives fifteen
 * minutes and the refresh cookie thirty days, so a tab left open holds a token
 * the API will reject long before the session is over; without a renewal here the
 * first call after a break comes back 401 and the screen treats it as a dead
 * session. Renewing before the call, and once on a 401, turns that into a
 * fresh token instead of the sign-in page.
 *
 * `headers` is a factory rather than a value because the retry must carry the
 * token the renewal just stored. At most one renewal is spent per request: a
 * second 401 is the API's answer, not a race to keep retrying.
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
 * What a call that ran past its deadline reports.
 *
 * An abandoned request is not a request that did not happen: the server may have
 * finished the work and lost only the answer, which for sign-up means the account
 * exists. So the wording tells the person what to do about that rather than
 * inviting a second attempt that would be refused as a duplicate.
 */
const TIMEOUT_MESSAGE = "Server terlalu lama menjawab. Akunmu mungkin sudah terbuat — coba masuk.";

/** request performs one API call and normalises the envelope. */
async function request<T>(path: string, options: RequestOptions = {}): Promise<ApiResult<T>> {
  const authenticated = options.authenticated !== false;
  const buildHeaders = () => {
    const headers = apiHeaders(options.workspace ?? null);
    if (!authenticated) {
      delete headers.Authorization;
    }
    return headers;
  };

  // The deadline is an abort signal rather than a race against a timer, because
  // the socket is what has to be let go: `fetch` has no timeout of its own, and a
  // server that holds the connection open would otherwise keep this promise — and
  // the button awaiting it — pending indefinitely.
  const controller = options.timeoutMs === undefined ? null : new AbortController();
  const timer = controller === null ? null : setTimeout(() => controller.abort(), options.timeoutMs);

  try {
    const response = await authorizedFetch(
      `${API_URL}/api${path}`,
      {
        method: options.method ?? "GET",
        credentials: "include",
        body: options.body === undefined ? undefined : JSON.stringify(options.body),
        cache: "no-store",
        signal: controller?.signal,
      },
      buildHeaders,
      authenticated,
    );

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

    const header = response.headers.get("Date");
    const serverDate = header === null ? Number.NaN : Date.parse(header);
    return {
      ok: true,
      status: response.status,
      data: envelope.data as T,
      meta: envelope.meta,
      serverDate: Number.isFinite(serverDate) ? serverDate : undefined,
    };
  } catch {
    // The signal is what tells an abandoned call apart from one the network
    // refused: both arrive here as a rejected fetch, and only the first has a
    // deadline behind it.
    if (controller?.signal.aborted) {
      return { ok: false, status: 0, code: "timeout", message: TIMEOUT_MESSAGE };
    }
    return {
      ok: false,
      status: 0,
      message: "Tidak bisa menghubungi server. Cek koneksimu lalu coba lagi.",
    };
  } finally {
    if (timer !== null) clearTimeout(timer);
  }
}

/** register creates a password account and emails a verification link. */
export function register(email: string, password: string): Promise<ApiResult<User>> {
  // Sign-up is a person watching a button, and it creates an account on the
  // server, so it may not hang: the deadline is what turns a backend that never
  // answers into something the form can explain.
  return request<User>("/auth/register", {
    method: "POST",
    body: { email, password },
    authenticated: false,
    timeoutMs: 20_000,
  });
}

/** verifyEmail consumes the one-time verification token. */
export function verifyEmail(token: string): Promise<ApiResult<User>> {
  return request<User>("/auth/verify-email", { method: "POST", body: { token }, authenticated: false });
}

/** resendVerification asks for a new verification link. */
export function resendVerification(email: string): Promise<ApiResult<null>> {
  return request<null>("/auth/resend-verification", { method: "POST", body: { email }, authenticated: false });
}

/** login signs in and stores the access token in memory. */
export async function login(email: string, password: string): Promise<ApiResult<Session>> {
  const result = await request<Session>("/auth/login", {
    method: "POST",
    body: { email, password },
    authenticated: false,
    timeoutMs: 20_000,
  });
  if (result.ok) {
    setAccessToken(result.data.access_token, result.data.access_expires_at);
  }
  return result;
}

/**
 * ensureActiveWorkspace returns the workspace this tab is working in, choosing one
 * when the browser has none stored.
 *
 * The id lives in local storage, and only the onboarding form ever wrote it. That
 * made "which workspace am I in" a property of one browser: signing in on a second
 * device, or in a private window, left every screen saying "Pilih workspace dulu"
 * with no way out — the account owned a workspace the app could not see. The
 * session is refreshed first because choosing a workspace needs a token, and the
 * first workspace is the right default for an account that owns one.
 */
export async function ensureActiveWorkspace(): Promise<string | null> {
  const stored = getActiveWorkspaceId();

  const session = await refreshSession();
  if (!session.ok) {
    return null;
  }

  // The remembered id is checked against the account's own workspaces, because
  // it outlives the session: signing in as somebody else in the same browser
  // otherwise leaves the app asking for a workspace that account is not a member
  // of, and answering with the API's own words rather than with a workspace.
  const result = await listWorkspaces();
  if (!result.ok || result.data.length === 0) {
    return stored && stored !== "" ? stored : null;
  }

  if (stored && result.data.some((workspace) => workspace.id === stored)) {
    return stored;
  }

  const workspace = result.data[0].id;
  setActiveWorkspaceId(workspace);
  return workspace;
}

/** logout revokes the session and clears the cookie. */
export async function logout(): Promise<ApiResult<null>> {
  const result = await request<null>("/auth/logout", { method: "POST" });
  setAccessToken(null);
  clearActiveWorkspaceId();
  return result;
}

/** forgotPassword emails a reset link. */
export function forgotPassword(email: string): Promise<ApiResult<null>> {
  return request<null>("/auth/password/forgot", { method: "POST", body: { email }, authenticated: false });
}

/** resetPassword consumes the reset token and stores the new password. */
export function resetPassword(token: string, password: string): Promise<ApiResult<null>> {
  return request<null>("/auth/password/reset", { method: "POST", body: { token, password }, authenticated: false });
}

/** currentUser returns the signed-in account. */
export function currentUser(): Promise<ApiResult<User>> {
  return request<User>("/auth/me");
}

/** googleStartURL is where the browser goes to sign in with Google. */
export function googleStartURL(): string {
  return `${API_URL}/api/auth/google/start`;
}

/** onboard creates the workspace and returns it with the default team. */
export function onboard(input: {
  name: string;
  business_field: string;
  timezone: string;
}): Promise<ApiResult<OnboardResult>> {
  return request<OnboardResult>("/workspaces/onboard", { method: "POST", body: input });
}

/** listWorkspaces returns every workspace the account belongs to. */
export function listWorkspaces(): Promise<ApiResult<Workspace[]>> {
  return request<Workspace[]>("/workspaces");
}

/** currentWorkspace returns the active workspace. */
export function currentWorkspace(workspaceId: string): Promise<ApiResult<Workspace>> {
  return request<Workspace>("/workspaces/current", { workspace: workspaceId });
}

/** updateWorkspace changes the workspace profile (owner only). */
export function updateWorkspace(
  workspaceId: string,
  input: {
    name: string;
    business_field: string;
    timezone: string;
    language?: string;
    work_hours_start?: string;
    work_hours_end?: string;
    work_days?: number[];
  },
): Promise<ApiResult<Workspace>> {
  return request<Workspace>("/workspaces/current", { method: "PATCH", body: input, workspace: workspaceId });
}

/** listTeams returns the teams of the active workspace. */
export function listTeams(workspaceId: string): Promise<ApiResult<Team[]>> {
  return request<Team[]>("/workspaces/current/teams", { workspace: workspaceId });
}

/** listMembers returns the members of the active workspace. */
export function listMembers(workspaceId: string): Promise<ApiResult<Member[]>> {
  return request<Member[]>("/workspaces/current/members", { workspace: workspaceId });
}

/** setMemberRole changes a member's role. */
export function setMemberRole(workspaceId: string, userId: string, role: string): Promise<ApiResult<Member>> {
  return request<Member>(`/workspaces/current/members/${userId}`, {
    method: "PATCH",
    body: { role },
    workspace: workspaceId,
  });
}

/** removeMember removes a member from the active workspace. */
export function removeMember(workspaceId: string, userId: string): Promise<ApiResult<null>> {
  return request<null>(`/workspaces/current/members/${userId}`, { method: "DELETE", workspace: workspaceId });
}

/** listInvitations returns the pending invitations of the active workspace. */
export function listInvitations(workspaceId: string): Promise<ApiResult<Invitation[]>> {
  return request<Invitation[]>("/workspaces/current/invitations", { workspace: workspaceId });
}

/** invite sends an invitation to join the active workspace. */
export function invite(workspaceId: string, email: string, role: string): Promise<ApiResult<Invitation>> {
  return request<Invitation>("/workspaces/current/invitations", {
    method: "POST",
    body: { email, role },
    workspace: workspaceId,
  });
}

/** revokeInvitation deletes a pending invitation. */
export function revokeInvitation(workspaceId: string, invitationId: string): Promise<ApiResult<null>> {
  return request<null>(`/workspaces/current/invitations/${invitationId}`, {
    method: "DELETE",
    workspace: workspaceId,
  });
}

/** acceptInvitation joins the signed-in account to the inviting workspace. */
export function acceptInvitation(token: string): Promise<ApiResult<Workspace>> {
  return request<Workspace>("/invitations/accept", { method: "POST", body: { token } });
}

export { API_URL };

// ---------------------------------------------------------------- agent registry

export type Agent = {
  id: string;
  team_id: string;
  team_name: string;
  team_kind: string;
  name: string;
  role: string;
  persona: string;
  tone: string;
  shape: string;
  color: string;
  /** The stored switch: active or resting. */
  status: string;
  /** Derived from tasks and drafts, never stored: working | waiting | idle | resting. */
  display_status: string;
  /** Where the Bolu sits in its team's list, counting from zero. */
  position: number;
  reason?: { kind: string; id: string; title?: string; status?: string; created_at: string };
  template_key?: string;
  tools: string[];
  default_model: Record<string, unknown>;
  /** Whether a quiet conversation is read into this Bolu's memory. */
  memory_extraction_enabled: boolean;
  created_at: string;
};

export type AgentTeam = {
  id: string;
  name: string;
  kind: string;
  agents: Agent[];
};

export type AgentTemplate = {
  key: string;
  name: string;
  role: string;
  persona: string;
  tone: string;
  shape: string;
  color: string;
  tools: string[];
  integrations: string[];
};

export type Tool = {
  name: string;
  integration_app: string;
  label: string;
  description: string;
};

export type Grant = {
  id: string;
  agent_id: string;
  integration_id: string;
  app: string;
  account_label: string;
  status: string;
  permission: string;
};

export type Integration = {
  id: string;
  app: string;
  account_label: string;
  status: string;
};

/** listAgentTeams returns the workspace's teams with their agents. */
export function listAgentTeams(workspaceId: string): Promise<ApiResult<AgentTeam[]>> {
  return request<AgentTeam[]>("/teams", { workspace: workspaceId });
}

/** listTemplates returns the seeded Bolu profiles. */
export function listTemplates(): Promise<ApiResult<AgentTemplate[]>> {
  return request<AgentTemplate[]>("/agents/templates");
}

/** listIntegrations returns the workspace's connected applications. */
export function listIntegrations(workspaceId: string): Promise<ApiResult<Integration[]>> {
  return request<Integration[]>("/agents/integrations", { workspace: workspaceId });
}

/** createAgent adds a Bolu, from a template, from a copy, or from scratch. */
export function createAgent(
  workspaceId: string,
  input: {
    team_id: string;
    name?: string;
    template_key?: string;
    copy_from?: string;
    persona?: string;
    role?: string;
    /** Body shape, named from the generator's own set. */
    shape?: string;
    /** Body colour; feet and ink are derived from it. */
    color?: string;
  },
): Promise<ApiResult<Agent>> {
  return request<Agent>("/agents", { method: "POST", body: input, workspace: workspaceId });
}

/** updateAgent edits a Bolu profile. */
export function updateAgent(
  workspaceId: string,
  agentId: string,
  input: { name: string; role?: string; persona?: string; tone?: string; tools?: string[] },
): Promise<ApiResult<Agent>> {
  return request<Agent>(`/agents/${agentId}`, { method: "PATCH", body: input, workspace: workspaceId });
}

/** setAgentStatus flips the rest switch. */
export function setAgentStatus(workspaceId: string, agentId: string, status: string): Promise<ApiResult<Agent>> {
  return request<Agent>(`/agents/${agentId}/status`, { method: "PATCH", body: { status }, workspace: workspaceId });
}

/** deleteAgent removes a Bolu from the registry, keeping its history. */
export function deleteAgent(workspaceId: string, agentId: string): Promise<ApiResult<null>> {
  return request<null>(`/agents/${agentId}`, { method: "DELETE", workspace: workspaceId });
}

/** listAgentGrants returns the integrations one Bolu may use. */
export function listAgentGrants(workspaceId: string, agentId: string): Promise<ApiResult<Grant[]>> {
  return request<Grant[]>(`/agents/${agentId}/grants`, { workspace: workspaceId });
}

/** setAgentGrant gives a Bolu read or read-write access to an integration. */
export function setAgentGrant(
  workspaceId: string,
  agentId: string,
  integrationId: string,
  permission: string,
): Promise<ApiResult<Grant>> {
  return request<Grant>(`/agents/${agentId}/grants`, {
    method: "PUT",
    body: { integration_id: integrationId, permission },
    workspace: workspaceId,
  });
}

/** listAgentTools returns the tools a Bolu may actually call. */
export function listAgentTools(workspaceId: string, agentId: string): Promise<ApiResult<Tool[]>> {
  return request<Tool[]>(`/agents/${agentId}/tools`, { workspace: workspaceId });
}

// ---------------------------------------------------------------- model gateway

/** One configured route to a model. The API never returns the secret itself. */
export type ModelProvider = {
  id: string;
  name: string;
  /** openai or anthropic: which wire format the endpoint speaks. */
  adapter: string;
  base_url: string;
  model: string;
  /** Lower runs first in the fallback chain. */
  priority: number;
  max_tokens: number;
  context_tokens: number;
  is_default: boolean;
  enabled: boolean;
  has_api_key: boolean;
  created_at: string;
  updated_at: string;
};

export type ModelProviderInput = {
  name: string;
  adapter: string;
  base_url?: string;
  model: string;
  /** Empty keeps the stored key; the screen never receives it back. */
  api_key?: string;
  clear_api_key?: boolean;
  priority?: number;
  max_tokens?: number;
  context_tokens?: number;
  is_default?: boolean;
  enabled?: boolean;
};

export type ProviderCapabilities = {
  tools: boolean;
  vision: boolean;
  streaming: boolean;
  prompt_caching: boolean;
  parallel_tool_calls: boolean;
  max_context_tokens: number;
};

/** One day of aggregated spend, which the model screen shows next to the quota. */
export type UsageDay = {
  day: string;
  provider: string;
  model: string;
  purpose: string;
  calls: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  total_tokens: number;
  /** Indonesian micro-rupiah: 1 rupiah is 1,000,000. */
  cost_micros: number;
};

export type AgentModelOverride = {
  provider_id?: string;
  adapter?: string;
  base_url?: string;
  model?: string;
  max_tokens?: number;
  has_api_key: boolean;
};

/** listModelProviders returns the workspace providers in fallback order. */
export function listModelProviders(workspaceId: string): Promise<ApiResult<ModelProvider[]>> {
  return request<ModelProvider[]>("/llm/providers", { workspace: workspaceId });
}

/**
 * listModelAdapters returns the wire formats the backend can serve.
 *
 * The route sits in the workspace guard with the rest of the model screen's reads, so the active
 * workspace travels with the call. Without it the backend answers 400 `invalid_workspace`, which the
 * caller reads as "the adapters are unavailable" and silently replaces with the fallback list.
 */
export function listModelAdapters(workspaceId: string): Promise<ApiResult<{ adapters: string[] }>> {
  return request<{ adapters: string[] }>("/llm/adapters", { workspace: workspaceId });
}

/** createModelProvider adds a provider to the fallback chain. */
export function createModelProvider(
  workspaceId: string,
  input: ModelProviderInput,
): Promise<ApiResult<ModelProvider>> {
  return request<ModelProvider>("/llm/providers", { method: "POST", body: input, workspace: workspaceId });
}

/** updateModelProvider edits a provider. An empty api_key keeps the stored one. */
export function updateModelProvider(
  workspaceId: string,
  providerId: string,
  input: ModelProviderInput,
): Promise<ApiResult<ModelProvider>> {
  return request<ModelProvider>(`/llm/providers/${providerId}`, {
    method: "PUT",
    body: input,
    workspace: workspaceId,
  });
}

/** deleteModelProvider removes a provider from the chain. */
export function deleteModelProvider(workspaceId: string, providerId: string): Promise<ApiResult<null>> {
  return request<null>(`/llm/providers/${providerId}`, { method: "DELETE", workspace: workspaceId });
}

/** testModelProvider calls the provider once, which is how a key is proven. */
export function testModelProvider(
  workspaceId: string,
  providerId: string,
): Promise<ApiResult<ProviderCapabilities>> {
  return request<ProviderCapabilities>(`/llm/providers/${providerId}/test`, {
    method: "POST",
    workspace: workspaceId,
  });
}

/** modelUsage returns the aggregated spend of the last days, newest first. */
export function modelUsage(workspaceId: string, days = 30): Promise<ApiResult<UsageDay[]>> {
  return request<UsageDay[]>(`/llm/usage?days=${days}`, { workspace: workspaceId });
}

/** getAgentModel reads one Bolu's model override. */
export function getAgentModel(workspaceId: string, agentId: string): Promise<ApiResult<AgentModelOverride>> {
  return request<AgentModelOverride>(`/agents/${agentId}/model`, { workspace: workspaceId });
}

/**
 * setAgentModel stores one Bolu's override.
 *
 * An empty provider_id with an empty model clears the override, which puts the
 * Bolu back on the workspace chain.
 */
export function setAgentModel(
  workspaceId: string,
  agentId: string,
  input: { provider_id?: string; adapter?: string; base_url?: string; model?: string; api_key?: string; max_tokens?: number },
): Promise<ApiResult<AgentModelOverride>> {
  return request<AgentModelOverride>(`/agents/${agentId}/model`, {
    method: "PUT",
    body: input,
    workspace: workspaceId,
  });
}

// ------------------------------------------------------------------ conversations

/** One content block of a message. The document is what the backend validated. */
export type TextBlock = { type: "text"; markdown: string; title?: string };

export type TableBlock = {
  type: "table";
  title?: string;
  columns: string[];
  rows: string[][];
  align?: ("left" | "right" | "center")[];
};

export type DraftBlock = {
  type: "draft";
  draft_id: string;
  action_kind?: string;
  title?: string;
  summary?: string;
  status?: "pending" | "approved" | "revise" | "sent" | "canceled";
  fields?: [string, string][];
  /** Why an approved action has not gone out, in the user's words. */
  blocked_reason?: string;
  /** Until when a held action waits, which is the workspace's next open window. */
  not_before?: string;
  /** How many times a person has sent this action back. */
  revision?: number;
};

export type ChartSpec = {
  kind: "bar" | "line" | "area" | "pie" | "scatter";
  title?: string;
  x_label?: string;
  y_label?: string;
  unit?: string;
  categories?: string[];
  series?: { name: string; data: number[] }[];
  slices?: { name: string; value: number }[];
  points?: { x: number | string; y: number }[];
  stacked?: boolean;
};

export type ChartBlock = { type: "chart"; title?: string; spec: ChartSpec };

export type MermaidBlock = {
  type: "mermaid";
  title?: string;
  code: string;
  diagram?: string;
};

export type HTMLBlock = {
  type: "html";
  title?: string;
  caption?: string;
  /** The storage key the content origin resolves. */
  content_ref: string;
  byte_size: number;
};

export type MessageBlock =
  | TextBlock
  | TableBlock
  | DraftBlock
  | ChartBlock
  | MermaidBlock
  | HTMLBlock;

/** A reply that failed or was cut short keeps what arrived, marked. */
export type MessageStatus = "complete" | "streaming" | "partial" | "failed";

export type Attachment = {
  id: string;
  filename: string;
  content_type: string;
  byte_size: number;
  url: string;
};

export type ChatMessage = {
  /** The server's own field name: it is what the stream's events carry too, so
   *  the same message from history and from an event compares equal. */
  message_id: string;
  conversation_id: string;
  agent_id?: string;
  user_id?: string;
  blocks: MessageBlock[];
  attachments: Attachment[];
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
  /** A thread is work a Bolu opened from another conversation; it names that one. */
  kind: "direct" | "group" | "thread";
  title: string;
  parent_conversation_id?: string;
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
  /** The Bolu that will answer: a group message can address more than one. */
  responders?: ChatParticipant[];
  routed: boolean;
};

/** One row of the activity stream. `id` is what a reconnect resumes from. */
export type ActivityEvent = {
  id: number;
  type: string;
  workspace_id: string;
  agent_id?: string;
  user_id?: string;
  conversation_id?: string;
  task_id?: string;
  draft_id?: string;
  payload?: Record<string, unknown>;
  created_at: string;
};

export type MessageEventPayload = {
  message_id: string;
  conversation_id: string;
  agent_id?: string;
  user_id?: string;
  status: MessageStatus;
  blocks: MessageBlock[];
  /**
   * The files on the message. The stream carries them, so a file uploaded in
   * another tab — or by the server itself — reaches this one without a reload.
   * Optional because an event written before the field existed has none.
   */
  attachments?: Attachment[];
  finish_reason?: string;
};

export type AgentStatePayload = {
  agent_id: string;
  state: "working" | "waiting" | "idle" | "resting" | "thinking";
  reason?: string;
  task_id?: string;
  draft_id?: string;
};

/** listConversations returns the threads of the workspace, most recent first. */
export function listConversations(workspaceId: string): Promise<ApiResult<ChatConversation[]>> {
  return request<ChatConversation[]>("/conversations", { workspace: workspaceId });
}

/** openDirectConversation returns the 1:1 thread with a Bolu, creating it once. */
export function openDirectConversation(
  workspaceId: string,
  agentId: string,
): Promise<ApiResult<ChatConversation>> {
  return request<ChatConversation>("/conversations/direct", {
    method: "POST",
    body: { agent_id: agentId },
    workspace: workspaceId,
  });
}

/**
 * arrangeAgents writes the sidebar's order.
 *
 * The whole layout is sent rather than one move: the positions are the indexes of
 * the lists, so the same body sent twice leaves the same sidebar and two tabs
 * cannot interleave. The answer is the stored teams, which is what the screen
 * renders next — a drag is a guess about a list, and the workspace is the one that
 * decides where things landed.
 */
export function arrangeAgents(
  workspaceId: string,
  teams: { team_id: string; agent_ids: string[] }[],
): Promise<ApiResult<AgentTeam[]>> {
  return request<AgentTeam[]>("/agents/order", {
    method: "PUT",
    body: { teams },
    workspace: workspaceId,
  });
}

/** createGroup makes a group with the named Bolu and members. */
export function createGroup(
  workspaceId: string,
  input: { title: string; agent_ids: string[]; user_ids?: string[] },
): Promise<ApiResult<ChatConversation>> {
  return request<ChatConversation>("/conversations/groups", {
    method: "POST",
    body: input,
    workspace: workspaceId,
  });
}

/** addGroupParticipants adds Bolu and members to a group. */
export function addGroupParticipants(
  workspaceId: string,
  conversationId: string,
  input: { agent_ids?: string[]; user_ids?: string[] },
): Promise<ApiResult<ChatConversation>> {
  return request<ChatConversation>(`/conversations/${conversationId}/participants`, {
    method: "POST",
    body: input,
    workspace: workspaceId,
  });
}

/** getConversation returns one thread with its participants. */
export function getConversation(
  workspaceId: string,
  conversationId: string,
): Promise<ApiResult<ChatConversation>> {
  return request<ChatConversation>(`/conversations/${conversationId}`, { workspace: workspaceId });
}

/** listMessages reads one page of history, newest first. */
export function listMessages(
  workspaceId: string,
  conversationId: string,
  cursor?: string,
  limit = 40,
): Promise<ApiResult<MessagePage>> {
  const query = new URLSearchParams({ limit: String(limit) });
  if (cursor) {
    query.set("cursor", cursor);
  }
  return request<MessagePage>(`/conversations/${conversationId}/messages?${query}`, {
    workspace: workspaceId,
  });
}

/**
 * sendMessage stores a message and starts the reply.
 *
 * The answer arrives on the event stream rather than in this response, so
 * closing the tab does not lose it.
 */
export function sendMessage(
  workspaceId: string,
  conversationId: string,
  input: { text: string; reply?: boolean; agent_id?: string; attachment_ids?: string[] },
): Promise<ApiResult<SendMessageResult>> {
  return request<SendMessageResult>(`/conversations/${conversationId}/messages`, {
    method: "POST",
    body: input,
    workspace: workspaceId,
  });
}

/** listEvents replays the stream after an event id, which is how a reconnect fills the gap. */
export function listEvents(
  workspaceId: string,
  afterId: number,
  limit = 200,
): Promise<ApiResult<ActivityEvent[]>> {
  return request<ActivityEvent[]>(`/events?after_id=${afterId}&limit=${limit}`, {
    workspace: workspaceId,
  });
}

/**
 * One file the API holds until the message that names it is stored.
 *
 * Its id is what a send carries; the bytes are already in storage, so the
 * message and its files are written in one transaction and the reply sees them
 * on the turn they were sent with.
 */
export type StagedAttachment = {
  attachment_id: string;
  name: string;
  mime: string;
  size: number;
  url: string;
};

/**
 * uploadStagedAttachment stores a file before the message that will carry it.
 *
 * XMLHttpRequest rather than `fetch`, because only it reports how far an upload
 * has got: a person watching a large file over a slow line needs to see it
 * moving, and a stalled upload otherwise looks exactly like a slow one.
 *
 * The file belongs to no message yet, so the API holds it for a day and the ids
 * this returns travel in the body of the send that claims them. Aborting through
 * `signal` drops the chip's upload; the request is answered with `code:
 * "aborted"` rather than a rejection, like every other failure here.
 */
export async function uploadStagedAttachment(
  workspaceId: string,
  conversationId: string,
  file: File,
  onProgress?: (fraction: number) => void,
  signal?: AbortSignal,
): Promise<ApiResult<StagedAttachment>> {
  const form = new FormData();
  form.append("file", file);

  // No Content-Type: the browser sets it with the multipart boundary.
  const buildHeaders = () => {
    const headers: Record<string, string> = { "X-Workspace-Id": workspaceId };
    if (accessToken) {
      headers.Authorization = `Bearer ${accessToken}`;
    }
    return headers;
  };

  // The renewal the other calls get from `authorizedFetch`: an upload started
  // after an idle spell would otherwise be refused with a 401.
  if (tokenIsSpent()) {
    await refreshSession();
  }
  if (signal?.aborted) {
    return ABORTED_UPLOAD;
  }

  const url = `${API_URL}/api/conversations/${conversationId}/attachments?staged=true`;
  let attempt = await sendUpload<StagedAttachment>(url, form, buildHeaders(), onProgress, signal);
  if (attempt.kind === "response" && attempt.status === 401) {
    const renewal = await refreshSession();
    if (renewal.ok && !signal?.aborted) {
      attempt = await sendUpload<StagedAttachment>(url, form, buildHeaders(), onProgress, signal);
    }
  }

  if (attempt.kind === "aborted") {
    return ABORTED_UPLOAD;
  }
  if (attempt.kind === "transport") {
    return { ok: false, status: 0, message: "Tidak bisa menghubungi server. Cek koneksimu lalu coba lagi." };
  }

  const { status, envelope } = attempt;
  if (status < 200 || status >= 300 || !envelope?.success || !envelope.data) {
    return {
      ok: false,
      status,
      message: envelope?.message ?? "Gagal mengunggah berkas.",
      code: envelope?.error?.code,
      details: envelope?.error?.details,
    };
  }
  return { ok: true, status, data: envelope.data };
}

/** What a call abandoned by its own caller reports. */
const ABORTED_UPLOAD: ApiError = {
  ok: false,
  status: 0,
  code: "aborted",
  message: "Unggahan dibatalkan.",
};

/** One upload attempt: what the XHR answered, or how it ended without an answer. */
type UploadAttempt<T> =
  | { kind: "response"; status: number; envelope: Envelope<T> | null }
  | { kind: "transport" }
  | { kind: "aborted" };

/**
 * sendUpload performs one multipart POST and reports how far it has got.
 *
 * This is the piece `fetch` cannot do: `xhr.upload` fires while the bytes are
 * leaving, which is the only moment a person can tell an upload from a hang.
 */
function sendUpload<T>(
  url: string,
  form: FormData,
  headers: Record<string, string>,
  onProgress: ((fraction: number) => void) | undefined,
  signal: AbortSignal | undefined,
): Promise<UploadAttempt<T>> {
  const { promise, resolve } = Promise.withResolvers<UploadAttempt<T>>();
  const xhr = new XMLHttpRequest();
  xhr.open("POST", url);
  xhr.withCredentials = true;
  for (const [name, value] of Object.entries(headers)) {
    xhr.setRequestHeader(name, value);
  }

  if (onProgress) {
    xhr.upload.onprogress = (event) => {
      // A length the browser cannot compute is not a progress report.
      if (event.lengthComputable && event.total > 0) {
        onProgress(Math.min(1, event.loaded / event.total));
      }
    };
  }

  const onAbort = () => xhr.abort();
  signal?.addEventListener("abort", onAbort);
  const settle = (attempt: UploadAttempt<T>) => {
    signal?.removeEventListener("abort", onAbort);
    resolve(attempt);
  };

  xhr.onload = () => {
    let envelope: Envelope<T> | null = null;
    try {
      envelope = xhr.responseText ? (JSON.parse(xhr.responseText) as Envelope<T>) : null;
    } catch {
      envelope = null;
    }
    settle({ kind: "response", status: xhr.status, envelope });
  };
  xhr.onerror = () => settle({ kind: "transport" });
  xhr.onabort = () => settle({ kind: "aborted" });

  xhr.send(form);
  return promise;
}

/** contentURL is where one sandboxed HTML document is served from. */
export function contentURL(reference: string): string {
  const origin = process.env.NEXT_PUBLIC_CONTENT_ORIGIN ?? API_URL;
  return `${origin}/content/${reference}`;
}

/* ----------------------------------------------------------------- approvals */

/**
 * A draft is a proposed action that reaches outside Bolu, waiting for a person.
 *
 * The payload is the action's own shape, which is what the card shows: the
 * frontend never interprets it beyond rendering the fields the backend already
 * labelled.
 */
export type Draft = {
  draft_id: string;
  task_id: string;
  agent_id: string;
  conversation_id?: string;
  action_kind: string;
  title: string;
  summary?: string;
  status: "pending" | "approved" | "revise" | "sent" | "canceled";
  revision: number;
  payload: Record<string, unknown>;
  decided_by?: string;
  decided_at?: string;
  decision_note?: string;
  blocked_reason?: string;
  not_before?: string;
  sent_at?: string;
  card_message_id?: string;
  idempotency_key?: string;
  created_at: string;
};

/** A revision is one turn of the trail: what the person said, and what they saw. */
export type DraftRevision = {
  revision: number;
  note: string;
  payload: Record<string, unknown>;
  decided_by?: string;
  created_at: string;
};

/**
 * A policy is what a workspace allows unattended for one kind of action.
 *
 * `auto` carries the action out without asking, but only while its value is
 * within `max_amount`. An absent ceiling means the workspace trusts the kind
 * entirely; an absent amount on the action means the ceiling cannot be met, so a
 * person is asked.
 */
export type ApprovalPolicy = {
  action_kind: string;
  mode: "always_draft" | "auto";
  max_amount: number | null;
  updated_by?: string;
  updated_at?: string;
};

/** An audit entry is one thing that happened, for the record. */
export type AuditEntry = {
  id: number;
  action: string;
  object_type: string;
  object_id: string;
  actor_user_id?: string;
  actor_agent_id?: string;
  metadata: Record<string, unknown>;
  created_at: string;
};

/**
 * A task is one unit of work a Bolu was given. `health` is derived from the
 * heartbeat, so a task whose worker died reads different from a working one even
 * though both store `running`.
 */
export type Task = {
  task_id: string;
  agent_id: string;
  parent_task_id?: string;
  conversation_id?: string;
  trigger: "chat" | "routine" | "webhook" | "handoff";
  title: string;
  status: string;
  health: "running" | "waiting" | "stuck" | "done";
  depth: number;
  summary?: string;
  stopped_reason?: string;
  waiting_reason?: string;
  waiting_draft_id?: string;
  workflow_id?: string;
  /** The run of that workflow, which is what a Temporal UI link needs to open the right one. */
  temporal_run_id?: string;
  step_count: number;
  input_tokens: number;
  output_tokens: number;
  cost_micros: number;
  started_at?: string;
  finished_at?: string;
  heartbeat_at?: string;
  created_at: string;
};

/** listTasks returns the workspace's task history, newest first. */
export function listTasks(
  workspaceId: string,
  filter: { status?: string; live?: boolean; agent_id?: string; limit?: number } = {},
): Promise<ApiResult<Task[]>> {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(filter)) {
    if (value !== undefined && value !== "" && value !== false) query.set(key, String(value));
  }
  const suffix = query.toString() ? `?${query}` : "";
  return request<Task[]>(`/tasks${suffix}`, { workspace: workspaceId });
}

/** A task that needs a person, with the link that diagnoses it. */
export type AttentionTask = Task & {
  /** Why it is on the list: it failed, it is stuck, or it is waiting on a decision. */
  attention: "failed" | "stuck" | "waiting";
  /**
   * Opens the task's workflow in this deployment's Temporal UI.
   *
   * Built by the server, because the deployment is the only thing that knows which Temporal it
   * has: a compose port, a Cloud host, or none at all. Empty means there is no UI to open.
   */
  temporal_url?: string;
};

/** listAttentionTasks returns the work that needs a person: failed, stuck, and waiting. */
export function listAttentionTasks(
  workspaceId: string,
  limit = 50,
): Promise<ApiResult<AttentionTask[]>> {
  return request<AttentionTask[]>(`/tasks/attention?limit=${limit}`, { workspace: workspaceId });
}

/** getTask returns one task by its id. */
export function getTask(workspaceId: string, taskID: string): Promise<ApiResult<Task>> {
  return request<Task>(`/tasks/${taskID}`, { workspace: workspaceId });
}

/** listTaskChildren returns the tasks this one handed work to, which is how a chain is read. */
export function listTaskChildren(workspaceId: string, taskID: string): Promise<ApiResult<Task[]>> {
  return request<Task[]>(`/tasks/${taskID}/children`, { workspace: workspaceId });
}

/**
 * cancelTask stops a task that has not finished.
 *
 * What comes back is the task as it now is, not an acknowledgement: a caller that
 * shows "canceled" without reading the answer would show it even when the task had
 * already succeeded and the call changed nothing.
 */
export function cancelTask(workspaceId: string, taskID: string): Promise<ApiResult<Task>> {
  return request<Task>(`/tasks/${taskID}/cancel`, { method: "POST", workspace: workspaceId });
}

/**
 * A step is one recorded round of a task: the model thinking, a tool being called,
 * a tool answering, or the final answer.
 *
 * `pruned` means the round's contents were removed by the retention job — its shape
 * stays so the history still reads as a history, and `input`/`output` are absent
 * rather than empty. A screen must show that difference: an empty round and one
 * whose payload was deliberately removed are two different facts.
 */
export type TaskStep = {
  step_id: string;
  seq: number;
  kind: "think" | "tool_call" | "tool_result" | "final";
  tool_name?: string;
  tool_label?: string;
  input?: unknown;
  output?: unknown;
  pruned: boolean;
  created_at: string;
  input_tokens: number;
  output_tokens: number;
};

/** listTaskSteps returns a task's rounds, in the order they happened. */
export function listTaskSteps(workspaceId: string, taskID: string): Promise<ApiResult<TaskStep[]>> {
  return request<TaskStep[]>(`/tasks/${taskID}/steps`, { workspace: workspaceId });
}

/**
 * A plan a Bolu keeps for itself: a short list of tasks, each with optional subtasks.
 *
 * It is read-only from here. The Bolu writes, ticks, and throws its list away through its own tools,
 * and a screen that could tick a line could make it believe work was done that nobody did.
 */
export type Checklist = {
  checklist_id: string;
  agent_id: string;
  /** The task the Bolu was working on when it wrote the plan down, when there was one. */
  task_id?: string;
  title: string;
  status: "active" | "done";
  /** How much of the plan is ticked, counted over tasks and subtasks together. */
  done_count: number;
  total_count: number;
  items: ChecklistItem[];
  created_at: string;
  completed_at?: string;
};

/** One line of a plan: a task, or a subtask of one. A subtask is never nested further. */
export type ChecklistItem = {
  item_id: string;
  title: string;
  done: boolean;
  subtasks?: ChecklistItem[];
};

/** listChecklists returns the plans Bolus keep, newest first. */
export function listChecklists(
  workspaceId: string,
  filter: { agent_id?: string; status?: string; limit?: number } = {},
): Promise<ApiResult<Checklist[]>> {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(filter)) {
    if (value !== undefined && value !== "") query.set(key, String(value));
  }
  const suffix = query.toString() ? `?${query}` : "";
  return request<Checklist[]>(`/checklists${suffix}`, { workspace: workspaceId });
}

/** What one window cost, read three ways. */
export type UsageReport = {
  days: number;
  /** The first moment the window covers, in the workspace's own timezone. */
  period_started_at: string;
  totals: UsageTotals;
  by_agent: UsageAgentRow[];
  by_task: UsageTaskRow[];
};

export type UsageTotals = {
  calls: number;
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  cost_micros: number;
  /** The same figure in rupiah, so a screen does not have to know the unit. */
  cost_idr: number;
};

export type UsageAgentRow = {
  agent_id: string;
  agent_name: string;
  calls: number;
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
  cost_micros: number;
  cost_idr: number;
};

export type UsageTaskRow = {
  task_id: string;
  task_title: string;
  task_status: string;
  calls: number;
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
  cost_micros: number;
  cost_idr: number;
};

/** usageReport returns what the workspace spent over a window, read three ways. */
export function usageReport(
  workspaceId: string,
  days = 30,
  limit = 25,
): Promise<ApiResult<UsageReport>> {
  return request<UsageReport>(`/llm/usage/report?days=${days}&limit=${limit}`, { workspace: workspaceId });
}

/**
 * usageReportCSV downloads the same report as a file.
 *
 * It fetches rather than pointing an anchor at the URL, because the export is a tenant-scoped read
 * and the token travels in a header rather than in a cookie-only session: an `<a href>` would
 * download a 401. The file is built in the browser from the response, which is also what lets the
 * name carry the day.
 */
export async function usageReportCSV(workspaceId: string, days = 30): Promise<{ ok: boolean; message: string }> {
  try {
    const response = await authorizedFetch(
      `${API_URL}/api/llm/usage/report?days=${days}&format=csv`,
      { credentials: "include", cache: "no-store" },
      () => apiHeaders(workspaceId),
      true,
    );

    if (!response.ok) {
      return { ok: false, message: "Ekspor tidak bisa diunduh. Coba lagi." };
    }

    const blob = await response.blob();
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement("a");
    anchor.href = url;
    anchor.download = `pemakaian-${new Date().toISOString().slice(0, 10)}.csv`;
    document.body.append(anchor);
    anchor.click();
    anchor.remove();
    URL.revokeObjectURL(url);

    return { ok: true, message: "Berkas pemakaian diunduh." };
  } catch {
    return { ok: false, message: "Tidak bisa menghubungi server. Cek koneksimu lalu coba lagi." };
  }
}

/** The record of a workspace's data having been erased, which is what a person is owed. */
export type ErasureRecord = {
  workspace_id: string;
  workspace_name: string;
  requested_by: string;
  requested_by_email: string;
  requested_at: string;
  completed_at: string;
  objects_deleted: number;
  rows_deleted: number;
  complete: boolean;
};

/**
 * eraseWorkspaceData removes the workspace and everything in it, on the owner's instruction.
 *
 * It is deliberately the last call a workspace ever makes: the workspace does not exist when this
 * returns, so there is nothing left to address afterwards — which is why the answer is the evidence
 * rather than a job id.
 */
export function eraseWorkspaceData(
  workspaceId: string,
): Promise<ApiResult<{ workspace_id: string; objects: number; rows: number }>> {
  return request<{ workspace_id: string; objects: number; rows: number }>(
    "/workspaces/current/erasure",
    { method: "POST", workspace: workspaceId },
  );
}

/** findErasure reads a workspace's erasure record, when there is one. */
export function findErasure(workspaceId: string): Promise<ApiResult<ErasureRecord>> {
  return request<ErasureRecord>("/workspaces/current/erasure", { workspace: workspaceId });
}

/** listDrafts returns the workspace's drafts, newest first. */
export function listDrafts(
  workspaceId: string,
  filter: { status?: string; agent_id?: string; action_kind?: string; task_id?: string; limit?: number } = {},
): Promise<ApiResult<Draft[]>> {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(filter)) {
    if (value !== undefined && value !== "" && value !== 0) query.set(key, String(value));
  }
  const suffix = query.toString() ? `?${query}` : "";
  return request<Draft[]>(`/drafts${suffix}`, { workspace: workspaceId });
}

/** getDraft returns one draft. */
export function getDraft(workspaceId: string, draftId: string): Promise<ApiResult<Draft>> {
  return request<Draft>(`/drafts/${draftId}`, { workspace: workspaceId });
}

/** listDraftRevisions returns a draft's trail, oldest first. */
export function listDraftRevisions(workspaceId: string, draftId: string): Promise<ApiResult<DraftRevision[]>> {
  return request<DraftRevision[]>(`/drafts/${draftId}/revisions`, { workspace: workspaceId });
}

/**
 * decideDraft records a person's answer.
 *
 * Approving sends, revising returns the task to its agent with the note, and
 * cancelling stops it. The note is required for a revision: an agent sent back to
 * work with nothing to change would produce the same draft again.
 */
export function decideDraft(
  workspaceId: string,
  draftId: string,
  decision: "approve" | "revise" | "cancel",
  note = "",
): Promise<ApiResult<Draft>> {
  return request<Draft>(`/drafts/${draftId}/${decision}`, {
    method: "POST",
    body: { note },
    workspace: workspaceId,
  });
}

/** listApprovalPolicies returns the policy in force for every external action. */
export function listApprovalPolicies(workspaceId: string): Promise<ApiResult<ApprovalPolicy[]>> {
  return request<ApprovalPolicy[]>("/approval-policies", { workspace: workspaceId });
}

/** setApprovalPolicy stores a workspace's decision about one action kind. */
export function setApprovalPolicy(
  workspaceId: string,
  policy: { action_kind: string; mode: string; max_amount: number | null },
): Promise<ApiResult<ApprovalPolicy>> {
  return request<ApprovalPolicy>("/approval-policies", {
    method: "PUT",
    body: policy,
    workspace: workspaceId,
  });
}

/** listAudit returns the workspace's approval trail, newest first. */
export function listAudit(workspaceId: string, limit = 50): Promise<ApiResult<AuditEntry[]>> {
  return request<AuditEntry[]>(`/audit?limit=${limit}`, { workspace: workspaceId });
}

// ------------------------------------------------------- integration hub (EPIC 8)

/** A connected account, as the hub reports it. */
export type HubIntegration = {
  integration_id: string;
  app: string;
  account_label: string;
  status: "connected" | "needs_relogin" | "disconnected";
  status_reason?: string;
  scopes: string[];
  expires_at?: string;
  last_refreshed_at?: string;
};

/** One tool an application offers, with the label that decides approval. */
export type AdapterTool = {
  name: string;
  label: string;
  description: string;
  /** The label restated for a screen: whether a person is asked before it runs. */
  needs_human: boolean;
};

export type AdapterTrigger = {
  kind: string;
  event: string;
  description: string;
};

/** An application this deployment speaks, and what connecting it would give. */
export type HubAdapter = {
  app: string;
  oauth: boolean;
  default_scopes?: string[];
  tools: AdapterTool[];
  triggers?: AdapterTrigger[];
};

/** A Model Context Protocol server a workspace added. */
export type MCPServer = {
  id: string;
  app: string;
  label: string;
  endpoint: string;
  enabled: boolean;
  tool_count: number;
  /** Why the server could not be reached, so a screen can say so. */
  last_error?: string;
};

/** A Bolu's permission on one connection. */
export type HubGrant = {
  integration_id: string;
  permission: "read" | "read_write";
};

/** listHubIntegrations returns the workspace's connected accounts. */
export function listHubIntegrations(workspaceId: string): Promise<ApiResult<HubIntegration[]>> {
  return request<HubIntegration[]>("/integrations", { workspace: workspaceId });
}

/** listHubAdapters returns the applications this deployment speaks. */
export function listHubAdapters(workspaceId: string): Promise<ApiResult<HubAdapter[]>> {
  return request<HubAdapter[]>("/integrations/adapters", { workspace: workspaceId });
}

/**
 * startAuthorization asks where to send the browser to connect an application.
 *
 * It is a request rather than a link, because the app authenticates with a token
 * held in memory: a plain navigation would carry no credential and be refused, and
 * putting the token in the URL would write it into the browser's history and the
 * server's log. So the consent URL is fetched with the header, and the browser is
 * sent to the provider from there.
 */
export function startAuthorization(
  workspaceId: string,
  app: string,
  returnTo = "/dashboard/integrations",
): Promise<ApiResult<{ url: string; state: string }>> {
  const query = new URLSearchParams({ return_to: returnTo });
  return request<{ url: string; state: string }>(
    `/integrations/${encodeURIComponent(app)}/connect?${query}`,
    { workspace: workspaceId },
  );
}

/** disconnectIntegration clears a connection's stored tokens. */
export function disconnectIntegration(workspaceId: string, id: string): Promise<ApiResult<null>> {
  return request<null>(`/integrations/${id}/disconnect`, { method: "POST", workspace: workspaceId });
}

/** refreshIntegration trades a stored refresh token for a new access token. */
export function refreshIntegration(workspaceId: string, id: string): Promise<ApiResult<HubIntegration>> {
  return request<HubIntegration>(`/integrations/${id}/refresh`, { method: "POST", workspace: workspaceId });
}

/** removeIntegration deletes a connection. */
export function removeIntegration(workspaceId: string, id: string): Promise<ApiResult<null>> {
  return request<null>(`/integrations/${id}`, { method: "DELETE", workspace: workspaceId });
}

/** Memory is one thing a Bolu remembers. */
export type Memory = {
  id: string;
  workspace_id: string;
  /** Empty for the workspace-wide layer, set for one Bolu's own. */
  agent_id?: string;
  content: string;
  kind: string;
  source_message_id?: string;
  pinned: boolean;
  created_by: string;
  /** Names the memory that replaced this one; set on a version kept as history. */
  superseded_by?: string;
  similarity?: number;
  created_at: string;
  updated_at: string;
};

/** KnowledgeItem is one chunk of a document the workspace owns. */
export type KnowledgeItem = {
  id: string;
  workspace_id: string;
  agent_id?: string;
  source: string;
  chunk_index: number;
  content: string;
  /** pending, ready, or failed. Only a ready chunk is ever searchable. */
  status: string;
  similarity?: number;
  created_at: string;
};

/** MemoryFilter narrows the memory screen's read. */
export type MemoryFilter = {
  /** A Bolu id, or "main" for the workspace-wide layer. */
  layer?: string;
  query?: string;
  /** Keeps the replaced versions in the answer. */
  history?: boolean;
  limit?: number;
};

/** listMemories returns what the workspace and its Bolu remember. */
export function listMemories(workspaceId: string, filter: MemoryFilter = {}): Promise<ApiResult<Memory[]>> {
  const query = new URLSearchParams();
  if (filter.layer) {
    query.set("agent_id", filter.layer);
  }
  if (filter.query) {
    query.set("q", filter.query);
  }
  if (filter.history) {
    query.set("history", "true");
  }
  if (filter.limit) {
    query.set("limit", String(filter.limit));
  }

  const suffix = query.toString();
  return request<Memory[]>(`/memories${suffix ? `?${suffix}` : ""}`, { workspace: workspaceId });
}

/** addMemory stores one memory a person wrote. */
export function addMemory(
  workspaceId: string,
  input: { content: string; kind: string; agent_id?: string; pinned?: boolean },
): Promise<ApiResult<Memory>> {
  return request<Memory>("/memories", { method: "POST", body: input, workspace: workspaceId });
}

/** updateMemory changes a memory: its text, its kind, its pin, or its layer. */
export function updateMemory(
  workspaceId: string,
  id: string,
  input: { content?: string; kind?: string; pinned?: boolean; agent_id?: string; move_to_main?: boolean },
): Promise<ApiResult<Memory>> {
  return request<Memory>(`/memories/${id}`, { method: "PATCH", body: input, workspace: workspaceId });
}

/** deleteMemory removes a memory and its embedding, permanently. */
export function deleteMemory(workspaceId: string, id: string): Promise<ApiResult<null>> {
  return request<null>(`/memories/${id}`, { method: "DELETE", workspace: workspaceId });
}

/** listKnowledge returns the workspace's document chunks, failed ones included. */
export function listKnowledge(
  workspaceId: string,
  filter: { status?: string; query?: string; agent_id?: string } = {},
): Promise<ApiResult<KnowledgeItem[]>> {
  const query = new URLSearchParams();
  if (filter.status) {
    query.set("status", filter.status);
  }
  if (filter.query) {
    query.set("q", filter.query);
  }
  if (filter.agent_id) {
    query.set("agent_id", filter.agent_id);
  }

  const suffix = query.toString();
  return request<KnowledgeItem[]>(`/knowledge${suffix ? `?${suffix}` : ""}`, { workspace: workspaceId });
}

/** ingestDocument chunks one document, embeds it, and stores the chunks. */
export function ingestDocument(
  workspaceId: string,
  input: { source: string; text: string; agent_id?: string },
): Promise<ApiResult<KnowledgeItem[]>> {
  return request<KnowledgeItem[]>("/knowledge", { method: "POST", body: input, workspace: workspaceId });
}

/** deleteKnowledge removes one chunk and its embedding. */
export function deleteKnowledge(workspaceId: string, id: string): Promise<ApiResult<null>> {
  return request<null>(`/knowledge/${id}`, { method: "DELETE", workspace: workspaceId });
}

/** setMemoryExtraction turns automatic extraction on or off for one Bolu. */
export function setMemoryExtraction(
  workspaceId: string,
  agentId: string,
  enabled: boolean,
): Promise<ApiResult<Agent>> {
  return request<Agent>(`/agents/${agentId}/memory-extraction`, {
    method: "PATCH",
    body: { enabled },
    workspace: workspaceId,
  });
}

/** listHubGrants returns the connections one Bolu may use. */
export function listHubGrants(workspaceId: string, agentId: string): Promise<ApiResult<HubGrant[]>> {
  return request<HubGrant[]>(`/agents/${agentId}/grants`, { workspace: workspaceId });
}

/** setHubGrant gives a Bolu read or read-write access to a connection. */
export function setHubGrant(
  workspaceId: string,
  agentId: string,
  integrationId: string,
  permission: string,
): Promise<ApiResult<HubGrant>> {
  return request<HubGrant>(`/agents/${agentId}/grants/${integrationId}`, {
    method: "PUT",
    body: { permission },
    workspace: workspaceId,
  });
}

/** deleteHubGrant takes a connection away from a Bolu. */
export function deleteHubGrant(
  workspaceId: string,
  agentId: string,
  integrationId: string,
): Promise<ApiResult<null>> {
  return request<null>(`/agents/${agentId}/grants/${integrationId}`, {
    method: "DELETE",
    workspace: workspaceId,
  });
}

/** listMCPServers returns the Model Context Protocol servers a workspace added. */
export function listMCPServers(workspaceId: string): Promise<ApiResult<MCPServer[]>> {
  return request<MCPServer[]>("/mcp/servers", { workspace: workspaceId });
}

/** addMCPServer registers a server and asks it what it offers. */
export function addMCPServer(
  workspaceId: string,
  input: { app: string; label?: string; endpoint: string; credential?: string; enabled?: boolean },
): Promise<ApiResult<MCPServer>> {
  return request<MCPServer>("/mcp/servers", { method: "POST", body: input, workspace: workspaceId });
}

/** refreshMCPServer asks one server what it offers and records the answer. */
export function refreshMCPServer(workspaceId: string, id: string): Promise<ApiResult<MCPServer>> {
  return request<MCPServer>(`/mcp/servers/${id}/refresh`, { method: "POST", workspace: workspaceId });
}

/** removeMCPServer removes one. */
export function removeMCPServer(workspaceId: string, id: string): Promise<ApiResult<null>> {
  return request<null>(`/mcp/servers/${id}`, { method: "DELETE", workspace: workspaceId });
}

/**
 * A routine is work a Bolu runs on its own.
 *
 * `kind` is the cadence: `daily` at a fixed time, `hourly_range` every hour inside
 * a window, `weekly` on one weekday, and `trigger` when an event arrives. `schedule`
 * is the cadence in words and `next_runs` are the next times it fires, computed in
 * the workspace's own timezone — they travel with the routine rather than in a
 * second request per row.
 */
export type Routine = {
  id: string;
  agent_id: string;
  name: string;
  kind: "daily" | "hourly_range" | "weekly" | "trigger";
  minute: number;
  hour_start: number;
  hour_end: number;
  day_of_week?: number;
  trigger_source?: string;
  instruction: string;
  enabled: boolean;
  paused_reason?: string;
  schedule: string;
  scheduled: boolean;
  next_runs: string[];
  created_at: string;
};

/** RoutineInput is one routine to store. */
export type RoutineInput = {
  agent_id: string;
  name: string;
  kind: Routine["kind"];
  minute?: number;
  hour_start?: number;
  hour_end?: number;
  day_of_week?: number;
  trigger_source?: string;
  instruction: string;
  enabled?: boolean;
};

/** A routine run is one execution and what it produced. */
export type RoutineRun = {
  run_id: string;
  routine_id?: string;
  routine_name: string;
  task_id?: string;
  status: "running" | "succeeded" | "failed" | "skipped";
  summary: string;
  started_at: string;
  finished_at?: string;
};

/** listRoutines returns the workspace's routines, each with its next run times. */
export function listRoutines(
  workspaceId: string,
  filter: { agent_id?: string; enabled?: boolean } = {},
): Promise<ApiResult<Routine[]>> {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(filter)) {
    if (value !== undefined && value !== "") query.set(key, String(value));
  }
  const suffix = query.toString() ? `?${query}` : "";
  return request<Routine[]>(`/routines${suffix}`, { workspace: workspaceId });
}

/** createRoutine stores a routine and registers its schedule. */
export function createRoutine(workspaceId: string, input: RoutineInput): Promise<ApiResult<Routine>> {
  return request<Routine>("/routines", { method: "POST", body: input, workspace: workspaceId });
}

/** updateRoutine changes a routine and re-registers its schedule. */
export function updateRoutine(
  workspaceId: string,
  routineId: string,
  input: RoutineInput,
): Promise<ApiResult<Routine>> {
  return request<Routine>(`/routines/${routineId}`, { method: "PATCH", body: input, workspace: workspaceId });
}

/** deleteRoutine removes a routine and its schedule; its history stays. */
export function deleteRoutine(workspaceId: string, routineId: string): Promise<ApiResult<null>> {
  return request<null>(`/routines/${routineId}`, { method: "DELETE", workspace: workspaceId });
}

/** pauseRoutine pauses a routine's schedule without losing its history. */
export function pauseRoutine(workspaceId: string, routineId: string): Promise<ApiResult<Routine>> {
  return request<Routine>(`/routines/${routineId}/pause`, { method: "POST", workspace: workspaceId });
}

/** resumeRoutine resumes a paused routine's schedule. */
export function resumeRoutine(workspaceId: string, routineId: string): Promise<ApiResult<Routine>> {
  return request<Routine>(`/routines/${routineId}/resume`, { method: "POST", workspace: workspaceId });
}

/** listRoutineRuns returns the workspace's run history, newest first. */
export function listRoutineRuns(
  workspaceId: string,
  filter: { routine_id?: string; agent_id?: string; status?: string; limit?: number } = {},
): Promise<ApiResult<RoutineRun[]>> {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(filter)) {
    if (value !== undefined && value !== "" && value !== 0) query.set(key, String(value));
  }
  const suffix = query.toString() ? `?${query}` : "";
  return request<RoutineRun[]>(`/routine-runs${suffix}`, { workspace: workspaceId });
}

/** previewRoutine answers the next run times for a cadence that is not saved yet. */
export function previewRoutine(
  workspaceId: string,
  input: { kind: Routine["kind"]; minute?: number; hour_start?: number; hour_end?: number; day_of_week?: number },
): Promise<ApiResult<{ next_runs: string[] }>> {
  return request<{ next_runs: string[] }>("/routines/preview", {
    method: "POST",
    body: input,
    workspace: workspaceId,
  });
}

/**
 * A WhatsApp number a workspace has linked, or is linking.
 *
 * `status` is the whole state machine: `unlinked` is a row somebody created and did
 * not finish, `linking` is a code being shown, `connected` is a live session, and
 * the last two are the two ways it can be down. `connected` and `linked` differ on
 * purpose — a session that exists but is offline is still linked, and the screen
 * says which of the two it is.
 */
export type WhatsAppNumber = {
  id: string;
  phone: string;
  label: string;
  agent_id?: string;
  status: "unlinked" | "linking" | "connected" | "needs_relink" | "disconnected";
  reason?: string;
  linked: boolean;
  connected: boolean;
  /** Whether this workspace accepted the warning about an unsupported route. */
  terms_accepted: boolean;
  hourly_limit: number;
  created_at: string;
  last_seen_at?: string;
};

/**
 * One attempt to link a number.
 *
 * `qr_matrix` is the code as rows of "1" and "0": it travels on the per-attempt
 * socket rather than on the workspace stream, because a QR is a capability and
 * whoever scans it links the number.
 */
export type WhatsAppLink = {
  link_id: string;
  number_id?: string;
  state: "waiting" | "code" | "paired" | "expired" | "failed";
  qr?: string;
  qr_matrix?: string[];
  pair_code?: string;
  expires_at?: string;
  reason?: string;
};

/** A message a customer sent. */
export type WhatsAppMessage = {
  id: string;
  wa_message_id: string;
  chat: string;
  from?: string;
  name?: string;
  group: boolean;
  ours: boolean;
  kind: "text" | "image" | "video" | "audio" | "document" | "sticker" | "other";
  text: string;
  media_kind?: string;
  media_status?: "" | "pending" | "ready" | "failed";
  media_error?: string;
  task_id?: string;
  received_at: string;
};

/** A message this workspace asked a number to send. */
export type WhatsAppOutbox = {
  id: string;
  to: string;
  kind: string;
  text: string;
  status: "queued" | "sending" | "sent" | "failed" | "held";
  attempts: number;
  last_error?: string;
  sent_at?: string;
  created_at: string;
};

/** listWhatsAppNumbers returns the workspace's numbers. */
export function listWhatsAppNumbers(workspaceId: string): Promise<ApiResult<WhatsAppNumber[]>> {
  return request<WhatsAppNumber[]>("/whatsapp/numbers", { workspace: workspaceId });
}

/** prepareWhatsAppNumber records a number and who answers it, before it is linked. */
export function prepareWhatsAppNumber(
  workspaceId: string,
  input: { phone: string; label?: string; agent_id: string },
): Promise<ApiResult<WhatsAppNumber>> {
  return request<WhatsAppNumber>("/whatsapp/numbers", { method: "POST", body: input, workspace: workspaceId });
}

/** updateWhatsAppNumber changes the label, the Bolu that answers, or the ceiling. */
export function updateWhatsAppNumber(
  workspaceId: string,
  numberId: string,
  input: { label?: string; agent_id?: string; hourly_limit?: number },
): Promise<ApiResult<WhatsAppNumber>> {
  return request<WhatsAppNumber>(`/whatsapp/numbers/${numberId}`, {
    method: "PATCH",
    body: input,
    workspace: workspaceId,
  });
}

/** acceptWhatsAppTerms records that the workspace accepted the warning. */
export function acceptWhatsAppTerms(workspaceId: string, numberId: string): Promise<ApiResult<WhatsAppNumber>> {
  return request<WhatsAppNumber>(`/whatsapp/numbers/${numberId}/terms`, { method: "POST", workspace: workspaceId });
}

/** startWhatsAppLink begins a pairing and answers the first state to show. */
export function startWhatsAppLink(
  workspaceId: string,
  numberId: string,
  input: { phone?: string } = {},
): Promise<ApiResult<WhatsAppLink>> {
  return request<WhatsAppLink>(`/whatsapp/numbers/${numberId}/link`, {
    method: "POST",
    body: input,
    workspace: workspaceId,
  });
}

/** whatsAppLinkState reads one attempt, which is how a screen recovers after a reload. */
export function whatsAppLinkState(workspaceId: string, linkId: string): Promise<ApiResult<WhatsAppLink>> {
  return request<WhatsAppLink>(`/whatsapp/links/${linkId}`, { workspace: workspaceId });
}

/** whatsAppPairCode asks for the phone-code alternative to scanning. */
export function whatsAppPairCode(workspaceId: string, linkId: string, phone: string): Promise<ApiResult<WhatsAppLink>> {
  return request<WhatsAppLink>(`/whatsapp/links/${linkId}/pair-code`, {
    method: "POST",
    body: { phone },
    workspace: workspaceId,
  });
}

/** cancelWhatsAppLink abandons an attempt. */
export function cancelWhatsAppLink(workspaceId: string, linkId: string): Promise<ApiResult<null>> {
  return request<null>(`/whatsapp/links/${linkId}`, { method: "DELETE", workspace: workspaceId });
}

/** unlinkWhatsAppNumber ends the session and keeps the message history. */
export function unlinkWhatsAppNumber(workspaceId: string, numberId: string): Promise<ApiResult<WhatsAppNumber>> {
  return request<WhatsAppNumber>(`/whatsapp/numbers/${numberId}`, { method: "DELETE", workspace: workspaceId });
}

/** listWhatsAppMessages returns what customers wrote, newest first. */
export function listWhatsAppMessages(
  workspaceId: string,
  numberId: string,
  filter: { chat?: string; limit?: number } = {},
): Promise<ApiResult<WhatsAppMessage[]>> {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(filter)) {
    if (value !== undefined && value !== "" && value !== 0) query.set(key, String(value));
  }
  const suffix = query.toString() ? `?${query}` : "";
  return request<WhatsAppMessage[]>(`/whatsapp/numbers/${numberId}/messages${suffix}`, { workspace: workspaceId });
}

/** listWhatsAppOutbox returns what this workspace asked a number to send. */
export function listWhatsAppOutbox(
  workspaceId: string,
  numberId: string,
  filter: { status?: string; limit?: number } = {},
): Promise<ApiResult<WhatsAppOutbox[]>> {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(filter)) {
    if (value !== undefined && value !== "" && value !== 0) query.set(key, String(value));
  }
  const suffix = query.toString() ? `?${query}` : "";
  return request<WhatsAppOutbox[]>(`/whatsapp/numbers/${numberId}/outbox${suffix}`, { workspace: workspaceId });
}

/**
 * whatsAppLinkSocket opens the per-attempt socket.
 *
 * It is the only place the QR travels: a browser cannot set a header on a
 * WebSocket, so the token and the workspace go in the query string, exactly as the
 * workspace event stream does it.
 */
export function whatsAppLinkSocketURL(workspaceId: string, linkId: string): string {
  const base = (process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:8080").replace(/^http/, "ws");
  const query = new URLSearchParams({ workspace_id: workspaceId });
  const token = getAccessToken();
  if (token) query.set("token", token);
  return `${base}/api/whatsapp/links/${linkId}/socket?${query}`;
}

// ---------------------------------------------------------------- billing

/** One package this deployment sells. */
export type BillingPlan = {
  key: string;
  name: string;
  monthly_price_idr: number;
  token_quota: number;
  daily_cost_micros: number;
  period_days: number;
  max_agents: number;
  max_integrations: number;
  max_members: number;
  is_trial: boolean;
};

/** One pack of tokens a workspace can buy inside a period. */
export type TopUpPackage = {
  key: string;
  name: string;
  token_amount: number;
  price_idr: number;
};

/**
 * The package a workspace is on, and what it has spent of it.
 *
 * `entitlement` is derived by the backend from the period and the grace window rather
 * than remembered, so a client reads it instead of computing one — two computations
 * would eventually disagree about whether a workspace has stopped.
 */
export type Subscription = {
  id: string;
  plan_key: string;
  plan_name: string;
  /** The package that takes effect at the next period, when somebody chose one. */
  pending_plan_key?: string;
  pending_plan_name?: string;
  status: "trial" | "active" | "past_due" | "canceled";
  entitlement: "active" | "grace" | "read_only";
  current_period_start: string;
  current_period_end: string;
  grace_until?: string;
  grace_days: number;
  read_only: boolean;
  tokens_granted: number;
  tokens_topped_up: number;
  tokens_used: number;
  /** Negative means the package does not bound tokens. */
  tokens_remaining: number;
  used_percent: number;
  daily_cost_micros: number;
  spent_today_micros: number;
  outstanding_idr: number;
  max_agents: number;
  max_integrations: number;
  max_members: number;
  top_ups: TopUpPackage[];
};

/** One amount a workspace owes, or owed. */
export type Invoice = {
  id: string;
  kind: "subscription" | "topup";
  status: "pending" | "paid" | "failed" | "expired";
  amount_idr: number;
  token_amount?: number;
  period_start: string;
  period_end: string;
  /** What a person acts on to pay: a link, or the virtual account and its bank. */
  payment_instructions?: string;
  provider?: string;
  attempts: number;
  last_error?: string;
  expires_at?: string;
  paid_at?: string;
  created_at: string;
  outstanding: boolean;
};

/** A package, its usage, and what it owes, read at one moment. */
export type BillingSnapshot = {
  plan: BillingPlan;
  subscription: Subscription;
  entitlement: Subscription["entitlement"];
  read_only: boolean;
  grace_days: number;
  invoices: Invoice[];
  outstanding: Invoice[];
  top_ups: TopUpPackage[];
};

/** billingSnapshot reads the workspace's package and what it owes. */
export function billingSnapshot(workspaceId: string): Promise<ApiResult<BillingSnapshot>> {
  return request<BillingSnapshot>("/billing/subscription", { workspace: workspaceId });
}

/** listBillingPlans returns the packages this deployment sells. */
export function listBillingPlans(workspaceId: string): Promise<ApiResult<BillingPlan[]>> {
  return request<BillingPlan[]>("/billing/plans", { workspace: workspaceId });
}

/** changeBillingPlan moves the workspace to another package. */
export function changeBillingPlan(workspaceId: string, planKey: string): Promise<ApiResult<BillingSnapshot>> {
  return request<BillingSnapshot>("/billing/subscription", {
    method: "POST",
    body: { plan_key: planKey },
    workspace: workspaceId,
  });
}

/** listInvoices returns what the workspace was charged, newest first. */
export function listInvoices(workspaceId: string, status?: string): Promise<ApiResult<Invoice[]>> {
  const suffix = status ? `?status=${encodeURIComponent(status)}` : "";
  return request<Invoice[]>(`/billing/invoices${suffix}`, { workspace: workspaceId });
}

/** payInvoice asks for a way to pay one invoice. */
export function payInvoice(workspaceId: string, invoiceId: string): Promise<ApiResult<Invoice>> {
  return request<Invoice>(`/billing/invoices/${invoiceId}/pay`, { method: "POST", workspace: workspaceId });
}

/** buyTopUp buys tokens inside the period the workspace is in. */
export function buyTopUp(workspaceId: string, packageKey: string): Promise<ApiResult<Invoice>> {
  return request<Invoice>("/billing/topups", {
    method: "POST",
    body: { package_key: packageKey },
    workspace: workspaceId,
  });
}

/* ------------------------------------------------------------ operator surface
 *
 * The endpoints behind /admin. They carry no workspace: the operator surface
 * administers the deployment rather than a tenant, so `workspace` is left unset
 * and the API never asks for the header. A plain account is answered 403, which
 * the screens treat as "this is not for you" rather than as an error to retry.
 */

/** AdminAccount is one account as the operator surface reads it. */
export type AdminAccount = {
  id: string;
  email: string;
  role: string;
  email_verified: boolean;
  onboarded: boolean;
  has_google: boolean;
  has_password: boolean;
  /** How many workspaces the account is a member of. */
  workspaces: number;
  created_at: string;
};

/** AdminStats is what the deployment's accounts add up to. */
export type AdminStats = {
  total: number;
  admins: number;
  users: number;
  verified: number;
  onboarded: number;
  google_accounts: number;
  new_last_7_days: number;
};

/** AdminPage is one page of accounts plus what the filter matched. */
export type AdminPage = {
  accounts: AdminAccount[];
  total: number;
  limit: number;
  offset: number;
};

/** adminOverview returns the numbers the operator dashboard opens with. */
export function adminOverview(): Promise<ApiResult<AdminStats>> {
  return request<AdminStats>("/admin/overview");
}

/**
 * adminUsers returns a page of accounts.
 *
 * The envelope's meta carries the total and the page the filter became, so the
 * screen never has to remember what it asked for.
 */
export async function adminUsers(filter: {
  search?: string;
  role?: string;
  limit?: number;
  offset?: number;
}): Promise<ApiResult<AdminPage>> {
  const params = new URLSearchParams();
  if (filter.search) params.set("q", filter.search);
  if (filter.role) params.set("role", filter.role);
  if (filter.limit) params.set("limit", String(filter.limit));
  if (filter.offset) params.set("offset", String(filter.offset));

  const suffix = params.size > 0 ? `?${params.toString()}` : "";
  const result = await request<AdminAccount[]>(`/admin/users${suffix}`);
  if (!result.ok) return result;

  const meta = result.meta ?? {};
  return {
    ...result,
    data: {
      accounts: result.data,
      total: Number(meta.total ?? result.data.length),
      limit: Number(meta.limit ?? result.data.length),
      offset: Number(meta.offset ?? 0),
    },
  };
}

/** adminSetRole promotes or demotes an account. */
export function adminSetRole(userId: string, role: string): Promise<ApiResult<AdminAccount>> {
  return request<AdminAccount>(`/admin/users/${userId}/role`, { method: "POST", body: { role } });
}
