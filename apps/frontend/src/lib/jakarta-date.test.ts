/**
 * Run it with Bun:
 *   cd apps/frontend && bun test src/lib/jakarta-date.test.ts
 */

import assert from "node:assert/strict";
import test from "node:test";

import { ASIA_JAKARTA, formatBusinessDay, formatComplianceTimestamp } from "./jakarta-date";

test("ASIA_JAKARTA constant is Asia/Jakarta", () => {
  assert.equal(ASIA_JAKARTA, "Asia/Jakarta");
});

test("formatBusinessDay formats UTC midnight to Indonesian business date in Asia/Jakarta regardless of process TZ", () => {
  const originalTz = process.env.TZ;
  try {
    process.env.TZ = "America/Los_Angeles";
    // 2026-12-01T00:00:00Z is 2026-11-30 16:00:00 in LA, but 2026-12-01 07:00:00 in Jakarta
    const result = formatBusinessDay("2026-12-01T00:00:00Z");
    assert.equal(result, "1 Desember 2026");
  } finally {
    if (originalTz === undefined) {
      delete process.env.TZ;
    } else {
      process.env.TZ = originalTz;
    }
  }
});

test("formatBusinessDay rolls 17:00 UTC over to the next day in Jakarta (UTC+7)", () => {
  const originalTz = process.env.TZ;
  try {
    process.env.TZ = "America/Los_Angeles";
    // 2026-11-30T17:00:00Z is 2026-12-01 00:00:00 WIB
    const result = formatBusinessDay("2026-11-30T17:00:00Z");
    assert.equal(result, "1 Desember 2026");
  } finally {
    if (originalTz === undefined) {
      delete process.env.TZ;
    } else {
      process.env.TZ = originalTz;
    }
  }
});

test("formatBusinessDay returns dash for empty or invalid strings", () => {
  assert.equal(formatBusinessDay(""), "—");
  assert.equal(formatBusinessDay(null), "—");
  assert.equal(formatBusinessDay(undefined), "—");
  assert.equal(formatBusinessDay("not-a-date"), "—");
});

test("formatComplianceTimestamp formats date with time and WIB zone indicator in Asia/Jakarta", () => {
  const originalTz = process.env.TZ;
  try {
    process.env.TZ = "America/Los_Angeles";
    // 2026-11-30T17:05:09Z -> 2026-12-01 00:05:09 WIB
    const result = formatComplianceTimestamp("2026-11-30T17:05:09Z");
    assert.match(result, /1 Desember 2026/);
    assert.match(result, /00[.:]05[.:]09/);
    assert.match(result, /WIB$/);
  } finally {
    if (originalTz === undefined) {
      delete process.env.TZ;
    } else {
      process.env.TZ = originalTz;
    }
  }
});

test("formatComplianceTimestamp returns dash for empty or invalid values", () => {
  assert.equal(formatComplianceTimestamp(""), "—");
  assert.equal(formatComplianceTimestamp(null), "—");
  assert.equal(formatComplianceTimestamp(undefined), "—");
  assert.equal(formatComplianceTimestamp("not-a-date"), "—");
});
