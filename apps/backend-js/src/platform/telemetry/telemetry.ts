/**
 * Deployment tracing.
 *
 * Ported from packages/platform/telemetry/telemetry.go.
 *
 * Three decisions are worth stating rather than discovering later:
 *
 * - **A deployment with no exporter is still traced.** The provider is always
 *   installed, so a request has a trace id and every log line can carry it; only
 *   the spans are dropped.
 * - **Sampling is parent-based, with a ratio for roots.** A trace a caller
 *   already sampled stays sampled; a ratio decides the rest.
 * - **No customer content is ever an attribute.** Spans carry identifiers,
 *   counts, model names, and outcomes.
 *
 * The Go side uses `go.opentelemetry.io/otel`. Those packages are not a
 * dependency of this app and are deliberately not added, so this module carries
 * the small surface the ported code needs: a tracer that starts spans, the W3C
 * `traceparent` propagator, and the `none` / `console` exporters. The `otlp`
 * exporter is a documented no-op that warns once at startup: spans stay in the
 * process, trace ids remain real.
 */

import { randomBytes } from "node:crypto";

import type { Logger } from "../logger";
import { nopLogger } from "../logger";

/** Exporters a deployment may name. */
export const EXPORTER_NONE = "none";
/** Prints spans to standard output, which is what a development run wants. */
export const EXPORTER_CONSOLE = "console";
/** Sends spans to a collector. Not implemented here — see the module comment. */
export const EXPORTER_OTLP = "otlp";

/** Bounds the flush on the way out, in milliseconds, so a stopping process still exits. */
export const SHUTDOWN_TIMEOUT = 5_000;

/** The instrumentation scope every span this codebase starts is filed under. */
export const SCOPE_NAME = "github.com/robzlabz/employeebot";

/** How a process is traced. */
export interface Config {
  /** What the spans are filed under: "bolu-api", "bolu-agent-worker". */
  serviceName: string;
  /** The deployment, e.g. "local" or "production". */
  environment: string;
  /** `none`, `console`, or `otlp`. An empty value is `none`. */
  exporter: string;
  /** The collector's host:port. Required by `otlp`, ignored by the others. */
  endpoint: string;
  /** The share of root traces to keep, between 0 and 1. */
  sampleRatio: number;
  /** Drops TLS on the collector connection. */
  insecure: boolean;
}

/** The identity of a span, in the W3C trace-context shape. */
export interface SpanContext {
  /** 32 lowercase hex characters. */
  traceId: string;
  /** 16 lowercase hex characters. */
  spanId: string;
  /** Bit 0 is the sampled flag. */
  traceFlags: number;
  /** True when the context arrived from another process. */
  isRemote?: boolean;
}

/** A span as the ported modules use it. */
export interface Span {
  spanContext(): SpanContext;
  setAttribute(key: string, value: AttributeValue): this;
  setAttributes(attributes: Record<string, AttributeValue>): this;
  addEvent(name: string, attributes?: Record<string, AttributeValue>): this;
  recordException(error: unknown): this;
  setStatus(status: { code: SpanStatusCode; message?: string }): this;
  end(): void;
  isRecording(): boolean;
}

export type AttributeValue = string | number | boolean | string[] | number[] | boolean[];

/** OpenTelemetry's status codes. */
export const SpanStatusCode = {
  UNSET: 0,
  OK: 1,
  ERROR: 2,
} as const;

export type SpanStatusCode = (typeof SpanStatusCode)[keyof typeof SpanStatusCode];

/** Options a caller passes when starting a span. */
export interface StartSpanOptions {
  attributes?: Record<string, AttributeValue>;
  /** An explicit parent, overriding the one carried in the context. */
  parent?: SpanContext | null;
  /** Overrides the provider's sampler for this span. */
  root?: boolean;
}

/** The tracer the ported modules start spans on. */
export interface Tracer {
  startSpan(name: string, options?: StartSpanOptions, parent?: SpanContext | null): Span;
}

