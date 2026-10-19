/**
 * The bridges between the platform layer and each module's own ports.
 *
 * Every module declares the interfaces it needs (`domain/ports.ts`) rather than
 * importing a concrete platform type — that is the Go architecture rule, and it
 * is why the container has an adapters file. Each adapter below is one Go
 * `type xAdapter struct` from `apps/backend/internal/container/adapters.go`.
 */

import type { Sender as MailSender } from "../platform/mail/mail.ts";
import type { Limiter } from "../platform/ratelimit/ratelimit.ts";
import type { Codec as OAuthStateCodec } from "../platform/oauthstate/state.ts";
import type { Client as GoogleClient } from "../platform/oauth/google/google.ts";
import { generateToken, hashPassword, verifyPassword } from "../platform/authn/index.ts";

import type {
  AgentDirectory as TaskAgentDirectory,
  AgentNamer as TaskAgentNamer,
  AgentRef as TaskAgentRef,
  AttachmentReader as TaskAttachmentReader,
  Block as TaskBlock,
  Budget as TaskBudget,
  DraftGate as TaskDraftGate,
  EventSink as TaskEventSink,
  MemoryRecall as TaskMemoryRecall,
  Message as TaskMessage,
  OutcomeSink as TaskOutcomeSink,
  ReplyWriter as TaskReplyWriter,
  Scope as TaskScope,
  Tool as TaskTool,
  ToolCall as TaskToolCall,
  ToolExecutor as TaskToolExecutor,
  ToolRegistry as TaskToolRegistry,
} from "../modules/task/domain/task.ts";
import {
  ErrDraftNoSender,
  ErrDraftNotApproved,
  ErrOutcomeUnclaimed,
  ErrToolNotAvailable,
  LabelWriteExternal,
  NilUUID,
  agentActive,
  errorMessage,
  wrap,
} from "../modules/task/domain/task.ts";
import type {
  Attachment as ChatAttachment,
  Block as ChatBlock,
  Message as ChatMessage,
} from "../modules/chat/domain/conversation.ts";
import type { Event as ChatEvent } from "../modules/chat/domain/event.ts";
import { EventMessageUpdated } from "../modules/chat/domain/event.ts";
import { WireMessage } from "../modules/chat/domain/wire.ts";
import type { Agent as AgentRecord, Tool as AgentToolRecord } from "../modules/agent/domain/agent.ts";
import { ErrAgentNotFound } from "../modules/agent/domain/agent.ts";
import type { Integration as IntegrationRecord, Scope as IntegrationScope } from "../modules/integration/domain/integration.ts";
import { ErrNotFound as ErrIntegrationNotFound, StatusConnected } from "../modules/integration/domain/integration.ts";
import type { Registry as IntegrationRegistry, Result as IntegrationResult } from "../modules/integration/domain/adapter.ts";
import { isConnectionless } from "../modules/integration/domain/adapter.ts";
import type { Scope as ApprovalScope } from "../modules/approval/domain/approval.ts";
import { ErrNoSender, ErrNotApproved, is as approvalIs } from "../modules/approval/domain/approval.ts";
import type { Outcome as ApprovalOutcome, Request as ApprovalRequest } from "../modules/approval/domain/service.ts";
import type { Recall as MemoryRecallResult, RecallRequest as MemoryRecallRequest } from "../modules/memory/domain/memory.ts";
import type { Outcome as RoutineOutcome } from "../modules/routine/domain/service.ts";

// --- auth module ports ------------------------------------------------------

export interface AuthMailer {
  send(to: string, subject: string, body: string): Promise<void>;
}

export function authMailer(sender: MailSender | null, from: string): AuthMailer {
  return {
    async send(to, subject, body) {
      if (!sender) throw new Error("mail is not configured");
      await sender.send({ to, subject, text: body });
    },
  };
}

export interface AuthStateSigner {
  sign(): Promise<string>;
  verify(state: string): Promise<void>;
}

