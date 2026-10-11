"use client";

/**
 * The Integrations screen: what the workspace has connected, who may use it, and
 * the servers it added.
 *
 * Everything here reads the hub's own endpoints rather than a local model, so what
 * a person sees is what the runtime will actually do. The screen a provider's
 * redirect lands on is this one, so it also reads the outcome of a flow from the
 * query string and says what happened.
 */

import { useCallback, useEffect, useState } from "react";

import {
  addMCPServer,
  deleteHubGrant,
  disconnectIntegration,
  getActiveWorkspaceId,
  listAgentTeams,
  listHubAdapters,
  listHubGrants,
  listHubIntegrations,
  listMCPServers,
  refreshIntegration,
  refreshMCPServer,
  refreshSession,
  removeIntegration,
  removeMCPServer,
  setHubGrant,
  startAuthorization,
  type Agent,
  type HubAdapter,
  type HubIntegration,
  type MCPServer,
} from "@/lib/api";

/** STATUS_LABEL says what a connection's state means, in the user's words. */
const STATUS_LABEL: Record<string, string> = {
  connected: "Tersambung",
  needs_relogin: "Perlu login ulang",
  disconnected: "Terputus",
};

const STATUS_CLASS: Record<string, string> = {
  connected: "bg-bolu-live/12 text-bolu-lunas",
  needs_relogin: "bg-bolu-flag/12 text-bolu-flag",
  disconnected: "bg-bolu-panel text-bolu-muted",
};

/** OUTCOME says what a finished authorization flow did. */
const OUTCOME: Record<string, string> = {
  connected: "Akun tersambung.",
  declined: "Kamu menolak aksesnya, jadi tidak ada yang tersambung.",
  invalid_state: "Tautan itu bukan milik alur yang dimulai dari sini, atau sudah dipakai. Coba sambungkan lagi.",
  exchange_failed: "Penyedia menolak kode itu. Biasanya karena terlalu lama di layar izin; coba lagi.",
  unknown_app: "Aplikasi itu tidak dikenal deployment ini.",
  failed: "Penyambungan gagal. Coba lagi.",
};

/**
 * readOutcome turns the query a provider's redirect left behind into a message.
 *
 * It runs only in the browser: the screen is a client component, but it is still
 * rendered once on the server, where there is no query string to read.
 */
function readOutcome(): string | null {
  if (typeof window === "undefined") {
    return null;
  }

  const params = new URLSearchParams(window.location.search);
  const outcome = params.get("integration");
  if (!outcome) {
    return null;
  }

  const message = OUTCOME[outcome] ?? OUTCOME.failed;
  const detail = params.get("detail");

  // A refusal's own words are worth showing; a success has nothing to add.
  if (detail && outcome !== "connected") {
    return `${message} (${detail})`;
  }
  return message;
}

