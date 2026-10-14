"use client";

/**
 * The memory screen: everything the workspace and its Bolu remember, and every
 * way of changing it.
 *
 * Two things are deliberate here:
 *
 * - the list is grouped by layer, because the layers mean different things. The
 *   main memory is shared by every call; a Bolu's own memory is its own way of
 *   working, and moving a fact between the two is a decision about who may use
 *   it.
 * - a version kept as history is shown with the memory that replaced it, not
 *   hidden. "Why does it say 18 ribu now" is a question the screen should be
 *   able to answer, and the answer is the row underneath.
 */

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useCallback, useEffect, useMemo, useState } from "react";

import { Alert } from "@/components/auth/auth-shell";
import {
  Agent,
  ApiResult,
  KnowledgeItem,
  Memory,
  addMemory,
  deleteKnowledge,
  deleteMemory,
  ensureActiveWorkspace,
  ingestDocument,
  listAgentTeams,
  listKnowledge,
  listMemories,
  refreshSession,
  setMemoryExtraction,
  updateMemory,
} from "@/lib/api";

const KIND_LABEL: Record<string, string> = {
  fact: "Fakta",
  preference: "Preferensi",
  instruction: "Instruksi",
};

const KIND_ORDER = ["fact", "preference", "instruction"];

const STATUS_LABEL: Record<string, string> = {
  pending: "Menunggu",
  ready: "Siap",
  failed: "Gagal",
};

const STATUS_CLASS: Record<string, string> = {
  pending: "bg-bolu-panel text-bolu-muted",
  ready: "bg-bolu-live/12 text-bolu-lunas",
  failed: "bg-bolu-belum/12 text-bolu-belum",
};

/** formatDate renders a timestamp the way the rest of the app does. */
function formatDate(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return "—";
  }
  return new Intl.DateTimeFormat("id-ID", { dateStyle: "medium", timeStyle: "short" }).format(date);
}

/** MAIN is the sentinel the API reads for the workspace-wide layer. */
const MAIN = "main";

