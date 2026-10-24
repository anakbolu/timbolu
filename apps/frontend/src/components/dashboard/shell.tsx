"use client";

/**
 * The dashboard shell: the frame every screen lives in.
 *
 * The sidebar is the internal panel design's (`timbolu_admin_panel.html`): 256px,
 * a shade off the page, a compact nav, the roster of every team, the recent
 * conversations, and the account at the foot. Navigation is by route rather than
 * by in-page state, so a screen keeps its own URL and the browser's back button
 * does what a person expects.
 */

import {
  Bot,
  ChevronDown,
  Clock,
  LayoutGrid,
  ListChecks,
  Menu,
  MessageCircle,
  Plus,
  Plug,
  Search,
  Settings,
  ShieldCheck,
  Users,
  X,
} from "lucide-react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { useState } from "react";

import { Bolu } from "@/components/bolu/bolu";
import { GroupIcon } from "@/components/bolu/group-icon";
import { ThemeToggle } from "@/components/ui/theme-toggle";
import type { Agent, ChatConversation } from "@/lib/api";
import { LOGO } from "@/lib/crew";
import { countPending, crewFor, relativeAge } from "@/lib/dashboard-view";

import { BoluRoster } from "./roster";
import { useWorkspace } from "./workspace-data";

/**
 * The nav entries, in the panel design's order. The design's own entries are the
 * product's screens under their own names: Dasbor where it says Projects, Tugas
 * where it says Artifacts, Rutinitas where it says Scheduled.
 */
const NAV = [
  {
    href: "/dashboard",
    label: "Dasbor",
    icon: LayoutGrid,
    /** matches reports whether a route is this entry, rather than a screen below it. */
    matches: (pathname: string) => pathname === "/dashboard",
  },
  {
    href: "/tasks",
    label: "Tugas",
    icon: ListChecks,
    // The detail page is below this entry rather than beside it, so the nav stays lit while a
    // person reads one task — the history is where they came from.
    matches: (pathname: string) => pathname === "/tasks" || pathname.startsWith("/tasks/"),
  },
  {
    href: "/routines",
    label: "Rutinitas",
    icon: Clock,
    matches: (pathname: string) => pathname === "/routines",
  },
  {
    href: "/dashboard/integrations",
    label: "Integrasi",
    icon: Plug,
    matches: (pathname: string) => pathname.startsWith("/dashboard/integrations"),
  },
  {
    href: "/settings",
    label: "Pengaturan",
    icon: Settings,
    matches: (pathname: string) => pathname.startsWith("/settings"),
  },
];

/** The settings screens that carry a name of their own in the mobile bar. */
const SETTINGS_TITLES: Record<string, string> = {
  "/settings/usage": "Pemakaian",
};

/** How many conversations the Terbaru section lists before it stops. */
const RECENT_LIMIT = 8;

/** The class every compact sidebar row shares, lit or not. */
function rowClass(on: boolean): string {
  return `flex min-h-[32px] items-center gap-3 rounded-md px-2 text-[14px] no-underline ${
    on
      ? "bg-bolu-hover font-semibold text-bolu-ink hover:text-bolu-ink"
      : "text-bolu-body hover:bg-bolu-hover/70 hover:text-bolu-ink"
  }`;
}

/**
 * RecentRow is one conversation in Terbaru: a direct one by its Bolu's face, a
 * group by its members'. A thread is not listed here — it lives in the panel of
 * the conversation it was opened from.
 */
function RecentRow({
  conversation,
  agents,
  onNavigate,
}: {
  conversation: ChatConversation;
  agents: Agent[];
  onNavigate: () => void;
}) {
  const participant = conversation.participants.find((item) => item.is_agent);
  const agent = agents.find((item) => item.id === participant?.agent_id) ?? null;
  const group = conversation.kind === "group";
  const name = group ? conversation.title || "Grup" : (participant?.name ?? agent?.name ?? "Bolu");
  // A direct thread is opened by its Bolu, which is the link the roster writes, so
  // the two open the same conversation.
  const href =
    !group && participant?.agent_id
      ? `/chat?agent=${participant.agent_id}`
      : `/chat?conversation=${conversation.id}`;

  return (
    <Link href={href} onClick={onNavigate} className={rowClass(false)} title={relativeAge(conversation.last_activity_at)}>
      {group ? (
        <GroupIcon participants={conversation.participants} agents={agents} size={20} className="flex-none" />
      ) : (
        <Bolu
          member={agent ? crewFor(agent) : LOGO}
          color={agent?.color || undefined}
          size={20}
          className="flex-none"
        />
      )}
      <span className="min-w-0 flex-1 truncate">{name}</span>
    </Link>
  );
}

