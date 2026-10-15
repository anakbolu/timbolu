package handler

import (
	"bytes"
	"encoding/json"
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

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/admin/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/admin/domain/mocks"
	authdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
)

// This file is the wire and nothing else: the query a filter becomes, the fields
// a screen reads, and the status and code it branches on. What a role means is
// the service's business and is tested there.

// newTestApp mounts the routes behind the identity the container's middleware
// would have put on the request.
func newTestApp(t *testing.T, service domain.Service, actorID uuid.UUID) *fiber.App {
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
		middleware.SetIdentity(c, actorID, "admin@example.com")
		return c.Next()
	})

	Routes(group, New(service, zap.NewNop()))

	return app
}

// do runs one request and returns what came back.
func do(t *testing.T, app *fiber.App, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if body != nil {
		req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	}

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

// envelope is the shape every screen branches on.
type envelope struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
	Meta    struct {
		Count  int    `json:"count"`
		Total  int64  `json:"total"`
		Limit  int    `json:"limit"`
		Offset int    `json:"offset"`
		Role   string `json:"role"`
		Search string `json:"search"`
	} `json:"meta"`
	Error *struct {
		Code string `json:"code"`
	} `json:"error"`
}

func decode(t *testing.T, resp *httptest.ResponseRecorder) envelope {
	t.Helper()

	var body envelope
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &body))
	return body
}

func TestOverviewAnswersTheNumbers(t *testing.T) {
	svc := mocks.NewService(t)
	actorID := uuid.New()

	svc.EXPECT().Overview(mock.Anything).Return(domain.Stats{
		Total: 41, Admins: 2, Users: 39, Verified: 30, Onboarded: 25,
		GoogleAccounts: 11, NewLast7Days: 4,
	}, nil).Once()

	resp := do(t, newTestApp(t, svc, actorID), http.MethodGet, "/api/admin/overview", nil)
	require.Equal(t, http.StatusOK, resp.Code)

	body := decode(t, resp)
	require.True(t, body.Success)

	var stats struct {
		Total          int64 `json:"total"`
		Admins         int64 `json:"admins"`
		Users          int64 `json:"users"`
		Verified       int64 `json:"verified"`
		Onboarded      int64 `json:"onboarded"`
		GoogleAccounts int64 `json:"google_accounts"`
		NewLast7Days   int64 `json:"new_last_7_days"`
	}
	require.NoError(t, json.Unmarshal(body.Data, &stats))
	require.Equal(t, int64(41), stats.Total)
	require.Equal(t, int64(2), stats.Admins)
	require.Equal(t, int64(4), stats.NewLast7Days)
}

// TestUsersPassesTheQueryThroughAndAnswersTheTotal: the filter the screen sends
// is the filter the service reads, and the meta carries the numbers a pager
// needs without the screen having to remember what it asked for.
func TestUsersPassesTheQueryThroughAndAnswersTheTotal(t *testing.T) {
	svc := mocks.NewService(t)
	actorID, first, second := uuid.New(), uuid.New(), uuid.New()

	svc.EXPECT().List(mock.Anything, domain.Filter{Search: "bolu", Role: authdomain.RoleAdmin, Limit: 10, Offset: 20}).
		Return([]domain.Account{
			{ID: first, Email: "a@example.com", Role: authdomain.RoleAdmin, EmailVerified: true, Workspaces: 2},
			{ID: second, Email: "b@example.com", Role: authdomain.RoleUser, HasGoogle: true},
		}, int64(41), nil).Once()

	resp := do(t, newTestApp(t, svc, actorID), http.MethodGet, "/api/admin/users?q=bolu&role=admin&limit=10&offset=20", nil)
	require.Equal(t, http.StatusOK, resp.Code)

	body := decode(t, resp)
	require.True(t, body.Success)
	require.Equal(t, 2, body.Meta.Count)
	require.Equal(t, int64(41), body.Meta.Total)
	require.Equal(t, 10, body.Meta.Limit)
	require.Equal(t, 20, body.Meta.Offset)
	require.Equal(t, "admin", body.Meta.Role)
	require.Equal(t, "bolu", body.Meta.Search)

	var accounts []struct {
		ID            string `json:"id"`
		Email         string `json:"email"`
		Role          string `json:"role"`
		EmailVerified bool   `json:"email_verified"`
		HasGoogle     bool   `json:"has_google"`
		Workspaces    int64  `json:"workspaces"`
		CreatedAt     string `json:"created_at"`
	}
	require.NoError(t, json.Unmarshal(body.Data, &accounts))
	require.Len(t, accounts, 2)
	require.Equal(t, first.String(), accounts[0].ID)
	require.Equal(t, "admin", accounts[0].Role)
	require.True(t, accounts[0].EmailVerified)
	require.Equal(t, int64(2), accounts[0].Workspaces)
	require.True(t, accounts[1].HasGoogle)
}

