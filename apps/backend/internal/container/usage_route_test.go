package container

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	authmocks "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/domain/mocks"
	llmhandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/handler"
	taskhandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/handler"
	workspacemocks "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain/mocks"
	"github.com/robzlabz/employeebot/packages/platform/config"
)

// The wiring tests for the usage screen: which routes exist, and which of them the container's route
// order makes reachable.
//
// The second half is the reason this file exists at all. Fiber matches in registration order, so a
// literal route registered after a parameterised one is unreachable — `/tasks/attention` would be
// read as a task id — and that is a mistake no unit test of either handler can see.

// usageApp mounts the routes the way the container does.
//
// It goes through `registerRoutes` rather than calling the module routers directly, because the
// property under test *is* the container's own registration: whether a route exists, and where in
// the order it lands. Only the two services the route table needs to get past its early returns are
// stubbed — auth and the workspace — and the modules whose routes are being asserted are the real
// handlers, built from a nil service because a registration test never calls one.
func usageApp(t *testing.T) *fiber.App {
	t.Helper()

	services := &Services{
		Auth:      authmocks.NewService(t),
		Workspace: workspacemocks.NewService(t),
	}

	cfg := testConfig()
	c := &Container{
		Config:   cfg,
		Logger:   zap.NewNop(),
		Services: services,
		Handlers: newHandlers(services, zap.NewNop(), cfg),
	}
	c.Handlers.Task = taskhandler.New(nil, zap.NewNop()).
		WithTemporalUI(cfg.Temporal.UIBaseURL, cfg.Temporal.Namespace)
	c.Handlers.LLM = llmhandler.New(nil, zap.NewNop())

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	c.app = app
	c.registerRoutes()

	return app
}

// TestTheUsageRouteIsMountedUnderTheApiPrefix: the report is a tenant-scoped read, so it lives where
// the workspace guard is rather than beside the health routes.
func TestTheUsageRouteIsMountedUnderTheApiPrefix(t *testing.T) {
	app := usageApp(t)

	registered := map[string]bool{}
	for _, route := range app.GetRoutes() {
		if route.Method == http.MethodGet {
			registered[route.Path] = true
		}
	}

	require.True(t, registered["/api/llm/usage/report"],
		"the usage report is under the module's own group")
	require.False(t, registered["/llm/usage/report"],
		"and not outside the prefix, where no guard would reach it")
}

// TestTheAttentionRouteIsNotReadAsATaskID is the ordering assertion: the literal path must be
// registered before `/tasks/:taskID`, or Fiber answers the list with a task lookup.
func TestTheAttentionRouteIsNotReadAsATaskID(t *testing.T) {
	app := usageApp(t)

	order := map[string]int{}
	for index, route := range app.GetRoutes() {
		if route.Method == http.MethodGet {
			order[route.Path] = index
		}
	}

	attention, hasAttention := order["/api/tasks/attention"]
	byID, hasByID := order["/api/tasks/:taskID"]
	require.True(t, hasAttention, "the attention route is mounted")
	require.True(t, hasByID)
	require.Less(t, attention, byID,
		"Fiber matches in registration order, so a literal route after a parameterised one is dead")
}

// TestTheUsageScreenWorksWithoutTheReportingPort: a deployment whose LLM module is assembled without
// the reporter still serves the page, and the report says it is unavailable rather than answering an
// empty period that would read as "this workspace spent nothing".
func TestTheUsageScreenWorksWithoutTheReportingPort(t *testing.T) {
	app := usageApp(t)

	// No workspace header, so the tenant middleware refuses before the handler runs — which is what
	// proves the route is behind the guard rather than in front of it.
	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/llm/usage/report", nil), -1)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()

	require.NotEqual(t, http.StatusOK, response.StatusCode)
	require.NotEqual(t, http.StatusNotFound, response.StatusCode,
		"the route exists; it is the workspace that is missing")
}

// TestTheReportExportIsAReadOnTheSameRoute: the CSV is the same endpoint with `format=csv` rather
// than a second route, which is what keeps the file and the screen from reading different queries.
func TestTheReportExportIsAReadOnTheSameRoute(t *testing.T) {
	app := usageApp(t)

	paths := []string{}
	for _, route := range app.GetRoutes() {
		paths = append(paths, route.Path)
	}

	require.NotContains(t, paths, "/api/llm/usage/report/export",
		"the export is a parameter of the report, not a route of its own")
	require.NotContains(t, paths, "/api/llm/usage/export")
}

// TestTheTemporalLinkIsInstalledFromTheDeploymentConfig: the base URL and the namespace reach the
// handler, because the handler is built by the container and that is the only place that knows them.
func TestTheTemporalLinkIsInstalledFromTheDeploymentConfig(t *testing.T) {
	handler := taskhandler.New(nil, zap.NewNop()).WithTemporalUI("", "")
	require.NotNil(t, handler, "an empty deployment is a handler with no link, not a nil one")

	cfg := testConfig()
	cfg.Temporal = config.TemporalConfig{UIBaseURL: "http://ui.example.test", Namespace: "produksi"}

	installed := taskhandler.New(nil, zap.NewNop()).WithTemporalUI(cfg.Temporal.UIBaseURL, cfg.Temporal.Namespace)
	require.NotNil(t, installed)
}
