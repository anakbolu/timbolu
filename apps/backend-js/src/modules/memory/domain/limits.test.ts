import { describe, expect, it } from "bun:test";
import { DefaultLimits, normaliseLimits, type Limits } from "./service.ts";
import { limitOf } from "../repository/postgres.ts";
import { chunkText } from "../service/chunk.ts";

describe("memory limits and boundaries", () => {
  it("normaliseLimits falls back missing or zero fields to defaults while preserving provided valid fields", () => {
    const partial = { maxContentRunes: 6000 } as unknown as Limits;
    const normalised = normaliseLimits(partial);

    expect(normalised.maxContentRunes).toBe(6000);
    expect(normalised.mainTokens).toBe(DefaultLimits.mainTokens);
    expect(normalised.recallMemories).toBe(DefaultLimits.recallMemories);
    expect(normalised.recallKnowledge).toBe(DefaultLimits.recallKnowledge);
    expect(normalised.similarity).toBe(DefaultLimits.similarity);
    expect(normalised.quietPeriod).toBe(DefaultLimits.quietPeriod);
    expect(normalised.extractionMessages).toBe(DefaultLimits.extractionMessages);
    expect(normalised.chunkRunes).toBe(DefaultLimits.chunkRunes);
    expect(normalised.chunkOverlapRunes).toBe(DefaultLimits.chunkOverlapRunes);

    for (const val of Object.values(normalised)) {
      expect(typeof val).toBe("number");
      expect(Number.isFinite(val)).toBe(true);
      expect(val).toBeGreaterThan(0);
    }
  });

  it("limitOf returns a positive integer bounded by listLimit and never returns undefined or NaN", () => {
    expect(limitOf(undefined as unknown as number)).toBe(50);
    expect(limitOf(null as unknown as number)).toBe(50);
    expect(limitOf(0)).toBe(50);
    expect(limitOf(-10)).toBe(50);
    expect(limitOf(NaN)).toBe(50);
    expect(limitOf(10)).toBe(10);
    expect(limitOf(150)).toBe(150);
    expect(limitOf(500)).toBe(200);
  });

  it("chunkText falls back to 1000 runes when size is undefined or non-positive", () => {
    const doc = "a".repeat(6000);
    const chunks = chunkText(doc, undefined as unknown as number, 0);

    expect(chunks.length).toBeGreaterThanOrEqual(6);
    for (const chunk of chunks) {
      expect(Array.from(chunk).length).toBeLessThanOrEqual(1000);
    }
  });
});
