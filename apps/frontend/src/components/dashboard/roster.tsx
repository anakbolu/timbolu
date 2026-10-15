"use client";

/**
 * The sidebar's Bolu roster: one section per team, draggable rows, and a header
 * that folds a team away.
 *
 * Three decisions are worth reading rather than inferring:
 *
 *   1. **A row is a box with a link and a handle, not a link with a handle.** The
 *      handle is a button, and a button inside an anchor is not markup the browser
 *      will parse — the row would either lose the button or lose the link. So the
 *      box carries the row's look and the link carries the face, the name, and the
 *      badge.
 *   2. **Dragging is pointer events, not the HTML5 drag API.** The sidebar is a
 *      drawer on a phone, and the HTML5 API does not fire on touch at all. Pointer
 *      capture also means the drop target is computed from the pointer's own
 *      position rather than from whatever element the browser decides is under it.
 *   3. **The drop target is a slot, not a row.** `index` counts the list *without*
 *      the Bolu being moved, which is what both a pointer and an arrow key can
 *      compute, and the slot is drawn by colouring the 2px border the row already
 *      has — so the indicator costs no layout and nothing jumps as the pointer
 *      moves.
 *
 * The order itself is the workspace's: this component sends the layout and renders
 * what comes back, so two tabs and a reload all agree.
 */

import { ChevronDown, GripVertical } from "lucide-react";
import Link from "next/link";
import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from "react";

import { Bolu } from "@/components/bolu/bolu";
import type { Agent } from "@/lib/api";
import { buildRoster, crewFor, expressionOf, stateOf, type RosterItem } from "@/lib/dashboard-view";

import { useWorkspace } from "./workspace-data";

/**
 * The teams a person folded away, per workspace.
 *
 * It is the browser's own state rather than the workspace's: which sections are
 * open is about this screen, not about the team, and storing it server-side would
 * let one person's folding hide a colleague's roster.
 *
 * It is read as an external store rather than through an effect, because the server
 * has no localStorage: a `useState` that read it would hydrate into a different
 * sidebar than the one that was sent, and an effect that set it after the fact
 * would render the wrong sidebar first. `useSyncExternalStore` is the one that
 * answers the server with "nothing is folded" and the browser with what it stored.
 */
const COLLAPSED_KEY = "bolu.roster.collapsed";
const FOLDING_EVENT = "bolu:roster-folding";
const NOTHING_FOLDED: string[] = [];

/** The last raw value parsed, so a snapshot is the same array until the store changes. */
let foldedKey = "";
let foldedRaw = "";
let foldedValue: string[] = NOTHING_FOLDED;

function parseFolding(raw: string): string[] {
  if (!raw) return NOTHING_FOLDED;
  try {
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return NOTHING_FOLDED;
    const teams = parsed.filter((entry): entry is string => typeof entry === "string");
    return teams.length === 0 ? NOTHING_FOLDED : teams;
  } catch {
    // A key somebody edited by hand is not worth an error screen: the roster opens
    // every section instead.
    return NOTHING_FOLDED;
  }
}

/** foldingOf is the folded teams of one workspace, cached by the raw value. */
function foldingOf(workspaceID: string): string[] {
  const key = `${COLLAPSED_KEY}:${workspaceID}`;
  let raw = "";
  try {
    raw = window.localStorage.getItem(key) ?? "";
  } catch {
    raw = "";
  }

  if (key !== foldedKey || raw !== foldedRaw) {
    foldedKey = key;
    foldedRaw = raw;
    foldedValue = parseFolding(raw);
  }
  return foldedValue;
}

/** subscribeFolding follows the store, including a fold made in another tab. */
function subscribeFolding(onChange: () => void): () => void {
  window.addEventListener("storage", onChange);
  window.addEventListener(FOLDING_EVENT, onChange);
  return () => {
    window.removeEventListener("storage", onChange);
    window.removeEventListener(FOLDING_EVENT, onChange);
  };
}

/** writeFolding stores the folded teams and wakes every reader of the store. */
function writeFolding(workspaceID: string, teams: string[]): void {
  try {
    window.localStorage.setItem(`${COLLAPSED_KEY}:${workspaceID}`, JSON.stringify(teams));
  } catch {
    // A browser that refuses storage still folds: the sections close for this
    // session and the next load opens them again, which is better than a click
    // that does nothing.
  }
  window.dispatchEvent(new Event(FOLDING_EVENT));
}

