package container

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	retentiondomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/retention/domain"
	taskdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	"github.com/robzlabz/employeebot/packages/platform/config"
)

// The wiring tests for retention cover what only the container can get wrong: that a deployment
// without a database leaves the job unwired rather than nil-dereferencing, and that the two
// adapters translate a workspace and a window without dropping either — the task module's step
// prune and the chat module's attachment sweep.

// TestOpenRetentionWithoutADatabaseLeavesTheJobUnwired keeps the container's "running without
// Postgres" contract: the pass has nowhere to read its queue from, so it is not built at all.
func TestOpenRetentionWithoutADatabaseLeavesTheJobUnwired(t *testing.T) {
	c := &Container{
		Config:       testConfig(),
		Logger:       zap.NewNop(),
		Repositories: &Repositories{},
		Services:     &Services{},
	}

	require.NoError(t, c.openRetention(context.Background(), c.Config))
	require.Nil(t, c.Retention)
	require.Nil(t, c.Services.Retention)
	require.Nil(t, c.Repositories.Retention)
}

// TestTheRetentionPolicyComesFromTheConfiguration: a deployment's days become the window the
// pass works in, and it is derived once rather than in each call site — which is what stops a
// configured zero from meaning "keep everything" in one place and "prune everything" in another.
func TestTheRetentionPolicyComesFromTheConfiguration(t *testing.T) {
	cfg := &config.Config{
		Retention: config.RetentionConfig{StepDays: 7, AttachmentDays: 120, Interval: time.Hour, Batch: 50},
		Storage:   config.StorageConfig{AttachmentStagedTTL: 6 * time.Hour},
	}

	service := buildRetention(cfg, nil, nil, nil, nil, zap.NewNop())
	require.NotNil(t, service)

	policy := service.Policy()
	require.Equal(t, 7*24*time.Hour, policy.StepWindow)
	require.Equal(t, 120*24*time.Hour, policy.AttachmentWindow)
	require.Equal(t, 6*time.Hour, policy.StagedTTL)
	require.Equal(t, 50, policy.Limit)

	// A deployment that names no window keeps the product default of thirty days, and a zero
	// attachment window keeps attachments with their message rather than being read as a
	// window of zero seconds.
	cfg.Retention = config.RetentionConfig{}
	cfg.Storage = config.StorageConfig{}
	policy = buildRetention(cfg, nil, nil, nil, nil, zap.NewNop()).Policy()
	require.Equal(t, retentiondomain.DefaultStepWindow, policy.StepWindow)
	require.Zero(t, policy.AttachmentWindow)
	require.Equal(t, retentiondomain.DefaultStagedTTL, policy.StagedTTL)
	require.Equal(t, retentiondomain.DefaultLimit, policy.Limit)
}

// TestTheStepAdapterPassesTheWorkspaceAndTheWindowOn: the job hands the task module a workspace
// and a cutoff, and what comes back is translated without losing the count or the backlog.
func TestTheStepAdapterPassesTheWorkspaceAndTheWindowOn(t *testing.T) {
	workspace := uuid.New()
	cutoff := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	steps := &recordingSteps{pruned: taskdomain.StepPrune{Pruned: 4, Remaining: 9}}
	adapter := taskStepPruner{steps: steps}

	result, err := adapter.PruneSteps(context.Background(), retentiondomain.Scope{WorkspaceID: workspace}, cutoff, 25)
	require.NoError(t, err)
	require.Equal(t, retentiondomain.PrunedSteps{Count: 4, Remaining: 9}, result)

	require.Len(t, steps.seen, 1)
	require.Equal(t, workspace, steps.seen[0].scope.WorkspaceID, "the workspace is the tenant, and it travels")
	require.Equal(t, cutoff, steps.seen[0].olderThan, "the window is the job's, not the module's")
	require.Equal(t, 25, steps.seen[0].limit)
}

// TestTheAttachmentAdapterPassesTheWorkspaceAndTheWindowOn: the same for the chat module, whose
// sweep carries the bytes back with the count.
func TestTheAttachmentAdapterPassesTheWorkspaceAndTheWindowOn(t *testing.T) {
	workspace := uuid.New()
	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	chat := &recordingChat{pruned: chatdomain.AttachmentPrune{Count: 2, BytesFreed: 8192}}
	adapter := chatAttachmentPruner{chat: chat}

	result, err := adapter.PruneAttachments(context.Background(), retentiondomain.Scope{WorkspaceID: workspace}, cutoff, 10)
	require.NoError(t, err)
	require.Equal(t, retentiondomain.PrunedAttachments{Count: 2, BytesFreed: int64(8192)}, result)

	require.Len(t, chat.seen, 1)
	require.Equal(t, workspace, chat.seen[0].scope.WorkspaceID)
	require.Equal(t, cutoff, chat.seen[0].olderThan)
	require.Equal(t, 10, chat.seen[0].limit)
}

