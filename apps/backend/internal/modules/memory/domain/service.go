package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Service is what the HTTP handlers and the other modules call.
type Service interface {
	// --- memory a person manages

	// Memories returns the memories of the workspace, newest first, optionally
	// narrowed to one layer and filtered by a search term.
	Memories(ctx context.Context, scope Scope, filter MemoryFilter) ([]Memory, error)
	// Add stores one memory a person wrote.
	Add(ctx context.Context, scope Scope, request AddRequest) (Memory, error)
	// Edit changes one memory. Changing the content re-embeds it, because the
	// stored vector has to describe the text it sits next to.
	Edit(ctx context.Context, scope Scope, request EditRequest) (Memory, error)
	// Forget deletes one memory and its vector, permanently.
	Forget(ctx context.Context, scope Scope, id uuid.UUID) error

	// --- knowledge

	Knowledge(ctx context.Context, scope Scope, filter KnowledgeFilter) ([]KnowledgeItem, error)
	// Ingest chunks one document, embeds it, and stores the chunks.
	Ingest(ctx context.Context, scope Scope, request IngestRequest) ([]KnowledgeItem, error)
	// ForgetKnowledge deletes one chunk and its vector.
	ForgetKnowledge(ctx context.Context, scope Scope, id uuid.UUID) error

	// --- what a round is given

	// Recall assembles the memory one round answers with, and records which
	// chunks it used.
	Recall(ctx context.Context, scope Scope, request RecallRequest) (Recall, error)

	// --- what a message asks for

	// Remember stores a direct request — "ingat ya, ..." — and answers with a
	// confirmation that names what was stored.
	Remember(ctx context.Context, scope Scope, request RememberRequest) (RememberResult, error)

	// --- extraction

	// Active resets the quiet period of a conversation, so extraction runs when
	// the thread stops moving rather than on every message.
	Active(ctx context.Context, scope Scope, conversationID, agentID uuid.UUID) error
	// Extract reads a quiet conversation and writes what is worth keeping. It is
	// the activity the extraction workflow calls.
	Extract(ctx context.Context, scope Scope, request ExtractRequest) (ExtractResult, error)
}

// ExtractRequest is one extraction pass.
type ExtractRequest struct {
	ConversationID uuid.UUID
	// AgentID is the Bolu whose switch decides whether extraction runs at all,
	// and whose private layer the role-specific memories belong to.
	AgentID uuid.UUID
}

// AddRequest is one memory a person writes, from the memory screen.
type AddRequest struct {
	Content string
	Kind    string
	// AgentID empty means the main memory.
	AgentID uuid.UUID
	Pinned  bool
}

// EditRequest is one edit from the memory screen.
type EditRequest struct {
	ID      uuid.UUID
	Content *string
	Kind    *string
	Pinned  *bool
	// AgentID moves the memory to another layer. MoveToMain moves it back to the
	// workspace-wide one, which is a value an empty agent id cannot express: an
	// absent field has to mean "leave the layer alone".
	AgentID    *uuid.UUID
	MoveToMain bool
}

// Limits bound what memory costs: how much of it travels in every prompt, and
// how long a conversation must be still before it is read.
type Limits struct {
	// MainTokens caps the main memory that is sent on every call. The cap is in
	// tokens rather than rows because that is what the bill is in.
	MainTokens int
	// RecallMemories and RecallKnowledge bound what one round retrieves.
	RecallMemories  int
	RecallKnowledge int
	// Similarity is how close a candidate has to be to be considered the same
	// fact. Above it, a newer memory replaces the older one instead of joining
	// it.
	Similarity float64
	// QuietPeriod is how long a conversation must not move before extraction
	// reads it.
	QuietPeriod time.Duration
	// ExtractionMessages bounds how much of the thread the extractor reads.
	ExtractionMessages int
	// ChunkRunes is the size of one knowledge chunk, and ChunkOverlapRunes is how
	// much of the end of one chunk repeats at the start of the next so a sentence
	// cut in half is still findable from both sides. Zero means the default rather
	// than no overlap: pieces that meet exactly are the thing the overlap exists
	// to avoid, so "off" is not a setting this product offers.
	ChunkRunes        int
	ChunkOverlapRunes int
	// MaxContentRunes bounds one memory and one chunk, so a model that answers
	// with an essay cannot fill the prompt on every later turn.
	MaxContentRunes int
}

// DefaultLimits are the product's judgement, so they live in one place rather
// than being repeated per call site.
var DefaultLimits = Limits{
	MainTokens:         1500,
	RecallMemories:     6,
	RecallKnowledge:    4,
	Similarity:         0.90,
	QuietPeriod:        5 * time.Minute,
	ExtractionMessages: 30,
	ChunkRunes:         1000,
	ChunkOverlapRunes:  150,
	MaxContentRunes:    2000,
}

// Normalise fills the zero fields from the defaults, so a partially configured
// deployment still has bounds.
func (l Limits) Normalise() Limits {
	if l.MainTokens <= 0 {
		l.MainTokens = DefaultLimits.MainTokens
	}
	if l.RecallMemories <= 0 {
		l.RecallMemories = DefaultLimits.RecallMemories
	}
	if l.RecallKnowledge <= 0 {
		l.RecallKnowledge = DefaultLimits.RecallKnowledge
	}
	if l.Similarity <= 0 {
		l.Similarity = DefaultLimits.Similarity
	}
	if l.QuietPeriod <= 0 {
		l.QuietPeriod = DefaultLimits.QuietPeriod
	}
	if l.ExtractionMessages <= 0 {
		l.ExtractionMessages = DefaultLimits.ExtractionMessages
	}
	if l.ChunkRunes <= 0 {
		l.ChunkRunes = DefaultLimits.ChunkRunes
	}
	if l.ChunkOverlapRunes <= 0 {
		l.ChunkOverlapRunes = DefaultLimits.ChunkOverlapRunes
	}
	if l.MaxContentRunes <= 0 {
		l.MaxContentRunes = DefaultLimits.MaxContentRunes
	}
	return l
}
