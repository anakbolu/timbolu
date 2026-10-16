/**
 * The one thing left to say while the workspace is not there yet.
 *
 * `WorkspaceProvider` owns the question: it resolves the workspace on its first
 * load and sends an account without one to /onboarding before any screen behind
 * the shell renders, so no screen shows an empty state for it. What is left is the
 * window before that answer lands, where an action that reads its id from the store
 * can still be asked to run — a draft decision in a thread, say — and there is
 * nothing to run it against. The only account that ever sees this sentence is one
 * that does have a workspace, so it asks for a moment rather than naming a screen
 * the redirect would have opened already.
 */
export const NO_WORKSPACE_MESSAGE = "Workspace masih dimuat. Coba lagi sebentar.";
