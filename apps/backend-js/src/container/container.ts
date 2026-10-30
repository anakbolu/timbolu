/**
 * The assembly point: the only place that calls a constructor.
 *
 * Ported from apps/backend/internal/container. Nothing here holds business
 * logic — it reads the configuration, opens the dependencies, builds one service
 * per module, and hands the handlers to the router.
 *
 * A module whose dependencies are missing is left unbuilt: the router then
 * registers its documented routes as 503 instead of 404, which is what the Go
 * container does and what makes a half-configured deployment debuggable.
 */

import type { Config } from "../platform/config/config.ts";
import { load as loadConfig } from "../platform/config/init.ts";
import { createLogger, type Logger } from "../platform/logger.ts";
import { createDatabase, type Database } from "../platform/database/index.ts";
import { Client as RedisClient } from "../platform/redis/redis.ts";
import { LocalStore, type Store } from "../platform/storage/storage.ts";
import { S3Store } from "../platform/storage/s3.ts";
import { Local as KmsLocal } from "../platform/kms/kms.ts";
import { Box } from "../platform/crypto/secretbox.ts";
import { Issuer } from "../platform/authn/index.ts";
import { LogSender, type Sender as MailSender } from "../platform/mail/mail.ts";
import { AsyncSender } from "../platform/mail/async.ts";
import { SMTPSender } from "../platform/mail/smtp.ts";
import { Limiter } from "../platform/ratelimit/ratelimit.ts";
import { Counter as QuotaCounter } from "../platform/quota/counter.ts";
import { Publisher } from "../platform/pubsub/pubsub.ts";
import { Codec as OAuthStateCodec } from "../platform/oauthstate/state.ts";
import { Client as GoogleClient, DefaultAuthURL, DefaultTokenURL, DefaultUserInfoURL } from "../platform/oauth/google/google.ts";
import { Table as CostTable, defaults as defaultPrices, DefaultFallback } from "../platform/llm/pricing/table.ts";
import { Factory as ProviderFactory } from "../platform/llm/registry.ts";

import {
  authGoogle,
  authMailer,
  authPasswords,
  authStateSigner,
  authThrottle,
  authTokens,
  workspaceMailer,
  workspacePlanLimits,
  workspaceTokens,
  agentPlanLimits,
  agentDirectory,
  chatTaskStarter,
  chatMemory,
  routineTaskStarter,
  spendCounter,
  memoryConversations,
  memoryScheduler,
  agentDirectoryForTask,
  memoryRecallForTask,
  taskBudget,
  taskDraftGate,
  taskEventSink,
  taskOutcomeSink,
  taskReplyWriter,
  taskToolExecutor,
  taskToolRegistry,
} from "./adapters.ts";
import { newTemporalStarter } from "../worker/starter.ts";
import { Scheduler } from "../platform/routine/temporal/scheduler.ts";
import { WorkflowName as RoutineWorkflowName } from "../modules/routine/workflow/workflow.ts";
import { WorkflowName as MemoryWorkflowName, SignalActive as MemorySignalActive } from "../modules/memory/workflow/workflow.ts";
import { ModelResponder } from "../modules/chat/service/responder.ts";
import { GroupRouter } from "../modules/chat/service/router.ts";

// health
import { Repository as HealthRepository } from "../modules/health/repository/postgres.ts";
import { Service as HealthService } from "../modules/health/service/service.ts";
import { Handler as HealthHandler } from "../modules/health/handler/handler.ts";
// auth
import { Repository as AuthRepository } from "../modules/auth/repository/postgres.ts";
import { Service as AuthService } from "../modules/auth/service/service.ts";
import { Handler as AuthHandler } from "../modules/auth/handler/handler.ts";
// workspace
import { PostgresRepository as WorkspaceRepository } from "../modules/workspace/repository/postgres.ts";
import { Service as WorkspaceService } from "../modules/workspace/service/service.ts";
import { Handler as WorkspaceHandler } from "../modules/workspace/handler/handler.ts";
// agent
import { PostgresRepository as AgentRepository } from "../modules/agent/repository/postgres.ts";
import { Service as AgentService } from "../modules/agent/service/service.ts";
import { Handler as AgentHandler } from "../modules/agent/handler/handler.ts";
// llm
import { Repository as LlmRepository, AgentStore } from "../modules/llm/repository/postgres.ts";
import { Service as LlmService } from "../modules/llm/service/service.ts";
import { Handler as LlmHandler } from "../modules/llm/handler/handler.ts";

