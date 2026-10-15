"use client";

/**
 * The gate in front of the operator surface.
 *
 * It asks the API two questions in order — is there a session, and does its
 * account carry the platform role — and it is the second answer that decides.
 * The role is not read from the session hint cookie, from local storage, or from
 * anything else the browser could edit: the API is the only thing that knows it,
 * and `/auth/me` is where it says so.
 *
 * This is a door, not the lock. Every request the screens make is checked again
 * by the API against the account's role, so a browser that got past this would
 * still be answered 403. What it buys is that a plain account is sent back to
 * their workspace instead of being shown a shell full of refusals.
 */

import { useRouter } from "next/navigation";
import { useEffect, useState } from "react";

import { AdminShell } from "@/components/admin/admin-shell";
import { Skeleton } from "@/components/ui/skeleton";
import { currentUser, refreshSession, setAccessToken, type User } from "@/lib/api";

/** What the gate is waiting on. */
type Gate =
  | { state: "checking" }
  | { state: "admitted"; user: User }
  | { state: "refused"; message: string };

export function AdminGate({ children }: { children: React.ReactNode }) {
  const router = useRouter();
  const [gate, setGate] = useState<Gate>({ state: "checking" });

  useEffect(() => {
    let live = true;

    void (async () => {
      // The access token lives in memory, so a reload has none: the refresh
      // cookie is what says whether this browser still has a session. It is the
      // same exchange the workspace screens do, for the same reason.
      const session = await refreshSession();
      if (!live) return;

      if (!session.ok) {
        setAccessToken(null);
        router.replace("/login?sesi=berakhir");
        return;
      }

      const account = await currentUser();
      if (!live) return;

      if (!account.ok) {
        if (account.status === 401) {
          setAccessToken(null);
          router.replace("/login?sesi=berakhir");
          return;
        }
        setGate({ state: "refused", message: account.message });
        return;
      }

      if (account.data.role !== "admin") {
        // Not an error to show: this account simply has a workspace to be in.
        router.replace("/dashboard");
        return;
      }

      setGate({ state: "admitted", user: account.data });
    })();

    return () => {
      live = false;
    };
  }, [router]);

  if (gate.state === "admitted") {
    return <AdminShell user={gate.user}>{children}</AdminShell>;
  }

  if (gate.state === "refused") {
    return (
      <div className="flex min-h-dvh items-center justify-center bg-bolu-bg p-6">
        <p role="alert" className="max-w-md rounded-md border border-bolu-belum/30 bg-bolu-belum/5 px-4 py-3 text-[15px] text-bolu-belum">
          {gate.message}
        </p>
      </div>
    );
  }

  // The shape of what is coming, so the redirect does not flash a blank page.
  return (
    <div className="flex min-h-dvh items-center justify-center bg-bolu-bg p-6">
      <div className="flex w-full max-w-md flex-col gap-3">
        <Skeleton className="h-6 w-40" />
        <Skeleton className="h-24 w-full" />
        <Skeleton className="h-24 w-full" />
      </div>
    </div>
  );
}
