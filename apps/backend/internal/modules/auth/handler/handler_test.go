package handler

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/auth/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/auth/domain/mocks"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/middleware"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/response"
)

// The handler tests exercise the HTTP surface with a mocked service: status
// codes, the response envelope, and the refresh cookie.

const apiPrefix = "/api"

func newTestApp(t *testing.T, service domain.Service, authenticated bool) *fiber.App {
	t.Helper()

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			var fiberErr *fiber.Error
			if errors.As(err, &fiberErr) {
				code = fiberErr.Code
			}
			return response.Error(c, code, err.Error(), "http_error", nil)
		},
	})

	handler := New(service, zap.NewNop(), Config{
		FrontendURL: "https://app.example.com",
		Cookie:      CookieConfig{Path: apiPrefix + "/auth"},
	})

	api := app.Group(apiPrefix)
	Routes(api, handler)

	protected := api.Group("", func(c *fiber.Ctx) error {
		if authenticated {
			middleware.SetIdentity(c, uuid.New(), "owner@example.com")
		}
		return c.Next()
	})
	ProtectedRoutes(protected, handler)

	return app
}

func do(t *testing.T, app *fiber.App, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	recorder := httptest.NewRecorder()
	recorder.Code = resp.StatusCode
	for key, values := range resp.Header {
		for _, value := range values {
			recorder.Header().Add(key, value)
		}
	}
	if content, err := io.ReadAll(resp.Body); err == nil {
		recorder.Body.Write(content)
	}

	return recorder
}

func TestRegisterHandler(t *testing.T) {
	t.Run("201 on success", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().Register(mock.Anything, domain.RegisterRequest{
			Email:    "owner@example.com",
			Password: "a-good-password",
		}).Return(domain.User{ID: uuid.New(), Email: "owner@example.com"}, nil).Once()

		app := newTestApp(t, service, false)
		resp := do(t, app, fiber.MethodPost, apiPrefix+"/auth/register",
			`{"email":"owner@example.com","password":"a-good-password"}`, nil)

		require.Equal(t, fiber.StatusCreated, resp.Code)
	})

	t.Run("409 when the address is taken", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().Register(mock.Anything, mock.Anything).Return(domain.User{}, domain.ErrEmailTaken).Once()

		app := newTestApp(t, service, false)
		resp := do(t, app, fiber.MethodPost, apiPrefix+"/auth/register",
			`{"email":"owner@example.com","password":"a-good-password"}`, nil)

		require.Equal(t, fiber.StatusConflict, resp.Code)
	})

	t.Run("400 for a weak password", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().Register(mock.Anything, mock.Anything).Return(domain.User{}, domain.ErrWeakPassword).Once()

		app := newTestApp(t, service, false)
		resp := do(t, app, fiber.MethodPost, apiPrefix+"/auth/register", `{"email":"a@b.com","password":"short"}`, nil)

		require.Equal(t, fiber.StatusBadRequest, resp.Code)
	})

	t.Run("400 for a malformed body", func(t *testing.T) {
		service := mocks.NewService(t)
		app := newTestApp(t, service, false)

		resp := do(t, app, fiber.MethodPost, apiPrefix+"/auth/register", `{`, nil)

		require.Equal(t, fiber.StatusBadRequest, resp.Code)
	})
}

func TestLoginHandler(t *testing.T) {
	t.Run("sets an httpOnly refresh cookie and returns the access token", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().Login(mock.Anything, mock.Anything).Return(domain.Session{
			AccessToken:     "access-token",
			AccessTokenExp:  time.Now().Add(15 * time.Minute),
			RefreshToken:    "refresh-token",
			RefreshTokenExp: time.Now().Add(24 * time.Hour),
			User:            domain.User{ID: uuid.New(), Email: "owner@example.com", EmailVerified: true},
		}, nil).Once()

		app := newTestApp(t, service, false)
		resp := do(t, app, fiber.MethodPost, apiPrefix+"/auth/login",
			`{"email":"owner@example.com","password":"a-good-password"}`, nil)

		require.Equal(t, fiber.StatusOK, resp.Code)

		// Fiber lowercases the attribute names, so the assertions are too.
		cookie := strings.ToLower(resp.Header().Get(fiber.HeaderSetCookie))
		require.Contains(t, cookie, strings.ToLower(CookieName+"=refresh-token"))
		require.Contains(t, cookie, "httponly", "the refresh token must not be readable from JavaScript")
		require.Contains(t, cookie, "path="+apiPrefix+"/auth", "the cookie must only be sent to the auth endpoints")
		require.NotContains(t, resp.Body.String(), "refresh-token", "the refresh token must not appear in the body")
	})

	t.Run("401 on wrong credentials", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().Login(mock.Anything, mock.Anything).Return(domain.Session{}, domain.ErrInvalidCredentials).Once()

		app := newTestApp(t, service, false)
		resp := do(t, app, fiber.MethodPost, apiPrefix+"/auth/login", `{"email":"a@b.com","password":"nope"}`, nil)

		require.Equal(t, fiber.StatusUnauthorized, resp.Code)
	})

	t.Run("429 when throttled", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().Login(mock.Anything, mock.Anything).Return(domain.Session{}, domain.ErrTooManyAttempts).Once()

		app := newTestApp(t, service, false)
		resp := do(t, app, fiber.MethodPost, apiPrefix+"/auth/login", `{"email":"a@b.com","password":"nope"}`, nil)

		require.Equal(t, fiber.StatusTooManyRequests, resp.Code)
	})
}

