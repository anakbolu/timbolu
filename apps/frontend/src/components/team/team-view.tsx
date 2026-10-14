"use client";

/**
 * The team screen: Tim Bolu and Tim Hore, each with its Bolu.
 *
 * A Bolu can be renamed, re-persona'd, put to rest, given tool access, or
 * created from a template or by copying one — none of which needs a deploy,
 * which is the point of the registry.
 */

import Link from "next/link";
import { useCallback, useEffect, useState } from "react";

import { Alert } from "@/components/auth/auth-shell";
import {
  ApiResult,
  Agent,
  Grant,
  Integration,
  AgentTeam,
  Tool,
  createAgent,
  deleteAgent,
  ensureActiveWorkspace,
  listAgentGrants,
  listAgentTools,
  listIntegrations,
  listAgentTeams,
  listTemplates,
  setAgentStatus,
  setAgentGrant,
  updateAgent,
  AgentTemplate,
} from "@/lib/api";

const STATUS_LABEL: Record<string, string> = {
  working: "Bekerja",
  waiting: "Menunggu kamu",
  idle: "Santai",
  resting: "Istirahat",
};

const STATUS_CLASS: Record<string, string> = {
  working: "bg-bolu-live/12 text-bolu-lunas",
  waiting: "bg-bolu-flag/12 text-bolu-flag",
  idle: "bg-bolu-panel text-bolu-muted",
  resting: "bg-bolu-panel text-bolu-rest",
};

