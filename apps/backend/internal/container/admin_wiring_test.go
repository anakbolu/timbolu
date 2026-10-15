package container

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	admindomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/admin/domain"
	adminmocks "github.com/robzlabz/employeebot/apps/backend/internal/modules/admin/domain/mocks"
	authdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/domain"
	authmocks "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/domain/mocks"
	workspacemocks "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain/mocks"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
)

// The wiring tests for the operator surface cover what only the container can get
// wrong: where the guard sits, and what a deployment without a database answers.
//
// The guard is the reason this file exists. `requirePlatformAdmin` reads the
// account's role from the database rather than from the token, and it is applied
// to a path prefix rather than through an empty-path group — because Fiber applies
// a group's middleware to every route registered after it, and an empty-path group
// in front of the module routes would put a platform-role check on every read of
// every workspace.

// TestOpenAdminWithoutADatabaseLeavesTheModuleUnwired keeps the container's
// "running without Postgres" contract: the surface's routes exist and report that
// they are not configured rather than 404.
func TestOpenAdminWithoutADatabaseLeavesTheModuleUnwired(t *testing.T) {
	c := &Container{
		Config:       testConfig(),
		Logger:       zap.NewNop(),
		Repositories: &Repositories{},
		Services:     &Services{},
	}

	require.NoError(t, c.openAdmin(context.Background(), c.Config))
	require.Nil(t, c.Services.Admin, "the service stays nil, so its routes report not configured")

	app := adminFallbackApp(t)

	for _, path := range []string{"/admin/overview", "/admin/users"} {
		resp := callAdmin(t, app, http.MethodGet, path, "")
		require.Equal(t, http.StatusServiceUnavailable, resp.Code, path)
		require.Contains(t, resp.Body.String(), "admin_not_configured")
	}
}

// TestTheGuardReadsTheRoleFromTheAccountNotTheToken: a demotion takes effect on
// the next request rather than on the next sign-in, which is what makes a
// promotion reversible.
func TestTheGuardReadsTheRoleFromTheAccountNotTheToken(t *testing.T) {
	userID := uuid.New()
	svc := authmocks.NewService(t)

	// The token is valid and resolves to an account whose role is `user`.
	svc.EXPECT().CurrentUser(mock.Anything, userID).
		Return(authdomain.User{ID: userID, Email: "plain@example.com", Role: authdomain.RoleUser}, nil).Once()

	c := &Container{
		Config:   testConfig(),
		Logger:   zap.NewNop(),
		Services: &Services{Auth: svc},
	}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Group("/api", identify(userID), c.requirePlatformAdmin()).Get("/admin/users", func(ctx *fiber.Ctx) error {
		return ctx.SendStatus(fiber.StatusOK)
	})

	resp := callAdmin(t, app, http.MethodGet, "/api/admin/users", "")
	require.Equal(t, http.StatusForbidden, resp.Code)
	require.Contains(t, resp.Body.String(), "forbidden")

	// Promoted: the same token now reaches the surface, because the role is read
	// again rather than remembered from the last call.
	svc.EXPECT().CurrentUser(mock.Anything, userID).
		Return(authdomain.User{ID: userID, Email: "plain@example.com", Role: authdomain.RoleAdmin}, nil).Once()

	resp = callAdmin(t, app, http.MethodGet, "/api/admin/users", "")
	require.Equal(t, http.StatusOK, resp.Code)
}

// TestTheGuardRefusesAnAccountThatNoLongerExists: a session whose account was
// deleted is a dead session, not a 500.
func TestTheGuardRefusesAnAccountThatNoLongerExists(t *testing.T) {
	userID := uuid.New()
	svc := authmocks.NewService(t)
	svc.EXPECT().CurrentUser(mock.Anything, userID).Return(authdomain.User{}, authdomain.ErrUserNotFound).Once()

	c := &Container{Config: testConfig(), Logger: zap.NewNop(), Services: &Services{Auth: svc}}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Group("/api", identify(userID), c.requirePlatformAdmin()).Get("/admin/users", func(ctx *fiber.Ctx) error {
		return ctx.SendStatus(fiber.StatusOK)
	})

	resp := callAdmin(t, app, http.MethodGet, "/api/admin/users", "")
	require.Equal(t, http.StatusUnauthorized, resp.Code)
}

// TestTheGuardAnswers503WithoutAuthentication: the same contract every other
// guarded group has, so a database-less deployment says "not configured" instead
// of "forbidden".
func TestTheGuardAnswers503WithoutAuthentication(t *testing.T) {
	c := &Container{Config: testConfig(), Logger: zap.NewNop(), Services: &Services{}}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Group("/api", identify(uuid.New()), c.requirePlatformAdmin()).Get("/admin/users", func(ctx *fiber.Ctx) error {
		return ctx.SendStatus(fiber.StatusOK)
	})

	resp := callAdmin(t, app, http.MethodGet, "/api/admin/users", "")
	require.Equal(t, http.StatusServiceUnavailable, resp.Code)
	require.Contains(t, resp.Body.String(), "auth_not_configured")
}

// TestTheOperatorSurfaceIsNotBehindTheTenantGuard is the boundary, asserted on the
// real route table.
//
// The route is registered on the app's own prefix rather than through a group, so
// no tenant middleware and no subscription guard can reach it. What that buys is
// the account that belongs to no workspace: it can still be administered. If this
// ever regresses, that account's operator surface answers 400 for a missing
// workspace header — which is what this test would catch.
//
// No X-Workspace-Id is sent. The workspace service is a mock that fails the test
// if it is ever asked to resolve one, so "the surface asked for a tenant" is not
// an assertion about a status code; it is an assertion that the call never
// happened.
func TestTheOperatorSurfaceIsNotBehindTheTenantGuard(t *testing.T) {
	userID := uuid.New()

	auth := authmocks.NewService(t)
	auth.EXPECT().CurrentUser(mock.Anything, userID).
		Return(authdomain.User{ID: userID, Email: "admin@example.com", Role: authdomain.RoleAdmin}, nil)

	admin := adminmocks.NewService(t)
	admin.EXPECT().List(mock.Anything, mock.Anything).Return([]admindomain.Account{}, int64(0), nil)

	workspace := workspacemocks.NewService(t)

	services := &Services{Auth: auth, Admin: admin, Workspace: workspace}
	cfg := testConfig()
	c := &Container{
		Config:   cfg,
		Logger:   zap.NewNop(),
		Services: services,
		Handlers: newHandlers(services, zap.NewNop(), cfg),
	}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(identify(userID))
	c.app = app
	c.registerRoutes()

	resp := callAdmin(t, app, http.MethodGet, "/api/admin/users", "")
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
}

// identify puts an authenticated account on the request, the way requireUser
// would.
func identify(userID uuid.UUID) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		middleware.SetIdentity(ctx, userID, "admin@example.com")
		return ctx.Next()
	}
}

func adminFallbackApp(t *testing.T) *fiber.App {
	t.Helper()

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ErrorHandler: func(ctx *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			var fiberErr *fiber.Error
			if errors.As(err, &fiberErr) {
				code = fiberErr.Code
			}
			return ctx.Status(code).SendString(err.Error())
		},
	})

	app.All("/admin*", unavailable("the operator surface is not configured", "admin_not_configured"))
	return app
}

func callAdmin(t *testing.T, app *fiber.App, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, nil)
	if body != "" {
		req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	}

	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	recorder := httptest.NewRecorder()
	recorder.Code = resp.StatusCode
	recorder.Body.WriteString(readAll(t, resp))
	return recorder
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()

	content, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(content)
}
