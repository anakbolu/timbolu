package container

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	agentdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/domain"
	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	taskdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	taskhandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/handler"
	taskservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/service"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/config"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
)

// The wiring tests cover what only the container can get wrong: which routes
// exist, what they answer when the module is not configured, and whether the
// adapters between the task runtime and the other modules hold.

// TestOpenTaskWithoutADatabaseLeavesTheModuleUnwired keeps the container's
// "running without Postgres" contract: the routes answer 503 rather than
// panicking, and the worker starts without the runtime.
func TestOpenTaskWithoutADatabaseLeavesTheModuleUnwired(t *testing.T) {
	c := &Container{
		Config:       testConfig(),
		Logger:       zap.NewNop(),
		Repositories: &Repositories{},
		Services:     &Services{},
	}

	require.NoError(t, c.openTask(context.Background(), c.Config))
	require.Nil(t, c.Services.Task, "the service stays nil, so its routes report not configured")
	require.Nil(t, c.Tasks, "a worker without the runtime registers the probe and nothing else")
}

// TestTaskRoutesAnswer503WhenTheRuntimeIsMissing is the honest answer for a
// deployment without a database: every documented task route exists and says the
// feature is unavailable, rather than answering 404 and looking like a typo.
func TestTaskRoutesAnswer503WhenTheRuntimeIsMissing(t *testing.T) {
	app := taskApp(t, nil)

	for _, path := range []string{
		"/api/tasks",
		"/api/tasks/" + uuid.NewString(),
		"/api/tasks/" + uuid.NewString() + "/steps",
		"/api/tasks/" + uuid.NewString() + "/children",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		resp, err := app.Test(req, -1)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		require.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode, path)
		require.Contains(t, string(body), "task_not_configured", path)
	}
}

// TestTaskRoutesAreMountedUnderTheTenantGroup proves the endpoints exist once the
// runtime is configured, and that the scope the handler passes on is the one the
// middleware resolved.
func TestTaskRoutesAreMountedUnderTheTenantGroup(t *testing.T) {
	service := &recordingTaskService{}
	app := taskApp(t, service)

	req := httptest.NewRequest(http.MethodGet, "/api/tasks?status=succeeded", nil)
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "succeeded", service.filter.Status)
	require.Equal(t, taskWorkspaceID, service.scope.WorkspaceID,
		"the handler runs as the workspace the tenant middleware resolved")
}

// TestTaskAgentAdapterCarriesTheRegistryFields is the adapter contract: the task
// runtime sees the Bolu the registry owns, including the rest switch, which is
// what makes a resting Bolu refuse new work.
func TestTaskAgentAdapterCarriesTheRegistryFields(t *testing.T) {
	agent := agentdomain.Agent{
		ID:      uuid.New(),
		Name:    "Ijo",
		Role:    "Gudang",
		Persona: "Rapi.",
		Tone:    "tenang",
		Status:  agentdomain.StoredResting,
		Display: agentdomain.DisplayResting,
		Tools:   []string{"drive.search"},
	}

	ref := toTaskAgentRef(agent)

	require.Equal(t, agent.ID, ref.ID)
	require.Equal(t, agent.Name, ref.Name)
	require.Equal(t, agent.Status, ref.Stored)
	require.Equal(t, agent.Display, ref.Display)
	require.False(t, ref.Active(), "a resting Bolu never accepts new work")
	require.True(t, taskAgentDirectory{}.Active(taskdomain.AgentRef{Stored: agentdomain.StoredActive}))
}

// TestTaskMessageAdapterKeepsTheBlockBodies is the reply contract: a block the
// task runtime writes is stored exactly as the chat module stores its own, so a
// task's answer renders like any other.
func TestTaskMessageAdapterKeepsTheBlockBodies(t *testing.T) {
	body := json.RawMessage(`{"type":"text","markdown":"Ada 3 pesanan."}`)
	message := chatdomain.Message{
		ID:             uuid.New(),
		ConversationID: uuid.New(),
		AuthorAgentID:  uuid.New(),
		Blocks:         []chatdomain.Block{{Type: chatdomain.BlockText, Body: body}},
	}

	converted := toTaskMessage(message)
	require.Len(t, converted.Blocks, 1)
	require.Equal(t, chatdomain.BlockText, converted.Blocks[0].Type)
	require.JSONEq(t, string(body), string(converted.Blocks[0].Body))
	require.Equal(t, message.AuthorAgentID, converted.AgentID)
	require.True(t, converted.FromAgent(), "a Bolu's answer must not read as the user's own turn")

	back := toChatBlocks(converted.Blocks)
	require.Len(t, back, 1)
	require.JSONEq(t, string(body), string(back[0].Body))
}