export function TeamView() {
  const [teams, setTeams] = useState<AgentTeam[]>([]);
  const [templates, setTemplates] = useState<AgentTemplate[]>([]);
  const [integrations, setIntegrations] = useState<Integration[]>([]);
  const [selected, setSelected] = useState<Agent | null>(null);
  const [grants, setGrants] = useState<Grant[]>([]);
  const [tools, setTools] = useState<Tool[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [workspaceId, setWorkspaceId] = useState<string | null>(null);

  const load = useCallback(async () => {
    // The workspace is resolved through `ensureActiveWorkspace`, which refreshes
    // the session itself. Refreshing first and then resolving was two uses of one
    // single-use refresh cookie, so the second call raced the first and the screen
    // could be told the session had ended while it was still valid.
    const active = await ensureActiveWorkspace();
    setWorkspaceId(active);
    if (!active) {
      setError("Pilih workspace dulu.");
      return;
    }

    const [teamResult, templateResult, integrationResult] = await Promise.all([
      listAgentTeams(active),
      listTemplates(),
      listIntegrations(active),
    ]);

    if (!teamResult.ok) {
      setError(teamResult.message);
      return;
    }
    setTeams(teamResult.data);
    if (templateResult.ok) {
      setTemplates(templateResult.data);
    }
    if (integrationResult.ok) {
      setIntegrations(integrationResult.data);
    }
  }, []);

  useEffect(() => {
    // The loader sets state after its first await, so nothing is set
    // synchronously from the effect body.
    void (async () => {
      await load();
    })();
  }, [load]);

  async function open(agent: Agent) {
    setSelected(agent);
    setNotice(null);
    setError(null);

    if (!workspaceId) {
      return;
    }

    const [grantResult, toolResult] = await Promise.all([
      listAgentGrants(workspaceId, agent.id),
      listAgentTools(workspaceId, agent.id),
    ]);
    setGrants(grantResult.ok ? grantResult.data : []);
    setTools(toolResult.ok ? toolResult.data : []);
  }

  // report turns an API failure into the page's error line.
  function report<T>(result: ApiResult<T>): result is Extract<ApiResult<T>, { ok: true }> {
    if (result.ok) {
      return true;
    }
    setError(result.message);
    return false;
  }

  async function saveProfile(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!selected || !workspaceId) {
      return;
    }

    const form = new FormData(event.currentTarget);
    setBusy(true);
    const result = await updateAgent(workspaceId, selected.id, {
      name: String(form.get("name") ?? ""),
      role: String(form.get("role") ?? ""),
      persona: String(form.get("persona") ?? ""),
      tone: String(form.get("tone") ?? ""),
    });
    setBusy(false);

    if (!report(result)) {
      return;
    }
    setNotice("Profil tersimpan. Tugas berikutnya memakai persona ini.");
    await load();
    await open(result.data);
  }

  async function toggleRest(agent: Agent) {
    if (!workspaceId) {
      return;
    }

    const next = agent.status === "resting" ? "active" : "resting";
    setBusy(true);
    const result = await setAgentStatus(workspaceId, agent.id, next);
    setBusy(false);

    if (!report(result)) {
      return;
    }
    setNotice(next === "resting" ? "Bolu ini istirahat: tidak menerima tugas baru." : "Bolu ini aktif lagi.");
    await load();
    if (selected?.id === agent.id) {
      setSelected(result.data);
    }
  }

  async function remove(agent: Agent) {
    if (!workspaceId) {
      return;
    }

    setBusy(true);
    const result = await deleteAgent(workspaceId, agent.id);
    setBusy(false);

    if (!report(result)) {
      return;
    }
    setNotice(`${agent.name} dikeluarkan dari tim. Riwayat tugasnya tetap tersimpan.`);
    setSelected(null);
    await load();
  }

  async function addBolu(event: React.FormEvent<HTMLFormElement>, teamId: string) {
    event.preventDefault();
    if (!workspaceId) {
      return;
    }

    const form = new FormData(event.currentTarget);
    const templateKey = String(form.get("template_key") ?? "");
    setBusy(true);
    const result = await createAgent(workspaceId, {
      team_id: teamId,
      name: String(form.get("name") ?? ""),
      template_key: templateKey,
    });
    setBusy(false);

    if (!report(result)) {
      return;
    }
    setNotice(`${result.data.name} bergabung ke tim.`);
    event.currentTarget.reset();
    await load();
  }

  async function saveGrant(agent: Agent, integrationId: string, permission: string) {
    if (!workspaceId) {
      return;
    }

    setBusy(true);
    const result = await setAgentGrant(workspaceId, agent.id, integrationId, permission);
    setBusy(false);

    if (!report(result)) {
      return;
    }
    setNotice("Izin disimpan. Daftar alat Bolu ikut berubah.");
    await open(agent);
  }

  if (!workspaceId) {
    return (
      <main className="mx-auto w-full max-w-3xl px-5 py-12">
        <Alert tone="info">
          Belum ada workspace aktif. <Link href="/login">Masuk</Link> dulu.
        </Alert>
      </main>
    );
  }

  return (
    <main className="mx-auto w-full max-w-5xl px-5 py-10">
      <header className="mb-8">
        <p className="text-sm text-bolu-muted">
          <Link href="/dashboard">Dasbor</Link> / Tim
        </p>
        <h1 className="mt-2 font-display text-3xl font-semibold">Tim Bolu</h1>
        <p className="mt-2 text-bolu-body">
          Setiap Bolu punya persona, gaya bahasa, dan alat sendiri. Ubah profilnya kapan saja tanpa deploy.
        </p>
      </header>

      {error ? <Alert tone="error">{error}</Alert> : null}
      {notice ? <Alert tone="success">{notice}</Alert> : null}

      <div className="grid gap-8 lg:grid-cols-[1fr_360px]">
        <div className="space-y-8">
          {teams.map((team) => (
            <section key={team.id}>
              <div className="mb-3 flex items-baseline justify-between">
                <h2 className="font-display text-xl font-semibold">{team.name}</h2>
                <span className="text-sm text-bolu-muted">{team.agents.length} Bolu</span>
              </div>

              {team.agents.length === 0 ? (
                <p className="rounded-md border border-dashed border-bolu-border px-4 py-6 text-sm text-bolu-muted">
                  Belum ada Bolu di tim ini.
                </p>
              ) : (
                <ul className="space-y-2">
                  {team.agents.map((agent) => (
                    <li key={agent.id}>
                      <button
                        type="button"
                        onClick={() => void open(agent)}
                        className={`flex w-full items-center gap-3 rounded-md border px-4 py-3 text-left transition hover:bg-bolu-panel/50 ${
                          selected?.id === agent.id ? "border-bolu-accent bg-bolu-panel/50" : "border-bolu-border bg-bolu-surface"
                        }`}
                      >
                        <span
                          aria-hidden
                          className="inline-block size-8 shrink-0 rounded-full"
                          style={{ backgroundColor: agent.color || "#8b88a0" }}
                        />
                        <span className="min-w-0 flex-1">
                          <span className="block truncate font-medium">{agent.name}</span>
                          <span className="block truncate text-sm text-bolu-muted">{agent.role || "—"}</span>
                        </span>
                        <span
                          className={`shrink-0 rounded-full px-2.5 py-1 text-xs font-medium ${
                            STATUS_CLASS[agent.display_status] ?? STATUS_CLASS.idle
                          }`}
                        >
                          {STATUS_LABEL[agent.display_status] ?? agent.display_status}
                        </span>
                      </button>
                    </li>
                  ))}
                </ul>
              )}

              <form onSubmit={(event) => void addBolu(event, team.id)} className="mt-3 flex flex-wrap gap-2">
                <input
                  name="name"
                  placeholder="Nama Bolu baru"
                  required
                  className="min-w-[160px] flex-1 rounded-md border border-bolu-border px-3 py-2 text-sm"
                />
                <select name="template_key" className="rounded-md border border-bolu-border px-3 py-2 text-sm">
                  <option value="">Profil kosong</option>
                  {templates.map((template) => (
                    <option key={template.key} value={template.key}>
                      {template.name} — {template.role}
                    </option>
                  ))}
                </select>
                <button
                  type="submit"
                  disabled={busy}
                  className="rounded-md bg-bolu-accent px-3.5 py-2 text-sm font-semibold text-white disabled:opacity-60"
                >
                  Tambah
                </button>
              </form>
            </section>
          ))}
        </div>

        <aside>
          {selected ? (
            <div className="rounded-md border border-bolu-border bg-bolu-surface p-5">
              <div className="mb-4 flex items-start justify-between gap-3">
                <div>
                  <h2 className="font-display text-lg font-semibold">{selected.name}</h2>
                  <p className="text-sm text-bolu-muted">
                    {selected.team_name} · {STATUS_LABEL[selected.display_status] ?? selected.display_status}
                  </p>
                </div>
                <button
                  type="button"
                  onClick={() => setSelected(null)}
                  className="text-sm text-bolu-muted hover:text-bolu-ink"
                >
                  Tutup
                </button>
              </div>

              {selected.reason ? (
                <p className="mb-4 rounded-md bg-bolu-panel/60 px-3 py-2 text-sm text-bolu-body">
                  {selected.reason.kind === "task" ? "Sedang mengerjakan" : "Menunggu persetujuanmu"}:{" "}
                  {selected.reason.title || selected.reason.status}
                </p>
              ) : null}

              <form onSubmit={(event) => void saveProfile(event)}>
                <label className="mb-1 block text-sm font-medium" htmlFor="agent-name">
                  Nama
                </label>
                <input
                  id="agent-name"
                  name="name"
                  defaultValue={selected.name}
                  required
                  className="mb-3 w-full rounded-md border border-bolu-border px-3 py-2 text-sm"
                />

                <label className="mb-1 block text-sm font-medium" htmlFor="agent-role">
                  Peran
                </label>
                <input
                  id="agent-role"
                  name="role"
                  defaultValue={selected.role}
                  className="mb-3 w-full rounded-md border border-bolu-border px-3 py-2 text-sm"
                />

                <label className="mb-1 block text-sm font-medium" htmlFor="agent-tone">
                  Gaya bahasa
                </label>
                <input
                  id="agent-tone"
                  name="tone"
                  defaultValue={selected.tone}
                  className="mb-3 w-full rounded-md border border-bolu-border px-3 py-2 text-sm"
                />

                <label className="mb-1 block text-sm font-medium" htmlFor="agent-persona">
                  Persona
                </label>
                <textarea
                  id="agent-persona"
                  name="persona"
                  defaultValue={selected.persona}
                  rows={5}
                  className="mb-4 w-full rounded-md border border-bolu-border px-3 py-2 text-sm"
                />

                <button
                  type="submit"
                  disabled={busy}
                  className="w-full rounded-md bg-bolu-accent px-4 py-2 text-sm font-semibold text-white disabled:opacity-60"
                >
                  Simpan profil
                </button>
              </form>

              <div className="mt-4 flex gap-2">
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => void toggleRest(selected)}
                  className="flex-1 rounded-md border border-bolu-border px-3 py-2 text-sm font-medium disabled:opacity-60"
                >
                  {selected.status === "resting" ? "Aktifkan lagi" : "Istirahatkan"}
                </button>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => void remove(selected)}
                  className="rounded-md border border-bolu-belum/30 px-3 py-2 text-sm font-medium text-bolu-belum disabled:opacity-60"
                >
                  Keluarkan
                </button>
              </div>

              <section className="mt-6 border-t border-bolu-line pt-4">
                <h3 className="mb-2 text-sm font-semibold">Akses aplikasi</h3>
                {integrations.length === 0 ? (
                  <p className="text-sm text-bolu-muted">
                    Belum ada aplikasi tersambung. Hubungkan dulu di halaman Integrasi.
                  </p>
                ) : (
                  <ul className="space-y-2">
                    {integrations.map((integration) => {
                      const grant = grants.find((entry) => entry.integration_id === integration.id);
                      return (
                        <li key={integration.id} className="flex items-center gap-2 text-sm">
                          <span className="flex-1 truncate">
                            {integration.app}
                            {integration.account_label ? ` · ${integration.account_label}` : ""}
                          </span>
                          <select
                            value={grant?.permission ?? ""}
                            disabled={busy}
                            onChange={(event) => void saveGrant(selected, integration.id, event.target.value)}
                            className="rounded-md border border-bolu-border px-2 py-1 text-sm"
                          >
                            <option value="" disabled>
                              Tanpa akses
                            </option>
                            <option value="read">Baca saja</option>
                            <option value="read_write">Baca & tulis</option>
                          </select>
                        </li>
                      );
                    })}
                  </ul>
                )}
              </section>

              <section className="mt-6 border-t border-bolu-line pt-4">
                <h3 className="mb-2 text-sm font-semibold">Alat yang boleh dipakai</h3>
                {tools.length === 0 ? (
                  <p className="text-sm text-bolu-muted">
                    Belum ada alat. Beri akses aplikasi di atas, lalu alat yang sesuai muncul di sini.
                  </p>
                ) : (
                  <ul className="space-y-1 text-sm">
                    {tools.map((tool) => (
                      <li key={tool.name} className="flex items-center justify-between gap-2">
                        <code className="truncate">{tool.name}</code>
                        <span className="shrink-0 rounded-full bg-bolu-panel px-2 py-0.5 text-xs text-bolu-muted">
                          {tool.label}
                        </span>
                      </li>
                    ))}
                  </ul>
                )}
              </section>
            </div>
          ) : (
            <p className="rounded-md border border-dashed border-bolu-border px-4 py-6 text-sm text-bolu-muted">
              Pilih satu Bolu untuk melihat dan mengubah profilnya.
            </p>
          )}
        </aside>
      </div>
    </main>
  );
}
