/**
 * The deployment's meter: the instruments the product records into, and the
 * exporters that carry them to a scraper or a collector.
 *
 * Ported from packages/platform/metrics/metrics.go.
 *
 * Two decisions shape everything here, and both are about cardinality — the
 * only thing that makes metrics expensive:
 *
 * - **No identifier is ever a label.** No workspace, no Bolu, no task, no
 *   workflow. What is recorded is the *shape* of the work — which trigger,
 *   which outcome, which reason class.
 * - **The reason is classified, not copied.** A reason the recorder does not
 *   recognise becomes `other` rather than a new time series.
 *
 * The Go side builds on `go.opentelemetry.io/otel/sdk/metric`. That package is
 * not a dependency here, so this module carries the small subset the port needs:
 * counters, up-down counters, histograms, and the same metric names, units, and
 * descriptions. The `prometheus` exporter is implemented in `scrape.ts`; the
 * `console` exporter prints a snapshot on an interval; `otlp` warns once at
 * startup and records nothing (documented gap — the SDK is not installed).
 */

import type { AttributeValue } from "../telemetry/telemetry";
import type { Logger } from "../logger";
import { nopLogger } from "../logger";
import type { Scraper } from "./scrape";
import { newScraper } from "./scrape";

/** Exporters a deployment may name. */
export const EXPORTER_NONE = "none";
/** Serves measurements to a scraper over HTTP. */
export const EXPORTER_PROMETHEUS = "prometheus";
/** Pushes them to a collector. Not implemented in this build. */
export const EXPORTER_OTLP = "otlp";
/** Prints them, which is what a development run wants. */
export const EXPORTER_CONSOLE = "console";

/** The instrumentation scope the instruments are filed under. */
export const SCOPE_NAME = "github.com/anakbolu/timbolu";

/** The SDK's default export interval, in milliseconds. */
const DEFAULT_INTERVAL_MS = 60_000;

/** The SDK's default explicit histogram bounds. */
const DEFAULT_HISTOGRAM_BOUNDS = [
  0, 5, 10, 25, 50, 75, 100, 250, 500, 750, 1000, 2500, 5000, 7500, 10000,
];

/** How a deployment reports metrics. */
export interface Config {
  /** Files the measurements under one service. */
  serviceName: string;
  /** The deployment, e.g. "local" or "production". */
  environment: string;
  /** `none`, `prometheus`, `otlp`, or `console`. */
  exporter: string;
  /** The collector's host:port, required by `otlp`. */
  endpoint: string;
  /** How often measurements are exported, in milliseconds. Zero takes the default. */
  interval: number;
  /** Drops TLS to the collector. */
  insecure: boolean;
}

/** One rendered data point. */
export interface DataPoint {
  attributes: Record<string, AttributeValue>;
  value: number;
}

/** One histogram data point. */
export interface HistogramDataPoint {
  attributes: Record<string, AttributeValue>;
  count: number;
  sum: number;
  /** Upper bounds, ascending. */
  bounds: number[];
  /** Cumulative counts per bound, then the overflow bucket. */
  bucketCounts: number[];
}

/** A metric family's measurements. */
export type MetricData =
  | { kind: "sum"; monotonic: boolean; dataPoints: DataPoint[] }
  | { kind: "gauge"; dataPoints: DataPoint[] }
  | { kind: "histogram"; dataPoints: HistogramDataPoint[] };

/** One metric family. */
export interface Metric {
  name: string;
  unit: string;
  description: string;
  data: MetricData;
}

/** A snapshot of everything a process holds. */
export interface Snapshot {
  serviceName: string;
  environment: string;
  collectedAt: Date;
  metrics: Metric[];
}

/** A reader produces a snapshot on demand. */
export interface Reader {
  collect(): Snapshot;
}

/**
 * The installed meter provider and the instruments built from it.
 *
 * `handler` is the Prometheus scrape handler, present only when that exporter
 * is configured. The container mounts it; a process without an HTTP surface
 * leaves it alone.
 */
export class Provider {
  /** The Prometheus scrape handler, when that exporter is configured. */
  readonly handler: Scraper | null;
  /** The product's own measures. */
  readonly instruments: Instruments;
  private readonly enabledFlag: boolean;
  private readonly log: Logger;
  private readonly reader: Reader | null;
  private closed = false;
  /** The console exporter's push interval, when that exporter is configured. */
  private timer: Timer | undefined;

