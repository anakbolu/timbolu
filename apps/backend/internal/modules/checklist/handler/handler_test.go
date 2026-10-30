package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/checklist/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/checklist/domain/mocks"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/middleware"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/response"
	"github.com/anakbolu/timbolu/packages/platform/database"
)

// This file is the wire and nothing else: the fields a screen reads, the query a filter becomes, and
// the status and code it branches on. What a plan means is the service's business and is tested there.

// newTestApp mounts the routes behind the identity the container's middlewares would have put on the
// request: the authenticated user, the verified tenant scope, and the workspace id the request carries.
func newTestApp(t *testing.T, service domain.Service, workspaceID uuid.UUID) *fiber.App {
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
		// The request context caches the workspace the request named, and the handler reads that local
		// rather than the scope. The key is unexported, so the shape here is the one the accessor reads.
		c.Locals("workspace_id", workspaceID.String())

		return c.Next()
	})

	Routes(group, New(service, zap.NewNop()))

	return app
}

// do runs one request and returns what came back.
func do(t *testing.T, app *fiber.App, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(fiber.MethodGet, path, nil)
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	recorder := httptest.NewRecorder()
	recorder.Code = resp.StatusCode
	for name, values := range resp.Header {
		for _, value := range values {
			recorder.Header().Add(name, value)
		}
	}
	if content, err := io.ReadAll(resp.Body); err == nil {
		recorder.Body.Write(content)
	}

	return recorder
}

// envelope reads the response as the shape every screen branches on.
type envelope struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
	Meta    struct {
		Count int `json:"count"`
	} `json:"meta"`
	Error *struct {
		Code string `json:"code"`
	} `json:"error"`
}

// decode reads one response body.
func decode(t *testing.T, recorder *httptest.ResponseRecorder) envelope {
	t.Helper()

	var body envelope
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))

	return body
}

// asWorkspace matches a call that ran as the tenant the request named and as a real user. A handler that
// passed a zero workspace would read another workspace's plans or none at all, and the only place that
// is visible is here.
func asWorkspace(workspaceID uuid.UUID) interface{} {
	return mock.MatchedBy(func(scope domain.Scope) bool {
		return scope.WorkspaceID == workspaceID && scope.UserID != uuid.Nil
	})
}

// planFixture is one plan as the module holds it: two tasks, one of them ticked with a subtask under it.
func planFixture() domain.Checklist {
	completed := time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC)
	id := uuid.New()

	return domain.Checklist{
		ID:          id,
		AgentID:     uuid.New(),
		TaskID:      uuid.New(),
		Title:       "Tagih pelanggan",
		Status:      domain.StatusDone,
		CompletedAt: &completed,
		CreatedAt:   time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC),
		Items: []domain.Item{
			{
				ID: uuid.New(), ChecklistID: id, Title: "cari harga", Done: true,
				Subtasks: []domain.Item{{ID: uuid.New(), ChecklistID: id, Title: "buka katalog", Done: true}},
			},
			{ID: uuid.New(), ChecklistID: id, Title: "kirim tagihan"},
		},
	}
}

// TestTheListAnswersWithThePlanAndItsTree: the screen renders tasks and subtasks from this payload, so
// the nesting and the counts travel with it — a screen that had to count for itself would disagree with
// the tool about how far along the plan is.
func TestTheListAnswersWithThePlanAndItsTree(t *testing.T) {
	workspaceID := uuid.New()
	service := mocks.NewService(t)
	service.EXPECT().List(mock.Anything, asWorkspace(workspaceID), mock.Anything).
		Return([]domain.Checklist{planFixture()}, nil)

	recorder := do(t, newTestApp(t, service, workspaceID), "/api/checklists")
	require.Equal(t, fiber.StatusOK, recorder.Code)

	body := decode(t, recorder)
	require.True(t, body.Success)
	require.Equal(t, 1, body.Meta.Count)

	var payload []map[string]any
	require.NoError(t, json.Unmarshal(body.Data, &payload))
	require.Len(t, payload, 1)

	plan := payload[0]
	require.Equal(t, "Tagih pelanggan", plan["title"])
	require.Equal(t, "done", plan["status"])
	require.Equal(t, float64(2), plan["done_count"])
	require.Equal(t, float64(3), plan["total_count"])
	require.NotEmpty(t, plan["completed_at"])

	items := plan["items"].([]any)
	require.Len(t, items, 2)
	require.Equal(t, "cari harga", items[0].(map[string]any)["title"])
	require.Equal(t, true, items[0].(map[string]any)["done"])
	require.Equal(t, "buka katalog", items[0].(map[string]any)["subtasks"].([]any)[0].(map[string]any)["title"])
	require.Equal(t, false, items[1].(map[string]any)["done"])
	require.Nil(t, items[1].(map[string]any)["subtasks"], "a task with no subtasks carries none")
}