/**
 * The installed tracer provider.
 *
 * `enabled` is whether spans leave the process. It is false for `none`, and for
 * `otlp` (whose exporter is not implemented), and true for `console`.
 */
export class Provider {
  private readonly log: Logger;
  private readonly config: Config;
  private readonly exporterEnabled: boolean;
  private readonly tracerImpl: Tracer;
  private closed = false;

  constructor(config: Config, logger: Logger, exporterEnabled: boolean) {
    this.config = config;
    this.log = logger;
    this.exporterEnabled = exporterEnabled;
    this.tracerImpl = new TracerImpl(config, logger, exporterEnabled);
  }

  /** Reports whether spans leave the process. */
  enabled(): boolean {
    return this.exporterEnabled;
  }

  /** Returns the deployment's tracer. */
  tracer(): Tracer {
    return this.tracerImpl;
  }

  /** Flushes what is buffered and releases the exporter. Safe to call twice. */
  async shutdown(): Promise<void> {
    if (this.closed) return;
    this.closed = true;
    // The exporters implemented here write synchronously, so there is nothing
    // buffered. The timeout is kept so a caller that awaits shutdown has the
    // same contract as the Go side.
  }
}

/**
 * Installs the provider and sets the W3C propagator.
 *
 * A `console` exporter prints every finished span as one JSON line. `none`
 * (and an empty value) keeps the spans in the process: a trace id still exists,
 * and nothing leaves. `otlp` logs one warning and behaves like `none`, because
 * the OpenTelemetry SDK is not a dependency of this app.
 */
export function setup(config: Config, logger: Logger = nopLogger): Provider {
  const exporter = config.exporter.trim().toLowerCase();

  let enabled: boolean;
  switch (exporter) {
    case "":
    case EXPORTER_NONE:
      enabled = false;
      break;

    case EXPORTER_CONSOLE:
      enabled = true;
      break;

    case EXPORTER_OTLP:
      enabled = false;
      if (config.endpoint.trim() === "") {
        logger.warn("telemetry: the otlp exporter needs an endpoint; spans stay in the process");
      } else {
        logger.warn(
          "telemetry: the otlp exporter is not implemented in this build; " +
            `spans stay in the process instead of going to ${config.endpoint}`,
        );
      }
      break;

    default:
      // Go returns an error here. A misspelled exporter is a configuration
      // mistake worth refusing over, and the caller has a logger to say so.
      throw new Error(`telemetry: unknown exporter ${JSON.stringify(config.exporter)}`);
  }

  const provider = new Provider(config, logger, enabled);

  logger.info(
    {
      service: config.serviceName,
      exporter,
      exporting: enabled,
      sample_ratio: sampleRatio(config.sampleRatio),
    },
    "telemetry installed",
  );

  return provider;
}

/** Keeps the ratio inside its own domain, where zero means "no roots". */
export function sampleRatio(ratio: number): number {
  if (Number.isNaN(ratio) || ratio < 0) return 0;
  if (ratio > 1) return 1;
  return ratio;
}

// ------------------------------------------------------------------- tracing

class TracerImpl implements Tracer {
  constructor(
    private readonly config: Config,
    private readonly log: Logger,
    private readonly exporting: boolean,
  ) {}

  startSpan(name: string, options: StartSpanOptions = {}, parent?: SpanContext | null): Span {
    const parentContext = options.parent ?? parent ?? null;

    // Parent-based sampling: a trace a caller already decided to sample stays
    // sampled, which is what keeps a trace from having a hole exactly where the
    // work happened.
    let traceId: string;
    let spanId: string;
    let sampled: boolean;

    if (parentContext && parentContext.traceId !== "") {
      traceId = parentContext.traceId;
      spanId = randomHex(8);
      sampled = (parentContext.traceFlags & 1) === 1;
    } else {
      traceId = randomHex(16);
      spanId = randomHex(8);
      sampled = Math.random() < sampleRatio(this.config.sampleRatio);
    }

    const context: SpanContext = {
      traceId,
      spanId,
      traceFlags: sampled ? 1 : 0,
      isRemote: false,
    };

    const recording = this.exporting && sampled;
    return new SpanImpl(name, context, recording, this.config, this.log);
  }
}

