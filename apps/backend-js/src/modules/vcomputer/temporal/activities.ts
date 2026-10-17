/**
 * The Temporal activities a worker registers to drive a bot's virtual computer.
 *
 * It is a thin layer for the same reason the other modules' activity packages
 * are: the workflow package must stay free of the module's services so replay
 * stays deterministic, and the client must stay free of the SDK so a unit test
 * can call it without a Temporal server. An activity is where those two meet.
 *
 * A configuration mistake is answered as a non-retryable Temporal error: a
 * missing control plane and a refused token will not become true by asking again,
 * so the activity stops instead of spending its attempts on the same refused
 * header (issue #218). Everything else the client classifies — an unreachable
 * control plane, a sandbox that is not up yet — stays retryable.
 *
 * Ported 1:1 from internal/modules/vcomputer/temporal/activities.go.
 */

import { ApplicationFailure } from "@temporalio/common";
import type { Logger } from "../../../platform/logger.ts";
import { nopLogger } from "../../../platform/logger.ts";
import {
  ErrNotConfigured,
  is,
  type ComputerUseClient,
  type ExecResult,
  type Session,
} from "../domain/ports.ts";

/** The activity dependencies. The client is the only required one. */
export interface Deps {
  /** The control plane. Absent is a deployment without a virtual computer. */
  computer?: ComputerUseClient | null;
  logger?: Logger | null;
}

/** The set a worker registers. */
export class Activities {
  constructor(private readonly deps: Deps) {}

  /** Makes sure the bot has a computer. */
  async EnsureSession(request: SessionRequest): Promise<Session> {
    try {
      return await this.client().ensureSession(undefined, request.bot_id);
    } catch (err) {
      throw permanent(err);
    }
  }

  /** Ends a session and keeps its volume. */
  async StopSession(request: SessionRequest): Promise<void> {
    try {
      await this.client().stopSession(undefined, request.bot_id);
    } catch (err) {
      throw permanent(err);
    }
  }

  /** The desktop as it is now. */
  async Screenshot(request: SessionRequest): Promise<ScreenshotResult> {
    let png: Uint8Array;
    try {
      png = await this.client().screenshot(undefined, request.bot_id);
    } catch (err) {
      throw permanent(err);
    }

    if (png.length === 0) {
      // An empty capture is not a failure of the sandbox, but it is useless to
      // the model, and returning it silently would turn into "the screen is
      // blank" in a task summary rather than into a bug.
      this.log().warn({ bot_id: request.bot_id }, "vcomputer: the control plane answered an empty screenshot");
    }

    return {
      // Base64, because that is how Go's `[]byte` travels on the Temporal wire:
      // a run recorded by the Go worker must replay through this activity.
      PNG: Buffer.from(png).toString("base64"),
      Bytes: png.length,
      CapturedAt: new Date().toISOString(),
    };
  }

  /** Presses a point on the desktop. */
  async Click(request: ClickRequest): Promise<void> {
    try {
      await this.client().click(undefined, request.bot_id, request.x, request.y);
    } catch (err) {
      throw permanent(err);
    }
  }

  /** Sends text to the focused window. */
  async Type(request: TypeRequest): Promise<void> {
    try {
      await this.client().type(undefined, request.bot_id, request.text);
    } catch (err) {
      throw permanent(err);
    }
  }

  /** Points the browser at a page. */
  async Navigate(request: NavigateRequest): Promise<void> {
    try {
      await this.client().navigate(undefined, request.bot_id, request.url);
    } catch (err) {
      throw permanent(err);
    }
  }

  /** Runs one command inside the sandbox and answers what it printed. */
  async Exec(request: ExecRequest): Promise<ExecResult> {
    try {
      return await this.client().exec(undefined, request.bot_id, request.command);
    } catch (err) {
      throw permanent(err);
    }
  }

  /** The single place the "no control plane" case is turned into an error. */
  private client(): ComputerUseClient {
    return this.deps.computer ?? unconfigured;
  }

  private log(): Logger {
    return this.deps.logger ?? nopLogger;
  }
}

/** Names the bot an activity acts on. */
export interface SessionRequest {
  bot_id: string;
}

/** One press. The coordinates come from a model reading a screenshot. */
export interface ClickRequest {
  bot_id: string;
  x: number;
  y: number;
}

/** One string sent to the focused window. */
export interface TypeRequest {
  bot_id: string;
  text: string;
}

/** One URL. */
export interface NavigateRequest {
  bot_id: string;
  url: string;
}

/** One command. It must never reach this activity without an approval behind it. */
export interface ExecRequest {
  bot_id: string;
  command: string;
}

/** Carries a capture back through the workflow. */
export interface ScreenshotResult {
  /** Base64 PNG, which is how a Go `[]byte` crosses the wire. */
  PNG: string;
  Bytes: number;
  CapturedAt: string;
}

/**
 * The type Temporal records for a configuration mistake.
 *
 * It is a name rather than a sentence because it is what a `RetryPolicy`'s
 * `nonRetryableErrorTypes` matches, and it is what a run's history shows beside
 * the message.
 */
export const NotConfiguredType = "VComputerNotConfigured";

/**
 * Where a deployment mistake stops being a retry.
 *
 * Temporal retries an activity until its policy says otherwise, and a missing
 * control plane or a refused token is the one class of failure that answers the
 * same way every time.
 */
export function permanent(err: unknown): unknown {
  if (err === null || err === undefined || !is(err, ErrNotConfigured)) {
    return err;
  }

  const message = err instanceof Error ? err.message : String(err);
  return ApplicationFailure.nonRetryable(message, NotConfiguredType, err);
}

/** The client a deployment without a control plane gets. */
const unconfigured: ComputerUseClient = {
  ensureSession: async () => {
    throw ErrNotConfigured;
  },
  stopSession: async () => {
    throw ErrNotConfigured;
  },
  screenshot: async () => {
    throw ErrNotConfigured;
  },
  click: async () => {
    throw ErrNotConfigured;
  },
  type: async () => {
    throw ErrNotConfigured;
  },
  navigate: async () => {
    throw ErrNotConfigured;
  },
  exec: async () => {
    throw ErrNotConfigured;
  },
};