// chat
import { PostgresRepository as ChatRepository } from "../modules/chat/repository/postgres.ts";
import { Service as ChatService } from "../modules/chat/service/service.ts";
import { Handler as ChatHandler } from "../modules/chat/handler/handler.ts";
// integration
import { PostgresRepository as IntegrationRepository } from "../modules/integration/repository/postgres.ts";
import { IntegrationService } from "../modules/integration/service/service.ts";
import { newRegistry } from "../modules/integration/service/registry.ts";
import { Handler as IntegrationHandler } from "../modules/integration/handler/handler.ts";
import type { Adapter as IntegrationAdapter } from "../modules/integration/domain/adapter.ts";
import { errorMessage } from "../modules/integration/domain/integration.ts";
import { GmailAdapter } from "../modules/integration/adapters/gmail/gmail.ts";
import { SlackAdapter } from "../modules/integration/adapters/slack/slack.ts";
import { GithubAdapter } from "../modules/integration/adapters/github/github.ts";
// checklist
import { PostgresRepository as ChecklistRepository } from "../modules/checklist/repository/postgres.ts";
import { ChecklistService } from "../modules/checklist/service/service.ts";
import { ChecklistAdapter } from "../modules/checklist/adapter/adapter.ts";
import { Handler as ChecklistHandler } from "../modules/checklist/handler/handler.ts";
// order
import { PostgresRepository as OrderRepository } from "../modules/order/repository/postgres.ts";
import { Service as OrderService } from "../modules/order/service/service.ts";
import { Handler as OrderHandler } from "../modules/order/handler/handler.ts";
import { Adapter as OrderAdapter } from "../modules/order/adapter/adapter.ts";
// billing
import { PostgresRepository as BillingRepository } from "../modules/billing/repository/postgres.ts";
import { BillingService } from "../modules/billing/service/service.ts";
import { Handler as BillingHandler } from "../modules/billing/handler/handler.ts";
// approval
import { Repository as ApprovalRepository } from "../modules/approval/repository/postgres.ts";
import { Service as ApprovalService } from "../modules/approval/service/service.ts";
import { Handler as ApprovalHandler } from "../modules/approval/handler/handler.ts";
// retention
import { PostgresRepository as RetentionRepository } from "../modules/retention/repository/postgres.ts";
import { Service as RetentionService } from "../modules/retention/service/service.ts";
import { Handler as RetentionHandler } from "../modules/retention/handler/handler.ts";
// memory
import { PostgresRepository as MemoryRepository } from "../modules/memory/repository/postgres.ts";
import { Service as MemoryService } from "../modules/memory/service/service.ts";
import { Handler as MemoryHandler } from "../modules/memory/handler/handler.ts";
// routine
import { PostgresRepository as RoutineRepository } from "../modules/routine/repository/postgres.ts";
import { RoutineService } from "../modules/routine/service/service.ts";
import { Handler as RoutineHandler } from "../modules/routine/handler/handler.ts";
// whatsapp
import { PostgresRepository as WhatsAppRepository } from "../modules/whatsapp/repository/postgres.ts";
import { Service as WhatsAppService } from "../modules/whatsapp/service/service.ts";
import { Handler as WhatsAppHandler } from "../modules/whatsapp/handler/handler.ts";
// task
import { PostgresRepository as TaskRepository } from "../modules/task/repository/postgres.ts";
import { Service as TaskService } from "../modules/task/service/service.ts";
import { Handler as TaskHandler } from "../modules/task/handler/handler.ts";
// device
import { Repository as DeviceRepository } from "../modules/device/repository/postgres.ts";
import { Service as DeviceService } from "../modules/device/service/service.ts";
import { Handler as DeviceHandler } from "../modules/device/handler/handler.ts";
// push
import { buildNotifier } from "../modules/push/service/service.ts";
import { Client as ExpoClient } from "../modules/push/expo/client.ts";
import type { Notifier } from "../modules/push/domain/push.ts";
// admin
import { Repository as AdminRepository } from "../modules/admin/repository/postgres.ts";
import { Service as AdminService } from "../modules/admin/service/service.ts";
import { Handler as AdminHandler } from "../modules/admin/handler/handler.ts";