/** The slot a Bolu would land in: a team, and a position inside it. */
type DropTarget = { teamId: string; index: number };

/** sectionTarget is the team whose section is nearest the pointer. */
function sectionTarget(sections: Map<string, HTMLElement>, clientY: number): string | null {
  let chosen: { teamId: string; distance: number } | null = null;

  for (const [teamId, section] of sections) {
    const rect = section.getBoundingClientRect();
    const distance = clientY < rect.top ? rect.top - clientY : clientY > rect.bottom ? clientY - rect.bottom : 0;
    if (!chosen || distance < chosen.distance) {
      chosen = { teamId, distance };
    }
  }

  return chosen?.teamId ?? null;
}

/**
 * slotIn is the position a pointer sits at inside one team's list.
 *
 * The row being moved is skipped: it is still on screen, and counting it would
 * offer the slot that means "where it already is" as a separate place. A section
 * with no rows left is one slot, which is what makes an emptied team a target.
 */
function slotIn(list: HTMLDivElement | undefined, agentId: string, clientY: number): number {
  if (!list) return 0;

  let index = 0;
  for (const row of list.querySelectorAll<HTMLElement>("[data-roster-row]")) {
    if (row.dataset.rosterRow === agentId) continue;
    const rect = row.getBoundingClientRect();
    if (rect.top + rect.height / 2 < clientY) index++;
  }
  return index;
}

/**
 * slotFor is the same computation for a keyboard: one step up or down the sidebar,
 * read as a slot.
 *
 * The sequence is the whole roster flattened rather than one team, because that is
 * what the sidebar reads as, and a keyboard that could not leave its own section
 * would make moving between teams a mouse-only act. A position exactly on a team
 * boundary is the start of the next team, and a position past the last row is the
 * end of the last team.
 */
function slotFor(teams: { id: string; agents: Agent[] }[], agent: Agent, step: number): DropTarget | null {
  const flat = teams.flatMap((team) => team.agents);
  const at = flat.findIndex((entry) => entry.id === agent.id);
  const to = at + step;
  if (at < 0 || to < 0 || to >= flat.length) return null;

  // The slot is counted in the lists without the Bolu being moved, which is the
  // same convention `slotIn` uses for a pointer: both answer "where among the rows
  // that stay".
  let remaining = to;
  for (const team of teams) {
    const rows = team.agents.filter((entry) => entry.id !== agent.id).length;
    if (remaining < rows) return { teamId: team.id, index: remaining };
    remaining -= rows;
  }

  return null;
}

/**
 * RosterRow is one Bolu: the face, the name, and — for a managing role — the handle
 * that moves it.
 *
 * `dropBefore` and `dropAfter` are the drop indicator. They colour a border the row
 * already carries, so marking a slot cannot move the row under the pointer.
 */
function RosterRow({
  item,
  draggable,
  dragging,
  dropBefore,
  dropAfter,
  onGrab,
  onMove,
  onDrop,
  onKeyMove,
}: {
  item: RosterItem;
  draggable: boolean;
  dragging: boolean;
  dropBefore: boolean;
  dropAfter: boolean;
  onGrab: (event: React.PointerEvent<HTMLButtonElement>) => void;
  onMove: (event: React.PointerEvent<HTMLButtonElement>) => void;
  onDrop: (event: React.PointerEvent<HTMLButtonElement>) => void;
  onKeyMove: (event: React.KeyboardEvent<HTMLButtonElement>) => void;
}) {
  const edge = dropBefore ? "border-t-bolu-ink" : dropAfter ? "border-b-bolu-ink" : "border-transparent";

  return (
    <div
      data-roster-row={item.agent.id}
      className={`group flex min-h-[46px] min-w-0 items-center gap-1 rounded-md border-2 ${edge} p-[5px_8px] text-bolu-ink ${
        dragging ? "opacity-40" : ""
      }`}
    >
      <Link
        href={`/chat?agent=${item.agent.id}`}
        className="flex min-w-0 flex-1 items-center gap-2.5 text-left text-bolu-ink no-underline hover:text-bolu-ink"
      >
        <div className="relative size-9 flex-none">
          <Bolu
            member={crewFor(item.agent)}
            expression={expressionOf(stateOf(item.agent))}
            color={item.agent.color || undefined}
            size={36}
          />
          <span
            className="absolute -right-px bottom-px size-[10px] rounded-full border-2 border-white"
            style={{ backgroundColor: item.dot }}
          />
        </div>
        <div className="flex min-w-0 flex-1 flex-col text-left">
          <span className="truncate font-display text-[15px] font-semibold leading-[1.2]">
            {item.agent.name}
          </span>
          <span className="truncate text-[12px] text-bolu-muted">{item.line}</span>
        </div>
        {item.waiting > 0 ? (
          <span className="box-border inline-flex h-[18px] min-w-[18px] items-center justify-center rounded-full bg-bolu-ink px-1.5 text-[11px] font-extrabold text-white">
            {item.waiting}
          </span>
        ) : null}
      </Link>

      {draggable ? (
        <button
          type="button"
          // The label names the Bolu because the handle is where a keyboard lands,
          // and "Ubah urutan" alone would not say which row it moves.
          aria-label={`Ubah urutan ${item.agent.name}`}
          onPointerDown={onGrab}
          onPointerMove={onMove}
          onPointerUp={onDrop}
          onPointerCancel={onDrop}
          onKeyDown={onKeyMove}
          className="flex size-7 flex-none cursor-grab items-center justify-center rounded border-0 bg-transparent text-bolu-muted opacity-0 transition-opacity group-hover:opacity-100 focus:opacity-100 active:cursor-grabbing [@media(hover:none)]:opacity-100 [touch-action:none]"
        >
          <GripVertical size={16} strokeWidth={2.5} aria-hidden="true" />
        </button>
      ) : null}
    </div>
  );
}

