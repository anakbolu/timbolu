"use client";

/**
 * StartLink is the landing page's "Coba gratis" call to action.
 *
 * A signed-in visitor is sent to the dashboard they already have; a signed-out
 * one is sent to the register page, because a dashboard a visitor cannot read
 * is a login wall in disguise. The hint cookie is readable on the client — the
 * API client writes it with document.cookie — so the choice is made after mount,
 * and the server renders the sign-up href, which is also the right static answer
 * for the landing page's audience.
 */

import Link from "next/link";
import { useSyncExternalStore } from "react";

import { SESSION_HINT_COOKIE } from "@/lib/session-hint";

/** readSessionHint is the snapshot the link's destination hangs on. */
function readSessionHint() {
  return document.cookie.includes(`${SESSION_HINT_COOKIE}=`);
}

/** The cookie changes only via sign-in/sign-out in the shell; nothing to watch. */
function subscribeSessionHint() {
  return () => {};
}

/** The landing page is static-rendered without a browser, so the server side is "no session". */
function getServerSessionHint() {
  return false;
}

export function StartLink({
  className,
  children,
  onClick,
}: {
  className?: string;
  children?: React.ReactNode;
  onClick?: () => void;
}) {
  // The server answer and the first hydration render agree (no session), then the
  // client snapshot — the actual cookie — replaces it after hydration, which is
  // what lets a signed-in visitor land on their dashboard without a flash of the
  // wrong destination.
  const signedIn = useSyncExternalStore(
    subscribeSessionHint,
    readSessionHint,
    getServerSessionHint,
  );

  return (
    <Link href={signedIn ? "/dashboard" : "/signup"} className={className} onClick={onClick}>
      {children}
    </Link>
  );
}