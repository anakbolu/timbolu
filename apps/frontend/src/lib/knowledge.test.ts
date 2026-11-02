/**
 * Tests for knowledge base helpers.
 *
 * Run it with Bun, like the other frontend tests:
 *   cd apps/frontend && bun test src/lib/knowledge.test.ts
 */

import assert from "node:assert/strict";
import test from "node:test";

import { KNOWLEDGE_LIST_LIMIT, formatKnowledgeCount } from "./knowledge";

test("formats 0 count", () => {
  assert.equal(formatKnowledgeCount(0), "0 potongan");
});

test("formats count below limit", () => {
  assert.equal(formatKnowledgeCount(49), "49 potongan");
});

test("formats count at default limit", () => {
  assert.equal(formatKnowledgeCount(200), "menampilkan 200 potongan terbaru");
});

test("formats count matching explicit limit", () => {
  assert.equal(formatKnowledgeCount(200, 200), "menampilkan 200 potongan terbaru");
  assert.equal(formatKnowledgeCount(KNOWLEDGE_LIST_LIMIT, KNOWLEDGE_LIST_LIMIT), "menampilkan 200 potongan terbaru");
});
