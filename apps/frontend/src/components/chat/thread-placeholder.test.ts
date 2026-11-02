import assert from "node:assert/strict";
import test from "node:test";

import { threadPlaceholder } from "./thread-placeholder";

test("loading + 0 pesan yields loading (not empty)", () => {
  const result = threadPlaceholder({
    active: true,
    loading: true,
    error: null,
    messageCount: 0,
  });
  assert.equal(result, "loading");
});
test("loading + error + 0 pesan yields loading (loading takes precedence)", () => {
  const result = threadPlaceholder({
    active: true,
    loading: true,
    error: "Gagal memuat pesan",
    messageCount: 0,
  });
  assert.equal(result, "loading");
});


test("error + 0 pesan yields error", () => {
  const result = threadPlaceholder({
    active: true,
    loading: false,
    error: "Gagal memuat pesan",
    messageCount: 0,
  });
  assert.equal(result, "error");
});

test("tanpa aktif yields no-conversation", () => {
  const result = threadPlaceholder({
    active: false,
    loading: false,
    error: null,
    messageCount: 0,
  });
  assert.equal(result, "no-conversation");
});

test("tanpa aktif yields no-conversation even if loading", () => {
  const result = threadPlaceholder({
    active: false,
    loading: true,
    error: null,
    messageCount: 0,
  });
  assert.equal(result, "no-conversation");
});

test("0 pesan tanpa loading tanpa error yields empty", () => {
  const result = threadPlaceholder({
    active: true,
    loading: false,
    error: null,
    messageCount: 0,
  });
  assert.equal(result, "empty");
});

test("ada pesan yields messages", () => {
  const result = threadPlaceholder({
    active: true,
    loading: false,
    error: null,
    messageCount: 5,
  });
  assert.equal(result, "messages");
});

test("ada pesan yields messages even if loading older", () => {
  const result = threadPlaceholder({
    active: true,
    loading: true,
    error: null,
    messageCount: 5,
  });
  assert.equal(result, "messages");
});

test("ada pesan yields messages even if error present", () => {
  const result = threadPlaceholder({
    active: true,
    loading: false,
    error: "Ada error",
    messageCount: 5,
  });
  assert.equal(result, "messages");
});
