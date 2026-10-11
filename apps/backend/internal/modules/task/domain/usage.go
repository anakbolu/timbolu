package domain

// Usage is what one model call cost, in tokens. It mirrors the gateway's shape
// so a step records the same numbers the ledger does.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
}
