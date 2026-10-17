/**
 * The scrape endpoint: Prometheus text exposition, version 0.0.4.
 *
 * Ported from packages/platform/metrics/scrape.go.
 *
 * It is written here rather than taken from an exporter, and the reason is what
 * that exporter costs: `prometheus/client_golang` needs a C toolchain on some
 * platforms. What the exporter does for us is turn one snapshot into text, which
 * is the whole of what is below.
 *
 * Two conventions matter and both are followed: a monotonic sum is a counter and
 * its name takes the `_total` suffix, and a histogram is three families —
 * cumulative buckets with an `le` label, a sum, and a count.
 */

import type { AttributeValue } from "../telemetry/telemetry";
import type { HistogramDataPoint, Metric, Reader, Snapshot } from "./metrics";

/** The content type the format requires. */
export const CONTENT_TYPE = "text/plain; version=0.0.4; charset=utf-8";

/** The rendered body and the headers a scrape should carry. */
export interface RenderedScrape {
  body: string;
  contentType: string;
  cacheControl: string;
}

/**
 * Renders one snapshot of a reader's measurements as the exposition format.
 *
 * It holds no state between scrapes: every request reads the reader, so what a
 * scraper sees is what the process holds at that moment.
 */
export class Scraper {
  constructor(private readonly reader: Reader) {}

  /** Renders the current measurements for a scraper. */
  render(): RenderedScrape {
    return {
      body: renderSnapshot(this.reader.collect()),
      contentType: CONTENT_TYPE,
      cacheControl: "no-store",
    };
  }
}

/** Builds the endpoint over a reader. */
export function newScraper(reader: Reader): Scraper {
  return new Scraper(reader);
}

/**
 * Renders a snapshot. Exported so a caller with its own reader (a test, a
 * one-off dump) renders through the same code path a scrape does.
 */
export function renderSnapshot(snapshot: Snapshot): string {
  const out: string[] = [];
  for (const metric of snapshot.metrics) {
    writeMetric(out, metric);
  }
  return out.join("");
}

/** Renders one metric family. */
function writeMetric(out: string[], metric: Metric): void {
  // The SDK omits an instrument that recorded nothing: a family with no series
  // is not a family a scraper should see, and emitting HELP/TYPE alone would
  // claim the deployment records something it never has.
  if (metric.data.dataPoints.length === 0) return;

  const name = prometheusName(metric.name, metric.unit);

  switch (metric.data.kind) {
    case "sum": {
      // A monotonic sum is a counter, and Prometheus's convention is the `_total`
      // suffix. A non-monotonic one is a gauge, which is what an up-down counter
      // renders as.
      const rendered = metric.data.monotonic ? `${name}_total` : name;
      writeHeader(out, rendered, metric.description, metric.data.monotonic ? "counter" : "gauge");
      for (const point of metric.data.dataPoints) {
        writePoint(out, rendered, labelsOf(point.attributes), formatFloat(point.value));
      }
      return;
    }

    case "gauge":
      writeHeader(out, name, metric.description, "gauge");
      for (const point of metric.data.dataPoints) {
        writePoint(out, name, labelsOf(point.attributes), formatFloat(point.value));
      }
      return;

    case "histogram":
      writeHistogram(out, name, metric.description, metric.data.dataPoints);
      return;
  }
}

/** Renders a histogram as its three families. */
function writeHistogram(
  out: string[],
  name: string,
  description: string,
  points: HistogramDataPoint[],
): void {
  writeHeader(out, name, description, "histogram");

  for (const point of points) {
    const labels = labelsOf(point.attributes);

    // The buckets are cumulative and every one of them is rendered, so a scrape
    // can compute a rate over any of them. The counts the registry keeps are
    // already cumulative; the `le` label is the bound itself.
    for (let index = 0; index < point.bounds.length; index++) {
      writePoint(
        out,
        `${name}_bucket`,
        [...labels, { key: "le", value: formatFloat(point.bounds[index]) }],
        formatUint(point.bucketCounts[index] ?? 0),
      );
    }
    writePoint(
      out,
      `${name}_bucket`,
      [...labels, { key: "le", value: "+Inf" }],
      formatUint(point.count),
    );

    writePoint(out, `${name}_sum`, labels, formatFloat(point.sum));
    writePoint(out, `${name}_count`, labels, formatUint(point.count));
  }
}

