package container

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"
	"go.uber.org/zap"

	agentdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/agent/domain"
	agentmocks "github.com/anakbolu/timbolu/apps/backend/internal/modules/agent/domain/mocks"
	approvaldomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/approval/domain"
	chatdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/chat/domain"
	chatmocks "github.com/anakbolu/timbolu/apps/backend/internal/modules/chat/domain/mocks"
	memoryactivity "github.com/anakbolu/timbolu/apps/backend/internal/modules/memory/activity"
	memorydomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/memory/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/memory/domain/mocks"
	memoryhandler "github.com/anakbolu/timbolu/apps/backend/internal/modules/memory/handler"
	taskdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/task/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/middleware"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/temporal"
	"github.com/anakbolu/timbolu/packages/platform/database"
)

// The wiring tests for the memory module cover what only the container can get
// wrong: which routes exist, what they answer when the module is not configured,
// and whether the two adapters hold — the one the chat module asks about a
// message, and the one the task runtime asks for a prompt section.

// TestOpenMemoryWithoutADatabaseLeavesTheModuleUnwired keeps the container's
// "running without Postgres" contract.
func TestOpenMemoryWithoutADatabaseLeavesTheModuleUnwired(t *testing.T) {
	c := &Container{
		Config:       testConfig(),
		Logger:       zap.NewNop(),
		Repositories: &Repositories{},
		Services:     &Services{},
	}

	require.NoError(t, c.openMemory(context.Background(), c.Config))
	require.Nil(t, c.Memory, "the service stays nil, so its routes report not configured")
	require.Nil(t, c.MemoryTasks, "and the worker registers nothing of it")
	require.Nil(t, c.memoryForChat(), "and the chat module is handed no port")
	require.Nil(t, c.memoryRecallForTask(), "and the task runtime is handed none either")
}

// TestMemoryRoutesAnswer503WhenTheModuleIsMissing is the honest answer for a
// deployment without a database: every documented route exists and says the
// feature is unavailable.
func TestMemoryRoutesAnswer503WhenTheModuleIsMissing(t *testing.T) {
	app := memoryApp(t, nil)

	for _, request := range []struct{ method, path string }{
		{http.MethodGet, "/api/memories"},
		{http.MethodPost, "/api/memories"},
		{http.MethodPatch, "/api/memories/" + uuid.NewString()},
		{http.MethodDelete, "/api/memories/" + uuid.NewString()},
		{http.MethodGet, "/api/knowledge"},
		{http.MethodPost, "/api/knowledge"},
		{http.MethodDelete, "/api/knowledge/" + uuid.NewString()},
	} {
		req := httptest.NewRequest(request.method, request.path, nil)
		resp, err := app.Test(req, -1)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		require.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode, request.path)
		require.Contains(t, string(body), "memory_not_configured", request.path)
	}
}

// TestMemoryRoutesAreMountedUnderTheTenantGroup proves the endpoints exist once
// the module is configured, and that the scope the handler passes on is the one
// the middleware resolved.
func TestMemoryRoutesAreMountedUnderTheTenantGroup(t *testing.T) {
	service := mocks.NewService(t)
	app := memoryApp(t, service)

	service.EXPECT().
		Memories(mock.Anything, mock.MatchedBy(func(scope memorydomain.Scope) bool {
			return scope.WorkspaceID == memoryWorkspaceID
		}), mock.Anything).
		Return(nil, nil).
		Once()

	req := httptest.NewRequest(http.MethodGet, "/api/memories?layer=main", nil)
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestMemoryChatAdapterCarriesTheConfirmation is the seam the reply path uses:
// what the memory module answers becomes what the chat module writes, verbatim,
// so the confirmation and the stored row cannot disagree.
func TestMemoryChatAdapterCarriesTheConfirmation(t *testing.T) {
	service := mocks.NewService(t)
	adapter := memoryChat{service: service}
	scope := chatdomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}
	agentID := uuid.New()
	messageID := uuid.New()

	service.EXPECT().
		Remember(mock.Anything, mock.MatchedBy(func(s memorydomain.Scope) bool {
			return s.WorkspaceID == scope.WorkspaceID
		}), mock.MatchedBy(func(req memorydomain.RememberRequest) bool {
			return req.Text == "ingat ya, jam buka 08.00" && req.AgentID == agentID && req.MessageID == messageID
		})).
		Return(memorydomain.RememberResult{
			Handled:      true,
			Confirmation: "Siap, aku ingat itu",
		}, nil).
		Once()

	result, err := adapter.Remember(context.Background(), scope, chatdomain.RememberRequest{
		Text:      "ingat ya, jam buka 08.00",
		AgentID:   agentID,
		MessageID: messageID,
	})
	require.NoError(t, err)
	require.True(t, result.Handled)
	require.Equal(t, "Siap, aku ingat itu", result.Confirmation)
}

