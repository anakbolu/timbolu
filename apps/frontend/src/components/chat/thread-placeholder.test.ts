import { expect, test } from "bun:test";

import { threadPlaceholder } from "./thread-placeholder";

test("loading + 0 pesan yields loading (not empty)", () => {
  const result = threadPlaceholder({
    active: true,
    loading: true,
    error: null,
    messageCount: 0,
  });
  expect(result).toBe("loading");
});

test("error + 0 pesan yields error", () => {
  const result = threadPlaceholder({
    active: true,
    loading: false,
    error: "Gagal memuat pesan",
    messageCount: 0,
  });
  expect(result).toBe("error");
});

test("tanpa aktif yields no-conversation", () => {
  const result = threadPlaceholder({
    active: false,
    loading: false,
    error: null,
    messageCount: 0,
  });
  expect(result).toBe("no-conversation");
});

test("tanpa aktif yields no-conversation even if loading", () => {
  const result = threadPlaceholder({
    active: false,
    loading: true,
    error: null,
    messageCount: 0,
  });
  expect(result).toBe("no-conversation");
});

test("0 pesan tanpa loading tanpa error yields empty", () => {
  const result = threadPlaceholder({
    active: true,
    loading: false,
    error: null,
    messageCount: 0,
  });
  expect(result).toBe("empty");
});

test("ada pesan yields messages", () => {
  const result = threadPlaceholder({
    active: true,
    loading: false,
    error: null,
    messageCount: 5,
  });
  expect(result).toBe("messages");
});

test("ada pesan yields messages even if loading older", () => {
  const result = threadPlaceholder({
    active: true,
    loading: true,
    error: null,
    messageCount: 5,
  });
  expect(result).toBe("messages");
});

test("ada pesan yields messages even if error present", () => {
  const result = threadPlaceholder({
    active: true,
    loading: false,
    error: "Ada error",
    messageCount: 5,
  });
  expect(result).toBe("messages");
});
