/**
 * The sentence a broken single-record link shows.
 *
 * Two regressions are pinned here. A `/chat?agent=<uuid asing>` link was read as
 * a missing conversation, so a person was told the chat was gone when the Bolu
 * was (#456); it now has its own sentence. And every retryable sentence ended in
 * "Coba lagi." beside a button saying the same, which printed "Coba lagi.Coba
 * lagi" on screen — the sentence now says what happened and the button offers the
 * repeat.
 *
 * Run it with Bun, like the other frontend tests, because the module resolves
 * `@/lib/api` through the app's alias:
 *
 *   cd apps/frontend && bun test src/lib/friendly-error.test.ts
 */

import assert from "node:assert/strict";
import test from "node:test";

import type { ApiError } from "@/lib/api";
import { CONVERSATION_NOT_FOUND, friendlyLoadFailure, type DeepLinkKind } from "./friendly-error";

const KINDS: DeepLinkKind[] = ["task", "draft", "conversation", "agent"];

function apiError(overrides: Partial<ApiError> = {}): ApiError {
  return { ok: false, status: 500, message: "internal server error", ...overrides };
}

test("a 404 for ?agent= names the Bolu, not the conversation", () => {
  const failure = friendlyLoadFailure(apiError({ status: 404, code: "agent_not_found", message: "agent not found" }), "agent");

  assert.equal(
    failure.message,
    "Bolu tidak ditemukan — mungkin sudah dihapus, atau tautannya berasal dari ruang kerja lain.",
  );
  assert.equal(failure.retryable, false);
});

test("an agent id that is not an id says the link is broken", () => {
  const failure = friendlyLoadFailure(apiError({ status: 400, code: "invalid_agent_id" }), "agent");

  assert.match(failure.message, /^Tautan tidak valid — id Bolu /);
  assert.equal(failure.retryable, false);
});

test("no retryable sentence repeats the button's words", () => {
  for (const kind of KINDS) {
    const failure = friendlyLoadFailure(apiError({ status: 503 }), kind);

    assert.equal(failure.retryable, true, kind);
    assert.doesNotMatch(failure.message, /coba lagi/i, `${kind}: ${failure.message}`);
  }
});

test("a readable detail is appended once, without a second invitation", () => {
  const failure = friendlyLoadFailure(
    apiError({ status: 502, message: "Server terlalu lama menjawab. Coba lagi." }),
    "agent",
  );

  assert.equal(
    failure.message,
    "Bolu gagal dimuat — server atau jaringannya sedang bermasalah. Server terlalu lama menjawab.",
  );
});

test("the conversation sentence is still the one the list-miss banner uses", () => {
  assert.equal(CONVERSATION_NOT_FOUND, friendlyLoadFailure(apiError({ status: 404 }), "conversation").message);
});