// TestChatTaskStarterForwardsTheTrigger covers the seam: what the chat module
// calls a message becomes what the runtime calls a task, with the trigger that
// says where it came from.
func TestChatTaskStarterForwardsTheTrigger(t *testing.T) {
	service := &recordingTaskService{}
	starter := chatTaskStarter{service: service}

	scope := chatdomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}
	agentID := uuid.New()
	conversationID := uuid.New()
	messageID := uuid.New()

	taskID, err := starter.Start(context.Background(), scope, chatdomain.StartRequest{
		ConversationID: conversationID,
		MessageID:      messageID,
		AgentID:        agentID,
		Trigger:        chatdomain.TriggerChat,
		Title:          "Rekap pesanan",
	})
	require.NoError(t, err)
	require.Equal(t, service.taskID, taskID)

	require.Equal(t, scope.WorkspaceID, service.scope.WorkspaceID)
	require.Equal(t, agentID, service.dispatch.AgentID)
	require.Equal(t, conversationID, service.dispatch.ConversationID)
	require.Equal(t, messageID, service.dispatch.ReplyMessageID,
		"the task fills the placeholder the chat module wrote")
	require.Equal(t, taskdomain.TriggerChat, service.dispatch.Trigger)
	require.Equal(t, "Rekap pesanan", service.dispatch.Prompt,
		"the task is asked the text the user wrote, not only the title")
}

// taskWorkspaceID is the workspace the mounted route table resolves.
var taskWorkspaceID = uuid.New()

// taskApp mounts the task routes the way the container does.
func taskApp(t *testing.T, service taskdomain.Service) *fiber.App {
	t.Helper()

	app := fiber.New(fiber.Config{DisableStartupMessage: true})

	handlers := &Handlers{}
	if service != nil {
		handlers.Task = taskhandler.New(service, zap.NewNop())
	}

	c := &Container{
		Config:   testConfig(),
		Logger:   zap.NewNop(),
		Services: &Services{Task: service},
		Handlers: handlers,
	}
	c.app = app

	// The tenant middleware is stood in for: what matters here is that the routes
	// exist under the authenticated group and that the handler reads the scope
	// from it.
	tenant := app.Group("/api", func(ctx *fiber.Ctx) error {
		userID := uuid.New()
		middleware.SetIdentity(ctx, userID, "owner@example.com")
		middleware.SetScope(ctx, database.Scope{UserID: userID, WorkspaceID: taskWorkspaceID})
		middleware.SetRole(ctx, "owner")
		return ctx.Next()
	})

	if c.Handlers.Task == nil {
		registerTaskFallback(tenant)
	} else {
		taskhandler.Routes(tenant, c.Handlers.Task)
	}

	return app
}

// recordingTaskService records what the handler asked of the runtime.
type recordingTaskService struct {
	taskID   uuid.UUID
	scope    taskdomain.Scope
	filter   taskdomain.Filter
	dispatch taskdomain.DispatchRequest
}

func (s *recordingTaskService) Dispatch(_ context.Context, scope taskdomain.Scope, req taskdomain.DispatchRequest) (taskdomain.Task, error) {
	s.scope = scope
	s.dispatch = req
	s.taskID = uuid.New()
	return taskdomain.Task{ID: s.taskID, Status: taskdomain.StatusQueued}, nil
}

func (s *recordingTaskService) Get(_ context.Context, scope taskdomain.Scope, id uuid.UUID) (taskdomain.Task, error) {
	s.scope = scope
	return taskdomain.Task{ID: id, Status: taskdomain.StatusRunning}, nil
}

func (s *recordingTaskService) History(_ context.Context, scope taskdomain.Scope, filter taskdomain.Filter) ([]taskdomain.Task, error) {
	s.scope = scope
	s.filter = filter
	return nil, nil
}

func (s *recordingTaskService) Steps(_ context.Context, scope taskdomain.Scope, _ uuid.UUID) ([]taskdomain.Step, error) {
	s.scope = scope
	return nil, nil
}

func (s *recordingTaskService) Children(_ context.Context, scope taskdomain.Scope, _ uuid.UUID) ([]taskdomain.Task, error) {
	s.scope = scope
	return nil, nil
}

func (s *recordingTaskService) Cancel(_ context.Context, scope taskdomain.Scope, id uuid.UUID) (taskdomain.Task, error) {
	s.scope = scope
	return taskdomain.Task{ID: id, Status: taskdomain.StatusCanceled}, nil
}

// Compile-time checks that the recording service is the interface the handler
// takes, and that the runtime's own service satisfies it too.
var (
	_ taskdomain.Service = (*recordingTaskService)(nil)
	_ taskdomain.Service = (*taskservice.Service)(nil)
	_                    = config.Config{}
)
