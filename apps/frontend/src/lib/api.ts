/**
 * API client for the Bolu backend.
 *
 * Two details matter here:
 * - every request sends credentials, because the refresh token lives in an
 *   httpOnly cookie that JavaScript must not read;
 * - the access token is kept in memory only. A reload calls /auth/refresh,
 *   which reads the cookie and returns a fresh access token.
 */

const API_URL = process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:8080";

export type ApiError = {
  ok: false;
  status: number;
  message: string;
  code?: string;
};

export type ApiSuccess<T> = {
  ok: true;
  status: number;
  data: T;
};

export type ApiResult<T> = ApiSuccess<T> | ApiError;

export type User = {
  id: string;
  email: string;
  email_verified: boolean;
  onboarded: boolean;
  has_password: boolean;
  has_google: boolean;
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

export function getAccessToken(): string | null {
  return accessToken;
}

export function setAccessToken(token: string | null): void {
  accessToken = token;
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
  error?: { code?: string; details?: unknown };
};

type RequestOptions = {
  method?: string;
  body?: unknown;
  /** Sends the active workspace header, for tenant-scoped endpoints. */
  workspace?: string | null;
  /** Sends the in-memory access token as a bearer token. */
  authenticated?: boolean;
};

/** request performs one API call and normalises the envelope. */
async function request<T>(path: string, options: RequestOptions = {}): Promise<ApiResult<T>> {
  const headers: Record<string, string> = { "Content-Type": "application/json" };

  if (options.authenticated !== false && accessToken) {
    headers.Authorization = `Bearer ${accessToken}`;
  }
  if (options.workspace) {
    headers["X-Workspace-Id"] = options.workspace;
  }

  let response: Response;
  try {
    response = await fetch(`${API_URL}/api${path}`, {
      method: options.method ?? "GET",
      headers,
      credentials: "include",
      body: options.body === undefined ? undefined : JSON.stringify(options.body),
      cache: "no-store",
    });
  } catch {
    return {
      ok: false,
      status: 0,
      message: "Tidak bisa menghubungi server. Cek koneksimu lalu coba lagi.",
    };
  }

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
    };
  }

  return { ok: true, status: response.status, data: envelope.data as T };
}

/** register creates a password account and emails a verification link. */
export function register(email: string, password: string): Promise<ApiResult<User>> {
  return request<User>("/auth/register", { method: "POST", body: { email, password }, authenticated: false });
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
  const result = await request<Session>("/auth/login", { method: "POST", body: { email, password }, authenticated: false });
  if (result.ok) {
    setAccessToken(result.data.access_token);
  }
  return result;
}

/** refreshSession exchanges the refresh cookie for a new access token. */
export async function refreshSession(): Promise<ApiResult<Session>> {
  const result = await request<Session>("/auth/refresh", { method: "POST", authenticated: false });
  if (result.ok) {
    setAccessToken(result.data.access_token);
  }
  return result;
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

/** onboard creates the workspace and returns it with the two default teams. */
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
  input: { name: string; business_field: string; timezone: string },
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
  reason?: { kind: string; id: string; title?: string; status?: string; created_at: string };
  template_key?: string;
  tools: string[];
  default_model: Record<string, unknown>;
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

/** listAgentTeams returns Tim Bolu and Tim Hore with their agents. */
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
  input: { team_id: string; name?: string; template_key?: string; copy_from?: string; persona?: string; role?: string },
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

/** listModelAdapters returns the wire formats the backend can serve. */
export function listModelAdapters(): Promise<ApiResult<{ adapters: string[] }>> {
  return request<{ adapters: string[] }>("/llm/adapters");
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
  kind: "direct" | "group";
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
  reply_message_id?: string;
  responder?: ChatParticipant;
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

/** uploadAttachment stores a file and returns its metadata. */
export async function uploadAttachment(
  workspaceId: string,
  conversationId: string,
  file: File,
  messageId?: string,
): Promise<ApiResult<Attachment>> {
  const form = new FormData();
  form.append("file", file);
  if (messageId) {
    form.append("message_id", messageId);
  }

  const headers: Record<string, string> = { "X-Workspace-Id": workspaceId };
  if (accessToken) {
    headers.Authorization = `Bearer ${accessToken}`;
  }

  let response: Response;
  try {
    response = await fetch(`${API_URL}/api/conversations/${conversationId}/attachments`, {
      method: "POST",
      headers,
      credentials: "include",
      body: form,
    });
  } catch {
    return { ok: false, status: 0, message: "Tidak bisa menghubungi server." };
  }

  const text = await response.text();
  let envelope: Envelope<Attachment> | null = null;
  try {
    envelope = text ? (JSON.parse(text) as Envelope<Attachment>) : null;
  } catch {
    envelope = null;
  }

  if (!response.ok || !envelope?.success || !envelope.data) {
    return {
      ok: false,
      status: response.status,
      message: envelope?.message ?? "Gagal mengunggah berkas.",
      code: envelope?.error?.code,
    };
  }
  return { ok: true, status: response.status, data: envelope.data };
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
