"use client";

/**
 * The group screen: who is in the room, and who answered.
 *
 * A group is where the router is visible. The screen shows the participant list
 * and the last routing decision, so "why did Biru answer and not Oren" has an
 * answer on screen rather than in a log.
 */

import { useCallback, useEffect, useState } from "react";

import { BotSvg } from "@/components/bolu/bot-svg";
import {
  Agent,
  ChatConversation,
  addGroupParticipants,
  createGroup,
  ensureActiveWorkspace,
  listAgentTeams,
  listConversations,
  refreshSession,
} from "@/lib/api";
import { bot } from "@/lib/crew";

export function GroupView() {
  const [agents, setAgents] = useState<Agent[]>([]);
  const [groups, setGroups] = useState<ChatConversation[]>([]);
  const [selected, setSelected] = useState<string[]>([]);
  const [title, setTitle] = useState("");
  const [workspaceId, setWorkspaceId] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const load = useCallback(async () => {
    const session = await refreshSession();
    if (!session.ok) {
      setError("Sesi berakhir. Masuk lagi.");
      return;
    }

    // Resolved from the session rather than only read from storage, so an account
    // that owns a workspace is not told to pick one it cannot see.
    const active = await ensureActiveWorkspace();
    setWorkspaceId(active);
    if (!active) {
      setError("Pilih workspace dulu.");
      return;
    }

    const [teams, conversations] = await Promise.all([
      listAgentTeams(active),
      listConversations(active),
    ]);

    if (!teams.ok) {
      setError(teams.message);
      return;
    }
    setAgents(teams.data.flatMap((team) => team.agents));
    if (conversations.ok) {
      setGroups(conversations.data.filter((conversation) => conversation.kind === "group"));
    }
    setError(null);
  }, []);

  useEffect(() => {
    void (async () => {
      await load();
    })();
  }, [load]);

  async function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!workspaceId || selected.length === 0) {
      return;
    }

    setBusy(true);
    const result = await createGroup(workspaceId, {
      title,
      agent_ids: selected,
      user_ids: [],
    });
    setBusy(false);

    if (!result.ok) {
      setError(result.message);
      return;
    }

    setNotice(`Grup ${result.data.title} dibuat dengan ${result.data.participants.length} peserta.`);
    setTitle("");
    setSelected([]);
    await load();
  }

  async function addToGroup(group: ChatConversation, agentId: string) {
    if (!workspaceId) {
      return;
    }

    setBusy(true);
    const result = await addGroupParticipants(workspaceId, group.id, { agent_ids: [agentId] });
    setBusy(false);

    if (!result.ok) {
      setError(result.message);
      return;
    }
    const added = agents.find((agent) => agent.id === agentId);
    setNotice(`${added?.name ?? "Bolu"} ditambahkan ke ${group.title || "grup"}.`);
    await load();
  }

  return (
    <main className="mx-auto w-full max-w-5xl px-5 py-10">
      <header className="mb-8">
        <h1 className="font-display text-3xl font-semibold">Grup Bolu</h1>
        <p className="mt-2 text-bolu-body">
          Di grup, satu pesan menghasilkan paling banyak satu balasan: router memilih Bolu yang paling
          cocok dari perannya, dan pilihannya dicatat supaya jejaknya jelas.
        </p>
      </header>

      {error ? (
        <p role="alert" className="mb-4 rounded-xl border border-bolu-belum/30 bg-bolu-belum/8 px-3 py-2 text-sm text-bolu-belum">
          {error}
        </p>
      ) : null}
      {notice ? (
        <p role="status" className="mb-4 rounded-xl border border-bolu-lunas/30 bg-bolu-lunas/8 px-3 py-2 text-sm text-bolu-lunas">
          {notice}
        </p>
      ) : null}

      <div className="grid gap-8 lg:grid-cols-[1fr_360px]">
        <section>
          <h2 className="mb-3 font-display text-xl font-semibold">Grup yang ada</h2>
          {groups.length === 0 ? (
            <p className="rounded-2xl border border-dashed border-bolu-border px-4 py-6 text-sm text-bolu-muted">
              Belum ada grup. Buat satu di kanan.
            </p>
          ) : (
            <ul className="space-y-2">
              {groups.map((group) => (
                <li key={group.id} className="rounded-2xl border border-bolu-border bg-bolu-surface px-4 py-3">
                  <div className="flex flex-wrap items-baseline justify-between gap-2">
                    <p className="font-medium">{group.title || "Grup tanpa nama"}</p>
                    <span className="text-sm text-bolu-muted">{group.message_count} pesan</span>
                  </div>
                  <ul className="mt-2 flex flex-wrap gap-2">
                    {group.participants
                      .filter((participant) => participant.is_agent)
                      .map((participant) => (
                        <li
                          key={participant.id}
                          className="inline-flex items-center gap-1.5 rounded-full bg-bolu-panel px-2.5 py-1 text-xs"
                        >
                          <BotSvg bot={bot(participant.name)} className="size-4" />
                          {participant.name} · {participant.role}
                        </li>
                      ))}
                  </ul>

                  <details className="mt-2">
                    <summary className="cursor-pointer text-sm text-bolu-accent">
                      Tambah Bolu ke grup ini
                    </summary>
                    <div className="mt-2 flex flex-wrap gap-1.5">
                      {agents
                        .filter(
                          (agent) =>
                            !group.participants.some((participant) => participant.agent_id === agent.id),
                        )
                        .map((agent) => (
                          <button
                            key={agent.id}
                            type="button"
                            disabled={busy}
                            onClick={() => void addToGroup(group, agent.id)}
                            className="rounded-full border border-bolu-border px-2.5 py-1 text-xs transition hover:bg-bolu-panel/60 disabled:opacity-50"
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

        <section>
          <h2 className="mb-3 font-display text-xl font-semibold">Buat grup</h2>
          <form onSubmit={submit} className="space-y-3 rounded-2xl border border-bolu-border bg-bolu-surface px-4 py-4">
            <label className="block text-sm">
              <span className="mb-1 block text-bolu-muted">Nama grup</span>
              <input
                value={title}
                onChange={(event) => setTitle(event.target.value)}
                required
                placeholder="mis. Grup operasional"
                className="w-full rounded-xl border border-bolu-border bg-bolu-surface px-3 py-2"
              />
            </label>

            <fieldset className="text-sm">
              <legend className="mb-1 text-bolu-muted">Bolu yang ikut</legend>
              <div className="space-y-1">
                {agents.map((agent) => (
                  <label key={agent.id} className="flex items-center gap-2">
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
              className="rounded-xl bg-bolu-accent px-4 py-2 text-sm font-medium text-white transition hover:opacity-90 disabled:opacity-50"
            >
              {busy ? "Membuat…" : "Buat grup"}
            </button>
          </form>
        </section>
      </div>
    </main>
  );
}
