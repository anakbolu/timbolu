"use client";

/**
 * The model screen: which providers the workspace may call, in which order, and
 * what they have spent so far.
 *
 * Two things are deliberate here:
 * - the API never returns an API key, so the form shows whether one is stored
 *   and leaves the field empty to keep it;
 * - the fallback order is visible, because "which provider answered" is what the
 *   cost report and the outage behaviour both depend on.
 */

import Link from "next/link";
import { useCallback, useEffect, useState } from "react";

import { Alert } from "@/components/auth/auth-shell";
import {
  Agent,
  AgentModelOverride,
  ApiResult,
  ModelProvider,
  ModelProviderInput,
  ProviderCapabilities,
  UsageDay,
  createModelProvider,
  deleteModelProvider,
  ensureActiveWorkspace,
  getAgentModel,
  listAgentTeams,
  listModelAdapters,
  listModelProviders,
  modelUsage,
  setAgentModel,
  testModelProvider,
  updateModelProvider,
} from "@/lib/api";

const ADAPTER_LABEL: Record<string, string> = {
  openai: "OpenAI-compatible",
  anthropic: "Anthropic",
};

const PURPOSE_LABEL: Record<string, string> = {
  agent: "Tugas Bolu",
  routing: "Uji koneksi",
  extraction: "Ekstraksi",
  embedding: "Embedding",
};

/** formatRupiah turns the micro-rupiah the API reports into rupiah. */
function formatRupiah(micros: number): string {
  return new Intl.NumberFormat("id-ID", {
    style: "currency",
    currency: "IDR",
    maximumFractionDigits: 2,
  }).format(micros / 1_000_000);
}

function formatTokens(tokens: number): string {
  return new Intl.NumberFormat("id-ID").format(tokens);
}

/**
 * describeOverride says which model a Bolu will actually use.
 *
 * A pinned provider means "this endpoint and its model", a bare model means
 * "the workspace chain with a different model", and neither means "the chain as
 * it is". The three are different answers, so they read differently.
 */
function describeOverride(override: AgentModelOverride | null, providers: ModelProvider[]): string {
  if (!override) {
    return "Sekarang: memuat…";
  }

  const pinned = providers.find((provider) => provider.id === override.provider_id);
  if (pinned) {
    return `Sekarang: ${pinned.name} (${pinned.model})${override.max_tokens ? ` · maks ${formatTokens(override.max_tokens)} token` : ""}`;
  }
  if (override.model) {
    return `Sekarang: ${override.model}${override.adapter ? ` (${override.adapter})` : ""}`;
  }
  return "Sekarang: mengikuti rantai workspace.";
}

