"use client";

/**
 * SignOutButton ends the session and returns to the sign-in screen.
 *
 * It clears the local token and the remembered workspace as well as revoking the
 * session on the server, because a screen that keeps working after a sign-out is a
 * screen that never really signed out. It navigates with `replace` so the back
 * button does not return to a page whose session is gone.
 */

import { useRouter } from "next/navigation";
import { useState } from "react";

import { logout } from "@/lib/api";

export function SignOutButton({ className }: { className?: string }) {
  const router = useRouter();
  const [busy, setBusy] = useState(false);

  async function signOut() {
    if (busy) {
      return;
    }

    setBusy(true);

    // The call is what revokes the refresh token on the server; the local state is
    // cleared either way, because a session the server already forgot must not
    // leave a usable token behind.
    await logout();

    router.replace("/login");
  }

  return (
    <button
      type="button"
      onClick={() => void signOut()}
      disabled={busy}
      className={
        className ??
        "flex min-h-[52px] cursor-pointer items-center gap-3 rounded-2xl border-0 bg-transparent px-3.5 text-[17px] font-bold text-bolu-ink disabled:opacity-60"
      }
    >
      <svg
        width="22"
        height="22"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="2"
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden="true"
      >
        <path d="M15 4h3a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2h-3" />
        <path d="M10 17l-5-5 5-5" />
        <path d="M5 12h11" />
      </svg>
      <span className="flex-1 text-left">{busy ? "Keluar…" : "Keluar"}</span>
    </button>
  );
}
