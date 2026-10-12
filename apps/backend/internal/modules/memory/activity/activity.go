// Package activity holds the side-effecting half of memory extraction: the
// method the worker registers, which reads the thread through the service.
//
// It exists as a thin layer rather than as a temporal-aware service for two
// reasons: the workflow package must stay free of the module's own services so
// the replay stays deterministic, and the service must stay free of the SDK so a
// unit test can call it without a server.
package activity

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/memory/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/memory/workflow"
)

// Deps are the activity dependencies.
type Deps struct {
	Memories domain.Service
	Logger   *zap.Logger
}

// Activities is the set the worker registers.
type Activities struct {
	deps Deps
}

// New builds the activity set.
func New(deps Deps) *Activities {
	return &Activities{deps: deps}
}

// ExtractConversation reads one quiet conversation and stores what it finds.
//
// It returns a result rather than an error for the states that are not failures:
// a Bolu that switched extraction off, and a thread that ended up with nothing
// worth keeping. An error is reserved for the cases a retry could fix.
func (a *Activities) ExtractConversation(ctx context.Context, input workflow.ExtractionInput) (workflow.ExtractionResult, error) {
	if a.deps.Memories == nil {
		return workflow.ExtractionResult{}, fmt.Errorf("%w: the memory service is not configured", domain.ErrInvalidInput)
	}

	workspaceID, err := parseID(input.WorkspaceID, "workspace")
	if err != nil {
		return workflow.ExtractionResult{}, err
	}
	conversationID, err := parseID(input.ConversationID, "conversation")
	if err != nil {
		return workflow.ExtractionResult{}, err
	}
	agentID, err := parseID(input.AgentID, "bolu")
	if err != nil {
		return workflow.ExtractionResult{}, err
	}

	result, err := a.deps.Memories.Extract(ctx, domain.Scope{WorkspaceID: workspaceID}, domain.ExtractRequest{
		ConversationID: conversationID,
		AgentID:        agentID,
	})
	if err != nil {
		return workflow.ExtractionResult{}, err
	}

	if a.deps.Logger != nil && (result.Stored > 0 || result.Merged > 0) {
		a.deps.Logger.Info("memory extracted",
			zap.String("conversation_id", conversationID.String()),
			zap.Int("stored", result.Stored),
			zap.Int("merged", result.Merged),
			zap.Int("skipped", result.Skipped),
			zap.Int("refused", result.Refused),
		)
	}

	return workflow.ExtractionResult{
		Disabled: result.Disabled,
		Stored:   result.Stored,
		Merged:   result.Merged,
		Skipped:  result.Skipped,
		Refused:  result.Refused,
	}, nil
}

// parseID reads an id that travelled through the workflow as a string. An empty
// agent id is allowed and means "no Bolu to check" — a conversation whose
// participants are all gone still has memories worth keeping.
func parseID(value, field string) (uuid.UUID, error) {
	if value == "" {
		return uuid.Nil, nil
	}

	id, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: %s id is not valid", domain.ErrInvalidInput, field)
	}

	return id, nil
}