  constructor(
    enabled: boolean,
    reader: Reader | null,
    handler: Scraper | null,
    instruments: Instruments,
    log: Logger,
  ) {
    this.enabledFlag = enabled;
    this.reader = reader;
    this.handler = handler;
    this.instruments = instruments;
    this.log = log;
  }

  /** Starts pushing snapshots to the logger at the configured interval. */
  startConsolePush(intervalMs: number, log: Logger): void {
    this.timer = setInterval(() => {
      if (this.reader) log.info(this.reader.collect(), "metrics");
    }, intervalMs);
    // A metrics interval must not be what keeps a process alive on shutdown.
    this.timer.unref?.();
  }

  /** Reports whether measurements leave the process. */
  enabled(): boolean {
    return this.enabledFlag;
  }

  /** Flushes what is buffered and releases the reader. Safe to call twice. */
  async shutdown(): Promise<void> {
    if (this.closed) return;
    this.closed = true;
    clearInterval(this.timer);
    this.timer = undefined;
  }
}

/**
 * Installs the meter provider the deployment configured and builds the
 * instruments. It is the only entry point a binary uses.
 */
export function setup(config: Config, logger: Logger = nopLogger): Provider {
  const exporter = config.exporter.trim().toLowerCase();

  switch (exporter) {
    case "":
    case EXPORTER_NONE: {
      const instruments = new Instruments(new Registry(config));
      logger.info(
        { service: config.serviceName, exporter, exporting: false },
        "metrics installed",
      );
      return new Provider(false, null, null, instruments, logger);
    }

    case EXPORTER_CONSOLE: {
      const registry = new Registry(config);
      const instruments = new Instruments(registry);
      const interval = config.interval > 0 ? config.interval : DEFAULT_INTERVAL_MS;
      const provider = new Provider(true, registry, null, instruments, logger);
      provider.startConsolePush(interval, logger);
      logger.info(
        { service: config.serviceName, exporter, exporting: true },
        "metrics installed",
      );
      return provider;
    }

    case EXPORTER_PROMETHEUS: {
      const registry = new Registry(config);
      const instruments = new Instruments(registry);
      const scraper = newScraper(registry);
      logger.info(
        { service: config.serviceName, exporter, exporting: true },
        "metrics installed",
      );
      return new Provider(true, registry, scraper, instruments, logger);
    }

    case EXPORTER_OTLP: {
      const instruments = new Instruments(new Registry(config));
      if (config.endpoint.trim() === "") {
        logger.warn("metrics: the otlp exporter needs an endpoint; measurements stay in the process");
      } else {
        logger.warn(
          "metrics: the otlp exporter is not implemented in this build; " +
            `measurements stay in the process instead of going to ${config.endpoint}`,
        );
      }
      return new Provider(false, null, null, instruments, logger);
    }

    default:
      throw new Error(`metrics: unknown exporter ${JSON.stringify(config.exporter)}`);
  }
}

// ------------------------------------------------------------------ registry

type InstrumentKind = "counter" | "updown" | "histogram";

interface InstrumentDef {
  name: string;
  unit: string;
  description: string;
  kind: InstrumentKind;
  bounds?: number[];
}

interface Series {
  attributes: Record<string, AttributeValue>;
  value: number;
  count?: number;
  sum?: number;
  bucketCounts?: number[];
}

/** The in-process store every instrument records into. */
export class Registry implements Reader {
  private readonly metrics = new Map<string, { def: InstrumentDef; series: Map<string, Series> }>();

  constructor(private readonly config: Config) {}

  /** Declares an instrument. */
  register(def: InstrumentDef): void {
    if (!this.metrics.has(def.name)) {
      this.metrics.set(def.name, { def, series: new Map() });
    }
  }

  /** Adds to a counter or an up-down counter. */
  add(name: string, value: number, attributes: Record<string, AttributeValue> = {}): void {
    const entry = this.metrics.get(name);
    if (!entry) return;
    const key = seriesKey(attributes);
    const existing = entry.series.get(key);
    if (existing) {
      existing.value += value;
      return;
    }
    entry.series.set(key, { attributes: { ...attributes }, value });
  }

