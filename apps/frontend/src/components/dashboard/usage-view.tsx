"use client";

/**
 * Pemakaian: what the workspace spent, and what is waiting on a person.
 *
 * Every number on this screen is a row the backend counted, read three ways over
 * one window: the period's totals, the same window per Bolu, and the same window
 * per task. The three are shown together rather than in tabs, because the
 * question a person arrives with — "why is this month expensive?" — is answered
 * by the difference between them: Oren spent more than the tasks Oren ran added
 * up to, and the remainder is the memory extraction and the embeddings, which are
 * real product work that belongs to no task.
 *
 * Two things are deliberate:
 *
 * - **the window is the server's**, and the moment it started is shown rather
 *   than recomputed here. A screen that derived "the last 30 days" from its own
 *   clock would disagree with the totals across a midnight, and the disagreement
 *   would be invisible;
 * - **the export is the same read as the screen**. It is fetched from the same
 *   endpoint with `format=csv`, so a spreadsheet and this page cannot tell two
 *   different stories about one month.
 *
 * The attention list is beside it rather than on its own page because the two are
 * read together: a workspace whose tasks are failing is a workspace whose spend is
 * about to change, and the reason is on the same screen.
 */

import Link from "next/link";
import { useCallback, useEffect, useMemo, useState } from "react";

import { Alert } from "@/components/auth/auth-shell";
import {
  type AttentionTask,
  type UsageReport,
  ensureActiveWorkspace,
  listAttentionTasks,
  usageReport,
  usageReportCSV,
} from "@/lib/api";

import { useWorkspace } from "./workspace-data";

const CARD = "rounded-[26px] border border-bolu-border bg-white";
const HEADING = "font-display text-[22px] font-semibold";
const BUTTON =
  "min-h-10 rounded-xl border border-bolu-border bg-white px-3.5 text-[14px] font-medium text-bolu-ink transition hover:border-bolu-accent disabled:opacity-50";

/** The windows the screen offers, in days. */
const WINDOWS = [7, 30, 90];

/** ATTENTION is what each reason on the list means, in the user's words. */
const ATTENTION: Record<AttentionTask["attention"], { label: string; tone: string; note: string }> = {
  failed: {
    label: "Gagal",
    tone: "border-bolu-belum/30 bg-bolu-belum/8 text-bolu-belum",
    note: "tugas berhenti tanpa hasil",
  },
  stuck: {
    label: "Macet",
    tone: "border-bolu-belum/30 bg-bolu-belum/8 text-bolu-belum",
    note: "detaknya berhenti, ada yang perlu dilihat",
  },
  waiting: {
    label: "Menunggu keputusanmu",
    tone: "border-bolu-accent/40 bg-bolu-accent/5 text-bolu-accent",
    note: "draf sudah siap dan menunggu disetujui",
  },
};

/** formatRupiah renders rupiah the way a reader expects it. */
function formatRupiah(amount: number): string {
  return new Intl.NumberFormat("id-ID", {
    style: "currency",
    currency: "IDR",
    maximumFractionDigits: amount < 1000 ? 2 : 0,
  }).format(amount);
}

/** formatTokens renders a token count with thousands separators. */
function formatTokens(tokens: number): string {
  return new Intl.NumberFormat("id-ID").format(tokens);
}

/** formatMoment renders an instant in the reader's own locale and zone. */
function formatMoment(value: string): string {
  const moment = new Date(value);
  if (Number.isNaN(moment.getTime())) {
    return value;
  }

  return new Intl.DateTimeFormat("id-ID", { dateStyle: "medium", timeStyle: "short" }).format(moment);
}

