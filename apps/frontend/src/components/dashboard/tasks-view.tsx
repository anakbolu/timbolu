"use client";

/**
 * Tugas: every task this workspace ran, what state each is in, and the way into one.
 *
 * The list is the history endpoint's own answer, filtered by the server rather than
 * here — the filters are query parameters (`status`, `live`, `agent_id`, `limit`),
 * so what a person narrows to is what the database was asked for. A screen that
 * filtered a page of results in the browser would answer "no failed tasks" while
 * the failed ones sat on the next page, which is the kind of wrong that looks like
 * good news.
 *
 * Three things this screen is careful about:
 *
 * - **the reason a task stopped is shown here, not only on its own page.** A list
 *   that says "Gagal" and makes you open the row to learn why is a list that fails
 *   the question people actually bring to it;
 * - **`stuck` is not `running`.** The label and the colour come from `lib/tasks`,
 *   which reads the derived health, so a task whose worker died is visibly not one
 *   that is working;
 * - **the live filter is the server's notion of live**, which is "still occupies a
 *   Bolu" — queued, running, or parked on a decision.
 */

import Link from "next/link";
import { useCallback, useEffect, useMemo, useState } from "react";

import { Alert } from "@/components/auth/auth-shell";
import { type Task, ensureActiveWorkspace, listTasks } from "@/lib/api";
import { relativeAge, taskState, taskTriggerLabel } from "@/lib/dashboard-view";

import { useWorkspace } from "./workspace-data";

const CARD = "rounded-[26px] border border-bolu-border bg-white";
const HEADING = "font-display text-[22px] font-semibold";
const BUTTON =
  "min-h-10 cursor-pointer rounded-xl border border-bolu-border bg-white px-3.5 text-[14px] font-medium text-bolu-ink transition hover:border-bolu-accent disabled:cursor-default disabled:opacity-50";
const CHIP = (on: boolean) =>
  `min-h-9 cursor-pointer rounded-full border px-3 text-[13px] font-medium transition ${
    on ? "border-bolu-ink bg-bolu-ink text-white" : "border-bolu-border bg-white text-bolu-muted hover:border-bolu-accent"
  }`;

/** The statuses a person can narrow to. Empty is "semua". */
const STATUSES: { value: string; label: string }[] = [
  { value: "", label: "Semua" },
  { value: "running", label: "Berjalan" },
  { value: "waiting_approval", label: "Menunggu kamu" },
  { value: "succeeded", label: "Selesai" },
  { value: "failed", label: "Gagal" },
  { value: "canceled", label: "Dibatalkan" },
];

const LIMITS = [25, 50, 100];

