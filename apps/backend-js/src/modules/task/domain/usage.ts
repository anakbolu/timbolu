/**
 * What one model call cost, in tokens.
 *
 * It mirrors the gateway's shape so a step records the same numbers the ledger
 * does. Ported 1:1 from internal/modules/task/domain/usage.go.
 */
export interface Usage {
  InputTokens: number;
  OutputTokens: number;
  CacheReadTokens: number;
  CacheWriteTokens: number;
}
