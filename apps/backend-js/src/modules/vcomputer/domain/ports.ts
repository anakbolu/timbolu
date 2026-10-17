/**
 * The vcomputer module's contract: the shape of a bot's virtual computer as the
 * backend sees it, and the one port the rest of the backend may use to reach it.
 *
 * The port is narrow on purpose. A single "run anything on the computer" method
 * would erase the granularity the approval gates depend on: browsing is `read`
 * — Navigate is the call that points the browser at a page — clicking and typing
 * are `write_external`, and exec is the most dangerous of the four. Each stays
 * its own method here so a caller can label them separately when they are wired.
 *
 * Ported 1:1 from internal/modules/vcomputer/domain/ports.go.
 */

/** Session states, mirroring what the control plane reports. */
export const StateStopped = "stopped";
export const StateRunning = "running";
export const StateError = "error";

/**
 * A call this deployment cannot make: either no control plane address was
 * configured, or the control plane refused the bearer token with a 401.
 *
 * Both are deployment mistakes, and neither becomes true by asking again — which
 * is why the activities answer it as a non-retryable Temporal error.
 */
export const ErrNotConfigured = new Error("vcomputer: the control plane is not configured");
/** An action on a bot whose session does not exist. */
export const ErrNoSession = new Error("vcomputer: no session for that bot");
/** A request the control plane refused. */
export const ErrInvalidInput = new Error("vcomputer: the control plane refused the request");
/** The control plane being unreachable or failing. It is worth retrying. */
export const ErrUnavailable = new Error("vcomputer: the control plane is unavailable");

/**
 * An error carrying the sentinel a caller switches on, so `is` reproduces Go's
 * `errors.Is` for the messages `fmt.Errorf("%w: %s")` produced.
 */
export class WrappedError extends Error {
  constructor(
    readonly sentinel: Error,
    message: string,
  ) {
    super(message);
    this.name = "WrappedError";
  }
}

/** Wraps a sentinel with a detail, rendering the message as Go's `%w: detail`. */
export function wrap(sentinel: Error, detail: string): WrappedError {
  return new WrappedError(sentinel, `${sentinel.message}: ${detail}`);
}

/** True when `err` is the sentinel or wraps it. */
export function is(err: unknown, sentinel: Error): boolean {
  let current: unknown = err;
  for (let guard = 0; guard < 32; guard++) {
    if (current === sentinel) return true;
    if (!(current instanceof Error)) return false;
    if (current instanceof WrappedError) {
      current = current.sentinel;
      continue;
    }
    current = "cause" in current ? current.cause : undefined;
  }
  return false;
}

/** The message of an unknown thrown value. */
export function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/** One bot's virtual computer. */
export interface Session {
  bot_id: string;
  /** One of the State* constants. */
  state: string;
  /** Names the implementation on the control plane's side: "memory" or "docker". */
  runtime: string;
  /** What an operator or a reaper needs to find the container. */
  container_id?: string;
  /**
   * Where a person watches the desktop. It is a path on the control plane, not a
   * sandbox address: the control plane reverse-proxies it to the sandbox's noVNC
   * port, which is never published.
   */
  novnc_url?: string;
  /** Chromium's DevTools endpoint, `http://<sandbox-ip>:<cdp-port>`. */
  cdp_url?: string;
  /** The named volume holding the browser profile and the bot's home. */
  volume?: string;
  created_at: string;
  last_used_at: string;
}

/**
 * The answer to one command.
 *
 * `stub` says whether the runtime actually ran it: false with the docker
 * runtime, true with the memory runtime, which echoes the command instead. A
 * caller has to read that field rather than the exit code.
 */
export interface ExecResult {
  command: string;
  stdout: string;
  stderr: string;
  exit_code: number;
  stub: boolean;
}

/** The backend's port onto the control plane. */
export interface ComputerUseClient {
  /** Creates the session if it does not exist and returns it either way. */
  ensureSession(ctx: AbortSignal | undefined, botId: string): Promise<Session>;
  /** Ends the session and keeps its volume. */
  stopSession(ctx: AbortSignal | undefined, botId: string): Promise<void>;
  /** The current desktop as PNG bytes. */
  screenshot(ctx: AbortSignal | undefined, botId: string): Promise<Uint8Array>;
  /** Presses a point on the desktop. A `write_external` action. */
  click(ctx: AbortSignal | undefined, botId: string, x: number, y: number): Promise<void>;
  /** Sends text to the focused window. Also `write_external`. */
  type(ctx: AbortSignal | undefined, botId: string, text: string): Promise<void>;
  /** Points the browser at a page. */
  navigate(ctx: AbortSignal | undefined, botId: string, url: string): Promise<void>;
  /** Runs a command inside the sandbox. */
  exec(ctx: AbortSignal | undefined, botId: string, command: string): Promise<ExecResult>;
}