/**
 * Signs the `state` of a Google sign-in. The platform codec signs a structured
 * OAuth state; the auth module's port only needs an opaque value it can verify
 * later, so the payload is minted here with no tenant (the sign-in happens before
 * a workspace is chosen).
 */
export function authStateSigner(codec: OAuthStateCodec | null, ttlMinutes: number): AuthStateSigner | null {
  if (!codec) return null;
  return {
    async sign() {
      return codec.sign({
        purpose: "oauth-state",
        workspaceId: "",
        userId: "",
        app: "google",
        nonce: crypto.randomUUID(),
        returnTo: "",
        expiresAt: Math.floor(Date.now() / 1000) + ttlMinutes * 60,
      } as any);
    },
    async verify(state) {
      codec.verify(state);
    },
  };
}

export interface AuthThrottle {
  allow(key: string, capacity: number, refill: number): Promise<{ allowed: boolean; retryAfter: number }>;
  reset(key: string): Promise<void>;
}

export function authThrottle(limiter: Limiter | null): AuthThrottle | null {
  if (!limiter) return null;
  return {
    async allow(key, capacity, refill) {
      const result = await limiter.allow(undefined, key, capacity, refill);
      return { allowed: result.allowed, retryAfter: result.retryAfter ?? 0 };
    },
    async reset(key) {
      await limiter.reset(undefined, key);
    },
  };
}

export interface AuthGoogle {
  authCodeURL(state: string): string;
  exchange(code: string): Promise<{ subject: string; email: string; emailVerified: boolean }>;
}

export function authGoogle(client: GoogleClient | null): AuthGoogle | null {
  if (!client) return null;
  return {
    authCodeURL(state) {
      return client.authCodeURL(state);
    },
    async exchange(code) {
      const info = await client.exchange(code);
      return { subject: info.sub, email: info.email, emailVerified: info.email_verified };
    },
  };
}

export interface AuthPasswords {
  hash(password: string): Promise<string>;
  verify(hash: string, password: string): Promise<boolean>;
  validate(password: string): Promise<void>;
}

/** The argon2id implementation, with the Go password policy. */
export const authPasswords: AuthPasswords = {
  hash: hashPassword,
  verify: verifyPassword,
  async validate(password) {
    // Mirrors the Go policy: a password must be long enough to be worth hashing.
    if (typeof password !== "string" || password.length < 8) {
      throw new Error("password must be at least 8 characters");
    }
  },
};

export interface AuthTokens {
  generate(): { raw: string; hash: string };
}

export const authTokens: AuthTokens = { generate: generateToken };

// --- workspace module ports -------------------------------------------------

export interface WorkspaceMailer {
  send(ctx: unknown, to: string, subject: string, body: string): Promise<void>;
}

export function workspaceMailer(sender: MailSender | null, from: string): WorkspaceMailer {
  return {
    async send(_ctx, to, subject, body) {
      if (!sender) throw new Error("mail is not configured");
      await sender.send({ to, subject, text: body });
    },
  };
}

export interface WorkspaceTokens {
  generate(): { raw: string; hash: string };
}

export const workspaceTokens: WorkspaceTokens = { generate: generateToken };

export interface WorkspacePlanLimits {
  maxMembers(ctx: unknown, workspaceId: string): Promise<number>;
}

/**
 * The member allowance. Until the billing module is wired this reports the plan's
 * default, which is what a workspace without a subscription is created under.
 */
export function workspacePlanLimits(maxMembers: number): WorkspacePlanLimits {
  return {
    async maxMembers() {
      return maxMembers;
    },
  };
}

// --- agent module ports -----------------------------------------------------

export interface AgentPlanLimits {
  maxAgents(workspaceId: string): Promise<number>;
}

export function agentPlanLimits(maxAgents: number): AgentPlanLimits {
  return {
    async maxAgents() {
      return maxAgents;
    },
  };
}

// --- the task runtime, reached from chat and from routines -------------------

