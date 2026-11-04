/**
 * Unit tests for shared rupiah and token formatters (issue #638).
 *
 * Run with:
 *   cd apps/frontend && bun test src/lib/format.test.ts
 */

// `bun:test` ships no types for the Next build's `tsc` run; the module only exists under `bun test`.
// @ts-expect-error -- bun:test is typed by bun's test runner, not by frontend's tsconfig
import { expect, test } from "bun:test";

import { formatRupiah, formatRupiahCost, formatTokens } from "./format";

/**
 * ICU data differs across runtimes on the spacing after the IDR currency symbol:
 * V8/Node emits a non-breaking space (`"Rp\u00A01.124.000"`), while JSC/Bun on
 * macOS omits the space (`"Rp1.124.000"`). Normalizing `\u00A0` (and optional space
 * right after `"Rp"`) to a single ASCII space lets the assertion verify the exact
 * currency prefix, thousands separators, and absence of decimal fractions across
 * both engines.
 */
function normalizeRupiah(value: string): string {
  return value.replace(/\u00A0/g, " ").replace(/^Rp\s*/, "Rp ");
}

test("formatRupiah formats whole rupiah with thousands separators and zero fraction digits", () => {
  expect(normalizeRupiah(formatRupiah(1_124_000))).toBe("Rp 1.124.000");
  expect(normalizeRupiah(formatRupiah(0))).toBe("Rp 0");
});

test("formatRupiahCost preserves whole rupiah at 1.000+ and up to two decimals below 1.000", () => {
  expect(normalizeRupiah(formatRupiahCost(1_124_000))).toBe("Rp 1.124.000");

  const subThousand = normalizeRupiah(formatRupiahCost(812.4));
  expect(subThousand).not.toBe("Rp 812");
  expect(subThousand).toContain("812,4");

  const subOne = normalizeRupiah(formatRupiahCost(0.42));
  expect(subOne).not.toBe("Rp 0");
  expect(subOne).toBe("Rp 0,42");
});

test("formatRupiahCost and formatRupiah diverge on fractional sub-rupiah amounts", () => {
  expect(formatRupiahCost(0.42)).not.toBe(formatRupiah(0.42));
});

test("formatTokens formats token counts with Indonesian thousands separators", () => {
  expect(formatTokens(1_124_000)).toBe("1.124.000");
  expect(formatTokens(0)).toBe("0");
});
