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

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain/mocks"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/middleware"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/response"
	"github.com/anakbolu/timbolu/packages/platform/database"
)

// identity stands in for what the container's auth and tenant middleware put on
// the request context.
type identity struct {
	userID      uuid.UUID
	workspaceID uuid.UUID
}

func newTestApp(t *testing.T, service domain.Gateway, id identity) *fiber.App {
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

	group := app.Group("/api", func(c *fiber.Ctx) error {
		middleware.SetIdentity(c, id.userID, "owner@example.com")
		middleware.SetScope(c, database.Scope{UserID: id.userID, WorkspaceID: id.workspaceID})
		return c.Next()
	})

	Routes(group, handler)
	ManagerRoutes(group, handler)

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

func redactedFixture() domain.Redacted {
	return domain.Redacted{
		ID:            uuid.New(),
		Name:          "Utama",
		Adapter:       domain.AdapterOpenAI,
		Model:         "gpt-4o-mini",
		Priority:      10,
		IsDefault:     true,
		Enabled:       true,
		HasAPIKey:     true,
		ContextTokens: 128000,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
}

func TestListProviders(t *testing.T) {
	service := mocks.NewGateway(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}

	service.EXPECT().Providers(mock.Anything, mock.Anything).Return([]domain.Redacted{redactedFixture()}, nil).Once()

	app := newTestApp(t, service, id)
	resp := do(t, app, fiber.MethodGet, "/api/llm/providers", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"has_api_key":true`)
	require.NotContains(t, resp.Body.String(), "api_key\":\"sk", "the secret never leaves the backend")
}

func TestAdaptersComeFromTheDomain(t *testing.T) {
	service := mocks.NewGateway(t)

	app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New()})
	resp := do(t, app, fiber.MethodGet, "/api/llm/adapters", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	for _, adapter := range domain.KnownAdapters {
		require.Contains(t, resp.Body.String(), adapter)
	}
}

// TestCreateProviderPassesTheKeyOnce: the key travels in, and only the presence
// flag travels back.
func TestCreateProviderPassesTheKeyOnce(t *testing.T) {
	service := mocks.NewGateway(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}

	service.EXPECT().Upsert(mock.Anything, mock.Anything, mock.MatchedBy(func(req domain.UpsertRequest) bool {
		return req.Name == "Utama" &&
			req.Adapter == domain.AdapterOpenAI &&
			req.Model == "gpt-4o-mini" &&
			req.APIKey == "sk-secret" &&
			req.Enabled
	})).Return(redactedFixture(), nil).Once()

	app := newTestApp(t, service, id)
	resp := do(t, app, fiber.MethodPost, "/api/llm/providers",
		`{"name":"Utama","adapter":"openai","model":"gpt-4o-mini","api_key":"sk-secret"}`)

	require.Equal(t, fiber.StatusCreated, resp.Code)
	require.NotContains(t, resp.Body.String(), "sk-secret")
}

// TestUpdateProviderKeepsEnabledWhenOmitted: a partial update must not disable a
// provider by accident.
func TestUpdateProviderKeepsEnabledWhenOmitted(t *testing.T) {
	service := mocks.NewGateway(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}
	existing := redactedFixture()
	existing.Enabled = false

	service.EXPECT().Providers(mock.Anything, mock.Anything).Return([]domain.Redacted{existing}, nil).Once()
	service.EXPECT().Upsert(mock.Anything, mock.Anything, mock.MatchedBy(func(req domain.UpsertRequest) bool {
		return req.ID == existing.ID && !req.Enabled
	})).Return(existing, nil).Once()

	app := newTestApp(t, service, id)
	resp := do(t, app, fiber.MethodPut, "/api/llm/providers/"+existing.ID.String(),
		`{"name":"Utama","adapter":"openai","model":"gpt-4o-mini"}`)

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"enabled":false`)
}

func TestUpdateProviderClearKey(t *testing.T) {
	service := mocks.NewGateway(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}
	existing := redactedFixture()

	service.EXPECT().Upsert(mock.Anything, mock.Anything, mock.MatchedBy(func(req domain.UpsertRequest) bool {
		return req.ClearKey && req.APIKey == ""
	})).Return(existing, nil).Once()

	app := newTestApp(t, service, id)
	resp := do(t, app, fiber.MethodPut, "/api/llm/providers/"+existing.ID.String(),
		`{"name":"Utama","adapter":"openai","model":"gpt-4o-mini","clear_api_key":true,"enabled":true}`)

	require.Equal(t, fiber.StatusOK, resp.Code)
}

func TestProviderFailuresMapToStatuses(t *testing.T) {
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}

	tests := map[string]struct {
		err    error
		status int
		code   string
	}{
		"missing provider": {err: domain.ErrProviderNotFound, status: fiber.StatusNotFound, code: "provider_not_found"},
		"duplicate name":   {err: domain.ErrDuplicateProvider, status: fiber.StatusConflict, code: "provider_exists"},
		"no provider":      {err: domain.ErrNoProvider, status: fiber.StatusConflict, code: "no_provider"},
		"invalid request":  {err: domain.ErrInvalidRequest, status: fiber.StatusBadRequest, code: "invalid_request"},
		"rate limited":     {err: domain.ErrRateLimited, status: fiber.StatusTooManyRequests, code: "rate_limited"},
		"provider down":    {err: domain.ErrProviderUnavailable, status: fiber.StatusBadGateway, code: "provider_unavailable"},
		"usage lost":       {err: domain.ErrUsageNotRecorded, status: fiber.StatusInternalServerError, code: "usage_not_recorded"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			service := mocks.NewGateway(t)
			service.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).Return(domain.Redacted{}, test.err).Once()

			app := newTestApp(t, service, id)
			resp := do(t, app, fiber.MethodPost, "/api/llm/providers",
				`{"name":"Utama","adapter":"openai","model":"gpt-4o-mini"}`)

			require.Equal(t, test.status, resp.Code)
			require.Contains(t, resp.Body.String(), test.code)
		})
	}
}

