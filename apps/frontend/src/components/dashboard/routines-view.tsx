"use client";

/**
 * Rutinitas: the work the team runs on its own.
 *
 * The design's screen lists three cadences and a form that adds one. What the
 * backend has today is the `routines` table and the task history those routines
 * produce — the scheduler that fires them, and the endpoints that read and write
 * a routine, are EPIC 11 and are not built. So this screen shows the runs that
 * actually happened, grouped the way the design groups them, and says plainly
 * that a schedule cannot be edited yet rather than showing a form that would
 * pretend to save one.
 */

import Link from "next/link";
import { useMemo } from "react";

import { Bolu } from "@/components/bolu/bolu";
import { allBot } from "@/lib/crew";
import { clockTime, crewFor, expressionOf, relativeAge, stateOf } from "@/lib/dashboard-view";

import { useWorkspace } from "./workspace-data";

const CARD = "rounded-[26px] border border-bolu-border bg-white";
const HEADING = "font-display text-[22px] font-semibold";

/** The three cadences the design groups routines into, plus what each means. */
const CADENCES = [
  {
    id: "hourly",
    title: "Sepanjang hari",
    note: "berjalan berulang tiap jam",
    test: (title: string) => /tiap jam|per jam|hourly|tiap \d+ menit/i.test(title),
  },
  {
    id: "daily",
    title: "Setiap hari",
    note: "diurutkan menurut jam",
    test: (title: string) => /tiap hari|setiap hari|harian|daily/i.test(title),
  },
  {
    id: "weekly",
    title: "Setiap minggu",
    note: "sekali seminggu",
    test: (title: string) => /tiap minggu|setiap minggu|mingguan|weekly/i.test(title),
  },
];

