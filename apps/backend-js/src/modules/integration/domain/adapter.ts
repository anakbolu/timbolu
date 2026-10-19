/**
 * The adapter contract. An application's entire surface — what it can do, what
 * it can tell us about, and how to run one call — is behind these types, so the
 * agent worker never learns that Gmail and Jira are different things. Adding an
 * application is adding an adapter.
 *
 * Ported 1:1 from internal/modules/integration/domain/adapter.go.
 */

import type { Scope, Token } from "./integration.ts";
import type { OAuthTokens, TriggerEvent } from "./oauth.ts";
import type { WebhookRequest } from "./webhook.ts";

/** Pushed to us by the provider. */
export const TriggerWebhook = "webhook";
/** Asked for on a schedule, for a provider with no webhook. */
export const TriggerPolling = "polling";

/** Two adapters claimed the same application. */
export const ErrAdapterExists = new Error("integration: an adapter for this app is already registered");
/** No adapter handles an application. */
export const ErrUnknownAdapter = new Error("integration: no adapter for this app");
/** The adapter has no tool by that name. */
export const ErrUnknownTool = new Error("integration: unknown tool");
/** An adapter declared something the registry refuses. */
export const ErrInvalidSpec = new Error("integration: the adapter declared an invalid spec");
/** A call's arguments did not match the tool's schema. */
export const ErrSchemaViolation = new Error("integration: the arguments do not match the tool's schema");

/** One thing an application can do. */
export interface ToolSpec {
  /** The tool's identity and the model's handle on it, as `app.action`. */
  name: string;
  /** One of the Label* constants. */
  label: string;
  /** What the model reads when choosing a tool. */
  description: string;
  /** The JSON Schema of the arguments, as raw JSON text. */
  schema: string;
}

/** The application part of a tool name: `gmail` for `gmail.send`. */
export function toolSpecApp(spec: ToolSpec): string {
  const cut = spec.name.indexOf(".");
  return cut < 0 ? spec.name : spec.name.slice(0, cut);
}

/** Checks the spec the way the registry and the schema do. Returns an error message or null. */
export function validateToolSpec(spec: ToolSpec): Error | null {
  if (spec.name.trim() === "") {
    return invalidSpec("a tool needs a name");
  }
  if (!spec.name.includes(".")) {
    return invalidSpec(`${JSON.stringify(spec.name)} must be named \`app.action\``);
  }
  if (spec.label !== "read" && spec.label !== "write_internal" && spec.label !== "write_external") {
    return invalidSpec(`${JSON.stringify(spec.name)} has label ${JSON.stringify(spec.label)}`);
  }
  if (!spec.schema || spec.schema.length === 0) {
    return invalidSpec(`${JSON.stringify(spec.name)} has no schema`);
  }
  try {
    JSON.parse(spec.schema);
  } catch {
    return invalidSpec(`${JSON.stringify(spec.name)} has a schema that is not JSON`);
  }
  return null;
}

/** Something an application can tell us about. */
export interface TriggerSpec {
  app: string;
  kind: string;
  /** What happened, as `app.thing`. */
  event: string;
  description: string;
}

/** Checks the trigger the way the registry does. */
export function validateTriggerSpec(trigger: TriggerSpec): Error | null {
  if (trigger.app.trim() === "") {
    return invalidSpec("a trigger needs an app");
  }
  if (trigger.kind !== TriggerWebhook && trigger.kind !== TriggerPolling) {
    return invalidSpec(`trigger ${JSON.stringify(trigger.event)} has kind ${JSON.stringify(trigger.kind)}`);
  }
  if (!trigger.event.includes(".")) {
    return invalidSpec(`trigger ${JSON.stringify(trigger.event)} must be named \`app.thing\``);
  }
  return null;
}

/** One tool invocation, with the credential already opened. */
export interface Call {
  scope: Scope;
  integrationId: string;
  tool: string;
  /** Raw JSON arguments. */
  arguments: string;
  token: Token;
}

