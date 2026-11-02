/**
 * Tests for WhatsApp phone number normalization.
 *
 * WhatsApp numbers registered through the dashboard must satisfy the backend
 * contract (country-coded format without leading '0' or '620'). These tests pin
 * the input transformation rules defined in #690.
 *
 * Run it with Bun, like the other frontend tests:
 *   cd apps/frontend && bun test src/lib/phone.test.ts
 */

import assert from "node:assert/strict";
import test from "node:test";

import { normalizeWhatsAppPhone, parseWhatsAppPhone } from "./phone";

test("converts leading 0 national format to 62", () => {
  assert.equal(normalizeWhatsAppPhone("0812-3456-789"), "628123456789");
});

test("strips formatting from numbers already starting with 62", () => {
  assert.equal(normalizeWhatsAppPhone("62812-3456-789"), "628123456789");
});

test("strips plus signs and spaces from international format", () => {
  assert.equal(normalizeWhatsAppPhone("+62 812 3456 789"), "628123456789");
});

test("collapses leading 620 to 62", () => {
  assert.equal(normalizeWhatsAppPhone("620812345678"), "62812345678");
});

test("returns empty string for empty input", () => {
  assert.equal(normalizeWhatsAppPhone(""), "");
});

test("returns empty string for all-non-digit input", () => {
  assert.equal(normalizeWhatsAppPhone("abc"), "");
});

test("parseWhatsAppPhone rejects invalid numbers", () => {
  const rejected = ["8123456789", "12345678", "+1 415 555 0100", "0812345", "0008123456"];
  for (const input of rejected) {
    const res = parseWhatsAppPhone(input);
    assert.equal(res.ok, false);
    if (!res.ok) {
      assert.ok(res.reason.length > 0);
    }
  }
  const leading8 = parseWhatsAppPhone("8123456789");
  assert.equal(leading8.ok, false);
  if (!leading8.ok) {
    assert.match(leading8.reason, /kode negara/i);
  }
});

test("parseWhatsAppPhone accepts valid numbers", () => {
  const accepted = ["0812-3456-789", "62812-3456-789", "+62 812 3456 789"];
  for (const input of accepted) {
    const res = parseWhatsAppPhone(input);
    assert.equal(res.ok, true);
    if (res.ok) {
      assert.equal(res.phone, "628123456789");
    }
  }
});
