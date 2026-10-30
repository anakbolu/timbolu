package container

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	agentdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/agent/domain"
	agentmocks "github.com/anakbolu/timbolu/apps/backend/internal/modules/agent/domain/mocks"
	agenthandler "github.com/anakbolu/timbolu/apps/backend/internal/modules/agent/handler"
	authmocks "github.com/anakbolu/timbolu/apps/backend/internal/modules/auth/domain/mocks"
	workspacedomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/workspace/domain"
	workspacemocks "github.com/anakbolu/timbolu/apps/backend/internal/modules/workspace/domain/mocks"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/middleware"
)

// The wiring test for the role groups: which role a route needs is decided by where it is registered,
// not by what it is called.
//
// It exists because Fiber applies a group's middleware to every route registered *after* it, whichever
// group registers that route. The managing-role and owner groups are therefore registered after every
// read a member may make, and the assertions below are what keep them there: a member's sidebar is
// built from `/teams` and `/agents`, and a group created one line too early turns those into 403s that
// no unit test of either handler can see.

// memberApp mounts the real route table behind an identity whose role the caller chooses.
//
// The services the table needs are stubs: auth is never called because the identity is set before the
// container's middleware runs, the workspace answers the membership lookup with the role under test,
// and the registry answers empty lists. Every route is registered by the real container, which is what
// is under test — the middleware in front of a route is the container's decision.
func memberApp(t *testing.T, role string) (*fiber.App, uuid.UUID) {
	t.Helper()

	workspaceID := uuid.New()

	auth := authmocks.NewService(t)
	workspace := workspacemocks.NewService(t)
	workspace.On("Resolve", mock.Anything, workspaceID, mock.Anything).Return(role, nil)
	workspace.On("Current", mock.Anything, mock.Anything).
		Return(workspacedomain.Workspace{ID: workspaceID, Name: "Toko Uji", Role: role}, nil).Maybe()

	agents := agentmocks.NewService(t)
	agents.On("Teams", mock.Anything, mock.Anything).Return([]agentdomain.Team{}, nil).Maybe()
	agents.On("List", mock.Anything, mock.Anything).Return([]agentdomain.Agent{}, nil).Maybe()

	services := &Services{Auth: auth, Workspace: workspace, Agent: agents}
	cfg := testConfig()
	c := &Container{
		Config:   cfg,
		Logger:   zap.NewNop(),
		Services: services,
		Handlers: newHandlers(services, zap.NewNop(), cfg),
	}
	c.Handlers.Agent = agenthandler.New(agents, zap.NewNop())

	app := fiber.New(fiber.Config{DisableStartupMessage: true})

	// The identity is set before the container's own middleware runs, which is what lets the
	// authentication middleware accept it instead of resolving a token. The tenant middleware still
	// runs in full: it is the one that asks the workspace service for the role.
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.SetIdentity(ctx, uuid.New(), "member@example.com")
		return ctx.Next()
	})

	c.app = app
	c.registerRoutes()

	return app, workspaceID
}

// request builds one call carrying the active workspace, the way the web app does.
func request(t *testing.T, app *fiber.App, method, path string, workspaceID uuid.UUID) int {
	t.Helper()

	req := httptest.NewRequest(method, path, nil)
	req.Header.Set(HeaderWorkspaceID, workspaceID.String())

	response, err := app.Test(req, -1)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())

	return response.StatusCode
}

// TestAMemberMayReadTheWorkspace is the whole point of the ordering: the sidebar is built from the
// registry and the teams, and both are read by every member of the workspace.
func TestAMemberMayReadTheWorkspace(t *testing.T) {
	app, workspaceID := memberApp(t, "member")

	for _, path := range []string{"/api/teams", "/api/agents", "/api/workspaces/current"} {
		require.Equal(t, http.StatusOK, request(t, app, http.MethodGet, path, workspaceID),
			"%s must be readable by a member, not only by a managing role", path)
	}
}

// TestAMemberMayNotArrangeTheRoster is the other half: the drag changes the workspace's registry, so
// it sits on the managing group with the other writes.
func TestAMemberMayNotArrangeTheRoster(t *testing.T) {
	app, workspaceID := memberApp(t, "member")

	require.Equal(t, http.StatusForbidden,
		request(t, app, http.MethodPut, "/api/agents/order", workspaceID))
}

// TestAManagingRoleMayArrangeTheRoster keeps the gate from being a wall: the same request passes the
// role check for an owner and is answered by the handler itself, whose own parsing refuses the empty
// body.
func TestAManagingRoleMayArrangeTheRoster(t *testing.T) {
	app, workspaceID := memberApp(t, "owner")

	require.Equal(t, http.StatusBadRequest,
		request(t, app, http.MethodPut, "/api/agents/order", workspaceID))
}
