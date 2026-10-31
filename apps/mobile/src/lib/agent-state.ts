/**
 * What an `agent.state` frame says a Bolu is doing, and how the app says it.
 *
 * The names are copied from `packages/contracts/events.ts`, which owns them:
 * that package is outside Metro's root and importing it needs a monorepo
 * `metro.config.js`, the same reason `@/lib/bolu.ts` copies the palette. A state
 * added or renamed there must be added or renamed here in the same change.
 */

export const AGENT_STATES = ["working", "thinking", "waiting", "idle", "resting"] as const;

export type AgentState = (typeof AGENT_STATES)[number];

/** The expression `BoluFace` draws; one per state, so the mapping stays explicit. */
export type BoluExpression = AgentState;

/** One parsed `agent.state` body, with the fields this app shows. */
export type AgentStateUpdate = {
  agent_id: string;
  state: AgentState;
  /** The Bolu's own sentence about the work; absent when the state says it all. */
  reason?: string;
};

const KNOWN_STATES: Record<string, true> = {
  working: true,
  thinking: true,
  waiting: true,
  idle: true,
  resting: true,
};

export function isAgentState(value: unknown): value is AgentState {
  return typeof value === "string" && KNOWN_STATES[value] === true;
}

/**
 * A state the app can draw, with `idle` for anything else.
 *
 * The server may learn a state before this build does; an unknown value must
 * leave the character resting rather than blank, because a missing face reads
 * as a rendering bug rather than as news the app cannot show yet.
 */
export function toAgentState(value: unknown): AgentState {
  return isAgentState(value) ? value : "idle";
}

/**
 * Reads one frame's payload without trusting it.
 *
 * `agent_id` is the only field a caller can act on — a state with no Bolu
 * attached cannot be placed in a thread — so a missing or empty one drops the
 * frame. Everything else falls back: an unknown `state` reads as `idle`, and an
 * absent, empty, or non-string `reason` is left out rather than rendered as
 * `undefined`.
 */
export function parseAgentState(payload: unknown): AgentStateUpdate | null {
  if (typeof payload !== "object" || payload === null) return null;
  const raw = payload as Record<string, unknown>;

  if (typeof raw.agent_id !== "string" || raw.agent_id === "") return null;

  const reason = typeof raw.reason === "string" ? raw.reason.trim() : "";
  return {
    agent_id: raw.agent_id,
    state: toAgentState(raw.state),
    ...(reason === "" ? {} : { reason }),
  };
}

/** The Indonesian label the screen shows next to one Bolu. */
const STATE_VIEW: Record<AgentState, { label: string; expression: BoluExpression }> = {
  working: { label: "bekerja", expression: "working" },
  thinking: { label: "berpikir", expression: "thinking" },
  waiting: { label: "menunggu kamu", expression: "waiting" },
  idle: { label: "santai", expression: "idle" },
  resting: { label: "istirahat", expression: "resting" },
};

/** The label and expression for a state, with `idle` for anything unknown. */
export function agentStateView(state: unknown): { label: string; expression: BoluExpression } {
  return STATE_VIEW[toAgentState(state)];
}