// TestChangeRoleTakesTheActorFromTheSession: a request that named its own actor
// could promote itself, so the body carries only the role and the id comes from
// the identity the middleware set.
func TestChangeRoleTakesTheActorFromTheSession(t *testing.T) {
	svc := mocks.NewService(t)
	actorID, targetID := uuid.New(), uuid.New()

	var received domain.ChangeRoleRequest
	svc.EXPECT().ChangeRole(mock.Anything, mock.MatchedBy(func(req domain.ChangeRoleRequest) bool {
		received = req
		return true
	})).Return(domain.Account{
		ID: targetID, Email: "member@example.com", Role: authdomain.RoleAdmin, Workspaces: 1,
	}, nil).Once()

	resp := do(t, newTestApp(t, svc, actorID), http.MethodPost,
		"/api/admin/users/"+targetID.String()+"/role", []byte(`{"role":"admin","actor_id":"`+uuid.NewString()+`"}`))
	require.Equal(t, http.StatusOK, resp.Code)

	require.Equal(t, actorID, received.ActorID, "the actor is the session's account, not the body's")
	require.Equal(t, targetID, received.TargetID)
	require.Equal(t, authdomain.RoleAdmin, received.Role)

	body := decode(t, resp)
	require.True(t, body.Success)

	var account struct {
		Role string `json:"role"`
	}
	require.NoError(t, json.Unmarshal(body.Data, &account))
	require.Equal(t, "admin", account.Role)
}

func TestChangeRoleRejectsAnIdThatIsNotAUuid(t *testing.T) {
	svc := mocks.NewService(t)

	resp := do(t, newTestApp(t, svc, uuid.New()), http.MethodPost, "/api/admin/users/nope/role", []byte(`{"role":"admin"}`))

	require.Equal(t, http.StatusBadRequest, resp.Code)
	require.Equal(t, "invalid_input", decode(t, resp).Error.Code)
	svc.AssertNotCalled(t, "ChangeRole", mock.Anything, mock.Anything)
}

// TestChangeRoleMapsTheRefusals is the table the screen branches on: each refusal
// is a different thing to tell the person, so each is a different status and code.
func TestChangeRoleMapsTheRefusals(t *testing.T) {
	targetID := uuid.New()

	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"not an admin", domain.ErrNotAdmin, http.StatusForbidden, "forbidden"},
		{"missing account", domain.ErrAccountNotFound, http.StatusNotFound, "user_not_found"},
		{"last admin", domain.ErrLastAdmin, http.StatusConflict, "last_admin"},
		{"self demotion", domain.ErrSelfDemotion, http.StatusConflict, "self_demotion"},
		{"unknown role", domain.ErrInvalidInput, http.StatusBadRequest, "invalid_input"},
		{"no database", domain.ErrNotConfigured, http.StatusServiceUnavailable, "admin_not_configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := mocks.NewService(t)
			svc.EXPECT().ChangeRole(mock.Anything, mock.Anything).Return(domain.Account{}, tc.err).Once()

			resp := do(t, newTestApp(t, svc, uuid.New()), http.MethodPost,
				"/api/admin/users/"+targetID.String()+"/role", []byte(`{"role":"user"}`))

			require.Equal(t, tc.status, resp.Code)
			body := decode(t, resp)
			require.False(t, body.Success)
			require.Equal(t, tc.code, body.Error.Code)
		})
	}
}

// TestAListRefusalIsMappedToo: the same errors reach the read endpoints, and an
// unknown role filter is the one a screen can send by accident.
func TestAListRefusalIsMappedToo(t *testing.T) {
	svc := mocks.NewService(t)
	svc.EXPECT().List(mock.Anything, mock.Anything).Return(nil, int64(0), domain.ErrInvalidInput).Once()

	resp := do(t, newTestApp(t, svc, uuid.New()), http.MethodGet, "/api/admin/users?role=owner", nil)

	require.Equal(t, http.StatusBadRequest, resp.Code)
	require.Equal(t, "invalid_input", decode(t, resp).Error.Code)
}
