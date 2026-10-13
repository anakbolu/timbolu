package container

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	authdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/domain/mocks"
	workspacedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	workspacemocks "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain/mocks"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
)

// streamApp mounts the stream middleware in front of a handler that reports the
// identity it resolved, which is what the live stream's own routes depend on.
func streamApp(c *Container) *fiber.App {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})

	app.Get("/stream", c.requireStreamAuth("chat_not_configured"), func(ctx *fiber.Ctx) error {
		return ctx.JSON(fiber.Map{
			"user":      middleware.CurrentUserID(ctx).String(),
			"workspace": middleware.WorkspaceUUID(ctx).String(),
		})
	})

	return app
}

func streamContainer(t *testing.T, auth authdomain.Service, workspace workspacedomain.Service) *Container {
	t.Helper()

	return &Container{
		Config: testConfig(),
		Logger: zap.NewNop(),
		Services: &Services{
			Auth:      auth,
			Workspace: workspace,
		},
	}
}

// TestStreamAuthReadsTheTokenFromTheQuery is the reason the middleware exists: a
// browser cannot set a header on a WebSocket or an EventSource, so the token has
// to be reachable another way — and the alternative, an unauthenticated stream,
// would leak another tenant's events.
func TestStreamAuthReadsTheTokenFromTheQuery(t *testing.T) {
	auth := mocks.NewService(t)
	workspace := workspacemocks.NewService(t)

	userID := uuid.New()
	workspaceID := uuid.New()

	auth.EXPECT().Authenticate(mock.Anything, "query-token").
		Return(authdomain.User{ID: userID, Email: "owner@example.com"}, nil).Once()
	workspace.EXPECT().Resolve(mock.Anything, workspaceID, userID).Return("owner", nil).Once()

	app := streamApp(streamContainer(t, auth, workspace))

	req := httptest.NewRequest(fiber.MethodGet,
		"/stream?token=query-token&workspace_id="+workspaceID.String(), nil)
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), userID.String())
	require.Contains(t, string(body), workspaceID.String())
}

// TestStreamAuthStillAcceptsTheHeader keeps the ordinary case working, so a
// client that can set a header is not forced through the query.
func TestStreamAuthStillAcceptsTheHeader(t *testing.T) {
	auth := mocks.NewService(t)
	workspace := workspacemocks.NewService(t)

	userID := uuid.New()
	workspaceID := uuid.New()

	auth.EXPECT().Authenticate(mock.Anything, "header-token").
		Return(authdomain.User{ID: userID, Email: "owner@example.com"}, nil).Once()
	workspace.EXPECT().Resolve(mock.Anything, workspaceID, userID).Return("owner", nil).Once()

	app := streamApp(streamContainer(t, auth, workspace))

	req := httptest.NewRequest(fiber.MethodGet, "/stream?workspace_id="+workspaceID.String(), nil)
	req.Header.Set(fiber.HeaderAuthorization, "Bearer header-token")
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusOK, resp.StatusCode)
}

