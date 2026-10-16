"use client";

/**
 * The conversation history: every thread the workspace has, newest first.
 *
 * The list is the same read the sidebar's group rows and the thread header use —
 * `conversations` from the workspace store — so a thread that answered a moment
 * ago moves to the top of this panel and the sidebar at the same instant, and
 * neither can disagree about which Bolu is in a room.
 *
 * Below 760px the panel is a drawer rather than a column: the thread is what a
 * phone is for, and the history is a step away behind the header's button. The
 * drawer is opened and closed by clicks only — no effect writes the state — so
 * the layout never moves under a render the reader did not ask for.
 */

import { X } from "lucide-react";
import Link from "next/link";
import { useMemo } from "react";

import { Bolu } from "@/components/bolu/bolu";
import { GroupIcon } from "@/components/bolu/group-icon";
import type { Agent, ChatConversation, ChatParticipant } from "@/lib/api";
import { allBot } from "@/lib/crew";
import { crewFor, expressionOf, relativeAge, stateOf } from "@/lib/dashboard-view";

/**
 * ConversationList is the panel left of the thread.
 *
 * `onNavigate` is called by every row: a drawer that stayed open over the thread
 * a person just chose would hide the thing they chose.
 */
export function ConversationList({
  conversations,
  agents,
  activeId,
  open,
  onClose,
  onNavigate,
}: {
  conversations: ChatConversation[];
  agents: Agent[];
  /** The open conversation, which is the row drawn as current. */
  activeId?: string;
  /** Whether the drawer is open. Ignored at 761px and up, where the panel is a column. */
  open: boolean;
  onClose: () => void;
  onNavigate: () => void;
}) {
  // The store's list already arrives newest first, but the panel is what a person
  // reads as a timeline, so the order is stated here rather than assumed.
  const ordered = useMemo(
    () =>
      [...conversations].sort(
        (left, right) => Date.parse(right.last_activity_at) - Date.parse(left.last_activity_at),
      ),
    [conversations],
  );

  return (
    <>
      {open ? (
        <div
          onClick={onClose}
          className="fixed inset-0 z-[35] bg-[rgba(30,27,46,0.4)] min-[761px]:hidden"
        />
      ) : null}

      <nav
        aria-label="Riwayat obrolan"
        className={`box-border flex w-[260px] flex-none flex-col overflow-y-auto border-r border-bolu-border bg-white p-[14px_12px] max-[760px]:fixed max-[760px]:inset-y-0 max-[760px]:left-0 max-[760px]:z-40 max-[760px]:w-[min(300px,86vw)] max-[760px]:transition-transform max-[760px]:duration-200 ${
          open
            ? "max-[760px]:[transform:translateX(0)] max-[760px]:shadow-[0_10px_40px_rgba(30,27,46,0.25)]"
            : "max-[760px]:[transform:translateX(-105%)]"
        }`}
      >
        <div className="flex items-center justify-between gap-2 px-2 pb-2">
          <h2 className="font-display text-[15px] font-bold">Riwayat Obrolan</h2>
          <button
            type="button"
            onClick={onClose}
            aria-label="Tutup riwayat"
            className="hidden size-9 flex-none cursor-pointer items-center justify-center rounded-full border-0 bg-bolu-bg max-[760px]:flex"
          >
            <X size={16} strokeWidth={2.5} aria-hidden="true" />
          </button>
        </div>

        {ordered.length === 0 ? (
          <p className="px-2 py-2 text-[13px] text-bolu-muted">
            Belum ada obrolan. Pilih Bolu di menu, lalu tulis sesuatu untuk memulai.
          </p>
        ) : (
          <div className="flex flex-col gap-0.5">
            {ordered.map((conversation) =>
              conversation.kind === "group" ? (
                <GroupRow
                  key={conversation.id}
                  conversation={conversation}
                  agents={agents}
                  active={conversation.id === activeId}
                  onNavigate={onNavigate}
                />
              ) : (
                <DirectRow
                  key={conversation.id}
                  conversation={conversation}
                  agents={agents}
                  active={conversation.id === activeId}
                  onNavigate={onNavigate}
                />
              ),
            )}
          </div>
        )}
      </nav>
    </>
  );
}

/** DirectRow is one 1:1 thread: the Bolu's face, its name, and its role. */
function DirectRow({
  conversation,
  agents,
  active,
  onNavigate,
}: {
  conversation: ChatConversation;
  agents: Agent[];
  active: boolean;
  onNavigate: () => void;
}) {
  const participant = conversation.participants.find((item) => item.is_agent);
  const agent = agents.find((item) => item.id === participant?.agent_id) ?? null;
  const name = participant?.name ?? agent?.name ?? conversation.title ?? "Bolu";

  // A direct thread is opened by the Bolu it is with, which is the same link the
  // sidebar writes, so the two open the same conversation. A participant whose
  // Bolu id is missing — a Bolu since deleted — can only be reached by the
  // thread's own id, and a row that cannot be clicked is worse than a fallback.
  const href = participant?.agent_id
    ? `/chat?agent=${participant.agent_id}`
    : `/chat?conversation=${conversation.id}`;

  return (
    <Link
      href={href}
      onClick={onNavigate}
      aria-current={active ? "true" : undefined}
      className={`flex min-h-[52px] items-center gap-2.5 rounded-md border-2 border-transparent p-[5px_8px] text-left text-bolu-ink no-underline hover:text-bolu-ink ${
        active ? "border-bolu-accent/40 bg-bolu-panel/70" : "hover:bg-bolu-panel/60"
      }`}
    >
      <Bolu
        member={agent ? crewFor(agent) : allBot("Oren")}
        expression={expressionOf(agent ? stateOf(agent) : "idle")}
        color={agent?.color || undefined}
        size={36}
        className="flex-none"
      />
      <div className="flex min-w-0 flex-1 flex-col">
        <span className="truncate font-display text-[15px] font-semibold leading-[1.2]">{name}</span>
        <span className="truncate text-[12px] text-bolu-muted">
          {participant?.role || agent?.role || "Bolu"} · {relativeAge(conversation.last_activity_at)}
        </span>
      </div>
    </Link>
  );
}

/** GroupRow is one group: its members' faces, its title, and how many Bolu it holds. */
function GroupRow({
  conversation,
  agents,
  active,
  onNavigate,
}: {
  conversation: ChatConversation;
  agents: Agent[];
  active: boolean;
  onNavigate: () => void;
}) {
  const members: ChatParticipant[] = conversation.participants.filter((item) => item.is_agent);

  return (
    <Link
      href={`/chat?conversation=${conversation.id}`}
      onClick={onNavigate}
      aria-current={active ? "true" : undefined}
      className={`flex min-h-[52px] items-center gap-2.5 rounded-md border-2 border-transparent p-[5px_8px] text-left text-bolu-ink no-underline hover:text-bolu-ink ${
        active ? "border-bolu-accent/40 bg-bolu-panel/70" : "hover:bg-bolu-panel/60"
      }`}
    >
      <GroupIcon participants={conversation.participants} agents={agents} size={36} className="flex-none" />
      <div className="flex min-w-0 flex-1 flex-col">
        <span className="truncate font-display text-[15px] font-semibold leading-[1.2]">
          {conversation.title || "Grup"}
        </span>
        <span className="truncate text-[12px] text-bolu-muted">
          Grup · {members.length} Bolu
        </span>
      </div>
      <span className="shrink-0 rounded-full bg-bolu-panel px-2 py-px text-[11px] font-bold text-bolu-muted">
        Grup
      </span>
    </Link>
  );
}