/** TasksView is the task history screen. */
export function TasksView() {
  const { workspaceID, agents, tasks: liveTasks, reloadTasks } = useWorkspace();

  const [tasks, setTasks] = useState<Task[]>([]);
  const [status, setStatus] = useState("");
  const [liveOnly, setLiveOnly] = useState(false);
  const [agentID, setAgentID] = useState("");
  const [limit, setLimit] = useState(50);

  const [load, setLoad] = useState<"loading" | "ready" | "error">("loading");
  const [message, setMessage] = useState<string | null>(null);
  const [now, setNow] = useState(() => Date.now());

  const fetchTasks = useCallback(
    async (workspace: string) => {
      setLoad("loading");
      setMessage(null);

      const result = await listTasks(workspace, {
        status: status || undefined,
        live: liveOnly || undefined,
        agent_id: agentID || undefined,
        limit,
      });

      if (!result.ok) {
        setLoad("error");
        setMessage(result.message);
        return;
      }

      setTasks(result.data);
      setLoad("ready");
    },
    [status, liveOnly, agentID, limit],
  );

  useEffect(() => {
    let cancelled = false;

    void (async () => {
      const workspace = workspaceID ?? (await ensureActiveWorkspace());
      if (cancelled || !workspace) {
        return;
      }

      await fetchTasks(workspace);
    })();

    return () => {
      cancelled = true;
    };
  }, [workspaceID, fetchTasks]);

  // A task that is still running has a duration and an age that grow while the page is open, and a
  // list of frozen numbers is a list somebody reloads to trust. One tick a second is enough for
  // "12 menit lalu" and cheap enough not to matter.
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);

  const crewName = useMemo(() => {
    const byID = new Map(agents.map((agent) => [agent.id, agent.name]));
    return (id: string) => byID.get(id) ?? "Bolu";
  }, [agents]);

  /**
   * refresh re-reads the list and the store's own copy.
   *
   * Both, because the store feeds the dashboard and the chat, and a task cancelled
   * here must not still read "berjalan" in the sidebar. The extra read is one query
   * per press, which is nothing next to being told two different things.
   */
  const refresh = useCallback(async () => {
    const workspace = workspaceID ?? (await ensureActiveWorkspace());
    if (!workspace) {
      return;
    }

    await Promise.all([fetchTasks(workspace), reloadTasks()]);
  }, [workspaceID, fetchTasks, reloadTasks]);

  return (
    <div className="flex flex-col gap-6 p-[24px_28px_48px] max-[760px]:p-[18px_14px_36px]">
      <header className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className={HEADING}>Tugas</h1>
          <p className="mt-1 text-[15px] text-bolu-muted">
            Semua pekerjaan yang pernah dijalankan workspace ini, terbaru di atas.
          </p>
        </div>

        <button type="button" className={BUTTON} onClick={() => void refresh()} disabled={load === "loading"}>
          {load === "loading" ? "Memuat…" : "Muat ulang"}
        </button>
      </header>

      {message ? <Alert tone="error">{message}</Alert> : null}

      <section className={`${CARD} flex flex-col gap-3 p-5`}>
        <div className="flex flex-wrap items-center gap-2">
          {STATUSES.map((option) => (
            <button
              key={option.value || "all"}
              type="button"
              className={CHIP(status === option.value)}
              onClick={() => setStatus(option.value)}
            >
              {option.label}
            </button>
          ))}

          <button type="button" className={CHIP(liveOnly)} onClick={() => setLiveOnly((on) => !on)}>
            Sedang dikerjakan
          </button>
        </div>

        <div className="flex flex-wrap items-center gap-3">
          <label className="flex items-center gap-2 text-[14px] text-bolu-muted">
            Bolu
            <select
              value={agentID}
              onChange={(event) => setAgentID(event.target.value)}
              className="min-h-10 rounded-xl border border-bolu-border bg-white px-2.5 text-[14px] text-bolu-ink"
            >
              <option value="">Semua Bolu</option>
              {agents.map((agent) => (
                <option key={agent.id} value={agent.id}>
                  {agent.name}
                </option>
              ))}
            </select>
          </label>

          <label className="flex items-center gap-2 text-[14px] text-bolu-muted">
            Tampilkan
            <select
              value={limit}
              onChange={(event) => setLimit(Number(event.target.value))}
              className="min-h-10 rounded-xl border border-bolu-border bg-white px-2.5 text-[14px] text-bolu-ink"
            >
              {LIMITS.map((option) => (
                <option key={option} value={option}>
                  {option} terakhir
                </option>
              ))}
            </select>
          </label>
        </div>
      </section>

      <section className={`${CARD} overflow-hidden`}>
        {load === "ready" && tasks.length === 0 ? (
          <p className="px-6 py-8 text-[15px] text-bolu-muted">
            {liveTasks.length === 0 && !status && !liveOnly && !agentID
              ? "Belum ada tugas. Suruh Bolu dari Dasbor atau Obrolan untuk memulai."
              : "Tidak ada tugas yang cocok dengan penyaring ini."}
          </p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[760px] text-[14px]">
              <thead className="text-left text-bolu-muted">
                <tr>
                  <th className="px-6 py-3 font-medium">Tugas</th>
                  <th className="px-3 py-3 font-medium">Bolu</th>
                  <th className="px-3 py-3 font-medium">Keadaan</th>
                  <th className="px-3 py-3 text-right font-medium">Langkah</th>
                  <th className="px-6 py-3 text-right font-medium">Umur</th>
                </tr>
              </thead>
              <tbody>
                {tasks.map((task) => {
                  const state = taskState(task);
                  return (
                    <tr key={task.task_id} className="border-t border-bolu-line align-top">
                      <td className="px-6 py-3">
                        <Link
                          href={`/tasks/${task.task_id}`}
                          className="font-semibold text-bolu-ink no-underline hover:underline"
                        >
                          {task.title || "Tugas"}
                        </Link>
                        <div className="text-[13px] text-bolu-muted">
                          {taskTriggerLabel(task.trigger)}
                          {task.depth > 0 ? ` · serah-terima ke-${task.depth}` : ""}
                        </div>
                      </td>
                      <td className="px-3 py-3">{crewName(task.agent_id)}</td>
                      <td className="px-3 py-3">
                        <span className="font-semibold" style={{ color: state.color }}>
                          {state.label}
                        </span>
                        {/* The reason is what the row is actually read for. It is shown as the
                            backend wrote it, because it is written for the person reading. */}
                        {task.stopped_reason ? (
                          <div className="max-w-[420px] text-[13px] text-bolu-muted">{task.stopped_reason}</div>
                        ) : null}
                        {!task.stopped_reason && task.waiting_reason ? (
                          <div className="max-w-[420px] text-[13px] text-bolu-muted">{task.waiting_reason}</div>
                        ) : null}
                        {!task.stopped_reason && !task.waiting_reason && state.note ? (
                          <div className="max-w-[420px] text-[13px] text-bolu-muted">{state.note}</div>
                        ) : null}
                      </td>
                      <td className="px-3 py-3 text-right">{task.step_count}</td>
                      <td className="px-6 py-3 text-right text-bolu-muted">{relativeAge(task.created_at, new Date(now))}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </div>
  );
}
