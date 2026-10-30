package repository

import (
	"time"

	"github.com/google/uuid"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/memory/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/memory/repository/sqlcgen"
)

// The conversions from the generated rows to the domain types. They are written
// out rather than shared through a helper because sqlc gives every query its own
// row type, and a positional helper that took eleven arguments would make a
// swapped field a silent bug instead of a compile error.

func fromCreateRow(row sqlcgen.CreateMemoryRow) domain.Memory {
	return domain.Memory{
		ID:              row.ID,
		WorkspaceID:     row.WorkspaceID,
		AgentID:         deref(row.AgentID),
		Content:         row.Content,
		Kind:            row.Kind,
		SourceMessageID: deref(row.SourceMessageID),
		Pinned:          row.Pinned,
		CreatedBy:       row.CreatedBy,
		SupersededBy:    deref(row.SupersededBy),
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

func fromGetRow(row sqlcgen.GetMemoryRow) domain.Memory {
	return domain.Memory{
		ID:              row.ID,
		WorkspaceID:     row.WorkspaceID,
		AgentID:         deref(row.AgentID),
		Content:         row.Content,
		Kind:            row.Kind,
		SourceMessageID: deref(row.SourceMessageID),
		Pinned:          row.Pinned,
		CreatedBy:       row.CreatedBy,
		SupersededBy:    deref(row.SupersededBy),
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

func fromListRow(row sqlcgen.ListMemoriesRow) domain.Memory {
	return domain.Memory{
		ID:              row.ID,
		WorkspaceID:     row.WorkspaceID,
		AgentID:         deref(row.AgentID),
		Content:         row.Content,
		Kind:            row.Kind,
		SourceMessageID: deref(row.SourceMessageID),
		Pinned:          row.Pinned,
		CreatedBy:       row.CreatedBy,
		SupersededBy:    deref(row.SupersededBy),
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

func fromUpdateRow(row sqlcgen.UpdateMemoryRow) domain.Memory {
	return domain.Memory{
		ID:              row.ID,
		WorkspaceID:     row.WorkspaceID,
		AgentID:         deref(row.AgentID),
		Content:         row.Content,
		Kind:            row.Kind,
		SourceMessageID: deref(row.SourceMessageID),
		Pinned:          row.Pinned,
		CreatedBy:       row.CreatedBy,
		SupersededBy:    deref(row.SupersededBy),
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

func fromSearchRow(row sqlcgen.SearchMemoriesRow) domain.Memory {
	return domain.Memory{
		ID:              row.ID,
		WorkspaceID:     row.WorkspaceID,
		AgentID:         deref(row.AgentID),
		Content:         row.Content,
		Kind:            row.Kind,
		SourceMessageID: deref(row.SourceMessageID),
		Pinned:          row.Pinned,
		CreatedBy:       row.CreatedBy,
		SupersededBy:    deref(row.SupersededBy),
		Similarity:      row.Similarity,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

func fromCreateKnowledgeRow(row sqlcgen.CreateKnowledgeRow) domain.KnowledgeItem {
	return knowledge(row.ID, row.WorkspaceID, row.AgentID, row.Source, row.ChunkIndex, row.Content, row.Status, 0, row.CreatedAt)
}

func fromGetKnowledgeRow(row sqlcgen.GetKnowledgeRow) domain.KnowledgeItem {
	return knowledge(row.ID, row.WorkspaceID, row.AgentID, row.Source, row.ChunkIndex, row.Content, row.Status, 0, row.CreatedAt)
}

func fromListKnowledgeRow(row sqlcgen.ListKnowledgeRow) domain.KnowledgeItem {
	return knowledge(row.ID, row.WorkspaceID, row.AgentID, row.Source, row.ChunkIndex, row.Content, row.Status, 0, row.CreatedAt)
}

func fromSearchKnowledgeRow(row sqlcgen.SearchKnowledgeRow) domain.KnowledgeItem {
	return knowledge(row.ID, row.WorkspaceID, row.AgentID, row.Source, row.ChunkIndex, row.Content, row.Status, row.Similarity, row.CreatedAt)
}

// knowledge builds one chunk. The four knowledge queries return the same
// columns, so they share one builder instead of four near-identical literals.
func knowledge(
	id, workspaceID uuid.UUID,
	agentID *uuid.UUID,
	source string,
	chunkIndex int32,
	content, status string,
	similarity float64,
	createdAt time.Time,
) domain.KnowledgeItem {
	return domain.KnowledgeItem{
		ID:          id,
		WorkspaceID: workspaceID,
		AgentID:     deref(agentID),
		Source:      source,
		ChunkIndex:  int(chunkIndex),
		Content:     content,
		Status:      status,
		Similarity:  similarity,
		CreatedAt:   createdAt,
	}
}

func deref(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}
