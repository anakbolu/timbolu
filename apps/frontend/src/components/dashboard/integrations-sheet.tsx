"use client";

/**
 * Integrasi's hub, opened from the page's pill: what the workspace has connected,
 * and who may use it.
 *
 * The page itself only names the connection, so everything that acts on it lives
 * here — the search and the categories, the connected and available applications,
 * the per-Bolu grants behind "Atur akses", and the MCP servers. Every value behind
 * it comes from the hub's own endpoints, so what a person sees is what the runtime
 * will do.
 */

import { Search } from "lucide-react";
import { useCallback, useMemo, useState } from "react";

import { Bolu } from "@/components/bolu/bolu";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import {
  addMCPServer,
  deleteHubGrant,
  disconnectIntegration,
  refreshIntegration,
  refreshMCPServer,
  removeIntegration,
  removeMCPServer,
  setHubGrant,
  startAuthorization,
  type HubAdapter,
} from "@/lib/api";
import { crewFor, expressionOf, stateOf } from "@/lib/dashboard-view";

import { AppTile, tileFor } from "./apps-pill";
import { WhatsAppPanel } from "./whatsapp-panel";
import { useWorkspace } from "./workspace-data";

const CARD = "rounded-md border border-bolu-border bg-bolu-surface";
const HEADING = "font-display text-[22px] font-semibold";

/** STATUS_LABEL says what a connection's state means, in the user's words. */
export const STATUS_LABEL: Record<string, string> = {
  connected: "Terhubung",
  needs_relogin: "Perlu login ulang",
  disconnected: "Terputus",
};

/** STATUS_DOT is the dot beside a connection's state, matching the design. */
const STATUS_DOT: Record<string, string> = {
  connected: "var(--color-bolu-live)",
  needs_relogin: "var(--color-bolu-flag)",
  disconnected: "var(--color-bolu-rest)",
};

/**
 * IntegrationsSheet is the hub behind the pill: the search, the categories, the
 * connected and available applications, the grants, and the MCP servers. It is
 * opened rather than scrolled past, so it owns the state those pieces act on.
 */
