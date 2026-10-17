/**
 * The activity event contract: the one stream every live view is built from.
 *
 * Ported 1:1 from internal/modules/chat/domain/event.go.
 */

/** Event types. Every live view is built from this one stream. */
export const EventMessageNew = "message.new";
export const EventMessageUpdated = "message.updated";
export const EventTaskStarted = "task.started";
export const EventTaskStep = "task.step";
export const EventTaskRunning = "task.running";
export const EventTaskWaiting = "task.waiting_approval";
export const EventTaskFinished = "task.finished";
export const EventDraftCreated = "draft.created";
export const EventDraftDecided = "draft.decided";
export const EventAgentState = "agent.state";
export const EventRoutineRun = "routine.run";
export const EventRouterDecision = "router.decision";
export const EventNotification = "notification.pending";

/**
 * How often a live stream sends a keep-alive, in milliseconds, so an idle
 * connection is not closed by a proxy between the client and the API.
 */
export const Heartbeat = 25_000;

/** Agent states, which are the same values the registry derives. */
export const StateWorking = "working";
export const StateWaiting = "waiting";
export const StateIdle = "idle";
export const StateResting = "resting";
export const StateThinking = "thinking";

/**
 * One row of the activity stream, and the message a realtime client receives.
 *
 * `id` is the activity_events sequence: it is monotonic per database, so a
 * client that reconnects with the last id it saw asks for exactly what it
 * missed. That is why the stream is durable rather than fire-and-forget
 * pub/sub: Redis delivers it live, and Postgres makes the delivery gap-free.
 */
export interface Event {
  id: number;
  workspaceId: string;
  type: string;
  actorAgentId: string;
  actorUserId: string;
  conversationId: string;
  taskId: string;
  draftId: string;
  payload: unknown;
  createdAt: Date;
}

/** The body of an `agent.state` event. */
export interface AgentStatePayload {
  agent_id: string;
  state: string;
  reason?: string;
  task_id?: string;
  draft_id?: string;
}

/**
 * The body of a message event, and the shape the message endpoints return.
 *
 * The identity fields are strings because this is a wire shape: Go cannot omit
 * a zero `uuid.UUID`, so absence has to be an empty string.
 */
export interface MessagePayload {
  message_id: string;
  conversation_id: string;
  agent_id?: string;
  user_id?: string;
  status: string;
  blocks: unknown[];
  attachments: AttachmentPayload[];
  task_id?: string;
  finish_reason?: string;
  created_at?: string;
}

/** The wire shape of one attachment. The URL is what a client follows. */
export interface AttachmentPayload {
  id: string;
  filename: string;
  content_type: string;
  byte_size: number;
  url: string;
}

/**
 * One event of a streaming reply as it is sent to a client. A token is not
 * worth a database row: chunks are delivered over the open connection, and the
 * message is written when the answer finishes (or fails).
 */
export interface StreamChunk {
  kind: string;
  text?: string;
  /** A complete content block, sent once it validates. */
  block?: unknown;
  /** The stored message, sent with the final chunk. */
  message?: MessagePayload;
  error?: string;
}

/** Stream chunk kinds. */
export const ChunkText = "text";
export const ChunkNotice = "notice";
export const ChunkBlock = "block";
export const ChunkDone = "done";
export const ChunkError = "error";

/**
 * The durable side of the stream: the row that is written before anything is
 * published, and the read a reconnecting client makes.
 */
export interface EventStore {
  appendEvent(event: Event): Promise<Event>;
  /** The events after an id, oldest first. */
  sinceEvents(workspaceId: string, afterId: number, limit: number): Promise<Event[]>;
  /** The newest events of a workspace, newest first. */
  latestEvents(workspaceId: string, limit: number): Promise<Event[]>;
}

/**
 * Delivers an event to whoever is connected right now. The fast path only: the
 * store above is what makes the stream complete.
 */
export interface EventPublisher {
  publish(workspaceId: string, event: Event): Promise<void>;
  /** The live events of one workspace; the iterable ends with the signal. */
  subscribe(workspaceId: string, signal?: AbortSignal): Promise<AsyncIterable<Event>>;
}

/**
 * What the rest of the application calls to record something that happened. It
 * writes the durable row and publishes it, in that order.
 */
export interface EventSink {
  emit(event: Event): Promise<void>;
}
