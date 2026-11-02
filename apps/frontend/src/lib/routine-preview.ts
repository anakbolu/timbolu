/**
 * Routine preview state representation and resolution.
 *
 * The routine form previews upcoming runs from the server before saving.
 * Previously, an empty run list was treated as "still calculating", which
 * caused failed preview requests (e.g. 402 read-only, quota exceeded, or network
 * error) to display "menghitung waktu berikutnya…" indefinitely (#697).
 *
 * This helper models the request lifecycle explicitly as a discriminated union
 * so UI rendering cannot conflate in-flight loading, request failure, and empty results.
 */

import type { ApiResult, Routine } from "@/lib/api";

export type RoutinePreviewState =
  | { state: "idle" }
  | { state: "busy" }
  | { state: "failed"; reason: string }
  | { state: "empty" }
  | { state: "ready"; runs: string[] };

export type RoutinePreviewInput = {
  kind?: Routine["kind"];
  busy: boolean;
  result: ApiResult<{ next_runs: string[] }> | null;
};

/**
 * resolveRoutinePreview computes the discriminated preview state from the
 * routine kind, in-flight busy flag, and last completed API result.
 *
 * Rules:
 * 1. Trigger routines have no schedule or times -> "idle".
 * 2. While a request is in flight -> "busy".
 * 3. Before any request has settled and not busy -> "idle".
 * 4. A failed request ({ ok: false }) -> "failed" with the error message.
 * 5. A successful request with runs -> "ready" with the next run ISO strings.
 * 6. A successful request with empty next_runs -> "empty".
 */
export function resolveRoutinePreview(input: RoutinePreviewInput): RoutinePreviewState {
  if (input.kind === "trigger") {
    return { state: "idle" };
  }

  if (input.busy) {
    return { state: "busy" };
  }

  if (!input.result) {
    return { state: "idle" };
  }

  if (!input.result.ok) {
    return {
      state: "failed",
      reason: input.result.message || "Gagal menghitung jadwal berikutnya",
    };
  }

  const runs = input.result.data.next_runs ?? [];
  if (runs.length === 0) {
    return { state: "empty" };
  }

  return { state: "ready", runs };
}