export function IntegrationsPanel() {
  const [connections, setConnections] = useState<HubIntegration[]>([]);
  const [adapters, setAdapters] = useState<HubAdapter[]>([]);
  const [servers, setServers] = useState<MCPServer[]>([]);
  const [agents, setAgents] = useState<Agent[]>([]);
  const [grants, setGrants] = useState<Record<string, string>>({});
  const [openAgent, setOpenAgent] = useState<string>("");

  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const workspaceId = typeof window === "undefined" ? null : getActiveWorkspaceId();

  const load = useCallback(async () => {
    if (!workspaceId) {
      setError("Pilih workspace dulu.");
      return;
    }

    const session = await refreshSession();
    if (!session.ok) {
      setError("Sesi berakhir. Masuk lagi.");
      return;
    }

    const [connectionResult, adapterResult, serverResult, teamResult] = await Promise.all([
      listHubIntegrations(workspaceId),
      listHubAdapters(workspaceId),
      listMCPServers(workspaceId),
      listAgentTeams(workspaceId),
    ]);

    if (!connectionResult.ok) {
      setError(connectionResult.message);
      return;
    }
    setError(null);
    setConnections(connectionResult.data);
    if (adapterResult.ok) setAdapters(adapterResult.data);
    if (serverResult.ok) setServers(serverResult.data);
    if (teamResult.ok) setAgents(teamResult.data.flatMap((team) => team.agents));
  }, [workspaceId]);

  useEffect(() => {
    void (async () => {
      await load();
    })();
  }, [load]);

  // The provider's redirect lands here with the outcome in the query string. It is
  // read once, when the screen first renders, rather than from an effect: what the
  // query said is part of what this screen is showing, not something that happens
  // to it afterwards.
  const [outcome] = useState<string | null>(() => readOutcome());

  // What a finished flow did is shown until something else says otherwise, which
  // is what makes the provider's redirect land somewhere that explains itself.
  const shown = notice ?? outcome;

  // And the query is then cleared, so a reload does not repeat a message about a
  // flow that already finished. This is the side effect an effect is for.
  useEffect(() => {
    if (!outcome) {
      return;
    }
    window.history.replaceState({}, "", window.location.pathname);
  }, [outcome]);

  async function openGrants(agent: Agent) {
    if (!workspaceId) return;

    setOpenAgent(agent.id);
    const result = await listHubGrants(workspaceId, agent.id);
    if (!result.ok) {
      setError(result.message);
      return;
    }

    const next: Record<string, string> = {};
    for (const grant of result.data) {
      next[grant.integration_id] = grant.permission;
    }
    setGrants(next);
  }

  async function grant(agentId: string, integrationId: string, permission: string) {
    if (!workspaceId) return;

    setBusy(true);
    const result = await setHubGrant(workspaceId, agentId, integrationId, permission);
    setBusy(false);

    if (!result.ok) {
      setError(result.message);
      return;
    }
    setNotice("Izin disimpan. Tugas berikutnya memakainya.");
    setGrants((previous) => ({ ...previous, [integrationId]: permission }));
  }

  async function revoke(agentId: string, integrationId: string) {
    if (!workspaceId) return;

    setBusy(true);
    const result = await deleteHubGrant(workspaceId, agentId, integrationId);
    setBusy(false);

    if (!result.ok) {
      setError(result.message);
      return;
    }
    setNotice("Izin dicabut.");
    setGrants((previous) => {
      const next = { ...previous };
      delete next[integrationId];
      return next;
    });
  }

  /** connect asks where to send the browser, then sends it there. */
  async function connect(app: string) {
    if (!workspaceId) return;

    setBusy(true);
    setError(null);
    const result = await startAuthorization(workspaceId, app);
    setBusy(false);

    if (!result.ok) {
      setError(result.message);
      return;
    }
    window.location.assign(result.data.url);
  }

  async function act(action: () => Promise<{ ok: boolean; message?: string }>, done: string) {
    setBusy(true);
    const result = await action();
    setBusy(false);

    if (!result.ok) {
      setError(result.message ?? "Gagal.");
      return;
    }
    setNotice(done);
    await load();
  }

  return (
    <div className="flex flex-col gap-5 p-[24px_28px_48px] max-[760px]:p-[18px_14px_36px]">
      <header className="flex flex-col gap-1">
        <h1 className="font-display text-[36px] font-bold leading-[1.1] max-[760px]:text-[28px]">
          Integrasi
        </h1>
        <p className="text-bolu-muted">
          Sambungkan akun yang dipakai tim Bolu, lalu atur siapa saja yang boleh memakainya.
          Token disimpan terenkripsi dan hanya dibuka saat dipakai.
        </p>
      </header>

      {error ? (
        <p role="alert" className="rounded-xl bg-bolu-flag/10 px-3 py-2 text-sm text-bolu-flag">
          {error}
        </p>
      ) : null}
      {shown ? (
        <p className="rounded-xl bg-bolu-live/10 px-3 py-2 text-sm text-bolu-lunas">{shown}</p>
      ) : null}

      {/* Connected accounts ------------------------------------------------ */}
      <section className="flex flex-col gap-3">
        <h2 className="font-display text-[22px] font-semibold">Akun tersambung</h2>

        {connections.length === 0 ? (
          <p className="rounded-2xl border border-dashed border-bolu-border px-4 py-6 text-center text-sm text-bolu-muted">
            Belum ada akun tersambung. Pilih aplikasi di bawah untuk menyambungkannya.
          </p>
        ) : (
          <ul className="flex flex-col gap-2">
            {connections.map((connection) => (
              <li
                key={connection.integration_id}
                className="flex flex-wrap items-start justify-between gap-3 rounded-2xl border border-bolu-border bg-bolu-surface px-4 py-3"
              >
                <div className="min-w-0">
                  <p className="text-xs uppercase tracking-wide text-bolu-muted">{connection.app}</p>
                  <p className="truncate font-semibold">{connection.account_label}</p>
                  {connection.status_reason ? (
                    <p className="mt-1 text-sm text-bolu-body">{connection.status_reason}</p>
                  ) : null}
                </div>

                <div className="flex flex-wrap items-center gap-2">
                  <span
                    className={`rounded-full px-2.5 py-1 text-xs font-medium ${
                      STATUS_CLASS[connection.status] ?? STATUS_CLASS.disconnected
                    }`}
                  >
                    {STATUS_LABEL[connection.status] ?? connection.status}
                  </span>

                  {connection.status === "needs_relogin" ? (
                    <>
                      <button
                        type="button"
                        disabled={busy}
                        onClick={() =>
                          act(
                            () => refreshIntegration(workspaceId!, connection.integration_id),
                            "Token disegarkan.",
                          )
                        }
                        className="min-h-9 cursor-pointer rounded-full border border-bolu-border px-3 text-sm font-semibold disabled:opacity-50"
                      >
                        Coba segarkan
                      </button>
                      <button
                        type="button"
                        disabled={busy}
                        onClick={() => void connect(connection.app)}
                        className="min-h-9 cursor-pointer rounded-full bg-bolu-ink px-3 text-sm font-semibold text-white disabled:opacity-50"
                      >
                        Sambungkan ulang
                      </button>
                    </>
                  ) : null}

                  {connection.status === "connected" ? (
                    <button
                      type="button"
                      disabled={busy}
                      onClick={() =>
                        act(
                          () => disconnectIntegration(workspaceId!, connection.integration_id),
                          "Akun diputuskan. Token-nya dihapus.",
                        )
                      }
                      className="min-h-9 cursor-pointer rounded-full border border-bolu-border px-3 text-sm font-semibold disabled:opacity-50"
                    >
                      Putuskan
                    </button>
                  ) : null}

                  <button
                    type="button"
                    disabled={busy}
                    onClick={() =>
                      act(
                        () => removeIntegration(workspaceId!, connection.integration_id),
                        "Koneksi dihapus.",
                      )
                    }
                    className="min-h-9 cursor-pointer rounded-full border border-bolu-flag/40 px-3 text-sm font-semibold text-bolu-flag disabled:opacity-50"
                  >
                    Hapus
                  </button>
                </div>
              </li>
            ))}
          </ul>
        )}
      </section>

      {/* Available applications ------------------------------------------- */}
      <section className="flex flex-col gap-3">
        <h2 className="font-display text-[22px] font-semibold">Aplikasi tersedia</h2>
        <p className="text-sm text-bolu-muted">
          Alat bertanda &ldquo;perlu persetujuan&rdquo; selalu berhenti menunggu keputusanmu sebelum
          apa pun keluar.
        </p>

        {adapters.length === 0 ? (
          <p className="rounded-2xl border border-dashed border-bolu-border px-4 py-6 text-center text-sm text-bolu-muted">
            Deployment ini belum mengonfigurasi aplikasi apa pun.
          </p>
        ) : (
          <ul className="flex flex-col gap-2">
            {adapters.map((adapter) => (
              <li
                key={adapter.app}
                className="flex flex-col gap-2 rounded-2xl border border-bolu-border bg-bolu-surface px-4 py-3"
              >
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <p className="font-semibold">{adapter.app}</p>
                  {adapter.oauth ? (
                    <button
                      type="button"
                      disabled={busy}
                      onClick={() => void connect(adapter.app)}
                      className="min-h-9 cursor-pointer rounded-full bg-bolu-ink px-3.5 text-sm font-semibold text-white disabled:opacity-50"
                    >
                      Hubungkan
                    </button>
                  ) : null}
                </div>

                <ul className="flex flex-wrap gap-1.5">
                  {adapter.tools.map((tool) => (
                    <li
                      key={tool.name}
                      title={tool.description}
                      className={`rounded-full px-2.5 py-1 text-xs font-medium ${
                        tool.needs_human
                          ? "bg-bolu-flag/12 text-bolu-flag"
                          : "bg-bolu-panel text-bolu-muted"
                      }`}
                    >
                      {tool.name}
                      {tool.needs_human ? " · perlu persetujuan" : ""}
                    </li>
                  ))}
                </ul>

                {adapter.triggers && adapter.triggers.length > 0 ? (
                  <p className="text-xs text-bolu-muted">
                    Pemicu: {adapter.triggers.map((trigger) => trigger.event).join(", ")}
                  </p>
                ) : null}
              </li>
            ))}
          </ul>
        )}
      </section>

      {/* Who may use what -------------------------------------------------- */}
      <section className="flex flex-col gap-3">
        <h2 className="font-display text-[22px] font-semibold">Izin per Bolu</h2>
        <p className="text-sm text-bolu-muted">
          Perubahan berlaku untuk tugas berikutnya. Izin &ldquo;baca&rdquo; menyembunyikan alat yang
          bisa mengubah apa pun di luar Bolu.
        </p>

        <div className="flex flex-wrap gap-2">
          {agents.map((agent) => (
            <button
              key={agent.id}
              type="button"
              onClick={() => void openGrants(agent)}
              aria-pressed={openAgent === agent.id}
              className={`min-h-10 cursor-pointer rounded-full border-2 px-3.5 text-sm font-semibold ${
                openAgent === agent.id
                  ? "border-bolu-ink bg-bolu-ink text-white"
                  : "border-bolu-border bg-white text-bolu-ink"
              }`}
            >
              {agent.name}
            </button>
          ))}
        </div>

        {openAgent ? (
          connections.length === 0 ? (
            <p className="text-sm text-bolu-muted">Sambungkan akun dulu untuk memberi izin.</p>
          ) : (
            <ul className="flex flex-col gap-2">
              {connections.map((connection) => {
                const permission = grants[connection.integration_id];
                return (
                  <li
                    key={connection.integration_id}
                    className="flex flex-wrap items-center justify-between gap-3 rounded-2xl border border-bolu-border bg-bolu-surface px-4 py-3"
                  >
                    <div className="min-w-0">
                      <p className="truncate font-semibold">
                        {connection.app} · {connection.account_label}
                      </p>
                      <p className="text-sm text-bolu-muted">
                        {permission === "read_write"
                          ? "Baca-tulis"
                          : permission === "read"
                            ? "Baca saja"
                            : "Belum diberi izin"}
                      </p>
                    </div>

                    <div className="flex flex-wrap gap-2">
                      <button
                        type="button"
                        disabled={busy}
                        onClick={() => void grant(openAgent, connection.integration_id, "read")}
                        aria-pressed={permission === "read"}
                        className={`min-h-9 cursor-pointer rounded-full border px-3 text-sm font-semibold disabled:opacity-50 ${
                          permission === "read"
                            ? "border-bolu-ink bg-bolu-ink text-white"
                            : "border-bolu-border"
                        }`}
                      >
                        Baca
                      </button>
                      <button
                        type="button"
                        disabled={busy}
                        onClick={() => void grant(openAgent, connection.integration_id, "read_write")}
                        aria-pressed={permission === "read_write"}
                        className={`min-h-9 cursor-pointer rounded-full border px-3 text-sm font-semibold disabled:opacity-50 ${
                          permission === "read_write"
                            ? "border-bolu-ink bg-bolu-ink text-white"
                            : "border-bolu-border"
                        }`}
                      >
                        Baca-tulis
                      </button>
                      {permission ? (
                        <button
                          type="button"
                          disabled={busy}
                          onClick={() => void revoke(openAgent, connection.integration_id)}
                          className="min-h-9 cursor-pointer rounded-full border border-bolu-border px-3 text-sm font-semibold disabled:opacity-50"
                        >
                          Cabut
                        </button>
                      ) : null}
                    </div>
                  </li>
                );
              })}
            </ul>
          )
        ) : null}
      </section>

      <MCPSection workspaceId={workspaceId} servers={servers} reload={load} />
    </div>
  );
}

