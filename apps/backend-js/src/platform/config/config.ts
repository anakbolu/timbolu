/**
 * Configuration types.
 *
 * Ported from packages/platform/config/config.go. The tree is reproduced
 * field-for-field: `cfg.Http.Address` becomes `cfg.http.address`, and every
 * `time.Duration` is a number of **milliseconds** (see `parseDuration`).
 *
 * Values come from the YAML file matching ENVIRONMENT and are then overridden
 * by environment variables. The loader lives in `init.ts`.
 */

/**
 * The root application configuration.
 *
 * `warnings()` lists the environment values the loader could not use, in the
 * order it found them, each naming the variable and what stood in its place.
 * A value that will not parse for a bound on an optional feature becomes a
 * warning and the configured default is used; a bound the product bills
 * against still refuses to start.
 */
export interface Config {
  application: AppConfig;
  http: HttpConfig;
  database: DatabaseConfig;
  redis: RedisConfig;
  temporal: TemporalConfig;
  jwt: JWTConfig;
  auth: AuthConfig;
  google: GoogleConfig;
  mail: MailConfig;
  plan: PlanConfig;
  llm: LlmConfig;
  kms: KmsConfig;
  integrations: IntegrationConfig;
  tasks: TasksConfig;
  /** How much of a message's files reaches the model. */
  attachmentContext: AttachmentContextConfig;
  memory: MemoryConfig;
  storage: StorageConfig;
  whatsApp: WhatsAppConfig;
  billing: BillingConfig;
  retention: RetentionConfig;
  vComputer: VComputerConfig;
  telemetry: TelemetryConfig;
  logging: LoggingConfig;

  warnings(): string[];
}

/**
 * Names this binary in the traces, unless the deployment named it.
 *
 * Mirrors Go `(*TelemetryConfig).DefaultServiceName`.
 */
export function defaultServiceName(t: TelemetryConfig, name: string): void {
  if (t.serviceName.trim() === "") {
    t.serviceName = name;
  }
}

/**
 * Where a deployment's traces go.
 *
 * The default is nowhere: a deployment that has not said where telemetry should
 * be sent should not be sending it. The provider is installed either way, so a
 * request still has a trace id and its log lines can be correlated — only the
 * spans are dropped.
 */
export interface TelemetryConfig {
  /** What the spans are filed under. Each binary names itself. */
  serviceName: string;
  /** "none", "console" (a development run) or "otlp" (a collector). */
  exporter: string;
  /** The collector's host:port, required by the otlp exporter. */
  endpoint: string;
  /** The share of root traces kept, between 0 and 1. */
  sampleRatio: number;
  /** Drops TLS to the collector, which is what a compose collector wants. */
  insecure: boolean;

  /** "none", "prometheus" (a scrape endpoint), "otlp", or "console". */
  metricsExporter: string;
  /** The collector's host:port, required by the otlp metrics exporter. */
  metricsEndpoint: string;
  /** How often measurements are pushed. Milliseconds; zero takes the SDK default. */
  metricsInterval: number;
}

/**
 * How long the deployment keeps what a customer said and what a model answered.
 *
 * A task's *shape* is kept for good, and its raw payload is not. A retention
 * rule that removed whole rows would take the task history and the cost report
 * with it.
 */
export interface RetentionConfig {
  /** How long a task step's raw input and output are kept. Zero: thirty days. */
  stepDays: number;
  /** How long an attachment's bytes are kept. Zero: as long as its message. */
  attachmentDays: number;
  /** How often the job runs, in milliseconds. Zero disables it. */
  interval: number;
  /** How many steps one pass prunes. */
  batch: number;
  /** How many workspaces one pass looks at. */
  sweepLimit: number;
}

/**
 * Where the backend reaches a bot's virtual computer.
 *
 * Empty controlUrl is a deployment without a virtual computer, and it is the
 * default: every activity then answers ErrNotConfigured.
 */
export interface VComputerConfig {
  /** The control plane's base address on the internal network. */
  controlUrl: string;
  /** The control plane's shared secret, sent as a bearer on every request. */
  token: string;
  /** Bounds one call to the control plane, in milliseconds. */
  timeout: number;
}