export function IntegrationsSheet({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { hub, agents, workspaceID, reloadHub } = useWorkspace();

  const [query, setQuery] = useState("");
  const [category, setCategory] = useState("Semua");
  const [openAgent, setOpenAgent] = useState<string>("");
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [mcpOpen, setMcpOpen] = useState(false);

  const connections = hub.connections;
  const adapters = hub.adapters;

  // Categories are the ones the deployment actually offers, plus the design's
  // "Semua" that always leads.
  const categories = useMemo(() => {
    const found = new Set<string>();
    for (const adapter of adapters) found.add(tileFor(adapter.app).cat);
    return ["Semua", ...Array.from(found).sort()];
  }, [adapters]);

  const connectedIDs = useMemo(
    () => new Set(connections.map((connection) => connection.app)),
    [connections],
  );

  const matches = useCallback(
    (app: string, desc: string) => {
      const cat = tileFor(app).cat;
      if (category !== "Semua" && cat !== category) return false;
      const needle = query.trim().toLowerCase();
      if (!needle) return true;
      const { name } = tileFor(app);
      return (
        name.toLowerCase().includes(needle) ||
        app.toLowerCase().includes(needle) ||
        desc.toLowerCase().includes(needle)
      );
    },
    [category, query],
  );

  const shownConnections = connections.filter((connection) => matches(connection.app, ""));
  const shownAdapters = adapters.filter((adapter) => matches(adapter.app, ""));

  async function connect(app: string) {
    if (!workspaceID) return;
    setBusy(true);
    setError(null);
    const result = await startAuthorization(workspaceID, app);
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
    setError(null);
    setNotice(done);
    await reloadHub();
  }

  async function grant(agentId: string, integrationId: string, permission: string) {
    if (!workspaceID) return;
    setBusy(true);
    const result = await setHubGrant(workspaceID, agentId, integrationId, permission);
    setBusy(false);
    if (!result.ok) {
      setError(result.message);
      return;
    }
    setError(null);
    setNotice("Izin disimpan. Tugas berikutnya memakainya.");
    await reloadHub();
  }

  async function revoke(agentId: string, integrationId: string) {
    if (!workspaceID) return;
    setBusy(true);
    const result = await deleteHubGrant(workspaceID, agentId, integrationId);
    setBusy(false);
    if (!result.ok) {
      setError(result.message);
      return;
    }
    setError(null);
    setNotice("Izin dicabut.");
    await reloadHub();
  }

  const agentFor = (integrationId: string) => {
    const perBolu = Object.entries(hub.grantsByAgent);
    return perBolu.filter(([, list]) => list.some((item) => item.integration_id === integrationId));
  };

  const grantOf = (agentId: string, integrationId: string): string | undefined =>
    hub.grantsByAgent[agentId]?.find((item) => item.integration_id === integrationId)?.permission;

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent side="right" className="w-full gap-0 bg-bolu-bg p-0 sm:max-w-[620px]">
        <SheetHeader className="sticky top-0 z-10 gap-1 border-b border-bolu-border bg-bolu-bg p-[18px_56px_16px_20px]">
          <SheetTitle className={HEADING}>Integrasi</SheetTitle>
          <SheetDescription className="text-[14px] leading-[1.5] text-bolu-muted">
            Sambungkan akun yang dipakai tim Bolu, lalu atur siapa saja yang boleh memakainya. Token
            disimpan terenkripsi dan hanya dibuka saat dipakai.
          </SheetDescription>
        </SheetHeader>

        <div className="flex min-h-0 flex-1 flex-col gap-[22px] overflow-y-auto p-[0_20px_32px]">
          <label className="flex min-h-[46px] w-full items-center gap-2 rounded-full border border-bolu-chip bg-bolu-surface px-4">
            <Search size={18} strokeWidth={2.2} aria-hidden="true" />
            <span className="sr-only">Cari aplikasi</span>
            <input
              type="search"
              value={query}
              onChange={(event) => setQuery(event.target.value)}
              placeholder="Cari aplikasi, misalnya Jira"
              className="min-w-0 flex-1 border-0 bg-transparent text-[15px] text-bolu-ink outline-none"
            />
          </label>

          {error ? (
            <p
              role="alert"
              className="rounded-md bg-bolu-flag/10 px-4 py-3 text-[15px] text-bolu-flag"
            >
              {error}
            </p>
          ) : null}
          {notice ? (
            <p
              role="status"
              className="rounded-md bg-bolu-live/10 px-4 py-3 text-[15px] text-bolu-lunas"
            >
              {notice}
            </p>
          ) : null}

          {/* WhatsApp is not behind an OAuth consent screen like the other
              applications, so it gets its own card rather than a tile in the grid: a
              number is linked by scanning, and the step that matters — reading the
              warning — has to be read before anything starts. */}
          <WhatsAppPanel />

          <div className="flex flex-wrap gap-2">
            {categories.map((item) => (
              <button
                key={item}
                type="button"
                aria-pressed={item === category}
                onClick={() => setCategory(item)}
                className={`min-h-10 cursor-pointer rounded-full border px-3.5 text-[14px] font-semibold ${
                  item === category
                    ? "border-bolu-ink bg-bolu-solid text-bolu-solid-ink"
                    : "border-bolu-border bg-bolu-surface text-bolu-ink"
                }`}
              >
                {item}
              </button>
            ))}
          </div>

          <section className="flex flex-col gap-3">
            <div className="flex items-baseline gap-2.5">
              <h2 className={HEADING}>Terhubung</h2>
              <span className="text-[14px] text-bolu-muted">
                {connections.length === 0 ? "belum ada" : `${connections.length} aplikasi`}
              </span>
            </div>

            {shownConnections.length === 0 ? (
              <p className="py-2 text-[15px] text-bolu-muted">
                {connections.length === 0
                  ? "Belum ada aplikasi terhubung. Pilih satu dari daftar di bawah untuk menyambungkannya."
                  : "Belum ada aplikasi terhubung di kategori ini."}
              </p>
            ) : (
              <ul className="grid grid-cols-[repeat(auto-fill,minmax(min(320px,100%),1fr))] gap-3">
                {shownConnections.map((connection) => {
                  const meta = tileFor(connection.app);
                  const users = agentFor(connection.integration_id)
                    .map(([agentId]) => agents.find((item) => item.id === agentId))
                    .filter((item): item is (typeof agents)[number] => Boolean(item));
                  return (
                    <li
                      key={connection.integration_id}
                      className="flex flex-col gap-3 rounded-md border border-bolu-border bg-bolu-surface p-4"
                    >
                      <div className="flex items-center gap-3">
                        <AppTile app={connection.app} size={48} />
                        <div className="min-w-0 flex-1">
                          <div className="truncate text-[17px] font-bold">{meta.name}</div>
                          <div className="truncate text-[14px] text-bolu-muted">
                            {connection.account_label}
                          </div>
                        </div>
                        <span className="inline-flex flex-none items-center gap-1.5 text-[13px] font-bold">
                          <span
                            className="size-2 rounded-full"
                            style={{
                              backgroundColor:
                                STATUS_DOT[connection.status] ?? "var(--color-bolu-rest)",
                            }}
                          />
                          {STATUS_LABEL[connection.status] ?? connection.status}
                        </span>
                      </div>

                      <div className="flex flex-wrap items-center gap-x-3.5 gap-y-2 text-[14px] text-bolu-muted">
                        {users.length > 0 ? (
                          <span className="flex">
                            {users.map((agent) => (
                              <span key={agent.id} className="-mr-1.5 last:mr-0">
                                <Bolu
                                  member={crewFor(agent)}
                                  expression={expressionOf(stateOf(agent))}
                                  color={agent.color || undefined}
                                  size={28}
                                  still
                                />
                              </span>
                            ))}
                          </span>
                        ) : null}
                        <span>
                          {users.length === 0
                            ? "Belum dipakai Bolu mana pun"
                            : `Dipakai ${users.map((agent) => agent.name).join(", ")}`}
                        </span>
                        {connection.status_reason ? <span>· {connection.status_reason}</span> : null}
                      </div>

                      <div className="flex flex-wrap gap-2">
                        {connection.status === "needs_relogin" ? (
                          <>
                            <button
                              type="button"
                              disabled={busy}
                              onClick={() =>
                                void act(
                                  () => refreshIntegration(workspaceID!, connection.integration_id),
                                  "Token disegarkan.",
                                )
                              }
                              className="min-h-10 cursor-pointer rounded-full border border-bolu-border px-4 text-[14px] font-semibold disabled:opacity-50"
                            >
                              Coba segarkan
                            </button>
                            <button
                              type="button"
                              disabled={busy}
                              onClick={() => void connect(connection.app)}
                              className="min-h-10 cursor-pointer rounded-full bg-bolu-solid px-4 text-[14px] font-semibold text-bolu-solid-ink disabled:opacity-50"
                            >
                              Sambungkan ulang
                            </button>
                          </>
                        ) : null}
                        <button
                          type="button"
                          aria-pressed={openAgent === "grants"}
                          onClick={() =>
                            setOpenAgent((previous) => (previous === "grants" ? "" : "grants"))
                          }
                          className="min-h-10 cursor-pointer rounded-full border-2 border-bolu-ink bg-bolu-surface px-4 text-[14px] font-semibold text-bolu-ink"
                        >
                          Atur akses
                        </button>
                        <button
                          type="button"
                          disabled={busy}
                          onClick={() =>
                            void act(
                              () => disconnectIntegration(workspaceID!, connection.integration_id),
                              "Akun diputuskan. Token-nya dihapus.",
                            )
                          }
                          className="min-h-10 cursor-pointer rounded-full border-0 bg-transparent px-4 text-[14px] font-bold text-bolu-belum disabled:opacity-50"
                        >
                          Putuskan
                        </button>
                        <button
                          type="button"
                          disabled={busy}
                          onClick={() =>
                            void act(
                              () => removeIntegration(workspaceID!, connection.integration_id),
                              "Koneksi dihapus.",
                            )
                          }
                          className="min-h-10 cursor-pointer rounded-full border-0 bg-transparent px-3 text-[14px] font-bold text-bolu-muted disabled:opacity-50"
                        >
                          Hapus
                        </button>
                      </div>
                    </li>
                  );
                })}
              </ul>
            )}
          </section>

          {/* Grants: the design puts this behind "Atur akses", because who may use a
              connection is a detail of that connection rather than a screen. */}
          {openAgent === "grants" && connections.length > 0 ? (
            <section className={`${CARD} flex flex-col gap-3 p-5`}>
              <h2 className={HEADING}>Siapa yang boleh memakai</h2>
              <p className="text-[15px] text-bolu-muted">
                Perubahan berlaku untuk tugas berikutnya. Izin &ldquo;baca&rdquo; menyembunyikan alat
                yang bisa mengubah apa pun di luar Bolu.
              </p>

              {agents.length === 0 ? (
                <p className="text-[15px] text-bolu-muted">Belum ada Bolu untuk diberi izin.</p>
              ) : (
                <ul className="flex flex-col gap-3">
                  {agents.map((agent) => (
                    <li key={agent.id} className="flex flex-wrap items-center gap-3">
                      <div className="flex min-w-[180px] flex-1 items-center gap-2.5">
                        <Bolu
                          member={crewFor(agent)}
                          expression={expressionOf(stateOf(agent))}
                          color={agent.color || undefined}
                          size={36}
                          still
                        />
                        <span className="font-semibold">{agent.name}</span>
                      </div>
                      <div className="flex flex-wrap gap-1.5">
                        {connections.map((connection) => {
                          const permission = grantOf(agent.id, connection.integration_id);
                          return (
                            <span
                              key={connection.integration_id}
                              className="flex items-center gap-1"
                            >
                              <AppTile app={connection.app} size={26} />
                              <button
                                type="button"
                                disabled={busy}
                                aria-pressed={permission === "read"}
                                onClick={() =>
                                  void grant(agent.id, connection.integration_id, "read")
                                }
                                className={`min-h-9 cursor-pointer rounded-full border px-3 text-[13px] font-semibold disabled:opacity-50 ${
                                  permission === "read"
                                    ? "border-bolu-ink bg-bolu-solid text-bolu-solid-ink"
                                    : "border-bolu-border"
                                }`}
                              >
                                Baca
                              </button>
                              <button
                                type="button"
                                disabled={busy}
                                aria-pressed={permission === "read_write"}
                                onClick={() =>
                                  void grant(agent.id, connection.integration_id, "read_write")
                                }
                                className={`min-h-9 cursor-pointer rounded-full border px-3 text-[13px] font-semibold disabled:opacity-50 ${
                                  permission === "read_write"
                                    ? "border-bolu-ink bg-bolu-solid text-bolu-solid-ink"
                                    : "border-bolu-border"
                                }`}
                              >
                                Baca-tulis
                              </button>
                              {permission ? (
                                <button
                                  type="button"
                                  disabled={busy}
                                  onClick={() => void revoke(agent.id, connection.integration_id)}
                                  className="min-h-9 cursor-pointer rounded-full border border-bolu-flag/40 px-3 text-[13px] font-semibold text-bolu-flag disabled:opacity-50"
                                >
                                  Cabut
                                </button>
                              ) : null}
                            </span>
                          );
                        })}
                      </div>
                    </li>
                  ))}
                </ul>
              )}
            </section>
          ) : null}

          <section className="flex flex-col gap-3">
            <div className="flex flex-wrap items-baseline gap-2.5">
              <h2 className={HEADING}>Tersedia</h2>
              <span className="text-[14px] text-bolu-muted">
                {adapters.length === 0 ? "tidak ada" : `${adapters.length} aplikasi`}
              </span>
            </div>
            <p className="text-[15px] text-bolu-muted">
              Alat bertanda &ldquo;perlu persetujuan&rdquo; selalu berhenti menunggu keputusanmu
              sebelum apa pun keluar.
            </p>

            {adapters.length === 0 ? (
              <p className="text-[15px] text-bolu-muted">
                Deployment ini belum mengonfigurasi aplikasi apa pun. Kredensial OAuth yang kosong
                membuat aplikasinya tidak ditawarkan, bukan ditawarkan lalu gagal.
              </p>
            ) : shownAdapters.length === 0 ? (
              <p className="text-[15px] text-bolu-muted">
                Tidak ada aplikasi yang cocok dengan pencarianmu.
              </p>
            ) : (
              <ul className="grid grid-cols-[repeat(auto-fill,minmax(min(260px,100%),1fr))] gap-3">
                {shownAdapters.map((adapter: HubAdapter) => {
                  const meta = tileFor(adapter.app);
                  const already = connectedIDs.has(adapter.app);
                  const tools = adapter.tools.length;
                  return (
                    <li
                      key={adapter.app}
                      className="flex flex-col gap-2.5 rounded-md border border-bolu-border bg-bolu-surface p-4"
                    >
                      <div className="flex items-center gap-3">
                        <AppTile app={adapter.app} size={44} />
                        <div className="min-w-0">
                          <div className="truncate font-bold">{meta.name}</div>
                          <div className="text-[13px] text-bolu-muted">{meta.cat}</div>
                        </div>
                      </div>

                      <div className="flex-1 text-[14px] text-bolu-body">
                        {tools === 0
                          ? "Belum menyatakan alat apa pun."
                          : `${tools} alat${
                              adapter.tools.some((tool) => tool.needs_human)
                                ? ", sebagian perlu persetujuanmu"
                                : ", semuanya hanya membaca"
                            }.`}
                      </div>

                      <div className="flex items-center justify-between gap-2">
                        {adapter.oauth ? (
                          <button
                            type="button"
                            disabled={busy}
                            onClick={() => void connect(adapter.app)}
                            className="min-h-10 cursor-pointer rounded-full bg-bolu-solid px-4 text-[14px] font-bold text-bolu-solid-ink disabled:opacity-50"
                          >
                            {already ? "Hubungkan lagi" : "Hubungkan"}
                          </button>
                        ) : (
                          <span className="text-[13px] text-bolu-muted">Tanpa alur OAuth</span>
                        )}
                        <span className="text-[13px] text-bolu-muted">
                          {already ? "Sudah tersambung" : "Belum tersambung"}
                        </span>
                      </div>
                    </li>
                  );
                })}
              </ul>
            )}

            <p className="text-[14px] text-bolu-muted">
              Aplikasimu belum ada? Beri tahu kami aplikasi apa yang kamu pakai, nanti kami kabari
              kalau sudah bisa disambungkan. Kanal untuk usulan itu belum dibangun.
            </p>
          </section>

          <section className={`${CARD} flex flex-col gap-3 p-5`}>
            <div className="flex flex-wrap items-center justify-between gap-2.5">
              <h2 className={HEADING}>Server MCP</h2>
              <button
                type="button"
                onClick={() => setMcpOpen((previous) => !previous)}
                aria-expanded={mcpOpen}
                className="min-h-10 cursor-pointer rounded-full border border-bolu-border px-3.5 text-[14px] font-semibold"
              >
                {mcpOpen ? "Tutup" : "Tambah server"}
              </button>
            </div>
            <p className="text-[15px] text-bolu-muted">
              Untuk aplikasi yang sudah punya server MCP sendiri. Alatnya muncul seperti alat lain,
              dan yang tidak menyatakan dirinya hanya-baca diperlakukan sebagai aksi keluar.
            </p>

            {hub.servers.length === 0 ? (
              <p className="text-[15px] text-bolu-muted">Belum ada server MCP yang ditambahkan.</p>
            ) : (
              <ul className="flex flex-col gap-2">
                {hub.servers.map((server) => (
                  <li
                    key={server.id}
                    className="flex flex-wrap items-start justify-between gap-3 rounded-md border border-bolu-line px-4 py-3"
                  >
                    <div className="min-w-0">
                      <p className="truncate font-semibold">{server.label || server.app}</p>
                      <p className="truncate text-[14px] text-bolu-muted">{server.endpoint}</p>
                      <p className="text-[14px] text-bolu-body">
                        {server.tool_count} alat
                        {server.last_error ? ` · terakhir gagal: ${server.last_error}` : ""}
                      </p>
                    </div>
                    <div className="flex flex-wrap gap-1.5">
                      <button
                        type="button"
                        disabled={busy}
                        onClick={() =>
                          void act(
                            () => refreshMCPServer(workspaceID!, server.id),
                            "Server disegarkan.",
                          )
                        }
                        className="min-h-10 cursor-pointer rounded-full border border-bolu-border px-3.5 text-[14px] font-semibold disabled:opacity-50"
                      >
                        Segarkan
                      </button>
                      <button
                        type="button"
                        disabled={busy}
                        onClick={() =>
                          void act(() => removeMCPServer(workspaceID!, server.id), "Server dihapus.")
                        }
                        className="min-h-10 cursor-pointer rounded-full border border-bolu-flag/40 px-3.5 text-[14px] font-semibold text-bolu-flag disabled:opacity-50"
                      >
                        Hapus
                      </button>
                    </div>
                  </li>
                ))}
              </ul>
            )}

            {mcpOpen ? <MCPForm workspaceID={workspaceID} onDone={reloadHub} /> : null}
          </section>
        </div>
      </SheetContent>
    </Sheet>
  );
}

