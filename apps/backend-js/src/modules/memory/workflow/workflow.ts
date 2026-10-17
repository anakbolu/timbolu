/**
 * The durable half of memory extraction: one workflow per conversation that
 * waits for the thread to stop moving and then asks the extractor to read it.
 *
 * The waiting is a Temporal timer rather than a sleep in the API process,
 * because the point of the delay is that it survives a deploy: a conversation
 * that goes quiet while a worker is being replaced is still extracted, and the
 * quiet period is measured in wall-clock time rather than in process uptime.
 *
 * This file is deliberately free of I/O. Everything that touches the network, a
 * database, or the clock happens in an activity, because a workflow is replayed
 * from its history after a worker restart.
 *
 * Ported 1:1 from internal/modules/memory/workflow/workflow.go.
 */

import {
  condition,
  defineSignal,
  proxyActivities,
  setHandler,
  type ActivityOptions,
} from "@temporalio/workflow";

/**
 * What the worker registers and what the schedule names.
 *
 * It is the function's own name, deliberately: the worker registers a workflow
 * by the name of the function it is handed, while the schedule starts one by
 * this string. Two names that are only equal by coincidence is the failure mode
 * where a workflow is scheduled, no worker recognises it, and the run retries
 * until it times out — which is a silent stall rather than a failure.
 */
export const WorkflowName = "MemoryExtractionWorkflow";

/** Reads the thread and writes what is worth keeping. */
export const ActivityExtract = "ExtractConversation";

/** What a message sends. It resets the quiet period. */
export const SignalActive = "conversation-active";

/**
 * How long a conversation must be still when the caller does not configure one.
 * Five minutes is longer than a reply and shorter than a coffee break: the
 * thread has stopped, but the person has not left the screen.
 */
export const DefaultQuietPeriod = 5 * 60_000;

/**
 * Bounds how many times one workflow may be reset before it gives up. A
 * conversation that keeps moving is not one to extract now, and each reset costs
 * workflow history: past the bound the run ends and the next message starts a
 * fresh one with an empty history.
 */
const maxQuietResets = 200;

/** Bounds one pass: a small model call and one embedding call. */
const extractionTimeout = 3 * 60_000;

/** The signal, named exactly as the Go workflow names its channel. */
export const activeSignal = defineSignal(SignalActive);

/**
 * One conversation to watch. Every field is a string so a workflow started by
 * an older deployment still deserialises.
 */
export interface ExtractionInput {
  WorkspaceID: string;
  ConversationID: string;
  /**
   * The Bolu that answered in this thread: its switch decides whether
   * extraction runs, and its private layer is where role-specific memories go.
   */
  AgentID: string;
  /**
   * How long the thread must not move, in milliseconds. It is carried in the
   * input so the configured period is the one that waits, rather than a
   * constant here disagreeing with the one in configuration.
   */
  QuietPeriod: number;
}

/** What one pass produced. */
export interface ExtractionResult {
  Disabled: boolean;
  Stored: number;
  Merged: number;
  Skipped: number;
  Refused: number;
}

/** The activity set, as the workflow calls it. Names come from the interface. */
export interface ActivitiesShape {
  ExtractConversation(input: ExtractionInput): Promise<ExtractionResult>;
}

const extractOptions: ActivityOptions = {
  startToCloseTimeout: extractionTimeout,
  // A pass that fails is worth one retry: the model provider may have been
  // briefly unavailable, and the memory is not urgent.
  retry: {
    initialInterval: "5s",
    backoffCoefficient: 2,
    maximumInterval: "60s",
    maximumAttempts: 2,
  },
};

const activities = proxyActivities<ActivitiesShape>(extractOptions);

/**
 * Waits for the conversation to go quiet and extracts once.
 *
 * The signals are what makes the wait a debounce rather than a delay: the
 * caller starts the workflow with SignalWithStart on every message, so the first
 * message starts a run and every later one pushes the timer out. When the timer
 * finally fires, nothing has been said for the whole quiet period.
 */
export async function MemoryExtractionWorkflow(input: ExtractionInput): Promise<ExtractionResult> {
  let quiet = input.QuietPeriod;
  if (!(quiet > 0)) {
    quiet = DefaultQuietPeriod;
  }

  let signals = 0;
  setHandler(activeSignal, () => {
    signals++;
  });

  for (let resets = 0; ; resets++) {
    const before = signals;
    // `condition` waits for the signal for at most the quiet period and reports
    // whether the predicate became true. That is the Go selector's timer/receive
    // race, expressed as the SDK's timer-backed condition.
    const reset = await condition(() => signals > before, quiet);

    if (!reset) {
      break;
    }
    if (resets >= maxQuietResets) {
      // The thread never settled. Ending here is better than growing one
      // workflow's history without bound; the next message opens a new run.
      return { Disabled: false, Stored: 0, Merged: 0, Skipped: 0, Refused: 0 };
    }
  }

  return activities.ExtractConversation(input);
}