/**
 * WorkspaceShell is the frame: a mobile bar, the sidebar, and the active screen.
 *
 * The sidebar follows the panel design: the mark and two quick actions on top, a
 * compact nav, the Bolu of every team, the conversations a person came from, and
 * the account at the foot. Which entry is lit comes from the route rather than
 * from a prop each screen would have to remember to pass.
 */
export function WorkspaceShell({ children }: { children: React.ReactNode }) {
  const { conversations, drafts, agents, error, user } = useWorkspace();
  const [navOpen, setNavOpen] = useState(false);
  const [addOpen, setAddOpen] = useState(false);
  const [recentOpen, setRecentOpen] = useState(true);
  const pathname = usePathname();

  const pending = countPending(drafts);
  // Threads belong to the conversation they were opened from, so the sidebar lists
  // the rooms and leaves the work inside them to the chat's own panel.
  const recents = conversations
    .filter((conversation) => conversation.kind !== "thread")
    .sort((left, right) => Date.parse(right.last_activity_at) - Date.parse(left.last_activity_at))
    .slice(0, RECENT_LIMIT);

  const screenTitle =
    NAV.find((item) => item.matches(pathname))?.label === "Pengaturan"
      ? (SETTINGS_TITLES[pathname] ?? "Pengaturan")
      : (NAV.find((item) => item.matches(pathname))?.label ?? "Kantor Tim Bolu");

  const close = () => setNavOpen(false);
  // The account has no display name in the store, so the email is what a person
  // recognises as their own, and its first letter is the avatar.
  const email = user?.email ?? "";
  const initial = email ? email.charAt(0).toUpperCase() : "?";

  return (
    <div className="flex h-dvh flex-col overflow-hidden bg-bolu-bg text-bolu-ink min-[761px]:flex-row">
      {/* Mobile bar: the sidebar is a drawer below 760px, and the current screen's
          name stands here instead. */}
      <div className="z-30 flex flex-none items-center gap-2.5 border-b border-bolu-border bg-bolu-surface px-3 py-2 min-[761px]:hidden">
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
        <div onClick={close} className="fixed inset-0 z-[35] bg-bolu-scrim min-[761px]:hidden" />
      ) : null}

      <nav
        aria-label="Menu"
        className={`box-border flex w-64 flex-none flex-col border-r border-bolu-border bg-bolu-sidebar max-[760px]:fixed max-[760px]:inset-y-0 max-[760px]:left-0 max-[760px]:z-40 max-[760px]:w-[min(300px,86vw)] max-[760px]:transition-transform max-[760px]:duration-200 ${
          navOpen
            ? "max-[760px]:[transform:translateX(0)] max-[760px]:shadow-[0_10px_40px_rgba(30,27,46,0.25)]"
            : "max-[760px]:[transform:translateX(-105%)]"
        }`}
      >
        {/* The mark is the product's own Bolu rather than the design's cake: the crew
            is who the product is, wherever it appears. */}
        <div className="flex h-14 flex-none items-center justify-between gap-2 border-b border-bolu-line px-4">
          <Link href="/" className="flex items-center gap-2 text-bolu-ink no-underline hover:text-bolu-ink">
            <span className="flex size-8 items-center justify-center rounded-md bg-bolu-amber-soft">
              <Bolu member={LOGO} size={24} />
            </span>
            <span className="font-display text-base font-semibold tracking-tight">Timbolu</span>
          </Link>
          <div className="flex gap-1">
            {/* There is no workspace search yet; the button holds the design's place
                and says so rather than opening nothing. */}
            <button
              type="button"
              disabled
              aria-label="Cari (segera hadir)"
              title="Cari — segera hadir"
              className="flex size-7 cursor-not-allowed items-center justify-center rounded-md border-0 bg-transparent text-bolu-muted opacity-60"
            >
              <Search size={16} strokeWidth={2} aria-hidden="true" />
            </button>
            <Link
              href="/bolu/new"
              onClick={close}
              aria-label="Tambah Bolu"
              className="flex size-7 items-center justify-center rounded-md text-bolu-muted no-underline hover:bg-bolu-hover hover:text-bolu-ink"
            >
              <Plus size={16} strokeWidth={2} aria-hidden="true" />
            </Link>
            <button
              type="button"
              onClick={close}
              aria-label="Tutup menu"
              className="hidden size-7 flex-none cursor-pointer items-center justify-center rounded-md border-0 bg-transparent text-bolu-muted max-[760px]:flex"
            >
              <X size={16} strokeWidth={2.5} aria-hidden="true" />
            </button>
          </div>
        </div>

        <div className="flex min-h-0 flex-1 flex-col gap-5 overflow-y-auto p-3">
          <div className="flex flex-col gap-0.5">
            {/* One entry rather than three: making a Bolu is the everyday act, and
                making a team or a group are the rare ones, so the menu opens on the
                common case. */}
            <button
              type="button"
              aria-expanded={addOpen}
              aria-controls="tambah-bolu-menu"
              onClick={() => setAddOpen((open) => !open)}
              className={`${rowClass(false)} cursor-pointer border-0 bg-transparent text-left`}
            >
              <Plus size={16} strokeWidth={2} aria-hidden="true" className="text-bolu-muted" />
              <span className="flex-1">Baru</span>
              <ChevronDown
                size={14}
                strokeWidth={2.5}
                aria-hidden="true"
                className={`text-bolu-muted transition-transform ${addOpen ? "rotate-180" : ""}`}
              />
            </button>
            {addOpen ? (
              <div id="tambah-bolu-menu" className="ml-7 flex flex-col gap-0.5 border-l border-bolu-line pl-2">
                {[
                  { href: "/bolu/new", label: "Tambah Bolu", icon: Bot },
                  { href: "/settings/team", label: "Buat tim", icon: Users },
                  { href: "/groups", label: "Buat grup", icon: MessageCircle },
                ].map((entry) => (
                  <Link
                    key={entry.href}
                    href={entry.href}
                    onClick={() => {
                      setAddOpen(false);
                      close();
                    }}
                    className={rowClass(false)}
                  >
                    <entry.icon size={15} strokeWidth={2} aria-hidden="true" className="text-bolu-muted" />
                    <span className="flex-1">{entry.label}</span>
                  </Link>
                ))}
              </div>
            ) : null}

            {NAV.map((item) => {
              const on = item.matches(pathname);
              return (
                <Link
                  key={item.href}
                  href={item.href}
                  aria-current={on ? "page" : undefined}
                  onClick={close}
                  className={rowClass(on)}
                >
                  <item.icon
                    size={16}
                    strokeWidth={2}
                    aria-hidden="true"
                    className={on ? "text-bolu-ink" : "text-bolu-muted"}
                  />
                  <span className="flex-1">{item.label}</span>
                  {item.href === "/dashboard" && pending > 0 ? (
                    <span className="rounded-full bg-bolu-flag px-1.5 text-[11px] font-bold text-bolu-solid-ink">
                      {pending}
                    </span>
                  ) : null}
                </Link>
              );
            })}

            {/* The operator surface is offered only to an account the API says
                carries the platform role. It is not a guard — /admin checks the
                role again on every request — but a link nobody can use is noise on
                the sidebar of everybody else. */}
            {user?.role === "admin" ? (
              <Link href="/admin/dashboard" onClick={close} className={rowClass(false)}>
                <ShieldCheck size={16} strokeWidth={2} aria-hidden="true" className="text-bolu-muted" />
                <span className="flex-1">Admin</span>
                <span className="text-[11px] text-bolu-muted">platform</span>
              </Link>
            ) : null}
          </div>

          <BoluRoster />

          <section className="flex flex-col">
            <button
              type="button"
              aria-expanded={recentOpen}
              onClick={() => setRecentOpen((open) => !open)}
              className="mb-1 flex cursor-pointer items-center justify-between border-0 bg-transparent px-2 text-[12px] font-semibold text-bolu-muted hover:text-bolu-body"
            >
              <span>Terbaru</span>
              <ChevronDown
                size={12}
                strokeWidth={2.5}
                aria-hidden="true"
                className={`transition-transform ${recentOpen ? "" : "-rotate-90"}`}
              />
            </button>
            {recentOpen ? (
              recents.length === 0 ? (
                <p className="px-2 text-[12px] text-bolu-muted">Belum ada obrolan.</p>
              ) : (
                <div className="flex flex-col gap-0.5">
                  {recents.map((conversation) => (
                    <RecentRow key={conversation.id} conversation={conversation} agents={agents} onNavigate={close} />
                  ))}
                </div>
              )
            ) : null}
          </section>
        </div>

        {/* The account at the foot, as in the design. The theme sits beside it: it
            is a preference a person changes once and then forgets, so it should be
            visible without being in the way. */}
        <div className="flex h-14 flex-none items-center justify-between gap-2 border-t border-bolu-border px-3">
          <Link
            href="/settings"
            onClick={close}
            className="flex min-w-0 flex-1 items-center gap-2 text-bolu-body no-underline hover:text-bolu-ink"
          >
            <span className="flex size-7 flex-none items-center justify-center rounded-full bg-bolu-accent text-[12px] font-bold text-bolu-solid-ink shadow-sm">
              {initial}
            </span>
            <span className="min-w-0 truncate text-[12px] font-medium">{email || "Akunmu"}</span>
          </Link>
          <ThemeToggle />
        </div>
      </nav>

      <div className="box-border flex min-h-0 min-w-0 flex-1 flex-col overflow-y-auto">
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