/** MCPForm adds one server, and says so when it did not answer. */
function MCPForm({
  workspaceID,
  onDone,
}: {
  workspaceID: string | null;
  onDone: () => Promise<void>;
}) {
  const [app, setApp] = useState("");
  const [endpoint, setEndpoint] = useState("");
  const [credential, setCredential] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function add(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!workspaceID || busy) return;

    setBusy(true);
    setError(null);

    const result = await addMCPServer(workspaceID, {
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
    setError(
      result.data.last_error
        ? `Server tersimpan, tapi belum menjawab: ${result.data.last_error}`
        : null,
    );
    setApp("");
    setEndpoint("");
    setCredential("");
    await onDone();
  }

  return (
    <form onSubmit={add} className="flex flex-wrap items-end gap-3">
      {error ? (
        <p
          role="alert"
          className="basis-full rounded-md bg-bolu-flag/10 px-3 py-2 text-[14px] text-bolu-flag"
        >
          {error}
        </p>
      ) : null}
      <label className="flex flex-col gap-1.5 text-[14px] font-semibold">
        Nama aplikasi
        <input
          value={app}
          onChange={(event) => setApp(event.target.value)}
          placeholder="notion"
          className="min-h-11 w-40 rounded-md border border-bolu-border px-3.5 text-[15px] font-normal outline-none focus:border-bolu-accent"
        />
      </label>
      <label className="flex min-w-[200px] flex-[1_1_260px] flex-col gap-1.5 text-[14px] font-semibold">
        Endpoint
        <input
          value={endpoint}
          onChange={(event) => setEndpoint(event.target.value)}
          placeholder="https://mcp.example.com/mcp"
          className="min-h-11 w-full rounded-md border border-bolu-border px-3.5 text-[15px] font-normal outline-none focus:border-bolu-accent"
        />
      </label>
      <label className="flex min-w-[160px] flex-[1_1_200px] flex-col gap-1.5 text-[14px] font-semibold">
        Token (opsional)
        <input
          type="password"
          value={credential}
          onChange={(event) => setCredential(event.target.value)}
          className="min-h-11 w-full rounded-md border border-bolu-border px-3.5 text-[15px] font-normal outline-none focus:border-bolu-accent"
        />
      </label>
      <button
        type="submit"
        disabled={busy || app.trim() === "" || endpoint.trim() === ""}
        className="min-h-11 cursor-pointer rounded-full bg-bolu-solid px-5 font-bold text-bolu-solid-ink disabled:opacity-50"
      >
        {busy ? "Menambah…" : "Tambah server"}
      </button>
    </form>
  );
}
