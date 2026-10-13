// Package service implements the memory use cases: what a Bolu remembers, what
// it knows, what one round is given, and what a conversation leaves behind.
//
// Three rules run through the package:
//
//   - nothing is stored without a vector. A memory or a chunk the search can
//     never find is worse than a refused write, because it looks like the Bolu
//     knows something and then never uses it.
//   - a write is filtered before it is stored. Card numbers, passwords, and one
//     time codes are refused rather than remembered.
//   - reading degrades, writing does not. A task must run when the embedding
//     provider is down; a memory must not be written when it cannot be.
package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/memory/domain"
)

// Deps are the service dependencies.
//
// The embedder and the model are optional in the sense that the module still
// serves reads without them: the memory list, the knowledge list, and the delete
// endpoints keep working, while a write that needs a vector answers with the
// reason instead of pretending it stored something.
type Deps struct {
	Repository domain.Repository
	Embedder   domain.Embedder
	Model      domain.Completer
	Threads    domain.Conversation
	Auditor    domain.Auditor
	Scheduler  domain.Scheduler
	Limits     domain.Limits
	Logger     *zap.Logger
}

// Service is the memory implementation.
type Service struct {
	deps   Deps
	limits domain.Limits
}

// New builds the service.
func New(deps Deps) *Service {
	return &Service{deps: deps, limits: deps.Limits.Normalise()}
}

// Active resets a conversation's quiet period.
//
// A deployment without the durable runtime has no scheduler, and then a
// conversation is simply never extracted: the switch exists, the writes do not.
func (s *Service) Active(ctx context.Context, scope domain.Scope, conversationID, agentID uuid.UUID) error {
	if s.deps.Scheduler == nil {
		return nil
	}
	if conversationID == uuid.Nil {
		return fmt.Errorf("%w: conversation id is required", domain.ErrInvalidInput)
	}

	return s.deps.Scheduler.ConversationActive(ctx, scope, conversationID, agentID)
}

// --------------------------------------------------------------- embedding

// embed turns text into vectors and refuses an answer that does not fit the
// schema.
//
// The width is checked before the vector is stored rather than after: a model
// that answers with 1024 numbers would otherwise fail on the insert, inside a
// transaction, with an error nobody can act on.
func (s *Service) embed(
	ctx context.Context,
	scope domain.Scope,
	agentID, taskID uuid.UUID,
	inputs []string,
) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	if s.deps.Embedder == nil {
		return nil, fmt.Errorf("%w: no embedding provider is configured", domain.ErrNoEmbedding)
	}

	response, err := s.deps.Embedder.Embed(ctx, llmdomain.Scope{
		WorkspaceID: scope.WorkspaceID,
		AgentID:     agentID,
	}, llmdomain.EmbedRequest{
		Inputs: inputs,
		Metadata: llmdomain.RequestMetadata{
			WorkspaceID: scope.WorkspaceID,
			AgentID:     agentID,
			TaskID:      taskID,
			Purpose:     llmdomain.PurposeEmbedding,
		},
	})
	if err != nil {
		return nil, err
	}
	if len(response.Vectors) != len(inputs) {
		return nil, fmt.Errorf("%w: %d vectors for %d inputs",
			domain.ErrVectorWidth, len(response.Vectors), len(inputs))
	}

	for i, vector := range response.Vectors {
		if len(vector) != llmdomain.EmbeddingDimensions {
			return nil, fmt.Errorf("%w: input %d has %d numbers, the schema stores %d",
				domain.ErrVectorWidth, i, len(vector), llmdomain.EmbeddingDimensions)
		}
	}

	return response.Vectors, nil
}

// ---------------------------------------------------------------- auditing

// audit records one change. A missing auditor is not an error: the trail is
// worth having, and losing a write because the trail is unavailable is worse.
func (s *Service) audit(ctx context.Context, scope domain.Scope, entry domain.AuditEntry) {
	if s.deps.Auditor == nil {
		return
	}

	entry.ActorUserID = scope.UserID
	if err := s.deps.Auditor.Record(ctx, scope, entry); err != nil && s.deps.Logger != nil {
		s.deps.Logger.Warn("could not record a memory change in the audit trail",
			zap.String("action", entry.Action),
			zap.String("object_id", entry.ObjectID),
			zap.Error(err),
		)
	}
}

// ------------------------------------------------------------------ reading