/** Everything a process needs, built once. */
export interface Container {
  config: Config;
  log: Logger;
  db: Database | null;
  redis: RedisClient | null;
  storage: Store | null;
  kms: KmsLocal | null;
  box: Box | null;
  issuer: Issuer | null;
  mailer: MailSender | null;
  limiter: Limiter | null;
  quota: QuotaCounter | null;
  publisher: Publisher | null;
  oauthState: OAuthStateCodec | null;
  google: GoogleClient | null;
  health: HealthHandler | null;
  auth: AuthHandler | null;
  workspace: WorkspaceHandler | null;
  agent: AgentHandler | null;
  llm: LlmHandler | null;
  admin: AdminHandler | null;
  chat: ChatHandler | null;
  integration: IntegrationHandler | null;
  checklist: ChecklistHandler | null;
  order: OrderHandler | null;
  billing: BillingHandler | null;
  approval: ApprovalHandler | null;
  device: DeviceHandler | null;
  retention: RetentionHandler | null;
  memory: MemoryHandler | null;
  routine: RoutineHandler | null;
  whatsapp: WhatsAppHandler | null;
  task: TaskHandler | null;
  /** The guards call these directly; a handler keeps its service private. */
  authService: AuthService | null;
  workspaceService: WorkspaceService | null;
  /** The repository onboarding writes through, which is what the trial provisioner is installed on. */
  workspaceRepository: WorkspaceRepository | null;
  /** The services the task runtime's ports read. The worker assembles its activities from them. */
  chatService: ChatService | null;
  chatRepo: ChatRepository | null;
  llmService: LlmService | null;
  agentService: AgentService | null;
  memoryService: MemoryService | null;
  approvalService: ApprovalService | null;
  integrationService: IntegrationService | null;
  routineService: RoutineService | null;
  taskService: TaskService | null;
  /** Wakes a person's phones; the worker hands it to the task activities. */
  pushService: Notifier | null;
  costs: CostTable;
  /** The two loops `cmd/api.ts` runs in-process. */
  retentionService: RetentionService | null;
  billingService: BillingService | null;
  close: () => Promise<void>;
}

