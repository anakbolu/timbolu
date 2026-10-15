package handler

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

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain/mocks"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
	"github.com/robzlabz/employeebot/packages/platform/database"
)

// identity stands in for what the container's auth and tenant middleware put on
// the request context.
type identity struct {
	userID      uuid.UUID
	email       string
	workspaceID uuid.UUID
	role        string
}

func newTestApp(t *testing.T, service domain.Service, id identity) *fiber.App {
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

	handler := New(service, zap.NewNop())

	withIdentity := func(c *fiber.Ctx) error {
		middleware.SetIdentity(c, id.userID, id.email)
		if id.workspaceID != uuid.Nil {
			middleware.SetScope(c, database.Scope{UserID: id.userID, WorkspaceID: id.workspaceID})
			middleware.SetRole(c, id.role)
		}
		return c.Next()
	}

	group := app.Group("/api", withIdentity)
	UserRoutes(group, handler)
	VerifiedRoutes(group, handler)
	TenantRoutes(group, handler)
	ManagerRoutes(group, handler)
	OwnerRoutes(group, handler)

	return app
}

func do(t *testing.T, app *fiber.App, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)

	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	recorder := httptest.NewRecorder()
	recorder.Code = resp.StatusCode
	if content, err := io.ReadAll(resp.Body); err == nil {
		recorder.Body.Write(content)
	}
	return recorder
}

func TestOnboardHandler(t *testing.T) {
	userID := uuid.New()
	workspaceID := uuid.New()

	t.Run("returns the workspace and its two default teams", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().Onboard(mock.Anything, userID, domain.OnboardRequest{
			Name:          "Toko Sinar",
			BusinessField: "Retail",
			Timezone:      "Asia/Jakarta",
		}).Return(domain.OnboardResult{
			Workspace: domain.Workspace{ID: workspaceID, Name: "Toko Sinar", Role: domain.RoleOwner},
			Teams: []domain.Team{
				{ID: uuid.New(), Name: "Tim Bolu", Kind: "bolu"},
				{ID: uuid.New(), Name: "Tim Hore", Kind: "hore"},
			},
			Created: true,
		}, nil).Once()

		app := newTestApp(t, service, identity{userID: userID, email: "owner@example.com"})
		resp := do(t, app, fiber.MethodPost, "/api/workspaces/onboard",
			`{"name":"Toko Sinar","business_field":"Retail","timezone":"Asia/Jakarta"}`)

		require.Equal(t, fiber.StatusOK, resp.Code)
		require.Contains(t, resp.Body.String(), `"created":true`)
		require.Contains(t, resp.Body.String(), "Tim Bolu")
		require.Contains(t, resp.Body.String(), "Tim Hore")
	})

	t.Run("400 for a malformed body", func(t *testing.T) {
		service := mocks.NewService(t)
		app := newTestApp(t, service, identity{userID: userID})

		resp := do(t, app, fiber.MethodPost, "/api/workspaces/onboard", `{`)
		require.Equal(t, fiber.StatusBadRequest, resp.Code)
	})
}

// TestTenantScopeIsPassedToTheService is the tenant plumbing check: the handler
// must hand the service the workspace from the request context, not a value it
// invented.
func TestTenantScopeIsPassedToTheService(t *testing.T) {
	userID, workspaceID := uuid.New(), uuid.New()

	service := mocks.NewService(t)
	service.EXPECT().Members(mock.Anything, domain.Scope{UserID: userID, WorkspaceID: workspaceID}).
		Return([]domain.Member{{UserID: userID, Email: "owner@example.com", Role: domain.RoleOwner}}, nil).Once()

	app := newTestApp(t, service, identity{userID: userID, email: "owner@example.com", workspaceID: workspaceID, role: domain.RoleOwner})
	resp := do(t, app, fiber.MethodGet, "/api/workspaces/current/members", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), "owner@example.com")
}

