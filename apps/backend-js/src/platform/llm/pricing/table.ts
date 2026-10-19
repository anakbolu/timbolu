/**
 * Turns token counts into a cost.
 *
 * Ported from internal/platform/llm/pricing/table.go. The prices are
 * configuration, not code, so the table holds the defaults and the
 * longest-prefix match: a dated model name ("gpt-4o-mini-2024-07-18") is priced
 * by its family.
 */

import type { Usage } from "../registry.ts";

/**
 * The cost of one million tokens, in micro-rupiah (1 rupiah = 1,000,000 micro).
 * Micro-rupiah keeps a single cheap call a whole number.
 */
export interface Price {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
}

/** Prices a call by model name. */
export class Table {
  private readonly prices: Record<string, Price>;
  private readonly fallback: Price;

  constructor(prices: Record<string, Price>, fallback: Price) {
    this.prices = Object.create(null) as Record<string, Price>;
    for (const [model, price] of Object.entries(prices)) {
      this.prices[model.toLowerCase().trim()] = price;
    }
    this.fallback = fallback;
  }

  /**
   * The price of one call, in micro-rupiah. An unknown model costs the fallback
   * price: a missing entry must not silently bill zero, because zero is
   * indistinguishable from a free call in every report the customer sees.
   */
  cost(model: string, usage: Usage): number {
    if (this === null || this === undefined) return 0;

    const price = this.lookup(model);
    if (price.input === 0 && price.output === 0 && price.cacheRead === 0 && price.cacheWrite === 0) {
      return 0;
    }

    return (
      perMillion(price.input, usage.inputTokens) +
      perMillion(price.output, usage.outputTokens) +
      perMillion(price.cacheRead, usage.cacheReadTokens) +
      perMillion(price.cacheWrite, usage.cacheWriteTokens)
    );
  }

  private lookup(model: string): Price {
    const name = model.toLowerCase().trim();
    if (name === "") return this.fallback;

    let best: Price | null = null;
    let bestSize = -1;
    for (const [prefix, price] of Object.entries(this.prices)) {
      if (!name.startsWith(prefix)) continue;
      if (prefix.length > bestSize) {
        best = price;
        bestSize = prefix.length;
      }
    }
    return best ?? this.fallback;
  }
}

/**
 * Converts a token count and a per-million price into micro-rupiah, rounding to
 * the nearest micro so a long conversation does not drift:
 * (tokens * price) / 1_000_000, with half the divisor added before the integer
 * division to avoid a float entirely.
 */
function perMillion(pricePerMillion: number, tokens: number): number {
  if (tokens <= 0 || pricePerMillion === 0) return 0;
  return Math.floor((tokens * pricePerMillion + 500_000) / 1_000_000);
}

/**
 * Default prices, in micro-rupiah per million tokens, from the providers'
 * published list prices converted at Rp 16,000/USD.
 */
export function defaults(): Record<string, Price> {
  return {
    "gpt-4o-mini": { input: 2_400, output: 9_600, cacheRead: 1_200, cacheWrite: 0 },
    "gpt-4o": { input: 40_000, output: 160_000, cacheRead: 20_000, cacheWrite: 0 },
    "claude-3-5-haiku": { input: 12_800, output: 64_000, cacheRead: 1_280, cacheWrite: 16_000 },
    "claude-3-5-sonnet": { input: 48_000, output: 240_000, cacheRead: 4_800, cacheWrite: 60_000 },
    "claude-sonnet-4": { input: 48_000, output: 240_000, cacheRead: 4_800, cacheWrite: 60_000 },
    "deepseek-chat": { input: 4_400, output: 8_800, cacheRead: 440, cacheWrite: 0 },
    "llama-3.3-70b": { input: 9_600, output: 9_600, cacheRead: 0, cacheWrite: 0 },
    "qwen2.5": { input: 6_400, output: 6_400, cacheRead: 0, cacheWrite: 0 },
  };
}

/**
 * The price used for a model the table does not know. Deliberately mid-range
 * rather than zero, so an unpriced model shows up in the cost report.
 */
export const DefaultFallback: Price = {
  input: 16_000,
  output: 64_000,
  cacheRead: 1_600,
  cacheWrite: 20_000,
};
