"use client";

/**
 * The dashboard shell: the frame every screen lives in.
 *
 * The sidebar is the design's, values and all — 270px, white, the roster of both
 * teams, and Integrasi/Pengaturan pinned to the bottom. Navigation is by route
 * rather than by in-page state, so a screen keeps its own URL and the browser's
 * back button does what a person expects; the visual result is identical.
 */

import {
  Bot,
  ChartColumn,
  ChevronDown,
  Clock,
  LayoutGrid,
  ListChecks,
  Menu,
  Plus,
  Plug,
  Settings,
  Users,
  X,
} from "lucide-react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { useState } from "react";

import { Bolu } from "@/components/bolu/bolu";
import type { ChatConversation } from "@/lib/api";
import { LOGO } from "@/lib/crew";
import {
  buildRoster,
  countPending,
  crewFor,
  expressionOf,
  relativeAge,
  stateOf,
  type RosterItem,
} from "@/lib/dashboard-view";

import { useWorkspace } from "./workspace-data";

/** The nav entries, in the design's order. */
const NAV = [
  {
    href: "/dashboard",
    label: "Dasbor",
    icon: LayoutGrid,
    /** matches reports whether a route is this entry, rather than a screen below it. */
    matches: (pathname: string) => pathname === "/dashboard",
  },
  {
    href: "/dashboard/routines",
    label: "Rutinitas",
    icon: Clock,
    matches: (pathname: string) => pathname === "/dashboard/routines",
  },
  {
    href: "/dashboard/usage",
    label: "Pemakaian",
    icon: ChartColumn,
    matches: (pathname: string) => pathname === "/dashboard/usage",
  },
  {
    href: "/tasks",
    label: "Tugas",
    icon: ListChecks,
    // The detail page is below this entry rather than beside it, so the nav stays lit while a
    // person reads one task — the history is where they came from.
    matches: (pathname: string) => pathname === "/tasks" || pathname.startsWith("/tasks/"),
  },
];

