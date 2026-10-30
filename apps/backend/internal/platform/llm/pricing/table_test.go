package pricing

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
)

func TestCostUsesTheModelPrice(t *testing.T) {
	table := New(map[string]Price{
		"gpt-4o-mini": {Input: 2_400, Output: 9_600, CacheRead: 1_200},
	}, Price{})

	// 1,000,000 tokens at 2,400 micros per million is 2,400 micros, which is
	// how the price is defined: per million, in micro-rupiah.
	require.Equal(t, int64(2_400+9_600), table.Cost("gpt-4o-mini", domain.Usage{
		InputTokens:  1_000_000,
		OutputTokens: 1_000_000,
	}))
	require.Equal(t, int64(2_400), table.Cost("gpt-4o-mini", domain.Usage{InputTokens: 1_000_000}))
}

func TestCostMatchesTheLongestPrefix(t *testing.T) {
	table := New(map[string]Price{
		"claude-3-5":       {Input: 10},
		"claude-3-5-haiku": {Input: 100},
		"gpt-4o":           {Input: 200},
	}, Price{})

	require.Equal(t, int64(100), table.Cost("Claude-3-5-haiku", domain.Usage{InputTokens: 1_000_000}))
	require.Equal(t, int64(100), table.Cost("claude-3-5-haiku-20241022", domain.Usage{InputTokens: 1_000_000}))
	require.Equal(t, int64(10), table.Cost("claude-3-5-sonnet", domain.Usage{InputTokens: 1_000_000}))
	require.Equal(t, int64(200), table.Cost("gpt-4o", domain.Usage{InputTokens: 1_000_000}))
}

// TestCostNeverSilentlyBillsZero is the reason the fallback price exists: a
// model missing from the table must show up in the report, not disappear from
// it.
func TestCostNeverSilentlyBillsZero(t *testing.T) {
	table := New(map[string]Price{}, DefaultFallback)

	cost := table.Cost("some-new-model", domain.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	require.Equal(t, DefaultFallback.Input+DefaultFallback.Output, cost)
	require.NotZero(t, cost)
}

func TestCostCountsTheCacheTokens(t *testing.T) {
	table := New(map[string]Price{
		"claude-sonnet-4": {Input: 48_000, Output: 240_000, CacheRead: 4_800, CacheWrite: 60_000},
	}, Price{})

	require.Equal(t, int64(4_800), table.Cost("claude-sonnet-4", domain.Usage{CacheReadTokens: 1_000_000}))
	require.Equal(t, int64(60_000), table.Cost("claude-sonnet-4", domain.Usage{CacheWriteTokens: 1_000_000}))
}

func TestCostIsZeroWithoutTokens(t *testing.T) {
	table := New(Default(), DefaultFallback)

	require.Zero(t, table.Cost("gpt-4o", domain.Usage{}))
	// An empty model name is not a model: it is priced as unknown, which is the
	// fallback rather than free.
	require.Equal(t, DefaultFallback.Input, table.Cost("", domain.Usage{InputTokens: 1_000_000}))
}

// TestCostRoundsToTheNearestMicro keeps a long conversation from drifting: the
// result is an integer count of micro-rupiah, and the rounding is deliberate.
func TestCostRoundsToTheNearestMicro(t *testing.T) {
	table := New(map[string]Price{"m": {Input: 3}}, Price{})

	// 1 token at 3 micros per million rounds to 0, and half a million tokens
	// rounds to the nearest whole micro.
	require.Equal(t, int64(0), table.Cost("m", domain.Usage{InputTokens: 1}))
	require.Equal(t, int64(2), table.Cost("m", domain.Usage{InputTokens: 500_000}))
	require.Equal(t, int64(3), table.Cost("m", domain.Usage{InputTokens: 1_000_000}))
}

func TestNilTableCostsNothing(t *testing.T) {
	var table *Table
	require.Zero(t, table.Cost("gpt-4o", domain.Usage{InputTokens: 1000}))
}

func TestDefaultPricesCoverTheSeededModels(t *testing.T) {
	table := New(Default(), DefaultFallback)

	for _, model := range []string{"gpt-4o-mini", "gpt-4o", "claude-3-5-sonnet", "deepseek-chat"} {
		require.NotZero(t, table.Cost(model, domain.Usage{InputTokens: 1000, OutputTokens: 1000}), model)
	}
}
