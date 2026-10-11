"use client";

/**
 * The approval surfaces: the drafts waiting for a decision, the policies that
 * decide whether a person is asked, and the trail of what was decided.
 *
 * Everything here reads and writes the same drafts the chat card does, so the two
 * cannot disagree: a decision made in the thread shows up here on the next read,
 * and vice versa.
 */

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  decideDraft,
  ensureActiveWorkspace,
  listApprovalPolicies,
  listAudit,
  listDrafts,
  listDraftRevisions,
  refreshSession,
  setApprovalPolicy,
  type ApprovalPolicy,
  type AuditEntry,
  type Draft,
  type DraftRevision,
} from "@/lib/api";

const STATUS_LABEL: Record<string, string> = {
  pending: "Menunggu kamu",
  approved: "Disetujui",
  revise: "Minta revisi",
  sent: "Terkirim",
  canceled: "Dibatalkan",
};

const STATUS_CLASS: Record<string, string> = {
  pending: "bg-bolu-flag/12 text-bolu-flag",
  approved: "bg-bolu-live/12 text-bolu-lunas",
  revise: "bg-bolu-panel text-bolu-body",
  sent: "bg-bolu-live/12 text-bolu-lunas",
  canceled: "bg-bolu-panel text-bolu-rest",
};

/** AUDIT_LABEL names what happened, in the user's words. */
const AUDIT_LABEL: Record<string, string> = {
  "draft.created": "Draf dibuat",
  "draft.approved": "Draf disetujui",
  "draft.revised": "Draf diminta revisi",
  "draft.sent": "Aksi dikirim",
  "draft.canceled": "Draf dibatalkan",
  "policy.updated": "Kebijakan diubah",
};

type Tab = "drafts" | "policies" | "audit";

