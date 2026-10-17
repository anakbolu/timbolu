/**
 * The LLM gateway contract: one internal message format, one Provider
 * interface, and one streaming event type.
 *
 * Ported from internal/modules/llm/domain/provider.go. The wire-level shapes
 * (Message, ChatRequest, StreamEvent, Capabilities, …) are the ones the
 * platform adapters implement and are re-exported from
 * `platform/llm/registry.ts`; the constants the Go package declares and the
 * usage/quota/metrics/audit ports are declared here.
 */

import type {
  FinishReason,
  Provider as PlatformProvider,
  Role,
  StreamEventType,
  ToolChoice,
  Usage,
} from "../../../platform/llm/registry.ts";
import type { Scope } from "./gateway.ts";

export type {
  Capabilities,
  ChatRequest,
  ChatResponse,
  EmbedRequest,
  EmbedResponse,
  FinishReason,
  Image,
  LLMError,
  Message,
  Provider,
  RequestMetadata,
  Role,
  StreamEvent,
  StreamEventType,
  Tool,
  ToolCall,
  ToolChoice,
  ToolResult,
  Usage,
} from "../../../platform/llm/registry.ts";

export {
  AdapterAnthropic,
  AdapterOpenAI,
  EmbeddingDimensions,
  ErrContextTooLong,
  ErrDuplicateProvider,
  ErrInvalidRequest,
  ErrNoProvider,
  ErrProviderNotFound,
  ErrProviderUnavailable,
  ErrQuotaExceeded,
  ErrRateLimited,
  ErrUnsupported,
  ErrUsageNotRecorded,
  estimateTokens,
  isLLMError,
  KnownAdapters,
  llmError,
  PurposeAgent,
  PurposeEmbedding,
  PurposeExtraction,
  PurposeRouting,
  retryable,
  supports,
  usageTotal,
} from "../../../platform/llm/registry.ts";

/** Who wrote a message. */
export const RoleSystem: Role = "system";
export const RoleUser: Role = "user";
export const RoleAssistant: Role = "assistant";
export const RoleTool: Role = "tool";

/** Why the model stopped. */
export const FinishStop: FinishReason = "stop";
export const FinishToolCalls: FinishReason = "tool_calls";
export const FinishLength: FinishReason = "length";
export const FinishContentFilter: FinishReason = "content_filter";
export const FinishUnknown: FinishReason = "unknown";

/** How firmly the model must use a tool. */
export const ToolChoiceAuto: ToolChoice = "auto";
export const ToolChoiceNone: ToolChoice = "none";
export const ToolChoiceRequired: ToolChoice = "required";

/** The kinds of a streaming event. */
export const EventText: StreamEventType = "text";
export const EventToolCall: StreamEventType = "tool_call";
export const EventUsage: StreamEventType = "usage";
export const EventDone: StreamEventType = "done";
export const EventError: StreamEventType = "error";

/** One row of the usage ledger. */
export interface UsageEntry {
  workspaceId: string;
  agentId: string;
  taskId: string;
  provider: string;
  model: string;
  purpose: string;
  usage: Usage;
  costMicros: number;
  createdAt: Date;
}

/** One day of aggregated usage, which the dashboard and the quota screen read instead of scanning the ledger. */
export interface UsageDaily {
  day: Date;
  provider: string;
  model: string;
  purpose: string;
  calls: number;
  inputTokens: number;
  outputTokens: number;
  cacheReadTokens: number;
  cacheWriteTokens: number;
  costMicros: number;
}

/** The billable token count of the day. */
export function usageDailyTotal(d: UsageDaily): number {
  return d.inputTokens + d.outputTokens;
}

/** What a workspace spent over a window, read three ways. */
export interface UsageReport {
  /**
   * The first moment the window covers, in the deployment's own timezone
   * (Asia/Jakarta). Resolved where the window is resolved.
   */
  periodStartedAt: Date;
  /** Every call in the window. */
  totals: UsageTotals;
  /** The same spend grouped by the Bolu that made it, dearest first. */
  byAgent: UsageByAgent[];
  /** The same spend grouped by the task that caused it, dearest first. */
  byTask: UsageByTask[];
}

/** One window's spend. */
export interface UsageTotals {
  calls: number;
  inputTokens: number;
  outputTokens: number;
  cacheReadTokens: number;
  cacheWriteTokens: number;
  costMicros: number;
}

