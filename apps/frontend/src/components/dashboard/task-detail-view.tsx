"use client";

/**
 * One task: what it did, round by round, and what it handed off.
 *
 * The page answers three questions in the order people ask them:
 *
 *  1. **what happened?** — the state, and the reason it stopped, in the backend's
 *     own words. A task stopped by a step or token bound has a `stopped_reason`
 *     written for a person, and it is shown as-is rather than reworded here;
 *  2. **what did it do?** — every recorded round in sequence: the model thinking, the
 *     tools it called, what they answered, and the final answer. A round whose raw
 *     payload the retention job removed says so, and that is a different thing from
 *     a round that had nothing in it;
 *  3. **what did it cause?** — the tasks it handed work to, each opening its own page.
 *
 * The cancel button is the one action, and it is offered only while the task could
 * still be running. What comes back from cancelling is the task as it now is, not an
 * acknowledgement: a task that had already succeeded is shown as succeeded, because
 * the answer is the truth and the button was the request.
 */

import Link from "next/link";
import { useCallback, useEffect, useState } from "react";

import { Alert } from "@/components/auth/auth-shell";
import {
  type Task,
  type TaskStep,
  cancelTask,
  ensureActiveWorkspace,
  getTask,
  listTaskChildren,
  listTaskSteps,
} from "@/lib/api";
import {
  formatMoment,
  formatTokens,
  taskDuration,
  taskState,
  taskStepLabel,
  taskTriggerLabel,
} from "@/lib/dashboard-view";

import { useWorkspace } from "./workspace-data";

const CARD = "rounded-md border border-bolu-border bg-bolu-surface";
const HEADING = "font-display text-[22px] font-semibold";
const BUTTON =
  "min-h-10 cursor-pointer rounded-md border border-bolu-border bg-bolu-surface px-3.5 text-[14px] font-medium text-bolu-ink transition hover:border-bolu-accent disabled:cursor-default disabled:opacity-50";

/** KIND_TONE is how each round reads at a glance: the model, a tool, or the answer. */
const KIND_TONE: Record<string, string> = {
  think: "border-bolu-border bg-bolu-panel/60 text-bolu-body",
  tool_call: "border-bolu-accent/40 bg-bolu-accent/5 text-bolu-accent",
  tool_result: "border-bolu-lunas/30 bg-bolu-lunas/8 text-bolu-lunas",
  final: "border-bolu-ink/20 bg-bolu-solid/5 text-bolu-ink",
};