/** UsageView is the usage and cost screen. */
export function UsageView() {
  const { workspaceID, agents, reloadTasks } = useWorkspace();

  const [report, setReport] = useState<UsageReport | null>(null);
  const [attention, setAttention] = useState<AttentionTask[]>([]);
  const [days, setDays] = useState(30);
  const [status, setStatus] = useState<"loading" | "ready" | "error">("loading");
  const [message, setMessage] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const load = useCallback(
    async (workspace: string, window: number) => {
      setStatus("loading");
      setMessage(null);

      const [reportResult, attentionResult] = await Promise.all([
        usageReport(workspace, window),
        listAttentionTasks(workspace),
      ]);

      if (!reportResult.ok) {
        setStatus("error");
        setMessage(reportResult.message);
        return;
      }

      setReport(reportResult.data);
      // A deployment without the reporting port answers the report and not the list, so a failed
      // list is not a failed screen: what it shows is the spend, and the list is what is missing.
      setAttention(attentionResult.ok ? attentionResult.data : []);
      setStatus("ready");
    },
    [],
  );

  useEffect(() => {
    let cancelled = false;

    void (async () => {
      const workspace = workspaceID ?? (await ensureActiveWorkspace());
      if (cancelled || !workspace) {
        return;
      }

      await load(workspace, days);
    })();

    return () => {
      cancelled = true;
    };
  }, [workspaceID, days, load]);

  /** exportFile downloads the same read as a file. */
  const exportFile = useCallback(async () => {
    const workspace = workspaceID ?? (await ensureActiveWorkspace());
    if (!workspace) {
      return;
    }

    const result = await usageReportCSV(workspace, days);
    setNotice(result.message);
  }, [workspaceID, days]);

  /** crewName is what a Bolu is called, from the store the rest of the screen reads. */
  const crewName = useMemo(() => {
    const byID = new Map(agents.map((agent) => [agent.id, agent.name]));
    return (id: string) => byID.get(id) ?? "";
  }, [agents]);

  const unrouted = useMemo(() => {
    if (!report) {
      return { calls: 0, cost: 0 };
    }

    // What the per-task rows do not account for: the memory extraction, the embeddings, and the
    // routing decisions — real work that belongs to no single task. It is shown rather than
    // hidden, because a person who adds up the rows and gets less than the total deserves to
    // know why.
    //
    // The *count* is what is displayed rather than the money, because a deployment that prices
    // no calls reports zero rupiah for every row and a column of "Rp 0" tells a reader nothing.
    // The calls are the fact either way.
    const perTaskCalls = report.by_task.reduce((sum, row) => sum + row.calls, 0);
    const perTaskCost = report.by_task.reduce((sum, row) => sum + row.cost_micros, 0);

    return {
      calls: Math.max(0, report.totals.calls - perTaskCalls),
      cost: Math.max(0, report.totals.cost_micros - perTaskCost),
    };
  }, [report]);

  return (
    <div className="flex flex-col gap-6 p-[24px_28px_48px] max-[760px]:p-[18px_14px_36px]">
      <header className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className={HEADING}>Pemakaian</h1>
          <p className="mt-1 text-[15px] text-bolu-muted">
            Token dan biaya workspace ini{report ? `, sejak ${formatMoment(report.period_started_at)}` : ""}.
          </p>
        </div>

        <div className="flex flex-wrap items-center gap-2">
          {WINDOWS.map((window) => (
            <button
              key={window}
              type="button"
              onClick={() => setDays(window)}
              aria-pressed={days === window}
              className={`${BUTTON} ${days === window ? "border-bolu-accent text-bolu-accent" : ""}`}
            >
              {window} hari
            </button>
          ))}
          <button type="button" onClick={exportFile} className={BUTTON}>
            Ekspor CSV
          </button>
        </div>
      </header>

      {notice ? <Alert tone="info">{notice}</Alert> : null}
      {status === "error" && message ? <Alert tone="error">{message}</Alert> : null}

      <section className={CARD}>
        <div className="flex flex-wrap items-baseline justify-between gap-2 border-b border-bolu-line px-6 py-4">
          <h2 className="font-display text-[17px] font-semibold">Ringkasan</h2>
          {report ? (
            <span className="text-[14px] text-bolu-muted">
              {formatTokens(report.totals.calls)} panggilan model
            </span>
          ) : null}
        </div>

        {status === "loading" && !report ? (
          <p className="px-6 py-6 text-[14px] text-bolu-muted">Memuat pemakaian…</p>
        ) : report ? (
          <dl className="grid grid-cols-2 gap-x-6 gap-y-4 px-6 py-5 md:grid-cols-4">
            <Stat label="Token masuk" value={formatTokens(report.totals.input_tokens)} />
            <Stat label="Token keluar" value={formatTokens(report.totals.output_tokens)} />
            <Stat label="Total token" value={formatTokens(report.totals.total_tokens)} />
            <Stat label="Biaya" value={formatRupiah(report.totals.cost_idr)} />
            {report.totals.cache_read_tokens > 0 ? (
              <Stat label="Cache dibaca" value={formatTokens(report.totals.cache_read_tokens)} />
            ) : null}
            {report.totals.cache_write_tokens > 0 ? (
              <Stat label="Cache ditulis" value={formatTokens(report.totals.cache_write_tokens)} />
            ) : null}
            {unrouted.calls > 0 ? (
              <Stat
                label="Di luar tugas"
                value={`${formatTokens(unrouted.calls)} panggilan`}
                note={
                  // The money is only named once it is a rupiah: a deployment that prices no
                  // calls reports a few micro-rupiah per period, and "Rp 0" in a caption reads
                  // as a bug rather than as a figure.
                  unrouted.cost >= 1_000_000
                    ? `${formatRupiah(unrouted.cost / 1_000_000)} untuk memori dan embedding, yang tidak dimiliki satu tugas pun`
                    : "memori dan embedding, yang tidak dimiliki satu tugas pun"
                }
              />
            ) : null}
          </dl>
        ) : null}
      </section>

      <div className="grid gap-6 lg:grid-cols-2">
        <section className={CARD}>
          <h2 className="border-b border-bolu-line px-6 py-4 font-display text-[17px] font-semibold">
            Per Bolu
          </h2>

          {report && report.by_agent.length > 0 ? (
            <div className="overflow-x-auto">
              <table className="w-full text-[14px]">
                <thead className="text-left text-bolu-muted">
                  <tr>
                    <th className="px-6 py-2 font-medium">Bolu</th>
                    <th className="px-3 py-2 text-right font-medium">Panggilan</th>
                    <th className="px-3 py-2 text-right font-medium">Token</th>
                    <th className="px-6 py-2 text-right font-medium">Biaya</th>
                  </tr>
                </thead>
                <tbody>
                  {report.by_agent.map((row) => (
                    <tr key={row.agent_id} className="border-t border-bolu-line">
                      <td className="px-6 py-2">
                        {/* A call made on no Bolu's behalf — a memory extraction, an embedding
                            — has a zero id and no name, and saying so is more useful than
                            inventing one. */}
                        {crewName(row.agent_id) || row.agent_name || "Tanpa Bolu"}
                      </td>
                      <td className="px-3 py-2 text-right">{formatTokens(row.calls)}</td>
                      <td className="px-3 py-2 text-right">{formatTokens(row.total_tokens)}</td>
                      <td className="px-6 py-2 text-right">{formatRupiah(row.cost_idr)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <p className="px-6 py-6 text-[14px] text-bolu-muted">Belum ada panggilan model di periode ini.</p>
          )}
        </section>

        <section className={CARD}>
          <h2 className="border-b border-bolu-line px-6 py-4 font-display text-[17px] font-semibold">
            Per tugas
          </h2>

          {report && report.by_task.length > 0 ? (
            <div className="overflow-x-auto">
              <table className="w-full text-[14px]">
                <thead className="text-left text-bolu-muted">
                  <tr>
                    <th className="px-6 py-2 font-medium">Tugas</th>
                    <th className="px-3 py-2 text-right font-medium">Token</th>
                    <th className="px-6 py-2 text-right font-medium">Biaya</th>
                  </tr>
                </thead>
                <tbody>
                  {report.by_task.map((row) => (
                    <tr key={row.task_id} className="border-t border-bolu-line">
                      <td className="max-w-[240px] truncate px-6 py-2" title={row.task_title}>
                        {row.task_title || row.task_id}
                        {row.task_status ? (
                          <span className="ml-2 text-bolu-muted">{row.task_status}</span>
                        ) : null}
                      </td>
                      <td className="px-3 py-2 text-right">{formatTokens(row.total_tokens)}</td>
                      <td className="px-6 py-2 text-right">{formatRupiah(row.cost_idr)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <p className="px-6 py-6 text-[14px] text-bolu-muted">
              Belum ada tugas yang mencatat pemakaian di periode ini.
            </p>
          )}
        </section>
      </div>

      <section className={CARD}>
        <div className="flex flex-wrap items-baseline justify-between gap-2 border-b border-bolu-line px-6 py-4">
          <h2 className="font-display text-[17px] font-semibold">Perlu perhatian</h2>
          <button type="button" onClick={() => void reloadTasks()} className={BUTTON}>
            Muat ulang
          </button>
        </div>

        {attention.length === 0 ? (
          <p className="px-6 py-6 text-[14px] text-bolu-muted">
            Tidak ada tugas yang menunggu, macet, atau gagal.
          </p>
        ) : (
          <ul className="divide-y divide-bolu-line">
            {attention.map((task) => {
              const reason = ATTENTION[task.attention];
              return (
                <li key={task.task_id} className="flex flex-wrap items-center gap-3 px-6 py-3">
                  <span
                    title={reason.note}
                    className={`rounded-full border px-2.5 py-0.5 text-[12px] font-semibold ${reason.tone}`}
                  >
                    {reason.label}
                  </span>

                  <span className="min-w-0 flex-1 truncate text-[14px]" title={task.title}>
                    {task.title || task.task_id}
                  </span>

                  {task.stopped_reason || task.waiting_reason ? (
                    <span className="min-w-0 max-w-[320px] truncate text-[13px] text-bolu-muted">
                      {task.stopped_reason || task.waiting_reason}
                    </span>
                  ) : null}

                  {/* The Temporal link is only offered when the deployment named a UI and the
                      task reached the engine; the server decides both. */}
                  {task.temporal_url ? (
                    <a
                      href={task.temporal_url}
                      target="_blank"
                      rel="noreferrer"
                      className="text-[13px] font-medium text-bolu-accent underline-offset-2 hover:underline"
                    >
                      Buka di Temporal
                    </a>
                  ) : null}

                  {task.waiting_draft_id ? (
                    <Link
                      href="/dashboard"
                      className="text-[13px] font-medium text-bolu-accent underline-offset-2 hover:underline"
                    >
                      Lihat draf
                    </Link>
                  ) : null}
                </li>
              );
            })}
          </ul>
        )}
      </section>
    </div>
  );
}

/** Stat is one labelled figure on the summary row. */
function Stat({ label, value, note }: { label: string; value: string; note?: string }) {
  return (
    <div className="flex flex-col gap-0.5">
      <dt className="text-[13px] text-bolu-muted">{label}</dt>
      <dd className="font-display text-[20px] font-semibold">{value}</dd>
      {note ? <p className="text-[12px] text-bolu-muted">{note}</p> : null}
    </div>
  );
}
