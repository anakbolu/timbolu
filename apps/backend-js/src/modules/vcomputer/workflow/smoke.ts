/**
 * The durable half of the virtual computer: one workflow that drives a bot's
 * sandbox through every activity the module offers.
 *
 * It is a smoke run rather than a task. The agent task loop does not reach the
 * sandbox yet, so what this workflow is for is proving the whole seam — agent
 * worker → activities → control plane → sandbox — before anything depends on it.
 * A deployment with no control plane configured fails it on the first step with
 * `ErrNotConfigured`, which is the honest answer rather than a silent pass.
 *
 * The two sleeps are the point of a smoke run rather than padding: a browser that
 * was just pointed at a page, and a window that was just typed into, are not ready
 * in the same instant.
 *
 * The run is also what ends the session it started. That is the one place this
 * differs from a task, which deliberately leaves its session up so the next one
 * reuses the browser profile.
 *
 * Ported 1:1 from internal/modules/vcomputer/workflow/smoke.go.
 */

import { CancellationScope, proxyActivities, sleep, type ActivityOptions } from "@temporalio/workflow";
import type { ExecResult, Session } from "../domain/ports.ts";
import type {
  ClickRequest,
  ExecRequest,
  NavigateRequest,
  ScreenshotResult,
  SessionRequest,
  TypeRequest,
} from "../temporal/activities.ts";

/** The name the worker registers this workflow under. */
export const SmokeWorkflowName = "SmokeWorkflow";

/**
 * The activity set, as this workflow sees it. The names come from the methods,
 * which is what keeps the set the workflow calls and the set the worker
 * registers from disagreeing.
 */
interface ActivitiesShape {
  EnsureSession(request: SessionRequest): Promise<Session>;
  StopSession(request: SessionRequest): Promise<void>;
  Screenshot(request: SessionRequest): Promise<ScreenshotResult>;
  Click(request: ClickRequest): Promise<void>;
  Type(request: TypeRequest): Promise<void>;
  Navigate(request: NavigateRequest): Promise<void>;
  Exec(request: ExecRequest): Promise<ExecResult>;
}

/** Bounds the one call that may have to create the sandbox. */
const ensureSessionTimeout = 90_000;
/** Bounds one of the remaining calls: navigate, screenshot, click, type, exec. */
const stepTimeout = 30_000;
/**
 * How many times one step is attempted. Three tolerates the transient half of the
 * seam — a control plane restarting, a sandbox not yet accepting connections —
 * without turning a real failure into a long wait. Configuration mistakes are not
 * retried at all: the activities answer them as non-retryable errors.
 */
const maximumAttempts = 3;
/** How long the browser is given to render what it was pointed at. */
const navigateSettle = 3_000;
/** How long the desktop is given to show what was clicked and typed. */
const inputSettle = 1_000;
/**
 * The command the run executes in the sandbox.
 *
 * It is fixed rather than taken from the input because it is the check, not the
 * payload: `echo` proves a shell ran and `whoami` proves which user it ran as.
 */
const smokeCommand = "echo vcomputer-smoke && whoami";

/** One run: which bot's computer to drive, where to point its browser, and what to click and type. */
export interface SmokeInput {
  bot_id: string;
  /** Where Navigate points the browser. The control plane validates the scheme. */
  url: string;
  click_x: number;
  click_y: number;
  /** What is typed into the focused window after the click. */
  text: string;
  /**
   * Leaves the sandbox running when the run is over. The default — false — is the
   * one that costs nothing: a smoke run is a person watching it, and the sandbox
   * it drove is of no further use to anyone.
   */
  keep_session: boolean;
}

/** What the run observed. */
export interface SmokeResult {
  /** The state the control plane reported when the session was ensured. */
  session_state: string;
  /** The sandbox's DevTools endpoint. */
  cdp_url: string;
  /** The size of the capture taken after the navigation. */
  screenshot_before_bytes: number;
  /** The size of the capture taken after the click and the text. */
  screenshot_after_bytes: number;
  /** What the command printed. */
  exec_stdout: string;
  /** Says whether the runtime really ran the command. */
  exec_stub: boolean;
  /** Says whether the run ended the session it started. */
  session_stopped: boolean;
}