/**
 * Turns an instrument name into a Prometheus metric name.
 *
 * Two rules, and both are the format's rather than a choice: a metric name may
 * only contain letters, digits, underscores, and colons (so the dots this
 * package names its instruments with become underscores), and the unit belongs
 * in the name — `bolu_task_duration` in seconds is
 * `bolu_task_duration_seconds`.
 */
export function prometheusName(name: string, unit: string): string {
  let sanitized = sanitizeMetricName(name);

  const suffix = prometheusUnitSuffix(unit);
  if (suffix !== "" && !sanitized.endsWith(suffix)) sanitized += suffix;

  return sanitized;
}

/**
 * The Prometheus spelling of a unit, for the units this deployment declares.
 *
 * A unit this function does not know leaves the name alone, which is the safe
 * direction: an invented suffix is a series no dashboard matches.
 */
export function prometheusUnitSuffix(unit: string): string {
  switch (unit) {
    case "s":
      return "_seconds";
    case "By":
      return "_bytes";
    default:
      // An annotated unit (`{task}`), an empty one, and a count the format has
      // no suffix for all mean the same thing here: nothing to add.
      return "";
  }
}

/** Replaces every character the format does not allow, and fixes a leading digit. */
export function sanitizeMetricName(name: string): string {
  const cleaned = Array.from(name)
    .map((char) =>
      /[a-zA-Z0-9_:]/.test(char) ? char : "_",
    )
    .join("");

  if (cleaned === "") return "_";
  if (cleaned[0] >= "0" && cleaned[0] <= "9") return `_${cleaned}`;
  return cleaned;
}

/** One rendered label, kept as a pair so the order is explicit. */
export interface Label {
  key: string;
  value: string;
}

/**
 * Renders a data point's attributes, sorted by key.
 *
 * The sort is not tidiness: the output is compared between scrapes by whatever
 * reads it, and a map iteration order would make two identical snapshots differ.
 */
export function labelsOf(attributes: Record<string, AttributeValue>): Label[] {
  const out: Label[] = [];
  for (const [key, value] of Object.entries(attributes)) {
    // The key is sanitized for the same reason the metric name is: a label name
    // has the same allowed characters.
    out.push({ key: sanitizeMetricName(key), value: attributeValueString(value) });
  }
  out.sort((a, b) => (a.key < b.key ? -1 : a.key > b.key ? 1 : 0));
  return out;
}

/**
 * Formats an attribute value the way OpenTelemetry's `Value.String()` does:
 * booleans as `true`/`false`, numbers in their shortest form, and string slices
 * in Go's `[a b c]` shape.
 */
function attributeValueString(value: AttributeValue): string {
  if (Array.isArray(value)) return `[${value.map((item) => String(item)).join(" ")}]`;
  return String(value);
}

/** Writes the two comment lines a metric family is introduced with. */
function writeHeader(out: string[], name: string, description: string, kind: string): void {
  if (description !== "") out.push(`# HELP ${name} ${escapeHelp(description)}\n`);
  out.push(`# TYPE ${name} ${kind}\n`);
}

/** Writes one series. */
function writePoint(out: string[], name: string, labels: Label[], value: string): void {
  if (labels.length === 0) {
    out.push(`${name} ${value}\n`);
    return;
  }

  const rendered = labels.map((label) => `${label.key}="${escapeLabel(label.value)}"`).join(",");
  out.push(`${name}{${rendered}} ${value}\n`);
}

/**
 * Escapes a label value, where a backslash, a quote, and a newline are the three
 * characters the format gives meaning to.
 */
export function escapeLabel(value: string): string {
  return value.replace(/\\/g, "\\\\").replace(/"/g, '\\"').replace(/\n/g, "\\n");
}

/** Escapes a help line, where only a backslash and a newline are special. */
export function escapeHelp(value: string): string {
  return value.replace(/\\/g, "\\\\").replace(/\n/g, "\\n");
}

/** Renders an unsigned value. */
function formatUint(value: number): string {
  return String(Math.trunc(value));
}

/**
 * Renders a float the way the format expects, including the three values that
 * are not numbers: Prometheus writes them by name rather than as a decimal.
 */
export function formatFloat(value: number): string {
  if (Number.isNaN(value)) return "NaN";
  if (value === Number.POSITIVE_INFINITY) return "+Inf";
  if (value === Number.NEGATIVE_INFINITY) return "-Inf";
  return String(value);
}