func TestRefreshHandler(t *testing.T) {
	t.Run("reads the cookie when the body is empty", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().Refresh(mock.Anything, "cookie-token").Return(domain.Session{
			AccessToken:     "new-access",
			RefreshToken:    "new-refresh",
			RefreshTokenExp: time.Now().Add(time.Hour),
			User:            domain.User{ID: uuid.New()},
		}, nil).Once()

		app := newTestApp(t, service, false)
		resp := do(t, app, fiber.MethodPost, apiPrefix+"/auth/refresh", "",
			map[string]string{fiber.HeaderCookie: CookieName + "=cookie-token"})

		require.Equal(t, fiber.StatusOK, resp.Code)
		require.Contains(t, resp.Header().Get(fiber.HeaderSetCookie), "new-refresh", "the rotated token must replace the old cookie")
	})

	t.Run("clears the cookie when the token is rejected", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().Refresh(mock.Anything, "stale").Return(domain.Session{}, domain.ErrInvalidToken).Once()

		app := newTestApp(t, service, false)
		resp := do(t, app, fiber.MethodPost, apiPrefix+"/auth/refresh", `{"token":"stale"}`, nil)

		require.Equal(t, fiber.StatusBadRequest, resp.Code)
		require.Contains(t, resp.Header().Get(fiber.HeaderSetCookie), CookieName+"=")
	})
}

func TestVerifyEmailHandler(t *testing.T) {
	t.Run("200 when the token is consumed", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().VerifyEmail(mock.Anything, "the-token").
			Return(domain.User{ID: uuid.New(), EmailVerified: true}, nil).Once()

		app := newTestApp(t, service, false)
		resp := do(t, app, fiber.MethodPost, apiPrefix+"/auth/verify-email", `{"token":"the-token"}`, nil)

		require.Equal(t, fiber.StatusOK, resp.Code)
	})

	t.Run("400 for a token that was already used", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().VerifyEmail(mock.Anything, "used").Return(domain.User{}, domain.ErrInvalidToken).Once()

		app := newTestApp(t, service, false)
		resp := do(t, app, fiber.MethodPost, apiPrefix+"/auth/verify-email", `{"token":"used"}`, nil)

		require.Equal(t, fiber.StatusBadRequest, resp.Code)
	})
}

func TestForgotPasswordNeverRevealsWhetherTheAddressExists(t *testing.T) {
	service := mocks.NewService(t)
	service.EXPECT().RequestPasswordReset(mock.Anything, "known@example.com").Return(nil).Once()
	service.EXPECT().RequestPasswordReset(mock.Anything, "unknown@example.com").Return(nil).Once()

	app := newTestApp(t, service, false)

	known := do(t, app, fiber.MethodPost, apiPrefix+"/auth/password/forgot", `{"email":"known@example.com"}`, nil)
	unknown := do(t, app, fiber.MethodPost, apiPrefix+"/auth/password/forgot", `{"email":"unknown@example.com"}`, nil)

	require.Equal(t, fiber.StatusOK, known.Code)
	require.Equal(t, known.Code, unknown.Code)
	require.Equal(t, known.Body.String(), unknown.Body.String())
}