// Memories returns the memories of the workspace.
func (s *Service) Memories(ctx context.Context, scope domain.Scope, filter domain.MemoryFilter) ([]domain.Memory, error) {
	if scope.IsZero() {
		return nil, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	return s.deps.Repository.ListMemories(ctx, scope, filter)
}

// Knowledge returns the chunks of the workspace, including the ones whose
// embedding failed.
func (s *Service) Knowledge(ctx context.Context, scope domain.Scope, filter domain.KnowledgeFilter) ([]domain.KnowledgeItem, error) {
	if scope.IsZero() {
		return nil, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	return s.deps.Repository.ListKnowledge(ctx, scope, filter)
}

// ------------------------------------------------------------------ writing

// Add stores one memory a person wrote.
func (s *Service) Add(ctx context.Context, scope domain.Scope, request domain.AddRequest) (domain.Memory, error) {
	if scope.IsZero() {
		return domain.Memory{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	content, err := s.accept(request.Content)
	if err != nil {
		return domain.Memory{}, err
	}

	kind := request.Kind
	if kind == "" {
		kind = domain.KindFact
	}
	if !knownKind(kind) {
		return domain.Memory{}, fmt.Errorf("%w: unknown kind %q", domain.ErrInvalidInput, kind)
	}

	vectors, err := s.embed(ctx, scope, request.AgentID, uuid.Nil, []string{content})
	if err != nil {
		return domain.Memory{}, err
	}

	memory, err := s.deps.Repository.CreateMemory(ctx, scope, domain.NewMemory{
		AgentID:   request.AgentID,
		Content:   content,
		Kind:      kind,
		Pinned:    request.Pinned,
		CreatedBy: domain.SourceUser,
		Embedding: vectors[0],
	})
	if err != nil {
		return domain.Memory{}, err
	}

	// The actor is the person who asked, taken from the scope rather than from the request body: a
	// line saying what changed but not who changed it cannot answer the question the trail exists
	// for, and the caller's identity is what the scope already carries.
	s.audit(ctx, scope, domain.AuditEntry{
		Action:      domain.ActionCreated,
		ObjectType:  "memory",
		ObjectID:    memory.ID.String(),
		ActorUserID: scope.UserID,
		Metadata: map[string]any{
			"layer":  memory.Layer(),
			"kind":   memory.Kind,
			"pinned": memory.Pinned,
		},
	})

	return memory, nil
}

// Edit changes one memory.
func (s *Service) Edit(ctx context.Context, scope domain.Scope, request domain.EditRequest) (domain.Memory, error) {
	if scope.IsZero() {
		return domain.Memory{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	stored, err := s.deps.Repository.Memory(ctx, scope, request.ID)
	if err != nil {
		return domain.Memory{}, err
	}

	change := domain.MemoryChange{
		Kind:   request.Kind,
		Pinned: request.Pinned,
	}

	if request.Kind != nil && !knownKind(*request.Kind) {
		return domain.Memory{}, fmt.Errorf("%w: unknown kind %q", domain.ErrInvalidInput, *request.Kind)
	}

	if request.Content != nil {
		content, err := s.accept(*request.Content)
		if err != nil {
			return domain.Memory{}, err
		}

		// A changed text needs a new vector: the stored one describes the words
		// that used to be there, and leaving it would make the memory findable
		// by something it no longer says.
		if content != stored.Content {
			vectors, err := s.embed(ctx, scope, stored.AgentID, uuid.Nil, []string{content})
			if err != nil {
				return domain.Memory{}, err
			}
			change.Content = &content
			change.Embedding = vectors[0]
		}
	}

	moved := false
	if request.MoveToMain {
		change.MoveMain = true
		moved = stored.AgentID != uuid.Nil
	} else if request.AgentID != nil && *request.AgentID != stored.AgentID {
		change.MoveTo = request.AgentID
		moved = true
	}

	updated, err := s.deps.Repository.UpdateMemory(ctx, scope, request.ID, change)
	if err != nil {
		return domain.Memory{}, err
	}

	action := domain.ActionUpdated
	if moved {
		action = domain.ActionMoved
	}
	s.audit(ctx, scope, domain.AuditEntry{
		Action:      action,
		ObjectType:  "memory",
		ObjectID:    updated.ID.String(),
		ActorUserID: scope.UserID,
		Metadata: map[string]any{
			"layer":     updated.Layer(),
			"from":      stored.Layer(),
			"kind":      updated.Kind,
			"pinned":    updated.Pinned,
			"rewritten": change.Content != nil,
		},
	})

	return updated, nil
}

// Forget deletes one memory and its vector, permanently.
func (s *Service) Forget(ctx context.Context, scope domain.Scope, id uuid.UUID) error {
	if scope.IsZero() {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	stored, err := s.deps.Repository.Memory(ctx, scope, id)
	if err != nil {
		return err
	}

	if err := s.deps.Repository.DeleteMemory(ctx, scope, id); err != nil {
		return err
	}

	// The trail says what was deleted and never copies it: a memory is deleted
	// because it should not be there, and writing it into another table would
	// keep it.
	s.audit(ctx, scope, domain.AuditEntry{
		Action:      domain.ActionDeleted,
		ObjectType:  "memory",
		ObjectID:    id.String(),
		ActorUserID: scope.UserID,
		Metadata:    map[string]any{"layer": stored.Layer(), "kind": stored.Kind},
	})

	return nil
}

// ForgetKnowledge deletes one chunk and its vector.
func (s *Service) ForgetKnowledge(ctx context.Context, scope domain.Scope, id uuid.UUID) error {
	if scope.IsZero() {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	stored, err := s.deps.Repository.Knowledge(ctx, scope, id)
	if err != nil {
		return err
	}

	if err := s.deps.Repository.DeleteKnowledge(ctx, scope, id); err != nil {
		return err
	}

	s.audit(ctx, scope, domain.AuditEntry{
		Action:      domain.ActionKnowledgeDel,
		ObjectType:  "knowledge_item",
		ObjectID:    id.String(),
		ActorUserID: scope.UserID,
		Metadata:    map[string]any{"source": stored.Source, "chunk_index": stored.ChunkIndex},
	})

	return nil
}

// Ingest chunks one document, embeds it, and stores the chunks.
//
// The chunks are written before they are embedded so an upload is visible while
// it runs, and they are settled to `ready` or `failed` afterwards. A failed
// chunk keeps its row: it is not searchable — every search requires `ready` —
// and it is the record that says the upload did not work.
func (s *Service) Ingest(ctx context.Context, scope domain.Scope, request domain.IngestRequest) ([]domain.KnowledgeItem, error) {
	if scope.IsZero() {
		return nil, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	text := strings.TrimSpace(request.Text)
	if text == "" {
		return nil, fmt.Errorf("%w: the document is empty", domain.ErrInvalidInput)
	}
	if len([]rune(text)) > maxDocumentRunes {
		return nil, fmt.Errorf("%w: the document is longer than %d characters", domain.ErrInvalidInput, maxDocumentRunes)
	}

	chunks := chunkText(text, s.limits.ChunkRunes, s.limits.ChunkOverlapRunes)
	if len(chunks) == 0 {
		return nil, fmt.Errorf("%w: the document has nothing to store", domain.ErrInvalidInput)
	}
	if len(chunks) > maxChunks {
		return nil, fmt.Errorf("%w: the document is %d chunks, the limit is %d",
			domain.ErrInvalidInput, len(chunks), maxChunks)
	}

	items := make([]domain.NewKnowledge, 0, len(chunks))
	for i, chunk := range chunks {
		items = append(items, domain.NewKnowledge{
			AgentID:    request.AgentID,
			Source:     strings.TrimSpace(request.Source),
			ChunkIndex: i,
			Content:    chunk,
		})
	}

	stored, err := s.deps.Repository.CreateKnowledge(ctx, scope, items)
	if err != nil {
		return nil, err
	}

	ids := make([]uuid.UUID, 0, len(stored))
	for _, item := range stored {
		ids = append(ids, item.ID)
	}

	vectors, embedErr := s.embed(ctx, scope, request.AgentID, uuid.Nil, chunks)
	if embedErr != nil {
		if failErr := s.deps.Repository.FailKnowledge(ctx, scope, ids); failErr != nil && s.deps.Logger != nil {
			s.deps.Logger.Error("could not settle the chunks of a failed upload",
				zap.String("workspace_id", scope.WorkspaceID.String()),
				zap.Error(failErr),
			)
		}
		return nil, embedErr
	}

	if err := s.deps.Repository.EmbedKnowledge(ctx, scope, ids, vectors); err != nil {
		return nil, err
	}

	for i := range stored {
		stored[i].Status = domain.StatusReady
	}

	s.audit(ctx, scope, domain.AuditEntry{
		Action:      domain.ActionIngested,
		ObjectType:  "knowledge_item",
		ObjectID:    stored[0].ID.String(),
		ActorUserID: scope.UserID,
		Metadata: map[string]any{
			"source": stored[0].Source,
			"chunks": len(stored),
			"layer":  stored[0].AgentID.String(),
		},
	})

	return stored, nil
}

// ----------------------------------------------------------------- helpers

// accept validates and filters one piece of text before it is stored.
func (s *Service) accept(content string) (string, error) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return "", fmt.Errorf("%w: the content is empty", domain.ErrInvalidInput)
	}
	if len([]rune(trimmed)) > s.limits.MaxContentRunes {
		return "", fmt.Errorf("%w: the content is longer than %d characters",
			domain.ErrInvalidInput, s.limits.MaxContentRunes)
	}
	if reason, sensitive := Sensitive(trimmed); sensitive {
		// The message is the sentence a person reads, because the handler returns
		// it verbatim: an error string that leaks the package name and the rule's
		// own wording ("memory: the content is sensitive") reads as a system fault
		// rather than as an answer about what was typed.
		return "", fmt.Errorf("%w: isinya mengandung %s, jadi tidak disimpan", domain.ErrSensitive, reason)
	}

	return trimmed, nil
}

func knownKind(kind string) bool {
	for _, candidate := range domain.Kinds {
		if candidate == kind {
			return true
		}
	}
	return false
}
