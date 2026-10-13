"use client";

/**
 * The dashboard shell: the frame every screen lives in.
 *
 * The sidebar is the design's, values and all — 270px, white, the roster of both
 * teams, and Integrasi/Pengaturan pinned to the bottom. Navigation is by route
 * rather than by in-page state, so a screen keeps its own URL and the browser's
 * back button does what a person expects; the visual result is identical.
 */

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useState } from "react";

import { Bolu } from "@/components/bolu/bolu";
import { LOGO } from "@/lib/crew";
import {
  buildRoster,
  countPending,
  crewFor,
  expressionOf,
  stateOf,
  type RosterItem,
} from "@/lib/dashboard-view";

import { useWorkspace } from "./workspace-data";

/** The nav entries, in the design's order. */
const NAV = [
  {
    href: "/dashboard",
    label: "Dasbor",
    icon: "M4 4h7v7H4zM13 4h7v4h-7zM13 10h7v10h-7zM4 13h7v7H4z",
    /** matches reports whether a route is this entry, rather than a screen below it. */
    matches: (pathname: string) => pathname === "/dashboard",
  },
  {
    href: "/dashboard/routines",
    label: "Rutinitas",
    icon: "M12 3a9 9 0 1 0 9 9M12 7v5l4 2M16 3h5v5",
    matches: (pathname: string) => pathname === "/dashboard/routines",
  },
  {
    href: "/dashboard/usage",
    label: "Pemakaian",
    icon: "M4 19h16M7 16V9M12 16V5M17 16v-4",
    matches: (pathname: string) => pathname === "/dashboard/usage",
  },
  {
    href: "/office",
    label: "Kantor",
    icon: "M3 10.5 12 4l9 6.5M5 10v10h14V10M10 20v-5h4v5",
    matches: (pathname: string) => pathname.startsWith("/office"),
  },
];

/** One sidebar row for a Bolu: it opens that Bolu's chat. */
function RosterRow({ item }: { item: RosterItem }) {
  return (
    <Link
      href={`/chat?agent=${item.agent.id}`}
      className="flex min-h-[60px] min-w-0 flex-[1_1_220px] items-center gap-3 rounded-2xl border-2 border-transparent p-[8px_10px] text-left text-bolu-ink no-underline hover:text-bolu-ink"
    >
      <div className="relative size-11 flex-none">
        <Bolu
          member={crewFor(item.agent)}
          expression={expressionOf(stateOf(item.agent))}
          color={item.agent.color || undefined}
          size={44}
        />
        <span
          className="absolute -right-px bottom-px size-[11px] rounded-full border-2 border-white"
          style={{ backgroundColor: item.dot }}
        />
      </div>
      <div className="flex min-w-0 flex-1 flex-col text-left">
        <span className="font-display text-[17px] font-semibold leading-[1.2]">{item.agent.name}</span>
        <span className="truncate text-[13px] text-bolu-muted">{item.line}</span>
      </div>
      {item.waiting > 0 ? (
        <span className="box-border inline-flex h-[22px] min-w-[22px] items-center justify-center rounded-full bg-bolu-ink px-1.5 text-[12px] font-extrabold text-white">
          {item.waiting}
        </span>
      ) : null}
    </Link>
  );
}

/**
 * WorkspaceShell is the frame: a mobile bar, the sidebar, and the active screen.
 *
 * Which entry is lit comes from the route rather than from a prop each screen
 * would have to remember to pass.
 */