/**
 * Hands a chat message to the task runtime.
 *
 * The container is where the two modules meet, so it is where one module's error
 * becomes the other's: the task module's failure is returned as-is and the chat
 * handler renders it. `prompt` is the title, which is what the Go adapter does —
 * the task's own context load reads the conversation.
 */
export function chatTaskStarter(taskService: {
  dispatch(ctx: undefined, scope: { userId?: string; workspaceId?: string }, req: any): Promise<{ id: string }>;
}) {
  return {
    async start(scope: { userId?: string; workspaceId?: string }, req: any): Promise<string> {
      const task = await taskService.dispatch(undefined, scope, {
        agentId: req.agentId,
        conversationId: req.conversationId,
        replyMessageId: req.messageId,
        trigger: req.trigger,
        title: req.title,
        prompt: req.title,
        parentTaskId: "",
        depth: 0,
      });
      return task.id;
    },
  };
}

/** Opens the task one routine run performs. */
export function routineTaskStarter(taskService: {
  dispatch(ctx: undefined, scope: { userId?: string; workspaceId?: string }, req: any): Promise<{ id: string }>;
}) {
  return {
    async start(scope: { userId?: string; workspaceId?: string }, request: any): Promise<string> {
      const task = await taskService.dispatch(undefined, scope, {
        agentId: request.agentId,
        conversationId: "",
        replyMessageId: "",
        trigger: "routine",
        title: request.title,
        prompt: request.prompt,
        parentTaskId: "",
        depth: 0,
      });
      return task.id;
    },
  };
}

// --- the agent registry, read by chat ---------------------------------------

/**
 * The registry as chat reads it: a Bolu's identity and whether it is awake.
 *
 * A port rather than a direct call because chat must not import another module's
 * service — only its domain — which is the Go architecture rule.
 */
export function agentDirectory(agentService: {
  get(scope: any, id: string): Promise<any>;
  list(scope: any): Promise<any[]>;
}) {
  const toRef = (agent: any) => ({
    id: agent.id,
    name: agent.name,
    role: agent.role,
    persona: agent.persona,
    tone: agent.tone ?? "",
    stored: agent.status ?? "",
    display: agent.displayStatus ?? agent.status ?? "",
    tools: agent.tools ?? [],
    defaultModel: agent.defaultModel ?? {},
  });

  return {
    async agent(scope: any, id: string) {
      return toRef(await agentService.get(scope, id));
    },
    async agents(scope: any, ids: string[]) {
      return Promise.all(ids.map((id) => agentService.get(scope, id).then(toRef)));
    },
    // A Bolu is awake unless it was explicitly rested: only the rest switch is a
    // column, and the display status is derived.
    active(ref: { stored: string }) {
      return ref.stored !== "resting" && ref.stored !== "paused";
    },
  };
}

// --- the memory module, read by chat ----------------------------------------

/**
 * What chat asks of memory: store a "remember this" request, and tell the
 * extractor the thread is still moving.
 *
 * A port rather than a direct call because chat may only import another module's
 * domain — which is the Go architecture rule, and the reason the container owns
 * this seam.
 */
export function chatMemory(memoryService: {
  remember(scope: any, req: any): Promise<any>;
  active(scope: any, conversationId: string, agentId: string): Promise<void>;
}) {
  return {
    async remember(scope: any, req: any) {
      const result = await memoryService.remember(scope, {
        text: req.text,
        agentId: req.agentId,
        messageId: req.messageId,
      });
      return {
        handled: result.handled,
        confirmation: result.confirmation ?? "",
        refused: result.refused ?? false,
      };
    },
    async conversationActive(scope: any, conversationId: string, agentId: string) {
      await memoryService.active(scope, conversationId, agentId);
    },
  };
}

// --- the spend counter, read by billing -------------------------------------

/**
 * The live spend as billing reads it.
 *
 * The platform counter counts tokens and cost with an abort signal and a Go-style
 * signature; billing's port is the two numbers it needs. Adapting here keeps the
 * platform free of billing's vocabulary and billing free of the platform's.
 */
