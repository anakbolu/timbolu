/**
 * The recording API: one method per measurement, taking plain values.
 *
 * Ported from packages/platform/metrics/recorder.go.
 *
 * They live on the provider rather than on the instruments so a caller never has
 * to ask whether a provider exists — a deployment with metrics off must not have
 * every call site carrying a branch. A null provider records nothing.
 *
 * Every label is classified against a vocabulary, so a reason string a future
 * version of the product invents becomes `other` rather than a new time series.
 */

import type { Provider } from "./metrics";

/** Records how long a draft took to be ready for a person. */
export function draftReady(p: Provider | null, seconds: number): void {
  if (!p || !p.enabled()) return;
  p.instruments.draftReady(seconds);
}

/**
 * Records what a person did with a draft.
 *
 * `outcome` is the deployment's own vocabulary — approved, revised, rejected —
 * rather than a status a table happens to store, so a renamed column does not
 * open a new time series.
 */
export function draftDecision(p: Provider | null, outcome: string, revision: boolean): void {
  if (!p || !p.enabled()) return;
  p.instruments.draftDecision(classify(outcome, DRAFT_OUTCOMES), revision);
}

/** Records a task that reached a terminal state, with what it spent. */
export function taskFinished(
  p: Provider | null,
  trigger: string,
  status: string,
  seconds: number,
  inputTokens: number,
  outputTokens: number,
  costMicros: number,
): void {
  if (!p || !p.enabled()) return;
  p.instruments.taskFinished(
    classify(trigger, TASK_TRIGGERS),
    classify(status, TASK_OUTCOMES),
    seconds,
    inputTokens,
    outputTokens,
    costMicros,
  );
}

/**
 * Records one tool call and whether it failed.
 *
 * It is labelled by the *integration* rather than by the tool name: "which
 * integration is failing" is the incident, and which of its operations failed is
 * a second question the step rows answer exactly.
 */
export function toolCall(p: Provider | null, app: string, failed: boolean, seconds: number): void {
  if (!p || !p.enabled()) return;
  p.instruments.toolCall(classifyApp(app), failed, seconds);
}

/** Records one provider call. */
export function modelCall(
  p: Provider | null,
  provider: string,
  purpose: string,
  fellBack: boolean,
  seconds: number,
): void {
  if (!p || !p.enabled()) return;
  p.instruments.modelCall(provider, classify(purpose, MODEL_PURPOSES), fellBack, seconds);
}

/** Records a request a limit refused. */
export function rateLimited(p: Provider | null, surface: string): void {
  if (!p || !p.enabled()) return;
  p.instruments.rateLimited(classify(surface, RATE_LIMIT_SURFACES));
}

/**
 * Records work this deployment accepted.
 *
 * It is one half of the depth rather than an increment of it: the task is
 * finished by a worker in another process, so the two counters are scraped from
 * the same fleet and the depth is their difference.
 */
export function taskDispatched(p: Provider | null): void {
  if (!p || !p.enabled()) return;
  p.instruments.taskDispatched();
}

/** Records work that reached a terminal state. */
export function taskSettled(p: Provider | null): void {
  if (!p || !p.enabled()) return;
  p.instruments.taskSettled();
}

/**
 * Records an activity attempt past the first.
 *
 * It is recorded where the attempt happens rather than counted from a log,
 * because "the retry rate went up" is the first sign of a provider or a queue
 * degrading.
 */
export function activityRetry(p: Provider | null, activity: string, attempt: number): void {
  if (!p || !p.enabled()) return;
  p.instruments.activityRetry(classify(activity, ACTIVITY_NAMES), attemptBucket(attempt));
}

/** Records what a retention pass removed. */
export function retentionPruned(p: Provider | null, kind: string, count: number): void {
  if (!p || !p.enabled() || count === 0) return;
  p.instruments.retentionPruned(classify(kind, RETENTION_KINDS), count);
}

// ---------------------------------------------------------------- vocabularies

/** A set of allowed label values. */
export type Vocabulary = Record<string, true>;

/** Builds a vocabulary from its members. `other` is always accepted. */
export function newVocabulary(...values: string[]): Vocabulary {
  const set: Vocabulary = { other: true };
  for (const value of values) set[value] = true;
  return set;
}

/**
 * The spellings mirror the approval module's own status values, which is what
 * its service reports: a status it renames becomes `other` rather than a new
 * series.
 */
export const DRAFT_OUTCOMES = newVocabulary("approved", "revise", "sent", "canceled");
export const TASK_TRIGGERS = newVocabulary("chat", "routine", "webhook", "handoff");
export const TASK_OUTCOMES = newVocabulary("succeeded", "failed", "canceled");
/** The model gateway's own Purpose values, which is what its callers set. */
export const MODEL_PURPOSES = newVocabulary("agent", "routing", "extraction", "embedding");
export const RATE_LIMIT_SURFACES = newVocabulary("login", "integration", "chat", "upload");
export const RETENTION_KINDS = newVocabulary("step", "attachment");
/**
 * The applications this codebase ships. `order` is one of them even though it is
 * not a connected account: its tools reach no provider, and they are still this
 * deployment's own.
 */
export const INTEGRATION_APPS = newVocabulary("gmail", "whatsapp", "order");
export const ACTIVITY_NAMES = newVocabulary(
  "LoadTaskContext",
  "CallModel",
  "RunTool",
  "RequestDraft",
  "CarryOutDraft",
  "RecordStep",
  "BeatTask",
  "CompleteTask",
  "PrepareHandoff",
  "ResumeTask",
  "MarkHandoffStarted",
  "ExtractConversation",
);

/** Where an unrecognised value lands. */
export const OTHER_LABEL = "other";

/**
 * Turns an attempt number into a bounded label.
 *
 * The exact number is a series per retry depth, and a queue that is failing hard
 * produces attempts that climb forever. What an operator acts on is the shape.
 */
export function attemptBucket(attempt: number): string {
  if (attempt <= 1) return "1";
  if (attempt === 2) return "2";
  if (attempt <= 5) return "3-5";
  return "6+";
}

/**
 * Keeps an integration name inside its vocabulary.
 *
 * An integration connected over MCP carries whatever name the workspace gave its
 * server, so an open label would be one time series per customer's own server.
 */
export function classifyApp(app: string): string {
  return classify(app, INTEGRATION_APPS);
}

/** Keeps a label inside its vocabulary. */
export function classify(value: string, allowed: Vocabulary): string {
  return allowed[value] ? value : OTHER_LABEL;
}
