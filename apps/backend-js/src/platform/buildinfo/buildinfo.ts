/**
 * Reports which build a running process came from, so a deployment can be
 * verified from outside.
 *
 * Ported from internal/platform/buildinfo/buildinfo.go. The obvious answer —
 * the commit SHA — is not available here either: the build context excludes
 * `.git`, and the deploy platform has no per-build build-argument field. What
 * does change with every commit is the source, so the Dockerfile hashes every
 * TypeScript file of this app and writes the result to FINGERPRINT_PATH at
 * build time. A caller who reads the fingerprint from a live process and
 * computes the same hash from a checkout proves the deployed image is that
 * tree — without a panel, a shell, or a commit SHA.
 *
 * The Go build injects these values with -ldflags; a Bun image has no
 * equivalent, so the build drops the fingerprint in a file and the platform
 * supplies the rest through the environment.
 *
 * The fingerprint answers *which tree* the process serves. It cannot answer
 * *when that image was built*, and the two questions are not the same: a
 * re-rolled container for a tree whose contents did not change (a docs-only
 * deploy, a rollout after a restart) reports the same fingerprint. So the build
 * writes the build time to a second file, and this module reads it the same
 * way.
 */

import { readFileSync } from "node:fs";

/** Where the image build writes the source fingerprint. */
const FINGERPRINT_PATH = "/app/.build-fingerprint";

/** Where the image build writes the RFC3339 timestamp of the build. */
const BUILT_AT_PATH = "/app/.build-built-at";

/** Info is the build identity in the shape the health endpoint publishes. */
export interface Info {
  commit: string;
  built_at: string;
  build: string;
}

/**
 * Read once per path: the probe runs on every container health check, and the
 * file cannot change while the process lives. A failed read is cached too, so a
 * missing file costs one syscall rather than one per probe.
 */
const fileValues: Record<string, string> = {};

/**
 * Returns the trimmed contents of `path`, or "" when it cannot be read. Shared
 * by both build files so the caching rule cannot drift between them.
 */
function readCached(path: string): string {
  let value = fileValues[path];
  if (value === undefined) {
    try {
      value = readFileSync(path, "utf8").trim();
    } catch {
      // Never rethrow: a probe must not die because the build identity is
      // missing, and `bun run src/cmd/api.ts` outside an image has no file.
      value = "";
    }
    fileValues[path] = value;
  }
  return value;
}

/**
 * Returns the build identity of this process. A value the build did not set is
 * reported as "unknown" rather than as an empty string, so a probe always
 * answers the same shape and an absent field is not mistaken for a broken one.
 *
 * The environment wins over the file, so a platform that gains a per-build
 * build-argument field can supply `BUILT_AT` without another image change.
 *
 * The two path parameters exist so a test can point at temporary files;
 * production callers pass nothing and get FINGERPRINT_PATH and BUILT_AT_PATH.
 */
export function snapshot(
  fingerprintPath: string = FINGERPRINT_PATH,
  builtAtPath: string = BUILT_AT_PATH,
): Info {
  return {
    commit: process.env.BUILD_COMMIT || "unknown",
    built_at: process.env.BUILT_AT || readCached(builtAtPath) || "unknown",
    build: readCached(fingerprintPath) || "unknown",
  };
}
