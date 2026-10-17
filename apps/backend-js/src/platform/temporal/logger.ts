/**
 * The Temporal SDK logger adapter.
 *
 * Ported from apps/backend/internal/platform/temporal/logger.go. The Go
 * adapter wraps zap; this one wraps the deployment's pino logger, so a
 * workflow's own lines (`workflow.log`) and the SDK's lines land in the same
 * stream, with the same fields, at the same level as everything else.
 *
 * The TypeScript SDK has no per-client logger option: the logger is installed
 * process-wide through `Runtime.logger` (from `@temporalio/worker`). This class
 * is the object to hand it.
 */

import type { Logger as TemporalLogger, LogLevel, LogMetadata } from "@temporalio/common";
import { nopLogger, type Logger } from "../logger.ts";

/**
 * Writes the SDK's lines through the deployment's logger.
 *
 * The Go adapter carries a `keyvals` slice because `sdklog.With` derives a
 * child logger. The TypeScript `Logger` interface has no `with`: the SDK passes
 * the workflow's identifiers as `meta` on every call instead, and those are
 * flattened into the line here.
 */
export class ZapLogger implements TemporalLogger {
  private readonly logger: Logger;

  constructor(logger: Logger | null | undefined) {
    this.logger = logger ?? nopLogger;
  }

  log(level: LogLevel, message: string, meta?: LogMetadata): void {
    this.write(level, message, meta);
  }

  trace(message: string, meta?: LogMetadata): void {
    this.write("TRACE", message, meta);
  }

  debug(message: string, meta?: LogMetadata): void {
    this.write("DEBUG", message, meta);
  }

  info(message: string, meta?: LogMetadata): void {
    this.write("INFO", message, meta);
  }

  warn(message: string, meta?: LogMetadata): void {
    this.write("WARN", message, meta);
  }

  error(message: string, meta?: LogMetadata): void {
    this.write("ERROR", message, meta);
  }

  /**
   * Emits one line with the metadata flattened into fields.
   *
   * A symbol key or an unserialisable value would otherwise lose the field
   * entirely; `describeValue` keeps a readable rendering instead. The check
   * against the level first avoids building fields for a line nobody will see,
   * exactly as the Go adapter does.
   */
  private write(level: LogLevel, message: string, meta?: LogMetadata): void {
    const pinoLevel = levelToPino(level);
    if (!this.logger.isLevelEnabled(pinoLevel)) {
      return;
    }

    const fields: Record<string, unknown> = {};
    if (meta) {
      for (const key of Reflect.ownKeys(meta)) {
        const name = typeof key === "symbol" ? key.toString() : key;
        fields[name] = describeValue(meta[key as string]);
      }
    }

    this.logger[pinoLevel](fields, message);
  }
}

type PinoLevel = "trace" | "debug" | "info" | "warn" | "error";

function levelToPino(level: LogLevel): PinoLevel {
  switch (level) {
    case "TRACE":
      return "trace";
    case "DEBUG":
      return "debug";
    case "INFO":
      return "info";
    case "WARN":
      return "warn";
    case "ERROR":
      return "error";
    default:
      return "info";
  }
}

/**
 * A value pino can serialise. The Go adapter used `zap.Any`, which reflects
 * over the value; pino needs a plain value or a string, and an Error in
 * particular is rendered as `{}` otherwise.
 */
function describeValue(value: unknown): unknown {
  if (value instanceof Error) {
    return { name: value.name, message: value.message, stack: value.stack };
  }
  if (typeof value === "bigint") {
    return value.toString();
  }
  if (typeof value === "function") {
    return "[function]";
  }
  return value;
}
