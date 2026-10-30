// Package repository implements the memory data access with pgx and the
// sqlc-generated queries.
//
// Every statement runs inside the caller's tenant scope, so Row Level Security
// filters it even when a query forgets its workspace filter, and the similarity
// reads filter by layer as well: a private memory that leaked into another
// Bolu's prompt would be as wrong as one that leaked into another workspace.
//
// Vectors are sent as text and never read back. Postgres computes the
// similarity, and the stored embedding is not a column any caller selects: 1536
// floats per row are not worth shipping to a handler that will not read them.
package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/memory/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/memory/repository/sqlcgen"
	"github.com/anakbolu/timbolu/packages/platform/database"
)

// Repository is the Postgres-backed implementation of the memory ports.
type Repository struct {
	pool *database.Pool
}

// New builds the repository.
func New(pool *database.Pool) *Repository {
	return &Repository{pool: pool}
}

// ---------------------------------------------------------------- memory

// CreateMemory stores one memory.
func (r *Repository) CreateMemory(ctx context.Context, scope domain.Scope, memory domain.NewMemory) (domain.Memory, error) {
	var stored domain.Memory

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.CreateMemory(ctx, sqlcgen.CreateMemoryParams{
			WorkspaceID:     scope.WorkspaceID,
			AgentID:         optionalID(memory.AgentID),
			Content:         memory.Content,
			Kind:            memory.Kind,
			SourceMessageID: optionalID(memory.SourceMessageID),
			Embedding:       vectorLiteral(memory.Embedding),
			Pinned:          memory.Pinned,
			CreatedBy:       memory.CreatedBy,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// The insert is a SELECT, so a Bolu outside this workspace comes
				// back as no rows rather than as a constraint violation.
				return fmt.Errorf("%w: the bolu is not in this workspace", domain.ErrInvalidInput)
			}
			return translate(err, "create memory")
		}

		stored = fromCreateRow(row)
		return nil
	})
	if err != nil {
		return domain.Memory{}, err
	}

	return stored, nil
}

// Memory returns one memory.
func (r *Repository) Memory(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Memory, error) {
	var memory domain.Memory

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetMemory(ctx, sqlcgen.GetMemoryParams{ID: id, WorkspaceID: scope.WorkspaceID})
		if err != nil {
			return translate(err, "get memory")
		}

		memory = fromGetRow(row)
		return nil
	})
	if err != nil {
		return domain.Memory{}, err
	}

	return memory, nil
}

// ListMemories returns the memories the screen shows.
func (r *Repository) ListMemories(ctx context.Context, scope domain.Scope, filter domain.MemoryFilter) ([]domain.Memory, error) {
	var memories []domain.Memory

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListMemories(ctx, sqlcgen.ListMemoriesParams{
			WorkspaceID:       scope.WorkspaceID,
			AgentID:           optionalID(filter.AgentID),
			MainOnly:          filter.Main,
			IncludeSuperseded: filter.IncludeSuperseded,
			Query:             optionalString(filter.Query),
			Limit:             limitOf(filter.Limit),
		})
		if err != nil {
			return translate(err, "list memories")
		}

		memories = make([]domain.Memory, 0, len(rows))
		for _, row := range rows {
			memories = append(memories, fromListRow(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return memories, nil
}

// UpdateMemory changes one memory.
func (r *Repository) UpdateMemory(ctx context.Context, scope domain.Scope, id uuid.UUID, change domain.MemoryChange) (domain.Memory, error) {
	var updated domain.Memory

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		params := sqlcgen.UpdateMemoryParams{
			ID:          id,
			WorkspaceID: scope.WorkspaceID,
			Content:     change.Content,
			Kind:        change.Kind,
			Pinned:      change.Pinned,
			MoveToMain:  change.MoveMain,
			AgentID:     change.MoveTo,
		}
		if len(change.Embedding) > 0 {
			// The vector is re-written only when the text changed: an edit that
			// only pins a memory must not spend an embedding call.
			literal := vectorLiteral(change.Embedding)
			params.Embedding = &literal
		}

		row, err := queries.UpdateMemory(ctx, params)
		if err != nil {
			return translate(err, "update memory")
		}

		updated = fromUpdateRow(row)
		return nil
	})
	if err != nil {
		return domain.Memory{}, err
	}

	return updated, nil
}

// DeleteMemory removes a memory and its vector.
func (r *Repository) DeleteMemory(ctx context.Context, scope domain.Scope, id uuid.UUID) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.DeleteMemory(ctx, sqlcgen.DeleteMemoryParams{ID: id, WorkspaceID: scope.WorkspaceID})
		if err != nil {
			return translate(err, "delete memory")
		}
		if rows == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
}

