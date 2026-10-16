/**
 * The one thing left to say about a workspace that is not there.
 *
 * `WorkspaceProvider` owns the question: it resolves the workspace on its first
 * load and sends an account without one to /onboarding before any screen behind
 * the shell renders, so no screen shows an empty state for it. An action that
 * reads its id from the store can still be asked to run in the window before that
 * answer lands — a draft decision in a thread, say — and there is nothing to run
 * it against. Saying so beats a request built on an id that is not there, and it
 * names the screen the redirect is about to open rather than a choice nobody made.
 */
export const NO_WORKSPACE_MESSAGE =
  "Workspace belum siap. Muat ulang halaman atau buat workspace di /onboarding.";
