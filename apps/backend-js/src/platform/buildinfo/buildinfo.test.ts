/**
 * The build identity is what makes a deploy verifiable from outside, so the
 * cases that matter are "the image wrote a fingerprint", "the image wrote a
 * build time", and "nothing did". The timestamp is separate from the
 * fingerprint because a re-rolled container for an unchanged tree reports the
 * same fingerprint but a different build time.
 */

import { afterEach, describe, expect, test } from "bun:test";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { snapshot } from "./buildinfo.ts";

afterEach(() => {
  delete process.env.BUILT_AT;
});

describe("snapshot", () => {
  // A fresh directory per case, so no two cases share a cached read.
  test("reports unknown when no fingerprint file exists", () => {
    const path = join(mkdtempSync(join(tmpdir(), "buildinfo-")), ".build-fingerprint");
    expect(snapshot(path).build).toBe("unknown");
  });

  test("reports the fingerprint the image build wrote", () => {
    const path = join(mkdtempSync(join(tmpdir(), "buildinfo-")), ".build-fingerprint");
    writeFileSync(path, "0123456789abcdef\n");
    expect(snapshot(path).build).toBe("0123456789abcdef");
  });

  test("reports unknown when no build time file exists", () => {
    const dir = mkdtempSync(join(tmpdir(), "buildinfo-"));
    expect(snapshot(join(dir, ".build-fingerprint"), join(dir, ".build-built-at")).built_at).toBe(
      "unknown",
    );
  });

  test("reports the build time the image build wrote", () => {
    const dir = mkdtempSync(join(tmpdir(), "buildinfo-"));
    const builtAt = join(dir, ".build-built-at");
    writeFileSync(builtAt, "2026-10-30T09:12:45Z\n");
    expect(snapshot(join(dir, ".build-fingerprint"), builtAt).built_at).toBe("2026-10-30T09:12:45Z");
  });

  test("reports the BUILT_AT environment value over the file", () => {
    const dir = mkdtempSync(join(tmpdir(), "buildinfo-"));
    const builtAt = join(dir, ".build-built-at");
    writeFileSync(builtAt, "2026-10-30T09:12:45Z\n");
    process.env.BUILT_AT = "2026-01-01T00:00:00Z";
    expect(snapshot(join(dir, ".build-fingerprint"), builtAt).built_at).toBe("2026-01-01T00:00:00Z");
  });
});
