package domain

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Event types. Every live view is built from this one stream: the chat thread,
// the activity feed, the dashboard, and the office.
const (
	// EventMessageNew is a message that was added to a conversation.
	EventMessageNew = "message.new"
	// EventMessageUpdated is a message whose body changed after it was written,
	// which is how a streamed reply grows.
	EventMessageUpdated = "message.updated"
	// EventTaskStarted, EventTaskStep, and EventTaskFinished follow a task.
	EventTaskStarted = "task.started"
	EventTaskStep    = "task.step"
	// EventTaskRunning is a task's heartbeat. It is what moves a Bolu in the
	// office view without the client polling.
	EventTaskRunning = "task.running"
	// EventTaskWaiting is a task parked on a human decision, which the office
	// shows at the approval desk rather than as work in progress.
	EventTaskWaiting  = "task.waiting_approval"
	EventTaskFinished = "task.finished"
	// EventDraftCreated and EventDraftDecided follow an approval.
	EventDraftCreated = "draft.created"
	EventDraftDecided = "draft.decided"
	// EventAgentState is a Bolu's derived state. The office view computes a
	// position from it; no coordinate is stored anywhere.
	EventAgentState = "agent.state"
	// EventRoutineRun is a routine that fired.
	EventRoutineRun = "routine.run"
	// EventRouterDecision records which Bolu the group router chose, and why.
	EventRouterDecision = "router.decision"
	// EventNotification is the hook the notification worker consumes (EPIC 13).
	EventNotification = "notification.pending"
)

// Heartbeat is how often a live stream sends a keep-alive, so an idle
// connection is not closed by a proxy between the client and the API.
const Heartbeat = 25 * time.Second

// Agent states, which are the same values the registry derives for its display
// status. The office view maps each to a place in the room.
const (
	StateWorking  = "working"
	StateWaiting  = "waiting"
	StateIdle     = "idle"
	StateResting  = "resting"
	StateThinking = "thinking"
)

// Event is one row of the activity stream, and the message a realtime client
// receives.
//
// ID is the activity_events sequence: it is monotonic per database, so a client
// that reconnects with the last ID it saw asks for exactly what it missed. That
// is why the stream is durable rather than fire-and-forget pub/sub: Redis
// delivers it live, and Postgres makes the delivery gap-free.
type Event struct {
	ID             int64
	WorkspaceID    uuid.UUID
	Type           string
	ActorAgentID   uuid.UUID
	ActorUserID    uuid.UUID
	ConversationID uuid.UUID
	TaskID         uuid.UUID
	DraftID        uuid.UUID
	Payload        json.RawMessage
	CreatedAt      time.Time
}

// AgentStatePayload is the body of an EventAgentState event.
//
// The ids are strings for the same reason as the message payload: a typed
// uuid.UUID cannot be omitted, so absence has to be an empty string.
type AgentStatePayload struct {
	AgentID string `json:"agent_id"`
	// State is one of the State* values.
	State string `json:"state"`
	// Reason is what the Bolu is doing, which the office shows next to it.
	Reason string `json:"reason,omitempty"`
	// TaskID and DraftID point at the work that decided the state.
	TaskID  string `json:"task_id,omitempty"`
	DraftID string `json:"draft_id,omitempty"`
}

// MessagePayload is the body of a message event, and the shape the message
// endpoints return.
//
// The identity fields are strings rather than UUIDs because this is a wire
// shape: encoding/json cannot omit a zero uuid.UUID — it is an array, which
// `omitempty` never treats as empty — so a typed field would publish
// "00000000-0000-0000-0000-000000000000" and a client would read it as an
// author. An empty string is what absence looks like on the wire.
type MessagePayload struct {
	MessageID      string `json:"message_id"`
	ConversationID string `json:"conversation_id"`
	AgentID        string `json:"agent_id,omitempty"`
	UserID         string `json:"user_id,omitempty"`
	// Status is complete, streaming, partial, or failed.
	Status string `json:"status"`
	// Blocks is the whole body, so a client renders the event without a fetch.
	// A streaming reply carries the blocks it has so far.
	Blocks []json.RawMessage `json:"blocks"`
	// Attachments is the metadata of the files on the message.
	Attachments []AttachmentPayload `json:"attachments"`
	// TaskID is the work that produced the message, when there is one.
	TaskID string `json:"task_id,omitempty"`
	// FinishReason explains a partial or failed message.
	FinishReason string `json:"finish_reason,omitempty"`
	// CreatedAt is the stored timestamp, so a client that receives only events
	// still orders the thread correctly.
	CreatedAt string `json:"created_at,omitempty"`
}

// AttachmentPayload is the wire shape of one attachment. The URL is what a
// client follows; the storage key stays on the server.
type AttachmentPayload struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	ByteSize    int64  `json:"byte_size"`
	URL         string `json:"url"`
}

// StreamChunk is one event of a streaming reply as it is sent to a client. It is
// separate from Event because a token is not worth a database row: chunks are
// delivered over the open connection, and the message is written when the answer
// finishes (or fails).
type StreamChunk struct {
	// Kind is one of the Chunk* values.
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
	// Block is a complete content block, sent once it validates.
	Block json.RawMessage `json:"block,omitempty"`
	// Message is the stored message, sent with the final chunk so the client
	// keeps the canonical body rather than reassembling one.
	Message *MessagePayload `json:"message,omitempty"`
	Err     string          `json:"error,omitempty"`
}

// Stream chunk kinds.
const (
	// ChunkText is a piece of the answer.
	ChunkText = "text"
	// ChunkNotice is a status line, such as a block being regenerated. It is
	// shown to the user so a repair is visible rather than silent.
	ChunkNotice = "notice"
	// ChunkBlock is one complete, validated content block.
	ChunkBlock = "block"
	// ChunkDone ends a successful answer.
	ChunkDone = "done"
	// ChunkError ends a failed one.
	ChunkError = "error"
)

// EventStore is the durable side of the stream: the row that is written before
// anything is published, and the read a reconnecting client makes.
type EventStore interface {
	// AppendEvent writes one event and returns it with its assigned ID.
	AppendEvent(ctx context.Context, event Event) (Event, error)
	// SinceEvents returns the events after an ID, oldest first. It is what a
	// reconnecting client asks for.
	SinceEvents(ctx context.Context, workspaceID uuid.UUID, afterID int64, limit int) ([]Event, error)
	// LatestEvents returns the newest events of a workspace, newest first.
	LatestEvents(ctx context.Context, workspaceID uuid.UUID, limit int) ([]Event, error)
}

// EventPublisher delivers an event to whoever is connected right now. It is the
// fast path only: the store above is what makes the stream complete.
type EventPublisher interface {
	Publish(ctx context.Context, workspaceID uuid.UUID, event Event) error
	// Subscribe returns the live events of one workspace. The channel closes
	// when the context ends.
	Subscribe(ctx context.Context, workspaceID uuid.UUID) (<-chan Event, error)
}

// EventSink is what the rest of the application calls to record something that
// happened. It writes the durable row and publishes it, in that order.
type EventSink interface {
	Emit(ctx context.Context, event Event) error
}
