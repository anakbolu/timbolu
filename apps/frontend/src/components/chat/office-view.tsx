"use client";

/**
 * The office: a room where each Bolu is drawn at a place derived from its state.
 *
 * The backend stores no coordinate. It publishes `agent.state` (working,
 * waiting, idle, resting, thinking), and the room is computed from that here:
 * the desk a Bolu works at, the queue by the approval table, the sofa, or the
 * bean bag. That is what keeps the office consistent with the task list by
 * construction rather than by two systems agreeing.
 */

import { useEffect, useMemo, useState } from "react";

import { BotSvg } from "@/components/bolu/bot-svg";
import type { AgentStatePayload, Agent, ActivityEvent } from "@/lib/api";
import { ensureActiveWorkspace, listAgentTeams, refreshSession } from "@/lib/api";
import { bot, type BotShape } from "@/lib/crew";
import { connect, type RealtimeStatus } from "@/lib/realtime";

import { BEAN, FURN, HOME, PLACE, QUEUE, SOFA, ST } from "@/components/dashboard/office";

/** Where a Bolu stands, per state. */
const SPOT: Record<string, "desk" | "queue" | "sofa" | "bean"> = {
  working: "desk",
  thinking: "desk",
  waiting: "queue",
  idle: "sofa",
  resting: "bean",
};

const LEGEND: { color: string; label: string }[] = [
  { color: "#2FA65A", label: "Bekerja di mejanya" },
  { color: "#E2602B", label: "Antre di meja persetujuanmu" },
  { color: "#4A86E8", label: "Santai di pojok sofa" },
  { color: "#8B88A0", label: "Istirahat" },
];

const STATE_LABEL: Record<string, string> = {
  working: "bekerja",
  thinking: "berpikir",
  waiting: "menunggu kamu",
  idle: "santai",
  resting: "istirahat",
};

type Placed = {
  agent: Agent;
  state: string;
  reason: string;
  x: number;
  y: number;
};