export function spendCounter(counter: {
  spent(ctx: AbortSignal | undefined, workspaceId: string): Promise<number>;
  spentCostToday(ctx: AbortSignal | undefined, workspaceId: string): Promise<number>;
}) {
  return {
    periodTokens(workspaceId: string): Promise<number> {
      return counter.spent(undefined, workspaceId);
    },
    dailyCost(workspaceId: string): Promise<number> {
      return counter.spentCostToday(undefined, workspaceId);
    },
  };
}

// --- memory's conversation reader and debounce ------------------------------

/** The id of one conversation's extraction run. */
export function memoryWorkflowId(conversationId: string): string {
  return `memory-${conversationId}`;
}

/** Concatenates the text a model reads out of a message's blocks. */
function textOf(blocks: unknown): string {
  if (!Array.isArray(blocks)) return "";
  const parts: string[] = [];
  for (const block of blocks) {
    const b = block as Record<string, unknown>;
    if (typeof b.markdown === "string") parts.push(b.markdown);
    else if (typeof b.text === "string") parts.push(b.text);
  }
  return parts.join("\n").trim();
}

/**
 * The thread an extraction runs over, read out of chat.
 *
 * A port rather than a direct call because memory may only import chat's domain —
 * the Go architecture rule, and the reason this lives in the container.
 */
export function memoryConversations(
  chatRepo: { latestMessages(scope: any, conversationId: string, limit: number): Promise<any[]> },
  agentService: { get(scope: any, id: string): Promise<any> } | null,
) {
  return {
    async recent(scope: any, conversationId: string, limit: number) {
      const messages = await chatRepo.latestMessages(scope, conversationId, limit);
      return Promise.all(
        messages.map(async (m: any) => {
          let agentName = "";
          if (m.authorAgentId && agentService) {
            try {
              agentName = (await agentService.get(scope, m.authorAgentId)).name;
            } catch {
              // A Bolu that has since been deleted keeps its words, unnamed.
              agentName = "";
            }
          }
          return {
            messageId: m.id,
            agentId: m.authorAgentId ?? "",
            agentName,
            text: textOf(m.blocks),
            createdAt: m.createdAt,
          };
        }),
      );
    },
  };
}

/**
 * Pushes a thread's quiet timer out.
 *
 * SignalWithStart, not Signal: the first message of a thread has no run yet, and
 * starting one with the signal already delivered is what makes the debounce
 * correct from the first write.
 */
export function memoryScheduler(
  handle: { client(): any; StartOptions(id: string): any } | null,
  quietMs: number,
  workflowName: string,
  signalName: string,
) {
  return {
    async conversationActive(scope: any, conversationId: string, agentId: string): Promise<void> {
      if (!handle || !conversationId) return;
      const cli = handle.client();
      await cli.workflow.signalWithStart(workflowName, {
        ...handle.StartOptions(memoryWorkflowId(conversationId)),
        args: [
          {
            WorkspaceID: scope.workspaceId,
            ConversationID: conversationId,
            AgentID: agentId,
            QuietPeriod: quietMs > 0 ? quietMs : 5 * 60_000,
          },
        ],
        signal: signalName,
        signalArgs: [],
      });
    },
  };
}


// --- the task runtime's ports ------------------------------------------------
//
// The workflow's activities are the only place the task runtime touches the
// world, and each touch is a port it declares. These adapters are the container
// answering those ports with another module's service: the task module learns
// nothing about chat, memory, approval, or integrations, and each of those
// learns nothing about tasks.

/** The tenant identity chat, memory, and the task runtime share. */
interface TenantScope {
  userId: string;
  workspaceId: string;
}

/** The tenant identity a task's scope names, with the task's nullable ids filled in. */
function tenantOf(scope: TaskScope): TenantScope {
  return { userId: scope.userId ?? "", workspaceId: scope.workspaceId ?? "" };
}

/** The JSON document a chat block stores, base64-encoded for the Temporal wire. */
function encodeBlockBody(body: unknown): string {
  return Buffer.from(JSON.stringify(body ?? {}), "utf8").toString("base64");
}