// TestTheStagedAdapterPassesTheWorkspaceAndTheWindowOn: the staged sweep goes through the same
// adapter as the attachment window and must not be answered by it — a file nobody ever sent is
// removed on its own clock, and a wiring that crossed the two would delete a file a message
// carries.
func TestTheStagedAdapterPassesTheWorkspaceAndTheWindowOn(t *testing.T) {
	workspace := uuid.New()
	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	chat := &recordingChat{staged: chatdomain.AttachmentPrune{Count: 3, BytesFreed: 4096}}
	adapter := chatAttachmentPruner{chat: chat}

	result, err := adapter.PruneStagedAttachments(context.Background(), retentiondomain.Scope{WorkspaceID: workspace}, cutoff, 10)
	require.NoError(t, err)
	require.Equal(t, retentiondomain.PrunedAttachments{Count: 3, BytesFreed: int64(4096)}, result)

	require.Len(t, chat.stagedSeen, 1)
	require.Empty(t, chat.seen, "the attachment window is a different sweep")
	require.Equal(t, workspace, chat.stagedSeen[0].scope.WorkspaceID)
	require.Equal(t, cutoff, chat.stagedSeen[0].olderThan)
	require.Equal(t, 10, chat.stagedSeen[0].limit)
}

// TestAnUnwiredPrunerIsNotAFailure: a deployment assembled without one of the two modules prunes
// the other and reports zero for what is missing, rather than failing the pass.
func TestAnUnwiredPrunerIsNotAFailure(t *testing.T) {
	workspace := retentiondomain.Scope{WorkspaceID: uuid.New()}

	steps, err := taskStepPruner{}.PruneSteps(context.Background(), workspace, time.Now(), 10)
	require.NoError(t, err)
	require.Equal(t, retentiondomain.PrunedSteps{}, steps)

	attachments, err := chatAttachmentPruner{}.PruneAttachments(context.Background(), workspace, time.Now(), 10)
	require.NoError(t, err)
	require.Equal(t, retentiondomain.PrunedAttachments{}, attachments)

	// A staged sweep of an unwired chat module is the same kind of nothing.
	staged, err := chatAttachmentPruner{}.PruneStagedAttachments(context.Background(), workspace, time.Now(), 10)
	require.NoError(t, err)
	require.Equal(t, retentiondomain.PrunedAttachments{}, staged)
}

// recordingSteps is the task module's pruner, recording what it was asked.
type recordingSteps struct {
	pruned taskdomain.StepPrune
	seen   []stepCall
}

type stepCall struct {
	scope     taskdomain.Scope
	olderThan time.Time
	limit     int
}

func (s *recordingSteps) PruneContents(_ context.Context, scope taskdomain.Scope, olderThan time.Time, limit int) (taskdomain.StepPrune, error) {
	s.seen = append(s.seen, stepCall{scope: scope, olderThan: olderThan, limit: limit})

	return s.pruned, nil
}

// recordingChat is the chat service, recording the same, with the staged sweep kept apart so a
// test can tell which window a workspace was swept with.
type recordingChat struct {
	chatdomain.Service

	pruned     chatdomain.AttachmentPrune
	seen       []attachmentCall
	staged     chatdomain.AttachmentPrune
	stagedSeen []attachmentCall
}

type attachmentCall struct {
	scope     chatdomain.Scope
	olderThan time.Time
	limit     int
}

func (c *recordingChat) PruneAttachments(_ context.Context, scope chatdomain.Scope, olderThan time.Time, limit int) (chatdomain.AttachmentPrune, error) {
	c.seen = append(c.seen, attachmentCall{scope: scope, olderThan: olderThan, limit: limit})

	return c.pruned, nil
}

// PruneStagedAttachments implements chatdomain.Service.
func (c *recordingChat) PruneStagedAttachments(_ context.Context, scope chatdomain.Scope, olderThan time.Time, limit int) (chatdomain.AttachmentPrune, error) {
	c.stagedSeen = append(c.stagedSeen, attachmentCall{scope: scope, olderThan: olderThan, limit: limit})

	return c.staged, nil
}
