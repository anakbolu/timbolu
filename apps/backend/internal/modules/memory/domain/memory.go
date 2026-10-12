// Package domain defines the memory contract: what a Bolu remembers, what it
// knows, and the ports the service needs to write, retrieve, and delete both.
//
// Two things are stored here and they are not the same thing:
//
//   - a **memory** is something a Bolu was told or worked out about the
//     workspace and its own role. It is short, it is written by a person or by
//     extraction, and it travels in the prompt on every call.
//   - a **knowledge item** is a piece of a document the workspace owns. It is
//     long, it is one chunk of many, and it is retrieved by similarity to the
//     task at hand.
//
// Both are filtered by workspace at every read, and both are deleted for real:
// there is no soft-delete column, because "forget this" that leaves the text in
// the database is not forgetting.
package domain

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
)

// Memory kinds, matching the check constraint on `memories.kind`.
const (
	KindFact        = "fact"
	KindPreference  = "preference"
	KindInstruction = "instruction"
)

// Kinds lists the kinds a write may use, in the order a screen offers them.
var Kinds = []string{KindFact, KindPreference, KindInstruction}

// CreatedBy values, matching the check constraint on `memories.created_by`. A
// memory a person asked for and one the extractor guessed are told apart because
// they earn different trust.
const (
	SourceUser       = "user"
	SourceExtraction = "extraction"
)

// Knowledge processing states, matching the check constraint on
// `knowledge_items.status`. `pending` is a row whose chunks are stored but not
// yet embedded; only `ready` is ever searchable.
const (
	StatusPending = "pending"
	StatusReady   = "ready"
	StatusFailed  = "failed"
)

// Audit actions this module records. They are the vocabulary of "who changed
// what a Bolu remembers".
const (
	ActionCreated      = "memory.created"
	ActionUpdated      = "memory.updated"
	ActionDeleted      = "memory.deleted"
	ActionMoved        = "memory.moved"
	ActionIngested     = "knowledge.ingested"
	ActionKnowledgeDel = "knowledge.deleted"
)

// Errors the service and the handler map onto responses.
var (
	ErrNotFound       = errors.New("memory: not found")
	ErrInvalidInput   = errors.New("memory: invalid input")
	ErrSensitive      = errors.New("memory: the content is sensitive")
	ErrNotRemembered  = errors.New("memory: the message is not a memory request")
	ErrNoEmbedding    = errors.New("memory: no provider can embed")
	ErrNoModel        = errors.New("memory: no model is configured to read the conversation")
	ErrVectorWidth    = errors.New("memory: the embedding has the wrong width")
	ErrExtractTooSoon = errors.New("memory: the conversation is not quiet yet")
)

// Scope is the tenant identity a query runs as.
type Scope struct {
	UserID      uuid.UUID
	WorkspaceID uuid.UUID
}

// IsZero reports whether the scope carries no workspace at all.
func (s Scope) IsZero() bool { return s.WorkspaceID == uuid.Nil }