// TestAnEmptyWorkspaceAnswersWithAnEmptyList: the shape is the same whether or not there are plans, so a
// screen does not have to branch on null before rendering.
func TestAnEmptyWorkspaceAnswersWithAnEmptyList(t *testing.T) {
	workspaceID := uuid.New()
	service := mocks.NewService(t)
	service.EXPECT().List(mock.Anything, asWorkspace(workspaceID), mock.Anything).
		Return([]domain.Checklist{}, nil)

	recorder := do(t, newTestApp(t, service, workspaceID), "/api/checklists")
	require.Equal(t, fiber.StatusOK, recorder.Code)

	body := decode(t, recorder)
	require.Equal(t, 0, body.Meta.Count)
	require.JSONEq(t, `[]`, string(body.Data))
}

// TestTheFiltersAreTheQuery: what a person narrows to is what the database is asked for. A screen that
// filtered a page in the browser would answer "no plans" while the plans sat on the next page.
func TestTheFiltersAreTheQuery(t *testing.T) {
	workspaceID := uuid.New()
	agentID := uuid.New()

	service := mocks.NewService(t)
	service.EXPECT().List(mock.Anything, asWorkspace(workspaceID), mock.MatchedBy(func(filter domain.Filter) bool {
		return filter.AgentID == agentID && filter.Status == domain.StatusActive && filter.Limit == 5
	})).Return([]domain.Checklist{}, nil)

	recorder := do(t, newTestApp(t, service, workspaceID),
		"/api/checklists?agent_id="+agentID.String()+"&status=active&limit=5")
	require.Equal(t, fiber.StatusOK, recorder.Code)
}

// TestAnAgentIdThatIsNotAUUIDIsRefused: it is a client bug rather than an empty result, and passing it
// down as no filter would quietly answer with every Bolu's plan.
func TestAnAgentIdThatIsNotAUUIDIsRefused(t *testing.T) {
	workspaceID := uuid.New()

	recorder := do(t, newTestApp(t, mocks.NewService(t), workspaceID), "/api/checklists?agent_id=oren")
	require.Equal(t, fiber.StatusBadRequest, recorder.Code)

	body := decode(t, recorder)
	require.False(t, body.Success)
	require.Equal(t, "invalid_input", body.Error.Code)
}

// TestARejectedFilterIsABadRequest: the module's own refusal is the caller's mistake, so it is a 400
// with the reason rather than a 500 that reads as "we broke".
func TestARejectedFilterIsABadRequest(t *testing.T) {
	workspaceID := uuid.New()
	service := mocks.NewService(t)
	service.EXPECT().List(mock.Anything, asWorkspace(workspaceID), mock.Anything).
		Return(nil, domain.ErrInvalidInput)

	recorder := do(t, newTestApp(t, service, workspaceID), "/api/checklists?status=abandoned")
	require.Equal(t, fiber.StatusBadRequest, recorder.Code)
	require.Equal(t, "invalid_input", decode(t, recorder).Error.Code)
}

// TestAStorageFailureIsAnInternalError: a failure that is not the caller's mistake is not described to
// them, and it is logged rather than answered with the driver's own words.
func TestAStorageFailureIsAnInternalError(t *testing.T) {
	workspaceID := uuid.New()
	service := mocks.NewService(t)
	service.EXPECT().List(mock.Anything, asWorkspace(workspaceID), mock.Anything).
		Return(nil, errors.New("the database is on fire"))

	recorder := do(t, newTestApp(t, service, workspaceID), "/api/checklists")
	require.Equal(t, fiber.StatusInternalServerError, recorder.Code)

	body := decode(t, recorder)
	require.False(t, body.Success)
	require.Equal(t, "internal_error", body.Error.Code)
	require.NotContains(t, recorder.Body.String(), "on fire")
}