/** Decodes a task block's body back into the document chat stores. */
function decodeBlockBody(body: string): unknown {
  try {
    return JSON.parse(Buffer.from(body, "base64").toString("utf8")) as unknown;
  } catch {
    return null;
  }
}

function toTaskMessage(message: ChatMessage): TaskMessage {
  return {
    ID: message.id,
    ConversationID: message.conversationId,
    AgentID: message.authorAgentId ?? "",
    UserID: message.authorUserId ?? "",
    Blocks: (message.blocks ?? []).map((block) => ({
      Type: block.type,
      Body: encodeBlockBody(block.body),
    })),
    Attachments: (message.attachments ?? []).map((attachment) => ({
      ID: attachment.id,
      Filename: attachment.filename,
      ContentType: attachment.contentType,
      ByteSize: attachment.byteSize,
    })),
  };
}

function toChatBlocks(blocks: TaskBlock[]): ChatBlock[] {
  return blocks.map((block) => ({ type: block.Type, body: decodeBlockBody(block.Body) }));
}

/**
 * Writes a task event to the durable stream the product reads.
 *
 * The seam is the chat module's event store, because a task event and a chat
 * event are the same fact: one row in `activity_events`, published to whoever is
 * connected. Routing it through the chat service is what keeps the two from
 * writing the stream differently.
 */
export function taskEventSink(chat: { emit(event: ChatEvent): Promise<void> }): TaskEventSink {
  return {
    async emit(_ctx, event) {
      await chat.emit({
        id: 0,
        workspaceId: event.workspaceId,
        type: event.type,
        actorAgentId: event.actorAgentId,
        actorUserId: event.actorUserId,
        conversationId: event.conversationId,
        taskId: event.taskId,
        draftId: event.draftId,
        payload: event.payload,
        createdAt: new Date(0),
      });
    },
  };
}

/** The conversation reads and writes a task's answer needs. */
export interface TaskChatStore {
  latestMessages(scope: TenantScope, conversationId: string, limit: number): Promise<ChatMessage[]>;
  getMessage(scope: TenantScope, id: string): Promise<ChatMessage>;
  update(scope: TenantScope, message: ChatMessage): Promise<ChatMessage>;
}

/**
 * Reads the thread a task answers and fills the placeholder the chat module
 * wrote.
 *
 * A task's answer is a chat message, so it is written through chat's own
 * repository and announced on chat's own stream. The alternative — a second
 * writer — is how the stored message and the live one drift apart.
 */
export function taskReplyWriter(
  store: TaskChatStore,
  chat: { emit(event: ChatEvent): Promise<void> },
): TaskReplyWriter {
  const sink = taskEventSink(chat);

  return {
    async history(_ctx, scope, conversationId, limit) {
      const stored = await store.latestMessages(tenantOf(scope), conversationId, limit);
      return stored.map(toTaskMessage);
    },

    async write(_ctx, scope, write) {
      if (!write.messageId || write.messageId === NilUUID) {
        return;
      }

      const message = await store.getMessage(tenantOf(scope), write.messageId);
      message.blocks = toChatBlocks(write.blocks);
      message.status = write.status;
      message.finishReason = write.finishReason;
      message.taskId = write.taskId;
      await store.update(tenantOf(scope), message);

      // The stream learns the answer grew, which is what a connected client
      // renders as the Bolu finishing its message. The payload is the same wire
      // shape the message endpoint returns, so the two cannot drift.
      await sink.emit(undefined, {
        workspaceId: message.workspaceId,
        type: EventMessageUpdated,
        actorAgentId: message.authorAgentId,
        actorUserId: "",
        conversationId: message.conversationId,
        taskId: message.taskId,
        draftId: "",
        payload: WireMessage(message),
      });
    },
  };
}

/**
 * Reads a message's files for the task runtime.
 *
 * The bytes live with the chat module: it resolves the upload inside its
 * workspace scope, so a file is read the same way whichever path asked for it.
 */
