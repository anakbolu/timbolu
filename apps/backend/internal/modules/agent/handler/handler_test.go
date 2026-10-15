package handler

import (
	"context"
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

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/domain/mocks"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
	"github.com/robzlabz/employeebot/packages/platform/database"
)

// identity stands in for what the container's auth and tenant middleware put on
// the request context.
type identity struct {
	userID      uuid.UUID
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

	group := app.Group("/api", func(c *fiber.Ctx) error {
		middleware.SetIdentity(c, id.userID, "owner@example.com")
		if id.workspaceID != uuid.Nil {
			middleware.SetScope(c, database.Scope{UserID: id.userID, WorkspaceID: id.workspaceID})
			middleware.SetRole(c, id.role)
		}
		return c.Next()
	})

	// The catalog routes come first, which is the order the container wires them
	// in: `/agents/templates` has to be registered before `/agents/:agentID`, or
	// the parameter route answers for it.
	CatalogRoutes(group, handler)
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

func agentFixture() domain.Agent {
	return domain.Agent{
		ID:          uuid.New(),
		WorkspaceID: uuid.New(),
		TeamID:      uuid.New(),
		TeamName:    "Tim Bolu",
		TeamKind:    "bolu",
		Name:        "Oren",
		Role:        "Penjualan",
		Persona:     "Ramah.",
		Tone:        "hangat",
		Shape:       "circle",
		Color:       "#F97316",
		Status:      domain.StoredActive,
		Display:     domain.DisplayIdle,
		TemplateKey: "oren",
		Tools:       []string{"gmail.search"},
		CreatedAt:   time.Now(),
	}
}

func TestListAgents(t *testing.T) {
	service := mocks.NewService(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New(), role: "owner"}

	service.EXPECT().List(mock.Anything, mock.Anything).Return([]domain.Agent{agentFixture()}, nil).Once()

	app := newTestApp(t, service, id)
	resp := do(t, app, fiber.MethodGet, "/api/agents", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"display_status":"idle"`)
}

func TestGetAgent(t *testing.T) {
	t.Run("200 with the derived status", func(t *testing.T) {
		service := mocks.NewService(t)
		id := identity{userID: uuid.New(), workspaceID: uuid.New(), role: "member"}
		agent := agentFixture()

		service.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything).Return(agent, nil).Once()

		app := newTestApp(t, service, id)
		resp := do(t, app, fiber.MethodGet, "/api/agents/"+agent.ID.String(), "")

		require.Equal(t, fiber.StatusOK, resp.Code)
		require.Contains(t, resp.Body.String(), "Tim Bolu")
	})

	t.Run("400 for a malformed id", func(t *testing.T) {
		service := mocks.NewService(t)
		app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New()})

		resp := do(t, app, fiber.MethodGet, "/api/agents/not-a-uuid", "")
		require.Equal(t, fiber.StatusBadRequest, resp.Code)
	})

	t.Run("404 for an unknown Bolu", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything).Return(domain.Agent{}, domain.ErrAgentNotFound).Once()

		app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New()})
		resp := do(t, app, fiber.MethodGet, "/api/agents/"+uuid.NewString(), "")

		require.Equal(t, fiber.StatusNotFound, resp.Code)
		require.Contains(t, resp.Body.String(), "agent_not_found")
	})
}

func TestCreateAgent(t *testing.T) {
	t.Run("201 from a template", func(t *testing.T) {
		service := mocks.NewService(t)
		id := identity{userID: uuid.New(), workspaceID: uuid.New(), role: "owner"}
		teamID := uuid.New()

		service.EXPECT().Create(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, _ domain.Scope, req domain.CreateRequest) (domain.Agent, error) {
				require.Equal(t, teamID, req.TeamID)
				require.Equal(t, "oren", req.TemplateKey)
				return agentFixture(), nil
			}).Once()

		app := newTestApp(t, service, id)
		resp := do(t, app, fiber.MethodPost, "/api/agents",
			`{"team_id":"`+teamID.String()+`","template_key":"oren"}`)

		require.Equal(t, fiber.StatusCreated, resp.Code)
	})

	t.Run("409 when the plan limit is reached", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().Create(mock.Anything, mock.Anything, mock.Anything).
			Return(domain.Agent{}, domain.ErrAgentLimitReached).Once()

		app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New(), role: "owner"})
		resp := do(t, app, fiber.MethodPost, "/api/agents", `{"team_id":"`+uuid.NewString()+`","name":"X"}`)

		require.Equal(t, fiber.StatusConflict, resp.Code)
		require.Contains(t, resp.Body.String(), "agent_limit_reached")
	})

	t.Run("400 for a malformed team id", func(t *testing.T) {
		service := mocks.NewService(t)
		app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New(), role: "owner"})

		resp := do(t, app, fiber.MethodPost, "/api/agents", `{"team_id":"nope","name":"X"}`)
		require.Equal(t, fiber.StatusBadRequest, resp.Code)
	})

	t.Run("400 for a malformed copy_from", func(t *testing.T) {
		service := mocks.NewService(t)
		app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New(), role: "owner"})

		resp := do(t, app, fiber.MethodPost, "/api/agents", `{"team_id":"`+uuid.NewString()+`","copy_from":"nope"}`)
		require.Equal(t, fiber.StatusBadRequest, resp.Code)
	})

	t.Run("400 for a malformed body", func(t *testing.T) {
		service := mocks.NewService(t)
		app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New(), role: "owner"})

		resp := do(t, app, fiber.MethodPost, "/api/agents", `{`)
		require.Equal(t, fiber.StatusBadRequest, resp.Code)
	})
}

func TestUpdateAgent(t *testing.T) {
	t.Run("200 with the saved profile", func(t *testing.T) {
		service := mocks.NewService(t)
		agent := agentFixture()

		service.EXPECT().Update(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, _ domain.Scope, _ uuid.UUID, req domain.UpdateRequest) (domain.Agent, error) {
				require.Equal(t, "Oren Baru", req.Name)
				require.Equal(t, "Selalu pakai faktur.", req.Persona)
				return agent, nil
			}).Once()

		app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New(), role: "admin"})
		resp := do(t, app, fiber.MethodPatch, "/api/agents/"+agent.ID.String(),
			`{"name":"Oren Baru","persona":"Selalu pakai faktur."}`)

		require.Equal(t, fiber.StatusOK, resp.Code)
	})

	t.Run("400 for invalid input", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().Update(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(domain.Agent{}, domain.ErrInvalidInput).Once()

		app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New(), role: "admin"})
		resp := do(t, app, fiber.MethodPatch, "/api/agents/"+uuid.NewString(), `{"name":""}`)

		require.Equal(t, fiber.StatusBadRequest, resp.Code)
		require.Contains(t, resp.Body.String(), "invalid_input")
	})
}

func TestSetStatusHandler(t *testing.T) {
	t.Run("200 for resting", func(t *testing.T) {
		service := mocks.NewService(t)
		agent := agentFixture()

		service.EXPECT().SetStatus(mock.Anything, mock.Anything, mock.Anything, domain.StoredResting).
			Return(agent, nil).Once()

		app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New(), role: "owner"})
		resp := do(t, app, fiber.MethodPatch, "/api/agents/"+agent.ID.String()+"/status", `{"status":"resting"}`)

		require.Equal(t, fiber.StatusOK, resp.Code)
	})

	t.Run("400 for an unknown status", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().SetStatus(mock.Anything, mock.Anything, mock.Anything, "sleepy").
			Return(domain.Agent{}, domain.ErrInvalidStatus).Once()

		app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New(), role: "owner"})
		resp := do(t, app, fiber.MethodPatch, "/api/agents/"+uuid.NewString()+"/status", `{"status":"sleepy"}`)

		require.Equal(t, fiber.StatusBadRequest, resp.Code)
	})
}

func TestSetMemoryExtractionHandler(t *testing.T) {
	t.Run("200 turns it off and reports the switch", func(t *testing.T) {
		service := mocks.NewService(t)
		agent := agentFixture()
		agent.MemoryExtraction = false

		service.EXPECT().SetMemoryExtraction(mock.Anything, mock.Anything, mock.Anything, false).
			Return(agent, nil).Once()

		app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New(), role: "owner"})
		resp := do(t, app, fiber.MethodPatch, "/api/agents/"+agent.ID.String()+"/memory-extraction", `{"enabled":false}`)

		require.Equal(t, fiber.StatusOK, resp.Code)
		require.Contains(t, resp.Body.String(), `"memory_extraction_enabled":false`)
	})

	t.Run("400 when the body says nothing", func(t *testing.T) {
		// An absent field must not be read as "turn it off": the switch decides
		// whether a Bolu learns from conversations, so a body that forgot it
		// changes nothing rather than something.
		service := mocks.NewService(t)

		app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New(), role: "owner"})
		resp := do(t, app, fiber.MethodPatch, "/api/agents/"+uuid.NewString()+"/memory-extraction", `{}`)

		require.Equal(t, fiber.StatusBadRequest, resp.Code)
	})

	t.Run("400 for a malformed id", func(t *testing.T) {
		service := mocks.NewService(t)

		app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New(), role: "owner"})
		resp := do(t, app, fiber.MethodPatch, "/api/agents/not-a-uuid/memory-extraction", `{"enabled":true}`)

		require.Equal(t, fiber.StatusBadRequest, resp.Code)
	})
}

func TestDeleteAgentHandler(t *testing.T) {
	t.Run("200 when the Bolu is idle", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().Delete(mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()

		app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New(), role: "owner"})
		resp := do(t, app, fiber.MethodDelete, "/api/agents/"+uuid.NewString(), "")

		require.Equal(t, fiber.StatusOK, resp.Code)
	})

	// A Bolu that is working must not be removed: the task would lose its agent.
	t.Run("409 while a task is running", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().Delete(mock.Anything, mock.Anything, mock.Anything).
			Return(domain.ErrAgentHasRunningTask).Once()

		app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New(), role: "owner"})
		resp := do(t, app, fiber.MethodDelete, "/api/agents/"+uuid.NewString(), "")

		require.Equal(t, fiber.StatusConflict, resp.Code)
		require.Contains(t, resp.Body.String(), "agent_busy")
	})
}

func TestArrangeHandler(t *testing.T) {
	bolu, hore := uuid.New(), uuid.New()
	first, second := uuid.New(), uuid.New()

	t.Run("applies the layout and answers with the stored teams", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().Arrange(mock.Anything, mock.Anything, domain.Arrangement{Teams: []domain.TeamArrangement{
			{TeamID: bolu, Agents: []uuid.UUID{second, first}},
			{TeamID: hore, Agents: []uuid.UUID{}},
		}}).Return([]domain.Team{
			{ID: bolu, Name: "Tim Bolu", Kind: "bolu", Agents: []domain.Agent{func() domain.Agent {
				agent := agentFixture()
				agent.ID, agent.TeamID = second, bolu
				return agent
			}()}},
			{ID: hore, Name: "Tim Hore", Kind: "hore"},
		}, nil).Once()

		app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New(), role: "owner"})
		body := `{"teams":[{"team_id":"` + bolu.String() + `","agent_ids":["` + second.String() + `","` + first.String() + `"]},{"team_id":"` + hore.String() + `","agent_ids":[]}]}`
		resp := do(t, app, fiber.MethodPut, "/api/agents/order", body)

		require.Equal(t, fiber.StatusOK, resp.Code, resp.Body.String())
		require.Contains(t, resp.Body.String(), "Tim Bolu")
	})

	t.Run("refuses a layout that is not the registry", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().Arrange(mock.Anything, mock.Anything, mock.Anything).
			Return(nil, domain.ErrOrderIncomplete).Once()

		app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New(), role: "owner"})
		resp := do(t, app, fiber.MethodPut, "/api/agents/order", `{"teams":[{"team_id":"`+bolu.String()+`","agent_ids":[]}]}`)

		require.Equal(t, fiber.StatusBadRequest, resp.Code)
		require.Contains(t, resp.Body.String(), "order_incomplete")
	})

	t.Run("refuses an id that is not a uuid", func(t *testing.T) {
		service := mocks.NewService(t)
		app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New(), role: "owner"})

		resp := do(t, app, fiber.MethodPut, "/api/agents/order", `{"teams":[{"team_id":"nope","agent_ids":[]}]}`)
		require.Equal(t, fiber.StatusBadRequest, resp.Code)
		require.Contains(t, resp.Body.String(), "invalid_request")
	})
}

func TestTeamsHandler(t *testing.T) {
	service := mocks.NewService(t)
	agent := agentFixture()

	service.EXPECT().Teams(mock.Anything, mock.Anything).Return([]domain.Team{
		{ID: uuid.New(), Name: "Tim Bolu", Kind: "bolu", Agents: []domain.Agent{agent}},
		{ID: uuid.New(), Name: "Tim Hore", Kind: "hore"},
	}, nil).Once()

	app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New(), role: "member"})
	resp := do(t, app, fiber.MethodGet, "/api/teams", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), "Tim Bolu")
	require.Contains(t, resp.Body.String(), "Tim Hore")
}

func TestTemplatesHandler(t *testing.T) {
	service := mocks.NewService(t)
	service.EXPECT().Templates(mock.Anything).Return([]domain.Template{
		{Key: "oren", Name: "Oren", Role: "Penjualan", Tools: []string{"gmail.search"}},
	}, nil).Once()

	app := newTestApp(t, service, identity{userID: uuid.New()})
	resp := do(t, app, fiber.MethodGet, "/api/agents/templates", "")

	require.Equal(t, fiber.StatusOK, resp.Code, resp.Body.String())
	require.Contains(t, resp.Body.String(), "oren")
}

func TestToolCatalogHandler(t *testing.T) {
	service := mocks.NewService(t)
	service.EXPECT().ToolCatalog(mock.Anything).Return([]domain.Tool{
		{Name: "gmail.send", Integration: "gmail", Label: domain.LabelWriteExternal},
	}, nil).Once()

	app := newTestApp(t, service, identity{userID: uuid.New()})
	resp := do(t, app, fiber.MethodGet, "/api/agents/tool-catalog", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), "write_external")
}

func TestIntegrationsHandler(t *testing.T) {
	service := mocks.NewService(t)
	service.EXPECT().Integrations(mock.Anything, mock.Anything).Return([]domain.Integration{
		{ID: uuid.New(), App: "gmail", AccountLabel: "toko@example.com", Status: "connected"},
	}, nil).Once()

	app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New()})
	resp := do(t, app, fiber.MethodGet, "/api/agents/integrations", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), "gmail")
}

func TestGrantsHandler(t *testing.T) {
	t.Run("lists the grants", func(t *testing.T) {
		service := mocks.NewService(t)
		agent := agentFixture()

		service.EXPECT().Grants(mock.Anything, mock.Anything, mock.Anything).Return([]domain.Grant{
			{ID: uuid.New(), AgentID: agent.ID, IntegrationID: uuid.New(), App: "gmail", Permission: domain.PermissionRead},
		}, nil).Once()

		app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New()})
		resp := do(t, app, fiber.MethodGet, "/api/agents/"+agent.ID.String()+"/grants", "")

		require.Equal(t, fiber.StatusOK, resp.Code)
		require.Contains(t, resp.Body.String(), "read")
	})

	t.Run("saves a grant", func(t *testing.T) {
		service := mocks.NewService(t)
		agent := agentFixture()
		integrationID := uuid.New()

		service.EXPECT().SetGrant(mock.Anything, mock.Anything, mock.Anything, integrationID, domain.PermissionReadWrite).
			Return(domain.Grant{AgentID: agent.ID, IntegrationID: integrationID, App: "gmail", Permission: domain.PermissionReadWrite}, nil).Once()

		app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New(), role: "owner"})
		resp := do(t, app, fiber.MethodPut, "/api/agents/"+agent.ID.String()+"/grants",
			`{"integration_id":"`+integrationID.String()+`","permission":"read_write"}`)

		require.Equal(t, fiber.StatusOK, resp.Code)
	})

	t.Run("400 for an unknown permission", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().SetGrant(mock.Anything, mock.Anything, mock.Anything, mock.Anything, "write").
			Return(domain.Grant{}, domain.ErrInvalidPermission).Once()

		app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New(), role: "owner"})
		resp := do(t, app, fiber.MethodPut, "/api/agents/"+uuid.NewString()+"/grants",
			`{"integration_id":"`+uuid.NewString()+`","permission":"write"}`)

		require.Equal(t, fiber.StatusBadRequest, resp.Code)
		require.Contains(t, resp.Body.String(), "invalid_permission")
	})

	t.Run("400 for a malformed integration id", func(t *testing.T) {
		service := mocks.NewService(t)
		app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New(), role: "owner"})

		resp := do(t, app, fiber.MethodPut, "/api/agents/"+uuid.NewString()+"/grants",
			`{"integration_id":"nope","permission":"read"}`)
		require.Equal(t, fiber.StatusBadRequest, resp.Code)
	})

	t.Run("revokes a grant", func(t *testing.T) {
		service := mocks.NewService(t)
		service.EXPECT().DeleteGrant(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()

		app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New(), role: "owner"})
		resp := do(t, app, fiber.MethodDelete, "/api/agents/"+uuid.NewString()+"/grants/"+uuid.NewString(), "")

		require.Equal(t, fiber.StatusOK, resp.Code)
	})

	t.Run("400 for a malformed path id", func(t *testing.T) {
		service := mocks.NewService(t)
		app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New(), role: "owner"})

		resp := do(t, app, fiber.MethodDelete, "/api/agents/"+uuid.NewString()+"/grants/nope", "")
		require.Equal(t, fiber.StatusBadRequest, resp.Code)
	})
}

func TestAllowedToolsHandler(t *testing.T) {
	service := mocks.NewService(t)
	agent := agentFixture()

	service.EXPECT().AllowedTools(mock.Anything, mock.Anything, mock.Anything).Return([]domain.Tool{
		{Name: "gmail.search", Integration: "gmail", Label: domain.LabelRead},
	}, nil).Once()

	app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New()})
	resp := do(t, app, fiber.MethodGet, "/api/agents/"+agent.ID.String()+"/tools", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), "gmail.search")
}

func TestErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		domainErr  error
		wantStatus int
		path       string
		method     string
		body       string
		call       func(*mocks.Service)
	}{
		{
			name:       "team not found",
			domainErr:  domain.ErrTeamNotFound,
			wantStatus: fiber.StatusNotFound,
			path:       "/api/agents",
			method:     fiber.MethodPost,
			body:       `{"team_id":"` + uuid.NewString() + `","name":"X"}`,
			call: func(m *mocks.Service) {
				m.EXPECT().Create(mock.Anything, mock.Anything, mock.Anything).Return(domain.Agent{}, domain.ErrTeamNotFound).Once()
			},
		},
		{
			name:       "integration not found",
			domainErr:  domain.ErrIntegrationNotFound,
			wantStatus: fiber.StatusNotFound,
			path:       "/api/agents/" + uuid.NewString() + "/grants",
			method:     fiber.MethodPut,
			body:       `{"integration_id":"` + uuid.NewString() + `","permission":"read"}`,
			call: func(m *mocks.Service) {
				m.EXPECT().SetGrant(mock.Anything, mock.Anything, mock.Anything, mock.Anything, domain.PermissionRead).
					Return(domain.Grant{}, domain.ErrIntegrationNotFound).Once()
			},
		},
		{
			name:       "internal failure",
			domainErr:  errors.New("database is down"),
			wantStatus: fiber.StatusInternalServerError,
			path:       "/api/agents",
			method:     fiber.MethodGet,
			call: func(m *mocks.Service) {
				m.EXPECT().List(mock.Anything, mock.Anything).Return(nil, errors.New("database is down")).Once()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := mocks.NewService(t)
			tt.call(service)

			app := newTestApp(t, service, identity{userID: uuid.New(), workspaceID: uuid.New(), role: "owner"})
			resp := do(t, app, tt.method, tt.path, tt.body)

			require.Equal(t, tt.wantStatus, resp.Code)
		})
	}
}