// Supersede marks an older memory as replaced.
func (r *Repository) Supersede(ctx context.Context, scope domain.Scope, older, newer uuid.UUID) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		return translate(queries.SupersedeMemory(ctx, sqlcgen.SupersedeMemoryParams{
			Older:       older,
			Newer:       optionalID(newer),
			WorkspaceID: scope.WorkspaceID,
		}), "supersede memory")
	})
}

// SearchMemories returns the memories of one layer nearest to a vector.
func (r *Repository) SearchMemories(ctx context.Context, scope domain.Scope, query domain.SearchQuery) ([]domain.Memory, error) {
	var memories []domain.Memory

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.SearchMemories(ctx, sqlcgen.SearchMemoriesParams{
			WorkspaceID:   scope.WorkspaceID,
			AgentID:       optionalID(query.AgentID),
			Vector:        vectorLiteral(query.Vector),
			MinSimilarity: query.MinSimilarity,
			Limit:         limitOf(query.Limit),
		})
		if err != nil {
			return translate(err, "search memories")
		}

		memories = make([]domain.Memory, 0, len(rows))
		for _, row := range rows {
			memories = append(memories, fromSearchRow(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return memories, nil
}

// ------------------------------------------------------------- knowledge

// CreateKnowledge stores the chunks of one document as pending rows.
func (r *Repository) CreateKnowledge(ctx context.Context, scope domain.Scope, items []domain.NewKnowledge) ([]domain.KnowledgeItem, error) {
	if len(items) == 0 {
		return nil, nil
	}

	agentID := items[0].AgentID
	chunks := make([]string, 0, len(items))
	for _, item := range items {
		if item.AgentID != agentID {
			return nil, fmt.Errorf("%w: one upload belongs to one layer", domain.ErrInvalidInput)
		}
		chunks = append(chunks, item.Content)
	}

	var stored []domain.KnowledgeItem

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.CreateKnowledge(ctx, sqlcgen.CreateKnowledgeParams{
			WorkspaceID: scope.WorkspaceID,
			AgentID:     optionalID(agentID),
			Source:      items[0].Source,
			Chunks:      chunks,
		})
		if err != nil {
			return translate(err, "create knowledge")
		}
		if len(rows) != len(chunks) {
			// The guard clause refuses a Bolu from another workspace, and the
			// answer is then "no rows". Saying so is better than storing half a
			// document.
			return fmt.Errorf("%w: the bolu is not in this workspace", domain.ErrInvalidInput)
		}

		stored = make([]domain.KnowledgeItem, 0, len(rows))
		for _, row := range rows {
			stored = append(stored, fromCreateKnowledgeRow(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return stored, nil
}

// EmbedKnowledge stores the vectors of the chunks that were written, in one
// transaction: a document is either embedded or not, never half.
func (r *Repository) EmbedKnowledge(ctx context.Context, scope domain.Scope, ids []uuid.UUID, vectors [][]float32) error {
	if len(ids) != len(vectors) {
		return fmt.Errorf("%w: %d chunks and %d vectors", domain.ErrInvalidInput, len(ids), len(vectors))
	}

	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		for i, id := range ids {
			err := queries.SetKnowledgeEmbedding(ctx, sqlcgen.SetKnowledgeEmbeddingParams{
				ID:          id,
				WorkspaceID: scope.WorkspaceID,
				Embedding:   vectorLiteral(vectors[i]),
			})
			if err != nil {
				return translate(err, "embed knowledge")
			}
		}
		return nil
	})
}

// FailKnowledge settles chunks that could not be embedded.
func (r *Repository) FailKnowledge(ctx context.Context, scope domain.Scope, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}

	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		return translate(queries.FailKnowledge(ctx, sqlcgen.FailKnowledgeParams{
			WorkspaceID: scope.WorkspaceID,
			Ids:         ids,
		}), "fail knowledge")
	})
}

// Knowledge returns one chunk.
func (r *Repository) Knowledge(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.KnowledgeItem, error) {
	var item domain.KnowledgeItem

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetKnowledge(ctx, sqlcgen.GetKnowledgeParams{ID: id, WorkspaceID: scope.WorkspaceID})
		if err != nil {
			return translate(err, "get knowledge")
		}

		item = fromGetKnowledgeRow(row)
		return nil
	})
	if err != nil {
		return domain.KnowledgeItem{}, err
	}

	return item, nil
}