/**
 * BoluRoster is every team's section of the sidebar.
 *
 * A section is rendered even when it has no Bolu: the header is how a team is
 * folded and unfolded, and a team that vanished with its last Bolu could not be
 * unfolded again — nor be a place to drop one into.
 */
export function BoluRoster() {
  const { teams, drafts, ready, user, members, workspaceID, moveAgent } = useWorkspace();

  const [drag, setDrag] = useState<{ agentId: string; teamId: string } | null>(null);
  const [drop, setDrop] = useState<DropTarget | null>(null);
  const [announcement, setAnnouncement] = useState("");

  const sections = useRef(new Map<string, HTMLElement>());
  const lists = useRef(new Map<string, HTMLDivElement>());
  /** The Bolu whose handle must be focused again after the next render. */
  const pendingFocus = useRef<string | null>(null);

  // A move re-renders the row — into another section, or into another place in
  // this one — and a new element cannot keep the old element's focus. Without this
  // the second press of an arrow key would go nowhere, which would make the
  // keyboard able to move a Bolu exactly once.
  useEffect(() => {
    const id = pendingFocus.current;
    if (!id) return;
    pendingFocus.current = null;
    document.querySelector<HTMLButtonElement>(`[data-roster-row="${id}"] button`)?.focus();
  });

  const foldedKey = workspaceID ?? "";
  const collapsed = useSyncExternalStore(
    subscribeFolding,
    () => foldingOf(foldedKey),
    () => NOTHING_FOLDED,
  );

  // Arranging the sidebar is a managing act, the same one that adds or removes a
  // Bolu. Every member still reads the roster.
  const role = members.find((member) => member.user_id === user?.id)?.role;
  const canArrange = role === "owner" || role === "admin";

  const toggle = useCallback(
    (teamId: string) => {
      writeFolding(
        foldedKey,
        collapsed.includes(teamId) ? collapsed.filter((entry) => entry !== teamId) : [...collapsed, teamId],
      );
    },
    [collapsed, foldedKey],
  );

  const grab = useCallback(
    (agent: Agent) => (event: React.PointerEvent<HTMLButtonElement>) => {
      event.preventDefault();
      event.currentTarget.setPointerCapture(event.pointerId);
      setDrag({ agentId: agent.id, teamId: agent.team_id });
      // The Bolu's stored position is its slot in the list without it, so a drag
      // that never moves is already expressed and needs no special case at the end.
      setDrop({ teamId: agent.team_id, index: agent.position });
    },
    [],
  );

  const move = useCallback(
    (event: React.PointerEvent<HTMLButtonElement>) => {
      if (!drag) return;
      const teamId = sectionTarget(sections.current, event.clientY);
      if (!teamId) return;
      setDrop({ teamId, index: slotIn(lists.current.get(teamId), drag.agentId, event.clientY) });
    },
    [drag],
  );

  const commit = useCallback(
    (agent: Agent, target: DropTarget) => {
      const team = teams.find((entry) => entry.id === target.teamId);
      void moveAgent(agent.id, target.teamId, target.index).then((outcome) => {
        // An empty message is the store saying the drop changed nothing, which is not
        // worth announcing: "Oren is now in Tim Bolu, position 3" read out over a row
        // that never moved is worse than silence.
        if (!outcome.message) return;
        setAnnouncement(
          outcome.ok
            ? `${agent.name} sekarang di ${team?.name ?? "tim lain"}, urutan ${target.index + 1}.`
            : outcome.message,
        );
      });

      // A Bolu dropped into a folded team is meant to be seen, so the section it
      // landed in opens.
      if (collapsed.includes(target.teamId)) {
        writeFolding(
          foldedKey,
          collapsed.filter((entry) => entry !== target.teamId),
        );
      }
    },
    [collapsed, foldedKey, moveAgent, teams],
  );

  const release = useCallback(() => {
    const agent = drag ? teams.flatMap((team) => team.agents).find((entry) => entry.id === drag.agentId) : null;
    // A drag that ended where it started is the same list back from `moveAgent`,
    // which the store reads as "nothing to write" — so this only has to hand it the
    // slot the pointer chose.
    if (agent && drop) commit(agent, drop);

    setDrag(null);
    setDrop(null);
  }, [commit, drag, drop, teams]);

  /**
   * keyMove is the same move without a pointer: one step up or down the roster.
   *
   * The handle is a real button, so it is reachable by Tab and the arrows work
   * where focus already is — which is the whole reason the handle is a button
   * rather than a `draggable` div.
   */
  const keyMove = useCallback(
    (agent: Agent) => (event: React.KeyboardEvent<HTMLButtonElement>) => {
      if (event.key !== "ArrowUp" && event.key !== "ArrowDown") return;
      event.preventDefault();

      const target = slotFor(teams, agent, event.key === "ArrowUp" ? -1 : 1);
      if (target) {
        pendingFocus.current = agent.id;
        commit(agent, target);
      }
    },
    [commit, teams],
  );

  return (
    <>
      {teams.map((team) => {
        const folded = collapsed.includes(team.id);
        const roster = buildRoster(team.agents, drafts);
        const onTarget = drop?.teamId === team.id;

        return (
          <section
            key={team.id}
            ref={(node) => {
              if (node) sections.current.set(team.id, node);
              else sections.current.delete(team.id);
            }}
            className="flex flex-col"
          >
            <button
              type="button"
              aria-expanded={!folded}
              onClick={() => toggle(team.id)}
              className={`flex cursor-pointer items-center gap-1 rounded border-2 px-2 pb-0.5 pt-3 text-[12px] font-bold text-bolu-muted hover:text-bolu-ink ${
                folded && onTarget ? "border-dashed border-bolu-ink" : "border-transparent"
              }`}
            >
              <span className="flex-1 text-left">{team.name}</span>
              <span className="font-medium">{roster.length} aktif</span>
              <ChevronDown
                size={14}
                strokeWidth={2.5}
                aria-hidden="true"
                className={`transition-transform ${folded ? "-rotate-90" : ""}`}
              />
            </button>

            {folded ? null : (
              <div
                ref={(node) => {
                  if (node) lists.current.set(team.id, node);
                  else lists.current.delete(team.id);
                }}
                className="flex flex-col gap-1"
              >
                {roster.length === 0 ? (
                  <p
                    className={`px-2 py-2 text-[12px] text-bolu-muted ${
                      onTarget ? "rounded-md border-2 border-dashed border-bolu-ink" : ""
                    }`}
                  >
                    {ready ? "Belum ada Bolu di tim ini." : "Memuat…"}
                  </p>
                ) : null}

                {roster.map((item, position) => (
                  <RosterRow
                    key={item.agent.id}
                    item={item}
                    draggable={canArrange}
                    dragging={drag?.agentId === item.agent.id}
                    dropBefore={onTarget && drop.index === position && drag?.agentId !== item.agent.id}
                    dropAfter={onTarget && position === roster.length - 1 && drop.index === roster.length}
                    onGrab={grab(item.agent)}
                    onMove={move}
                    onDrop={release}
                    onKeyMove={keyMove(item.agent)}
                  />
                ))}
              </div>
            )}
          </section>
        );
      })}

      <p aria-live="polite" className="sr-only">
        {announcement}
      </p>
    </>
  );
}
