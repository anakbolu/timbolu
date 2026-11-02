export type ThreadPlaceholderState = {
  active: boolean;
  loading: boolean;
  error: string | null;
  messageCount: number;
};

export type ThreadPlaceholderDecision =
  | "no-conversation"
  | "loading"
  | "error"
  | "empty"
  | "messages";

/**
 * threadPlaceholder decides what should be rendered in the message area.
 *
 * Precedence:
 * 1. Without an active conversation -> "no-conversation"
 * 2. If messages are already present -> "messages"
 * 3. While loading -> "loading"
 * 4. If an error occurred -> "error"
 * 5. Otherwise (0 messages, not loading, no error) -> "empty"
 */
export function threadPlaceholder(state: ThreadPlaceholderState): ThreadPlaceholderDecision {
  if (!state.active) {
    return "no-conversation";
  }
  if (state.messageCount > 0) {
    return "messages";
  }
  if (state.loading) {
    return "loading";
  }
  if (state.error) {
    return "error";
  }
  return "empty";
}