func TestMeRequiresIdentity(t *testing.T) {
	t.Run("200 with the account", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().CurrentUser(mock.Anything, mock.Anything).
			Return(domain.User{ID: uuid.New(), Email: "owner@example.com", EmailVerified: true}, nil).Once()

		app := newTestApp(t, service, true)
		resp := do(t, app, fiber.MethodGet, apiPrefix+"/auth/me", "", nil)

		require.Equal(t, fiber.StatusOK, resp.Code)
	})

	// The container's auth middleware answers 401 before the handler runs; here
	// the middleware is replaced by a stub, so the handler sees a zero user id
	// and the service reports it as not found.
	t.Run("404 when the identity is missing", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().CurrentUser(mock.Anything, uuid.Nil).
			Return(domain.User{}, domain.ErrUserNotFound).Once()

		app := newTestApp(t, service, false)
		resp := do(t, app, fiber.MethodGet, apiPrefix+"/auth/me", "", nil)

		require.Equal(t, fiber.StatusNotFound, resp.Code)
	})
}

func TestGoogleEndpoints(t *testing.T) {
	t.Run("start redirects to the consent screen", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().GoogleAuthURL(mock.Anything).
			Return("https://accounts.google.com/o/oauth2/v2/auth?state=x", nil).Once()

		app := newTestApp(t, service, false)
		resp := do(t, app, fiber.MethodGet, apiPrefix+"/auth/google/start", "", nil)

		require.Equal(t, fiber.StatusFound, resp.Code)
		require.Contains(t, resp.Header().Get(fiber.HeaderLocation), "accounts.google.com")
	})

	t.Run("callback sets the cookie and returns to the frontend", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().GoogleCallback(mock.Anything, mock.Anything).Return(domain.Session{
			RefreshToken:    "refresh",
			RefreshTokenExp: time.Now().Add(time.Hour),
			User:            domain.User{ID: uuid.New(), Onboarded: false},
		}, nil).Once()

		app := newTestApp(t, service, false)
		resp := do(t, app, fiber.MethodGet, apiPrefix+"/auth/google/callback?code=c&state=s", "", nil)

		require.Equal(t, fiber.StatusFound, resp.Code)
		require.Equal(t, "https://app.example.com/onboarding?google=ok", resp.Header().Get(fiber.HeaderLocation))
		require.Contains(t, resp.Header().Get(fiber.HeaderSetCookie), CookieName+"=refresh")
	})

	t.Run("callback sends an onboarded user to the app", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().GoogleCallback(mock.Anything, mock.Anything).Return(domain.Session{
			RefreshToken:    "refresh",
			RefreshTokenExp: time.Now().Add(time.Hour),
			User:            domain.User{ID: uuid.New(), Onboarded: true},
		}, nil).Once()

		app := newTestApp(t, service, false)
		resp := do(t, app, fiber.MethodGet, apiPrefix+"/auth/google/callback?code=c&state=s", "", nil)

		require.Equal(t, "https://app.example.com/login?google=ok", resp.Header().Get(fiber.HeaderLocation))
	})

	t.Run("callback reports a failure without leaking the reason", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().GoogleCallback(mock.Anything, mock.Anything).
			Return(domain.Session{}, domain.ErrInvalidToken).Once()

		app := newTestApp(t, service, false)
		resp := do(t, app, fiber.MethodGet, apiPrefix+"/auth/google/callback?code=c&state=bad", "", nil)

		require.Equal(t, fiber.StatusFound, resp.Code)
		require.Equal(t, "https://app.example.com/login?google_error=1", resp.Header().Get(fiber.HeaderLocation))
	})

	t.Run("start reports 503 when Google is not configured", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().GoogleAuthURL(mock.Anything).Return("", domain.ErrGoogleNotConfigured).Once()

		app := newTestApp(t, service, false)
		resp := do(t, app, fiber.MethodGet, apiPrefix+"/auth/google/start", "", nil)

		require.Equal(t, fiber.StatusServiceUnavailable, resp.Code)
	})
}

func TestLogoutHandler(t *testing.T) {
	service := mocks.NewService(t)
	service.EXPECT().Logout(mock.Anything, "refresh").Return(nil).Once()

	app := newTestApp(t, service, false)
	resp := do(t, app, fiber.MethodPost, apiPrefix+"/auth/logout", "",
		map[string]string{fiber.HeaderCookie: CookieName + "=refresh"})

	require.Equal(t, fiber.StatusOK, resp.Code)

	cookie := strings.ToLower(resp.Header().Get(fiber.HeaderSetCookie))
	require.Contains(t, cookie, strings.ToLower(CookieName)+"=;", "the cookie must be emptied")
	require.Contains(t, cookie, "expires=", "the cookie must expire immediately")
}