/** Builds the container. A dependency that cannot be opened is logged and skipped. */
export async function newContainer(
  overrides: { config?: Config; db?: Database } = {},
): Promise<Container> {
  const config = overrides.config ?? (await loadConfig());
  const log = createLogger({
    level: config.logging.level,
    format: config.logging.format === "console" ? "console" : "json",
  });

  for (const warning of config.warnings()) log.warn(warning);

  // --- infrastructure -------------------------------------------------------
  // A caller-supplied pool is already open and is neither pinged nor closed
  // here: it belongs to the caller, which is how a test drives the assembly
  // without a Postgres.
  const ownsDb = overrides.db === undefined;
  let db: Database | null = overrides.db ?? null;
  if (ownsDb) {
    if (config.database.url) {
      try {
        db = createDatabase({ url: config.database.url });
        await db.ping();
      } catch (err) {
        log.error({ err }, "database is unreachable; starting without it");
        db = null;
      }
    } else {
      log.warn("DATABASE_URL is empty: starting without a database");
    }
  }

  let redis: RedisClient | null = null;
  if (config.redis.url) {
    try {
      redis = await RedisClient.create(config.redis.url);
    } catch (err) {
      log.error({ err }, "redis is unreachable; starting without it");
      redis = null;
    }
  }

  let storage: Store | null = null;
  if (config.storage.driver === "s3") {
    try {
      storage = await S3Store.create({
        endpoint: config.storage.endpoint,
        region: config.storage.region,
        bucket: config.storage.bucket,
        accessKey: config.storage.accessKey,
        secretKey: config.storage.secretKey,
        useSSL: config.storage.useSsl,
        pathStyle: config.storage.pathStyle,
      });
    } catch (err) {
      log.error({ err }, "object store is unreachable; attachments are unavailable");
    }
  } else if (config.storage.localRoot) {
    try {
      storage = await LocalStore.create(config.storage.localRoot);
    } catch (err) {
      log.error({ err }, "local object store could not open");
    }
  }

  const kms = KmsLocal.newLocalFromConfig(config.kms.masterKey, config.kms.masterKeyId, config.kms.retiredKeys);

  let box: Box | null = null;
  if (config.llm.secretEncryptionKey) {
    try {
      box = new Box(Buffer.from(config.llm.secretEncryptionKey, "base64"));
    } catch (err) {
      log.error({ err }, "SECRET_ENCRYPTION_KEY is not usable; provider keys cannot be stored");
    }
  }

  // The access token's lifetime: the configured minutes, or fifteen when unset —
  // the same fallback the Go container's durationMinutes applies.
  const accessTokenMinutes = config.auth.accessTokenMinutes > 0 ? config.auth.accessTokenMinutes : 15;

  let issuer: Issuer | null = null;
  if (config.jwt.secret) {
    try {
      issuer = new Issuer(config.jwt.secret, accessTokenMinutes * 60);
    } catch (err) {
      log.error({ err }, "JWT_SECRET is not usable; authentication is unavailable");
    }
  }

  // --- mail -----------------------------------------------------------------
  let mailer: MailSender | null = null;
  if (config.mail.driver === "smtp" && config.mail.host) {
    mailer = new AsyncSender(
      SMTPSender.create({
        host: config.mail.host,
        port: config.mail.port,
        username: config.mail.username,
        password: config.mail.password,
        from: config.mail.from,
        timeoutMs: config.mail.timeoutSeconds * 1000,
      }),
    );
  } else {
    // The log driver is what local development and the demo run: the message is
    // written to the log instead of sent.
    mailer = new LogSender((fmt, ...args) => log.info(formatLog(fmt, args)));
  }

  const limiter = new Limiter(redis, (err) => log.warn({ err }, "rate limiter degraded"));
  const quota = new QuotaCounter(redis, (err) => log.warn({ err }, "quota counter degraded"));
  const publisher = new Publisher(redis);
  const oauthState = OAuthStateCodec.create(config.integrations.oauthStateKey);
  const google = config.google.clientId
    ? new GoogleClient({
        clientId: config.google.clientId,
        clientSecret: config.google.clientSecret,
        redirectUrl: config.google.redirectUrl,
        authUrl: DefaultAuthURL,
        tokenUrl: DefaultTokenURL,
        userInfoUrl: DefaultUserInfoURL,
      })
    : null;

  // --- handlers -------------------------------------------------------------
  const health = new HealthHandler(
    new HealthService(db ? new HealthRepository(db) : null, redis),
    log,
  );

  let auth: AuthHandler | null = null;
  let authService: AuthService | null = null;
  if (db && issuer) {
    const service = new AuthService({
      repository: new AuthRepository(db),
      issuer,
      states: authStateSigner(oauthState, config.auth.stateMinutes > 0 ? config.auth.stateMinutes : 10),
      mailer: authMailer(mailer, config.mail.from),
      throttle: authThrottle(limiter),
      google: authGoogle(google),
      passwords: authPasswords,
      tokens: authTokens,
      // The service's own Config: lifetimes in milliseconds, the same units the
      // Go service takes a time.Duration for.
      config: {
        refreshTTL: (config.auth.refreshHours > 0 ? config.auth.refreshHours : 720) * 3600 * 1000,
        verificationTTL: (config.auth.verificationHours > 0 ? config.auth.verificationHours : 24) * 3600 * 1000,
        resetTTL: (config.auth.resetMinutes > 0 ? config.auth.resetMinutes : 30) * 60 * 1000,
        frontendURL: config.application.frontendUrl,
        loginThrottleCapacity: config.auth.loginThrottleCapacity,
        loginThrottleRefill: config.auth.loginThrottleRefill,
        requireEmailVerification: config.auth.requireEmailVerification,
      },
      logf: (fmt, ...args) => log.warn(formatLog(fmt, args)),
    });
    authService = service;
    auth = new AuthHandler(service, log, {
      cookie: {
        path: "/api/auth",
        domain: config.auth.cookieDomain,
        secure: config.auth.cookieSecure,
        sameSite: "Lax",
      },
      frontendURL: config.application.frontendUrl,
    });
  }

  let workspace: WorkspaceHandler | null = null;
  let workspaceService: WorkspaceService | null = null;
  let workspaceRepository: WorkspaceRepository | null = null;
  if (db) {
    const repository = new WorkspaceRepository(db);
    workspaceRepository = repository;
    const service = new WorkspaceService({
      repository,
      mailer: workspaceMailer(mailer, config.mail.from),
      tokens: workspaceTokens,
      limits: workspacePlanLimits(config.plan.maxAgentsPerWorkspace > 0 ? config.plan.maxAgentsPerWorkspace : 50),
    });
    workspaceService = service;
    workspace = new WorkspaceHandler(service, log);
  }

  let agent: AgentHandler | null = null;
  let agentService: AgentService | null = null;
  if (db) {
    const service = new AgentService({
      repository: new AgentRepository(db),
      limits: agentPlanLimits(config.plan.maxAgentsPerWorkspace),
    });
    agentService = service;
    agent = new AgentHandler(service, log);
  }

  // One price table for the gateway and for the task runtime: a task's own
  // total and the invoice must price a call the same way.
  const costs = new CostTable(defaultPrices(), DefaultFallback);

  let llm: LlmHandler | null = null;
  let llmService: LlmService | null = null;
  if (db) {
    const repo = new LlmRepository(db);
    const service = new LlmService({
      store: repo,
      agents: new AgentStore(db, box),
      factory: new ProviderFactory(),
      box,
      recorder: repo,
      reader: repo,
      counter: quota,
      // The quota policy belongs to the billing module. Until it is wired the
      // gateway runs without a policy, which is the same shape as a deployment
      // whose quota switch is off.
      quota: null,
      costs,
      // The deployment default is a provider row that exists only in
      // configuration: it has no workspace and no id until it is persisted, and
      // it is never the default of a workspace it does not belong to.
      default: {
        id: "",
        workspaceId: "",
        name: "default",
        adapter: config.llm.default.adapter,
        baseUrl: config.llm.default.baseUrl,
        model: config.llm.default.model,
        apiKey: config.llm.default.apiKey,
        priority: 0,
        maxTokens: config.llm.default.maxTokens,
        isDefault: true,
        contextTokens: config.llm.default.contextTokens,
        enabled: true,
        createdAt: new Date(0),
        updatedAt: new Date(0),
      },
      // Empty means a provider's own configured model is used for embeddings,
      // which is what the Go container passes when the deployment names none.
      embeddingModel: "",
      auditor: null,
      logger: log,
      tracer: null,
      metrics: null,
      settings: {
        quotaEnabled: config.llm.quotaEnabled,
        chainLimit: config.llm.chainLimit,
      },
    });
    llmService = service;
    llm = new LlmHandler(service, log);
  }

  let admin: AdminHandler | null = null;
  if (db) {
    admin = new AdminHandler(new AdminService({ repository: new AdminRepository(db) }), log);
  }


  // --- the rest of the modules ---------------------------------------------
  let chat: ChatHandler | null = null;
  let chatService: ChatService | null = null;
  let chatRepo: ChatRepository | null = null;
  let integration: IntegrationHandler | null = null;
  let integrationService: IntegrationService | null = null;
  let checklist: ChecklistHandler | null = null;
  let order: OrderHandler | null = null;
  let billing: BillingHandler | null = null;
  let billingService: BillingService | null = null;
  let approval: ApprovalHandler | null = null;
  let approvalService: ApprovalService | null = null;
  let device: DeviceHandler | null = null;
  let pushService: Notifier | null = null;
  let retention: RetentionHandler | null = null;
  let retentionService: RetentionService | null = null;
  let memory: MemoryHandler | null = null;
  let memoryService: MemoryService | null = null;
  let routine: RoutineHandler | null = null;
  let routineService: RoutineService | null = null;
  let whatsapp: WhatsAppHandler | null = null;
  let task: TaskHandler | null = null;
  let taskService: TaskService | null = null;

  // One Temporal connection per process, shared by the task starter and the
  // routine scheduler.
  const runtime = await newTemporalStarter(config, log);

  // The push kill switch says so once, at startup, so an operator asking why a
  // finished task rang no phone finds the answer in the log rather than in the
  // code. The device registry keeps working either way.
  if (!config.push.enabled) {
    log.warn(
      "push notifications are disabled: PUSH_ENABLED is not true, so nothing is sent to exp.host",
    );
  }

  if (db) {
    // Each service takes the ports it declares and nothing more; the optional ones
    // stay null, which is the same shape as a deployment that has not connected
    // that dependency yet.
    chatRepo = new ChatRepository(db);
    const directory = agentService ? agentDirectory(agentService as any) : null;

    // A message is answered in process by the model gateway, and becomes a
    // durable task when the runtime is configured. The task runtime needs the
    // registry to know which Bolu the message addresses, so both must be present.
    const responder = llmService
      ? new ModelResponder({
          gateway: llmService as any,
          storage,
          limits: {
            maxFileBytes: config.attachmentContext.maxFileBytes,
            maxCharsPerFile: config.attachmentContext.maxCharsPerFile,
            maxCharsTotal: config.attachmentContext.maxCharsTotal,
            maxImages: config.attachmentContext.maxImages,
            maxImageBytes: config.attachmentContext.maxImageBytes,
          } as any,
          logger: log,
        })
      : null;

    if (!responder || !directory) {
      log.warn("chat replies are disabled: the model gateway or the agent registry is not configured");
    }

    chatService = new ChatService({
        conversations: chatRepo,
        messages: chatRepo,
        attachments: chatRepo,
        events: chatRepo,
        publisher,
        agents: directory as any,
        responder: responder as any,
        router: responder && directory ? new GroupRouter({ gateway: llmService as any, agents: directory as any, logger: log }) : null,
        // Chat is built before the task runtime, so these read the container's
        // field at call time rather than capturing a null.
        tasks: {
          start: async (scope: any, req: any) => {
            if (!taskService) throw new Error("chat: the task runtime is not configured");
            return chatTaskStarter(taskService as any).start(scope, req);
          },
        },
        memory: {
          remember: async (scope: any, req: any) => {
            if (!memoryService) throw new Error("chat: memory is not configured");
            return chatMemory(memoryService as any).remember(scope, req);
          },
          conversationActive: async (scope: any, conversationId: string, agentId: string) => {
            if (!memoryService) return;
            return chatMemory(memoryService as any).conversationActive(scope, conversationId, agentId);
          },
        },
        storage,
        logger: log,
        config: { attachmentAllowedExtensions: config.storage.attachmentAllowedExtensions },
      } as any);
    chat = new ChatHandler(chatService, log);

    // The modules the hub's own adapters serve. They are built before the hub
    // because an adapter hands its tools straight to its module; the Go tree
    // resolves them at call time only because there the hub is assembled first.
    const orderService = new OrderService({ repository: new OrderRepository(db), logger: log });
    const checklistService = new ChecklistService({ repository: new ChecklistRepository(db), logger: log });

    const integrationRepo = new IntegrationRepository(db);

    // The adapters are what declare the tools, their labels, and their schemas,
    // so the registry is built before anything reads the hub. A refusal here is a
    // bug in one adapter, and it is said while the process is starting rather
    // than left for the first model call.
    const registry = newRegistry();
    for (const adapter of integrationAdapters(config, log, orderService, checklistService)) {
      try {
        registry.register(adapter);
      } catch (err) {
        throw new Error(`integration: register the ${adapter.app()} adapter: ${errorMessage(err)}`);
      }
    }

    integrationService = new IntegrationService({
      repository: integrationRepo,
      // Nothing else is wired yet: tokens, schemas, state and triggers are ports
      // this deployment has not filled, and the service refuses those paths
      // rather than pretending they work.
      cipher: null,
      registry,
      logger: log,
      clock: null,
      limits: null,
      auditor: null,
      validator: null,
      states: null,
      stateStore: null,
      refs: null,
      queue: null,
      triggers: null,
      limiter: null,
      mcp: null,
      mcpClients: null,
    });
    integration = new IntegrationHandler(
      integrationService,
      log,
      {
        frontendUrl: config.application.frontendUrl,
        redirectBase: config.integrations.oauthRedirectBase,
      },
    );

    checklist = new ChecklistHandler(checklistService, log);
    order = new OrderHandler(orderService, log);

    const billingRepository = new BillingRepository(db);
    billingService = new BillingService({
      repository: billingRepository,
      counter: quota ? spendCounter(quota) : null,
      logger: log,
    } as any);
    billing = new BillingHandler(billingService, log);

    // A new workspace gets its trial package inside the onboarding transaction, so
    // "exactly one live subscription" is a fact about the data rather than a rule
    // every read has to remember. Billing is assembled after the workspace module,
    // so the provisioner is installed here — at the one point where both exist —
    // rather than at the workspace's own wiring, which would need it first.
    if (workspaceRepository) workspaceRepository.setSubscriptionProvisioner(billingRepository);

    // Push notifications. The sender reads addresses from the device registry
    // and hands the batches to Expo; both env values are optional, and an
    // absent access token is the normal state for a project that has not
    // enabled Expo's enhanced security. Built before the approval service
    // because a new draft is the first thing worth ringing about. `enabled`
    // comes from PUSH_ENABLED, so an off deployment gets a notifier that
    // answers null without touching the registry or the network.
    pushService = buildNotifier({
      enabled: config.push.enabled,
      devices: new DeviceRepository(db),
      expo: new ExpoClient({ logger: log }),
      logger: log,
    });

    approvalService = new ApprovalService({
      drafts: new ApprovalRepository(db),
      notifications: pushService,
      logger: log,
    });
    approval = new ApprovalHandler(approvalService, log);

    // The registry of Expo push addresses. It needs nothing but the pool: the
    // policy on `device_tokens` is keyed on the user, so the service never asks
    // another module who the caller is.
    device = new DeviceHandler(
      new DeviceService({ repository: new DeviceRepository(db) }),
      log,
    );

    // Erasure is the half of this module a customer can invoke, and it needs
    // all three ports to be honest: the evidence, the workspace deletion, and
    // the bucket walk. They are passed structurally so the compiler keeps that
    // shape — an omitted port used to be hidden by a cast, and the result was a
    // deployed service answering 503 `erasure_unavailable` to every request.
    // Anything still null here is a deployment that cannot erase, which the
    // service says rather than a state the cast should cover.
    const retentionRepo = new RetentionRepository(db);
    retentionService = new RetentionService({
      repository: retentionRepo,
      logger: log,
      erasures: retentionRepo,
      workspaces: workspaceRepository,
      objects: chatService,
    });
    retention = new RetentionHandler(retentionService, log);

    // The embedder and the model are the same gateway object: memory imports the
    // llm domain's ports, so the service satisfies both structurally. With no
    // provider configured the writes that need a vector answer with the reason
    // instead of pretending they stored something.
    memoryService = new MemoryService({
      repository: new MemoryRepository(db),
      embedder: llmService as any,
      model: llmService as any,
      // The thread an extraction reads, and the debounce that decides when it
      // runs. Without both, extraction is configured and never happens.
      threads: memoryConversations(chatRepo as any, agentService as any),
      scheduler: runtime
        ? memoryScheduler(
            clientHandleOver(runtime.client, config.temporal.taskQueueAgent || "agent"),
            config.memory.quietSeconds * 1000,
            MemoryWorkflowName,
            MemorySignalActive,
          )
        : null,
      limits: { maxContentRunes: config.memory.maxContentRunes } as any,
      logger: log,
    } as any);
    memory = new MemoryHandler(memoryService, log);

    routineService = new RoutineService({
        repository: new RoutineRepository(db),
        tasks: {
          start: async (scope: any, req: any) => {
            if (!taskService) throw new Error("routine: the task runtime is not configured");
            return routineTaskStarter(taskService as any).start(scope, req);
          },
        },
        // The schedule engine is the platform scheduler over the same Temporal
        // client: without it a routine is stored and never fires. The scheduler
        // wants the platform's ClientHandle (it calls .client() and
        // .StartOptions()), so the already-open client is presented that way
        // rather than opening a second connection to the same frontend.
        engine: runtime
          ? Scheduler.create(clientHandleOver(runtime.client, config.temporal.taskQueueAgent || "agent"), config.temporal.taskQueueAgent || "agent", RoutineWorkflowName)
          : null,
        logger: log,
      } as any);
    routine = new RoutineHandler(routineService, log);

    const taskRepo = new TaskRepository(db);
    // The starter is the Temporal seam: without it a task is recorded and stays
    // queued, which is the documented behaviour of a deployment with no runtime.
    // The other ports are the ones the service asks before it opens a task: the
    // registry it validates the Bolu against, the stream it announces the task
    // on, and the quota check that refuses work a workspace cannot afford.
    taskService = new TaskService({
      tasks: taskRepo,
      steps: taskRepo,
      starter: runtime?.starter ?? null,
      agents: agentDirectoryForTask(agentService),
      events: chatService ? taskEventSink(chatService) : null,
      budget: taskBudget(llmService),
      limits: {
        MaxSteps: config.tasks.maxSteps,
        MaxTokens: config.tasks.maxTokens,
        MaxHandoffDepth: config.tasks.maxHandoffDepth,
        MaxToolResultBytes: config.tasks.maxToolResultBytes,
        MaxRevisions: 0,
      },
      logger: log,
    });
    task = new TaskHandler(taskService, log);

    whatsapp = new WhatsAppHandler(
      new WhatsAppService({
        repository: new WhatsAppRepository(db),
        sessions: { forget: async () => undefined } as any,
        storage,
        hourlyLimit: config.whatsApp.hourlyLimit,
        logger: log,
      } as any),
      log,
    );
  }

  const close = async (): Promise<void> => {
    if (ownsDb) await db?.close().catch(() => undefined);
    await redis?.close().catch(() => undefined);
  };

  return {
    config,
    log,
    db,
    redis,
    storage,
    kms,
    box,
    issuer,
    mailer,
    limiter,
    quota,
    publisher,
    oauthState,
    google,
    health,
    auth,
    workspace,
    agent,
    llm,
    admin,
    chat,
    integration,
    checklist,
    order,
    billing,
    approval,
    device,
    retention,
    memory,
    routine,
    whatsapp,
    task,
    authService,
    workspaceService,
    workspaceRepository,
    chatService,
    chatRepo,
    llmService,
    agentService,
    memoryService,
    approvalService,
    integrationService,
    routineService,
    taskService,
    pushService,
    costs,
    retentionService,
    billingService,
    close,
  };
}