export function MemoryView() {
  const router = useRouter();
  const [memories, setMemories] = useState<Memory[]>([]);
  const [knowledge, setKnowledge] = useState<KnowledgeItem[]>([]);
  const [agents, setAgents] = useState<Agent[]>([]);
  const [workspaceId, setWorkspaceId] = useState<string | null>(null);
  const [query, setQuery] = useState("");
  const [showHistory, setShowHistory] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const load = useCallback(async () => {
    const session = await refreshSession();
    if (!session.ok) {
      // A refresh that fails leaves no token behind, so there is no session to
      // read this screen against: the door is where the person belongs, and it
      // says why.
      router.replace("/login?sesi=berakhir");
      return;
    }

    const active = await ensureActiveWorkspace();
    setWorkspaceId(active);
    if (!active) {
      setError("Pilih workspace dulu.");
      return;
    }

    const [memoryResult, knowledgeResult, teamResult] = await Promise.all([
      listMemories(active, { history: true, limit: 200 }),
      listKnowledge(active),
      listAgentTeams(active),
    ]);

    if (!memoryResult.ok) {
      setError(memoryResult.message);
      return;
    }

    setMemories(memoryResult.data);
    setKnowledge(knowledgeResult.ok ? knowledgeResult.data : []);
    setAgents(teamResult.ok ? teamResult.data.flatMap((team) => team.agents) : []);
  }, [router]);

  useEffect(() => {
    void (async () => {
      await load();
    })();
  }, [load]);

  function report<T>(result: ApiResult<T>): result is Extract<ApiResult<T>, { ok: true }> {
    if (result.ok) {
      return true;
    }
    setError(result.message);
    return false;
  }

  function run(operation: () => Promise<void>) {
    void (async () => {
      setBusy(true);
      setError(null);
      await operation();
      setBusy(false);
    })();
  }

  /**
   * The grouping the screen is built around. A search filters within each layer
   * rather than flattening the sections, so "where does this live" stays visible
   * while the reader looks for one thing.
   */
  const filtered = useMemo(() => {
    const needle = query.trim().toLowerCase();

    const visible = memories.filter((memory) => {
      if (!showHistory && memory.superseded_by) {
        return false;
      }
      if (!needle) {
        return true;
      }
      return memory.content.toLowerCase().includes(needle);
    });

    const main = visible.filter((memory) => !memory.agent_id);
    const perAgent = new Map<string, Memory[]>();
    for (const memory of visible) {
      if (!memory.agent_id) {
        continue;
      }
      const list = perAgent.get(memory.agent_id) ?? [];
      list.push(memory);
      perAgent.set(memory.agent_id, list);
    }

    return { main, perAgent };
  }, [memories, query, showHistory]);

  async function add(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!workspaceId) {
      return;
    }

    const form = event.currentTarget;
    const data = new FormData(form);
    const content = String(data.get("content") ?? "").trim();
    if (!content) {
      setError("Tulis dulu isi yang mau diingat.");
      return;
    }

    const layer = String(data.get("layer") ?? MAIN);
    run(async () => {
      const result = await addMemory(workspaceId, {
        content,
        kind: String(data.get("kind") ?? "fact"),
        agent_id: layer === MAIN ? undefined : layer,
        pinned: data.get("pinned") === "on",
      });
      if (!report(result)) {
        return;
      }
      setNotice("Memori disimpan. Panggilan agen berikutnya sudah memakainya.");
      form.reset();
      await load();
    });
  }

  /**
   * edit saves one memory.
   *
   * It reports whether the save went through, because the row uses that to decide
   * whether to leave edit mode: a form that closes on a failed save would hide the
   * text the person just typed, and the error notice would be about something they
   * can no longer fix.
   */
  async function edit(memory: Memory, event: React.FormEvent<HTMLFormElement>): Promise<boolean> {
    event.preventDefault();
    if (!workspaceId) {
      return false;
    }

    const data = new FormData(event.currentTarget);
    const content = String(data.get("content") ?? "").trim();
    const kind = String(data.get("kind") ?? memory.kind);

    setBusy(true);
    setError(null);

    const result = await updateMemory(workspaceId, memory.id, { content, kind, pinned: memory.pinned });
    if (!report(result)) {
      setBusy(false);
      return false;
    }

    setNotice("Memori diperbarui. Isi barunya yang dikirim ke model mulai sekarang.");
    await load();
    setBusy(false);
    return true;
  }

  async function togglePin(memory: Memory) {
    if (!workspaceId) {
      return;
    }
    run(async () => {
      const result = await updateMemory(workspaceId, memory.id, { pinned: !memory.pinned });
      if (!report(result)) {
        return;
      }
      setNotice(memory.pinned ? "Semat dilepas." : "Disematkan: masuk lebih dulu saat batas token ketat.");
      await load();
    });
  }

  async function move(memory: Memory, target: string) {
    if (!workspaceId) {
      return;
    }
    run(async () => {
      const result =
        target === MAIN
          ? await updateMemory(workspaceId, memory.id, { move_to_main: true })
          : await updateMemory(workspaceId, memory.id, { agent_id: target });
      if (!report(result)) {
        return;
      }
      setNotice(
        target === MAIN
          ? "Dipindah ke memori utama: dipakai semua Bolu."
          : `Dipindah ke memori pribadi ${agents.find((agent) => agent.id === target)?.name ?? "Bolu yang sudah dihapus"}.`,
      );
      await load();
    });
  }

  async function forget(memory: Memory) {
    if (!workspaceId) {
      return;
    }
    run(async () => {
      const result = await deleteMemory(workspaceId, memory.id);
      if (!report(result)) {
        return;
      }
      setNotice("Memori dihapus permanen, termasuk vektornya.");
      await load();
    });
  }

  async function toggleExtractor(agent: Agent) {
    if (!workspaceId) {
      return;
    }
    run(async () => {
      const result = await setMemoryExtraction(workspaceId, agent.id, !agent.memory_extraction_enabled);
      if (!report(result)) {
        return;
      }
      setNotice(
        result.data.memory_extraction_enabled
          ? `${agent.name} mulai belajar dari percakapan lagi.`
          : `${agent.name} berhenti belajar otomatis. Memori yang sudah ada tetap dipakai.`,
      );
      await load();
    });
  }

  async function ingest(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!workspaceId) {
      return;
    }

    const form = event.currentTarget;
    const data = new FormData(form);
    const text = String(data.get("text") ?? "").trim();
    if (!text) {
      setError("Tempel dulu isi dokumennya.");
      return;
    }

    const layer = String(data.get("layer") ?? MAIN);
    run(async () => {
      const result = await ingestDocument(workspaceId, {
        source: String(data.get("source") ?? "").trim() || "tanpa nama",
        text,
        agent_id: layer === MAIN ? undefined : layer,
      });
      if (!report(result)) {
        return;
      }
      setNotice(`Dokumen disimpan sebagai ${result.data.length} potongan dan langsung bisa dicari.`);
      form.reset();
      await load();
    });
  }

  async function removeChunk(chunk: KnowledgeItem) {
    if (!workspaceId) {
      return;
    }
    run(async () => {
      const result = await deleteKnowledge(workspaceId, chunk.id);
      if (!report(result)) {
        return;
      }
      setNotice("Potongan dokumen dihapus, vektornya ikut.");
      await load();
    });
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
          <Link href="/dashboard">Dasbor</Link> / Pengaturan / Memori
        </p>
        <h1 className="mt-2 font-display text-3xl font-semibold">Memori &amp; pengetahuan</h1>
        <p className="mt-2 text-bolu-body">
          Memori utama dipakai semua Bolu dan dikirim di setiap panggilan. Memori pribadi hanya dipakai
          satu Bolu. Basis pengetahuan dicari per kesamaan dengan tugas yang sedang dikerjakan.
        </p>
      </header>

      {error ? <Alert tone="error">{error}</Alert> : null}
      {notice ? <Alert tone="success">{notice}</Alert> : null}

      <div className="mb-8 flex flex-wrap items-end gap-3">
        <label className="min-w-[240px] flex-1 text-sm">
          <span className="mb-1 block text-bolu-muted">Cari</span>
          <input
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder="cari isi memori…"
            className="w-full rounded-md border border-bolu-border bg-bolu-surface px-3 py-2"
          />
        </label>
        <label className="flex items-center gap-2 text-sm text-bolu-body">
          <input
            type="checkbox"
            checked={showHistory}
            onChange={(event) => setShowHistory(event.target.checked)}
            className="size-4"
          />
          Tampilkan versi lama
        </label>
        <span className="text-sm text-bolu-muted">{memories.length} memori tersimpan</span>
      </div>

      <div className="grid gap-8 lg:grid-cols-[1fr_360px]">
        <div className="space-y-8">
          <MemorySection
            title="Memori utama"
            note="Dipakai semua Bolu, dan dikirim di setiap panggilan."
            memories={filtered.main}
            agents={agents}
            busy={busy}
            onEdit={edit}
            onPin={togglePin}
            onMove={move}
            onForget={forget}
          />

          {agents.map((agent) => (
            <section key={agent.id}>
              <div className="mb-3 flex flex-wrap items-baseline justify-between gap-2">
                <h2 className="font-display text-xl font-semibold">
                  Memori {agent.name}
                  <span className="ml-2 text-sm font-normal text-bolu-muted">{agent.role || "tanpa peran"}</span>
                </h2>
                <label className="flex items-center gap-2 text-sm text-bolu-body">
                  <input
                    type="checkbox"
                    checked={agent.memory_extraction_enabled}
                    disabled={busy}
                    onChange={() => void toggleExtractor(agent)}
                    className="size-4"
                  />
                  Belajar otomatis dari percakapan
                </label>
              </div>

              <MemoryList
                memories={filtered.perAgent.get(agent.id) ?? []}
                layer={agent.id}
                agents={agents}
                busy={busy}
                emptyNote={`${agent.name} belum punya memori pribadi.`}
                onEdit={edit}
                onPin={togglePin}
                onMove={move}
                onForget={forget}
              />

              {agent.memory_extraction_enabled ? null : (
                <p className="mt-2 text-xs text-bolu-muted">
                  Belajar otomatis mati: hanya permintaan langsung yang disimpan.
                </p>
              )}
            </section>
          ))}

          <section>
            <div className="mb-3 flex items-baseline justify-between">
              <h2 className="font-display text-xl font-semibold">Basis pengetahuan</h2>
              <span className="text-sm text-bolu-muted">{knowledge.length} potongan</span>
            </div>

            {knowledge.length === 0 ? (
              <p className="rounded-md border border-dashed border-bolu-border px-4 py-6 text-sm text-bolu-muted">
                Belum ada dokumen. Tempel teks di kanan untuk mulai.
              </p>
            ) : (
              <ul className="space-y-2">
                {knowledge.map((chunk) => (
                  <li
                    key={chunk.id}
                    className="rounded-md border border-bolu-border bg-bolu-surface px-4 py-3"
                  >
                    <div className="flex items-start justify-between gap-3">
                      <div className="min-w-0">
                        <div className="flex flex-wrap items-center gap-2">
                          <span className="font-medium">{chunk.source || "tanpa nama"}</span>
                          <span className="rounded-full bg-bolu-panel px-2 py-0.5 text-xs text-bolu-muted">
                            potongan {chunk.chunk_index + 1}
                          </span>
                          <span
                            className={`rounded-full px-2 py-0.5 text-xs font-medium ${STATUS_CLASS[chunk.status] ?? STATUS_CLASS.pending}`}
                          >
                            {STATUS_LABEL[chunk.status] ?? chunk.status}
                          </span>
                        </div>
                        <p className="mt-1 line-clamp-3 text-sm text-bolu-body">{chunk.content}</p>
                        <p className="mt-1 text-xs text-bolu-muted">{formatDate(chunk.created_at)}</p>
                      </div>
                      <button
                        type="button"
                        onClick={() => void removeChunk(chunk)}
                        disabled={busy}
                        className="shrink-0 rounded-md border border-bolu-border px-2.5 py-1 text-sm text-bolu-belum transition hover:bg-bolu-belum/8 disabled:opacity-50"
                      >
                        Hapus
                      </button>
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </section>
        </div>

        <aside className="space-y-8">
          <form onSubmit={add} className="rounded-md border border-bolu-border bg-bolu-surface p-4">
            <h2 className="mb-3 font-display text-lg font-semibold">Tambah memori</h2>

            <label className="mb-3 block text-sm">
              <span className="mb-1 block text-bolu-muted">Isi</span>
              <textarea
                name="content"
                rows={3}
                placeholder="Ongkir Bandung 18 ribu"
                className="w-full rounded-md border border-bolu-border bg-white px-3 py-2"
              />
            </label>

            <label className="mb-3 block text-sm">
              <span className="mb-1 block text-bolu-muted">Jenis</span>
              <select name="kind" className="w-full rounded-md border border-bolu-border bg-white px-3 py-2">
                {KIND_ORDER.map((kind) => (
                  <option key={kind} value={kind}>
                    {KIND_LABEL[kind]}
                  </option>
                ))}
              </select>
            </label>

            <label className="mb-3 block text-sm">
              <span className="mb-1 block text-bolu-muted">Lapisan</span>
              <select name="layer" className="w-full rounded-md border border-bolu-border bg-white px-3 py-2">
                <option value={MAIN}>Memori utama (semua Bolu)</option>
                {agents.map((agent) => (
                  <option key={agent.id} value={agent.id}>
                    Pribadi — {agent.name}
                  </option>
                ))}
              </select>
            </label>

            <label className="mb-3 flex items-center gap-2 text-sm text-bolu-body">
              <input type="checkbox" name="pinned" className="size-4" />
              Sematkan
            </label>

            <button
              type="submit"
              disabled={busy}
              className="min-h-10 w-full rounded-full bg-bolu-accent px-4 font-semibold text-white disabled:opacity-60"
            >
              Simpan
            </button>
            <p className="mt-2 text-xs text-bolu-muted">
              Isi yang mengandung kata sandi, kode OTP, nomor kartu, atau nomor identitas ditolak.
            </p>
          </form>

          <form onSubmit={ingest} className="rounded-md border border-bolu-border bg-bolu-surface p-4">
            <h2 className="mb-3 font-display text-lg font-semibold">Tambah dokumen</h2>

            <label className="mb-3 block text-sm">
              <span className="mb-1 block text-bolu-muted">Nama sumber</span>
              <input
                name="source"
                placeholder="katalog-ongkir.txt"
                className="w-full rounded-md border border-bolu-border bg-white px-3 py-2"
              />
            </label>

            <label className="mb-3 block text-sm">
              <span className="mb-1 block text-bolu-muted">Isi</span>
              <textarea
                name="text"
                rows={5}
                placeholder="Tempel teks dokumen di sini."
                className="w-full rounded-md border border-bolu-border bg-white px-3 py-2"
              />
            </label>

            <label className="mb-3 block text-sm">
              <span className="mb-1 block text-bolu-muted">Lapisan</span>
              <select name="layer" className="w-full rounded-md border border-bolu-border bg-white px-3 py-2">
                <option value={MAIN}>Berlaku untuk seluruh workspace</option>
                {agents.map((agent) => (
                  <option key={agent.id} value={agent.id}>
                    Hanya untuk {agent.name}
                  </option>
                ))}
              </select>
            </label>

            <button
              type="submit"
              disabled={busy}
              className="min-h-10 w-full rounded-full bg-bolu-ink px-4 font-semibold text-white disabled:opacity-60"
            >
              Simpan &amp; indeks
            </button>
            <p className="mt-2 text-xs text-bolu-muted">
              Teks dipotong jadi beberapa bagian, masing-masing dibuatkan embedding. Potongan yang gagal
              tetap terlihat di daftar dengan status Gagal.
            </p>
          </form>
        </aside>
      </div>
    </main>
  );
}

/** MemorySection is one layer with its heading. */
function MemorySection({
  title,
  note,
  memories,
  agents,
  busy,
  onEdit,
  onPin,
  onMove,
  onForget,
}: {
  title: string;
  note: string;
  memories: Memory[];
  agents: Agent[];
  busy: boolean;
  onEdit: (memory: Memory, event: React.FormEvent<HTMLFormElement>) => Promise<boolean>;
  onPin: (memory: Memory) => Promise<void>;
  onMove: (memory: Memory, target: string) => Promise<void>;
  onForget: (memory: Memory) => Promise<void>;
}) {
  return (
    <section>
      <h2 className="font-display text-xl font-semibold">{title}</h2>
      <p className="mb-3 text-sm text-bolu-muted">{note}</p>
      <MemoryList
        memories={memories}
        layer={MAIN}
        agents={agents}
        busy={busy}
        emptyNote="Belum ada yang diingat di lapisan ini."
        onEdit={onEdit}
        onPin={onPin}
        onMove={onMove}
        onForget={onForget}
      />
    </section>
  );
}

/** MemoryList is one layer's rows, each editable in place. */
function MemoryList({
  memories,
  layer,
  agents,
  busy,
  emptyNote,
  onEdit,
  onPin,
  onMove,
  onForget,
}: {
  memories: Memory[];
  layer: string;
  agents: Agent[];
  busy: boolean;
  emptyNote: string;
  onEdit: (memory: Memory, event: React.FormEvent<HTMLFormElement>) => Promise<boolean>;
  onPin: (memory: Memory) => Promise<void>;
  onMove: (memory: Memory, target: string) => Promise<void>;
  onForget: (memory: Memory) => Promise<void>;
}) {
  const [editing, setEditing] = useState<string | null>(null);

  if (memories.length === 0) {
    return (
      <p className="rounded-md border border-dashed border-bolu-border px-4 py-6 text-sm text-bolu-muted">
        {emptyNote}
      </p>
    );
  }

  return (
    <ul className="space-y-2">
      {memories.map((memory) => (
        <li key={memory.id} className="rounded-md border border-bolu-border bg-bolu-surface px-4 py-3">
          <div className="flex items-start justify-between gap-3">
            <div className="min-w-0">
              <div className="mb-1 flex flex-wrap items-center gap-2">
                <span className="rounded-full bg-bolu-panel px-2 py-0.5 text-xs font-medium text-bolu-muted">
                  {KIND_LABEL[memory.kind] ?? memory.kind}
                </span>
                {memory.pinned ? (
                  <span className="rounded-full bg-bolu-flag/12 px-2 py-0.5 text-xs font-medium text-bolu-flag">
                    disematkan
                  </span>
                ) : null}
                {memory.superseded_by ? (
                  <span className="rounded-full bg-bolu-panel px-2 py-0.5 text-xs text-bolu-rest">
                    digantikan
                  </span>
                ) : null}
                <span className="rounded-full bg-bolu-panel px-2 py-0.5 text-xs text-bolu-muted">
                  {memory.created_by === "user" ? "diminta" : "otomatis"}
                </span>
              </div>

              {editing === memory.id ? (
                <form
                  onSubmit={(event) => {
                    void onEdit(memory, event).then((saved) => {
                      if (saved) {
                        setEditing(null);
                      }
                    });
                  }}
                  className="mt-2 space-y-2"
                >
                  <textarea
                    name="content"
                    rows={3}
                    defaultValue={memory.content}
                    className="w-full rounded-md border border-bolu-border bg-white px-3 py-2 text-sm"
                  />
                  <select
                    name="kind"
                    defaultValue={memory.kind}
                    className="rounded-md border border-bolu-border bg-white px-3 py-2 text-sm"
                  >
                    {KIND_ORDER.map((kind) => (
                      <option key={kind} value={kind}>
                        {KIND_LABEL[kind]}
                      </option>
                    ))}
                  </select>
                  <div className="flex gap-2">
                    <button
                      type="submit"
                      disabled={busy}
                      className="min-h-9 rounded-full bg-bolu-accent px-3.5 text-sm font-semibold text-white disabled:opacity-60"
                    >
                      Simpan
                    </button>
                    <button
                      type="button"
                      onClick={() => setEditing(null)}
                      className="min-h-9 rounded-full border border-bolu-border px-3.5 text-sm font-semibold"
                    >
                      Batal
                    </button>
                  </div>
                </form>
              ) : (
                <>
                  <p className="text-bolu-body">{memory.content}</p>
                  <p className="mt-1 text-xs text-bolu-muted">
                    {formatDate(memory.updated_at)}
                    {memory.source_message_id ? (
                      <>
                        {" · "}
                        <Link href="/chat" className="underline">
                          dari percakapan
                        </Link>
                      </>
                    ) : null}
                  </p>
                </>
              )}
            </div>

            <div className="flex shrink-0 flex-col gap-1 text-sm">
              <button
                type="button"
                onClick={() => setEditing(editing === memory.id ? null : memory.id)}
                className="rounded-md border border-bolu-border px-2.5 py-1 transition hover:bg-bolu-panel/60"
              >
                {editing === memory.id ? "Tutup" : "Ubah"}
              </button>
              <button
                type="button"
                onClick={() => void onPin(memory)}
                disabled={busy}
                className="rounded-md border border-bolu-border px-2.5 py-1 transition hover:bg-bolu-panel/60 disabled:opacity-50"
              >
                {memory.pinned ? "Lepas" : "Sematkan"}
              </button>
              <button
                type="button"
                onClick={() => void onForget(memory)}
                disabled={busy}
                className="rounded-md border border-bolu-border px-2.5 py-1 text-bolu-belum transition hover:bg-bolu-belum/8 disabled:opacity-50"
              >
                Hapus
              </button>
            </div>
          </div>

          {memory.superseded_by ? (
            <p className="mt-2 text-xs text-bolu-muted">
              Versi lama: tidak lagi dikirim ke model, disimpan sebagai riwayat.
            </p>
          ) : null}

          <div className="mt-2 flex flex-wrap items-center gap-1 text-xs">
            <span className="text-bolu-muted">Pindah ke:</span>
            {layer !== MAIN ? (
              <button
                type="button"
                onClick={() => void onMove(memory, MAIN)}
                disabled={busy}
                className="rounded-full border border-bolu-border px-2 py-0.5 transition hover:bg-bolu-panel/60 disabled:opacity-50"
              >
                Memori utama
              </button>
            ) : null}
            {agents
              .filter((agent) => agent.id !== layer)
              .map((agent) => (
                <button
                  key={agent.id}
                  type="button"
                  onClick={() => void onMove(memory, agent.id)}
                  disabled={busy}
                  className="rounded-full border border-bolu-border px-2 py-0.5 transition hover:bg-bolu-panel/60 disabled:opacity-50"
                >
                  {agent.name}
                </button>
              ))}
          </div>
        </li>
      ))}
    </ul>
  );
}