// ListKnowledge returns the chunks the screen shows, including the ones whose
// embedding failed: an upload that did not work is something a person has to be
// able to see.
func (r *Repository) ListKnowledge(ctx context.Context, scope domain.Scope, filter domain.KnowledgeFilter) ([]domain.KnowledgeItem, error) {
	var items []domain.KnowledgeItem

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListKnowledge(ctx, sqlcgen.ListKnowledgeParams{
			WorkspaceID: scope.WorkspaceID,
			AgentID:     optionalID(filter.AgentID),
			Status:      optionalString(filter.Status),
			Source:      optionalString(filter.Source),
			Query:       optionalString(filter.Query),
			Limit:       limitOf(filter.Limit),
		})
		if err != nil {
			return translate(err, "list knowledge")
		}

		items = make([]domain.KnowledgeItem, 0, len(rows))
		for _, row := range rows {
			items = append(items, fromListKnowledgeRow(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return items, nil
}

// DeleteKnowledge removes one chunk and its vector.
func (r *Repository) DeleteKnowledge(ctx context.Context, scope domain.Scope, id uuid.UUID) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.DeleteKnowledge(ctx, sqlcgen.DeleteKnowledgeParams{ID: id, WorkspaceID: scope.WorkspaceID})
		if err != nil {
			return translate(err, "delete knowledge")
		}
		if rows == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
}

// SearchKnowledge returns the ready chunks nearest to a vector.
func (r *Repository) SearchKnowledge(ctx context.Context, scope domain.Scope, query domain.SearchQuery) ([]domain.KnowledgeItem, error) {
	var items []domain.KnowledgeItem

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.SearchKnowledge(ctx, sqlcgen.SearchKnowledgeParams{
			WorkspaceID:   scope.WorkspaceID,
			AgentID:       optionalID(query.AgentID),
			Vector:        vectorLiteral(query.Vector),
			MinSimilarity: query.MinSimilarity,
			Limit:         limitOf(query.Limit),
		})
		if err != nil {
			return translate(err, "search knowledge")
		}

		items = make([]domain.KnowledgeItem, 0, len(rows))
		for _, row := range rows {
			items = append(items, fromSearchKnowledgeRow(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return items, nil
}

// RecordRetrieval notes one chunk that reached a prompt.
func (r *Repository) RecordRetrieval(ctx context.Context, scope domain.Scope, usage domain.Retrieval) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		return translate(queries.RecordRetrieval(ctx, sqlcgen.RecordRetrievalParams{
			WorkspaceID:     scope.WorkspaceID,
			TaskID:          optionalID(usage.TaskID),
			AgentID:         optionalID(usage.AgentID),
			MemoryID:        optionalID(usage.MemoryID),
			KnowledgeItemID: optionalID(usage.KnowledgeID),
			Similarity:      float32(usage.Similarity),
		}), "record retrieval")
	})
}

// ---------------------------------------------------------------- agents

// Agents returns the Bolu of the workspace with the name and the switch.
func (r *Repository) Agents(ctx context.Context, scope domain.Scope) ([]domain.AgentInfo, error) {
	var agents []domain.AgentInfo

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListWorkspaceAgents(ctx, scope.WorkspaceID)
		if err != nil {
			return translate(err, "list agents")
		}

		agents = make([]domain.AgentInfo, 0, len(rows))
		for _, row := range rows {
			agents = append(agents, domain.AgentInfo{
				ID:                row.ID,
				Name:              row.Name,
				ExtractionEnabled: row.MemoryExtractionEnabled,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return agents, nil
}

// ExtractionEnabled reports one Bolu's switch.
func (r *Repository) ExtractionEnabled(ctx context.Context, scope domain.Scope, agentID uuid.UUID) (bool, error) {
	enabled := false

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		value, err := queries.AgentExtractionEnabled(ctx, sqlcgen.AgentExtractionEnabledParams{
			ID:          agentID,
			WorkspaceID: scope.WorkspaceID,
		})
		if err != nil {
			return translate(err, "agent extraction switch")
		}

		enabled = value
		return nil
	})
	if err != nil {
		return false, err
	}

	return enabled, nil
}

// ----------------------------------------------------------------- helpers

func (r *Repository) read(ctx context.Context, scope domain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("memory: database is not configured")
	}
	return r.pool.InScopeRead(ctx, databaseScope(scope), func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

func (r *Repository) write(ctx context.Context, scope domain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("memory: database is not configured")
	}
	return r.pool.InScope(ctx, databaseScope(scope), func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

func databaseScope(scope domain.Scope) database.Scope {
	return database.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}
}

// translate maps the driver's errors onto this module's.
func translate(err error, operation string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", domain.ErrNotFound, operation)
	}
	return fmt.Errorf("memory: %s: %w", operation, err)
}

// vectorLiteral renders a vector the way Postgres reads it, which is the one
// representation both sides of the wire agree on without the driver knowing the
// type.
func vectorLiteral(vector []float32) string {
	return pgvector.NewVector(vector).String()
}

func optionalID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// listLimit is the ceiling of a list read, so one request cannot pull a whole
// knowledge base.
const listLimit = 200

func limitOf(limit int) int32 {
	if limit <= 0 {
		return 50
	}
	if limit > listLimit {
		return listLimit
	}
	return int32(limit)
}