// TestMemoryChatAdapterForwardsTheDebounce: a message resets the extraction
// timer, and the conversation id is what the schedule is keyed on.
func TestMemoryChatAdapterForwardsTheDebounce(t *testing.T) {
	service := mocks.NewService(t)
	adapter := memoryChat{service: service}
	scope := chatdomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}
	conversationID := uuid.New()
	agentID := uuid.New()

	service.EXPECT().
		Active(mock.Anything, mock.Anything, conversationID, agentID).
		Return(nil).
		Once()

	require.NoError(t, adapter.ConversationActive(context.Background(), scope, conversationID, agentID))
}

// TestMemoryRecallAdapterCarriesBothHalves is the task runtime's seam: the main
// memory and the retrieved block reach the workflow, and the parts that are only
// the memory module's business do not.
func TestMemoryRecallAdapterCarriesBothHalves(t *testing.T) {
	service := mocks.NewService(t)
	adapter := memoryRecall{service: service}
	agentID := uuid.New()
	taskID := uuid.New()

	service.EXPECT().
		Recall(mock.Anything, mock.Anything, mock.MatchedBy(func(req memorydomain.RecallRequest) bool {
			return req.AgentID == agentID && req.TaskID == taskID && req.Query == "berapa ongkir?"
		})).
		Return(memorydomain.Recall{
			Stable:    "Yang kamu ingat tentang workspace ini:\n- Jam buka 08.00-21.00",
			Retrieved: "<data sumber=\"katalog.pdf\">\nOngkir Bandung 15 ribu\n</data>",
			Used:      1,
		}, nil).
		Once()

	recall, err := adapter.Recall(context.Background(), taskdomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()},
		taskdomain.RecallRequest{AgentID: agentID, TaskID: taskID, Query: "berapa ongkir?"})
	require.NoError(t, err)

	require.Contains(t, recall.Stable, "Jam buka 08.00-21.00")
	require.Contains(t, recall.Retrieved, "Ongkir Bandung 15 ribu")
	require.Equal(t, 1, recall.Used)
}

// TestMemoryAuditorWritesThroughTheApprovalRepository is why the adapter exists:
// the trail lives in the approval module's table, and the module that owns a table
// is the module that writes it.
func TestMemoryAuditorWritesThroughTheApprovalRepository(t *testing.T) {
	repository := &recordingAuditRepository{}
	auditor := memoryAuditor{repository: repository}

	userID := uuid.New()
	scope := memorydomain.Scope{UserID: userID, WorkspaceID: uuid.New()}

	require.NoError(t, auditor.Record(context.Background(), scope, memorydomain.AuditEntry{
		Action:     memorydomain.ActionCreated,
		ObjectType: "memory",
		ObjectID:   uuid.NewString(),
		Metadata:   map[string]any{"layer": memorydomain.LayerMain},
	}))

	require.Len(t, repository.entries, 1)
	require.Equal(t, memorydomain.ActionCreated, repository.entries[0].Action)
	require.Equal(t, userID, repository.entries[0].ActorUserID, "the person who did it travels with the entry")
	require.Equal(t, memorydomain.LayerMain, repository.entries[0].Metadata["layer"])
}

