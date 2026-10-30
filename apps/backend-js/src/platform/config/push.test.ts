/**
 * The PUSH_ENABLED parser, pinned value by value.
 *
 * The switch guards a request that leaves the process, so its answer for the
 * spellings a deployment might actually use is the contract: `true` in any
 * case turns it on, and everything else — including `1`, which other flags in
 * this tree accept — stays off.
 */

import { describe, expect, test } from "bun:test";

import { parsePushEnabled } from "./push.ts";

describe("parsePushEnabled", () => {
  test("the literal true turns push on, whatever its case or surrounding space", () => {
    for (const value of ["true", "TRUE", "True", " true ", "\ttrue\n"]) {
      expect(parsePushEnabled(value)).toBe(true);
    }
  });

  test("every other value stays off", () => {
    for (const value of ["1", "yes", "on", "false", "0", "", "   ", undefined, null]) {
      expect(parsePushEnabled(value)).toBe(false);
    }
  });
});