export function taskAttachmentReader(
  chat: {
    attachmentContent(
      scope: TenantScope,
      id: string,
    ): Promise<{ attachment: ChatAttachment; content: Uint8Array }>;
  } | null,
): TaskAttachmentReader {
  return {
    async readAttachment(_ctx, scope, attachmentId) {
      if (!chat) {
        throw new Error("chat: the conversation module is not configured");
      }
      const { attachment, content } = await chat.attachmentContent(
        tenantOf(scope),
        attachmentId,
      );
      return {
        filename: attachment.filename,
        contentType: attachment.contentType,
        content,
      };
    },
  };
}

/**
 * The memory one round answers with.
 *
 * Memory's own recall decides what a Bolu knows; the task runtime only asks for
 * it. The two blocks travel separately because the provider caches the stable
 * half and re-reads the retrieved one.
 */
export function memoryRecallForTask(
  memory: {
    recall(scope: TenantScope, request: MemoryRecallRequest): Promise<MemoryRecallResult>;
  } | null,
): TaskMemoryRecall | null {
  if (!memory) return null;

  return {
    async recall(_ctx, scope, request) {
      const result = await memory.recall(tenantOf(scope), {
        agentId: request.agentId,
        taskId: request.taskId,
        query: request.query,
      });
      return {
        Stable: result.stable,
        Retrieved: result.retrieved,
        Used: result.used,
      };
    },
  };
}

/**
 * The agent registry as the task runtime reads it.
 *
 * It answers two questions that look different and are the same lookup: which
 * Bolu a task runs as, and which Bolu a handoff named. The second is by name
 * because that is what the model knows — it is told the roster, not the ids.
 */
export function agentDirectoryForTask(
  service: {
    get(scope: TenantScope, id: string): Promise<AgentRecord>;
    list(scope: TenantScope): Promise<AgentRecord[]>;
  } | null,
): (TaskAgentDirectory & TaskAgentNamer) | null {
  if (!service) return null;

  const toRef = (agent: AgentRecord): TaskAgentRef => ({
    ID: agent.id,
    Name: agent.name,
    Role: agent.role ?? "",
    Persona: agent.persona ?? "",
    Tone: agent.tone ?? "",
    Stored: agent.status ?? "",
    Display: agent.display ?? agent.status ?? "",
    Tools: agent.tools ?? [],
    DefaultModel: agent.defaultModel ?? {},
  });

  return {
    async agent(_ctx, scope, id) {
      return toRef(await service.get(tenantOf(scope), id));
    },

    active(ref) {
      return agentActive(ref);
    },

    async byName(_ctx, scope, name) {
      const agents = await service.list(tenantOf(scope));
      const wanted = name.trim().toLowerCase();
      for (const agent of agents) {
        if (agent.name.toLowerCase() === wanted) {
          return toRef(agent);
        }
      }
      throw ErrAgentNotFound;
    },
  };
}

/** A tool with no adapter to ask is offered with an open schema; the hub validates on the call. */
const openSchema = { type: "object", additionalProperties: true };

/** Parses the adapter's declared schema, falling back to the open one. */
function schemaOf(raw: string): unknown {
  try {
    return JSON.parse(raw) as unknown;
  } catch {
    return openSchema;
  }
}

/**
 * The tools a Bolu was actually granted.
 *
 * Two filters are real: the grant (the workspace's catalogue joined against the
 * Bolu's profile) and an executor. A catalogue row no adapter claims is not
 * offered — teaching the model to call something that cannot run is worse than
 * omitting it.
 */