class SpanImpl implements Span {
  private attributes: Record<string, AttributeValue> = {};
  private events: Array<{ name: string; attributes?: Record<string, AttributeValue>; at: string }> = [];
  private status: { code: SpanStatusCode; message?: string } = { code: SpanStatusCode.UNSET };
  private exception: string | undefined;
  private readonly startedAt = Date.now();
  private ended = false;

  constructor(
    private readonly name: string,
    private readonly context: SpanContext,
    private readonly recording: boolean,
    private readonly config: Config,
    private readonly log: Logger,
  ) {}

  spanContext(): SpanContext {
    return this.context;
  }

  setAttribute(key: string, value: AttributeValue): this {
    if (!this.recording || this.ended) return this;
    this.attributes[key] = value;
    return this;
  }

  setAttributes(attributes: Record<string, AttributeValue>): this {
    if (!this.recording || this.ended) return this;
    Object.assign(this.attributes, attributes);
    return this;
  }

  addEvent(name: string, attributes?: Record<string, AttributeValue>): this {
    if (!this.recording || this.ended) return this;
    this.events.push({ name, attributes, at: new Date().toISOString() });
    return this;
  }

  recordException(error: unknown): this {
    if (!this.recording || this.ended) return this;
    this.exception = error instanceof Error ? `${error.name}: ${error.message}` : String(error);
    return this;
  }

  setStatus(status: { code: SpanStatusCode; message?: string }): this {
    if (!this.recording || this.ended) return this;
    this.status = status;
    return this;
  }

  isRecording(): boolean {
    return this.recording && !this.ended;
  }

  /** Writes the span out, if it was recorded and the exporter is `console`. */
  end(): void {
    if (this.ended) return;
    this.ended = true;
    if (!this.recording) return;

    this.log.info(
      {
        name: this.name,
        trace_id: this.context.traceId,
        span_id: this.context.spanId,
        parent_span_id: null,
        service: this.config.serviceName,
        environment: this.config.environment,
        scope: SCOPE_NAME,
        start: new Date(this.startedAt).toISOString(),
        end: new Date().toISOString(),
        duration_ms: Date.now() - this.startedAt,
        attributes: this.attributes,
        events: this.events.length > 0 ? this.events : undefined,
        status: this.status.code,
        status_message: this.status.message,
        exception: this.exception,
      },
      "span",
    );
  }
}

function randomHex(bytes: number): string {
  return randomBytes(bytes).toString("hex");
}

// ---------------------------------------------------------------- propagation

/**
 * The W3C trace-context header, in the shape the Go propagator emits:
 * `00-<trace-id>-<span-id>-<flags>`.
 */
export function injectTraceparent(context: SpanContext): string {
  const flags = (context.traceFlags & 1).toString(16).padStart(2, "0");
  return `00-${context.traceId}-${context.spanId}-${flags}`;
}

/**
 * Reads a `traceparent` header. Returns null for a missing or malformed value,
 * which is what the Go propagator does rather than inventing a context.
 */
export function extractTraceparent(header: string | null | undefined): SpanContext | null {
  if (!header) return null;
  const match = /^([0-9a-f]{2})-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})$/i.exec(header.trim());
  if (!match) return null;

  const [, version, traceId, spanId, flags] = match;
  if (version === "ff") return null;
  if (/^0+$/.test(traceId) || /^0+$/.test(spanId)) return null;

  return {
    traceId: traceId.toLowerCase(),
    spanId: spanId.toLowerCase(),
    traceFlags: Number.parseInt(flags, 16) & 1,
    isRemote: true,
  };
}

/** A tracer that records nothing but still mints valid 32-hex trace ids. */
export function noopTracer(): Tracer {
  return new TracerImpl(
    {
      serviceName: "",
      environment: "",
      exporter: EXPORTER_NONE,
      endpoint: "",
      sampleRatio: 0,
      insecure: false,
    },
    nopLogger,
    false,
  );
}
