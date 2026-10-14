"use client";

/**
 * The group screen: who is in the room, and who answered.
 *
 * A group is where the router is visible. The screen shows the participant list
 * and the last routing decision, so "why did Biru answer and not Oren" has an
 * answer on screen rather than in a log.
 *
 * The roster and the group list come from the workspace store, so creating a
 * group here is visible in the sidebar and in the dashboard at the same moment.
 */

import { useState } from "react";

import { Bolu } from "@/components/bolu/bolu";
import { addGroupParticipants, createGroup, type ChatConversation } from "@/lib/api";
import { allBot } from "@/lib/crew";
import { crewFor, expressionOf, stateOf } from "@/lib/dashboard-view";

import { useWorkspace } from "@/components/dashboard/workspace-data";

/** GroupView is the groups screen. */
export function GroupView() {
  const store = useWorkspace();
  const { agents, conversations, workspaceID } = store;

  const [selected, setSelected] = useState<string[]>([]);
  const [title, setTitle] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const groups = conversations.filter((conversation) => conversation.kind === "group");

  async function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!workspaceID || selected.length === 0) {
      return;
    }

    setBusy(true);
    const result = await createGroup(workspaceID, { title, agent_ids: selected, user_ids: [] });
    setBusy(false);

    if (!result.ok) {
      setError(result.message);
      return;
    }

    setError(null);
    setNotice(
      `Grup ${result.data.title} dibuat dengan ${result.data.participants.length} peserta.`,
    );
    setTitle("");
    setSelected([]);
    await store.reloadConversations();
  }

  async function addToGroup(group: ChatConversation, agentId: string) {
    if (!workspaceID) {
      return;
    }

    setBusy(true);
    const result = await addGroupParticipants(workspaceID, group.id, { agent_ids: [agentId] });
    setBusy(false);

    if (!result.ok) {
      setError(result.message);
      return;
    }
    const added = agents.find((agent) => agent.id === agentId);
    setError(null);
    setNotice(`${added?.name ?? "Bolu"} ditambahkan ke ${group.title || "grup"}.`);
    await store.reloadConversations();
  }

  return (
    <div className="flex flex-col gap-[22px] p-[24px_28px_48px] max-[760px]:p-[18px_14px_36px]">
      <header>
        <h1 className="font-display text-[36px] font-bold leading-[1.1] max-[760px]:text-[28px]">
          Grup Bolu
        </h1>
        <p className="text-bolu-muted">
          Di grup, satu pesan menghasilkan paling banyak satu balasan: router memilih Bolu yang
          paling cocok dari perannya, dan pilihannya dicatat supaya jejaknya jelas.
        </p>
      </header>

      {error ? (
        <p
          role="alert"
          className="rounded-md border border-bolu-belum/30 bg-bolu-belum/5 px-4 py-3 text-[15px] text-bolu-belum"
        >
          {error}
        </p>
      ) : null}
      {notice ? (
        <p
          role="status"
          className="rounded-md border border-bolu-lunas/30 bg-bolu-lunas/5 px-4 py-3 text-[15px] text-bolu-lunas"
        >
          {notice}
        </p>
      ) : null}

      <div className="grid gap-[18px] lg:grid-cols-[1fr_360px]">
        <section className="flex flex-col gap-2.5 rounded-md border border-bolu-border bg-white p-5">
          <h2 className="font-display text-[22px] font-semibold">Grup yang ada</h2>
          {groups.length === 0 ? (
            <p className="rounded-md border border-dashed border-bolu-border px-4 py-6 text-[15px] text-bolu-muted">
              Belum ada grup. Buat satu di kanan.
            </p>
          ) : (
            <ul className="flex flex-col gap-2">
              {groups.map((group) => (
                <li
                  key={group.id}
                  className="rounded-md border border-bolu-line px-4 py-3"
                >
                  <div className="flex flex-wrap items-baseline justify-between gap-2">
                    <p className="font-semibold">{group.title || "Grup tanpa nama"}</p>
                    <span className="text-[13px] text-bolu-muted">{group.message_count} pesan</span>
                  </div>
                  <ul className="mt-2 flex flex-wrap gap-2">
                    {group.participants
                      .filter((participant) => participant.is_agent)
                      .map((participant) => {
                        const agent = agents.find((item) => item.id === participant.agent_id);
                        return (
                          <li
                            key={participant.id}
                            className="inline-flex items-center gap-1.5 rounded-full bg-bolu-panel px-2.5 py-1 text-[12px]"
                          >
                            <Bolu
                              member={agent ? crewFor(agent) : allBot("Oren")}
                              expression={expressionOf(agent ? stateOf(agent) : "idle")}
                              color={agent?.color || undefined}
                              size={16}
                              still
                            />
                            {participant.name} · {participant.role}
                          </li>
                        );
                      })}
                  </ul>

                  <details className="mt-2">
                    <summary className="cursor-pointer text-[14px] text-bolu-accent">
                      Tambah Bolu ke grup ini
                    </summary>
                    <div className="mt-2 flex flex-wrap gap-1.5">
                      {agents
                        .filter(
                          (agent) =>
                            !group.participants.some(
                              (participant) => participant.agent_id === agent.id,
                            ),
                        )
                        .map((agent) => (
                          <button
                            key={agent.id}
                            type="button"
                            disabled={busy}
                            onClick={() => void addToGroup(group, agent.id)}
                            className="cursor-pointer rounded-full border border-bolu-border px-2.5 py-1 text-[12px] disabled:opacity-50"
                          >
                            {agent.name}
                          </button>
                        ))}
                    </div>
                  </details>
                </li>
              ))}
            </ul>
          )}
        </section>

        <section className="flex flex-col gap-2.5 rounded-md border border-bolu-border bg-white p-5">
          <h2 className="font-display text-[22px] font-semibold">Buat grup</h2>
          {agents.length === 0 ? (
            <p className="text-[15px] text-bolu-muted">
              Belum ada Bolu untuk dimasukkan ke grup. Buat satu dulu di halaman Tim.
            </p>
          ) : (
            <form onSubmit={submit} className="flex flex-col gap-3">
              <label className="block text-[14px] font-semibold">
                <span className="mb-1.5 block">Nama grup</span>
                <input
                  value={title}
                  onChange={(event) => setTitle(event.target.value)}
                  required
                  placeholder="mis. Grup operasional"
                  className="min-h-11 w-full rounded-md border border-bolu-border bg-white px-3.5 text-[15px] font-normal outline-none focus:border-bolu-accent"
                />
              </label>

              <fieldset className="text-[14px]">
                <legend className="mb-1.5 font-semibold">Bolu yang ikut</legend>
                <div className="flex flex-col gap-1.5">
                  {agents.map((agent) => (
                    <label key={agent.id} className="flex items-center gap-2.5 font-normal">
                      <input
                        type="checkbox"
                        checked={selected.includes(agent.id)}
                        onChange={(event) =>
                          setSelected((previous) =>
                            event.target.checked
                              ? [...previous, agent.id]
                              : previous.filter((id) => id !== agent.id),
                          )
                        }
                      />
                      <Bolu
                        member={crewFor(agent)}
                        expression={expressionOf(stateOf(agent))}
                        color={agent.color || undefined}
                        size={24}
                      />
                      <span>
                        {agent.name} <span className="text-bolu-muted">· {agent.role}</span>
                      </span>
                    </label>
                  ))}
                </div>
              </fieldset>

              <button
                type="submit"
                disabled={busy || selected.length === 0 || title.trim() === ""}
                className="min-h-11 cursor-pointer rounded-full bg-bolu-ink px-5 font-bold text-white disabled:opacity-50"
              >
                {busy ? "Membuat…" : "Buat grup"}
              </button>
            </form>
          )}
        </section>
      </div>
    </div>
  );
}