/**
 * Lists the applications this deployment speaks.
 *
 * It is the only place an application is named: adding one is adding a line
 * here, and the agent worker, the approval gate, and the tool registry never
 * learn that it happened. Ported from `integrationAdapters` in
 * apps/backend/internal/container/integration.go.
 *
 * A deployment that has not registered an OAuth application for Gmail, Slack, or
 * GitHub simply does not offer it, rather than offering it and failing at the
 * consent screen.
 */
function integrationAdapters(
  config: Config,
  log: Logger,
  orders: OrderService,
  plans: ChecklistService,
): IntegrationAdapter[] {
  // The workspace's own data and the plan a Bolu keeps for itself are the
  // applications that are not accounts somebody connected: there is no provider,
  // no token, and nothing outside this deployment to reach. They are offered
  // whenever there is storage to answer from, which is what this block runs under.
  const adapters: IntegrationAdapter[] = [new OrderAdapter(orders), new ChecklistAdapter(plans)];

  const { integrations } = config;

  if (integrations.gmailClientId.trim() !== "" && integrations.gmailClientSecret.trim() !== "") {
    try {
      adapters.push(
        new GmailAdapter({
          clientId: integrations.gmailClientId,
          clientSecret: integrations.gmailClientSecret,
          apiBase: integrations.gmailApiBase,
          authEndpoint: integrations.gmailAuthEndpoint,
          tokenEndpoint: integrations.gmailTokenEndpoint,
        }),
      );
    } catch (err) {
      // Said rather than fatal: the rest of the hub works without Gmail, and a
      // deployment that mistyped a credential should still serve.
      log.error({ err }, "the Gmail adapter is disabled");
    }
  }

  if (integrations.slackClientId.trim() !== "" && integrations.slackClientSecret.trim() !== "") {
    try {
      adapters.push(
        new SlackAdapter({
          clientId: integrations.slackClientId,
          clientSecret: integrations.slackClientSecret,
          apiBase: integrations.slackApiBase,
          authEndpoint: integrations.slackAuthEndpoint,
          tokenEndpoint: integrations.slackTokenEndpoint,
        }),
      );
    } catch (err) {
      log.error({ err }, "the Slack adapter is disabled");
    }
  }

  if (integrations.gitHubClientId.trim() !== "" && integrations.gitHubClientSecret.trim() !== "") {
    try {
      adapters.push(
        new GithubAdapter({
          clientId: integrations.gitHubClientId,
          clientSecret: integrations.gitHubClientSecret,
          apiBase: integrations.gitHubApiBase,
          authEndpoint: integrations.gitHubAuthEndpoint,
          tokenEndpoint: integrations.gitHubTokenEndpoint,
        }),
      );
    } catch (err) {
      log.error({ err }, "the GitHub adapter is disabled");
    }
  }

  return adapters;
}

/**
 * Presents an open Temporal client as the platform's `ClientHandle`.
 *
 * `ClientHandle.connect` is the canonical way to get one, and it health-checks
 * the endpoint. The task starter already opened a connection, so wrapping it
 * here keeps one connection per process; the follow-up is to build both from a
 * single `ClientHandle`.
 */
function clientHandleOver(client: unknown, taskQueue: string): any {
  return {
    client: () => client,
    StartOptions: (workflowId: string) => ({ taskQueue, workflowId }),
  };
}

/** Minimal stand-in for the Go mail package's format helper. */
function formatLog(format: string, args: unknown[]): string {
  let i = 0;
  return format.replace(/%[svd]/g, () => String(args[i++]));
}
