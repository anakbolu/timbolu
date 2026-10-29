/**
 * The build identity is what makes a deploy verifiable from outside, so the two
 * cases that matter are "the image wrote a fingerprint" and "nothing did".
 */

import { describe, expect, test } from "bun:test";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { snapshot } from "./buildinfo.ts";

describe("snapshot", () => {
  test("reports unknown when no fingerprint file exists", () => {
    const path = join(mkdtempSync(join(tmpdir(), "buildinfo-")), ".build-fingerprint");
    expect(snapshot(path).build).toBe("unknown");
  });

  test("reports the fingerprint the image build wrote", () => {
    const dir = mkdtempSync(join(tmpdir(), "buildinfo-"));
    const path = join(dir, ".build-fingerprint");
    writeFileSync(path, "0123456789abcdef\n");
    expect(snapshot(path).build).toBe("0123456789abcdef");
  });
});
