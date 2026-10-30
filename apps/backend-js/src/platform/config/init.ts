/**
 * Configuration loading.
 *
 * Ported from packages/platform/config/init.go. Loading order is
 * environment file → deep merge → environment-variable override. The base file
 * supplies every default and `config-<environment>.yaml` overlays what differs;
 * local reads its own file alone.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";

import type { Config } from "./config";
import { formatDuration, parseDuration } from "./config";
import { parsePushEnabled } from "./push";
import { validate } from "./validation";

const LOCAL_ENV = "local";
const ENV_KEY = "ENVIRONMENT";

/**
 * Reads the YAML config for the current ENVIRONMENT (local uses
 * config-local.yaml, anything else config.yaml) and applies environment
 * variable overrides on top of it.
 */
export async function load(): Promise<Config> {
  const cfg = loadFromFile(process.env[ENV_KEY] ?? "");

  const warnings: string[] = [];
  cfg.warnings = () => warnings;

  loadFromEnv(cfg, warnings);
  validate(cfg);

  return cfg;
}

/** Reads and merges the config files for an environment. */
export function loadFromFile(env: string): Config {
  const merged: Record<string, unknown> = {};
  for (const name of configNames(env)) {
    const layer = readConfigLayer(name);
    if (layer === null) continue;
    mergeLayer(merged, layer);
  }

  // A deployment with no config file at all is still valid: the environment can
  // supply every required value, which is what the container images rely on.
  return decodeConfig(merged);
}

/**
 * The files an environment reads, base first.
 *
 * The overlay exists so staging and production carry only the values that
 * differ instead of two hundred-line copies of the base that drift apart.
 */
export function configNames(env: string): string[] {
  const trimmed = env.trim();
  if (trimmed === "" || trimmed === LOCAL_ENV) {
    return ["config-local"];
  }
  return ["config", `config-${trimmed}`];
}

/**
 * Where a config file may live, in the order they are tried.
 *
 * The first two cover a process started from the app directory (the normal
 * development and test case) and one started from the repository root; the
 * rest mirror the layouts a container image uses.
 */
const CONFIG_PATHS = [
  "./src/platform/config/",
  "./apps/backend-js/src/platform/config/",
  "../../packages/platform/config/",
  "./internal/platform/config/",
  "./config/",
  ".",
];

/** Reads one file into a map, or null when it is absent. */
function readConfigLayer(name: string): Record<string, unknown> | null {
  for (const dir of CONFIG_PATHS) {
    const path = join(dir, `${name}.yaml`);
    let content: string;
    try {
      content = readFileSync(path, "utf8");
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === "ENOENT") continue;
      throw new Error(`read ${path}: ${(err as Error).message}`);
    }

    let parsed: unknown;
    try {
      parsed = Bun.YAML.parse(content);
    } catch (err) {
      throw new Error(`parse ${path}: ${(err as Error).message}`);
    }

    if (parsed === null || parsed === undefined) return {};
    if (typeof parsed !== "object" || Array.isArray(parsed)) {
      throw new Error(`parse ${path}: expected a mapping at the top level`);
    }
    return parsed as Record<string, unknown>;
  }

  return null;
}

/**
 * Writes one file's values over another's, section by section.
 *
 * The merge is deep rather than whole-key: an overlay that names only
 * `Http.ReadTimeout` must not erase the rest of the `Http` block.
 */
export function mergeLayer(into: Record<string, unknown>, layer: Record<string, unknown>): void {
  for (const [key, value] of Object.entries(layer)) {
    if (typeof value !== "object" || value === null || Array.isArray(value)) {
      into[key] = value;
      continue;
    }

    const existing = into[key];
    if (typeof existing !== "object" || existing === null || Array.isArray(existing)) {
      into[key] = value;
      continue;
    }

    mergeLayer(existing as Record<string, unknown>, value as Record<string, unknown>);
  }
}

// ------------------------------------------------------------------ decoding

type Kind = "string" | "int" | "float" | "bool" | "duration" | "strings";

interface FieldSpec {
  kind: Kind;
  optional?: boolean;
}

interface SectionSpec {
  kind: "section";
  fields: Record<string, AnySpec>;
  optional?: boolean;
}

type AnySpec = FieldSpec | SectionSpec;