/** What a tool answered. */
export interface Result {
  /** What the model reads, as raw JSON. */
  content: string;
  /** One line for the step ledger and the task feed. */
  summary: string;
}

/** What a limiter answers about one call. */
export interface LimitDecision {
  allowed: boolean;
  /** Milliseconds. */
  retryAfter: number;
  perMinute: number;
}

/** Decides whether a workspace may spend one call on an integration. */
export interface CallLimiter {
  allow(ctx: unknown, app: string, integrationId: string): Promise<LimitDecision>;
}

/** Says a call was held rather than refused. */
export class RateLimitedError extends Error {
  constructor(
    readonly app: string,
    readonly perMinute: number,
    readonly retryAfter: number,
  ) {
    super(
      `${ErrRateLimited.message}: ${app} allows ${perMinute} calls a minute and they are spent; retry in ${formatDuration(retryAfter)}`,
    );
    this.name = "RateLimitedError";
  }
}

/** The sentinel `RateLimitedError` answers for. */
export const ErrRateLimited = new Error("integration: the call limit for this integration is reached");

/**
 * An adapter whose tools belong to the workspace itself rather than to a
 * connected account.
 */
export interface Connectionless {
  needsConnection(): boolean;
}

/**
 * An adapter that says where its credential comes from. An adapter that says
 * nothing needs a token, which is the common case and the safe default.
 */
export interface Credentialled {
  needsToken(): boolean;
}

/** One application's whole surface. */
export interface Adapter {
  app(): string;
  tools(): ToolSpec[];
  triggers(): TriggerSpec[];
  execute(ctx: unknown, call: Call): Promise<Result>;
}

/** Compiles and applies the JSON Schema of a tool's arguments. */
export interface SchemaValidator {
  validate(schema: string, args: string): Promise<void>;
}

/** Every adapter this deployment knows, by application. */
export interface Registry {
  register(adapter: Adapter): void;
  apps(): string[];
  adapter(app: string): Adapter | null;
  tools(): ToolSpec[];
  toolsFor(app: string): ToolSpec[];
  triggers(): TriggerSpec[];
  lookup(name: string): { spec: ToolSpec; adapter: Adapter } | null;
}

/** Builds a tool name from an application and an action. */
export function toolName(app: string, action: string): string {
  return `${app}.${action}`;
}

/** True for an adapter connected through OAuth. */
export function isOAuthAdapter(adapter: Adapter): adapter is Adapter & {
  authCodeUrl(state: string, redirectUri: string): string;
  exchange(ctx: unknown, code: string, redirectUri: string): Promise<OAuthTokens>;
  refresh(ctx: unknown, refreshToken: string): Promise<OAuthTokens>;
  defaultScopes(): string[];
} {
  const candidate = adapter as unknown as Record<string, unknown>;
  return (
    typeof candidate.authCodeUrl === "function" &&
    typeof candidate.exchange === "function" &&
    typeof candidate.refresh === "function" &&
    typeof candidate.defaultScopes === "function"
  );
}

/** True for an adapter that receives pushes. */
export function isTriggerAdapter(adapter: Adapter): adapter is Adapter & {
  verifyWebhook(ctx: unknown, request: WebhookRequest): Promise<TriggerEvent[]>;
} {
  return typeof (adapter as unknown as Record<string, unknown>).verifyWebhook === "function";
}

/** True for an adapter that carries its own credential. */
export function isCredentialled(adapter: Adapter): adapter is Adapter & Credentialled {
  return typeof (adapter as unknown as Record<string, unknown>).needsToken === "function";
}

/** True for an adapter whose tools need no connection. */
export function isConnectionless(adapter: Adapter): adapter is Adapter & Connectionless {
  return typeof (adapter as unknown as Record<string, unknown>).needsConnection === "function";
}

function invalidSpec(detail: string): Error {
  return new Error(`${ErrInvalidSpec.message}: ${detail}`);
}

/** Renders a duration the way Go's `Duration.Round(time.Second)` does. */
function formatDuration(millis: number): string {
  const seconds = Math.round(millis / 1000);
  return `${seconds}s`;
}
