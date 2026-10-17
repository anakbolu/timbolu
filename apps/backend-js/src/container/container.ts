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
} from "./adapters.ts";

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
  /** The guards call these directly; a handler keeps its service private. */
  authService: AuthService | null;
  workspaceService: WorkspaceService | null;
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
  if (db) {
    const service = new AgentService({
      repository: new AgentRepository(db),
      limits: agentPlanLimits(config.plan.maxAgentsPerWorkspace),
    });
    agent = new AgentHandler(service, log);
  }

  let llm: LlmHandler | null = null;
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
      costs: new CostTable(defaultPrices(), DefaultFallback),
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
    llm = new LlmHandler(service, log);
  }

  let admin: AdminHandler | null = null;
  if (db) {
    admin = new AdminHandler(new AdminService({ repository: new AdminRepository(db) }), log);
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
    authService,
    workspaceService,
    close,
  };
}

/** Minimal stand-in for the Go mail package's format helper. */
function formatLog(format: string, args: unknown[]): string {
  let i = 0;
  return format.replace(/%[svd]/g, () => String(args[i++]));
}