  /** Records one observation into a histogram. */
  record(name: string, value: number, attributes: Record<string, AttributeValue> = {}): void {
    const entry = this.metrics.get(name);
    if (!entry) return;
    const bounds = entry.def.bounds ?? DEFAULT_HISTOGRAM_BOUNDS;
    const key = seriesKey(attributes);
    let point = entry.series.get(key);
    if (!point) {
      point = {
        attributes: { ...attributes },
        value: 0,
        count: 0,
        sum: 0,
        bucketCounts: new Array(bounds.length + 1).fill(0),
      };
      entry.series.set(key, point);
    }

    point.count = (point.count ?? 0) + 1;
    point.sum = (point.sum ?? 0) + value;

    const counts = point.bucketCounts ?? [];
    let placed = false;
    for (let index = 0; index < bounds.length; index++) {
      if (value <= bounds[index]) {
        counts[index] = (counts[index] ?? 0) + 1;
        placed = true;
        break;
      }
    }
    if (!placed) counts[bounds.length] = (counts[bounds.length] ?? 0) + 1;
  }

  /** Produces the current snapshot. */
  collect(): Snapshot {
    const metrics: Metric[] = [];

    for (const { def, series } of this.metrics.values()) {
      if (def.kind === "histogram") {
        const bounds = def.bounds ?? DEFAULT_HISTOGRAM_BOUNDS;
        metrics.push({
          name: def.name,
          unit: def.unit,
          description: def.description,
          data: {
            kind: "histogram",
            dataPoints: Array.from(series.values()).map((point) => {
              const raw = point.bucketCounts ?? [];
              // The format's buckets are cumulative; the store keeps each
              // observation in exactly one bucket.
              let running = 0;
              const bucketCounts = bounds.map((_, index) => {
                running += raw[index] ?? 0;
                return running;
              });
              running += raw[bounds.length] ?? 0;
              bucketCounts.push(running);
              return {
                attributes: point.attributes,
                count: point.count ?? 0,
                sum: point.sum ?? 0,
                bounds,
                bucketCounts,
              };
            }),
          },
        });
        continue;
      }

      const dataPoints = Array.from(series.values()).map((point) => ({
        attributes: point.attributes,
        value: point.value,
      }));

      metrics.push({
        name: def.name,
        unit: def.unit,
        description: def.description,
        data:
          def.kind === "counter"
            ? { kind: "sum", monotonic: true, dataPoints }
            : { kind: "sum", monotonic: false, dataPoints },
      });
    }

    return {
      serviceName: this.config.serviceName,
      environment: this.config.environment,
      collectedAt: new Date(),
      metrics,
    };
  }
}

/** A stable key for one attribute set, independent of insertion order. */
function seriesKey(attributes: Record<string, AttributeValue>): string {
  const keys = Object.keys(attributes).sort();
  return keys.map((key) => `${key}=${JSON.stringify(attributes[key])}`).join(",");
}

// ---------------------------------------------------------------- instruments

/**
 * The product's measures, declared in one place.
 *
 * Each is a counter, a histogram, or an up-down counter, and the unit says what
 * it counts. None of them takes an identifier as an attribute — see the module
 * comment for why that is the rule rather than a preference.
 */