func TestTestProviderReturnsCapabilities(t *testing.T) {
	service := mocks.NewGateway(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}
	provider := redactedFixture()

	service.EXPECT().Test(mock.Anything, mock.Anything, provider.ID).Return(domain.Capabilities{
		Tools:            true,
		Streaming:        true,
		Vision:           true,
		MaxContextTokens: 128000,
	}, nil).Once()

	app := newTestApp(t, service, id)
	resp := do(t, app, fiber.MethodPost, "/api/llm/providers/"+provider.ID.String()+"/test", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"max_context_tokens":128000`)
}

func TestUsagePayloadCarriesTheTotal(t *testing.T) {
	service := mocks.NewGateway(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}

	service.EXPECT().Usage(mock.Anything, mock.Anything, 7).Return([]domain.UsageDaily{{
		Day:          time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC),
		Provider:     domain.AdapterOpenAI,
		Model:        "gpt-4o-mini",
		Purpose:      domain.PurposeAgent,
		Calls:        3,
		InputTokens:  1000,
		OutputTokens: 250,
		CostMicros:   4200,
	}}, nil).Once()

	app := newTestApp(t, service, id)
	resp := do(t, app, fiber.MethodGet, "/api/llm/usage?days=7", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"day":"2026-10-09"`)
	require.Contains(t, resp.Body.String(), `"total_tokens":1250`)
	require.Contains(t, resp.Body.String(), `"cost_micros":4200`)
}

// TestAgentModelNeverReturnsTheKey: a Bolu may hold its own key, and the screen
// still only learns whether one exists.
func TestAgentModelNeverReturnsTheKey(t *testing.T) {
	service := mocks.NewGateway(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}
	agentID := uuid.New()

	service.EXPECT().AgentModel(mock.Anything, mock.Anything).Return(domain.AgentOverride{
		Adapter:   domain.AdapterAnthropic,
		Model:     "claude-3-5-haiku",
		APIKey:    "sk-agent-secret",
		MaxTokens: 2048,
	}, nil).Once()

	app := newTestApp(t, service, id)
	resp := do(t, app, fiber.MethodGet, "/api/agents/"+agentID.String()+"/model", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"has_api_key":true`)
	require.NotContains(t, resp.Body.String(), "sk-agent-secret")
	require.NotContains(t, resp.Body.String(), `"api_key"`)
}

func TestSetAgentModelValidatesTheProviderID(t *testing.T) {
	service := mocks.NewGateway(t)

	app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New()})
	resp := do(t, app, fiber.MethodPut, "/api/agents/"+uuid.New().String()+"/model",
		`{"provider_id":"bukan-uuid","model":"gpt-4o-mini"}`)

	require.Equal(t, fiber.StatusBadRequest, resp.Code)
	require.Contains(t, resp.Body.String(), "invalid provider id")
}

func TestSetAgentModelRejectsABadAgentID(t *testing.T) {
	service := mocks.NewGateway(t)

	app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New()})
	resp := do(t, app, fiber.MethodPut, "/api/agents/bukan-uuid/model", `{}`)

	require.Equal(t, fiber.StatusBadRequest, resp.Code)
	require.Contains(t, resp.Body.String(), "invalid agent id")
}