// TestStreamAuthRefusesTheCasesThatMustNotReachTheStream covers each way the
// check can fail, so a mistake is a 4xx rather than a leaked stream.
func TestStreamAuthRefusesTheCasesThatMustNotReachTheStream(t *testing.T) {
	workspaceID := uuid.New()

	t.Run("no token", func(t *testing.T) {
		app := streamApp(streamContainer(t, mocks.NewService(t), workspacemocks.NewService(t)))

		req := httptest.NewRequest(fiber.MethodGet, "/stream?workspace_id="+workspaceID.String(), nil)
		resp, err := app.Test(req, -1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })

		require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("invalid token", func(t *testing.T) {
		auth := mocks.NewService(t)
		auth.EXPECT().Authenticate(mock.Anything, "bad").
			Return(authdomain.User{}, authdomain.ErrInvalidToken).Once()

		app := streamApp(streamContainer(t, auth, workspacemocks.NewService(t)))

		req := httptest.NewRequest(fiber.MethodGet,
			"/stream?token=bad&workspace_id="+workspaceID.String(), nil)
		resp, err := app.Test(req, -1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })

		require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("no workspace", func(t *testing.T) {
		auth := mocks.NewService(t)
		auth.EXPECT().Authenticate(mock.Anything, "token").
			Return(authdomain.User{ID: uuid.New()}, nil).Once()

		app := streamApp(streamContainer(t, auth, workspacemocks.NewService(t)))

		resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/stream?token=token", nil), -1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })

		require.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
	})

	t.Run("workspace is not a uuid", func(t *testing.T) {
		auth := mocks.NewService(t)
		auth.EXPECT().Authenticate(mock.Anything, "token").
			Return(authdomain.User{ID: uuid.New()}, nil).Once()

		app := streamApp(streamContainer(t, auth, workspacemocks.NewService(t)))

		resp, err := app.Test(httptest.NewRequest(fiber.MethodGet,
			"/stream?token=token&workspace_id=bukan-uuid", nil), -1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })

		require.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
	})

	t.Run("not a member", func(t *testing.T) {
		auth := mocks.NewService(t)
		workspace := workspacemocks.NewService(t)
		userID := uuid.New()

		auth.EXPECT().Authenticate(mock.Anything, "token").
			Return(authdomain.User{ID: userID}, nil).Once()
		workspace.EXPECT().Resolve(mock.Anything, workspaceID, userID).
			Return("", workspacedomain.ErrNotMember).Once()

		app := streamApp(streamContainer(t, auth, workspace))

		req := httptest.NewRequest(fiber.MethodGet,
			"/stream?token=token&workspace_id="+workspaceID.String(), nil)
		resp, err := app.Test(req, -1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })

		require.Equal(t, fiber.StatusForbidden, resp.StatusCode)

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Contains(t, string(body), "not_a_member")
	})

	t.Run("the membership lookup fails", func(t *testing.T) {
		auth := mocks.NewService(t)
		workspace := workspacemocks.NewService(t)
		userID := uuid.New()

		auth.EXPECT().Authenticate(mock.Anything, "token").
			Return(authdomain.User{ID: userID}, nil).Once()
		workspace.EXPECT().Resolve(mock.Anything, workspaceID, userID).
			Return("", errors.New("the database is down")).Once()

		app := streamApp(streamContainer(t, auth, workspace))

		req := httptest.NewRequest(fiber.MethodGet,
			"/stream?token=token&workspace_id="+workspaceID.String(), nil)
		resp, err := app.Test(req, -1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })

		require.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
	})
}

// TestStreamAuthWithoutTheServicesIsUnavailable keeps a deployment that has no
// database answering 503 rather than pretending the stream works.
func TestStreamAuthWithoutTheServicesIsUnavailable(t *testing.T) {
	c := &Container{Config: testConfig(), Logger: zap.NewNop(), Services: &Services{}}
	app := streamApp(c)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/stream?token=x", nil), -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "chat_not_configured")
}

// TestBearerTokenReadsOnlyTheRightShape keeps a malformed header from being
// treated as a credential.
func TestBearerTokenReadsOnlyTheRightShape(t *testing.T) {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/token", func(ctx *fiber.Ctx) error {
		return ctx.SendString(bearerToken(ctx))
	})

	cases := map[string]string{
		"Bearer abc":  "abc",
		"bearer abc":  "abc",
		"Bearer  abc": "abc",
		"":            "",
		"Basic abc":   "",
		"abc":         "",
		"Bearer":      "",
	}

	for header, want := range cases {
		t.Run(strings.ReplaceAll(header, " ", "_"), func(t *testing.T) {
			req := httptest.NewRequest(fiber.MethodGet, "/token", nil)
			if header != "" {
				req.Header.Set(fiber.HeaderAuthorization, header)
			}

			resp, err := app.Test(req, -1)
			require.NoError(t, err)
			t.Cleanup(func() { _ = resp.Body.Close() })

			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, want, string(body))
		})
	}
}