const SECTIONS: Record<string, SectionSpec> = {
  Application: {
    kind: "section",
    fields: {
      Environment: { kind: "string" },
      FrontendURL: { kind: "string", optional: true },
      CORSOrigins: { kind: "strings", optional: true },
    },
  },
  Http: {
    kind: "section",
    fields: {
      OpsAddress: { kind: "string", optional: true },
      InternalAddress: { kind: "string", optional: true },
      Address: { kind: "string" },
      ReadTimeout: { kind: "duration" },
      WriteTimeout: { kind: "duration" },
      ApiPrefix: { kind: "string" },
    },
  },
  Database: {
    kind: "section",
    fields: {
      URL: { kind: "string", optional: true },
      MaxOpenConns: { kind: "int", optional: true },
      MaxIdleConns: { kind: "int", optional: true },
      ConnMaxLifetime: { kind: "int", optional: true },
    },
  },
  Redis: {
    kind: "section",
    fields: {
      URL: { kind: "string", optional: true },
    },
  },
  Temporal: {
    kind: "section",
    fields: {
      HostPort: { kind: "string", optional: true },
      Namespace: { kind: "string", optional: true },
      TaskQueueAgent: { kind: "string", optional: true },
      TaskQueueIntegration: { kind: "string", optional: true },
      TLS: { kind: "bool" },
      APIKey: { kind: "string", optional: true },
      UIBaseURL: { kind: "string", optional: true },
    },
  },
  JWT: {
    kind: "section",
    fields: {
      Secret: { kind: "string" },
      ExpireHours: { kind: "int", optional: true },
    },
  },
  Auth: {
    kind: "section",
    fields: {
      AccessTokenMinutes: { kind: "int", optional: true },
      RefreshHours: { kind: "int", optional: true },
      VerificationHours: { kind: "int", optional: true },
      ResetMinutes: { kind: "int", optional: true },
      StateMinutes: { kind: "int", optional: true },
      CookieDomain: { kind: "string", optional: true },
      CookieSecure: { kind: "bool" },
      LoginThrottleCapacity: { kind: "int", optional: true },
      LoginThrottleRefill: { kind: "float", optional: true },
      RequireEmailVerification: { kind: "bool", optional: true },
    },
  },
  Google: {
    kind: "section",
    fields: {
      ClientID: { kind: "string", optional: true },
      ClientSecret: { kind: "string", optional: true },
      RedirectURL: { kind: "string", optional: true },
    },
  },
  Mail: {
    kind: "section",
    fields: {
      Driver: { kind: "string", optional: true },
      From: { kind: "string", optional: true },
      Host: { kind: "string", optional: true },
      Port: { kind: "int", optional: true },
      Username: { kind: "string", optional: true },
      Password: { kind: "string", optional: true },
      TimeoutSeconds: { kind: "int", optional: true },
    },
  },
  Plan: {
    kind: "section",
    fields: {
      MaxAgentsPerWorkspace: { kind: "int", optional: true },
      DailyCostMicros: { kind: "int", optional: true },
    },
  },
  Llm: {
    kind: "section",
    fields: {
      SecretEncryptionKey: { kind: "string", optional: true },
      Default: {
        kind: "section",
        fields: {
          Adapter: { kind: "string", optional: true },
          BaseURL: { kind: "string", optional: true },
          Model: { kind: "string", optional: true },
          APIKey: { kind: "string", optional: true },
          MaxTokens: { kind: "int", optional: true },
          ContextTokens: { kind: "int", optional: true },
        },
      },
      QuotaEnabled: { kind: "bool" },
      TokensPerPeriod: { kind: "int" },
      ChainLimit: { kind: "int" },
      TextOnlyModels: { kind: "strings", optional: true },
    },
  },
  Kms: {
    kind: "section",
    fields: {
      MasterKey: { kind: "string", optional: true },
      MasterKeyID: { kind: "string", optional: true },
      RetiredKeys: { kind: "string", optional: true },
    },
  },
  Integrations: {
    kind: "section",
    fields: {
      OAuthStateKey: { kind: "string", optional: true },
      OAuthRedirectBase: { kind: "string", optional: true },
      DefaultCallsPerMinute: { kind: "int", optional: true },
      RateLimits: { kind: "string", optional: true },
      MCPTimeout: { kind: "duration", optional: true },
      GmailClientID: { kind: "string", optional: true },
      GmailClientSecret: { kind: "string", optional: true },
      GmailAPIBase: { kind: "string", optional: true },
      GmailAuthEndpoint: { kind: "string", optional: true },
      GmailTokenEndpoint: { kind: "string", optional: true },
      SlackClientID: { kind: "string", optional: true },
      SlackClientSecret: { kind: "string", optional: true },
      SlackAPIBase: { kind: "string", optional: true },
      SlackAuthEndpoint: { kind: "string", optional: true },
      SlackTokenEndpoint: { kind: "string", optional: true },
      GitHubClientID: { kind: "string", optional: true },
      GitHubClientSecret: { kind: "string", optional: true },
      GitHubAPIBase: { kind: "string", optional: true },
      GitHubAuthEndpoint: { kind: "string", optional: true },
      GitHubTokenEndpoint: { kind: "string", optional: true },
    },
  },
  Tasks: {
    kind: "section",
    fields: {
      MaxSteps: { kind: "int", optional: true },
      MaxTokens: { kind: "int", optional: true },
      MaxHandoffDepth: { kind: "int", optional: true },
      MaxToolResultBytes: { kind: "int", optional: true },
    },
  },
  AttachmentContext: {
    kind: "section",
    fields: {
      MaxFileBytes: { kind: "int", optional: true },
      MaxCharsPerFile: { kind: "int", optional: true },
      MaxCharsTotal: { kind: "int", optional: true },
      MaxImages: { kind: "int", optional: true },
      MaxImageBytes: { kind: "int", optional: true },
    },
  },
  Memory: {
    kind: "section",
    fields: {
      MainTokens: { kind: "int", optional: true },
      RecallMemories: { kind: "int", optional: true },
      RecallKnowledge: { kind: "int", optional: true },
      Similarity: { kind: "float", optional: true },
      QuietSeconds: { kind: "int", optional: true },
      ExtractionMessages: { kind: "int", optional: true },
      ChunkRunes: { kind: "int", optional: true },
      ChunkOverlapRunes: { kind: "int", optional: true },
      MaxContentRunes: { kind: "int", optional: true },
    },
  },
  Storage: {
    kind: "section",
    fields: {
      Driver: { kind: "string", optional: true },
      LocalRoot: { kind: "string", optional: true },
      Endpoint: { kind: "string", optional: true },
      Region: { kind: "string", optional: true },
      Bucket: { kind: "string", optional: true },
      AccessKey: { kind: "string", optional: true },
      SecretKey: { kind: "string", optional: true },
      UseSSL: { kind: "bool", optional: true },
      PathStyle: { kind: "bool", optional: true },
      ContentOrigin: { kind: "string", optional: true },
      ContentFrameAncestors: { kind: "strings", optional: true },
      AttachmentAllowedExtensions: { kind: "strings", optional: true },
    },
  },
  WhatsApp: {
    kind: "section",
    fields: {
      GatewayURL: { kind: "string", optional: true },
      APIURL: { kind: "string", optional: true },
      GatewayToken: { kind: "string", optional: true },
      Address: { kind: "string", optional: true },
      Owner: { kind: "string", optional: true },
      HourlyLimit: { kind: "int", optional: true },
      LeaseTTL: { kind: "duration", optional: true },
      LeaseRenew: { kind: "duration", optional: true },
      TypingDelay: { kind: "duration", optional: true },
      MaxMediaBytes: { kind: "int", optional: true },
      LogWhatsmeow: { kind: "bool", optional: true },
    },
  },
  Billing: {
    kind: "section",
    fields: {
      Provider: { kind: "string", optional: true },
      GraceDays: { kind: "int", optional: true },
      SyncInterval: { kind: "duration", optional: true },
      SweepInterval: { kind: "duration", optional: true },
      SweepLimit: { kind: "int", optional: true },
      Duitku: {
        kind: "section",
        fields: {
          MerchantCode: { kind: "string", optional: true },
          APIKey: { kind: "string", optional: true },
          Sandbox: { kind: "bool", optional: true },
          PaymentMethod: { kind: "string", optional: true },
          ExpiryMinutes: { kind: "int", optional: true },
          CallbackURL: { kind: "string", optional: true },
          ReturnURL: { kind: "string", optional: true },
          BaseURL: { kind: "string", optional: true },
        },
      },
    },
  },
  Retention: {
    kind: "section",
    fields: {
      StepDays: { kind: "int", optional: true },
      AttachmentDays: { kind: "int", optional: true },
      Interval: { kind: "duration", optional: true },
      Batch: { kind: "int", optional: true },
      SweepLimit: { kind: "int", optional: true },
    },
  },
  VComputer: {
    kind: "section",
    fields: {
      ControlURL: { kind: "string", optional: true },
      Token: { kind: "string", optional: true },
      Timeout: { kind: "duration", optional: true },
    },
  },
  Push: {
    kind: "section",
    fields: {
      Enabled: { kind: "bool", optional: true },
    },
  },
  Telemetry: {
    kind: "section",
    fields: {
      ServiceName: { kind: "string", optional: true },
      Exporter: { kind: "string", optional: true },
      Endpoint: { kind: "string", optional: true },
      SampleRatio: { kind: "float", optional: true },
      Insecure: { kind: "bool", optional: true },
      MetricsExporter: { kind: "string", optional: true },
      MetricsEndpoint: { kind: "string", optional: true },
      MetricsInterval: { kind: "duration", optional: true },
    },
  },
  Logging: {
    kind: "section",
    fields: {
      Level: { kind: "string", optional: true },
      Format: { kind: "string", optional: true },
    },
  },
};