function activityOptions(startToCloseTimeout: number): ActivityOptions {
  return {
    startToCloseTimeout,
    retry: { maximumAttempts },
  };
}

/**
 * Drives one bot's virtual computer through every activity the module offers, in
 * the order a task would use them, and reports what each one answered.
 *
 * It ends the session it started unless `keep_session` says otherwise, and it
 * does so on every path: the cleanup is scheduled on a non-cancellable scope, so
 * a run that failed halfway still takes its sandbox with it. A stop that failed is
 * a failure of the run — the leftover container is the complaint this answers —
 * and it is reported alongside whatever failed first.
 */
export async function SmokeWorkflow(input: SmokeInput): Promise<SmokeResult> {
  const result: SmokeResult = {
    session_state: "",
    cdp_url: "",
    screenshot_before_bytes: 0,
    screenshot_after_bytes: 0,
    exec_stdout: "",
    exec_stub: false,
    session_stopped: false,
  };

  const ensureCtx = proxyActivities<ActivitiesShape>(activityOptions(ensureSessionTimeout));
  const stepCtx = proxyActivities<ActivitiesShape>(activityOptions(stepTimeout));

  let session: Session;
  let failure: unknown = null;
  let stopFailure: unknown = null;

  try {
    session = await ensureCtx.EnsureSession({ bot_id: input.bot_id });

    await stepCtx.Navigate({ bot_id: input.bot_id, url: input.url });
    await sleep(navigateSettle);

    const before = await stepCtx.Screenshot({ bot_id: input.bot_id });

    await stepCtx.Click({ bot_id: input.bot_id, x: input.click_x, y: input.click_y });
    await stepCtx.Type({ bot_id: input.bot_id, text: input.text });
    await sleep(inputSettle);

    const after = await stepCtx.Screenshot({ bot_id: input.bot_id });
    const executed = await stepCtx.Exec({ bot_id: input.bot_id, command: smokeCommand });

    result.session_state = session.state;
    result.cdp_url = session.cdp_url ?? "";
    result.screenshot_before_bytes = before.Bytes;
    result.screenshot_after_bytes = after.Bytes;
    result.exec_stdout = executed.stdout;
    result.exec_stub = executed.stub;
  } catch (err) {
    failure = err;
  } finally {
    if (!input.keep_session) {
      // The run owns the sandbox it just made, so it is the run that ends it — on
      // every path, including the one where a later step failed. The
      // non-cancellable scope is what makes the cleanup survive the case that
      // needs it most: a workflow that is failing or being cancelled.
      try {
        await CancellationScope.nonCancellable(() =>
          stepCtx.StopSession({ bot_id: input.bot_id }),
        );
        result.session_stopped = true;
      } catch (err) {
        // A stop that failed is a container still running, so it is the run's own
        // failure rather than a note on a success. It is joined rather than
        // substituted when a step failed first: the two are different answers, and
        // the leftover sandbox is the one an operator has to act on.
        stopFailure = err;
      }
    }
  }

  if (failure !== null || stopFailure !== null) {
    throw joinedFailure(input.bot_id, failure, stopFailure);
  }

  return result;
}

/**
 * Joins the two failures the way Go's `errors.Join` does, keeping both messages.
 *
 * The result is one plain `Error` rather than an `AggregateError`: the SDK
 * converts an error thrown by a workflow into a failure, and a message that
 * carries both answers is what an operator reads — the step that failed first,
 * and the sandbox that was left behind.
 */
function joinedFailure(botId: string, failure: unknown, stopFailure: unknown): Error {
  const messageOf = (err: unknown): string => {
    // The SDK's `ActivityFailure` keeps the activity's own sentence in `cause`,
    // which is the part an operator needs.
    let current: unknown = err;
    for (let guard = 0; guard < 16 && current instanceof Error && current.cause instanceof Error; guard++) {
      current = current.cause;
    }
    return current instanceof Error ? current.message : String(current);
  };

  if (stopFailure !== null) {
    const stop = `stop the session for ${botId}: ${messageOf(stopFailure)}`;
    if (failure === null) return new Error(stop);
    return new Error(`${messageOf(failure)}\n${stop}`, { cause: failure });
  }

  return failure instanceof Error ? failure : new Error(messageOf(failure));
}
