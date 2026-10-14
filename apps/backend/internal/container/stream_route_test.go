package container

import (
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	authdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/domain"
	authmocks "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/domain/mocks"
	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	chatmocks "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain/mocks"
	chathandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/handler"
	workspacedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	workspacemocks "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain/mocks"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/packages/platform/database"
	"go.uber.org/zap"
)

// TestTheLiveStreamAuthenticatesFromTheQuery is the gate that a browser can
// reach the stream at all.
//
// The whole route table is mounted, because the bug this guards against is an
// ordering one: the tenant middleware registered at the API prefix runs before
// the stream middleware unless the stream routes go on first, and it then
// rejects a request that carries its token in the query — which is exactly what
// a WebSocket and an EventSource have to do.
func TestTheLiveStreamAuthenticatesFromTheQuery(t *testing.T) {
	auth := authmocks.NewService(t)
	workspace := workspacemocks.NewService(t)
	chat := chatmocks.NewService(t)

	userID := uuid.New()
	workspaceID := uuid.New()

	auth.EXPECT().Authenticate(mock.Anything, "query-token").
		Return(authdomain.User{ID: userID, Email: "owner@example.com"}, nil).Once()
	workspace.EXPECT().Resolve(mock.Anything, workspaceID, userID).Return("owner", nil).Once()

	events := make(chan chatdomain.Event, 1)
	events <- chatdomain.Event{
		ID:          1,
		WorkspaceID: workspaceID,
		Type:        chatdomain.EventMessageNew,
		CreatedAt:   time.Now(),
	}
	close(events)

	chat.EXPECT().Stream(mock.Anything, mock.Anything, int64(0)).Return(events, nil).Once()

	app := mountedApp(t, auth, workspace, chat)

	req := httptest.NewRequest(fiber.MethodGet,
		"/api/events/stream?workspace_id="+workspaceID.String()+"&token=query-token", nil)
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusOK, resp.StatusCode, "the query token must be enough")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "event: message.new")
}

// TestTheSocketAlsoAuthenticatesFromTheQuery covers the primary transport.
func TestTheSocketAlsoAuthenticatesFromTheQuery(t *testing.T) {
	auth := authmocks.NewService(t)
	workspace := workspacemocks.NewService(t)
	chat := chatmocks.NewService(t)

	userID := uuid.New()
	workspaceID := uuid.New()

	auth.EXPECT().Authenticate(mock.Anything, "query-token").
		Return(authdomain.User{ID: userID, Email: "owner@example.com"}, nil).Once()
	workspace.EXPECT().Resolve(mock.Anything, workspaceID, userID).Return("owner", nil).Once()

	app := mountedApp(t, auth, workspace, chat)

	// A plain request is refused as an upgrade, not as unauthenticated: the
	// credential was accepted.
	req := httptest.NewRequest(fiber.MethodGet,
		"/api/events/socket?workspace_id="+workspaceID.String()+"&token=query-token", nil)
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusUpgradeRequired, resp.StatusCode)
}

// TestTheContentOriginNeedsNoCredential keeps the stream middleware scoped to
// the two stream paths.
//
// The content route must be reachable without a credential, because the iframe
// that frames it has an opaque origin and sends no cookie: the reference in the
// URL is the only capability. If the stream middleware covered the whole API
// prefix, this route would answer 401 and every sandboxed block would break.
func TestTheContentOriginNeedsNoCredential(t *testing.T) {
	chat := chatmocks.NewService(t)
	reference := chatdomain.EncodeContentRef(chatdomain.ContentRefPrefix + "/workspaces/a/objects/b/index.html")

	chat.EXPECT().Content(mock.Anything, reference).
		Return([]byte("<html></html>"), nil).Once()

	app := mountedApp(t, authmocks.NewService(t), workspacemocks.NewService(t), chat)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/content/"+reference, nil), -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusOK, resp.StatusCode, "the content origin is not behind the stream middleware")
}

// TestTheStreamStillRefusesWithoutACredential is the other half of the gate: the
// query string is an additional way in, not a way around.
func TestTheStreamStillRefusesWithoutACredential(t *testing.T) {
	app := mountedApp(t, authmocks.NewService(t), workspacemocks.NewService(t), chatmocks.NewService(t))

	req := httptest.NewRequest(fiber.MethodGet, "/api/events/stream?workspace_id="+uuid.NewString(), nil)
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
}