/** ApprovalsView is the dashboard's approval pane. */
export function ApprovalsView() {
  const [tab, setTab] = useState<Tab>("drafts");
  const [drafts, setDrafts] = useState<Draft[]>([]);
  const [policies, setPolicies] = useState<ApprovalPolicy[]>([]);
  const [audit, setAudit] = useState<AuditEntry[]>([]);

  const [statusFilter, setStatusFilter] = useState("pending");
  const [agentFilter, setAgentFilter] = useState("");
  const [kindFilter, setKindFilter] = useState("");

  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [trail, setTrail] = useState<{ draft: Draft; revisions: DraftRevision[] } | null>(null);
  const [workspaceId, setWorkspaceId] = useState<string | null>(null);

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

    const [draftResult, policyResult, auditResult] = await Promise.all([
      listDrafts(active, {
        status: statusFilter,
        agent_id: agentFilter,
        action_kind: kindFilter,
      }),
      listApprovalPolicies(active),
      listAudit(active, 50),
    ]);

    if (!draftResult.ok) {
      setError(draftResult.message);
      return;
    }
    setError(null);
    setDrafts(draftResult.data);
    if (policyResult.ok) setPolicies(policyResult.data);
    if (auditResult.ok) setAudit(auditResult.data);
  }, [statusFilter, agentFilter, kindFilter]);

  // The load is wrapped so the effect's body does not call setState directly:
  // the state updates land in the promise's own turn rather than during render.
  useEffect(() => {
    void (async () => {
      await load();
    })();
  }, [load]);

  /** decide records one answer, then re-reads so both surfaces agree. */
  async function decide(draft: Draft, decision: "approve" | "revise" | "cancel", note = "") {
    if (!workspaceId) return;

    setBusy(true);
    setNotice(null);

    const result = await decideDraft(workspaceId, draft.draft_id, decision, note);
    setBusy(false);

    if (!result.ok) {
      setError(
        result.code === "draft_already_decided"
          ? "Draf ini sudah diputuskan."
          : result.message,
      );
      return;
    }

    setNotice(
      decision === "approve"
        ? "Draf disetujui. Aksi dikirim pada jam kerja berikutnya bila di luar jam kerja."
        : decision === "revise"
          ? "Revisi dikirim ke Bolu yang mengerjakannya."
          : "Draf dibatalkan.",
    );
    await load();
  }

  async function openTrail(draft: Draft) {
    if (!workspaceId) return;

    const result = await listDraftRevisions(workspaceId, draft.draft_id);
    setTrail({ draft, revisions: result.ok ? result.data : [] });
  }

  async function savePolicy(policy: ApprovalPolicy, mode: string, maxAmount: number | null) {
    if (!workspaceId) return;

    setBusy(true);
    const result = await setApprovalPolicy(workspaceId, {
      action_kind: policy.action_kind,
      mode,
      max_amount: mode === "auto" ? maxAmount : null,
    });
    setBusy(false);

    if (!result.ok) {
      setError(result.message);
      return;
    }

    setNotice(`${policy.action_kind}: kebijakan disimpan.`);
    await load();
  }

  const kinds = useMemo(
    () => Array.from(new Set(drafts.map((draft) => draft.action_kind))).sort(),
    [drafts],
  );

  return (
    <section className="space-y-4">
      <header className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold">Persetujuan</h2>
          <p className="text-sm text-bolu-muted">
            Manusia memegang pintu keluar: aksi ke pihak luar berhenti di sini sampai kamu memutuskan.
          </p>
        </div>

        <nav className="flex gap-1 rounded-full bg-bolu-panel p-1">
          {(
            [
              ["drafts", "Draf"],
              ["policies", "Kebijakan"],
              ["audit", "Jejak"],
            ] as [Tab, string][]
          ).map(([value, label]) => (
            <button
              key={value}
              type="button"
              onClick={() => setTab(value)}
              className={`rounded-full px-3.5 py-1.5 text-sm font-medium ${
                tab === value ? "bg-bolu-surface shadow-sm" : "text-bolu-muted"
              }`}
            >
              {label}
            </button>
          ))}
        </nav>
      </header>

      {error ? (
        <p className="rounded-xl bg-bolu-flag/10 px-3 py-2 text-sm text-bolu-flag">{error}</p>
      ) : null}
      {notice ? (
        <p className="rounded-xl bg-bolu-live/10 px-3 py-2 text-sm text-bolu-lunas">{notice}</p>
      ) : null}

      {tab === "drafts" ? (
        <>
          <div className="flex flex-wrap gap-2">
            <label className="text-sm">
              <span className="mr-1.5 text-bolu-muted">Status</span>
              <select
                value={statusFilter}
                onChange={(event) => setStatusFilter(event.target.value)}
                className="rounded-lg border border-bolu-border bg-bolu-surface px-2 py-1"
              >
                <option value="">Semua</option>
                <option value="pending">Menunggu</option>
                <option value="approved">Disetujui</option>
                <option value="revise">Minta revisi</option>
                <option value="sent">Terkirim</option>
                <option value="canceled">Dibatalkan</option>
              </select>
            </label>

            <label className="text-sm">
              <span className="mr-1.5 text-bolu-muted">Bolu</span>
              <input
                value={agentFilter}
                onChange={(event) => setAgentFilter(event.target.value)}
                placeholder="id Bolu"
                className="w-40 rounded-lg border border-bolu-border bg-bolu-surface px-2 py-1"
              />
            </label>

            <label className="text-sm">
              <span className="mr-1.5 text-bolu-muted">Jenis aksi</span>
              <select
                value={kindFilter}
                onChange={(event) => setKindFilter(event.target.value)}
                className="rounded-lg border border-bolu-border bg-bolu-surface px-2 py-1"
              >
                <option value="">Semua</option>
                {kinds.map((kind) => (
                  <option key={kind} value={kind}>
                    {kind}
                  </option>
                ))}
              </select>
            </label>
          </div>

          {drafts.length === 0 ? (
            <p className="rounded-2xl border border-dashed border-bolu-border px-4 py-6 text-center text-sm text-bolu-muted">
              Tidak ada draf pada filter ini.
            </p>
          ) : (
            <ul className="space-y-2">
              {drafts.map((draft) => (
                <li
                  key={draft.draft_id}
                  className="rounded-2xl border border-bolu-border bg-bolu-surface px-4 py-3"
                >
                  <div className="flex flex-wrap items-start justify-between gap-3">
                    <div className="min-w-0">
                      <p className="text-xs uppercase tracking-wide text-bolu-muted">
                        {draft.action_kind}
                      </p>
                      <p className="truncate font-semibold">{draft.title}</p>
                      {draft.summary ? (
                        <p className="text-sm text-bolu-body">{draft.summary}</p>
                      ) : null}
                    </div>

                    <span
                      className={`shrink-0 rounded-full px-2.5 py-1 text-xs font-medium ${
                        STATUS_CLASS[draft.status] ?? STATUS_CLASS.pending
                      }`}
                    >
                      {STATUS_LABEL[draft.status] ?? draft.status}
                    </span>
                  </div>

                  {/* A held action says why, which is the difference between
                      "broken" and "waiting for tomorrow morning". */}
                  {draft.blocked_reason ? (
                    <p className="mt-2 rounded-xl bg-bolu-panel px-3 py-2 text-sm text-bolu-body">
                      {draft.blocked_reason}
                      {draft.not_before
                        ? ` · ${new Date(draft.not_before).toLocaleString("id-ID")}`
                        : null}
                    </p>
                  ) : null}

                  {draft.revision > 0 ? (
                    <button
                      type="button"
                      onClick={() => void openTrail(draft)}
                      className="mt-2 text-xs text-bolu-accent hover:underline"
                    >
                      Sudah direvisi {draft.revision}× · lihat riwayat
                    </button>
                  ) : null}

                  {draft.status === "pending" ? (
                    <div className="mt-3 flex flex-wrap gap-2">
                      <button
                        type="button"
                        disabled={busy}
                        onClick={() => void decide(draft, "approve")}
                        className="rounded-full bg-bolu-accent px-3.5 py-1.5 text-sm font-medium text-white disabled:opacity-50"
                      >
                        Setujui
                      </button>
                      <button
                        type="button"
                        disabled={busy}
                        onClick={() => {
                          const note = window.prompt("Apa yang perlu diubah?");
                          if (note && note.trim() !== "") void decide(draft, "revise", note.trim());
                        }}
                        className="rounded-full border border-bolu-border px-3.5 py-1.5 text-sm font-medium disabled:opacity-50"
                      >
                        Minta revisi
                      </button>
                      <button
                        type="button"
                        disabled={busy}
                        onClick={() => void decide(draft, "cancel")}
                        className="rounded-full px-3 py-1.5 text-sm text-bolu-muted disabled:opacity-50"
                      >
                        Batalkan
                      </button>
                    </div>
                  ) : null}
                </li>
              ))}
            </ul>
          )}
        </>
      ) : null}

      {tab === "policies" ? (
        <ul className="space-y-2">
          {policies.map((policy) => (
            <li
              key={policy.action_kind}
              className="rounded-2xl border border-bolu-border bg-bolu-surface px-4 py-3"
            >
              <div className="flex flex-wrap items-center justify-between gap-3">
                <div className="min-w-0">
                  <p className="font-semibold">{policy.action_kind}</p>
                  <p className="text-sm text-bolu-muted">
                    {policy.mode === "auto"
                      ? policy.max_amount === null
                        ? "Otomatis, tanpa batas nilai"
                        : `Otomatis sampai Rp${policy.max_amount.toLocaleString("id-ID")}`
                      : "Selalu minta persetujuan"}
                  </p>
                </div>

                <div className="flex flex-wrap items-center gap-2">
                  <select
                    value={policy.mode}
                    disabled={busy}
                    onChange={(event) =>
                      void savePolicy(
                        policy,
                        event.target.value,
                        policy.max_amount ?? null,
                      )
                    }
                    className="rounded-lg border border-bolu-border bg-bolu-page px-2 py-1 text-sm"
                  >
                    <option value="always_draft">Selalu minta persetujuan</option>
                    <option value="auto">Otomatis</option>
                  </select>

                  {policy.mode === "auto" ? (
                    <label className="text-sm">
                      <span className="mr-1.5 text-bolu-muted">Batas Rp</span>
                      <input
                        type="number"
                        min={0}
                        defaultValue={policy.max_amount ?? ""}
                        disabled={busy}
                        onBlur={(event) => {
                          const raw = event.target.value.trim();
                          const value = raw === "" ? null : Number(raw);
                          if (value !== policy.max_amount) void savePolicy(policy, "auto", value);
                        }}
                        className="w-32 rounded-lg border border-bolu-border bg-bolu-page px-2 py-1"
                      />
                    </label>
                  ) : null}
                </div>
              </div>
            </li>
          ))}
        </ul>
      ) : null}

      {tab === "audit" ? (
        <ul className="space-y-1.5">
          {audit.map((entry) => (
            <li
              key={entry.id}
              className="flex flex-wrap items-baseline justify-between gap-2 rounded-xl border border-bolu-line px-3 py-2 text-sm"
            >
              <span className="font-medium">
                {AUDIT_LABEL[entry.action] ?? entry.action}
                <span className="ml-2 text-bolu-muted">{entry.object_type}</span>
              </span>
              <span className="text-xs text-bolu-muted">
                {entry.actor_user_id ? "oleh pengguna" : entry.actor_agent_id ? "oleh Bolu" : ""}
                {" · "}
                {new Date(entry.created_at).toLocaleString("id-ID")}
              </span>
            </li>
          ))}
        </ul>
      ) : null}

      {trail ? (
        <div className="fixed inset-0 z-40 flex items-center justify-center bg-black/40 p-4">
          <div className="w-full max-w-lg rounded-2xl bg-bolu-surface p-4">
            <div className="flex items-start justify-between gap-3">
              <div>
                <h3 className="font-semibold">Riwayat revisi</h3>
                <p className="text-sm text-bolu-muted">{trail.draft.title}</p>
              </div>
              <button
                type="button"
                onClick={() => setTrail(null)}
                className="text-sm text-bolu-muted"
              >
                Tutup
              </button>
            </div>

            {trail.revisions.length === 0 ? (
              <p className="mt-3 text-sm text-bolu-muted">Belum ada revisi.</p>
            ) : (
              <ol className="mt-3 space-y-2">
                {trail.revisions.map((revision) => (
                  <li key={revision.revision} className="rounded-xl bg-bolu-panel px-3 py-2 text-sm">
                    <p className="font-medium">Revisi {revision.revision}</p>
                    <p className="text-bolu-body">{revision.note}</p>
                    <p className="mt-1 text-xs text-bolu-muted">
                      {new Date(revision.created_at).toLocaleString("id-ID")}
                    </p>
                  </li>
                ))}
              </ol>
            )}
          </div>
        </div>
      ) : null}
    </section>
  );
}