/** What taking money online needs. Empty Duitku credentials close the payment path. */
export interface BillingConfig {
  /** The payment gateway this deployment speaks. Empty means none. */
  provider: string;
  /** How long a workspace keeps working after its period ends. Zero: a week. */
  graceDays: number;
  /** How often one workspace is reconciled. Milliseconds. */
  syncInterval: number;
  /** How often the reconciliation pass runs. Milliseconds. Zero disables it. */
  sweepInterval: number;
  /** Bounds one pass. */
  sweepLimit: number;
  duitku: DuitkuConfig;
}

/** The Duitku adapter's settings. */
export interface DuitkuConfig {
  merchantCode: string;
  apiKey: string;
  /** Selects Duitku's sandbox host. */
  sandbox: boolean;
  /** Duitku's own two-letter payment-channel code. Empty: bank virtual account. */
  paymentMethod: string;
  /** How long a payment stays payable. Zero: a day. */
  expiryMinutes: number;
  callbackUrl: string;
  returnUrl: string;
  baseUrl: string;
}

/**
 * What the WhatsApp gateway needs.
 *
 * Two configurations in one namespace because the two ends are one deployment:
 * the API process reaches the gateway, and the gateway process owns the
 * connections.
 */
export interface WhatsAppConfig {
  /** The wa-gateway's internal address, as the API reaches it. */
  gatewayUrl: string;
  /** The backend's internal address, as the gateway reaches it. */
  apiUrl: string;
  /** Shared secret that authenticates the API to the gateway. */
  gatewayToken: string;
  /** Where the gateway process listens. */
  address: string;
  /** Names this gateway instance in the lease. */
  owner: string;
  /** Default send ceiling of a number, in messages per hour. */
  hourlyLimit: number;
  /** How long a lease survives without a renewal, in milliseconds. */
  leaseTtl: number;
  /** How often a held lease is extended, in milliseconds. */
  leaseRenew: number;
  /** How long the typing indicator shows, in milliseconds. */
  typingDelay: number;
  /** Refuses an inbound attachment larger than this. */
  maxMediaBytes: number;
  /** whatsmeow's own protocol logging. */
  logWhatsmeow: boolean;
}

/** What the integration hub needs beyond its adapters. */
export interface IntegrationConfig {
  /** Signs the value an authorization flow carries. */
  oauthStateKey: string;
  /** The public origin providers redirect back to. */
  oauthRedirectBase: string;
  /** Paces every application that has no ceiling of its own. */
  defaultCallsPerMinute: number;
  /** Per-application ceilings, as `app:perMinute` pairs separated by commas. */
  rateLimits: string;
  /** Bounds one request to an MCP server, in milliseconds. */
  mcpTimeout: number;
  gmailClientId: string;
  gmailClientSecret: string;
  gmailApiBase: string;
  gmailAuthEndpoint: string;
  gmailTokenEndpoint: string;
  slackClientId: string;
  slackClientSecret: string;
  slackApiBase: string;
  slackAuthEndpoint: string;
  slackTokenEndpoint: string;
  gitHubClientId: string;
  gitHubClientSecret: string;
  gitHubApiBase: string;
  gitHubAuthEndpoint: string;
  gitHubTokenEndpoint: string;
}

/** The key-management key that protects every workspace's integration tokens. */
export interface KmsConfig {
  /** Wraps the per-workspace data keys. */
  masterKey: string;
  /** Names the active key. */
  masterKeyId: string;
  /** Older keys, as `id:key` pairs separated by commas. */
  retiredKeys: string;
}

/** Bounds what memory costs and how long a conversation must be quiet. */
export interface MemoryConfig {
  mainTokens: number;
  recallMemories: number;
  recallKnowledge: number;
  similarity: number;
  quietSeconds: number;
  extractionMessages: number;
  chunkRunes: number;
  chunkOverlapRunes: number;
  maxContentRunes: number;
}

/** Bounds what one task may spend. */
export interface TasksConfig {
  maxSteps: number;
  maxTokens: number;
  maxHandoffDepth: number;
  maxToolResultBytes: number;
}

/** Bounds what the attachments of one message contribute to a model call. */
export interface AttachmentContextConfig {
  maxFileBytes: number;
  maxCharsPerFile: number;
  maxCharsTotal: number;
  maxImages: number;
  maxImageBytes: number;
}