/** One sidebar row for a Bolu: it opens that Bolu's chat. */
function RosterRow({ item }: { item: RosterItem }) {
  return (
    <Link
      href={`/chat?agent=${item.agent.id}`}
      className="flex min-h-[60px] min-w-0 flex-[1_1_220px] items-center gap-3 rounded-md border-2 border-transparent p-[8px_10px] text-left text-bolu-ink no-underline hover:text-bolu-ink"
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

/** One sidebar row for a group: it opens that group's thread. */
function GroupRow({ conversation }: { conversation: ChatConversation }) {
  const members = conversation.participants.filter((participant) => participant.is_agent).length;
  return (
    <Link
      href={`/chat?conversation=${conversation.id}`}
      className="flex min-h-[60px] min-w-0 flex-[1_1_220px] items-center gap-3 rounded-md border-2 border-transparent p-[8px_10px] text-left text-bolu-ink no-underline hover:text-bolu-ink"
    >
      <div className="flex size-11 flex-none items-center justify-center rounded-md bg-bolu-panel text-bolu-body">
        <Users size={20} strokeWidth={2} aria-hidden="true" />
      </div>
      <div className="flex min-w-0 flex-1 flex-col text-left">
        <span className="font-display text-[17px] font-semibold leading-[1.2]">
          {conversation.title || "Grup"}
        </span>
        <span className="truncate text-[13px] text-bolu-muted">
          {members} Bolu · {relativeAge(conversation.last_activity_at)}
        </span>
      </div>
      <span className="shrink-0 text-[12px] text-bolu-muted">{conversation.message_count}</span>
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
  const { conversations, drafts, teams, hub, ready, error } = useWorkspace();
  const [navOpen, setNavOpen] = useState(false);
  const [addOpen, setAddOpen] = useState(false);
  const pathname = usePathname();

  const roster = teams[0]?.agents ?? [];
  const groups = conversations.filter((conversation) => conversation.kind === "group");
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
          className="relative flex size-11 cursor-pointer items-center justify-center rounded-md border-0 bg-bolu-bg"
        >
          <Menu size={20} strokeWidth={2.5} aria-hidden="true" />
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
            <X size={18} strokeWidth={2.5} aria-hidden="true" />
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
                className={`flex min-h-[52px] items-center gap-3 rounded-md px-3.5 text-[17px] font-bold no-underline hover:text-bolu-ink ${
                  on ? "bg-bolu-ink text-white hover:text-white" : "bg-transparent text-bolu-ink"
                }`}
              >
                <item.icon size={22} strokeWidth={2} aria-hidden="true" />
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

        {groups.length > 0 ? (
          <>
            <div className="flex items-center justify-between px-2.5 pb-0.5 pt-4 text-[13px] font-bold text-bolu-muted">
              <span>Grup</span>
              <span className="font-medium">{groups.length}</span>
            </div>
            <div className="flex flex-wrap gap-1">
              {groups.map((conversation) => (
                <GroupRow key={conversation.id} conversation={conversation} />
              ))}
            </div>
          </>
        ) : null}

        <Link
          href="/groups"
          onClick={() => setNavOpen(false)}
          className="mt-3 flex min-h-[46px] items-center gap-2.5 rounded-md border-2 border-dashed border-bolu-track px-3.5 font-semibold text-bolu-body no-underline hover:text-bolu-ink"
        >
          <Plus size={20} strokeWidth={2.5} aria-hidden="true" />
          Buat grup
        </Link>

        {/* One button rather than two links: making a Bolu is the everyday act and
            making a team is the rare one, so the menu opens on the common case. */}
        <div className="mt-3 flex flex-col gap-1">
          <button
            type="button"
            aria-expanded={addOpen}
            aria-controls="tambah-bolu-menu"
            onClick={() => setAddOpen((open) => !open)}
            className="flex min-h-[46px] cursor-pointer items-center gap-2.5 rounded-md border-2 border-dashed border-bolu-track bg-transparent px-3.5 font-semibold text-bolu-body hover:text-bolu-ink"
          >
            <Plus size={20} strokeWidth={2.5} aria-hidden="true" />
            <span className="flex-1 text-left">Tambah Bolu</span>
            <ChevronDown
              size={18}
              strokeWidth={2.5}
              aria-hidden="true"
              className={`transition-transform ${addOpen ? "rotate-180" : ""}`}
            />
          </button>

          {addOpen ? (
            <div
              id="tambah-bolu-menu"
              className="flex flex-col gap-1 rounded-md border border-bolu-line p-1"
            >
              <Link
                href="/bolu/new"
                onClick={() => {
                  setAddOpen(false);
                  setNavOpen(false);
                }}
                className="flex min-h-[44px] items-center gap-2.5 rounded-md px-3 font-semibold text-bolu-body no-underline hover:bg-bolu-panel/60 hover:text-bolu-ink"
              >
                <Bot size={18} strokeWidth={2} aria-hidden="true" />
                <span className="flex-1 text-left">Tambah Bolu</span>
              </Link>
              <Link
                href="/settings/team"
                onClick={() => {
                  setAddOpen(false);
                  setNavOpen(false);
                }}
                className="flex min-h-[44px] items-center gap-2.5 rounded-md px-3 font-semibold text-bolu-body no-underline hover:bg-bolu-panel/60 hover:text-bolu-ink"
              >
                <Users size={18} strokeWidth={2} aria-hidden="true" />
                <span className="flex-1 text-left">Buat tim</span>
              </Link>
            </div>
          ) : null}
        </div>

        <div className="min-h-3 flex-1" />

        <div className="flex flex-col gap-1 border-t border-bolu-line pt-2.5">
          <Link
            href="/dashboard/integrations"
            onClick={() => setNavOpen(false)}
            className={`flex min-h-[52px] items-center gap-3 rounded-md px-3.5 text-[17px] font-bold no-underline hover:text-bolu-ink ${
              onIntegrations
                ? "bg-bolu-ink text-white hover:text-white"
                : "bg-transparent text-bolu-ink"
            }`}
          >
            <Plug size={22} strokeWidth={2} aria-hidden="true" />
            <span className="flex-1 text-left">Integrasi</span>
            <span className={`text-[13px] font-bold ${onIntegrations ? "text-white" : "text-bolu-muted"}`}>
              {connected === 0 ? "—" : `${connected} aktif`}
            </span>
          </Link>
          <Link
            href="/settings"
            onClick={() => setNavOpen(false)}
            className={`flex min-h-[52px] items-center gap-3 rounded-md px-3.5 text-[17px] font-bold no-underline hover:text-bolu-ink ${
              onSettings ? "bg-bolu-ink text-white hover:text-white" : "bg-transparent text-bolu-ink"
            }`}
          >
            <Settings size={22} strokeWidth={2} aria-hidden="true" />
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
            className="m-[18px_20px_0] rounded-md border border-bolu-belum/30 bg-bolu-belum/5 px-4 py-3 text-[15px] text-bolu-belum"
          >
            {error}
          </p>
        ) : null}
        {children}
      </div>
    </div>
  );
}