/** TaskDetailView is one task's page. */
export function TaskDetailView({ taskID }: { taskID: string }) {
  const { workspaceID, agents, reloadTasks } = useWorkspace();

  const [task, setTask] = useState<Task | null>(null);
  const [steps, setSteps] = useState<TaskStep[]>([]);
  const [children, setChildren] = useState<Task[]>([]);
  const [load, setLoad] = useState<"loading" | "ready" | "error">("loading");
  const [message, setMessage] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [cancelling, setCancelling] = useState(false);
  // A running task's duration grows while the page is open, and a number frozen at load is a number a
  // person reloads to trust. The clock is state rather than a call in render, because reading it
  // during render is impure and would make two renders of the same data disagree.
  const [now, setNow] = useState(() => Date.now());

  const read = useCallback(
    async (workspace: string) => {
      setLoad("loading");
      setMessage(null);

      const [taskResult, stepsResult, childrenResult] = await Promise.all([
        getTask(workspace, taskID),
        listTaskSteps(workspace, taskID),
        listTaskChildren(workspace, taskID),
      ]);

      if (!taskResult.ok) {
        setLoad("error");
        setMessage(taskResult.message);
        return;
      }

      setTask(taskResult.data);
      // A task with no steps is a task that has not run yet, which is not an error, so a failed read
      // of one section does not fail the page: what a person came for is the state and the reason.
      setSteps(stepsResult.ok ? stepsResult.data : []);
      setChildren(childrenResult.ok ? childrenResult.data : []);
      setLoad("ready");
    },
    [taskID],
  );

  useEffect(() => {
    let cancelled = false;

    void (async () => {
      const workspace = workspaceID ?? (await ensureActiveWorkspace());
      if (cancelled || !workspace) {
        return;
      }

      await read(workspace);
    })();

    return () => {
      cancelled = true;
    };
  }, [workspaceID, read]);

  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);

  /** cancel stops a task that has not finished, after the person confirms. */
  const cancel = useCallback(async () => {
    const workspace = workspaceID ?? (await ensureActiveWorkspace());
    if (!workspace || !task) {
      return;
    }

    // Asked here rather than in a dialog component, because the sentence has to name what is being
    // stopped: "batalkan tugas ini?" means nothing next to four rows that look alike.
    const confirmed = window.confirm(
      `Batalkan tugas ini?\n\n${task.title || task.task_id}\n\nBolu berhenti di langkah berikutnya, dan hasilnya tidak akan ada.`,
    );
    if (!confirmed) {
      return;
    }

    setCancelling(true);
    setNotice(null);

    const result = await cancelTask(workspace, task.task_id);
    setCancelling(false);

    if (!result.ok) {
      setMessage(result.message);
      return;
    }

    // The answer is the task as it now is: a task that had already finished comes back finished, and
    // saying "dibatalkan" here would be a lie the screen told on the button's behalf.
    setTask(result.data);
    setNotice(
      taskState(result.data).label === "Dibatalkan"
        ? "Tugas dibatalkan."
        : `Tugas tidak dibatalkan — keadaannya sudah "${taskState(result.data).label}".`,
    );
    await read(workspace);
    await reloadTasks();
  }, [workspaceID, task, read, reloadTasks]);

  const crewName = (id: string) => agents.find((agent) => agent.id === id)?.name ?? "Bolu";
  const state = task ? taskState(task) : null;

  if (load === "error") {
    return (
      <div className="flex flex-col gap-4">
        <Alert tone="error">{message ?? "Tugas tidak ditemukan."}</Alert>
        <Link href="/dashboard" className={`${BUTTON} w-fit no-underline`}>
          ← Kembali ke Dasbor
        </Link>
      </div>
    );
  }

  if (!task || !state) {
    return <p className="text-[15px] text-bolu-muted">Memuat tugas…</p>;
  }

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <Link href="/dashboard" className="text-[14px] text-bolu-muted no-underline hover:underline">
            ← Dasbor
          </Link>
          <h1 className={`${HEADING} mt-1`}>{task.title || "Tugas"}</h1>
          <p className="mt-1 text-[15px] text-bolu-muted">
            {crewName(task.agent_id)} · {taskTriggerLabel(task.trigger)} · {formatMoment(task.created_at)}
          </p>
        </div>

        <div className="flex flex-none items-center gap-2">
          {/* Offered only while the task can still be stopped: a button on a finished task is a
              button that teaches people the product lies about what it can do. */}
          {state.live && task.status !== "waiting_approval" ? (
            <button type="button" className={BUTTON} onClick={() => void cancel()} disabled={cancelling}>
              {cancelling ? "Membatalkan…" : "Batalkan tugas"}
            </button>
          ) : null}
          {task.status === "waiting_approval" && task.waiting_draft_id ? (
            <Link href="/dashboard" className={`${BUTTON} no-underline`}>
              Lihat drafnya
            </Link>
          ) : null}
        </div>
      </div>

      {notice ? <Alert tone="info">{notice}</Alert> : null}
      {message ? <Alert tone="error">{message}</Alert> : null}

      <section className={`${CARD} flex flex-col gap-4 p-5`}>
        <div className="flex flex-wrap items-center gap-x-8 gap-y-3">
          <div>
            <div className="text-[13px] text-bolu-muted">Keadaan</div>
            <div className="text-[17px] font-semibold" style={{ color: state.color }}>
              {state.label}
            </div>
          </div>
          <div>
            <div className="text-[13px] text-bolu-muted">Durasi</div>
            <div className="text-[17px] font-semibold">{taskDuration(task, now) || "—"}</div>
          </div>
          <div>
            <div className="text-[13px] text-bolu-muted">Langkah</div>
            <div className="text-[17px] font-semibold">{task.step_count}</div>
          </div>
          <div>
            <div className="text-[13px] text-bolu-muted">Token masuk / keluar</div>
            <div className="text-[17px] font-semibold">
              {formatTokens(task.input_tokens)} / {formatTokens(task.output_tokens)}
            </div>
          </div>
          {task.workflow_id ? (
            <div className="min-w-0">
              <div className="text-[13px] text-bolu-muted">Workflow</div>
              <div className="truncate font-mono text-[13px]">{task.workflow_id}</div>
            </div>
          ) : null}
        </div>

        {/* The reason comes first among the explanations, because it is the question a person opens
            a failed or bound-hit task with. */}
        {task.stopped_reason ? (
          <Alert tone="error">Tugas berhenti: {task.stopped_reason}</Alert>
        ) : null}
        {/* Gated on the task still being live: the reason is kept as history by the backend, and a
            cancelled task that still announced what it was waiting for would say two things at once —
            one of which is that it is waiting. */}
        {state.live && task.waiting_reason ? <Alert tone="info">Menunggu: {task.waiting_reason}</Alert> : null}
        {task.summary ? <p className="text-[15px]">{task.summary}</p> : null}
      </section>

      <section className={`${CARD} overflow-hidden`}>
        <h2 className={`${HEADING} px-6 pb-3 pt-5`}>Langkahnya</h2>

        {steps.length === 0 ? (
          <p className="px-6 pb-6 text-[15px] text-bolu-muted">
            {state.live ? "Belum ada langkah yang tercatat." : "Tidak ada langkah yang tercatat untuk tugas ini."}
          </p>
        ) : (
          <ol className="flex flex-col">
            {steps.map((step) => (
              <li key={step.step_id} className="flex flex-col gap-2 border-t border-bolu-line px-6 py-4">
                <div className="flex flex-wrap items-center gap-3">
                  <span className="w-8 flex-none text-right font-mono text-[13px] text-bolu-muted">{step.seq}</span>
                  <span
                    className={`rounded-full border px-2.5 py-0.5 text-[12px] font-semibold ${
                      KIND_TONE[step.kind] ?? "border-bolu-border bg-bolu-surface text-bolu-body"
                    }`}
                  >
                    {taskStepLabel(step.kind)}
                  </span>
                  {step.tool_name ? (
                    <span className="font-mono text-[13px] text-bolu-ink">{step.tool_name}</span>
                  ) : null}
                  {step.tool_label ? (
                    <span className="text-[12px] text-bolu-muted">{step.tool_label}</span>
                  ) : null}
                  {step.input_tokens || step.output_tokens ? (
                    <span className="text-[12px] text-bolu-muted">
                      {formatTokens(step.input_tokens)} masuk / {formatTokens(step.output_tokens)} keluar
                    </span>
                  ) : null}
                  <span className="ml-auto text-[12px] text-bolu-muted">{formatMoment(step.created_at)}</span>
                </div>

                {/* A pruned round is stated, not left blank: "the retention job removed this" and
                    "nothing happened here" are two different facts and only one of them is true. */}
                {step.pruned ? (
                  <p className="pl-11 text-[13px] text-bolu-muted">
                    Isinya sudah dipangkas oleh kebijakan retensi; bentuk langkahnya tetap disimpan.
                  </p>
                ) : (
                  <StepBody input={step.input} output={step.output} />
                )}
              </li>
            ))}
          </ol>
        )}
      </section>

      <section className={`${CARD} flex flex-col gap-3 p-5`}>
        <h2 className={HEADING}>Serah-terima</h2>

        {children.length === 0 ? (
          <p className="text-[15px] text-bolu-muted">
            Tugas ini tidak menyerahkan pekerjaan ke Bolu lain.
          </p>
        ) : (
          <ul className="flex flex-col">
            {children.map((child) => {
              const childState = taskState(child);
              return (
                <li key={child.task_id} className="border-t border-bolu-line first:border-t-0">
                  <Link
                    href={`/tasks/${child.task_id}`}
                    className="flex flex-wrap items-center justify-between gap-2 py-3 text-[15px] text-bolu-ink no-underline hover:text-bolu-ink"
                  >
                    <span className="min-w-0">
                      <span className="font-semibold">{child.title || "Tugas"}</span>
                      <span className="ml-2 text-[13px] text-bolu-muted">
                        {crewName(child.agent_id)} · langkah {child.depth}
                      </span>
                    </span>
                    <span className="font-semibold" style={{ color: childState.color }}>
                      {childState.label}
                    </span>
                  </Link>
                </li>
              );
            })}
          </ul>
        )}
      </section>
    </div>
  );
}

/**
 * StepBody renders a round's payload.
 *
 * It is JSON, and it is shown as JSON: the payload is a tool's arguments or a model's
 * answer, and reformatting it into prose would mean guessing what it says. A round
 * that carries neither is said to be empty rather than drawn as an empty box.
 */
function StepBody({ input, output }: { input?: unknown; output?: unknown }) {
  if (input === undefined && output === undefined) {
    return <p className="pl-11 text-[13px] text-bolu-muted">Tidak ada isi pada langkah ini.</p>;
  }

  return (
    <div className="flex flex-col gap-2 pl-11">
      {input !== undefined ? <Payload title="Masuk" value={input} /> : null}
      {output !== undefined ? <Payload title="Keluar" value={output} /> : null}
    </div>
  );
}

function Payload({ title, value }: { title: string; value: unknown }) {
  return (
    <div className="min-w-0">
      <div className="text-[12px] text-bolu-muted">{title}</div>
      <pre className="mt-1 max-h-72 overflow-auto whitespace-pre-wrap break-words rounded-md border border-bolu-line bg-bolu-panel/40 p-3 font-mono text-[12px] leading-relaxed">
        {typeof value === "string" ? value : JSON.stringify(value, null, 2)}
      </pre>
    </div>
  );
}