/** Application-level settings. */
export interface AppConfig {
  environment: string;
  /** Base URL of the web app. */
  frontendUrl: string;
  /** Browser origins allowed to call the API. Empty allows any origin. */
  corsOrigins: string[];
}

/** HTTP server settings. */
export interface HttpConfig {
  /** Where a process with no HTTP API of its own serves /health, /ready, /metrics. */
  opsAddress: string;
  /** Where the API serves the gateway's own calls. */
  internalAddress: string;
  address: string;
  /** Milliseconds. */
  readTimeout: number;
  /** Milliseconds. */
  writeTimeout: number;
  apiPrefix: string;
}

/** Postgres connection settings. */
export interface DatabaseConfig {
  url: string;
  maxOpenConns: number;
  maxIdleConns: number;
  /** Seconds. */
  connMaxLifetime: number;
}

/** Redis connection settings. */
export interface RedisConfig {
  url: string;
}

/** Temporal connection settings. */
export interface TemporalConfig {
  hostPort: string;
  namespace: string;
  taskQueueAgent: string;
  taskQueueIntegration: string;
  tls: boolean;
  apiKey: string;
  /** Where this deployment's Temporal UI is reachable. Empty: no link. */
  uiBaseUrl: string;
}

/** JWT signing settings. */
export interface JWTConfig {
  secret: string;
  expireHours: number;
}

/** Authentication lifetimes, cookie settings, and the login throttle. */
export interface AuthConfig {
  accessTokenMinutes: number;
  refreshHours: number;
  verificationHours: number;
  resetMinutes: number;
  stateMinutes: number;
  cookieDomain: string;
  cookieSecure: boolean;
  loginThrottleCapacity: number;
  loginThrottleRefill: number;
  requireEmailVerification: boolean;
}

/** Google OAuth client registration. Empty clientId disables Google sign-in. */
export interface GoogleConfig {
  clientId: string;
  clientSecret: string;
  redirectUrl: string;
}

/** How transactional email is delivered. */
export interface MailConfig {
  driver: string;
  from: string;
  host: string;
  port: number;
  username: string;
  password: string;
  /** Bounds one SMTP delivery, in seconds. Zero takes the package default. */
  timeoutSeconds: number;
}

/** Plan defaults. */
export interface PlanConfig {
  /** Caps the Bolu registry. Zero means unlimited. */
  maxAgentsPerWorkspace: number;
  /** The cost ceiling of one day per workspace, in micro-rupiah. */
  dailyCostMicros: number;
}

/** Model gateway settings. */
export interface LlmConfig {
  /** Seals provider API keys at rest. */
  secretEncryptionKey: string;
  /** The platform-level provider. */
  default: DefaultProviderConfig;
  /** Turns the pre-flight token check on. */
  quotaEnabled: boolean;
  /** Monthly token allowance when no subscription is wired. */
  tokensPerPeriod: number;
  /** How many providers one request tries. */
  chainLimit: number;
  /** Models that cannot read a picture. */
  textOnlyModels: string[];
}

/** The platform-level provider. */
export interface DefaultProviderConfig {
  adapter: string;
  baseUrl: string;
  model: string;
  apiKey: string;
  maxTokens: number;
  contextTokens: number;
}

/** Object storage settings. */
export interface StorageConfig {
  /** "local" or "s3". */
  driver: string;
  /** Where the local driver writes. */
  localRoot: string;
  /** The S3 host without a scheme, e.g. "minio:9000". */
  endpoint: string;
  region: string;
  bucket: string;
  accessKey: string;
  secretKey: string;
  useSsl: boolean;
  pathStyle: boolean;
  /** Where sandboxed HTML documents are served from. */
  contentOrigin: string;
  /** Origins allowed to frame a content document. */
  contentFrameAncestors: string[];
  /** Narrows which file types a chat upload may be, by extension. */
  attachmentAllowedExtensions: string[];
}

/** Structured logging settings. */
export interface LoggingConfig {
  /** debug|info|warn|error */
  level: string;
  /** json|console */
  format: string;
}

// ------------------------------------------------------------------ durations