// mountedApp builds a container with the chat handler and the real route table,
// which is what makes the ordering observable.
func mountedApp(
	t *testing.T,
	auth authdomain.Service,
	workspace workspacedomain.Service,
	chat chatdomain.Service,
) *fiber.App {
	t.Helper()

	c := &Container{
		Config: testConfig(),
		Logger: zap.NewNop(),
		Services: &Services{
			Auth:      auth,
			Workspace: workspace,
			Chat:      chat,
		},
		Handlers: &Handlers{
			Chat: chathandler.New(chat, zap.NewNop()),
		},
	}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(func(ctx *fiber.Ctx) error {
		// The request context middleware normally wraps this; the handlers do not
		// need it.
		return ctx.Next()
	})
	c.app = app

	c.registerStreamRoutes()
	c.registerRoutes()

	return app
}

// TestTheTenantMiddlewareVerifiesTheHeaderWorkspace is the guard on the check
// that decides whether to skip verification.
//
// The workspace header is a request value, not proof that membership was
// verified: treating it as one would let a request name any workspace and skip
// the membership lookup, which is both a hole and a source of "forbidden" for
// the caller's own workspace.
func TestTheTenantMiddlewareVerifiesTheHeaderWorkspace(t *testing.T) {
	auth := authmocks.NewService(t)
	workspace := workspacemocks.NewService(t)

	c := &Container{
		Config:   testConfig(),
		Logger:   zap.NewNop(),
		Services: &Services{Auth: auth, Workspace: workspace},
	}

	userID := uuid.New()
	workspaceID := uuid.New()

	auth.EXPECT().Authenticate(mock.Anything, "token").
		Return(authdomain.User{ID: userID, Email: "owner@example.com"}, nil).Once()
	// The lookup must happen, even though the header names the workspace.
	workspace.EXPECT().Resolve(mock.Anything, workspaceID, userID).Return("owner", nil).Once()

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/tenant", c.requireUser(), c.requireTenant(), requireRole("owner"), func(ctx *fiber.Ctx) error {
		return ctx.JSON(fiber.Map{
			"role":      middleware.Role(ctx),
			"workspace": middleware.WorkspaceUUID(ctx).String(),
		})
	})

	req := httptest.NewRequest(fiber.MethodGet, "/tenant", nil)
	req.Header.Set(fiber.HeaderAuthorization, "Bearer token")
	req.Header.Set(HeaderWorkspaceID, workspaceID.String())

	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), `"role":"owner"`)
	require.Contains(t, string(body), workspaceID.String())
}

// TestTheTenantMiddlewareDoesNotResolveTwice pins the idempotency the ordering
// depends on: an identity already on the request is not looked up again.
func TestTheTenantMiddlewareDoesNotResolveTwice(t *testing.T) {
	auth := authmocks.NewService(t)
	workspace := workspacemocks.NewService(t)

	c := &Container{
		Config:   testConfig(),
		Logger:   zap.NewNop(),
		Services: &Services{Auth: auth, Workspace: workspace},
	}

	userID := uuid.New()
	workspaceID := uuid.New()

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/twice", func(ctx *fiber.Ctx) error {
		// Pretend the stream middleware already resolved both.
		middleware.SetIdentity(ctx, userID, "owner@example.com")
		middleware.SetScope(ctx, database.Scope{UserID: userID, WorkspaceID: workspaceID})
		return ctx.Next()
	}, c.requireUser(), c.requireTenant(), func(ctx *fiber.Ctx) error {
		return ctx.SendString("passed")
	})

	req := httptest.NewRequest(fiber.MethodGet, "/twice", nil)
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusOK, resp.StatusCode, "no header is needed once the identity is set")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "passed", string(body))

	// Neither service was called, which is what "does not resolve twice" means.
	auth.AssertNotCalled(t, "Authenticate", mock.Anything, mock.Anything)
	workspace.AssertNotCalled(t, "Resolve", mock.Anything, mock.Anything, mock.Anything)
}