export function OfficeView() {
  const [agents, setAgents] = useState<Agent[]>([]);
  const [states, setStates] = useState<Record<string, AgentStatePayload>>({});
  const [status, setStatus] = useState<RealtimeStatus>("connecting");
  const [error, setError] = useState<string | null>(null);
  const [workspaceId, setWorkspaceId] = useState<string | null>(null);

  useEffect(() => {
    void (async () => {
      const session = await refreshSession();
      if (!session.ok) {
        setError("Sesi berakhir. Masuk lagi.");
        return;
      }

      const active = await ensureActiveWorkspace();
      setWorkspaceId(active);
      if (!active) {
        setError("Pilih workspace dulu.");
        return;
      }

      const result = await listAgentTeams(active);
      if (!result.ok) {
        setError(result.message);
        return;
      }
      setAgents(result.data.flatMap((team) => team.agents));
    })();
  }, []);

  // The office is a view of the same stream as the chat and the feed. The id comes
  // from the load above rather than from storage, so a tab that had none stored
  // still opens its stream once the workspace is known.
  useEffect(() => {
    if (!workspaceId) {
      return;
    }

    const handle = connect(
      workspaceId,
      {
        onEvent: (event: ActivityEvent) => {
          if (event.type !== "agent.state") {
            return;
          }
          const payload = readState(event);
          if (!payload) {
            return;
          }
          setStates((previous) => ({ ...previous, [payload.agent_id]: payload }));
        },
        onStatus: setStatus,
      },
      0,
    );

    return () => handle.close();
  }, [workspaceId]);

  const placed = useMemo(() => place(agents, states), [agents, states]);

  return (
    <div className="flex flex-col gap-[18px] p-[24px_28px_48px] max-[760px]:p-[18px_14px_36px]">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="font-display text-[36px] font-bold leading-[1.1] max-[760px]:text-[28px]">
            Kantor Tim Bolu
          </h1>
          <div className="text-bolu-muted">
            Posisi tiap Bolu dihitung dari statusnya, jadi kantor ini selalu sama dengan daftar tugas.
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2 text-[14px]">
          {LEGEND.map((item) => (
            <span key={item.label} className="inline-flex items-center gap-1.5">
              <span className="size-2.5 rounded-full" style={{ backgroundColor: item.color }} />
              {item.label}
            </span>
          ))}
          <span className="rounded-full bg-bolu-panel px-2.5 py-1 text-xs text-bolu-muted">
            {status === "live" ? "langsung" : status}
          </span>
        </div>
      </div>

      {error ? (
        <p role="alert" className="rounded-xl border border-bolu-belum/30 bg-bolu-belum/8 px-3 py-2 text-sm text-bolu-belum">
          {error}
        </p>
      ) : null}

      <div className="overflow-x-auto rounded-[32px]">
        <div className="relative box-border aspect-[16/10] min-w-[820px] overflow-hidden rounded-[32px] border-[12px] border-[#2B2840] bg-[#EFE4D6] bg-[linear-gradient(#E4D6C4_2px,transparent_2px),linear-gradient(90deg,#E4D6C4_2px,transparent_2px)] bg-[length:64px_64px] max-[760px]:min-w-[620px]">
          <div className="absolute inset-x-0 top-0 h-[9%] bg-[#DCD3EA]" />
          <div className="absolute left-[66%] top-[1.6%] flex h-[5.6%] w-[12%] items-center justify-center rounded-lg bg-[#2B2840] font-display text-[14px] font-semibold text-white">
            Kantor Bolu
          </div>

          {FURN.map((furniture, index) => (
            <div
              key={`${furniture.label}-${index}`}
              className="absolute box-border rounded-xl"
              style={{
                left: `${furniture.x}%`,
                top: `${furniture.y}%`,
                width: `${furniture.w}%`,
                height: `${furniture.h}%`,
                backgroundColor: furniture.bg,
                ...furniture.extra,
              }}
            >
              <span className="absolute bottom-[-22px] left-1/2 -translate-x-1/2 whitespace-nowrap text-[12px] font-bold text-[#6B6880]">
                {furniture.label}
              </span>
            </div>
          ))}

          {placed.map(({ agent, state, reason, x, y }) => (
            <div
              key={agent.id}
              className="absolute z-10 flex -translate-x-1/2 -translate-y-1/2 flex-col items-center transition-all duration-700 ease-out"
              style={{ left: `${x}%`, top: `${y}%` }}
            >
              <BotSvg
                bot={bot(agent.name) as BotShape}
                className="size-[54px] drop-shadow max-[760px]:size-[40px]"
              />
              <span className="mt-0.5 whitespace-nowrap rounded-full bg-white/85 px-2 py-0.5 text-[11px] font-semibold text-[#3E3B52]">
                {agent.name} · {STATE_LABEL[state] ?? state}
              </span>
              {reason ? (
                <span className="whitespace-nowrap text-[10px] text-[#6B6880]">{reason}</span>
              ) : null}
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}

/**
 * place turns the agents and their states into positions.
 *
 * The position is derived, never stored: the desk is the Bolu's own, the queue
 * and the sofa have fixed slots assigned in list order, and a Bolu whose state
 * is unknown stands at its home desk. Two Bolu can therefore never be drawn on
 * top of each other, which a free-form coordinate would not guarantee.
 */
function place(agents: Agent[], states: Record<string, AgentStatePayload>): Placed[] {
  const counters: Record<string, number> = {};

  return agents.map((agent) => {
    const payload = states[agent.id];
    const state = payload?.state ?? "idle";
    const reason = payload?.reason ?? "";
    const spot = SPOT[state] ?? "desk";

    const seat = counters[spot] ?? 0;
    counters[spot] = seat + 1;

    const home = ST[HOME[agent.name] ?? ""] ?? [50, 60];
    const position =
      spot === "queue"
        ? (QUEUE[seat % QUEUE.length] ?? home)
        : spot === "sofa"
          ? (SOFA[seat % SOFA.length] ?? home)
          : spot === "bean"
            ? (BEAN[seat % BEAN.length] ?? home)
            : home;

    return {
      agent,
      state,
      reason,
      x: position[0],
      y: position[1],
    };
  });
}

/** readState narrows one streamed agent.state payload. */
function readState(event: ActivityEvent): AgentStatePayload | null {
  const raw = event.payload;
  if (!raw) {
    return null;
  }

  const agentId = typeof raw.agent_id === "string" ? raw.agent_id : event.agent_id;
  const state = typeof raw.state === "string" ? raw.state : null;
  if (!agentId || !state) {
    return null;
  }

  return {
    agent_id: agentId,
    state: state as AgentStatePayload["state"],
    reason: typeof raw.reason === "string" ? raw.reason : undefined,
    task_id: typeof raw.task_id === "string" ? raw.task_id : undefined,
    draft_id: typeof raw.draft_id === "string" ? raw.draft_id : undefined,
  };
}

/** PLACE is exported by the design tables and kept here for the tooltip copy. */
export const OFFICE_PLACE = PLACE;