/** Turns a Go field name into its TypeScript camelCase spelling. */
export function toCamel(name: string): string {
  const normalized = name.replace(/OAuth/g, "Oauth");
  const words = normalized.match(/[A-Z]+(?![a-z])|[A-Z]?[a-z]+|[0-9]+/g) ?? [normalized];
  return words
    .map((word, index) =>
      index === 0 ? word.toLowerCase() : word[0].toUpperCase() + word.slice(1).toLowerCase(),
    )
    .join("");
}

/** Builds a Config from an already-merged YAML map. */
function decodeConfig(raw: Record<string, unknown>): Config {
  const decoded = decodeSection(raw, SECTIONS, "");
  return {
    ...decoded,
    // Replaced by `load` with the real list once the environment is applied.
    warnings: () => [],
  } as unknown as Config;
}

function decodeSection(
  raw: Record<string, unknown>,
  spec: Record<string, AnySpec>,
  path: string,
): Record<string, unknown> {
  const known = new Map<string, string>();
  for (const name of Object.keys(spec)) {
    known.set(name.toLowerCase(), name);
  }

  for (const key of Object.keys(raw)) {
    if (!known.has(key.toLowerCase())) {
      throw new Error(`config: unknown key ${path}${key}`);
    }
  }

  const out: Record<string, unknown> = {};
  for (const [goName, field] of Object.entries(spec)) {
    const rawKey = findKey(raw, goName);
    const value = rawKey === undefined ? undefined : raw[rawKey];
    const target = toCamel(goName);

    if (field.kind === "section") {
      if (value === undefined || value === null) {
        out[target] = decodeSection({}, field.fields, `${path}${goName}.`);
      } else if (typeof value === "object" && !Array.isArray(value)) {
        // The typeof check above proves an object; the cast names the shape the
        // YAML layer is already known to hold (a mapping, checked by mergeLayer).
        out[target] = decodeSection(value as Record<string, unknown>, field.fields, `${path}${goName}.`);
      } else {
        throw new Error(`config: ${path}${goName} must be a mapping`);
      }
      continue;
    }

    out[target] = decodeValue(value, field, `${path}${goName}`);
  }

  return out;
}

