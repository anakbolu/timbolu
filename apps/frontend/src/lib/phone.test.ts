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

import { normalizeWhatsAppPhone } from "./phone";

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
