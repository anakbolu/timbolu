"use client";

/**
 * One draft, opened from a notification, a dashboard card, or a task.
 *
 * A draft is the one kind of work that reaches outside the workspace, so the
 * screen answers the questions a person needs before deciding: what will leave,
 * who prepared it, and what was already asked of it. The decision itself is
 * written through the same endpoint the chat card and the dashboard use, which
 * is what keeps the three places from disagreeing about one draft.
 *
 * A draft that is already decided stays readable: the answer is a record, not a
 * dead link, and the controls are gone rather than disabled-looking invitations
 * to try again.
 */

import Link from "next/link";
import { useCallback, useEffect, useState } from "react";

import { Alert } from "@/components/auth/auth-shell";
import { Bolu } from "@/components/bolu/bolu";
import {
  type Draft,
  type DraftRevision,
  decideDraft,
  ensureActiveWorkspace,
  getDraft,
  listDraftRevisions,
} from "@/lib/api";
import { crewFor, expressionOf, formatMoment, stateOf } from "@/lib/dashboard-view";
import { NO_WORKSPACE_MESSAGE } from "@/lib/workspace-messages";

import { useWorkspace } from "./workspace-data";

const CARD = "rounded-md border border-bolu-border bg-bolu-surface";
const HEADING = "font-display text-[22px] font-semibold";
const BUTTON =
  "min-h-10 cursor-pointer rounded-md border border-bolu-border bg-bolu-surface px-3.5 text-[14px] font-medium text-bolu-ink transition hover:border-bolu-accent disabled:cursor-default disabled:opacity-50";
const PRIMARY =
  "min-h-10 cursor-pointer rounded-full border-0 bg-bolu-solid px-4 text-[14px] font-bold text-bolu-solid-ink disabled:cursor-default disabled:opacity-50";

const DRAFT_STATUS: Record<Draft["status"], { label: string; className: string }> = {
  pending: { label: "Menunggu kamu", className: "bg-bolu-flag/12 text-bolu-flag" },
  approved: { label: "Disetujui", className: "bg-bolu-live/12 text-bolu-lunas" },
  revise: { label: "Minta revisi", className: "bg-bolu-panel text-bolu-body" },
  sent: { label: "Terkirim", className: "bg-bolu-live/12 text-bolu-lunas" },
  canceled: { label: "Dibatalkan", className: "bg-bolu-panel text-bolu-rest" },
};

type Decision = "approve" | "revise" | "cancel";

const DECISION_NOTICE: Record<Decision, string> = {
  approve: "Draf disetujui.",
  revise: "Draf dikirim kembali ke Bolu untuk direvisi.",
  cancel: "Draf dibatalkan.",
};