function decodeValue(value: unknown, field: FieldSpec, path: string): unknown {
  if (value === undefined || value === null) {
    switch (field.kind) {
      case "string":
        return "";
      case "int":
      case "float":
      case "duration":
        return 0;
      case "bool":
        return false;
      case "strings":
        return [];
    }
  }

  try {
    switch (field.kind) {
      case "string":
        return weakString(value);
      case "int":
        return weakInt(value);
      case "float":
        return weakFloat(value);
      case "bool":
        return weakBool(value);
      case "duration":
        if (typeof value === "string") return parseDuration(value);
        if (typeof value === "number") return value / 1e6; // Go reads a bare number as nanoseconds.
        throw new Error("expected a duration string");
      case "strings":
        if (!Array.isArray(value)) throw new Error("expected a list");
        return value.map((item) => weakString(item));
    }
  } catch (err) {
    throw new Error(`config: ${path}: ${(err as Error).message}`);
  }
}

function findKey(raw: Record<string, unknown>, name: string): string | undefined {
  if (name in raw) return name;
  const lower = name.toLowerCase();
  for (const key of Object.keys(raw)) {
    if (key.toLowerCase() === lower) return key;
  }
  return undefined;
}

function weakString(value: unknown): string {
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  throw new Error("expected a string");
}

function weakInt(value: unknown): number {
  if (typeof value === "number" && Number.isFinite(value)) return Math.trunc(value);
  if (typeof value === "boolean") return value ? 1 : 0;
  if (typeof value === "string") {
    if (!/^[+-]?\d+$/.test(value.trim())) throw new Error(`expected an integer, got ${JSON.stringify(value)}`);
    return Number(value.trim());
  }
  throw new Error("expected an integer");
}

function weakFloat(value: unknown): number {
  if (typeof value === "number") return value;
  if (typeof value === "boolean") return value ? 1 : 0;
  if (typeof value === "string") {
    const parsed = Number(value.trim());
    if (value.trim() === "" || Number.isNaN(parsed)) {
      throw new Error(`expected a number, got ${JSON.stringify(value)}`);
    }
    return parsed;
  }
  throw new Error("expected a number");
}

function weakBool(value: unknown): boolean {
  if (typeof value === "boolean") return value;
  if (typeof value === "number") return value !== 0;
  if (typeof value === "string") return parseBool(value);
  throw new Error("expected a boolean");
}

// --------------------------------------------------------- environment input

function getenv(name: string): string | null {
  const value = process.env[name];
  return value === undefined || value === "" ? null : value;
}

function readEnv<T>(name: string, parse: (value: string) => T): T | null {
  const value = getenv(name);
  if (value === null) return null;
  try {
    return parse(value);
  } catch (err) {
    throw new Error(`parse ${name}: ${(err as Error).message}`);
  }
}

/** `strconv.Atoi`. */
function atoi(value: string): number {
  const trimmed = value.trim();
  if (!/^[+-]?\d+$/.test(trimmed)) {
    throw new Error(`strconv.Atoi: parsing ${JSON.stringify(value)}: invalid syntax`);
  }
  return Number(trimmed);
}

/** `strconv.ParseInt(v, 10, 64)`. */
function parseInt64(value: string): number {
  const trimmed = value.trim();
  if (!/^[+-]?\d+$/.test(trimmed)) {
    throw new Error(`strconv.ParseInt: parsing ${JSON.stringify(value)}: invalid syntax`);
  }
  return Number(trimmed);
}

/** `strconv.ParseFloat`. */
function parseFloat(value: string): number {
  const trimmed = value.trim();
  const special = /^[+-]?(inf|infinity|nan)$/i.test(trimmed);
  const decimal = /^[+-]?(\d+(\.\d*)?|\.\d+)([eE][+-]?\d+)?$/.test(trimmed);
  if (!special && !decimal) {
    throw new Error(`strconv.ParseFloat: parsing ${JSON.stringify(value)}: invalid syntax`);
  }
  return Number(trimmed);
}

/** `strconv.ParseBool`. */
function parseBool(value: string): boolean {
  switch (value) {
    case "1":
    case "t":
    case "T":
    case "TRUE":
    case "true":
    case "True":
      return true;
    case "0":
    case "f":
    case "F":
    case "FALSE":
    case "false":
    case "False":
      return false;
    default:
      throw new Error(`strconv.ParseBool: parsing ${JSON.stringify(value)}: invalid syntax`);
  }
}

/**
 * Applies the supported environment-variable overrides. Only non-empty
 * variables win, so a YAML default is never wiped by an unset env.
 *
 * A value that cannot be parsed is an error rather than a silent default. The
 * one exception is the virtual computer's timeout, a bound on an optional
 * feature: it becomes a warning and the configured value stands.
 */