const NANOSECOND = 1;
const MICROSECOND = 1e3;
const MILLISECOND = 1e6;
const SECOND = 1e9;
const MINUTE = 6e10;
const HOUR = 3.6e12;

/**
 * Parses a Go duration string into **milliseconds**.
 *
 * Understands the same grammar as `time.ParseDuration`: an optional sign
 * followed by one or more decimal numbers with a unit, e.g. `"30s"`, `"1.5h"`,
 * `"1h30m"`, `"1200ms"`. Units are `ns`, `us`, `µs` (U+00B5), `μs` (U+03BC),
 * `ms`, `s`, `m`, and `h`; a bare `"0"` is zero.
 *
 * Each component is truncated to whole nanoseconds, exactly as Go does, then
 * the total is converted to milliseconds.
 */
export function parseDuration(s: string): number {
  if (s === "0") return 0;
  if (s === "") throw new Error('time: invalid duration ""');

  let i = 0;
  let negative = false;
  if (s[0] === "+" || s[0] === "-") {
    negative = s[0] === "-";
    i++;
  }

  let totalNs = 0;
  let sawNumber = false;

  while (i < s.length) {
    const start = i;
    let whole = 0;
    let sawDigit = false;
    while (i < s.length && s[i] >= "0" && s[i] <= "9") {
      whole = whole * 10 + (s.charCodeAt(i) - 48);
      sawDigit = true;
      i++;
    }

    let fraction = 0;
    let fractionScale = 1;
    if (i < s.length && s[i] === ".") {
      i++;
      while (i < s.length && s[i] >= "0" && s[i] <= "9") {
        fraction = fraction * 10 + (s.charCodeAt(i) - 48);
        fractionScale *= 10;
        sawDigit = true;
        i++;
      }
    }

    if (!sawDigit) {
      throw new Error(`time: invalid duration ${JSON.stringify(s)}`);
    }

    const unitStart = i;
    while (i < s.length && !(s[i] >= "0" && s[i] <= "9") && s[i] !== ".") {
      i++;
    }
    const unit = s.slice(unitStart, i);
    if (unit === "") {
      throw new Error(`time: missing unit in duration ${JSON.stringify(s.slice(start))}`);
    }

    const scale = durationUnit(unit);
    if (scale === null) {
      throw new Error(`time: unknown unit ${JSON.stringify(unit)} in duration ${JSON.stringify(s)}`);
    }

    const value = whole + fraction / fractionScale;
    totalNs += Math.trunc(value * scale);
    sawNumber = true;
  }

  if (!sawNumber) throw new Error(`time: invalid duration ${JSON.stringify(s)}`);
  return (negative ? -totalNs : totalNs) / MILLISECOND;
}

function durationUnit(unit: string): number | null {
  switch (unit) {
    case "ns":
      return NANOSECOND;
    case "us":
    case "\u00b5s": // µs
    case "\u03bcs": // μs
      return MICROSECOND;
    case "ms":
      return MILLISECOND;
    case "s":
      return SECOND;
    case "m":
      return MINUTE;
    case "h":
      return HOUR;
    default:
      return null;
  }
}

/**
 * Renders milliseconds the way Go's `time.Duration.String()` does, e.g.
 * `"30s"`, `"20m0s"`, `"1.2s"`, `"500ms"`. Used in the virtual-computer
 * timeout warning, where the standing value is named.
 */
export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms)) return String(ms);
  if (ms === 0) return "0s";

  const negative = ms < 0;
  let ns = Math.round(Math.abs(ms) * MILLISECOND);

  let out: string;
  if (ns < MICROSECOND) {
    out = `${ns}ns`;
  } else if (ns < MILLISECOND) {
    out = `${ns / MICROSECOND}\u00b5s`;
  } else if (ns < SECOND) {
    out = `${ns / MILLISECOND}ms`;
  } else {
    const hours = Math.floor(ns / HOUR);
    ns -= hours * HOUR;
    const minutes = Math.floor(ns / MINUTE);
    ns -= minutes * MINUTE;
    const seconds = ns / SECOND;
    out = "";
    if (hours > 0) out += `${hours}h`;
    if (minutes > 0 || hours > 0) out += `${minutes}m`;
    out += `${seconds}s`;
  }

  return negative ? `-${out}` : out;
}