// TestMemoryConversationsRendersTheThreadTheExtractorReads: the adapter turns
// stored blocks into the text a model reads, and drops the turns that carry none,
// which is what makes the transcript a conversation rather than a list of empty
// placeholders.
func TestMemoryConversationsRendersTheThreadTheExtractorReads(t *testing.T) {
	speaker := uuid.New()
	thread := []chatdomain.Message{
		{ID: uuid.New(), AuthorUserID: uuid.New(), Blocks: []chatdomain.Block{textBody("berapa ongkir ke Bandung?")}},
		{ID: uuid.New(), AuthorAgentID: speaker, Blocks: []chatdomain.Block{textBody("Sekitar 15 ribu.")}},
		// A chart with no prose is summarised rather than dropped, and a
		// placeholder with nothing at all is dropped.
		{ID: uuid.New(), AuthorAgentID: speaker, Blocks: []chatdomain.Block{{Type: chatdomain.BlockChart, Body: []byte(`{"type":"chart","title":"Ongkir"}`)}}},
		{ID: uuid.New(), AuthorAgentID: speaker, Blocks: nil},
	}

	messages := chatmocks.NewMessageRepository(t)
	messages.EXPECT().LatestMessages(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(thread, nil).Once()

	agents := agentmocks.NewService(t)
	conversations := memoryConversations{messages: messages, agents: agents}

	agents.EXPECT().List(mock.Anything, mock.Anything).Return([]agentdomain.Agent{{ID: speaker, Name: "Biru", Role: "Operasional"}}, nil).Once()

	scope := memorydomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}
	utterances, err := conversations.Recent(context.Background(), scope, uuid.New(), 30)
	require.NoError(t, err)

	require.Len(t, utterances, 3, "a turn with no text at all is not a turn")
	require.Equal(t, "berapa ongkir ke Bandung?", utterances[0].Text)
	require.False(t, utterances[0].FromAgent())
	require.Equal(t, "Biru", utterances[1].AgentName, "the transcript says who spoke, not which id did")
	require.True(t, utterances[1].FromAgent())
	require.Equal(t, "[grafik]", utterances[2].Text)
}

// TestMemoryConversationsSurvivesAMissingRegistry: the names are a nicety, and a
// registry that cannot be read must not stop the extraction.
func TestMemoryConversationsSurvivesAMissingRegistry(t *testing.T) {
	messages := chatmocks.NewMessageRepository(t)
	messages.EXPECT().LatestMessages(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]chatdomain.Message{
			{ID: uuid.New(), AuthorUserID: uuid.New(), Blocks: []chatdomain.Block{textBody("halo")}},
		}, nil).
		Once()

	agents := agentmocks.NewService(t)
	agents.EXPECT().List(mock.Anything, mock.Anything).Return(nil, errors.New("the registry is down")).Once()

	conversations := memoryConversations{messages: messages, agents: agents}
	utterances, err := conversations.Recent(context.Background(), memorydomain.Scope{WorkspaceID: uuid.New()}, uuid.New(), 30)
	require.NoError(t, err)
	require.Len(t, utterances, 1)
}

// TestMemorySchedulerUsesTheConfiguredQuietPeriod is the defect this wiring had:
// the scheduler was built with the service it was about to create, so it captured
// the nil interface and every conversation waited for the product default instead
// of the configured period. The stub stack's timer is what exposed it.
func TestMemorySchedulerUsesTheConfiguredQuietPeriod(t *testing.T) {
	bare := &Container{Config: testConfig(), Logger: zap.NewNop()}
	require.Nil(t, bare.memoryScheduler(bare.Config),
		"without Temporal there is nothing to schedule, which is not an error")

	cfg := testConfig()
	cfg.Memory.QuietSeconds = 7

	// The client is a value rather than a connection: what is under test is which
	// period the scheduler carries, not whether it can reach a server.
	c := &Container{Config: cfg, Logger: zap.NewNop(), Temporal: &temporal.Client{}}

	scheduler, ok := c.memoryScheduler(cfg).(memoryScheduler)
	require.True(t, ok, "temporal is configured, so a scheduler must be built")
	require.Equal(t, 7*time.Second, scheduler.quiet,
		"the workflow must wait for the configured period, not for the default")
}

