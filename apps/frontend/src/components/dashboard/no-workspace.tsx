"use client";

/**
 * The empty state for a screen that needs a workspace to show anything.
 *
 * Resolving the workspace is a network call that can come back empty: the
 * session is valid, but the account never finished onboarding or its workspace
 * is gone. The screens used to answer that with a red error line naming the
 * missing choice, which reads as a failure the person caused; having no
 * workspace yet is a starting point, so the screen offers the door out instead.
 */

import Link from "next/link";

/**
 * NO_WORKSPACE_MESSAGE is the inline error for an action that runs inside work
 * already on screen — a draft decision in a thread, say — where replacing the
 * screen with an empty state would throw away what the person was reading.
 */
export const NO_WORKSPACE_MESSAGE =
  "Workspace belum siap. Muat ulang halaman atau buat workspace di /onboarding.";

/** NoWorkspaceNotice is the card a screen shows when there is no workspace yet. */
export function NoWorkspaceNotice() {
  return (
    <section className="flex flex-col items-start gap-3 rounded-md border border-bolu-border bg-white p-5">
      <h2 className="font-display text-[18px] font-semibold">Belum ada workspace</h2>
      <p className="text-[15px] text-bolu-muted">
        Buat workspace dulu supaya Bolu bisa mulai bekerja.
      </p>
      <Link
        href="/onboarding"
        className="rounded-md bg-bolu-accent px-4 py-2 text-sm font-semibold text-white transition hover:opacity-90"
      >
        Buat workspace
      </Link>
    </section>
  );
}
