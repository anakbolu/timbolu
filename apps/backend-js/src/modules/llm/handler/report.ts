/**
 * The usage screen's read, and its export.
 *
 * Ported from internal/modules/llm/handler/report.go. Two endpoints rather than
 * one, because the two consumers want different things: a screen wants numbers
 * it can format, and an accountant wants a file with a header and rows. Serving
 * the CSV from the same read as the JSON is what keeps the two from disagreeing.
 */

import type { AppContext } from "../../../platform/context.ts";
import { error, ok } from "../../../platform/response.ts";
import type { UsageReport } from "../domain/provider.ts";
import { queryInt, scope, type Handler } from "./handler.ts";

/** The report as the screen reads it. */
interface UsageReportPayload {
  days: number;
  /**
   * The first moment the window covers, in Asia/Jakarta. It is stated rather
   * than left to the client to recompute.
   */
  period_started_at: string;
  totals: UsageTotalsPayload;
  by_agent: UsageAgentPayload[];
  by_task: UsageTaskPayload[];
}

interface UsageTotalsPayload {
  calls: number;
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  cost_micros: number;
  cost_idr: number;
}

interface UsageAgentPayload {
  agent_id: string;
  agent_name: string;
  calls: number;
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
  cost_micros: number;
  cost_idr: number;
}

interface UsageTaskPayload {
  task_id: string;
  task_title: string;
  task_status: string;
  calls: number;
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
  cost_micros: number;
  cost_idr: number;
}

/** The period's usage, read three ways, or the same thing as a CSV. */
export async function report(h: Handler, c: AppContext): Promise<Response> {
  const days = queryInt(c, "days", 0);
  const limit = queryInt(c, "limit", 0);

  const reporter = h.reporter;
  if (reporter === null) {
    return error(
      c,
      503,
      "laporan pemakaian tidak tersedia di deployment ini",
      "usage_report_unavailable",
    );
  }

  let usage: UsageReport;
  try {
    usage = await reporter.report(undefined, scope(c), days, limit);
  } catch (err) {
    return h.fail(c, "read usage report", err);
  }

  if (c.req.query("format") === "csv") {
    return reportCSV(h, c, usage, days);
  }

  return ok(c, "ok", reportPayload(usage, days));
}

/** Renders the report for a screen. */
export function reportPayload(report: UsageReport, days: number): UsageReportPayload {
  return {
    days,
    period_started_at: rfc3339(report.periodStartedAt),
    totals: {
      calls: report.totals.calls,
      input_tokens: report.totals.inputTokens,
      output_tokens: report.totals.outputTokens,
      total_tokens: report.totals.inputTokens + report.totals.outputTokens,
      cache_read_tokens: report.totals.cacheReadTokens,
      cache_write_tokens: report.totals.cacheWriteTokens,
      cost_micros: report.totals.costMicros,
      cost_idr: idr(report.totals.costMicros),
    },
    by_agent: report.byAgent.map((agent) => ({
      agent_id: agent.agentId,
      agent_name: agent.agentName,
      calls: agent.calls,
      input_tokens: agent.inputTokens,
      output_tokens: agent.outputTokens,
      total_tokens: agent.inputTokens + agent.outputTokens,
      cost_micros: agent.costMicros,
      cost_idr: idr(agent.costMicros),
    })),
    by_task: report.byTask.map((task) => ({
      task_id: task.taskId,
      task_title: task.taskTitle,
      task_status: task.taskStatus,
      calls: task.calls,
      input_tokens: task.inputTokens,
      output_tokens: task.outputTokens,
      total_tokens: task.inputTokens + task.outputTokens,
      cost_micros: task.costMicros,
      cost_idr: idr(task.costMicros),
    })),
  };
}

/**
 * The report as a spreadsheet, for internal accounting.
 *
 * One rectangular table rather than three sections: every row carries the same
 * columns and the `bagian` (section) column says which kind of row it is. A CSV
 * with two-field preamble rows and nine-field data rows is a file a strict
 * reader refuses.
 *
 * The body is built as a `ReadableStream` so a large report does not buffer
 * entirely before the first byte.
 */