/** MCPSection is the servers a workspace added, kept apart from its own adapters. */
function MCPSection({
  workspaceId,
  servers,
  reload,
}: {
  workspaceId: string | null;
  servers: MCPServer[];
  reload: () => Promise<void>;
}) {
  const [app, setApp] = useState("");
  const [endpoint, setEndpoint] = useState("");
  const [credential, setCredential] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function add(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!workspaceId || busy) return;

    setBusy(true);
    setError(null);

    const result = await addMCPServer(workspaceId, {
      app: app.trim(),
      endpoint: endpoint.trim(),
      credential: credential.trim(),
    });
    setBusy(false);

    if (!result.ok) {
      setError(result.message);
      return;
    }

    // A server that was added but did not answer is listed with its reason, so
    // that is a notice rather than a failure.
    if (result.data.last_error) {
      setError(`Server tersimpan, tapi belum menjawab: ${result.data.last_error}`);
    } else {
      setError(null);
    }

    setApp("");
    setEndpoint("");
    setCredential("");
    await reload();
  }

  return (
    <section className="flex flex-col gap-3">
      <h2 className="font-display text-[22px] font-semibold">Server MCP</h2>
      <p className="text-sm text-bolu-muted">
        Untuk aplikasi yang sudah punya server MCP sendiri. Alatnya muncul seperti alat lain, dan
        yang tidak menyatakan dirinya hanya-baca diperlakukan sebagai aksi keluar.
      </p>

      {error ? (
        <p role="alert" className="rounded-xl bg-bolu-flag/10 px-3 py-2 text-sm text-bolu-flag">
          {error}
        </p>
      ) : null}

      {servers.length > 0 ? (
        <ul className="flex flex-col gap-2">
          {servers.map((server) => (
            <li
              key={server.id}
              className="flex flex-wrap items-start justify-between gap-3 rounded-2xl border border-bolu-border bg-bolu-surface px-4 py-3"
            >
              <div className="min-w-0">
                <p className="truncate font-semibold">
                  {server.label || server.app} · {server.app}
                </p>
                <p className="truncate text-sm text-bolu-muted">{server.endpoint}</p>
                <p className="text-sm text-bolu-body">
                  {server.tool_count} alat
                  {server.last_error ? ` · terakhir gagal: ${server.last_error}` : ""}
                </p>
              </div>

              <div className="flex flex-wrap gap-2">
                <button
                  type="button"
                  disabled={busy}
                  onClick={() =>
                    void (async () => {
                      setBusy(true);
                      await refreshMCPServer(workspaceId!, server.id);
                      setBusy(false);
                      await reload();
                    })()
                  }
                  className="min-h-9 cursor-pointer rounded-full border border-bolu-border px-3 text-sm font-semibold disabled:opacity-50"
                >
                  Segarkan
                </button>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() =>
                    void (async () => {
                      setBusy(true);
                      await removeMCPServer(workspaceId!, server.id);
                      setBusy(false);
                      await reload();
                    })()
                  }
                  className="min-h-9 cursor-pointer rounded-full border border-bolu-flag/40 px-3 text-sm font-semibold text-bolu-flag disabled:opacity-50"
                >
                  Hapus
                </button>
              </div>
            </li>
          ))}
        </ul>
      ) : null}

      <form onSubmit={add} className="flex flex-wrap items-end gap-2">
        <label className="text-sm">
          <span className="mr-1.5 text-bolu-muted">Nama aplikasi</span>
          <input
            value={app}
            onChange={(event) => setApp(event.target.value)}
            placeholder="notion"
            className="w-36 rounded-lg border border-bolu-border bg-bolu-surface px-2 py-1"
          />
        </label>
        <label className="min-w-0 flex-[1_1_260px] text-sm">
          <span className="mr-1.5 text-bolu-muted">Endpoint</span>
          <input
            value={endpoint}
            onChange={(event) => setEndpoint(event.target.value)}
            placeholder="https://mcp.example.com/mcp"
            className="w-full rounded-lg border border-bolu-border bg-bolu-surface px-2 py-1"
          />
        </label>
        <label className="min-w-0 flex-[1_1_200px] text-sm">
          <span className="mr-1.5 text-bolu-muted">Token (opsional)</span>
          <input
            type="password"
            value={credential}
            onChange={(event) => setCredential(event.target.value)}
            className="w-full rounded-lg border border-bolu-border bg-bolu-surface px-2 py-1"
          />
        </label>
        <button
          type="submit"
          disabled={busy || app.trim() === "" || endpoint.trim() === ""}
          className="min-h-10 cursor-pointer rounded-full bg-bolu-ink px-4 font-semibold text-white disabled:opacity-50"
        >
          Tambah server
        </button>
      </form>
    </section>
  );
}