func TestWorkspaceErrorMapping(t *testing.T) {
	userID, workspaceID := uuid.New(), uuid.New()

	tests := []struct {
		name       string
		domainErr  error
		wantStatus int
		call       func(*mocks.Service, identity)
		path       string
		method     string
		body       string
	}{
		{
			name:       "a member who is not allowed to manage gets 403",
			domainErr:  domain.ErrForbidden,
			wantStatus: fiber.StatusForbidden,
			path:       "/api/workspaces/current/members/" + uuid.NewString(),
			method:     fiber.MethodPatch,
			body:       `{"role":"admin"}`,
			call: func(m *mocks.Service, id identity) {
				m.EXPECT().SetRole(mock.Anything, mock.Anything, id.role, mock.Anything, "admin").
					Return(domain.Member{}, domain.ErrForbidden).Once()
			},
		},
		{
			name:       "the last owner cannot be demoted",
			domainErr:  domain.ErrLastOwner,
			wantStatus: fiber.StatusConflict,
			path:       "/api/workspaces/current/members/" + uuid.NewString(),
			method:     fiber.MethodPatch,
			body:       `{"role":"member"}`,
			call: func(m *mocks.Service, id identity) {
				m.EXPECT().SetRole(mock.Anything, mock.Anything, id.role, mock.Anything, "member").
					Return(domain.Member{}, domain.ErrLastOwner).Once()
			},
		},
		{
			name:       "a pending invitation is reported as a conflict",
			domainErr:  domain.ErrInvitationAlreadySent,
			wantStatus: fiber.StatusConflict,
			path:       "/api/workspaces/current/invitations",
			method:     fiber.MethodPost,
			body:       `{"email":"new@example.com","role":"member"}`,
			call: func(m *mocks.Service, id identity) {
				m.EXPECT().Invite(mock.Anything, mock.Anything, id.role, id.userID, "new@example.com", "member").
					Return(domain.Invitation{}, domain.ErrInvitationAlreadySent).Once()
			},
		},
		{
			name:       "an unknown role is a bad request",
			domainErr:  domain.ErrInvalidRole,
			wantStatus: fiber.StatusBadRequest,
			path:       "/api/workspaces/current/invitations",
			method:     fiber.MethodPost,
			body:       `{"email":"new@example.com","role":"wizard"}`,
			call: func(m *mocks.Service, id identity) {
				m.EXPECT().Invite(mock.Anything, mock.Anything, id.role, id.userID, "new@example.com", "wizard").
					Return(domain.Invitation{}, domain.ErrInvalidRole).Once()
			},
		},
		{
			name:       "an unusable invitation token is a bad request",
			domainErr:  domain.ErrInvitationInvalid,
			wantStatus: fiber.StatusBadRequest,
			path:       "/api/invitations/accept",
			method:     fiber.MethodPost,
			body:       `{"token":"used"}`,
			call: func(m *mocks.Service, id identity) {
				m.EXPECT().AcceptInvitation(mock.Anything, "used", id.userID, id.email).
					Return(domain.Workspace{}, domain.ErrInvitationInvalid).Once()
			},
		},
		{
			name:       "signing in with the wrong address is forbidden",
			domainErr:  domain.ErrInvitationEmailMatch,
			wantStatus: fiber.StatusForbidden,
			path:       "/api/invitations/accept",
			method:     fiber.MethodPost,
			body:       `{"token":"live"}`,
			call: func(m *mocks.Service, id identity) {
				m.EXPECT().AcceptInvitation(mock.Anything, "live", id.userID, id.email).
					Return(domain.Workspace{}, domain.ErrInvitationEmailMatch).Once()
			},
		},
		{
			name:       "removing yourself is a conflict",
			domainErr:  domain.ErrCannotRemoveYourself,
			wantStatus: fiber.StatusConflict,
			path:       "/api/workspaces/current/members/" + userID.String(),
			method:     fiber.MethodDelete,
			call: func(m *mocks.Service, id identity) {
				m.EXPECT().RemoveMember(mock.Anything, mock.Anything, id.role, id.userID, userID).
					Return(domain.ErrCannotRemoveYourself).Once()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := mocks.NewService(t)
			id := identity{userID: userID, email: "owner@example.com", workspaceID: workspaceID, role: domain.RoleOwner}
			tt.call(service, id)

			app := newTestApp(t, service, id)
			resp := do(t, app, tt.method, tt.path, tt.body)

			require.Equal(t, tt.wantStatus, resp.Code)
		})
	}
}

func TestManagerRoutesUseTheRequestRole(t *testing.T) {
	userID, workspaceID, target := uuid.New(), uuid.New(), uuid.New()

	// The role check itself lives in the container middleware; here the handler
	// must forward the role it was given, which is what the service authorises on.
	service := mocks.NewService(t)
	service.EXPECT().SetRole(mock.Anything, mock.Anything, domain.RoleAdmin, target, domain.RoleMember).
		Return(domain.Member{UserID: target, Email: "member@example.com", Role: domain.RoleMember}, nil).Once()

	app := newTestApp(t, service, identity{
		userID:      userID,
		email:       "admin@example.com",
		workspaceID: workspaceID,
		role:        domain.RoleAdmin,
	})
	resp := do(t, app, fiber.MethodPatch, "/api/workspaces/current/members/"+target.String(), `{"role":"member"}`)

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), "member@example.com")
}

func TestMalformedPathIDIsRejected(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New(), role: domain.RoleOwner})

	for _, path := range []string{"/api/workspaces/current/members/not-a-uuid", "/api/workspaces/current/invitations/not-a-uuid"} {
		method := fiber.MethodPatch
		if strings.Contains(path, "invitations") {
			method = fiber.MethodDelete
		}
		resp := do(t, app, method, path, "")
		require.Equal(t, fiber.StatusBadRequest, resp.Code, path)
	}
}

func TestListWorkspaces(t *testing.T) {
	userID := uuid.New()

	service := mocks.NewService(t)
	service.EXPECT().Workspaces(mock.Anything, userID).Return([]domain.Workspace{
		{ID: uuid.New(), Name: "Toko Sinar", Role: domain.RoleOwner},
		{ID: uuid.New(), Name: "Klien Budi", Role: domain.RoleMember},
	}, nil).Once()

	app := newTestApp(t, service, identity{userID: userID, email: "owner@example.com"})
	resp := do(t, app, fiber.MethodGet, "/api/workspaces", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), "Toko Sinar")
	require.Contains(t, resp.Body.String(), "Klien Budi")
	require.Contains(t, resp.Body.String(), `"role":"member"`)
}