/** ApprovalView is one draft's page. */
export function ApprovalView({ draftID }: { draftID: string }) {
  const { workspaceID, agents, members, reloadDrafts, reloadTasks } = useWorkspace();

  const [draft, setDraft] = useState<Draft | null>(null);
  const [revisions, setRevisions] = useState<DraftRevision[]>([]);
  const [load, setLoad] = useState<"loading" | "ready" | "missing" | "error">("loading");
  const [message, setMessage] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState<Decision | null>(null);
  const [attempt, setAttempt] = useState(0);

  const read = useCallback(
    async (workspace: string, mode: "initial" | "refresh") => {
      if (mode === "initial") {
        setLoad("loading");
        setMessage(null);
      }

      const [draftResult, revisionsResult] = await Promise.all([
        getDraft(workspace, draftID),
        listDraftRevisions(workspace, draftID),
      ]);

      if (!draftResult.ok) {
        // A 404 is the API's answer for both "never existed" and "belongs to
        // another workspace", so it is a missing draft rather than a retryable
        // network failure. A refresh that discovers it is treated the same way:
        // the page must not keep showing a draft that is no longer there.
        if (draftResult.code === "draft_not_found" || draftResult.status === 404) {
          setLoad("missing");
          return;
        }
        setLoad("error");
        setMessage(draftResult.message);
        return;
      }

      setDraft(draftResult.data);
      // An empty trail and a failed read of it both render as no section: what a
      // person decides on is the draft itself, and a failed read of history must
      // not take the decision away from them.
      setRevisions(revisionsResult.ok ? revisionsResult.data : []);
      setLoad("ready");
    },
    [draftID],
  );

  useEffect(() => {
    let cancelled = false;

    void (async () => {
      const workspace = workspaceID ?? (await ensureActiveWorkspace());
      if (cancelled) return;
      if (!workspace) {
        setLoad("error");
        setMessage(NO_WORKSPACE_MESSAGE);
        return;
      }

      await read(workspace, "initial");
    })();

    return () => {
      cancelled = true;
    };
  }, [workspaceID, read, attempt]);

  const decide = useCallback(
    async (decision: Decision) => {
      const workspace = workspaceID ?? (await ensureActiveWorkspace());
      if (!workspace || !draft) {
        setMessage(NO_WORKSPACE_MESSAGE);
        return;
      }

      const trimmed = note.trim();
      // The API refuses a revision without a note, and it is right to: an agent
      // sent back with nothing to change produces the same draft again. Saying
      // so here keeps the refusal from arriving as an error after a click.
      if (decision === "revise" && trimmed === "") {
        setMessage("Tulis dulu apa yang perlu diubah, supaya Bolu tidak mengulang draf yang sama.");
        return;
      }

      setBusy(decision);
      setMessage(null);
      setNotice(null);

      const result = await decideDraft(workspace, draft.draft_id, decision, trimmed);
      setBusy(null);

      if (!result.ok) {
        // A second decision is refused because the first one stands. The draft is
        // read back so the screen shows that decision instead of leaving the
        // buttons in place as if nothing had happened.
        if (result.code === "draft_already_decided") {
          await read(workspace, "refresh");
          await reloadDrafts();
          setNotice("Draf ini sudah diputuskan. Yang tampil sekarang adalah keputusan yang berlaku.");
          return;
        }
        setMessage(result.message);
        return;
      }

      setNote("");
      await read(workspace, "refresh");
      await Promise.all([reloadDrafts(), reloadTasks()]);
      setNotice(DECISION_NOTICE[decision]);
    },
    [workspaceID, draft, note, read, reloadDrafts, reloadTasks],
  );

  if (load === "loading") {
    return (
      <div className="p-[24px_28px_48px] max-[760px]:p-[18px_14px_36px]">
        <p className="text-[15px] text-bolu-muted">Memuat draf…</p>
      </div>
    );
  }

  if (load === "missing") {
    return (
      <div className="flex flex-col gap-4 p-[24px_28px_48px] max-[760px]:p-[18px_14px_36px]">
        <Alert tone="error">
          Draf tidak ditemukan. Mungkin sudah dihapus, atau tautannya berasal dari ruang kerja lain.
        </Alert>
        <Link href="/dashboard" className={`${BUTTON} w-fit no-underline`}>
          ← Kembali ke Dasbor
        </Link>
      </div>
    );
  }

  if (load === "error" || !draft) {
    return (
      <div className="flex flex-col gap-4 p-[24px_28px_48px] max-[760px]:p-[18px_14px_36px]">
        <Alert tone="error">{message ?? "Draf gagal dimuat."}</Alert>
        <button type="button" className={`${BUTTON} w-fit`} onClick={() => setAttempt((count) => count + 1)}>
          Coba lagi
        </button>
      </div>
    );
  }

  const status = DRAFT_STATUS[draft.status] ?? DRAFT_STATUS.pending;
  const agent = agents.find((item) => item.id === draft.agent_id) ?? null;
  const crewName = agent?.name ?? "Bolu";
  const kind = draft.action_kind.replace(/[._]/g, " ").trim() || "Draf";
  const open = draft.status === "pending";
  const entries = Object.entries(draft.payload ?? {});
  const decidedBy = draft.decided_by ? members.find((member) => member.user_id === draft.decided_by) : null;

  return (
    <div className="flex flex-col gap-6 p-[24px_28px_48px] max-[760px]:p-[18px_14px_36px]">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-3 text-[14px] text-bolu-muted">
            <Link href="/approval" className="no-underline hover:underline">
              ← Semua draf
            </Link>
            <Link href="/dashboard" className="no-underline hover:underline">
              Dasbor
            </Link>
          </div>
          <h1 className={`${HEADING} mt-1`}>{draft.title || "Draf menunggu persetujuan"}</h1>
          <div className="mt-1.5 flex flex-wrap items-center gap-2 text-[15px] text-bolu-muted">
            <span className="size-6 flex-none">
              {agent ? (
                <Bolu
                  member={crewFor(agent)}
                  expression={expressionOf(stateOf(agent))}
                  color={agent.color || undefined}
                  size={24}
                />
              ) : null}
            </span>
            <span>{crewName}</span>
            <span aria-hidden="true">·</span>
            <span>{kind}</span>
            <span aria-hidden="true">·</span>
            <span>{formatMoment(draft.created_at)}</span>
          </div>
        </div>

        <span className={`flex-none rounded-full px-3 py-1 text-[13px] font-semibold ${status.className}`}>
          {status.label}
        </span>
      </div>

      {notice ? <Alert tone="info">{notice}</Alert> : null}
      {message ? <Alert tone="error">{message}</Alert> : null}

      <section className={`${CARD} flex flex-col gap-4 p-5`}>
        <div>
          <h2 className={HEADING}>Yang diminta</h2>
          <p className="mt-1 text-[15px] text-bolu-muted">
            {draft.summary || "Draf ini tidak membawa ringkasan."}
          </p>
        </div>

        {entries.length === 0 ? (
          <p className="text-[15px] text-bolu-muted">Draf ini tidak membawa isi tambahan.</p>
        ) : (
          <dl className="flex flex-col gap-3">
            {entries.map(([key, value], index) => {
              const block = typeof value === "object" && value !== null;
              const label = key.replace(/[._]/g, " ").trim();
              return (
                <div
                  key={`${key}-${index}`}
                  className="flex flex-col gap-1 border-t border-bolu-line pt-3 first:border-t-0 first:pt-0"
                >
                  <dt className="text-[13px] font-semibold text-bolu-muted">
                    {label.charAt(0).toUpperCase() + label.slice(1)}
                  </dt>
                  <dd className="min-w-0 text-[15px]">
                    {block ? (
                      <pre className="overflow-x-auto whitespace-pre-wrap break-words rounded-md bg-bolu-panel/60 p-3 font-mono text-[13px]">
                        {JSON.stringify(value, null, 2)}
                      </pre>
                    ) : (
                      <span className="whitespace-pre-wrap break-words">{valueText(key, value)}</span>
                    )}
                  </dd>
                </div>
              );
            })}
          </dl>
        )}
      </section>

      <section className={`${CARD} flex flex-col gap-4 p-5`}>
        <h2 className={HEADING}>Keputusan</h2>

        {open ? (
          <>
            <label className="flex flex-col gap-1.5">
              <span className="text-[14px] font-medium">Catatan</span>
              <textarea
                value={note}
                onChange={(event) => setNote(event.target.value)}
                rows={3}
                placeholder="Wajib untuk minta revisi; opsional untuk setujui atau batalkan."
                className="w-full rounded-md border border-bolu-border bg-bolu-page px-3 py-2 text-[15px]"
              />
            </label>

            <div className="flex flex-wrap gap-2">
              <button
                type="button"
                className={PRIMARY}
                onClick={() => void decide("approve")}
                disabled={busy !== null}
              >
                {busy === "approve" ? "Menyetujui…" : "Setujui"}
              </button>
              <button
                type="button"
                className={BUTTON}
                onClick={() => void decide("revise")}
                disabled={busy !== null || note.trim() === ""}
              >
                {busy === "revise" ? "Mengirim…" : "Minta revisi"}
              </button>
              <button
                type="button"
                className={BUTTON}
                onClick={() => void decide("cancel")}
                disabled={busy !== null}
              >
                {busy === "cancel" ? "Membatalkan…" : "Batalkan"}
              </button>
            </div>
          </>
        ) : (
          <div className="flex flex-col gap-2">
            <p className="text-[15px]">
              Status sekarang: <strong>{status.label}</strong>
              {draft.decided_at ? ` · ${formatMoment(draft.decided_at)}` : ""}
              {decidedBy ? ` oleh ${decidedBy.email}` : ""}.
            </p>
            {draft.decision_note ? (
              <p className="rounded-md bg-bolu-panel/60 p-3 text-[15px] whitespace-pre-wrap break-words">
                {draft.decision_note}
              </p>
            ) : null}
            {draft.blocked_reason ? (
              <Alert tone="info">
                Ditahan: {draft.blocked_reason}
                {draft.not_before ? ` · dikirim setelah ${formatMoment(draft.not_before)}` : null}
              </Alert>
            ) : null}
            {draft.sent_at ? (
              <p className="text-[14px] text-bolu-muted">Terkirim {formatMoment(draft.sent_at)}.</p>
            ) : null}
          </div>
        )}
      </section>

      {revisions.length > 0 ? (
        <section className={`${CARD} flex flex-col gap-3 p-5`}>
          <h2 className={HEADING}>Jejak revisi</h2>
          <ol className="flex flex-col">
            {revisions.map((revision) => {
              const by = revision.decided_by
                ? members.find((member) => member.user_id === revision.decided_by)
                : null;
              return (
                <li
                  key={revision.revision}
                  className="flex flex-col gap-1 border-t border-bolu-line py-3 first:border-t-0 first:pt-0"
                >
                  <div className="flex flex-wrap items-baseline gap-2">
                    <span className="font-semibold">Revisi {revision.revision}</span>
                    <span className="text-[13px] text-bolu-muted">
                      {formatMoment(revision.created_at)}
                      {by ? ` · ${by.email}` : ""}
                    </span>
                  </div>
                  <p className="text-[15px] whitespace-pre-wrap break-words">
                    {revision.note || "Tanpa catatan."}
                  </p>
                </li>
              );
            })}
          </ol>
        </section>
      ) : null}
    </div>
  );
}

/**
 * valueText renders one payload value the way the card in a thread would: plain
 * text for the values a person reads, and the amount with rupiah when the key
 * says it is money.
 */
function valueText(key: string, value: unknown): string {
  if (value === null || value === undefined) return "—";
  if (typeof value === "string") return value || "—";
  if (typeof value === "boolean") return value ? "ya" : "tidak";
  if (typeof value === "number") {
    const formatted = new Intl.NumberFormat("id-ID").format(value);
    return key === "amount" || key.endsWith("_amount") ? `Rp ${formatted}` : formatted;
  }
  return String(value);
}
