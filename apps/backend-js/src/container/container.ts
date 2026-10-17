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
import { Handler as IntegrationHandler } from "../modules/integration/handler/handler.ts";
// checklist
import { PostgresRepository as ChecklistRepository } from "../modules/checklist/repository/postgres.ts";
import { ChecklistService } from "../modules/checklist/service/service.ts";
import { Handler as ChecklistHandler } from "../modules/checklist/handler/handler.ts";
// order
import { PostgresRepository as OrderRepository } from "../modules/order/repository/postgres.ts";
import { Service as OrderService } from "../modules/order/service/service.ts";
import { Handler as OrderHandler } from "../modules/order/handler/handler.ts";
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
  retention: RetentionHandler | null;
  memory: MemoryHandler | null;
  routine: RoutineHandler | null;
  whatsapp: WhatsAppHandler | null;
  task: TaskHandler | null;
  /** The guards call these directly; a handler keeps its service private. */
  authService: AuthService | null;
  workspaceService: WorkspaceService | null;
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
  costs: CostTable;
  /** The two loops `cmd/api.ts` runs in-process. */
  retentionService: RetentionService | null;
  billingService: BillingService | null;
  close: () => Promise<void>;
}

/** Builds the container. A dependency that cannot be opened is logged and skipped. */
export async function newContainer(overrides: { config?: Config } = {}): Promise<Container> {
  const config = overrides.config ?? (await loadConfig());
  const log = createLogger({
    level: config.logging.level,
    format: config.logging.format === "console" ? "console" : "json",
  });

  for (const warning of config.warnings()) log.warn(warning);

  // --- infrastructure -------------------------------------------------------
  let db: Database | null = null;
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
  if (db) {
    const service = new WorkspaceService({
      repository: new WorkspaceRepository(db),
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

    const integrationRepo = new IntegrationRepository(db);
    integrationService = new IntegrationService({ repository: integrationRepo, box, kms, logger: log } as any);
    integration = new IntegrationHandler(
      integrationService,
      log,
      {
        frontendUrl: config.application.frontendUrl,
        redirectBase: config.integrations.oauthRedirectBase,
      },
    );

    checklist = new ChecklistHandler(
      new ChecklistService({ repository: new ChecklistRepository(db), logger: log } as any),
      log,
    );

    order = new OrderHandler(
      new OrderService({ repository: new OrderRepository(db), logger: log } as any),
      log,
    );

    billingService = new BillingService({
      repository: new BillingRepository(db),
      counter: quota ? spendCounter(quota) : null,
      logger: log,
    } as any);
    billing = new BillingHandler(billingService, log);

    approvalService = new ApprovalService({
      drafts: new ApprovalRepository(db),
      logger: log,
    } as any);
    approval = new ApprovalHandler(approvalService, log);

    const retentionRepo = new RetentionRepository(db);
    retentionService = new RetentionService({ repository: retentionRepo, logger: log } as any);
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
    await db?.close().catch(() => undefined);
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
    retention,
    memory,
    routine,
    whatsapp,
    task,
    authService,
    workspaceService,
    chatService,
    chatRepo,
    llmService,
    agentService,
    memoryService,
    approvalService,
    integrationService,
    routineService,
    taskService,
    costs,
    retentionService,
    billingService,
    close,
  };
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