// Memory is one thing a Bolu remembers.
//
// The ids are carried as strings on the wire and typed inside the domain, because
// `encoding/json` cannot omit a `uuid.UUID` — it is an array — so a typed field
// would publish `00000000-0000-0000-0000-000000000000` and a client would read it
// as an author: a main memory would look like it belonged to a Bolu whose id is
// all zeroes, and "is this superseded" would be true for every row.
type Memory struct {
	ID          uuid.UUID `json:"-"`
	WorkspaceID uuid.UUID `json:"-"`
	// AgentID is empty for the main memory, which the whole workspace shares, and
	// set for a Bolu's private memory.
	AgentID         uuid.UUID `json:"-"`
	Content         string    `json:"content"`
	Kind            string    `json:"kind"`
	SourceMessageID uuid.UUID `json:"-"`
	Pinned          bool      `json:"pinned"`
	CreatedBy       string    `json:"created_by"`
	// SupersededBy names the memory that replaced this one. A superseded memory
	// is history: it is shown, and it is never sent to a model again.
	SupersededBy uuid.UUID `json:"-"`
	// Similarity is how close the memory was to the query, when it came back
	// from a search. It is zero for a list read.
	Similarity float64   `json:"-"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// MarshalJSON renders a memory for a client, with the ids that are absent left
// out rather than sent as the zero value.
func (m Memory) MarshalJSON() ([]byte, error) {
	type wire struct {
		ID              string  `json:"id"`
		WorkspaceID     string  `json:"workspace_id"`
		AgentID         string  `json:"agent_id,omitempty"`
		Content         string  `json:"content"`
		Kind            string  `json:"kind"`
		SourceMessageID string  `json:"source_message_id,omitempty"`
		Pinned          bool    `json:"pinned"`
		CreatedBy       string  `json:"created_by"`
		SupersededBy    string  `json:"superseded_by,omitempty"`
		Similarity      float64 `json:"similarity,omitempty"`
		Layer           string  `json:"layer"`
		CreatedAt       string  `json:"created_at"`
		UpdatedAt       string  `json:"updated_at"`
	}

	return json.Marshal(wire{
		ID:              m.ID.String(),
		WorkspaceID:     m.WorkspaceID.String(),
		AgentID:         wireID(m.AgentID),
		Content:         m.Content,
		Kind:            m.Kind,
		SourceMessageID: wireID(m.SourceMessageID),
		Pinned:          m.Pinned,
		CreatedBy:       m.CreatedBy,
		SupersededBy:    wireID(m.SupersededBy),
		Similarity:      m.Similarity,
		Layer:           m.Layer(),
		CreatedAt:       m.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:       m.UpdatedAt.UTC().Format(time.RFC3339Nano),
	})
}

// Layer reports which of the two layers the memory belongs to.
func (m Memory) Layer() string {
	if m.AgentID == uuid.Nil {
		return LayerMain
	}
	return LayerPrivate
}

// Layers.
const (
	LayerMain    = "main"
	LayerPrivate = "private"
)

// NewMemory is one memory to write.
type NewMemory struct {
	AgentID         uuid.UUID
	Content         string
	Kind            string
	SourceMessageID uuid.UUID
	Pinned          bool
	CreatedBy       string
	// Embedding is the vector of Content. A write without one stores a memory
	// that can never be found by similarity, so the service refuses instead.
	Embedding []float32
}

// MemoryChange is one edit. A nil field is left alone; an explicit zero
// AgentID in MoveToMain moves the memory to the main layer.
type MemoryChange struct {
	Content  *string
	Kind     *string
	Pinned   *bool
	MoveTo   *uuid.UUID
	MoveMain bool
	// Embedding is the vector of a new Content. It is required when Content
	// changes, because the stored vector has to describe the text it sits next
	// to.
	Embedding []float32
}

// MemoryFilter narrows a list read.
type MemoryFilter struct {
	// AgentID limits the read to one layer. Main filters the workspace-wide rows.
	AgentID uuid.UUID
	Main    bool
	// Query is a substring match on the content, for the screen's search box.
	Query string
	// IncludeSuperseded keeps the history in the answer.
	IncludeSuperseded bool
	// Limit bounds the read; zero means the repository default.
	Limit int
}

// KnowledgeItem is one chunk of a document the workspace owns. Its ids travel as
// strings for the reason Memory's do.
type KnowledgeItem struct {
	ID          uuid.UUID `json:"-"`
	WorkspaceID uuid.UUID `json:"-"`
	AgentID     uuid.UUID `json:"-"`
	Source      string    `json:"source"`
	ChunkIndex  int       `json:"chunk_index"`
	Content     string    `json:"content"`
	Status      string    `json:"status"`
	Similarity  float64   `json:"-"`
	CreatedAt   time.Time `json:"created_at"`
}

// MarshalJSON renders a chunk for a client, with the ids that are absent left out.
func (k KnowledgeItem) MarshalJSON() ([]byte, error) {
	type wire struct {
		ID          string  `json:"id"`
		WorkspaceID string  `json:"workspace_id"`
		AgentID     string  `json:"agent_id,omitempty"`
		Source      string  `json:"source"`
		ChunkIndex  int     `json:"chunk_index"`
		Content     string  `json:"content"`
		Status      string  `json:"status"`
		Similarity  float64 `json:"similarity,omitempty"`
		CreatedAt   string  `json:"created_at"`
	}

	return json.Marshal(wire{
		ID:          k.ID.String(),
		WorkspaceID: k.WorkspaceID.String(),
		AgentID:     wireID(k.AgentID),
		Source:      k.Source,
		ChunkIndex:  k.ChunkIndex,
		Content:     k.Content,
		Status:      k.Status,
		Similarity:  k.Similarity,
		CreatedAt:   k.CreatedAt.UTC().Format(time.RFC3339Nano),
	})
}

// wireID renders an id that may be absent, which is the empty string rather than
// the zero uuid.
func wireID(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

// NewKnowledge is one chunk to write.
type NewKnowledge struct {
	AgentID    uuid.UUID
	Source     string
	ChunkIndex int
	Content    string
}

// KnowledgeFilter narrows a list read.
type KnowledgeFilter struct {
	AgentID    uuid.UUID
	AllAgents  bool
	Status     string
	Query      string
	Source     string
	Limit      int
	WithVector bool
}

// SearchQuery is one similarity read.
type SearchQuery struct {
	// AgentID narrows a memory search to one Bolu's private layer. Knowledge is
	// searched workspace-wide by default and narrowed when a document belongs to
	// one Bolu.
	AgentID uuid.UUID
	Main    bool
	Vector  []float32
	Limit   int
	// MinSimilarity drops weak matches. Zero keeps everything the index returns.
	MinSimilarity float64
}

// Retrieval is one record of a chunk that reached a prompt.
//
// Exactly one of MemoryID and KnowledgeID is set: a row that named both would
// not answer "which memory did this answer see".
type Retrieval struct {
	TaskID      uuid.UUID
	AgentID     uuid.UUID
	MemoryID    uuid.UUID
	KnowledgeID uuid.UUID
	Similarity  float64
}

// RecallRequest is what one round asks for.
type RecallRequest struct {
	AgentID uuid.UUID
	TaskID  uuid.UUID
	// Query is the task at hand, which is what the private memory and the
	// knowledge base are searched with.
	Query string
}

// Recall is the memory a round is given.
//
// The two halves travel separately because they are cached differently: Stable
// goes into the prefix a provider caches — the persona, the instructions, and
// the main memory — while Retrieved is the part that changes with the question.
type Recall struct {
	// Stable is the main memory of the workspace, rendered for a prompt.
	Stable string
	// Retrieved is this Bolu's private memory and the knowledge chunks the
	// search found, wrapped in a marker that says it is data.
	Retrieved string
	// Used is how many chunks were injected, which is what the quality
	// measurement counts.
	Used int
}

// RememberRequest is one message that may ask for something to be remembered.
type RememberRequest struct {
	// Text is what the user wrote.
	Text string
	// AgentID is the Bolu being addressed, which is the layer a private memory
	// belongs to.
	AgentID uuid.UUID
	// MessageID is the message it came from, so the memory can point back at it.
	MessageID uuid.UUID
}

// RememberResult is what the memory module answered.
type RememberResult struct {
	// Handled says the message asked to remember something and was answered
	// here. A false leaves the message to the ordinary reply path.
	Handled bool
	// Stored is the memory that was written, when one was.
	Stored *Memory
	// Refused says the content was sensitive: nothing was stored and the
	// confirmation says so.
	Refused bool
	// Confirmation is what the Bolu answers. It names exactly what was stored,
	// which is what makes the answer checkable.
	Confirmation string
}

// IngestRequest is a document to chunk, embed, and store.
type IngestRequest struct {
	AgentID uuid.UUID
	Source  string
	Text    string
}

// ExtractResult is what one extraction pass did.
type ExtractResult struct {
	// Disabled says the Bolu does not want automatic extraction.
	Disabled bool
	// Stored, Merged, Skipped, and Refused account for every candidate the model
	// proposed, so "why did nothing happen" has an answer.
	Stored  int
	Merged  int
	Skipped int
	Refused int
}

// Extracted is one memory the model proposed.
type Extracted struct {
	Content string `json:"content"`
	Kind    string `json:"kind"`
	// Layer is "main" or "private": general workspace facts go to the main
	// memory, role-specific ones to the Bolu's own.
	Layer string `json:"layer"`
}

// Utterance is one message of a conversation, as the extractor reads it.
type Utterance struct {
	MessageID uuid.UUID
	AgentID   uuid.UUID
	AgentName string
	Text      string
	CreatedAt time.Time
}

// FromAgent reports whether a Bolu wrote the message.
func (u Utterance) FromAgent() bool { return u.AgentID != uuid.Nil }

// AgentInfo is the per-Bolu part the memory screen shows: the name a layer is
// labelled with and whether extraction is on.
type AgentInfo struct {
	ID                uuid.UUID
	Name              string
	ExtractionEnabled bool
}

// Repository persists memories, knowledge, and the record of what was retrieved.
//
// Every method takes the scope rather than a workspace id, so the same
// transaction-local tenant setting that Row Level Security reads is applied,
// and a query that forgot its filter still cannot cross a workspace.
type Repository interface {
	// --- memory

	CreateMemory(ctx context.Context, scope Scope, memory NewMemory) (Memory, error)
	Memory(ctx context.Context, scope Scope, id uuid.UUID) (Memory, error)
	ListMemories(ctx context.Context, scope Scope, filter MemoryFilter) ([]Memory, error)
	UpdateMemory(ctx context.Context, scope Scope, id uuid.UUID, change MemoryChange) (Memory, error)
	// DeleteMemory removes the row and its vector. There is no soft delete: a
	// memory that is not deleted is a memory a model can still be given.
	DeleteMemory(ctx context.Context, scope Scope, id uuid.UUID) error
	// Supersede marks an older memory as replaced by a newer one, which is what
	// keeps a corrected fact from being sent alongside its own correction.
	Supersede(ctx context.Context, scope Scope, older, newer uuid.UUID) error
	SearchMemories(ctx context.Context, scope Scope, query SearchQuery) ([]Memory, error)

	// --- knowledge

	CreateKnowledge(ctx context.Context, scope Scope, items []NewKnowledge) ([]KnowledgeItem, error)
	// EmbedKnowledge stores the vectors of the chunks that were written and
	// settles their status, in one statement per chunk set.
	EmbedKnowledge(ctx context.Context, scope Scope, ids []uuid.UUID, vectors [][]float32) error
	// FailKnowledge settles chunks that could not be embedded, so an upload is
	// monitorable instead of left pending forever.
	FailKnowledge(ctx context.Context, scope Scope, ids []uuid.UUID) error
	Knowledge(ctx context.Context, scope Scope, id uuid.UUID) (KnowledgeItem, error)
	ListKnowledge(ctx context.Context, scope Scope, filter KnowledgeFilter) ([]KnowledgeItem, error)
	DeleteKnowledge(ctx context.Context, scope Scope, id uuid.UUID) error
	SearchKnowledge(ctx context.Context, scope Scope, query SearchQuery) ([]KnowledgeItem, error)

	// --- what was used

	RecordRetrieval(ctx context.Context, scope Scope, usage Retrieval) error

	// --- other modules' reads

	// Agents returns the Bolu of the workspace with the two facts this module
	// needs: the name a memory layer is labelled with, and the extraction switch.
	// The rows belong to the agent module; this is a read of two columns, and the
	// write of the switch stays there.
	Agents(ctx context.Context, scope Scope) ([]AgentInfo, error)
	// ExtractionEnabled reports one Bolu's switch.
	ExtractionEnabled(ctx context.Context, scope Scope, agentID uuid.UUID) (bool, error)
}

// Embedder is the model gateway's embedding port, aliased so the pipeline and
// the gateway cannot drift apart.
type Embedder = llmdomain.Embedder

// Completer is the model call the extractor makes. It is one method rather than
// the whole gateway because an extraction is a single small question.
type Completer interface {
	Chat(ctx context.Context, scope llmdomain.Scope, req llmdomain.ChatRequest) (llmdomain.ChatResponse, error)
}

// Conversation reads the thread an extraction runs over.
type Conversation interface {
	// Recent returns the last messages, oldest first, with the text a model
	// reads.
	Recent(ctx context.Context, scope Scope, conversationID uuid.UUID, limit int) ([]Utterance, error)
}

// Auditor records what a person did, for the trail the product answers
// "who changed this" with. The approval module owns the table.
type Auditor interface {
	Record(ctx context.Context, scope Scope, entry AuditEntry) error
}

// AuditEntry is one thing that happened, for the record.
type AuditEntry struct {
	Action     string
	ObjectType string
	ObjectID   string
	// ActorUserID is who did it, when a person did. ActorAgentID is the Bolu,
	// when one did.
	ActorUserID  uuid.UUID
	ActorAgentID uuid.UUID
	// Metadata is the detail: the layer, the kind, the old and new content, and
	// never a secret. The sensitive filter runs before a write, so a stored
	// memory's content is what a person may read back.
	Metadata map[string]any
}

// Scheduler asks the durable runtime to extract later.
//
// It is a port rather than a direct call because the debounce is a Temporal
// schedule: the memory module declares what it wants and the container, which is
// the only package that knows both sides, supplies the engine.
type Scheduler interface {
	// ConversationActive tells the extractor that a conversation moved, which
	// resets the quiet period it waits for.
	ConversationActive(ctx context.Context, scope Scope, conversationID, agentID uuid.UUID) error
}
