package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/device/domain"
	deviceservice "github.com/anakbolu/timbolu/apps/backend/internal/modules/device/service"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/middleware"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/response"
	"github.com/anakbolu/timbolu/packages/platform/database"
)

// This file is the wire: the status and code a client branches on, and the payload it reads back.
// The rules are the service's and are tested there; the service is real here so a refused platform
// travels the same path it will in production.

// stubRepository keeps the registry in memory with the same key the unique index uses.
type stubRepository struct {
	mu    sync.Mutex
	rows  map[string]domain.Device
	clock time.Time
}

func newStubRepository() *stubRepository {
	return &stubRepository{
		rows:  map[string]domain.Device{},
		clock: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC),
	}
}

func (s *stubRepository) Register(_ context.Context, scope domain.Scope, request domain.RegisterRequest) (domain.Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.clock = s.clock.Add(time.Minute)
	key := scope.UserID.String() + "|" + request.Token

	if existing, ok := s.rows[key]; ok {
		existing.Platform = request.Platform
		existing.AppVersion = request.AppVersion
		existing.LastSeenAt = s.clock
		s.rows[key] = existing

		return existing, nil
	}

	device := domain.Device{
		ID:          uuid.New(),
		UserID:      scope.UserID,
		WorkspaceID: scope.WorkspaceID,
		Platform:    request.Platform,
		Token:       request.Token,
		AppVersion:  request.AppVersion,
		LastSeenAt:  s.clock,
		CreatedAt:   s.clock,
	}
	s.rows[key] = device

	return device, nil
}

func (s *stubRepository) Delete(_ context.Context, scope domain.Scope, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := scope.UserID.String() + "|" + token
	if _, ok := s.rows[key]; !ok {
		return domain.ErrNotFound
	}

	delete(s.rows, key)

	return nil
}

func (s *stubRepository) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.rows)
}

func (s *stubRepository) seed(userID, workspaceID uuid.UUID, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.rows[userID.String()+"|"+token] = domain.Device{
		ID:          uuid.New(),
		UserID:      userID,
		WorkspaceID: workspaceID,
		Platform:    domain.PlatformIOS,
		Token:       token,
		LastSeenAt:  s.clock,
		CreatedAt:   s.clock,
	}
}

// newTestApp mounts the routes behind the identity the container's middlewares would have put on the
// request.
func newTestApp(t *testing.T, repo *stubRepository, userID, workspaceID uuid.UUID) *fiber.App {
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
		middleware.SetIdentity(c, userID, "owner@example.com")
		middleware.SetScope(c, database.Scope{UserID: userID, WorkspaceID: workspaceID})
		c.Locals("workspace_id", workspaceID.String())

		return c.Next()
	})

	Routes(group, New(deviceservice.New(deviceservice.Deps{
		Repository: repo,
	}), zap.NewNop()))

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

type envelope struct {
	Success bool `json:"success"`
	Data    struct {
		DeviceID   string `json:"device_id"`
		Platform   string `json:"platform"`
		Token      string `json:"token"`
		LastSeenAt string `json:"last_seen_at"`
	} `json:"data"`
	Error *struct {
		Code string `json:"code"`
	} `json:"error"`
}

func decode(t *testing.T, recorder *httptest.ResponseRecorder) envelope {
	t.Helper()

	var body envelope
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))

	return body
}

// TestRegisterReturnsTheStoredDevice: a client reads the row back, which is how it knows the
// registration landed and which id it holds.
func TestRegisterReturnsTheStoredDevice(t *testing.T) {
	repo := newStubRepository()
	app := newTestApp(t, repo, uuid.New(), uuid.New())

	resp := do(t, app, fiber.MethodPost, "/api/devices",
		`{"token":"ExponentPushToken[abc123]","platform":"ios","app_version":"1.0.0"}`)

	require.Equal(t, fiber.StatusCreated, resp.Code)
	body := decode(t, resp)
	require.True(t, body.Success)
	require.NotEmpty(t, body.Data.DeviceID)
	require.Equal(t, domain.PlatformIOS, body.Data.Platform)
	require.Equal(t, "ExponentPushToken[abc123]", body.Data.Token)
	require.NotEmpty(t, body.Data.LastSeenAt)
	require.Equal(t, 1, repo.count())
}

// TestAnUnknownPlatformIsAValidationFailure: 422 with `validation_failed` is the shape a client
// branches on, rather than a 400 that could also mean a malformed body.
func TestAnUnknownPlatformIsAValidationFailure(t *testing.T) {
	repo := newStubRepository()
	app := newTestApp(t, repo, uuid.New(), uuid.New())

	resp := do(t, app, fiber.MethodPost, "/api/devices",
		`{"token":"ExponentPushToken[abc123]","platform":"web"}`)

	require.Equal(t, fiber.StatusUnprocessableEntity, resp.Code)
	body := decode(t, resp)
	require.False(t, body.Success)
	require.NotNil(t, body.Error)
	require.Equal(t, "validation_failed", body.Error.Code)
	require.Zero(t, repo.count())
}

// TestDeletingAnotherAccountsTokenIs404: the endpoint does not confirm that somebody else registered
// the token. 403 would be that confirmation.
func TestDeletingAnotherAccountsTokenIs404(t *testing.T) {
	repo := newStubRepository()
	workspaceID := uuid.New()
	otherUser := uuid.New()
	caller := uuid.New()
	const token = "ExponentPushToken[abc123]"

	repo.seed(otherUser, workspaceID, token)
	app := newTestApp(t, repo, caller, workspaceID)

	resp := do(t, app, fiber.MethodDelete, "/api/devices/"+token, "")

	require.Equal(t, fiber.StatusNotFound, resp.Code)
	body := decode(t, resp)
	require.False(t, body.Success)
	require.NotNil(t, body.Error)
	require.Equal(t, "not_found", body.Error.Code)
	require.Equal(t, 1, repo.count(), "the other account's row is untouched")
}

// TestDeletingOwnTokenIs204: the row is gone and there is nothing left to say about it.
func TestDeletingOwnTokenIs204(t *testing.T) {
	repo := newStubRepository()
	workspaceID := uuid.New()
	caller := uuid.New()
	const token = "ExponentPushToken[abc123]"

	repo.seed(caller, workspaceID, token)
	app := newTestApp(t, repo, caller, workspaceID)

	resp := do(t, app, fiber.MethodDelete, "/api/devices/"+token, "")

	require.Equal(t, fiber.StatusNoContent, resp.Code)
	require.Empty(t, resp.Body.String())
	require.Zero(t, repo.count())
}
