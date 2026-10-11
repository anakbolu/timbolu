package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
)

// The handler tests cover the task endpoints' shape: what a client reads, which
// filters reach the service, and which error becomes which status code.

// stubService records what the handler asked of the runtime.
type stubService struct {
	tasks    []domain.Task
	steps    []domain.Step
	task     domain.Task
	err      error
	history  domain.Filter
	gotSteps uuid.UUID
	gotID    uuid.UUID
	canceled bool
}

func (s *stubService) Dispatch(context.Context, domain.Scope, domain.DispatchRequest) (domain.Task, error) {
	return domain.Task{}, errors.New("not used")
}

func (s *stubService) Get(_ context.Context, _ domain.Scope, id uuid.UUID) (domain.Task, error) {
	s.gotID = id
	return s.task, s.err
}

func (s *stubService) History(_ context.Context, _ domain.Scope, filter domain.Filter) ([]domain.Task, error) {
	s.history = filter
	return s.tasks, s.err
}

func (s *stubService) Steps(_ context.Context, _ domain.Scope, taskID uuid.UUID) ([]domain.Step, error) {
	s.gotSteps = taskID
	return s.steps, s.err
}

func (s *stubService) Children(_ context.Context, _ domain.Scope, taskID uuid.UUID) ([]domain.Task, error) {
	s.gotID = taskID
	return s.tasks, s.err
}

func (s *stubService) Cancel(_ context.Context, _ domain.Scope, id uuid.UUID) (domain.Task, error) {
	s.canceled = true
	s.gotID = id
	return s.task, s.err
}

func newApp(t *testing.T, service domain.Service, workspaceID uuid.UUID) *fiber.App {
	t.Helper()

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			var fiberErr *fiber.Error
			code := fiber.StatusInternalServerError
			if errors.As(err, &fiberErr) {
				code = fiberErr.Code
			}
			return response.Error(c, code, err.Error(), "http_error", nil)
		},
	})

	group := app.Group("/api", func(c *fiber.Ctx) error {
		userID := uuid.New()
		middleware.SetIdentity(c, userID, "owner@example.com")
		middleware.SetScope(c, database.Scope{UserID: userID, WorkspaceID: workspaceID})
		middleware.SetRole(c, "owner")
		return c.Next()
	})

	Routes(group, New(service, zap.NewNop()))

	return app
}

func get(t *testing.T, app *fiber.App, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var envelope map[string]any
	require.NoError(t, json.Unmarshal(body, &envelope))

	return &httptest.ResponseRecorder{Code: resp.StatusCode}, envelope
}

func post(t *testing.T, app *fiber.App, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, path, nil)
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var envelope map[string]any
	require.NoError(t, json.Unmarshal(body, &envelope))

	return &httptest.ResponseRecorder{Code: resp.StatusCode}, envelope
}

func sampleTask() domain.Task {
	now := time.Now()
	heartbeat := now.Add(-time.Minute)

	return domain.Task{
		ID:             uuid.New(),
		WorkspaceID:    uuid.New(),
		AgentID:        uuid.New(),
		ConversationID: uuid.New(),
		Trigger:        domain.TriggerChat,
		Title:          "Rekap pesanan",
		Status:         domain.StatusRunning,
		WorkflowID:     "task-" + uuid.NewString(),
		Depth:          1,
		Summary:        "3 pesanan",
		StepCount:      2,
		InputTokens:    120,
		OutputTokens:   30,
		CostMicros:     4200,
		HeartbeatAt:    &heartbeat,
		CreatedAt:      now,
	}
}

// TestHistoryReturnsTheWorkspaceTasks covers the list endpoint: the payload
// carries the derived health, and the filters reach the service rather than
// being dropped.
func TestHistoryReturnsTheWorkspaceTasks(t *testing.T) {
	service := &stubService{tasks: []domain.Task{sampleTask()}}
	app := newApp(t, service, uuid.New())

	agentID := uuid.New()
	recorder, envelope := get(t, app,
		"/api/tasks?status=running&live=true&limit=5&agent_id="+agentID.String())

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, true, envelope["success"])
	require.Equal(t, domain.StatusRunning, service.history.Status)
	require.True(t, service.history.Live)
	require.Equal(t, 5, service.history.Limit)
	require.Equal(t, agentID, service.history.AgentID)

	data, ok := envelope["data"].([]any)
	require.True(t, ok, "the data field must be a list")
	require.Len(t, data, 1)

	first, ok := data[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "running", first["health"], "the health mark is derived from the heartbeat")
	require.Equal(t, float64(1), first["depth"])
	require.Equal(t, "3 pesanan", first["summary"])
}

// TestHistoryRefusesAMalformedFilter covers the query guard: a filter that is not
// a uuid is refused rather than silently ignored, which would return the wrong
// tasks.
func TestHistoryRefusesAMalformedFilter(t *testing.T) {
	service := &stubService{}
	app := newApp(t, service, uuid.New())

	recorder, envelope := get(t, app, "/api/tasks?agent_id=bukan-uuid")

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Equal(t, false, envelope["success"])
}

// TestGetReturnsOneTask covers the read a client follows a task with.
func TestGetReturnsOneTask(t *testing.T) {
	task := sampleTask()
	service := &stubService{task: task}
	app := newApp(t, service, uuid.New())

	recorder, envelope := get(t, app, "/api/tasks/"+task.ID.String())

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, task.ID, service.gotID)

	data, ok := envelope["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, task.ID.String(), data["task_id"])
	require.Equal(t, "chat", data["trigger"])
	require.Equal(t, float64(120), data["input_tokens"])
	require.NotEmpty(t, data["workflow_id"], "the workflow id is what a restart resumes with")
}

