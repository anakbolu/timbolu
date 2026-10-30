package activity

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/memory/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/memory/workflow"
)

// stubService is the memory service the activity drives.
type stubService struct {
	result domain.ExtractResult
	err    error
	asked  []domain.ExtractRequest
	scopes []domain.Scope
}

func (s *stubService) Extract(_ context.Context, scope domain.Scope, request domain.ExtractRequest) (domain.ExtractResult, error) {
	s.scopes = append(s.scopes, scope)
	s.asked = append(s.asked, request)
	if s.err != nil {
		return domain.ExtractResult{}, s.err
	}
	return s.result, nil
}

// The other methods are not reached by this activity, and a stub that implemented
// them would be a second copy of the service's contract.
func (s *stubService) Memories(context.Context, domain.Scope, domain.MemoryFilter) ([]domain.Memory, error) {
	return nil, nil
}

func (s *stubService) Add(context.Context, domain.Scope, domain.AddRequest) (domain.Memory, error) {
	return domain.Memory{}, nil
}

func (s *stubService) Edit(context.Context, domain.Scope, domain.EditRequest) (domain.Memory, error) {
	return domain.Memory{}, nil
}

func (s *stubService) Forget(context.Context, domain.Scope, uuid.UUID) error { return nil }

func (s *stubService) Knowledge(context.Context, domain.Scope, domain.KnowledgeFilter) ([]domain.KnowledgeItem, error) {
	return nil, nil
}

func (s *stubService) Ingest(context.Context, domain.Scope, domain.IngestRequest) ([]domain.KnowledgeItem, error) {
	return nil, nil
}

func (s *stubService) ForgetKnowledge(context.Context, domain.Scope, uuid.UUID) error { return nil }

func (s *stubService) Recall(context.Context, domain.Scope, domain.RecallRequest) (domain.Recall, error) {
	return domain.Recall{}, nil
}

func (s *stubService) Remember(context.Context, domain.Scope, domain.RememberRequest) (domain.RememberResult, error) {
	return domain.RememberResult{}, nil
}

func (s *stubService) Active(context.Context, domain.Scope, uuid.UUID, uuid.UUID) error { return nil }

// TestExtractConversationPassesTheScopeAndTheThread is the activity contract: the
// workspace and the Bolu travel from the workflow input, and every candidate the
// service reports is passed back so the workflow can log what one pass did.
func TestExtractConversationPassesTheScopeAndTheThread(t *testing.T) {
	service := &stubService{result: domain.ExtractResult{Stored: 2, Merged: 1, Skipped: 3, Refused: 1}}
	activities := New(Deps{Memories: service})

	workspace := uuid.New()
	conversation := uuid.New()
	agent := uuid.New()

	result, err := activities.ExtractConversation(context.Background(), workflow.ExtractionInput{
		WorkspaceID:    workspace.String(),
		ConversationID: conversation.String(),
		AgentID:        agent.String(),
	})
	require.NoError(t, err)

	require.Equal(t, 2, result.Stored)
	require.Equal(t, 1, result.Merged)
	require.Equal(t, 3, result.Skipped)
	require.Equal(t, 1, result.Refused)
	require.False(t, result.Disabled)

	require.Len(t, service.asked, 1)
	require.Equal(t, conversation, service.asked[0].ConversationID)
	require.Equal(t, agent, service.asked[0].AgentID)
	require.Equal(t, workspace, service.scopes[0].WorkspaceID)
}

// TestExtractConversationPassesDisabledThrough: a Bolu that turned extraction off
// is not a failure, so the workflow must not retry it.
func TestExtractConversationPassesDisabledThrough(t *testing.T) {
	service := &stubService{result: domain.ExtractResult{Disabled: true}}
	activities := New(Deps{Memories: service})

	result, err := activities.ExtractConversation(context.Background(), workflow.ExtractionInput{
		WorkspaceID:    uuid.NewString(),
		ConversationID: uuid.NewString(),
		AgentID:        uuid.NewString(),
	})
	require.NoError(t, err)
	require.True(t, result.Disabled)
}

// TestExtractConversationRefusesABadId: an id that will not parse is the caller's
// problem, and reporting it is what stops a retry that would fail the same way.
func TestExtractConversationRefusesABadId(t *testing.T) {
	activities := New(Deps{Memories: &stubService{}})

	_, err := activities.ExtractConversation(context.Background(), workflow.ExtractionInput{
		WorkspaceID:    "bukan-uuid",
		ConversationID: uuid.NewString(),
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = activities.ExtractConversation(context.Background(), workflow.ExtractionInput{
		WorkspaceID:    uuid.NewString(),
		ConversationID: "bukan-uuid",
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = activities.ExtractConversation(context.Background(), workflow.ExtractionInput{
		WorkspaceID:    uuid.NewString(),
		ConversationID: uuid.NewString(),
		AgentID:        "bukan-uuid",
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestExtractConversationWithoutAWorkspace: a deployment whose memory service was
// never assembled reports it rather than dereferencing nil.
func TestExtractConversationWithoutAWorkspace(t *testing.T) {
	activities := New(Deps{})

	_, err := activities.ExtractConversation(context.Background(), workflow.ExtractionInput{
		WorkspaceID:    uuid.NewString(),
		ConversationID: uuid.NewString(),
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestExtractConversationReportsAFailure: a pass that failed is an error, which
// is what lets the workflow's retry policy run.
func TestExtractConversationReportsAFailure(t *testing.T) {
	service := &stubService{err: errors.New("the model is down")}
	activities := New(Deps{Memories: service})

	_, err := activities.ExtractConversation(context.Background(), workflow.ExtractionInput{
		WorkspaceID:    uuid.NewString(),
		ConversationID: uuid.NewString(),
	})
	require.Error(t, err)
}

// TestEveryExtractionActivityNameHasAMethod is the same contract the task runtime
// carries: the workflow schedules an activity by name, and nothing in the
// compiler connects that name to the worker's method.
func TestEveryExtractionActivityNameHasAMethod(t *testing.T) {
	require.Equal(t, "ExtractConversation", workflow.ActivityExtract,
		"the registered method's name must be the name the workflow schedules")

	// The worker registers the method itself, so a rename that broke the pair
	// would leave the extraction scheduled with nobody to run it.
	activities := New(Deps{Memories: &stubService{}})
	require.NotNil(t, activities)
}