export function loadFromEnv(cfg: Config, warnings: string[] = []): void {
  const strings: Array<[string, (value: string) => void]> = [
    ["ENVIRONMENT", (v) => (cfg.application.environment = v)],
    ["HTTP_ADDRESS", (v) => (cfg.http.address = v)],
    ["OPS_ADDRESS", (v) => (cfg.http.opsAddress = v)],
    ["INTERNAL_ADDRESS", (v) => (cfg.http.internalAddress = v)],
    ["DATABASE_URL", (v) => (cfg.database.url = v)],
    ["REDIS_URL", (v) => (cfg.redis.url = v)],
    ["TEMPORAL_HOST_PORT", (v) => (cfg.temporal.hostPort = v)],
    ["TEMPORAL_NAMESPACE", (v) => (cfg.temporal.namespace = v)],
    // The YAML default is TLS on, because the hosted deployment targets Temporal
    // Cloud. A compose-network server speaks plaintext, so this override is the
    // only thing that makes the local and staging workers able to connect at all
    // — without it they attempt a TLS handshake against a plaintext port and the
    // worker dies on startup.
    ["TEMPORAL_TLS", (v) => (cfg.temporal.tls = parseBool(v))],
    ["TEMPORAL_TASK_QUEUE_AGENT", (v) => (cfg.temporal.taskQueueAgent = v)],
    ["TEMPORAL_TASK_QUEUE_INTEGRATION", (v) => (cfg.temporal.taskQueueIntegration = v)],
    ["TEMPORAL_API_KEY", (v) => (cfg.temporal.apiKey = v)],
    ["JWT_SECRET", (v) => (cfg.jwt.secret = v)],
    ["LOG_LEVEL", (v) => (cfg.logging.level = v)],
    ["LOG_FORMAT", (v) => (cfg.logging.format = v)],
    ["FRONTEND_URL", (v) => (cfg.application.frontendUrl = v)],
    ["GOOGLE_CLIENT_ID", (v) => (cfg.google.clientId = v)],
    ["GOOGLE_CLIENT_SECRET", (v) => (cfg.google.clientSecret = v)],
    ["GOOGLE_REDIRECT_URL", (v) => (cfg.google.redirectUrl = v)],
    ["MAIL_DRIVER", (v) => (cfg.mail.driver = v)],
    ["MAIL_FROM", (v) => (cfg.mail.from = v)],
    ["MAIL_HOST", (v) => (cfg.mail.host = v)],
    ["MAIL_USERNAME", (v) => (cfg.mail.username = v)],
    ["MAIL_PASSWORD", (v) => (cfg.mail.password = v)],
    ["SECRET_ENCRYPTION_KEY", (v) => (cfg.llm.secretEncryptionKey = v)],
    ["LLM_DEFAULT_ADAPTER", (v) => (cfg.llm.default.adapter = v)],
    ["LLM_DEFAULT_BASE_URL", (v) => (cfg.llm.default.baseUrl = v)],
    ["LLM_DEFAULT_MODEL", (v) => (cfg.llm.default.model = v)],
    ["LLM_DEFAULT_API_KEY", (v) => (cfg.llm.default.apiKey = v)],
    ["STORAGE_DRIVER", (v) => (cfg.storage.driver = v)],
    ["STORAGE_LOCAL_ROOT", (v) => (cfg.storage.localRoot = v)],
    ["S3_ENDPOINT", (v) => (cfg.storage.endpoint = v)],
    ["S3_REGION", (v) => (cfg.storage.region = v)],
    ["S3_BUCKET", (v) => (cfg.storage.bucket = v)],
    ["S3_ACCESS_KEY", (v) => (cfg.storage.accessKey = v)],
    ["S3_SECRET_KEY", (v) => (cfg.storage.secretKey = v)],
    ["CONTENT_ORIGIN", (v) => (cfg.storage.contentOrigin = v)],
    ["KMS_MASTER_KEY", (v) => (cfg.kms.masterKey = v)],
    ["KMS_MASTER_KEY_ID", (v) => (cfg.kms.masterKeyId = v)],
    ["KMS_RETIRED_KEYS", (v) => (cfg.kms.retiredKeys = v)],
    ["OAUTH_STATE_KEY", (v) => (cfg.integrations.oauthStateKey = v)],
    ["OAUTH_REDIRECT_BASE", (v) => (cfg.integrations.oauthRedirectBase = v)],
    ["INTEGRATION_RATE_LIMITS", (v) => (cfg.integrations.rateLimits = v)],
    ["GMAIL_CLIENT_ID", (v) => (cfg.integrations.gmailClientId = v)],
    ["GMAIL_CLIENT_SECRET", (v) => (cfg.integrations.gmailClientSecret = v)],
    ["GMAIL_API_BASE", (v) => (cfg.integrations.gmailApiBase = v)],
    ["GMAIL_AUTH_ENDPOINT", (v) => (cfg.integrations.gmailAuthEndpoint = v)],
    ["GMAIL_TOKEN_ENDPOINT", (v) => (cfg.integrations.gmailTokenEndpoint = v)],
    ["SLACK_CLIENT_ID", (v) => (cfg.integrations.slackClientId = v)],
    ["SLACK_CLIENT_SECRET", (v) => (cfg.integrations.slackClientSecret = v)],
    ["SLACK_API_BASE", (v) => (cfg.integrations.slackApiBase = v)],
    ["SLACK_AUTH_ENDPOINT", (v) => (cfg.integrations.slackAuthEndpoint = v)],
    ["SLACK_TOKEN_ENDPOINT", (v) => (cfg.integrations.slackTokenEndpoint = v)],
    ["GITHUB_CLIENT_ID", (v) => (cfg.integrations.gitHubClientId = v)],
    ["GITHUB_CLIENT_SECRET", (v) => (cfg.integrations.gitHubClientSecret = v)],
    ["GITHUB_API_BASE", (v) => (cfg.integrations.gitHubApiBase = v)],
    ["GITHUB_AUTH_ENDPOINT", (v) => (cfg.integrations.gitHubAuthEndpoint = v)],
    ["GITHUB_TOKEN_ENDPOINT", (v) => (cfg.integrations.gitHubTokenEndpoint = v)],
    // WhatsApp: the two processes reach each other over one shared secret, so
    // every one of these is a secret or an address.
    ["WA_GATEWAY_URL", (v) => (cfg.whatsApp.gatewayUrl = v)],
    ["WA_API_URL", (v) => (cfg.whatsApp.apiUrl = v)],
    ["WA_GATEWAY_TOKEN", (v) => (cfg.whatsApp.gatewayToken = v)],
    ["WA_ADDRESS", (v) => (cfg.whatsApp.address = v)],
    ["WA_OWNER", (v) => (cfg.whatsApp.owner = v)],
    // Virtual computer: both values come from the environment and the address
    // stays out of the YAML file that ships with the image.
    ["VCOMPUTER_CONTROL_URL", (v) => (cfg.vComputer.controlUrl = v)],
    ["VCOMPUTER_CONTROL_TOKEN", (v) => (cfg.vComputer.token = v)],
  ];

  for (const [name, apply] of strings) {
    const value = getenv(name);
    if (value !== null) apply(value);
  }

  const mailTimeout = readEnv("MAIL_TIMEOUT_SECONDS", atoi);
  if (mailTimeout !== null) cfg.mail.timeoutSeconds = mailTimeout;

  const callsPerMinute = readEnv("INTEGRATION_CALLS_PER_MINUTE", atoi);
  if (callsPerMinute !== null) cfg.integrations.defaultCallsPerMinute = callsPerMinute;

  const mcpTimeout = readEnv("MCP_TIMEOUT", parseDuration);
  if (mcpTimeout !== null) cfg.integrations.mcpTimeout = mcpTimeout;

  const quotaEnabled = readEnv("LLM_QUOTA_ENABLED", parseBool);
  if (quotaEnabled !== null) cfg.llm.quotaEnabled = quotaEnabled;

  const requireVerification = readEnv("AUTH_REQUIRE_EMAIL_VERIFICATION", parseBool);
  if (requireVerification !== null) cfg.auth.requireEmailVerification = requireVerification;

  const tokensPerPeriod = readEnv("LLM_TOKENS_PER_PERIOD", parseInt64);
  if (tokensPerPeriod !== null) cfg.llm.tokensPerPeriod = tokensPerPeriod;

  const chainLimit = readEnv("LLM_CHAIN_LIMIT", atoi);
  if (chainLimit !== null) cfg.llm.chainLimit = chainLimit;

  const textOnlyModels = getenv("LLM_TEXT_ONLY_MODELS");
  if (textOnlyModels !== null) {
    // A model name is matched case-insensitively, and a provider spells it its
    // own way, so the list is normalized here; a trailing comma is a typo.
    cfg.llm.textOnlyModels = textOnlyModels
      .split(",")
      .map((model) => model.trim().toLowerCase())
      .filter((model) => model !== "");
  }

  const defaultMaxTokens = readEnv("LLM_DEFAULT_MAX_TOKENS", atoi);
  if (defaultMaxTokens !== null) cfg.llm.default.maxTokens = defaultMaxTokens;

  const frameAncestors = getenv("CONTENT_FRAME_ANCESTORS");
  if (frameAncestors !== null) {
    cfg.storage.contentFrameAncestors = frameAncestors
      .split(",")
      .map((origin) => origin.trim())
      .filter((origin) => origin !== "");
  }

  const allowedExtensions = getenv("ATTACHMENT_ALLOWED_EXTENSIONS");
  if (allowedExtensions !== null) {
    // A leading dot is how a person writes an extension and the chat module
    // spells them without one; `.PNG` and `png` name the same type.
    cfg.storage.attachmentAllowedExtensions = allowedExtensions
      .split(",")
      .map((extension) => extension.trim().toLowerCase().replace(/^\./, ""))
      .filter((extension) => extension !== "");
  }

  const useSsl = readEnv("S3_USE_SSL", parseBool);
  if (useSsl !== null) cfg.storage.useSsl = useSsl;

  const pathStyle = readEnv("S3_PATH_STYLE", parseBool);
  if (pathStyle !== null) cfg.storage.pathStyle = pathStyle;

  const contextTokens = readEnv("LLM_DEFAULT_CONTEXT_TOKENS", atoi);
  if (contextTokens !== null) cfg.llm.default.contextTokens = contextTokens;

  const billingProvider = getenv("BILLING_PROVIDER");
  if (billingProvider !== null) cfg.billing.provider = billingProvider.trim();

  const merchantCode = getenv("DUITKU_MERCHANT_CODE");
  if (merchantCode !== null) {
    cfg.billing.duitku.merchantCode = merchantCode.trim();
    // Naming the code implies the provider: a deployment that supplies a code
    // meant to take payment.
    if (cfg.billing.provider.trim() === "") cfg.billing.provider = "duitku";
  }

  const duitkuApiKey = getenv("DUITKU_API_KEY");
  if (duitkuApiKey !== null) {
    cfg.billing.duitku.apiKey = duitkuApiKey.trim();
    if (cfg.billing.provider.trim() === "") cfg.billing.provider = "duitku";
  }

  const duitkuSandbox = readEnv("DUITKU_SANDBOX", parseBool);
  if (duitkuSandbox !== null) cfg.billing.duitku.sandbox = duitkuSandbox;

  const paymentMethod = getenv("DUITKU_PAYMENT_METHOD");
  if (paymentMethod !== null) cfg.billing.duitku.paymentMethod = paymentMethod.trim();

  const expiryMinutes = readEnv("DUITKU_EXPIRY_MINUTES", atoi);
  if (expiryMinutes !== null) cfg.billing.duitku.expiryMinutes = expiryMinutes;

  const callbackUrl = getenv("DUITKU_CALLBACK_URL");
  if (callbackUrl !== null) cfg.billing.duitku.callbackUrl = callbackUrl.trim();

  const returnUrl = getenv("DUITKU_RETURN_URL");
  if (returnUrl !== null) cfg.billing.duitku.returnUrl = returnUrl.trim();

  const duitkuBaseUrl = getenv("DUITKU_BASE_URL");
  if (duitkuBaseUrl !== null) cfg.billing.duitku.baseUrl = duitkuBaseUrl.trim();

  const graceDays = readEnv("BILLING_GRACE_DAYS", atoi);
  if (graceDays !== null) cfg.billing.graceDays = graceDays;

  const syncInterval = readEnv("BILLING_SYNC_INTERVAL", parseDuration);
  if (syncInterval !== null) cfg.billing.syncInterval = syncInterval;

  const sweepInterval = readEnv("BILLING_SWEEP_INTERVAL", parseDuration);
  if (sweepInterval !== null) cfg.billing.sweepInterval = sweepInterval;

  const hourlyLimit = readEnv("WA_HOURLY_LIMIT", atoi);
  if (hourlyLimit !== null) cfg.whatsApp.hourlyLimit = hourlyLimit;

  const leaseTtl = readEnv("WA_LEASE_TTL", parseDuration);
  if (leaseTtl !== null) cfg.whatsApp.leaseTtl = leaseTtl;

  const leaseRenew = readEnv("WA_LEASE_RENEW", parseDuration);
  if (leaseRenew !== null) cfg.whatsApp.leaseRenew = leaseRenew;

  const typingDelay = readEnv("WA_TYPING_DELAY", parseDuration);
  if (typingDelay !== null) cfg.whatsApp.typingDelay = typingDelay;

  const maxMediaBytes = readEnv("WA_MAX_MEDIA_BYTES", atoi);
  if (maxMediaBytes !== null) cfg.whatsApp.maxMediaBytes = maxMediaBytes;

  const logWhatsmeow = readEnv("WA_LOG_WHATSMEOW", parseBool);
  if (logWhatsmeow !== null) cfg.whatsApp.logWhatsmeow = logWhatsmeow;

  const rawTimeout = getenv("VCOMPUTER_CONTROL_TIMEOUT");
  if (rawTimeout !== null) {
    try {
      cfg.vComputer.timeout = parseDuration(rawTimeout);
    } catch (err) {
      // Warned about and ignored rather than fatal, unlike every other duration
      // here. The virtual computer is an optional feature and this bounds one
      // call to it, so a typo in it must not take down the agent worker and the
      // workflows that have nothing to do with a sandbox (issue #219). The value
      // that stands is the one the config file carries.
      const standing =
        cfg.vComputer.timeout > 0
          ? `the configured timeout of ${formatDuration(cfg.vComputer.timeout)}`
          : "the timeout the client defaults to";
      warnings.push(
        `VCOMPUTER_CONTROL_TIMEOUT=${JSON.stringify(rawTimeout)} is not a duration, ` +
          `so it is ignored and ${standing} stands: ${(err as Error).message}`,
      );
    }
  }

  const dailyCostMicros = readEnv("PLAN_DAILY_COST_MICROS", parseInt64);
  if (dailyCostMicros !== null) cfg.plan.dailyCostMicros = dailyCostMicros;

  // The Temporal UI link the task screen offers. It is a base URL rather than a
  // built one, because the path below it is the UI's own.
  const uiBaseUrl = getenv("TEMPORAL_UI_BASE_URL");
  if (uiBaseUrl !== null) cfg.temporal.uiBaseUrl = uiBaseUrl;

  const otelExporter = getenv("OTEL_EXPORTER");
  if (otelExporter !== null) cfg.telemetry.exporter = otelExporter;

  const otelEndpoint = getenv("OTEL_ENDPOINT");
  if (otelEndpoint !== null) cfg.telemetry.endpoint = otelEndpoint;

  const otelServiceName = getenv("OTEL_SERVICE_NAME");
  if (otelServiceName !== null) cfg.telemetry.serviceName = otelServiceName;

  const sampleRatio = readEnv("OTEL_SAMPLE_RATIO", parseFloat);
  if (sampleRatio !== null) cfg.telemetry.sampleRatio = sampleRatio;

  const insecure = getenv("OTEL_INSECURE");
  if (insecure !== null) {
    cfg.telemetry.insecure = insecure.toLowerCase() === "true" || insecure === "1";
  }

  const metricsExporter = getenv("OTEL_METRICS_EXPORTER");
  if (metricsExporter !== null) cfg.telemetry.metricsExporter = metricsExporter;

  const metricsEndpoint = getenv("OTEL_METRICS_ENDPOINT");
  if (metricsEndpoint !== null) cfg.telemetry.metricsEndpoint = metricsEndpoint;

  const metricsInterval = readEnv("OTEL_METRICS_INTERVAL", parseDuration);
  if (metricsInterval !== null) cfg.telemetry.metricsInterval = metricsInterval;

  const stepDays = readEnv("RETENTION_STEP_DAYS", atoi);
  if (stepDays !== null) cfg.retention.stepDays = stepDays;

  const attachmentDays = readEnv("RETENTION_ATTACHMENT_DAYS", atoi);
  if (attachmentDays !== null) cfg.retention.attachmentDays = attachmentDays;

  const retentionInterval = readEnv("RETENTION_INTERVAL", parseDuration);
  if (retentionInterval !== null) cfg.retention.interval = retentionInterval;

  const retentionBatch = readEnv("RETENTION_BATCH", atoi);
  if (retentionBatch !== null) cfg.retention.batch = retentionBatch;

  const maxSteps = readEnv("TASK_MAX_STEPS", atoi);
  if (maxSteps !== null) cfg.tasks.maxSteps = maxSteps;

  const maxTokens = readEnv("TASK_MAX_TOKENS", parseInt64);
  if (maxTokens !== null) cfg.tasks.maxTokens = maxTokens;

  const maxHandoffDepth = readEnv("TASK_MAX_HANDOFF_DEPTH", atoi);
  if (maxHandoffDepth !== null) cfg.tasks.maxHandoffDepth = maxHandoffDepth;

  const maxToolResultBytes = readEnv("TASK_MAX_TOOL_RESULT_BYTES", atoi);
  if (maxToolResultBytes !== null) cfg.tasks.maxToolResultBytes = maxToolResultBytes;

  const maxFileBytes = readEnv("ATTACHMENT_CONTEXT_MAX_FILE_BYTES", parseInt64);
  if (maxFileBytes !== null) cfg.attachmentContext.maxFileBytes = maxFileBytes;

  const maxCharsPerFile = readEnv("ATTACHMENT_CONTEXT_MAX_CHARS_PER_FILE", atoi);
  if (maxCharsPerFile !== null) cfg.attachmentContext.maxCharsPerFile = maxCharsPerFile;

  const maxCharsTotal = readEnv("ATTACHMENT_CONTEXT_MAX_CHARS_TOTAL", atoi);
  if (maxCharsTotal !== null) cfg.attachmentContext.maxCharsTotal = maxCharsTotal;

  const maxImages = readEnv("ATTACHMENT_CONTEXT_MAX_IMAGES", atoi);
  if (maxImages !== null) cfg.attachmentContext.maxImages = maxImages;

  const maxImageBytes = readEnv("ATTACHMENT_CONTEXT_MAX_IMAGE_BYTES", parseInt64);
  if (maxImageBytes !== null) cfg.attachmentContext.maxImageBytes = maxImageBytes;

  const mainTokens = readEnv("MEMORY_MAIN_TOKENS", atoi);
  if (mainTokens !== null) cfg.memory.mainTokens = mainTokens;

  const recallMemories = readEnv("MEMORY_RECALL_MEMORIES", atoi);
  if (recallMemories !== null) cfg.memory.recallMemories = recallMemories;

  const recallKnowledge = readEnv("MEMORY_RECALL_KNOWLEDGE", atoi);
  if (recallKnowledge !== null) cfg.memory.recallKnowledge = recallKnowledge;

  const similarity = readEnv("MEMORY_SIMILARITY", parseFloat);
  if (similarity !== null) cfg.memory.similarity = similarity;

  const quietSeconds = readEnv("MEMORY_QUIET_SECONDS", atoi);
  if (quietSeconds !== null) cfg.memory.quietSeconds = quietSeconds;

  const extractionMessages = readEnv("MEMORY_EXTRACTION_MESSAGES", atoi);
  if (extractionMessages !== null) cfg.memory.extractionMessages = extractionMessages;

  const chunkRunes = readEnv("MEMORY_CHUNK_RUNES", atoi);
  if (chunkRunes !== null) cfg.memory.chunkRunes = chunkRunes;

  const chunkOverlapRunes = readEnv("MEMORY_CHUNK_OVERLAP_RUNES", atoi);
  if (chunkOverlapRunes !== null) cfg.memory.chunkOverlapRunes = chunkOverlapRunes;

  const maxContentRunes = readEnv("MEMORY_MAX_CONTENT_RUNES", atoi);
  if (maxContentRunes !== null) cfg.memory.maxContentRunes = maxContentRunes;

  // The kill switch. Only the literal `true` turns push on; see ./push.ts for
  // why `1` and `yes` deliberately do not.
  const pushEnabled = getenv("PUSH_ENABLED");
  if (pushEnabled !== null) cfg.push.enabled = parsePushEnabled(pushEnabled);
}