// TestGetMapsAMissingTaskTo404 is the error mapping: a task of another workspace
// answers not found rather than an empty object.
func TestGetMapsAMissingTaskTo404(t *testing.T) {
	service := &stubService{err: domain.ErrTaskNotFound}
	app := newApp(t, service, uuid.New())

	recorder, envelope := get(t, app, "/api/tasks/"+uuid.NewString())

	require.Equal(t, http.StatusNotFound, recorder.Code)
	require.Equal(t, "task_not_found", envelope["error"].(map[string]any)["code"])
}

// TestStepsKeepsAPrunedStepReadable is the E6.6 shape: a step whose payload the
// retention job removed keeps its kind and sequence and loses its contents.
func TestStepsKeepsAPrunedStepReadable(t *testing.T) {
	task := sampleTask()
	service := &stubService{steps: []domain.Step{
		{ID: uuid.New(), TaskID: task.ID, Seq: 1, Kind: domain.StepThink, Output: json.RawMessage(`{"text":"cek"}`)},
		{ID: uuid.New(), TaskID: task.ID, Seq: 2, Kind: domain.StepFinal, Pruned: true, Output: json.RawMessage(`{"text":"rahasia"}`)},
	}}
	app := newApp(t, service, uuid.New())

	recorder, envelope := get(t, app, "/api/tasks/"+task.ID.String()+"/steps")

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, task.ID, service.gotSteps)

	data, ok := envelope["data"].([]any)
	require.True(t, ok)
	require.Len(t, data, 2)

	first := data[0].(map[string]any)
	require.Equal(t, float64(1), first["seq"])
	require.Equal(t, "think", first["kind"])
	require.NotNil(t, first["output"])

	second := data[1].(map[string]any)
	require.Equal(t, true, second["pruned"])
	require.Nil(t, second["output"], "a pruned step keeps its shape and loses its contents")
}

// TestChildrenReturnsTheHandoffChain covers the read that follows a chain of
// handoffs from its root.
func TestChildrenReturnsTheHandoffChain(t *testing.T) {
	parent := sampleTask()
	child := sampleTask()
	child.ParentTaskID = parent.ID
	child.Depth = parent.Depth + 1

	service := &stubService{tasks: []domain.Task{child}}
	app := newApp(t, service, uuid.New())

	recorder, envelope := get(t, app, "/api/tasks/"+parent.ID.String()+"/children")

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, parent.ID, service.gotID)

	data, ok := envelope["data"].([]any)
	require.True(t, ok)
	require.Len(t, data, 1)
	require.Equal(t, float64(child.Depth), data[0].(map[string]any)["depth"])
}

// TestCancelStopsATask covers the one write the endpoints expose.
func TestCancelStopsATask(t *testing.T) {
	task := sampleTask()
	task.Status = domain.StatusCanceled
	service := &stubService{task: task}
	app := newApp(t, service, uuid.New())

	recorder, envelope := post(t, app, "/api/tasks/"+task.ID.String()+"/cancel")

	require.Equal(t, http.StatusOK, recorder.Code)
	require.True(t, service.canceled)
	require.Equal(t, "canceled", envelope["data"].(map[string]any)["status"])
}

// TestCancelRefusesAFinishedTask maps the conflict: a task the user already saw
// finish is not canceled afterwards.
func TestCancelRefusesAFinishedTask(t *testing.T) {
	service := &stubService{err: domain.ErrInvalidInput}
	app := newApp(t, service, uuid.New())

	recorder, envelope := post(t, app, "/api/tasks/"+uuid.NewString()+"/cancel")

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Equal(t, "invalid_input", envelope["error"].(map[string]any)["code"])
}

// TestLimitErrorsMapToConflict keeps the bound reasons distinguishable from a
// bad request: a task stopped by a limit is a conflict, not a client mistake.
func TestLimitErrorsMapToConflict(t *testing.T) {
	for _, err := range []error{
		domain.ErrStepLimitReached,
		domain.ErrTokenLimitReached,
		domain.ErrCostLimitReached,
		domain.ErrDepthLimitReached,
	} {
		service := &stubService{err: err}
		app := newApp(t, service, uuid.New())

		recorder, envelope := post(t, app, "/api/tasks/"+uuid.NewString()+"/cancel")
		require.Equal(t, http.StatusConflict, recorder.Code, err)
		require.Equal(t, "task_limit_reached", envelope["error"].(map[string]any)["code"])
	}
}

// TestMalformedTaskIDIsRefused covers the path parameter: a path that is not a
// uuid never reaches the service.
func TestMalformedTaskIDIsRefused(t *testing.T) {
	service := &stubService{}
	app := newApp(t, service, uuid.New())

	recorder, _ := get(t, app, "/api/tasks/bukan-uuid")
	require.Equal(t, http.StatusBadRequest, recorder.Code)

	recorder, _ = get(t, app, "/api/tasks/bukan-uuid/steps")
	require.Equal(t, http.StatusBadRequest, recorder.Code)

	recorder, _ = post(t, app, "/api/tasks/bukan-uuid/cancel")
	require.Equal(t, http.StatusBadRequest, recorder.Code)

	require.Equal(t, uuid.Nil, service.gotID, "a malformed id never reaches the runtime")
}

// TestInternalErrorsDoNotLeakDetail is the safe failure: an unexpected error is
// logged and answered generically, never echoed to the client.
func TestInternalErrorsDoNotLeakDetail(t *testing.T) {
	service := &stubService{err: errors.New("pq: password authentication failed for user \"bolu_app\"")}
	app := newApp(t, service, uuid.New())

	recorder, envelope := get(t, app, "/api/tasks/"+uuid.NewString())

	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	require.Equal(t, "internal server error", envelope["message"])
	require.NotContains(t, envelope["message"], "password")
}
