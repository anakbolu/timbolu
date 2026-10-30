// Package pricing turns token counts into a cost.
//
// The prices are configuration, not code: the packages the product sells are not
// decided yet, and a provider changes its price without warning. The table here
// holds the defaults and the longest-prefix match, so a dated model name
// ("gpt-4o-mini-2024-07-18") is priced by its family.
package pricing

import (
	"strings"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
)

// Price is the cost of one million tokens, in micro-rupiah (1 rupiah =
// 1,000,000 micro). Micro-rupiah keeps a single cheap call a whole number, which
// a rupiah-denominated float would not.
type Price struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// Table prices a call by model name.
type Table struct {
	prices   map[string]Price
	fallback Price
}

// New builds a table. Keys are matched case-insensitively, and the longest key
// that prefixes the model name wins, so a family entry covers its dated
// variants.
func New(prices map[string]Price, fallback Price) *Table {
	normalised := make(map[string]Price, len(prices))
	for model, price := range prices {
		normalised[strings.ToLower(strings.TrimSpace(model))] = price
	}
	return &Table{prices: normalised, fallback: fallback}
}

// Cost returns the price of one call, in micro-rupiah.
//
// An unknown model costs the fallback price. That is deliberate: a missing entry
// must not silently bill zero, because zero is indistinguishable from a free
// call in every report the customer sees.
func (t *Table) Cost(model string, usage domain.Usage) int64 {
	if t == nil {
		return 0
	}

	price := t.lookup(model)
	if price.Input == 0 && price.Output == 0 && price.CacheRead == 0 && price.CacheWrite == 0 {
		return 0
	}

	return perMillion(price.Input, usage.InputTokens) +
		perMillion(price.Output, usage.OutputTokens) +
		perMillion(price.CacheRead, usage.CacheReadTokens) +
		perMillion(price.CacheWrite, usage.CacheWriteTokens)
}

func (t *Table) lookup(model string) Price {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return t.fallback
	}

	var (
		best     Price
		bestSize = -1
	)
	for prefix, price := range t.prices {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if len(prefix) > bestSize {
			best, bestSize = price, len(prefix)
		}
	}
	if bestSize < 0 {
		return t.fallback
	}
	return best
}

// perMillion converts a token count and a per-million price into micro-rupiah,
// rounding to the nearest micro so a long conversation does not drift.
func perMillion(pricePerMillion int64, tokens int) int64 {
	if tokens <= 0 || pricePerMillion == 0 {
		return 0
	}
	// (tokens * price) / 1_000_000, rounded: adding half the divisor before the
	// integer division avoids a float completely.
	return (int64(tokens)*pricePerMillion + 500_000) / 1_000_000
}

// Default prices, in micro-rupiah per million tokens, from the providers'
// published list prices converted at Rp 16,000/USD. They are the starting point
// the configuration overrides, not a promise: EPIC 13 (#98) owns the real table.
func Default() map[string]Price {
	return map[string]Price{
		"gpt-4o-mini":       {Input: 2_400, Output: 9_600, CacheRead: 1_200},
		"gpt-4o":            {Input: 40_000, Output: 160_000, CacheRead: 20_000},
		"claude-3-5-haiku":  {Input: 12_800, Output: 64_000, CacheRead: 1_280, CacheWrite: 16_000},
		"claude-3-5-sonnet": {Input: 48_000, Output: 240_000, CacheRead: 4_800, CacheWrite: 60_000},
		"claude-sonnet-4":   {Input: 48_000, Output: 240_000, CacheRead: 4_800, CacheWrite: 60_000},
		"deepseek-chat":     {Input: 4_400, Output: 8_800, CacheRead: 440},
		"llama-3.3-70b":     {Input: 9_600, Output: 9_600},
		"qwen2.5":           {Input: 6_400, Output: 6_400},
	}
}

// DefaultFallback is the price used for a model the table does not know. It is
// deliberately a mid-range price rather than zero, so an unpriced model shows up
// in the cost report instead of hiding in it.
var DefaultFallback = Price{Input: 16_000, Output: 64_000, CacheRead: 1_600, CacheWrite: 20_000}

// compile-time check: the table satisfies the module's port.
var _ domain.CostTable = (*Table)(nil)