export function WorkspaceShell({ children }: { children: React.ReactNode }) {
  const { drafts, teams, hub, ready, error } = useWorkspace();
  const [navOpen, setNavOpen] = useState(false);
  const pathname = usePathname();

  const roster = teams[0]?.agents ?? [];
  const otherTeams = teams.slice(1);
  const pending = countPending(drafts);
  const connected = hub.connections.length;

  const onIntegrations = pathname.startsWith("/dashboard/integrations");
  const onSettings = pathname.startsWith("/settings");
  const screenTitle =
    NAV.find((item) => item.matches(pathname))?.label ??
    (onIntegrations ? "Integrasi" : onSettings ? "Pengaturan" : "Kantor Tim Bolu");

  return (
    <div className="flex min-h-screen flex-wrap items-stretch bg-bolu-bg text-bolu-ink">
      {/* Mobile bar: the design hides the sidebar below 760px and puts the
          current screen's name here instead. */}
      <div className="sticky top-0 z-30 flex flex-[1_1_100%] items-center gap-2.5 border-b border-bolu-border bg-white px-3 py-2 min-[761px]:hidden">
        <button
          type="button"
          aria-label="Buka menu"
          aria-expanded={navOpen}
          onClick={() => setNavOpen(true)}
          className="relative flex size-11 cursor-pointer items-center justify-center rounded-[14px] border-0 bg-bolu-bg"
        >
          <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="#1E1B2E" strokeWidth="2.5" strokeLinecap="round" aria-hidden="true">
            <path d="M4 7h16M4 12h16M4 17h16" />
          </svg>
          {pending > 0 ? (
            <span className="absolute right-2 top-2 size-2.5 rounded-full border-2 border-bolu-bg bg-bolu-flag" />
          ) : null}
        </button>
        <Bolu member={LOGO} size={32} />
        <div className="min-w-0 flex-1 truncate font-display text-[19px] font-semibold">
          {screenTitle}
        </div>
      </div>

      {navOpen ? (
        <div
          onClick={() => setNavOpen(false)}
          className="fixed inset-0 z-[35] bg-[rgba(30,27,46,0.4)] min-[761px]:hidden"
        />
      ) : null}

      <nav
        aria-label="Menu"
        className={`box-border flex w-[270px] flex-none flex-col gap-1.5 border-r border-bolu-border bg-white p-[20px_14px] max-[760px]:fixed max-[760px]:inset-y-0 max-[760px]:left-0 max-[760px]:z-40 max-[760px]:w-[min(320px,86vw)] max-[760px]:overflow-y-auto max-[760px]:transition-transform max-[760px]:duration-200 ${
          navOpen
            ? "max-[760px]:[transform:translateX(0)] max-[760px]:shadow-[0_10px_40px_rgba(30,27,46,0.25)]"
            : "max-[760px]:[transform:translateX(-105%)]"
        }`}
      >
        <div className="flex items-center justify-between gap-2 pb-2.5">
          <Link href="/" className="flex items-center gap-2.5 px-2 text-bolu-ink no-underline hover:text-bolu-ink">
            <Bolu member={LOGO} size={36} />
            <span className="font-display text-2xl font-bold">bolu</span>
          </Link>
          <button
            type="button"
            onClick={() => setNavOpen(false)}
            aria-label="Tutup menu"
            className="hidden size-11 flex-none cursor-pointer items-center justify-center rounded-full border-0 bg-bolu-bg max-[760px]:flex"
          >
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="#1E1B2E" strokeWidth="2.5" strokeLinecap="round" aria-hidden="true">
              <path d="M6 6l12 12M18 6L6 18" />
            </svg>
          </button>
        </div>

        <div className="flex flex-col gap-1">
          {NAV.map((item) => {
            const on = item.matches(pathname);
            return (
              <Link
                key={item.href}
                href={item.href}
                aria-current={on ? "page" : undefined}
                onClick={() => setNavOpen(false)}
                className={`flex min-h-[52px] items-center gap-3 rounded-2xl px-3.5 text-[17px] font-bold no-underline hover:text-bolu-ink ${
                  on ? "bg-bolu-ink text-white hover:text-white" : "bg-transparent text-bolu-ink"
                }`}
              >
                <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                  <path d={item.icon} />
                </svg>
                <span className="flex-1 text-left">{item.label}</span>
                {item.href === "/dashboard" && pending > 0 ? (
                  <span className="rounded-full bg-bolu-flag px-2 py-px text-[13px] font-bold text-white">
                    {pending}
                  </span>
                ) : null}
              </Link>
            );
          })}
        </div>

        {roster.length > 0 ? (
          <>
            <div className="flex items-center justify-between px-2.5 pb-0.5 pt-4 text-[13px] font-bold text-bolu-muted">
              <span>{teams[0]?.name ?? "Tim Bolu"}</span>
              <span className="font-medium">{roster.length} aktif</span>
            </div>
            <div className="flex flex-wrap gap-1">
              {buildRoster(roster, drafts).map((item) => (
                <RosterRow key={item.agent.id} item={item} />
              ))}
            </div>
          </>
        ) : null}

        {otherTeams.map((team) => (
          <div key={team.id}>
            <div className="px-2.5 pb-0.5 pt-4 text-[13px] font-bold text-bolu-muted">{team.name}</div>
            <div className="flex flex-wrap gap-1">
              {buildRoster(team.agents, drafts).map((item) => (
                <RosterRow key={item.agent.id} item={item} />
              ))}
            </div>
          </div>
        ))}

        {ready && roster.length === 0 ? (
          <p className="px-2.5 py-3 text-[13px] text-bolu-muted">
            Belum ada Bolu di tim ini.
          </p>
        ) : null}

        <Link
          href="/settings/team"
          className="mt-3 flex min-h-[46px] items-center gap-2.5 rounded-2xl border-2 border-dashed border-bolu-track px-3.5 font-semibold text-bolu-body no-underline hover:text-bolu-ink"
        >
          <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" aria-hidden="true">
            <path d="M12 5v14M5 12h14" />
          </svg>
          Buat tim atau grup
        </Link>

        <div className="min-h-3 flex-1" />

        <div className="flex flex-col gap-1 border-t border-bolu-line pt-2.5">
          <Link
            href="/dashboard/integrations"
            onClick={() => setNavOpen(false)}
            className={`flex min-h-[52px] items-center gap-3 rounded-2xl px-3.5 text-[17px] font-bold no-underline hover:text-bolu-ink ${
              onIntegrations
                ? "bg-bolu-ink text-white hover:text-white"
                : "bg-transparent text-bolu-ink"
            }`}
          >
            <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
              <path d="M9 7V3M15 7V3M7 7h10v4a5 5 0 0 1-10 0zM12 16v5" />
            </svg>
            <span className="flex-1 text-left">Integrasi</span>
            <span className={`text-[13px] font-bold ${onIntegrations ? "text-white" : "text-bolu-muted"}`}>
              {connected === 0 ? "—" : `${connected} aktif`}
            </span>
          </Link>
          <Link
            href="/settings"
            onClick={() => setNavOpen(false)}
            className={`flex min-h-[52px] items-center gap-3 rounded-2xl px-3.5 text-[17px] font-bold no-underline hover:text-bolu-ink ${
              onSettings ? "bg-bolu-ink text-white hover:text-white" : "bg-transparent text-bolu-ink"
            }`}
          >
            <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
              <circle cx="12" cy="12" r="3" />
              <path d="M12 2.5v3M12 18.5v3M2.5 12h3M18.5 12h3M5.3 5.3l2.1 2.1M16.6 16.6l2.1 2.1M5.3 18.7l2.1-2.1M16.6 7.4l2.1-2.1" />
            </svg>
            <span className="flex-1 text-left">Pengaturan</span>
          </Link>
        </div>
      </nav>

      <div className="box-border flex min-w-0 flex-[999_1_640px] flex-col">
        {/* A failed load is said here rather than inside one screen: the reason is
            the same for every screen, and a screen that only shows its own empty
            state would read as "nothing here" instead of "nothing loaded". */}
        {error ? (
          <p
            role="alert"
            className="m-[18px_20px_0] rounded-2xl border border-bolu-belum/30 bg-bolu-belum/5 px-4 py-3 text-[15px] text-bolu-belum"
          >
            {error}
          </p>
        ) : null}
        {children}
      </div>
    </div>
  );
}
