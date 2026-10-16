"use client";

/**
 * Rencana: the plan each Bolu is working from, and the ones it has finished.
 *
 * A Bolu writes its own list — the tasks, the subtasks, and the ticks — through its tools, and this
 * screen only reads it. That is the whole reason there is no checkbox to press here: a client that
 * could tick a line could make a Bolu believe work was done that nobody did. What a person can do is
 * *see* what a Bolu is doing, which is the question this section exists to answer — the history below
 * says what it did, and this says what it is doing.
 *
 * The list is the endpoint's own answer, so the filter is a query parameter rather than a filter in the
 * browser: narrowing a page of results here would answer "no plans" while the plans sat on the next
 * page, which is the kind of wrong that looks like good news.
 */

import { Check } from "lucide-react";
import { useCallback, useEffect, useState } from "react";

import { Alert } from "@/components/auth/auth-shell";
import { Bolu } from "@/components/bolu/bolu";
import { type Agent, type Checklist, type ChecklistItem, ensureActiveWorkspace, listChecklists } from "@/lib/api";
import { crewFor, expressionOf, relativeAge, stateOf } from "@/lib/dashboard-view";

import { useWorkspace } from "./workspace-data";

const CARD = "rounded border border-bolu-border bg-bolu-surface";
const CHIP = (on: boolean) =>
  `min-h-9 cursor-pointer rounded-full border px-3 text-[13px] font-medium transition ${
    on ? "border-bolu-ink bg-bolu-solid text-bolu-solid-ink" : "border-bolu-border bg-bolu-surface text-bolu-muted hover:border-bolu-accent"
  }`;

/** How many plans to read. One page is enough: a Bolu holds one open list at a time. */
const LIMIT = 25;

/** ChecklistsView lists what the Bolus are working from. */
export function ChecklistsView() {
  const { workspaceID, agents } = useWorkspace();

  const [plans, setPlans] = useState<Checklist[]>([]);
  const [activeOnly, setActiveOnly] = useState(false);
  const [load, setLoad] = useState<"loading" | "ready" | "error">("loading");
  const [message, setMessage] = useState<string | null>(null);

  const fetchPlans = useCallback(
    async (workspace: string) => {
      setLoad("loading");
      setMessage(null);

      const result = await listChecklists(workspace, {
        status: activeOnly ? "active" : undefined,
        limit: LIMIT,
      });

      if (!result.ok) {
        setLoad("error");
        setMessage(result.message);
        return;
      }

      setPlans(result.data);
      setLoad("ready");
    },
    [activeOnly],
  );

  useEffect(() => {
    let cancelled = false;

    void (async () => {
      const workspace = workspaceID ?? (await ensureActiveWorkspace());
      if (cancelled || !workspace) {
        return;
      }

      await fetchPlans(workspace);
    })();

    return () => {
      cancelled = true;
    };
  }, [workspaceID, fetchPlans]);

  const byID = new Map(agents.map((agent) => [agent.id, agent]));
  const open = plans.filter((plan) => plan.status === "active").length;

  return (
    <section className="flex flex-col gap-3">
      <header className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h2 className="font-display text-[18px] font-semibold">Rencana Bolu</h2>
          <p className="mt-1 text-[14px] text-bolu-muted">
            Apa yang sedang dikerjakan tiap Bolu, dan yang sudah selesai. Daftarnya ditulis dan
            dicentang oleh Bolunya sendiri.
          </p>
        </div>

        <div className="flex flex-wrap items-center gap-2">
          <button type="button" className={CHIP(!activeOnly)} onClick={() => setActiveOnly(false)}>
            Semua
          </button>
          <button type="button" className={CHIP(activeOnly)} onClick={() => setActiveOnly(true)}>
            Berjalan{open > 0 ? ` (${open})` : ""}
          </button>
        </div>
      </header>

      {message ? <Alert tone="error">{message}</Alert> : null}

      {load === "loading" && plans.length === 0 ? (
        <p className={`${CARD} px-6 py-6 text-[15px] text-bolu-muted`}>Memuat rencana…</p>
      ) : null}

      {load === "ready" && plans.length === 0 ? (
        <p className={`${CARD} px-6 py-6 text-[15px] text-bolu-muted`}>
          {activeOnly
            ? "Tidak ada Bolu yang sedang memegang rencana."
            : "Belum ada rencana. Bolu menulisnya sendiri kalau pekerjaannya butuh beberapa langkah."}
        </p>
      ) : null}

      <div className="grid gap-3 min-[980px]:grid-cols-2">
        {plans.map((plan) => (
          <PlanCard key={plan.checklist_id} plan={plan} agent={byID.get(plan.agent_id)} />
        ))}
      </div>
    </section>
  );
}