export function taskToolRegistry(
  agents: {
    allowedTools(scope: TenantScope, agentId: string): Promise<AgentToolRecord[]>;
  } | null,
  integration: { registry(): IntegrationRegistry | null } | null,
): TaskToolRegistry | null {
  if (!agents) return null;

  return {
    async tools(_ctx, scope, agentId) {
      const granted = await agents.allowedTools(tenantOf(scope), agentId);
      const registry = integration ? integration.registry() : null;
      const out: TaskTool[] = [];

      for (const tool of granted) {
        if (registry) {
          const found = registry.lookup(tool.name);
          if (!found) {
            continue;
          }
          out.push({
            Name: tool.name,
            Integration: tool.integration,
            Description: tool.description,
            Schema: schemaOf(found.spec.schema),
            Label: tool.label,
          });
          continue;
        }

        // A deployment with no hub offers the catalogue with an open schema
        // rather than hiding it: the executor says "not available" when a call
        // arrives, which is the honest answer.
        out.push({
          Name: tool.name,
          Integration: tool.integration,
          Description: tool.description,
          Schema: openSchema,
          Label: tool.label,
        });
      }

      return out;
    },
  };
}

/**
 * Runs the tool calls a task makes.
 *
 * Three of the product's rules live here: a `write_external` tool is refused
 * (the workflow routes those to the approval gate first), a tool the Bolu was
 * not granted is refused (the registry is asked, not the call), and the
 * connection is the Bolu's grant rather than the caller's word.
 */
export function taskToolExecutor(
  integration: {
    registry(): IntegrationRegistry | null;
    list(ctx: unknown, scope: IntegrationScope): Promise<IntegrationRecord[]>;
    allowedPermissions(
      ctx: unknown,
      scope: IntegrationScope,
      agentId: string,
    ): Promise<Map<string, string>>;
    execute(
      ctx: unknown,
      scope: IntegrationScope,
      integrationId: string,
      tool: string,
      args: string,
    ): Promise<IntegrationResult>;
  } | null,
  tools: TaskToolRegistry | null,
): TaskToolExecutor | null {
  if (!integration) return null;

  const grantedConnection = async (
    scope: TaskScope,
    agentId: string,
    app: string,
  ): Promise<string> => {
    const integrationScope: IntegrationScope = tenantOf(scope);
    const grants = await integration.allowedPermissions(undefined, integrationScope, agentId);
    const connections = await integration.list(undefined, integrationScope);

    for (const connection of connections) {
      if (connection.app !== app.trim() || connection.status !== StatusConnected) {
        continue;
      }
      if (grants.has(connection.id)) {
        return connection.id;
      }
    }

    throw wrap(
      ErrIntegrationNotFound,
      `tidak ada akun ${app} yang tersambung dan diberikan ke Bolu ini`,
    );
  };

  return {
    async execute(_ctx, scope, call: TaskToolCall) {
      const found = integration.registry()?.lookup(call.name) ?? null;
      if (!found) {
        throw wrap(ErrToolNotAvailable, call.name);
      }
      if (!tools) {
        throw wrap(ErrToolNotAvailable, call.name);
      }

      // The registry's list is the effective one: the Bolu's profile, the
      // workspace's catalogue, and the grants, filtered together. The app it
      // reports is what the connection is looked up by.
      const granted = await tools.tools(undefined, scope, call.agentId);
      const permission = granted.find((tool) => tool.Name === call.name);
      if (!permission) {
        throw wrap(ErrToolNotAvailable, call.name);
      }

      // The gate's rule, enforced where the call actually happens. The workflow
      // sends a `write_external` tool to the approval gate before an executor
      // sees it, so this is the second lock on the same door.
      if (permission.Label === LabelWriteExternal) {
        throw wrap(
          ErrToolNotAvailable,
          `${call.name} perlu persetujuan manusia; jalankan lewat gerbang persetujuan`,
        );
      }

      let integrationId = NilUUID;
      if (!isConnectionless(found.adapter) || found.adapter.needsConnection()) {
        integrationId = await grantedConnection(scope, call.agentId, permission.Integration);
      }

      const args =
        typeof call.arguments === "string" ? call.arguments : JSON.stringify(call.arguments ?? {});

      try {
        const result = await integration.execute(
          undefined,
          {
            userId: scope.userId ?? "",
            workspaceId: scope.workspaceId ?? "",
            agentId: call.agentId,
            taskId: call.taskId,
          },
          integrationId,
          call.name,
          args,
        );
        return { content: parseToolContent(result.content), isError: false };
      } catch (err) {
        // A tool that failed is answered rather than thrown: the model is told
        // what went wrong and may try another tool or answer with what it knows.
        return { content: { error: errorMessage(err) }, isError: true };
      }
    },
  };
}