export class Instruments {
  constructor(private readonly registry: Registry) {
    this.registry.register({
      name: "bolu.draft.ready.duration",
      unit: "s",
      description: "how long a draft took from its task opening to being ready for a person",
      kind: "histogram",
    });
    this.registry.register({
      name: "bolu.draft.decisions",
      unit: "{decision}",
      description: "what a person did with a draft, by outcome",
      kind: "counter",
    });
    this.registry.register({
      name: "bolu.task.finished",
      unit: "{task}",
      description: "tasks that finished, by trigger and outcome",
      kind: "counter",
    });
    this.registry.register({
      name: "bolu.task.tokens",
      unit: "{token}",
      description: "tokens a task spent, by direction",
      kind: "counter",
    });
    // No unit is declared, and the description carries it instead: the
    // Prometheus exporter appends a declared unit to the metric name, so a unit
    // of "uRp" would serve the series as `bolu_task_cost_uRp_total`.
    this.registry.register({
      name: "bolu.task.cost",
      unit: "",
      description: "what a task cost, in micro-rupiah",
      kind: "counter",
    });
    this.registry.register({
      name: "bolu.task.duration",
      unit: "s",
      description: "how long a task ran",
      kind: "histogram",
    });
    this.registry.register({
      name: "bolu.tool.calls",
      unit: "{call}",
      description: "tool calls, by tool and outcome",
      kind: "counter",
    });
    this.registry.register({
      name: "bolu.tool.duration",
      unit: "s",
      description: "how long a tool call took, by integration",
      kind: "histogram",
    });
    this.registry.register({
      name: "bolu.tool.failures",
      unit: "{failure}",
      description: "tool calls that failed, by tool",
      kind: "counter",
    });
    this.registry.register({
      name: "bolu.model.calls",
      unit: "{call}",
      description: "model calls, by provider and purpose",
      kind: "counter",
    });
    this.registry.register({
      name: "bolu.model.fallbacks",
      unit: "{call}",
      description: "model calls that had to try another provider",
      kind: "counter",
    });
    this.registry.register({
      name: "bolu.model.duration",
      unit: "s",
      description: "how long a provider took, by provider",
      kind: "histogram",
    });
    this.registry.register({
      name: "bolu.ratelimit.refused",
      unit: "{request}",
      description: "requests refused by a rate limit, by surface",
      kind: "counter",
    });
    this.registry.register({
      name: "bolu.queue.depth",
      unit: "{item}",
      description: "work waiting on a queue",
      kind: "updown",
    });
    this.registry.register({
      name: "bolu.activity.retries",
      unit: "{attempt}",
      description: "activity attempts past the first, by activity",
      kind: "counter",
    });
    this.registry.register({
      name: "bolu.task.dispatched",
      unit: "{task}",
      description: "tasks this deployment accepted; the depth is dispatched minus settled",
      kind: "counter",
    });
    this.registry.register({
      name: "bolu.task.settled",
      unit: "{task}",
      description: "tasks that reached a terminal state; the depth is dispatched minus settled",
      kind: "counter",
    });
    this.registry.register({
      name: "bolu.retention.pruned",
      unit: "{item}",
      description: "what the retention pass removed, by kind",
      kind: "counter",
    });
  }

  /** Records how long a draft took to be ready for a person. */
  draftReady(seconds: number): void {
    this.registry.record("bolu.draft.ready.duration", seconds);
  }

  /** Counts what a person did with a draft. */
  draftDecision(outcome: string, revision: boolean): void {
    this.registry.add("bolu.draft.decisions", 1, { outcome, revised: revision });
  }

  /** Records a finished task and what it spent. */
  taskFinished(
    trigger: string,
    status: string,
    seconds: number,
    inputTokens: number,
    outputTokens: number,
    costMicros: number,
  ): void {
    const attributes = { trigger, outcome: status };
    this.registry.add("bolu.task.finished", 1, attributes);
    this.registry.add("bolu.task.tokens", inputTokens, { direction: "input" });
    this.registry.add("bolu.task.tokens", outputTokens, { direction: "output" });
    this.registry.add("bolu.task.cost", costMicros);
    this.registry.record("bolu.task.duration", seconds, attributes);
  }

  /** Records one tool call and whether it failed. */
  toolCall(app: string, failed: boolean, seconds: number): void {
    const attributes = { app };
    this.registry.add("bolu.tool.calls", 1, attributes);
    if (failed) this.registry.add("bolu.tool.failures", 1, attributes);
    this.registry.record("bolu.tool.duration", seconds, attributes);
  }

  /** Records one provider call. */
  modelCall(provider: string, purpose: string, fellBack: boolean, seconds: number): void {
    const attributes = { provider, purpose };
    this.registry.add("bolu.model.calls", 1, attributes);
    if (fellBack) this.registry.add("bolu.model.fallbacks", 1, attributes);
    this.registry.record("bolu.model.duration", seconds, { provider });
  }

  /** Records a request a limit refused. */
  rateLimited(surface: string): void {
    this.registry.add("bolu.ratelimit.refused", 1, { surface });
  }

  /** Records work this deployment accepted. */
  taskDispatched(): void {
    this.registry.add("bolu.task.dispatched", 1);
  }

  /** Records work that reached a terminal state. */
  taskSettled(): void {
    this.registry.add("bolu.task.settled", 1);
  }

  /** Records an activity attempt past the first. */
  activityRetry(activity: string, attempt: string): void {
    this.registry.add("bolu.activity.retries", 1, { activity, attempt });
  }

  /** Records what a retention pass removed. */
  retentionPruned(kind: string, count: number): void {
    if (count === 0) return;
    this.registry.add("bolu.retention.pruned", count, { kind });
  }

  /** Records a change in how much work is waiting on a queue. */
  queueDepth(delta: number, queue: string): void {
    this.registry.add("bolu.queue.depth", delta, { queue });
  }
}