// TestMemoryScheduleAllowsAFinishedRunToBeReplaced is the defect the stub stack
// found and no unit test could: the default reuse policy refuses an id that already
// ran, so a conversation was extracted at most once in its whole life — every later
// message logged a warning and learned nothing. A task's id must still be refused
// (a duplicate dispatch there would replace a finished task's history), so the
// policy is set on the memory schedule rather than in StartOptions.
func TestMemoryScheduleAllowsAFinishedRunToBeReplaced(t *testing.T) {
	cfg := testConfig()
	c := &Container{Config: cfg, Logger: zap.NewNop(), Temporal: &temporal.Client{}}

	scheduler, ok := c.memoryScheduler(cfg).(memoryScheduler)
	require.True(t, ok)

	options := scheduler.options(uuid.New())
	require.Equal(t, enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE, options.WorkflowIDReusePolicy)
	require.Equal(t, scheduler.client.TaskQueueAgent(), options.TaskQueue,
		"the extraction rides on the agent worker's queue, which is what registers it")

	// And the task runtime keeps the opposite policy, which is what makes the two
	// schedules different rather than a shared helper nobody can reason about.
	taskOptions := scheduler.client.StartOptions("task-anything")
	require.Equal(t, enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE, taskOptions.WorkflowIDReusePolicy)
}

// TestRegisterMemoryTasksNeedsAWorker: a deployment without the module registers
// nothing rather than panicking on a nil activity set.
func TestRegisterMemoryTasksNeedsAWorker(t *testing.T) {
	worker := &recordingWorker{}

	RegisterMemoryTasks(worker, &Container{})
	require.Empty(t, worker.workflows)

	RegisterMemoryTasks(worker, &Container{MemoryTasks: memoryactivity.New(memoryactivity.Deps{})})
	require.Len(t, worker.workflows, 1)
	require.Len(t, worker.activities, 1)
}

// memoryWorkspaceID is the workspace the mounted route table resolves.
var memoryWorkspaceID = uuid.New()

// memoryApp mounts the memory routes the way the container does.
func memoryApp(t *testing.T, service memorydomain.Service) *fiber.App {
	t.Helper()

	app := fiber.New(fiber.Config{DisableStartupMessage: true})

	handlers := &Handlers{}
	if service != nil {
		handlers.Memory = memoryhandler.New(service, zap.NewNop())
	}

	tenant := app.Group("/api", func(ctx *fiber.Ctx) error {
		userID := uuid.New()
		middleware.SetIdentity(ctx, userID, "owner@example.com")
		middleware.SetScope(ctx, database.Scope{UserID: userID, WorkspaceID: memoryWorkspaceID})
		middleware.SetRole(ctx, "owner")
		return ctx.Next()
	})

	if handlers.Memory == nil {
		registerMemoryFallback(tenant)
	} else {
		memoryhandler.Routes(tenant, handlers.Memory)
	}

	return app
}

func textBody(markdown string) chatdomain.Block {
	return chatdomain.Block{
		Type: chatdomain.BlockText,
		Body: []byte(`{"type":"text","markdown":"` + markdown + `"}`),
	}
}

// recordingAuditRepository records the entries written through the approval
// port. It implements only that one method because it is used only as an
// Auditor; a full repository double would be a second copy of the approval
// module's contract.
type recordingAuditRepository struct {
	entries []approvaldomain.AuditEntry
}

func (r *recordingAuditRepository) RecordAudit(_ context.Context, scope approvaldomain.Scope, entry approvaldomain.AuditEntry) error {
	entry.ActorUserID = scope.UserID
	r.entries = append(r.entries, entry)
	return nil
}

// Compile-time check: the adapter takes the port the memory module declares, and
// the approval repository is what the container hands it.
var (
	_ memorydomain.Auditor      = memoryAuditor{}
	_ approvalAuditRecorder     = (*recordingAuditRepository)(nil)
	_ approvaldomain.Repository = (approvaldomain.Repository)(nil)
)

// approvalAuditRecorder is the one method of the approval repository this adapter
// uses. Naming it separately is what lets the double stay a double: a full
// repository would be a second copy of the approval module's contract.
type approvalAuditRecorder interface {
	RecordAudit(ctx context.Context, scope approvaldomain.Scope, entry approvaldomain.AuditEntry) error
}

// recordingWorker records what a registration installed.
type recordingWorker struct {
	workflows  []any
	activities []any
}

func (w *recordingWorker) RegisterWorkflow(workflowFn any) {
	w.workflows = append(w.workflows, workflowFn)
}

func (w *recordingWorker) RegisterActivity(activityFn any) {
	w.activities = append(w.activities, activityFn)
}