/** The billable token count of the window. */
export function usageTotalsTotal(t: UsageTotals): number {
  return t.inputTokens + t.outputTokens;
}

/**
 * How many Bolu and how many tasks one report lists when the caller does not
 * say. A page rather than the whole of history.
 */
export const DefaultReportPage = 50;

/** One Bolu's share of a window. */
export interface UsageByAgent {
  agentId: string;
  agentName: string;
  calls: number;
  inputTokens: number;
  outputTokens: number;
  costMicros: number;
}

/** The billable token count for this Bolu. */
export function usageByAgentTotal(u: UsageByAgent): number {
  return u.inputTokens + u.outputTokens;
}

/** One task's share of a window. */
export interface UsageByTask {
  taskId: string;
  taskTitle: string;
  taskStatus: string;
  calls: number;
  inputTokens: number;
  outputTokens: number;
  costMicros: number;
}

/** The billable token count for this task. */
export function usageByTaskTotal(u: UsageByTask): number {
  return u.inputTokens + u.outputTokens;
}

/** Reads the aggregated usage of a workspace. */
export interface UsageReader {
  /** Per-day totals for the last days, most recent first. */
  daily(ctx: AbortSignal | undefined, workspaceId: string, since: Date): Promise<UsageDaily[]>;
  /** The period's totals over a window, with the per-Bolu and per-task breakdowns. */
  report(
    ctx: AbortSignal | undefined,
    workspaceId: string,
    since: Date,
    limit: number,
  ): Promise<UsageReport>;
  /**
   * The tokens recorded since a moment, the durable fallback when the live
   * counter is unavailable.
   */
  sumSince(ctx: AbortSignal | undefined, workspaceId: string, since: Date): Promise<number>;
  /** The cost recorded since a moment, in micro-rupiah. */
  costSince(ctx: AbortSignal | undefined, workspaceId: string, since: Date): Promise<number>;
}

/** Where the model gateway reports the calls it made. */
export interface Metrics {
  /**
   * One call that reached a provider. `seconds` is how long the provider took,
   * and `fellBack` is whether this deployment had already tried another one for
   * the same request.
   */
  modelCall(
    ctx: AbortSignal | undefined,
    provider: string,
    purpose: string,
    fellBack: boolean,
    seconds: number,
  ): void;
}

/** Persists what a call cost. Every provider call goes through it. */
export interface UsageRecorder {
  record(ctx: AbortSignal | undefined, entry: UsageEntry): Promise<void>;
}

/** Rejects a call when the workspace has used up its token allowance. */
export interface QuotaChecker {
  check(ctx: AbortSignal | undefined, workspaceId: string): Promise<void>;
}

/**
 * The live spend of a workspace: a cache in front of the ledger, tracking a
 * token allowance per billing period and a cost ceiling per day.
 */
export interface QuotaCounter {
  /** The tokens recorded for the current period. */
  spent(ctx: AbortSignal | undefined, workspaceId: string): Promise<number>;
  /** Records tokens spent by one call. */
  add(ctx: AbortSignal | undefined, workspaceId: string, tokens: number): Promise<void>;
  /** The cost recorded for the current day, in micro-rupiah. */
  spentCostToday(ctx: AbortSignal | undefined, workspaceId: string): Promise<number>;
  /** Records what one call cost. */
  addCost(ctx: AbortSignal | undefined, workspaceId: string, costMicros: number): Promise<void>;
}

/**
 * What a workspace may spend. Zero means unlimited, and the two bounds are
 * separate.
 */
export interface QuotaPolicy {
  /** The token allowance of the current period. */
  allowance(ctx: AbortSignal | undefined, workspaceId: string): Promise<number>;
  /** The cost ceiling of one day, in micro-rupiah. */
  dailyCostAllowance(ctx: AbortSignal | undefined, workspaceId: string): Promise<number>;
}

/** Records what a person did, for the workspace's trail. */
export interface Auditor {
  record(ctx: AbortSignal | undefined, scope: Scope, entry: AuditEntry): Promise<void>;
}

/** One thing that happened, for the record. */
export interface AuditEntry {
  action: string;
  objectType: string;
  objectId: string;
  actorUserId: string;
  metadata: Record<string, unknown>;
}

/** Reports the time, so cost and expiry rules are testable. */
export type Clock = () => Date;

/** The provider contract as this module's consumers see it. */
export type LLMProvider = PlatformProvider;