/** PlanCard is one plan: whose it is, how far along, and the lines themselves. */
function PlanCard({ plan, agent }: { plan: Checklist; agent?: Agent }) {
  const percent = plan.total_count > 0 ? Math.round((plan.done_count / plan.total_count) * 100) : 0;

  return (
    <article className={`${CARD} flex flex-col gap-3 p-4`}>
      <div className="flex items-center gap-3">
        {agent ? (
          <Bolu
            member={crewFor(agent)}
            expression={expressionOf(stateOf(agent))}
            color={agent.color || undefined}
            size={36}
          />
        ) : null}

        <div className="min-w-0 flex-1">
          <div className="truncate font-display text-[16px] font-semibold">{plan.title || "Rencana"}</div>
          <div className="truncate text-[13px] text-bolu-muted">
            {agent?.name ?? "Bolu"} ·{" "}
            {plan.status === "active"
              ? `ditulis ${relativeAge(plan.created_at)}`
              : `selesai ${relativeAge(plan.completed_at ?? plan.created_at)}`}
          </div>
        </div>

        <span
          className={`flex-none rounded-full px-2.5 py-1 text-[12px] font-semibold ${
            plan.status === "active" ? "bg-bolu-solid text-bolu-solid-ink" : "bg-bolu-line text-bolu-muted"
          }`}
        >
          {plan.status === "active" ? "Berjalan" : "Selesai"}
        </span>
      </div>

      {/* The bar is what makes "4 dari 9" readable at a glance; the fraction is what makes it exact. */}
      <div className="flex items-center gap-2">
        <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-bolu-line">
          <div
            className="h-full rounded-full bg-bolu-accent transition-[width]"
            style={{ width: `${percent}%` }}
          />
        </div>
        <span className="flex-none text-[12px] text-bolu-muted">
          {plan.done_count}/{plan.total_count}
        </span>
      </div>

      <ul className="flex flex-col gap-1.5 text-[14px]">
        {plan.items.map((item) => (
          <li key={item.item_id} className="flex flex-col gap-1">
            <Line item={item} />
            {item.subtasks?.length ? (
              <ul className="ml-6 flex flex-col gap-1">
                {item.subtasks.map((subtask) => (
                  <li key={subtask.item_id}>
                    <Line item={subtask} />
                  </li>
                ))}
              </ul>
            ) : null}
          </li>
        ))}
      </ul>
    </article>
  );
}

/**
 * Line is one task or subtask.
 *
 * A ticked line is struck through as well as marked, because colour alone is not an answer for
 * somebody who cannot see it — and because a list where "done" is a shade of grey is a list that gets
 * misread.
 */
function Line({ item }: { item: ChecklistItem }) {
  return (
    <span className="flex items-start gap-2">
      <span
        className={`mt-[2px] flex size-4 flex-none items-center justify-center rounded-[4px] border ${
          item.done ? "border-bolu-accent bg-bolu-accent text-bolu-solid-ink" : "border-bolu-border bg-bolu-surface"
        }`}
        aria-hidden
      >
        {item.done ? <Check className="size-3" strokeWidth={3} /> : null}
      </span>
      <span className={item.done ? "text-bolu-muted line-through" : "text-bolu-ink"}>
        {item.title}
        <span className="sr-only">{item.done ? " (selesai)" : " (belum selesai)"}</span>
      </span>
    </span>
  );
}