export function reportCSV(
  h: Handler,
  c: AppContext,
  usage: UsageReport,
  days: number,
): Response {
  const period = rfc3339(usage.periodStartedAt);
  const daysText = String(days);
  const today = new Date().toISOString().slice(0, 10);

  const rows = csvRows(usage, period, daysText);

  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      const encoder = new TextEncoder();
      for (const row of rows) {
        controller.enqueue(encoder.encode(encodeRow(row)));
      }
      controller.close();
    },
  });

  c.header("Content-Type", "text/csv; charset=utf-8");
  c.header("Content-Disposition", `attachment; filename="pemakaian-${today}.csv"`);

  return c.body(stream);
}

/** The header and every row, in the order the questions are asked in. */
function csvRows(usage: UsageReport, period: string, days: string): string[][] {
  const rows: string[][] = [
    [
      "bagian",
      "id",
      "nama",
      "status",
      "periode_mulai",
      "hari",
      "panggilan",
      "token_masuk",
      "token_keluar",
      "total_token",
      "cache_baca",
      "cache_tulis",
      "biaya_mikro_rupiah",
      "biaya_rupiah",
    ],
  ];

  rows.push([
    "total",
    "",
    "",
    "",
    period,
    days,
    String(usage.totals.calls),
    String(usage.totals.inputTokens),
    String(usage.totals.outputTokens),
    String(usage.totals.inputTokens + usage.totals.outputTokens),
    String(usage.totals.cacheReadTokens),
    String(usage.totals.cacheWriteTokens),
    String(usage.totals.costMicros),
    money(usage.totals.costMicros),
  ]);

  for (const agent of usage.byAgent) {
    rows.push([
      "per_bolu",
      agent.agentId,
      agent.agentName,
      "",
      period,
      days,
      String(agent.calls),
      String(agent.inputTokens),
      String(agent.outputTokens),
      String(agent.inputTokens + agent.outputTokens),
      "",
      "",
      String(agent.costMicros),
      money(agent.costMicros),
    ]);
  }

  for (const task of usage.byTask) {
    rows.push([
      "per_tugas",
      task.taskId,
      task.taskTitle,
      task.taskStatus,
      period,
      days,
      String(task.calls),
      String(task.inputTokens),
      String(task.outputTokens),
      String(task.inputTokens + task.outputTokens),
      "",
      "",
      String(task.costMicros),
      money(task.costMicros),
    ]);
  }

  return rows;
}

/** Quotes one CSV record the way encoding/csv does: only when a field needs it. */
function encodeRow(row: string[]): string {
  return row.map(encodeField).join(",") + "\n";
}

function encodeField(value: string): string {
  // encoding/csv quotes a field that holds a comma, a quote, a line break, or
  // that starts with whitespace.
  if (value === "" || (!/[",\r\n]/.test(value) && !/^\s/.test(value))) return value;
  return `"${value.replace(/"/g, '""')}"`;
}

/** Renders an instant the way Go's `time.RFC3339` does: no fractional seconds. */
export function rfc3339(at: Date): string {
  return at.toISOString().replace(/\.\d{3}Z$/, "Z");
}

/** Turns micro-rupiah into rupiah, which is what a person reads. */
export function idr(micros: number): number {
  return micros / 1_000_000;
}

/**
 * Renders micro-rupiah as rupiah with two decimals.
 *
 * Written by hand rather than with a formatter: the export is read by a
 * spreadsheet, and a formatter would follow the process's locale — a comma
 * decimal separator in some deployments would make the column text in the
 * two-column countries it imported into.
 */
export function money(micros: number): string {
  const whole = Math.trunc(micros / 1_000_000);
  let cents = Math.trunc((micros % 1_000_000) / 10_000);
  if (cents < 0) cents = -cents;

  if (whole < 0 || micros < 0) {
    return "-" + String(-whole) + "." + pad2(cents);
  }

  return String(whole) + "." + pad2(cents);
}

/** Renders a two-digit fraction part. */
function pad2(value: number): string {
  return value < 10 ? "0" + String(value) : String(value);
}