/** A tool's answer is raw JSON; the runtime hands the model the document. */
function parseToolContent(content: string): unknown {
  try {
    return JSON.parse(content) as unknown;
  } catch {
    return content;
  }
}

/**
 * Turns a `write_external` action into a draft a person decides on.
 *
 * The approval module owns the policy: the task proposes an action and this
 * answers whether a person is asked, or whether it already ran.
 */
export function taskDraftGate(
  service: {
    request(
      ctx: AbortSignal | undefined,
      scope: ApprovalScope,
      req: ApprovalRequest,
    ): Promise<ApprovalOutcome>;
    carryOut(
      ctx: AbortSignal | undefined,
      scope: ApprovalScope,
      draftId: string,
    ): Promise<ApprovalOutcome>;
  } | null,
): TaskDraftGate | null {
  if (!service) return null;

  return {
    async requestDraft(_ctx, scope, request) {
      const outcome = await service.request(undefined, tenantOf(scope), {
        taskId: request.taskId,
        agentId: request.agentId,
        conversationId: request.conversationId,
        actionKind: request.actionKind,
        title: request.title,
        summary: request.title,
        payload: request.payload,
      });

      // A policy that allowed the action unattended is reported as an auto
      // draft that already ran, so the workflow records what happened instead
      // of parking on a signal nobody will send.
      return {
        id: outcome.draftId,
        title: outcome.title,
        status: outcome.status,
        note: outcome.reason,
        auto: outcome.executed,
        content: outcome.content,
      };
    },

    async carryOut(_ctx, scope, draftId) {
      try {
        const outcome = await service.carryOut(undefined, tenantOf(scope), draftId);
        return {
          status: outcome.status,
          content: outcome.content,
          held: outcome.notBefore !== null && !outcome.executed,
          notBefore: outcome.notBefore,
          reason: outcome.reason,
        };
      } catch (err) {
        // The approval module's permanent states become the task module's own:
        // the runtime has to tell them from a hiccup without importing the
        // module that decided it.
        if (approvalIs(err, ErrNoSender)) throw ErrDraftNoSender;
        if (approvalIs(err, ErrNotApproved)) throw ErrDraftNotApproved;
        throw err;
      }
    },
  };
}

/**
 * Reports a finished task's summary to the routine run it belongs to.
 *
 * A task opened by chat has no run, and that is not a failure — the sink simply
 * answers "unclaimed", which the activity ignores.
 */
export function taskOutcomeSink(
  routine: {
    recordTaskOutcome(
      scope: { userId?: string | null; workspaceId: string },
      taskId: string,
      outcome: RoutineOutcome,
    ): Promise<void>;
  } | null,
): TaskOutcomeSink {
  return {
    async report(_ctx, scope, report) {
      if (!routine) {
        throw ErrOutcomeUnclaimed;
      }
      await routine.recordTaskOutcome(tenantOf(scope), report.taskId, {
        status: report.status,
        summary: report.summary,
        reason: report.reason,
      });
    },
  };
}

/**
 * Refuses work a workspace can no longer afford.
 *
 * It is the gateway's own pre-flight check, asked earlier: the gateway refuses
 * the call and this refuses the round, so a task stops at a clean boundary
 * instead of halfway through a model call.
 */
export function taskBudget(
  gateway: { check(ctx: AbortSignal | undefined, workspaceId: string): Promise<void> } | null,
): TaskBudget | null {
  if (!gateway) return null;
  return {
    allow(ctx, workspaceId) {
      return gateway.check(ctx, workspaceId);
    },
  };
}