/** RoutinesView is the routines screen. */
export function RoutinesView() {
  const { tasks, agents, drafts } = useWorkspace();

  const now = useMemo(() => new Date(), []);
  const scheduled = useMemo(() => tasks.filter((task) => task.trigger === "routine"), [tasks]);

  const grouped = CADENCES.map((cadence) => ({
    ...cadence,
    runs: scheduled.filter((task) => cadence.test(task.title)),
  }));

  const unsorted = scheduled.filter(
    (task) => !CADENCES.some((cadence) => cadence.test(task.title)),
  );

  const running = scheduled.filter((task) => task.health === "running" || task.health === "waiting");
  const waiting = drafts.filter((draft) => draft.status === "pending").length;

  return (
    <div className="flex flex-col gap-[22px] p-[24px_28px_48px] max-[760px]:p-[18px_14px_36px]">
      <div>
        <h1 className="font-display text-[36px] font-bold leading-[1.1] max-[760px]:text-[28px]">
          Rutinitas
        </h1>
        <div className="text-bolu-muted">
          Pekerjaan yang dijalankan tim secara otomatis, tanpa perlu kamu suruh.{" "}
          {scheduled.length === 0
            ? "Belum ada yang dijalankan otomatis."
            : `${running.length} sedang jalan, ${scheduled.length} sudah pernah jalan.`}
        </div>
      </div>

      {/* The scheduler is the missing piece, and it is said here rather than
          implied by an empty list: a screen that shows nothing either way would
          read as "no routines configured" instead of "not available yet". */}
      <div className={`${CARD} flex flex-wrap items-center justify-between gap-3.5 p-[18px_20px]`}>
        <div className="min-w-0">
          <div className="font-display text-[17px] font-semibold">Penjadwalan belum tersedia</div>
          <p className="mt-1 max-w-[620px] text-[14px] text-bolu-muted">
            Menambah dan mengubah jadwal rutinitas belum bisa dari layar ini. Yang tampil di bawah
            adalah pekerjaan otomatis yang benar-benar sudah dijalankan workspace ini, dibaca dari
            riwayat tugas — bukan contoh.
          </p>
        </div>
        <Link
          href="/chat"
          className="flex min-h-11 items-center rounded-full bg-bolu-ink px-5 text-[14px] font-bold text-white no-underline hover:text-white"
        >
          Suruh lewat obrolan
        </Link>
      </div>

      {grouped.map((cadence) => (
        <section key={cadence.id} className={`${CARD} flex flex-col gap-3 p-5`}>
          <div className="flex flex-wrap items-center justify-between gap-2.5">
            <h2 className={HEADING}>{cadence.title}</h2>
            <span className="text-[14px] text-bolu-muted">
              {cadence.note} · {cadence.runs.length} jalan
            </span>
          </div>
          {cadence.runs.length === 0 ? (
            <p className="text-[14px] text-bolu-muted">Belum ada yang berjalan di kelompok ini.</p>
          ) : (
            <ul className="flex flex-col">
              {cadence.runs.slice(0, 6).map((run) => {
                const agent = agents.find((item) => item.id === run.agent_id) ?? null;
                return (
                  <li
                    key={run.task_id}
                    className="flex flex-wrap items-center gap-3 border-t border-bolu-line p-[10px_0]"
                  >
                    <span className="w-[52px] flex-none text-[14px] font-semibold text-bolu-muted">
                      {run.started_at ? clockTime(run.started_at) : "—"}
                    </span>
                    <div className="flex-none">
                      <Bolu
                        member={agent ? crewFor(agent) : allBot("Oren")}
                        expression={expressionOf(agent ? stateOf(agent) : "idle")}
                        color={agent?.color || undefined}
                        size={32}
                      />
                    </div>
                    <div className="min-w-0 flex-1">
                      <div className="truncate font-semibold">{run.title || "Tugas otomatis"}</div>
                      <div className="text-[13px] text-bolu-muted">
                        {agent?.name ?? "Bolu"} · {run.step_count} langkah ·{" "}
                        {relativeAge(run.created_at, now)}
                      </div>
                    </div>
                    <span
                      className="flex-none rounded-full px-2.5 py-1 text-[12px] font-bold"
                      style={{
                        backgroundColor: run.status === "done" ? "#1E7A43" + "1F" : "#E2602B1F",
                        color: run.status === "done" ? "#1E7A43" : "#E2602B",
                      }}
                    >
                      {run.status === "done" ? "Selesai" : "Berjalan"}
                    </span>
                  </li>
                );
              })}
            </ul>
          )}
        </section>
      ))}

      {unsorted.length > 0 ? (
        <section className={`${CARD} flex flex-col gap-3 p-5`}>
          <div className="flex flex-wrap items-center justify-between gap-2.5">
            <h2 className={HEADING}>Jadwal lain</h2>
            <span className="text-[14px] text-bolu-muted">
              {unsorted.length} jalan · jadwalnya tidak disebut di judulnya
            </span>
          </div>
          <ul className="flex flex-col">
            {unsorted.slice(0, 6).map((run) => (
              <li
                key={run.task_id}
                className="flex items-center justify-between gap-3 border-t border-bolu-line py-2.5 text-[15px]"
              >
                <span className="min-w-0 truncate font-semibold">{run.title || "Tugas otomatis"}</span>
                <span className="flex-none text-[13px] text-bolu-muted">
                  {relativeAge(run.created_at, now)}
                </span>
              </li>
            ))}
          </ul>
        </section>
      ) : null}

      <div className="grid gap-3.5 sm:grid-cols-2">
        <div className={`${CARD} flex flex-col gap-1 p-[18px_20px]`}>
          <div className="text-[15px] font-medium text-bolu-muted">Menunggu keputusanmu</div>
          <div className="font-display text-[34px] font-bold leading-[1.1]">{waiting}</div>
          <div className="text-[14px] text-bolu-muted">
            {waiting === 0 ? "tidak ada draf" : "draf dari pekerjaan otomatis dan obrolan"}
          </div>
        </div>
        <div className={`${CARD} flex flex-col gap-1 p-[18px_20px]`}>
          <div className="text-[15px] font-medium text-bolu-muted">Bolu yang bisa disuruh</div>
          <div className="font-display text-[34px] font-bold leading-[1.1]">
            {agents.filter((agent) => agent.status !== "resting").length}
          </div>
          <div className="text-[14px] text-bolu-muted">dari {agents.length} anggota tim</div>
        </div>
      </div>
    </div>
  );
}
