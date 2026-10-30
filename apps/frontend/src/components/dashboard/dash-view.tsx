"use client";

/**
 * Dasbor: the front door.
 *
 * Every number on this screen is a count of something the workspace actually
 * has — the drafts waiting, the tasks that finished today, the tasks running,
 * and the rounds the Bolu executed this week. The design's "time saved" and
 * "unpaid invoices" cards have no endpoint behind them, so the card shapes stay
 * and the values come from what the API can answer.
 */

import { Bell, Check } from "lucide-react";
import Link from "next/link";
import { useMemo, useState } from "react";

import { Bolu } from "@/components/bolu/bolu";
import { allBot } from "@/lib/crew";
import {
  buildFeed,
  buildPending,
  buildStats,
  buildWeek,
  crewFor,
  expressionOf,
  greeting,
  longDate,
  relativeAge,
  stateOf,
  taskState,
} from "@/lib/dashboard-view";

import { useWorkspace } from "./workspace-data";

const CARD = "rounded-md border border-bolu-border bg-bolu-surface";
const HEADING = "font-display text-[22px] font-semibold";

/** Dasbor is the overview screen. */
export function DashView() {
  const { user, drafts, tasks, agents, events, status, ready, workspaceID, decide, sendTask } =
    useWorkspace();
  const [instruction, setInstruction] = useState("");
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  // The composer is the one action on this screen that can be reached before the
  // store has its first answer, and the id it needs comes from that answer. Until
  // both are there the control is closed and says why, so a click cannot turn into
  // a message about a workspace that was merely still loading.
  const workspaceReady = ready && workspaceID !== null;

  // Every sentence the composer can raise presupposes the workspace, so none of
  // them is shown before it is there: a notice from the loading window goes away by
  // itself the moment the id lands rather than sitting under the field.
  const currentNotice = workspaceReady ? notice : null;

  const now = useMemo(() => new Date(), []);
  // The running count is only as fresh as the stream, so the stats card takes the
  // same status the activity card does: one screen must not carry two claims about
  // the connection.
  const stats = useMemo(
    () => buildStats({ drafts, tasks, agents, status, now }),
    [drafts, tasks, agents, status, now],
  );
  const week = useMemo(() => buildWeek(tasks, now), [tasks, now]);
  const feed = useMemo(() => buildFeed(events, agents), [events, agents]);
  const pending = useMemo(() => buildPending(drafts, agents), [drafts, agents]);

  const initials = (user?.email ?? "A").slice(0, 1).toUpperCase();

  // The standing hint is a statement about the roster, so it waits for the roster:
  // "no Bolu yet" is not true of an account whose agents simply have not arrived,
  // and the button already carries the loading state.
  const hint =
    agents.length === 0
      ? "Belum ada Bolu untuk disuruh. Buat satu dulu di halaman Tim."
      : "Tugas otomatis diteruskan ke anggota yang paling cocok, dan obrolannya muncul di daftar kiri.";

  async function send() {
    const text = instruction.trim();
    if (!text || busy || !workspaceReady) return;
    setBusy(true);
    const outcome = await sendTask(text);
    setBusy(false);
    setNotice(outcome.ok ? "Tugas diteruskan ke timmu." : outcome.message);
    if (outcome.ok) setInstruction("");
  }

  return (
    <div className="flex flex-col gap-[22px] p-[24px_28px_48px] max-[760px]:p-[18px_14px_36px]">
      <div className="flex flex-wrap items-center justify-between gap-4">
        <div>
          <h1 className="font-display text-[36px] font-bold leading-[1.1] max-[760px]:text-[28px]">
            {greeting(now)}
          </h1>
          <div className="text-bolu-muted">
            {longDate(now)} · ini ringkasan kerja {agents.length > 0 ? "tim kecilmu" : "ruang kerjamu"}
          </div>
        </div>
        <div className="flex items-center gap-2.5">
          <button
            type="button"
            aria-label="Notifikasi"
            className="flex size-11 cursor-pointer items-center justify-center rounded-full border border-bolu-border bg-bolu-surface"
          >
            <Bell size={20} strokeWidth={2} aria-hidden="true" />
          </button>
          <div className="flex size-11 items-center justify-center rounded-full bg-bolu-tint font-display font-bold text-bolu-on-tint">
            {initials}
          </div>
        </div>
      </div>

      {/* Suruh Bolu: the one place a task is given. The backend's router is what
          decides which Bolu answers, so the message goes into the team's group
          thread and the choice is made there rather than guessed here. */}
      <div className="flex flex-wrap items-center gap-3.5 rounded-md bg-bolu-solid p-[18px_20px] text-bolu-solid-ink">
        <label htmlFor="suruh" className="flex-none font-display text-[19px] font-semibold max-[760px]:basis-full">
          Suruh Bolu
        </label>
        <input
          id="suruh"
          type="text"
          value={instruction}
          onChange={(event) => setInstruction(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Enter") void send();
          }}
          disabled={!workspaceReady}
          placeholder="misalnya: tagih semua invoice yang telat"
          className="min-h-[46px] min-w-0 flex-[1_1_260px] rounded-full border-0 bg-bolu-surface px-[18px] text-bolu-ink disabled:opacity-60"
        />
        <button
          type="button"
          onClick={() => void send()}
          disabled={busy || !workspaceReady || instruction.trim() === ""}
          className="min-h-[46px] cursor-pointer rounded-full border-0 bg-bolu-accent px-[22px] font-bold text-bolu-solid-ink disabled:cursor-not-allowed disabled:opacity-60"
        >
          {busy ? "Mengirim…" : workspaceReady ? "Kirim tugas" : "Memuat workspace…"}
        </button>
        <div className="basis-full text-[14px] opacity-75">
          {currentNotice ?? (workspaceReady ? hint : null)}
        </div>
      </div>

      <div className="grid grid-cols-[repeat(auto-fit,minmax(min(200px,100%),1fr))] gap-3.5">
        {stats.map((stat) => (
          <div key={stat.label} className={`${CARD} flex flex-col gap-1 p-[18px_20px]`}>
            <div className="text-[15px] font-medium text-bolu-muted">{stat.label}</div>
            <div className="font-display text-[34px] font-bold leading-[1.1]">{stat.value}</div>
            <div className="text-[14px] text-bolu-muted">{stat.note}</div>
          </div>
        ))}
      </div>

      <div className="flex flex-wrap items-start gap-[18px]">
        <section className={`${CARD} flex min-w-0 flex-[999_1_420px] flex-col gap-3 p-5`}>
          <div className="flex flex-wrap items-center justify-between gap-2.5">
            <h2 className={HEADING}>Menunggu persetujuanmu</h2>
            <span className="text-[14px] text-bolu-muted">
              {pending.length === 0 ? "tidak ada" : `${pending.length} draf`}
            </span>
          </div>

          {pending.length === 0 ? (
            <div className="pop flex flex-col items-center gap-2.5 px-3 py-8 text-center">
              <Bolu
                member={agents[0] ? crewFor(agents[0]) : allBot("Pinky")}
                expression="senang"
                color={agents[0]?.color || undefined}
                size={90}
              />
              <div className="font-display text-[17px] font-semibold">Semua beres</div>
              <p className="max-w-[380px] text-[14px] text-bolu-muted">
                Tidak ada yang menunggu. Draf berikutnya muncul di sini begitu tim selesai
                menyiapkannya.
              </p>
            </div>
          ) : (
            <div className="flex flex-col gap-2">
              {pending.map((row) => (
                <div
                  key={row.draft.draft_id}
                  className="flex flex-wrap items-center gap-3 rounded-md border border-bolu-line p-[10px_12px]"
                >
                  <div className="size-11 flex-none">
                    {row.agent ? (
                      <Bolu
                        member={crewFor(row.agent)}
                        expression={expressionOf(stateOf(row.agent))}
                        color={row.agent.color || undefined}
                        size={44}
                      />
                    ) : null}
                  </div>
                  {/* The card opens the draft itself, because a decision made from a
                      one-line summary is a decision made without the payload. The
                      approve button stays beside it for the drafts that need no
                      second look. */}
                  <Link
                    href={`/approval/${row.draft.draft_id}`}
                    className="flex min-w-0 flex-[1_1_200px] flex-col text-bolu-ink no-underline hover:text-bolu-ink"
                  >
                    <div className="text-[13px] font-semibold text-bolu-muted">{row.kind}</div>
                    <div className="truncate font-semibold">{row.title}</div>
                    <div className="text-[14px] text-bolu-muted">{row.meta}</div>
                  </Link>
                  <div className="flex flex-none gap-1.5">
                    <Link
                      href="/chat"
                      className="flex min-h-10 items-center rounded-full border-2 border-bolu-ink bg-bolu-surface px-3.5 text-[14px] font-semibold text-bolu-ink no-underline hover:text-bolu-ink"
                    >
                      Buka obrolan
                    </Link>
                    <button
                      type="button"
                      onClick={() => void decide(row.draft.draft_id, "approve")}
                      className="min-h-10 cursor-pointer rounded-full border-0 bg-bolu-solid px-3.5 text-[14px] font-bold text-bolu-solid-ink"
                    >
                      Setujui
                    </button>
                  </div>
                </div>
              ))}
            </div>
          )}
        </section>

        <section className={`${CARD} flex min-w-0 flex-[1_1_320px] flex-col gap-2.5 p-5`}>
          <div className="flex items-center justify-between">
            <h2 className={HEADING}>Aktivitas</h2>
            {status === "live" ? (
              <span className="pulse text-[13px] font-bold text-bolu-lunas">LIVE</span>
            ) : status === "slow" ? (
              <span className="text-[13px] text-bolu-muted">pembaruan tertunda</span>
            ) : (
              <span className="text-[13px] text-bolu-muted">menyambung…</span>
            )}
          </div>
          <div className="flex flex-col">
            {feed.length === 0 ? (
              <p className="py-4 text-[14px] text-bolu-muted">
                Belum ada aktivitas. Pekerjaan tim muncul di sini begitu mereka mulai.
              </p>
            ) : (
              feed.map((row) => (
                <div key={row.key} className="flex gap-3 border-b border-bolu-line p-[9px_0]">
                  <div className="size-8 flex-none">
                    {row.agent ? (
                      <Bolu
                        member={crewFor(row.agent)}
                        expression={expressionOf(stateOf(row.agent))}
                        color={row.agent.color || undefined}
                        size={32}
                      />
                    ) : (
                      <div className="flex size-8 items-center justify-center rounded-full bg-bolu-solid">
                        <Check size={16} strokeWidth={3} className="text-bolu-solid-ink" aria-hidden="true" />
                      </div>
                    )}
                  </div>
                  <div className="min-w-0 flex-1 text-[15px] leading-[1.4]">
                    <span className="font-semibold">{row.who}</span> {row.text}
                  </div>
                  <div className="flex-none text-[13px] text-bolu-muted">{row.time}</div>
                </div>
              ))
            )}
          </div>
        </section>
      </div>

      <div className="flex flex-wrap items-stretch gap-[18px]">
        <section className={`${CARD} flex min-w-0 flex-[999_1_460px] flex-col gap-4 p-5`}>
          <div className="flex flex-wrap items-baseline justify-between gap-2">
            <h2 className={HEADING}>Tugas selesai minggu ini</h2>
            <span className="text-[14px] text-bolu-muted">per hari, semua anggota</span>
          </div>
          <div className="grid h-[190px] grid-cols-7 items-end gap-3">
            {week.map((bar) => (
              <div key={bar.day} className="flex h-full flex-col items-center justify-end gap-1.5">
                <div className="text-[13px] font-semibold">{bar.label}</div>
                <div
                  className="w-full max-w-[46px] rounded-md"
                  style={{ height: bar.h, backgroundColor: bar.bg }}
                />
                <div className="text-[14px] text-bolu-muted" style={{ fontWeight: bar.weight }}>
                  {bar.day}
                </div>
              </div>
            ))}
          </div>
        </section>

        <section className={`${CARD} flex min-w-0 flex-[1_1_320px] flex-col gap-1 p-5`}>
          <h2 className={`${HEADING} pb-1.5`}>Tugas terakhir</h2>
          {tasks.length === 0 ? (
            <p className="py-3 text-[14px] text-bolu-muted">
              Belum ada tugas. Suruh Bolu di atas untuk memulai.
            </p>
          ) : (
            tasks.slice(0, 4).map((task) => {
              const agent = agents.find((item) => item.id === task.agent_id);
              return (
                <Link
                  key={task.task_id}
                  href={`/tasks/${task.task_id}`}
                  className="flex items-center justify-between gap-2.5 border-b border-bolu-line p-[8px_0] text-[15px] text-bolu-ink no-underline hover:text-bolu-ink"
                >
                  <div className="min-w-0">
                    <div className="truncate font-semibold">{task.title || "Tugas"}</div>
                    <div className="text-[13px] text-bolu-muted">
                      {agent?.name ?? "Bolu"} · {" "}
                      {relativeAge(task.finished_at ?? task.created_at, now)}
                    </div>
                  </div>
                  <div className="flex-none text-right">
                    <div className="text-[13px] font-bold" style={{ color: taskState(task).color }}>
                      {taskState(task).label}
                    </div>
                  </div>
                </Link>
              );
            })
          )}
        </section>
      </div>
    </div>
  );
}