export function ModelView() {
  const [providers, setProviders] = useState<ModelProvider[]>([]);
  const [adapters, setAdapters] = useState<string[]>(["openai", "anthropic"]);
  const [agents, setAgents] = useState<Agent[]>([]);
  const [usage, setUsage] = useState<UsageDay[]>([]);
  const [editing, setEditing] = useState<ModelProvider | null>(null);
  const [capabilities, setCapabilities] = useState<{ provider: string; result: ProviderCapabilities } | null>(null);
  const [selectedAgent, setSelectedAgent] = useState<string>("");
  const [override, setOverride] = useState<AgentModelOverride | null>(null);
  const [workspaceId, setWorkspaceId] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

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

    const [providerResult, adapterResult, teamResult, usageResult] = await Promise.all([
      listModelProviders(active),
      listModelAdapters(),
      listAgentTeams(active),
      modelUsage(active, 30),
    ]);

    if (!providerResult.ok) {
      setError(providerResult.message);
      return;
    }
    setProviders(providerResult.data);
    if (adapterResult.ok) {
      setAdapters(adapterResult.data.adapters);
    }
    if (teamResult.ok) {
      setAgents(teamResult.data.flatMap((team) => team.agents));
    }
    setUsage(usageResult.ok ? usageResult.data : []);
  }, []);

  useEffect(() => {
    void (async () => {
      await load();
    })();
  }, [load]);

  useEffect(() => {
    void (async () => {
      if (!workspaceId || !selectedAgent) {
        setOverride(null);
        return;
      }
      const result = await getAgentModel(workspaceId, selectedAgent);
      setOverride(result.ok ? result.data : null);
    })();
  }, [workspaceId, selectedAgent]);

  function report<T>(result: ApiResult<T>): result is Extract<ApiResult<T>, { ok: true }> {
    if (result.ok) {
      return true;
    }
    setError(result.message);
    return false;
  }

  async function saveProvider(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!workspaceId) {
      return;
    }

    const form = event.currentTarget;
    const data = new FormData(form);
    const input: ModelProviderInput = {
      name: String(data.get("name") ?? ""),
      adapter: String(data.get("adapter") ?? "openai"),
      base_url: String(data.get("base_url") ?? ""),
      model: String(data.get("model") ?? ""),
      priority: Number(data.get("priority") ?? 100),
      max_tokens: Number(data.get("max_tokens") ?? 0),
      context_tokens: Number(data.get("context_tokens") ?? 0),
      is_default: data.get("is_default") === "on",
      enabled: data.get("enabled") === "on",
    };

    const apiKey = String(data.get("api_key") ?? "").trim();
    if (apiKey) {
      input.api_key = apiKey;
    }
    if (editing && data.get("clear_api_key") === "on") {
      input.clear_api_key = true;
    }

    setBusy(true);
    const result = editing
      ? await updateModelProvider(workspaceId, editing.id, input)
      : await createModelProvider(workspaceId, input);
    setBusy(false);

    if (!report(result)) {
      return;
    }
    setNotice(editing ? "Penyedia diperbarui." : "Penyedia ditambahkan ke rantai fallback.");
    setEditing(null);
    form.reset();
    await load();
  }

  async function remove(provider: ModelProvider) {
    if (!workspaceId) {
      return;
    }
    setBusy(true);
    const result = await deleteModelProvider(workspaceId, provider.id);
    setBusy(false);

    if (!report(result)) {
      return;
    }
    setNotice(`${provider.name} dihapus dari rantai.`);
    await load();
  }

  async function test(provider: ModelProvider) {
    if (!workspaceId) {
      return;
    }
    setBusy(true);
    setCapabilities(null);
    const result = await testModelProvider(workspaceId, provider.id);
    setBusy(false);

    if (!report(result)) {
      return;
    }
    setCapabilities({ provider: provider.name, result: result.data });
    setNotice(`${provider.name} menjawab. Panggilan uji ikut tercatat di pemakaian.`);
    await load();
  }

  async function saveOverride(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!workspaceId || !selectedAgent) {
      return;
    }

    const data = new FormData(event.currentTarget);
    const providerID = String(data.get("provider_id") ?? "");
    setBusy(true);
    const result = await setAgentModel(workspaceId, selectedAgent, {
      provider_id: providerID,
      model: String(data.get("model") ?? ""),
      max_tokens: Number(data.get("max_tokens") ?? 0),
    });
    setBusy(false);

    if (!report(result)) {
      return;
    }
    setOverride(result.data);
    setNotice("Model Bolu disimpan. Tugas berikutnya memakainya.");
  }

  const totalCost = usage.reduce((sum, day) => sum + day.cost_micros, 0);
  const totalTokens = usage.reduce((sum, day) => sum + day.total_tokens, 0);

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
          <Link href="/dashboard">Dasbor</Link> / Pengaturan / Model
        </p>
        <h1 className="mt-2 font-display text-3xl font-semibold">Model &amp; penyedia</h1>
        <p className="mt-2 text-bolu-body">
          Rantai fallback dipakai berurutan: default dulu, lalu prioritas terkecil. Kalau satu penyedia
          menolak atau mati, Bolu pindah ke penyedia berikutnya dan yang menjawab tercatat di pemakaian.
        </p>
      </header>

      {error ? <Alert tone="error">{error}</Alert> : null}
      {notice ? <Alert tone="success">{notice}</Alert> : null}

      <div className="grid gap-8 lg:grid-cols-[1fr_380px]">
        <div className="space-y-8">
          <section>
            <div className="mb-3 flex items-baseline justify-between">
              <h2 className="font-display text-xl font-semibold">Rantai penyedia</h2>
              <span className="text-sm text-bolu-muted">{providers.length} penyedia</span>
            </div>

            {providers.length === 0 ? (
              <p className="rounded-2xl border border-dashed border-bolu-border px-4 py-6 text-sm text-bolu-muted">
                Belum ada penyedia. Tambahkan satu di kanan: tanpa penyedia, Bolu tidak bisa memanggil model.
              </p>
            ) : (
              <ul className="space-y-2">
                {providers.map((provider) => (
                  <li
                    key={provider.id}
                    className="rounded-2xl border border-bolu-border bg-bolu-surface px-4 py-3"
                  >
                    <div className="flex items-start justify-between gap-3">
                      <div className="min-w-0">
                        <div className="flex flex-wrap items-center gap-2">
                          <span className="font-medium">{provider.name}</span>
                          {provider.is_default ? (
                            <span className="rounded-full bg-bolu-live/12 px-2 py-0.5 text-xs font-medium text-bolu-lunas">
                              default
                            </span>
                          ) : null}
                          {!provider.enabled ? (
                            <span className="rounded-full bg-bolu-panel px-2 py-0.5 text-xs font-medium text-bolu-rest">
                              nonaktif
                            </span>
                          ) : null}
                          <span className="rounded-full bg-bolu-panel px-2 py-0.5 text-xs text-bolu-muted">
                            {ADAPTER_LABEL[provider.adapter] ?? provider.adapter}
                          </span>
                        </div>
                        <p className="mt-1 truncate text-sm text-bolu-muted">
                          {provider.model}
                          {provider.base_url ? ` · ${provider.base_url}` : ""}
                        </p>
                        <p className="mt-1 text-xs text-bolu-muted">
                          prioritas {provider.priority} ·{" "}
                          {provider.has_api_key ? "kunci tersimpan" : "belum ada kunci"} ·{" "}
                          {provider.context_tokens > 0
                            ? `${formatTokens(provider.context_tokens)} token konteks`
                            : "konteks default"}
                        </p>
                      </div>
                      <div className="flex shrink-0 flex-col gap-1 text-sm">
                        <button
                          type="button"
                          onClick={() => void test(provider)}
                          disabled={busy}
                          className="rounded-lg border border-bolu-border px-2.5 py-1 transition hover:bg-bolu-panel/60 disabled:opacity-50"
                        >
                          Uji
                        </button>
                        <button
                          type="button"
                          onClick={() => setEditing(provider)}
                          className="rounded-lg border border-bolu-border px-2.5 py-1 transition hover:bg-bolu-panel/60"
                        >
                          Ubah
                        </button>
                        <button
                          type="button"
                          onClick={() => void remove(provider)}
                          disabled={busy}
                          className="rounded-lg border border-bolu-border px-2.5 py-1 text-bolu-belum transition hover:bg-bolu-belum/8 disabled:opacity-50"
                        >
                          Hapus
                        </button>
                      </div>
                    </div>
                  </li>
                ))}
              </ul>
            )}

            {capabilities ? (
              <div className="mt-3 rounded-2xl border border-bolu-border bg-bolu-panel/50 px-4 py-3 text-sm text-bolu-body">
                <p className="font-medium">{capabilities.provider} menjawab dengan:</p>
                <p className="mt-1">
                  alat {capabilities.result.tools ? "ya" : "tidak"} · streaming{" "}
                  {capabilities.result.streaming ? "ya" : "tidak"} · gambar{" "}
                  {capabilities.result.vision ? "ya" : "tidak"} · cache prompt{" "}
                  {capabilities.result.prompt_caching ? "ya" : "tidak"} ·{" "}
                  {formatTokens(capabilities.result.max_context_tokens)} token konteks
                </p>
              </div>
            ) : null}
          </section>

          <section>
            <h2 className="mb-3 font-display text-xl font-semibold">Model per Bolu</h2>
            <p className="mb-3 text-sm text-bolu-muted">
              Kosongkan pilihan penyedia untuk mengikuti rantai workspace.
            </p>

            <label className="mb-3 block text-sm">
              <span className="mb-1 block text-bolu-muted">Bolu</span>
              <select
                value={selectedAgent}
                onChange={(event) => setSelectedAgent(event.target.value)}
                className="w-full rounded-xl border border-bolu-border bg-bolu-surface px-3 py-2"
              >
                <option value="">— pilih Bolu —</option>
                {agents.map((agent) => (
                  <option key={agent.id} value={agent.id}>
                    {agent.name} · {agent.role || "tanpa peran"}
                  </option>
                ))}
              </select>
            </label>

            {selectedAgent ? (
              <form
                onSubmit={saveOverride}
                key={`${selectedAgent}-${override?.provider_id ?? ""}-${override?.model ?? ""}-${override?.max_tokens ?? 0}`}
                className="space-y-3 rounded-2xl border border-bolu-border bg-bolu-surface px-4 py-3"
              >
                <p className="text-sm text-bolu-muted">
                  {describeOverride(override, providers)}
                </p>
                <label className="block text-sm">
                  <span className="mb-1 block text-bolu-muted">Penyedia</span>
                  <select
                    name="provider_id"
                    defaultValue={override?.provider_id ?? ""}
                    className="w-full rounded-xl border border-bolu-border bg-bolu-surface px-3 py-2"
                  >
                    <option value="">Ikut rantai workspace</option>
                    {providers.map((provider) => (
                      <option key={provider.id} value={provider.id}>
                        {provider.name} ({provider.model})
                      </option>
                    ))}
                  </select>
                </label>
                <div className="grid gap-3 sm:grid-cols-2">
                  <label className="block text-sm">
                    <span className="mb-1 block text-bolu-muted">Model khusus (opsional)</span>
                    <input
                      name="model"
                      defaultValue={override?.model ?? ""}
                      placeholder="mis. gpt-4o-mini"
                      className="w-full rounded-xl border border-bolu-border bg-bolu-surface px-3 py-2"
                    />
                  </label>
                  <label className="block text-sm">
                    <span className="mb-1 block text-bolu-muted">Batas token jawaban</span>
                    <input
                      name="max_tokens"
                      type="number"
                      min={0}
                      defaultValue={override?.max_tokens ?? 0}
                      className="w-full rounded-xl border border-bolu-border bg-bolu-surface px-3 py-2"
                    />
                  </label>
                </div>
                <button
                  type="submit"
                  disabled={busy}
                  className="rounded-xl bg-bolu-accent px-4 py-2 text-sm font-medium text-white transition hover:opacity-90 disabled:opacity-50"
                >
                  Simpan model
                </button>
              </form>
            ) : null}
          </section>

          <section>
            <div className="mb-3 flex items-baseline justify-between">
              <h2 className="font-display text-xl font-semibold">Pemakaian 30 hari</h2>
              <span className="text-sm text-bolu-muted">
                {formatTokens(totalTokens)} token · {formatRupiah(totalCost)}
              </span>
            </div>

            {usage.length === 0 ? (
              <p className="rounded-2xl border border-dashed border-bolu-border px-4 py-6 text-sm text-bolu-muted">
                Belum ada panggilan model yang tercatat.
              </p>
            ) : (
              <div className="overflow-hidden rounded-2xl border border-bolu-border">
                <table className="w-full text-sm">
                  <thead className="bg-bolu-panel/60 text-left text-bolu-muted">
                    <tr>
                      <th className="px-3 py-2 font-medium">Hari</th>
                      <th className="px-3 py-2 font-medium">Model</th>
                      <th className="px-3 py-2 font-medium">Untuk</th>
                      <th className="px-3 py-2 text-right font-medium">Panggilan</th>
                      <th className="px-3 py-2 text-right font-medium">Token</th>
                      <th className="px-3 py-2 text-right font-medium">Biaya</th>
                    </tr>
                  </thead>
                  <tbody>
                    {usage.map((day) => (
                      <tr key={`${day.day}-${day.provider}-${day.model}-${day.purpose}`} className="border-t border-bolu-line">
                        <td className="px-3 py-2">{day.day}</td>
                        <td className="px-3 py-2">{day.model}</td>
                        <td className="px-3 py-2 text-bolu-muted">{PURPOSE_LABEL[day.purpose] ?? day.purpose}</td>
                        <td className="px-3 py-2 text-right">{day.calls}</td>
                        <td className="px-3 py-2 text-right">{formatTokens(day.total_tokens)}</td>
                        <td className="px-3 py-2 text-right">{formatRupiah(day.cost_micros)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </section>
        </div>

        <section>
          <h2 className="mb-3 font-display text-xl font-semibold">
            {editing ? `Ubah ${editing.name}` : "Tambah penyedia"}
          </h2>

          <form onSubmit={saveProvider} className="space-y-3 rounded-2xl border border-bolu-border bg-bolu-surface px-4 py-4">
            <label className="block text-sm">
              <span className="mb-1 block text-bolu-muted">Nama</span>
              <input
                name="name"
                required
                defaultValue={editing?.name ?? ""}
                placeholder="mis. OpenRouter (utama)"
                className="w-full rounded-xl border border-bolu-border bg-bolu-surface px-3 py-2"
              />
            </label>

            <label className="block text-sm">
              <span className="mb-1 block text-bolu-muted">Format</span>
              <select
                name="adapter"
                defaultValue={editing?.adapter ?? adapters[0] ?? "openai"}
                className="w-full rounded-xl border border-bolu-border bg-bolu-surface px-3 py-2"
              >
                {adapters.map((adapter) => (
                  <option key={adapter} value={adapter}>
                    {ADAPTER_LABEL[adapter] ?? adapter}
                  </option>
                ))}
              </select>
            </label>

            <label className="block text-sm">
              <span className="mb-1 block text-bolu-muted">Alamat endpoint (opsional)</span>
              <input
                name="base_url"
                defaultValue={editing?.base_url ?? ""}
                placeholder="kosong = endpoint resmi"
                className="w-full rounded-xl border border-bolu-border bg-bolu-surface px-3 py-2"
              />
            </label>

            <label className="block text-sm">
              <span className="mb-1 block text-bolu-muted">Model</span>
              <input
                name="model"
                required
                defaultValue={editing?.model ?? ""}
                placeholder="mis. gpt-4o-mini"
                className="w-full rounded-xl border border-bolu-border bg-bolu-surface px-3 py-2"
              />
            </label>

            <label className="block text-sm">
              <span className="mb-1 block text-bolu-muted">
                Kunci API {editing?.has_api_key ? "(sudah tersimpan)" : ""}
              </span>
              <input
                name="api_key"
                type="password"
                autoComplete="off"
                placeholder={editing?.has_api_key ? "biarkan kosong untuk mempertahankan kunci" : "sk-…"}
                className="w-full rounded-xl border border-bolu-border bg-bolu-surface px-3 py-2"
              />
            </label>

            {editing?.has_api_key ? (
              <label className="flex items-center gap-2 text-sm text-bolu-muted">
                <input type="checkbox" name="clear_api_key" />
                Hapus kunci yang tersimpan
              </label>
            ) : null}

            <div className="grid gap-3 sm:grid-cols-3">
              <label className="block text-sm">
                <span className="mb-1 block text-bolu-muted">Prioritas</span>
                <input
                  name="priority"
                  type="number"
                  min={0}
                  defaultValue={editing?.priority ?? 100}
                  className="w-full rounded-xl border border-bolu-border bg-bolu-surface px-3 py-2"
                />
              </label>
              <label className="block text-sm">
                <span className="mb-1 block text-bolu-muted">Batas jawaban</span>
                <input
                  name="max_tokens"
                  type="number"
                  min={0}
                  defaultValue={editing?.max_tokens ?? 0}
                  className="w-full rounded-xl border border-bolu-border bg-bolu-surface px-3 py-2"
                />
              </label>
              <label className="block text-sm">
                <span className="mb-1 block text-bolu-muted">Konteks</span>
                <input
                  name="context_tokens"
                  type="number"
                  min={0}
                  defaultValue={editing?.context_tokens ?? 0}
                  className="w-full rounded-xl border border-bolu-border bg-bolu-surface px-3 py-2"
                />
              </label>
            </div>

            <label className="flex items-center gap-2 text-sm">
              <input type="checkbox" name="is_default" defaultChecked={editing?.is_default ?? providers.length === 0} />
              Jadikan default
            </label>
            <label className="flex items-center gap-2 text-sm">
              <input type="checkbox" name="enabled" defaultChecked={editing?.enabled ?? true} />
              Aktif
            </label>

            <div className="flex items-center gap-2">
              <button
                type="submit"
                disabled={busy}
                className="rounded-xl bg-bolu-accent px-4 py-2 text-sm font-medium text-white transition hover:opacity-90 disabled:opacity-50"
              >
                {editing ? "Simpan perubahan" : "Tambah"}
              </button>
              {editing ? (
                <button
                  type="button"
                  onClick={() => setEditing(null)}
                  className="rounded-xl border border-bolu-border px-4 py-2 text-sm transition hover:bg-bolu-panel/60"
                >
                  Batal
                </button>
              ) : null}
            </div>
          </form>
        </section>
      </div>
    </main>
  );
}