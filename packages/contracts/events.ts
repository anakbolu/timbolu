/**
 * The realtime stream's names, in one place both clients can import.
 *
 * `docs/api.md` (§ The stream) is the source of truth; this file is its
 * TypeScript transcription. The names used to live only in the frontend and in
 * the Go constants, so an event could exist on the server and be invisible to a
 * client without anything failing. `scripts/check.mjs` now fails when the docs,
 * this file, and the Go constants disagree.
 *
 * Order follows the docs, not the frontend, because a diff here should read like
 * a diff of the documentation.
 */

/** The event types the chat thread renders while a reply is still streaming. */
export const CHAT_EVENT_TYPES = [
  "message.new",
  "message.updated",
  "agent.state",
  "router.decision",
] as const;

/**
 * Every event type the stream carries.
 *
 * A client may subscribe to fewer of these — the frontend names only the ones it
 * acts on — but a name that is not here does not exist.
 */
export const REALTIME_EVENT_TYPES = [
  "message.new",
  "message.updated",
  "task.started",
  "task.step",
  "task.running",
  "task.waiting_approval",
  "task.finished",
  "draft.created",
  "draft.decided",
  "agent.state",
  "router.decision",
  "routine.run",
  "notification.pending",
] as const;

/**
 * A Bolu's derived state. No coordinate is stored anywhere: a client that draws
 * a position derives it from this value.
 */
export const AGENT_STATES = [
  "working",
  "thinking",
  "waiting",
  "idle",
  "resting",
] as const;

export type AgentState = (typeof AGENT_STATES)[number];

/** The body of an `agent.state` event. */
export interface AgentStateEvent {
  agent_id: string;
  state: AgentState;
  /** What the Bolu is doing; absent when the state speaks for itself. */
  reason?: string;
  /** The work that decided the state, when there is one. */
  task_id?: string;
  draft_id?: string;
}

/**
 * The body of a `draft.created` (and `draft.decided`) event.
 *
 * Ids are strings: `encoding/json` cannot omit an id, so absence is an empty
 * string on the wire rather than an all-zero UUID.
 */
export interface DraftCreatedEvent {
  draft_id: string;
  task_id: string;
  agent_id: string;
  action_kind: string;
  status: string;
  title: string;
  summary: string;
  revision: number;
  /** RFC 3339 timestamp, or an empty string when the draft is not scheduled. */
  not_before: string;
  /** The reason the draft is held, or an empty string when it is not blocked. */
  blocked: string;
}