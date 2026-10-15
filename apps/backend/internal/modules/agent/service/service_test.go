package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/domain/mocks"
)

func scope() domain.Scope {
	return domain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}
}

func agentFixture(overrides ...func(*domain.Agent)) domain.Agent {
	agent := domain.Agent{
		ID:          uuid.New(),
		WorkspaceID: uuid.New(),
		TeamID:      uuid.New(),
		TeamName:    "Tim Bolu",
		TeamKind:    "bolu",
		Name:        "Oren",
		Role:        "Penjualan",
		Persona:     "Menangani pesanan dengan ramah.",
		Tone:        "hangat",
		Shape:       "circle",
		Color:       "#F97316",
		Status:      domain.StoredActive,
		TemplateKey: "oren",
		Tools:       []string{"gmail.search"},
	}
	for _, override := range overrides {
		override(&agent)
	}
	return agent
}

// TestDeriveStatus is the four-case derivation from the task: the display status
// is computed, never stored.
func TestDeriveStatus(t *testing.T) {
	tests := []struct {
		name     string
		stored   string
		activity domain.Activity
		want     string
	}{
		{name: "a task in flight means working", stored: domain.StoredActive, activity: domain.Activity{Working: true}, want: domain.DisplayWorking},
		{name: "a pending draft means waiting", stored: domain.StoredActive, activity: domain.Activity{Waiting: true}, want: domain.DisplayWaiting},
		{name: "nothing in flight means idle", stored: domain.StoredActive, activity: domain.Activity{}, want: domain.DisplayIdle},
		{name: "the rest switch wins", stored: domain.StoredResting, activity: domain.Activity{}, want: domain.DisplayResting},
		{name: "resting outranks a running task", stored: domain.StoredResting, activity: domain.Activity{Working: true}, want: domain.DisplayResting},
		{name: "work outranks a waiting draft", stored: domain.StoredActive, activity: domain.Activity{Working: true, Waiting: true}, want: domain.DisplayWorking},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, domain.DeriveStatus(tt.stored, tt.activity))
		})
	}
}

// TestRestingAgentsTakeNoNewWork is the rule the task runtime relies on.
func TestRestingAgentsTakeNoNewWork(t *testing.T) {
	require.True(t, domain.AcceptsNewTasks(domain.StoredActive))
	require.False(t, domain.AcceptsNewTasks(domain.StoredResting))
}

func TestListDecoratesTheDisplayStatus(t *testing.T) {
	h := newHarness(t)
	s := scope()

	working := agentFixture()
	waiting := agentFixture()
	idle := agentFixture()
	resting := agentFixture(func(a *domain.Agent) { a.Status = domain.StoredResting })

	h.repo.EXPECT().List(mock.Anything, s).Return([]domain.Agent{working, waiting, idle, resting}, nil).Once()
	h.repo.EXPECT().Activity(mock.Anything, s).Return(map[uuid.UUID]domain.Activity{
		working.ID: {Working: true},
		waiting.ID: {Waiting: true},
	}, nil).Once()

	agents, err := h.service.List(context.Background(), s)
	require.NoError(t, err)
	require.Len(t, agents, 4)

	byID := map[uuid.UUID]string{}
	for _, agent := range agents {
		byID[agent.ID] = agent.Display
	}
	require.Equal(t, domain.DisplayWorking, byID[working.ID])
	require.Equal(t, domain.DisplayWaiting, byID[waiting.ID])
	require.Equal(t, domain.DisplayIdle, byID[idle.ID])
	require.Equal(t, domain.DisplayResting, byID[resting.ID])
}

// TestGetExplainsAWorkingStatus keeps the "why" available to the UI.
func TestGetExplainsAWorkingStatus(t *testing.T) {
	h := newHarness(t)
	s := scope()
	agent := agentFixture()
	taskID := uuid.New()

	h.repo.EXPECT().Get(mock.Anything, s, agent.ID).Return(agent, nil).Once()
	h.repo.EXPECT().ActivityFor(mock.Anything, s, agent.ID).Return(domain.Activity{Working: true}, nil).Once()
	h.repo.EXPECT().ReasonFor(mock.Anything, s, agent.ID).Return(&domain.Reason{
		Kind: "task", ID: taskID, Title: "Catat pesanan", Status: "running",
	}, nil).Once()

	got, err := h.service.Get(context.Background(), s, agent.ID)
	require.NoError(t, err)
	require.Equal(t, domain.DisplayWorking, got.Display)
	require.NotNil(t, got.Reason)
	require.Equal(t, taskID, got.Reason.ID)
	require.Equal(t, "task", got.Reason.Kind)
}

// TestGetSkipsTheReasonWhenIdle keeps the extra query off the common path.
func TestGetSkipsTheReasonWhenIdle(t *testing.T) {
	h := newHarness(t)
	s := scope()
	agent := agentFixture()

	h.repo.EXPECT().Get(mock.Anything, s, agent.ID).Return(agent, nil).Once()
	h.repo.EXPECT().ActivityFor(mock.Anything, s, agent.ID).Return(domain.Activity{}, nil).Once()

	got, err := h.service.Get(context.Background(), s, agent.ID)
	require.NoError(t, err)
	require.Equal(t, domain.DisplayIdle, got.Display)
	require.Nil(t, got.Reason, "an idle Bolu has no reason to explain")
}

func TestCreate(t *testing.T) {
	t.Run("from a template", func(t *testing.T) {
		h := newHarness(t)
		s := scope()
		teamID := uuid.New()

		h.repo.EXPECT().AgentTemplate(mock.Anything, "oren").Return(domain.NewAgent{
			Name: "Oren", Role: "Penjualan", Persona: "Ramah.", TemplateKey: "oren", Tools: []string{"gmail.search"},
		}, nil).Once()
		h.repo.EXPECT().Create(mock.Anything, s, mock.Anything).
			RunAndReturn(func(_ context.Context, _ domain.Scope, input domain.NewAgent) (domain.Agent, error) {
				require.Equal(t, teamID, input.TeamID)
				require.Equal(t, "Oren", input.Name)
				require.Equal(t, "circle", input.Shape, "the shape defaults so the UI always has one")
				return agentFixture(), nil
			}).Once()
		h.repo.EXPECT().ActivityFor(mock.Anything, s, mock.Anything).Return(domain.Activity{}, nil).Once()

		created, err := h.service.Create(context.Background(), s, domain.CreateRequest{TeamID: teamID, TemplateKey: "oren"})
		require.NoError(t, err)
		require.Equal(t, "Oren", created.Name)
	})

	t.Run("by copying an existing Bolu", func(t *testing.T) {
		h := newHarness(t)
		s := scope()
		source := agentFixture()

		h.repo.EXPECT().Get(mock.Anything, s, source.ID).Return(source, nil).Once()
		h.repo.EXPECT().Create(mock.Anything, s, mock.Anything).
			RunAndReturn(func(_ context.Context, _ domain.Scope, input domain.NewAgent) (domain.Agent, error) {
				require.Equal(t, source.Persona, input.Persona)
				require.Contains(t, input.Name, "salinan")
				require.Empty(t, input.TemplateKey, "a copy is not a template instance")
				return agentFixture(), nil
			}).Once()
		h.repo.EXPECT().ActivityFor(mock.Anything, s, mock.Anything).Return(domain.Activity{}, nil).Once()

		_, err := h.service.Create(context.Background(), s, domain.CreateRequest{TeamID: uuid.New(), CopyFrom: source.ID})
		require.NoError(t, err)
	})

	t.Run("request fields win over the template", func(t *testing.T) {
		h := newHarness(t)
		s := scope()

		h.repo.EXPECT().AgentTemplate(mock.Anything, "oren").Return(domain.NewAgent{
			Name: "Oren", Persona: "Bawaan.", Shape: "circle",
		}, nil).Once()
		h.repo.EXPECT().Create(mock.Anything, s, mock.Anything).
			RunAndReturn(func(_ context.Context, _ domain.Scope, input domain.NewAgent) (domain.Agent, error) {
				require.Equal(t, "Kasir", input.Name)
				require.Equal(t, "Khusus kasir.", input.Persona)
				return agentFixture(), nil
			}).Once()
		h.repo.EXPECT().ActivityFor(mock.Anything, s, mock.Anything).Return(domain.Activity{}, nil).Once()

		_, err := h.service.Create(context.Background(), s, domain.CreateRequest{
			TeamID: uuid.New(), TemplateKey: "oren", Name: "Kasir", Persona: "Khusus kasir.",
		})
		require.NoError(t, err)
	})

	t.Run("rejects a missing team", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.Create(context.Background(), scope(), domain.CreateRequest{Name: "Tanpa tim"})
		require.ErrorIs(t, err, domain.ErrInvalidInput)
	})

	t.Run("rejects a missing name", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.Create(context.Background(), scope(), domain.CreateRequest{TeamID: uuid.New()})
		require.ErrorIs(t, err, domain.ErrInvalidInput)
	})

	t.Run("enforces the plan limit", func(t *testing.T) {
		h := newHarness(t, func(deps *Deps) {
			limits := mocks.NewPlanLimits(t)
			limits.EXPECT().MaxAgents(mock.Anything, mock.Anything).Return(6, nil).Once()
			deps.Limits = limits
		})
		s := scope()

		h.repo.EXPECT().AgentTemplate(mock.Anything, mock.Anything).Return(domain.NewAgent{Name: "Baru", Shape: "circle"}, nil).Once()
		h.repo.EXPECT().CountLive(mock.Anything, s).Return(int64(6), nil).Once()

		_, err := h.service.Create(context.Background(), s, domain.CreateRequest{TeamID: uuid.New(), TemplateKey: "oren"})
		require.ErrorIs(t, err, domain.ErrAgentLimitReached)
	})
}

func TestUpdate(t *testing.T) {
	t.Run("saves the persona", func(t *testing.T) {
		h := newHarness(t)
		s := scope()
		agent := agentFixture()

		updated := agent
		updated.Persona = "Selalu pakai faktur pajak."
		h.repo.EXPECT().Update(mock.Anything, s, agent.ID, mock.Anything).
			RunAndReturn(func(_ context.Context, _ domain.Scope, _ uuid.UUID, req domain.UpdateRequest) (domain.Agent, error) {
				require.Equal(t, "Selalu pakai faktur pajak.", req.Persona)
				require.Equal(t, "Oren Baru", req.Name, "the name is trimmed")
				return updated, nil
			}).Once()
		h.repo.EXPECT().Get(mock.Anything, s, agent.ID).Return(agent, nil).Once()
		h.repo.EXPECT().ActivityFor(mock.Anything, s, agent.ID).Return(domain.Activity{}, nil).Once()

		result, err := h.service.Update(context.Background(), s, agent.ID, domain.UpdateRequest{
			Name: "Oren Baru", Persona: "Selalu pakai faktur pajak.",
		})
		require.NoError(t, err)
		require.Equal(t, "Selalu pakai faktur pajak.", result.Persona)
	})

	// An edit that only changes the persona must not silently strip the tools.
	t.Run("keeps the current tools when the request omits them", func(t *testing.T) {
		h := newHarness(t)
		s := scope()
		agent := agentFixture()
		agent.Tools = []string{"gmail.search", "gmail.send"}

		h.repo.EXPECT().Get(mock.Anything, s, agent.ID).Return(agent, nil).Once()
		h.repo.EXPECT().Update(mock.Anything, s, agent.ID, mock.Anything).
			RunAndReturn(func(_ context.Context, _ domain.Scope, _ uuid.UUID, req domain.UpdateRequest) (domain.Agent, error) {
				require.Equal(t, agent.Tools, req.Tools)
				return agent, nil
			}).Once()
		h.repo.EXPECT().ActivityFor(mock.Anything, s, agent.ID).Return(domain.Activity{}, nil).Once()

		_, err := h.service.Update(context.Background(), s, agent.ID, domain.UpdateRequest{Name: "Oren"})
		require.NoError(t, err)
	})

	t.Run("rejects a blank name", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.Update(context.Background(), scope(), uuid.New(), domain.UpdateRequest{Name: "   "})
		require.ErrorIs(t, err, domain.ErrInvalidInput)
	})

	t.Run("rejects an over-long persona", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.Update(context.Background(), scope(), uuid.New(), domain.UpdateRequest{
			Name:    "Oren",
			Persona: strings.Repeat("a", PersonaMaxLength+1),
		})
		require.ErrorIs(t, err, domain.ErrInvalidInput)
	})
}

func TestSetStatus(t *testing.T) {
	t.Run("resting", func(t *testing.T) {
		h := newHarness(t)
		s := scope()
		agent := agentFixture()

		resting := agent
		resting.Status = domain.StoredResting
		h.repo.EXPECT().SetStatus(mock.Anything, s, agent.ID, domain.StoredResting).Return(resting, nil).Once()
		h.repo.EXPECT().ActivityFor(mock.Anything, s, agent.ID).Return(domain.Activity{}, nil).Once()

		updated, err := h.service.SetStatus(context.Background(), s, agent.ID, domain.StoredResting)
		require.NoError(t, err)
		require.Equal(t, domain.StoredResting, updated.Status)
		require.Equal(t, domain.DisplayResting, updated.Display)
	})

	t.Run("rejects an unknown status", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.SetStatus(context.Background(), scope(), uuid.New(), "sleepy")
		require.ErrorIs(t, err, domain.ErrInvalidStatus)
	})
}

// TestDeleteRefusesWhileWorking protects a running task from losing its agent.
func TestDeleteRefusesWhileWorking(t *testing.T) {
	h := newHarness(t)
	s := scope()
	agent := agentFixture()

	h.repo.EXPECT().RunningTasks(mock.Anything, s, agent.ID).Return(int64(2), nil).Once()

	err := h.service.Delete(context.Background(), s, agent.ID)
	require.ErrorIs(t, err, domain.ErrAgentHasRunningTask)
}

func TestDeleteMarksTheBoluRemoved(t *testing.T) {
	h := newHarness(t)
	s := scope()
	agent := agentFixture()

	h.repo.EXPECT().RunningTasks(mock.Anything, s, agent.ID).Return(int64(0), nil).Once()
	h.repo.EXPECT().Delete(mock.Anything, s, agent.ID).Return(nil).Once()

	require.NoError(t, h.service.Delete(context.Background(), s, agent.ID))
}

func TestTeamsGroupAgentsByTeam(t *testing.T) {
	h := newHarness(t)
	s := scope()
	boluTeam, horeTeam := uuid.New(), uuid.New()

	h.repo.EXPECT().Teams(mock.Anything, s).Return([]domain.Team{
		{ID: boluTeam, Name: "Tim Bolu", Kind: "bolu", Agents: []domain.Agent{agentFixture()}},
		{ID: horeTeam, Name: "Tim Hore", Kind: "hore"},
	}, nil).Once()
	h.repo.EXPECT().Activity(mock.Anything, s).Return(map[uuid.UUID]domain.Activity{}, nil).Once()

	teams, err := h.service.Teams(context.Background(), s)
	require.NoError(t, err)
	require.Len(t, teams, 2)
	require.Equal(t, "Tim Bolu", teams[0].Name)
	require.Equal(t, domain.DisplayIdle, teams[0].Agents[0].Display)
	require.Empty(t, teams[1].Agents)
}

func TestGrants(t *testing.T) {
	t.Run("refuses an unknown permission", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.SetGrant(context.Background(), scope(), uuid.New(), uuid.New(), "write")
		require.ErrorIs(t, err, domain.ErrInvalidPermission)
	})

	t.Run("refuses a grant for an unknown Bolu", func(t *testing.T) {
		h := newHarness(t)
		s := scope()
		agentID := uuid.New()

		h.repo.EXPECT().Get(mock.Anything, s, agentID).Return(domain.Agent{}, domain.ErrAgentNotFound).Once()

		_, err := h.service.SetGrant(context.Background(), s, agentID, uuid.New(), domain.PermissionRead)
		require.ErrorIs(t, err, domain.ErrAgentNotFound)
	})

	t.Run("saves a read grant", func(t *testing.T) {
		h := newHarness(t)
		s := scope()
		agent := agentFixture()
		integrationID := uuid.New()

		h.repo.EXPECT().Get(mock.Anything, s, agent.ID).Return(agent, nil).Once()
		h.repo.EXPECT().SetGrant(mock.Anything, s, agent.ID, integrationID, domain.PermissionRead).
			Return(domain.Grant{AgentID: agent.ID, IntegrationID: integrationID, App: "gmail", Permission: domain.PermissionRead}, nil).Once()

		grant, err := h.service.SetGrant(context.Background(), s, agent.ID, integrationID, domain.PermissionRead)
		require.NoError(t, err)
		require.Equal(t, "gmail", grant.App)
	})

	t.Run("revokes a grant", func(t *testing.T) {
		h := newHarness(t)
		s := scope()

		h.repo.EXPECT().DeleteGrant(mock.Anything, s, mock.Anything, mock.Anything).Return(nil).Once()

		require.NoError(t, h.service.DeleteGrant(context.Background(), s, uuid.New(), uuid.New()))
	})
}

// TestAllowedToolsIsTheGrantIntersection is the guardrail: a tool only reaches
// the model when its integration was granted to that Bolu.
func TestAllowedToolsIsTheGrantIntersection(t *testing.T) {
	h := newHarness(t)
	s := scope()
	agent := agentFixture()

	h.repo.EXPECT().Get(mock.Anything, s, agent.ID).Return(agent, nil).Once()
	h.repo.EXPECT().AllowedTools(mock.Anything, s, agent.ID).Return([]domain.Tool{
		{Name: "gmail.search", Integration: "gmail", Label: domain.LabelRead},
	}, nil).Once()

	tools, err := h.service.AllowedTools(context.Background(), s, agent.ID)
	require.NoError(t, err)
	require.Len(t, tools, 1)
	require.Equal(t, domain.LabelRead, tools[0].Label)
}

func TestRequiresAWorkspaceScope(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.List(context.Background(), domain.Scope{UserID: uuid.New()})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestAcceptsNewTasksReadsTheSwitch keeps the runtime's gate honest.
func TestAcceptsNewTasksReadsTheSwitch(t *testing.T) {
	h := newHarness(t)
	s := scope()
	resting := agentFixture(func(a *domain.Agent) { a.Status = domain.StoredResting })

	h.repo.EXPECT().Get(mock.Anything, s, resting.ID).Return(resting, nil).Once()

	ok, err := h.service.AcceptsNewTasks(context.Background(), s, resting.ID)
	require.NoError(t, err)
	require.False(t, ok, "a resting Bolu must not accept new tasks")

	h.repo.EXPECT().Get(mock.Anything, s, mock.Anything).Return(agentFixture(), nil).Once()
	ok, err = h.service.AcceptsNewTasks(context.Background(), s, uuid.New())
	require.NoError(t, err)
	require.True(t, ok)
}

func TestScopeErrors(t *testing.T) {
	h := newHarness(t)
	empty := domain.Scope{}

	_, err := h.service.Get(context.Background(), empty, uuid.New())
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Teams(context.Background(), empty)
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	err = h.service.Delete(context.Background(), empty, uuid.New())
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Create(context.Background(), empty, domain.CreateRequest{TeamID: uuid.New(), Name: "X"})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.SetGrant(context.Background(), empty, uuid.New(), uuid.New(), domain.PermissionRead)
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	require.ErrorIs(t, h.service.DeleteGrant(context.Background(), empty, uuid.New(), uuid.New()), domain.ErrInvalidInput)

	_, err = h.service.AllowedTools(context.Background(), empty, uuid.New())
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Integrations(context.Background(), empty)
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Grants(context.Background(), empty, uuid.New())
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Update(context.Background(), empty, uuid.New(), domain.UpdateRequest{Name: "X"})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.SetStatus(context.Background(), empty, uuid.New(), domain.StoredActive)
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

func TestRepositoryErrorsSurface(t *testing.T) {
	h := newHarness(t)
	s := scope()
	agent := agentFixture()

	h.repo.EXPECT().Get(mock.Anything, s, agent.ID).Return(domain.Agent{}, domain.ErrAgentNotFound).Once()
	_, err := h.service.Get(context.Background(), s, agent.ID)
	require.ErrorIs(t, err, domain.ErrAgentNotFound)

	h.repo.EXPECT().Delete(mock.Anything, s, agent.ID).Return(errors.New("database is down")).Once()
	h.repo.EXPECT().RunningTasks(mock.Anything, s, agent.ID).Return(int64(0), nil).Once()
	err = h.service.Delete(context.Background(), s, agent.ID)
	require.Error(t, err)

	h.repo.EXPECT().ToolCatalog(mock.Anything).Return([]domain.Tool{{Name: "gmail.search"}}, nil).Once()
	tools, err := h.service.ToolCatalog(context.Background())
	require.NoError(t, err)
	require.Len(t, tools, 1)

	h.repo.EXPECT().Templates(mock.Anything).Return([]domain.Template{{Key: "oren"}}, nil).Once()
	templates, err := h.service.Templates(context.Background())
	require.NoError(t, err)
	require.Len(t, templates, 1)
}

// TestArrangeWritesTheLayoutTheSidebarAskedFor is the sidebar's move at the service
// level: the layout arrives whole, the positions are the indexes it sent, and the
// answer is the stored teams.
func TestArrangeWritesTheLayoutTheSidebarAskedFor(t *testing.T) {
	h := newHarness(t)
	s := scope()
	bolu, hore := uuid.New(), uuid.New()
	first := agentFixture(func(a *domain.Agent) { a.TeamID = bolu; a.Position = 0 })
	second := agentFixture(func(a *domain.Agent) { a.Name = "Senja"; a.TeamID = bolu; a.Position = 1 })
	third := agentFixture(func(a *domain.Agent) { a.Name = "Kopi"; a.TeamID = hore; a.Position = 0 })

	h.repo.EXPECT().Teams(mock.Anything, s).Return([]domain.Team{
		{ID: bolu, Name: "Tim Bolu", Kind: "bolu", Agents: []domain.Agent{first, second}},
		{ID: hore, Name: "Tim Hore", Kind: "hore", Agents: []domain.Agent{third}},
	}, nil).Once()

	// Kopi moves up into Tim Bolu, and Senja and Oren swap. The whole layout is one
	// request because that is what a drag produces: the list as it should read.
	h.repo.EXPECT().SetPlacements(mock.Anything, s, []domain.Placement{
		{AgentID: third.ID, TeamID: bolu, Position: 0},
		{AgentID: first.ID, TeamID: bolu, Position: 1},
		{AgentID: second.ID, TeamID: bolu, Position: 2},
	}).Return(nil).Once()

	stored := []domain.Team{
		{ID: bolu, Name: "Tim Bolu", Kind: "bolu", Agents: []domain.Agent{third, first, second}},
		{ID: hore, Name: "Tim Hore", Kind: "hore"},
	}
	h.repo.EXPECT().Teams(mock.Anything, s).Return(stored, nil).Once()
	h.repo.EXPECT().Activity(mock.Anything, s).Return(map[uuid.UUID]domain.Activity{}, nil).Once()

	teams, err := h.service.Arrange(context.Background(), s, domain.Arrangement{Teams: []domain.TeamArrangement{
		{TeamID: bolu, Agents: []uuid.UUID{third.ID, first.ID, second.ID}},
		{TeamID: hore},
	}})
	require.NoError(t, err)
	require.Len(t, teams, 2)
	require.Equal(t, third.ID, teams[0].Agents[0].ID, "the answer is the stored order, not the requested one")
	require.Equal(t, domain.DisplayIdle, teams[0].Agents[0].Display, "the derived status is still computed")
}

// TestArrangeRefusesALayoutThatIsNotTheRegistry covers the two ways a client can be
// wrong. A partial layout would leave the Bolu it forgot at its old position, and a
// foreign id would be refused by RLS halfway through the write — so both are
// refused before anything is written, and nothing is.
func TestArrangeRefusesALayoutThatIsNotTheRegistry(t *testing.T) {
	bolu, hore := uuid.New(), uuid.New()
	first := agentFixture(func(a *domain.Agent) { a.TeamID = bolu })
	second := agentFixture(func(a *domain.Agent) { a.Name = "Senja"; a.TeamID = hore })

	registry := []domain.Team{
		{ID: bolu, Name: "Tim Bolu", Kind: "bolu", Agents: []domain.Agent{first}},
		{ID: hore, Name: "Tim Hore", Kind: "hore", Agents: []domain.Agent{second}},
	}

	tests := []struct {
		name        string
		arrangement domain.Arrangement
		message     string
	}{
		{
			name: "a Bolu is missing",
			arrangement: domain.Arrangement{Teams: []domain.TeamArrangement{
				{TeamID: bolu, Agents: []uuid.UUID{first.ID}},
				{TeamID: hore},
			}},
			message: "bolu",
		},
		{
			name: "a team is missing",
			arrangement: domain.Arrangement{Teams: []domain.TeamArrangement{
				{TeamID: bolu, Agents: []uuid.UUID{first.ID, second.ID}},
			}},
			message: "team",
		},
		{
			name: "a Bolu is listed twice",
			arrangement: domain.Arrangement{Teams: []domain.TeamArrangement{
				{TeamID: bolu, Agents: []uuid.UUID{first.ID, second.ID}},
				{TeamID: hore, Agents: []uuid.UUID{first.ID}},
			}},
			message: "twice",
		},
		{
			name: "a stranger is named",
			arrangement: domain.Arrangement{Teams: []domain.TeamArrangement{
				{TeamID: bolu, Agents: []uuid.UUID{first.ID, second.ID, uuid.New()}},
				{TeamID: hore},
			}},
			message: "not in this workspace",
		},
		{
			name: "a team from another workspace is named",
			arrangement: domain.Arrangement{Teams: []domain.TeamArrangement{
				{TeamID: bolu, Agents: []uuid.UUID{first.ID}},
				{TeamID: hore, Agents: []uuid.UUID{second.ID}},
				{TeamID: uuid.New()},
			}},
			message: "not in this workspace",
		},
		{
			name:        "nothing is named at all",
			arrangement: domain.Arrangement{},
			message:     "missing",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			s := scope()
			h.repo.EXPECT().Teams(mock.Anything, s).Return(registry, nil).Once()

			_, err := h.service.Arrange(context.Background(), s, test.arrangement)
			require.ErrorIs(t, err, domain.ErrOrderIncomplete)
			require.Contains(t, err.Error(), test.message)
		})
	}
}

// TestArrangeKeepsTheLayoutOfAnEmptyWorkspace: a workspace with teams and no Bolu
// is a valid layout of nothing, and it must not be refused for the same reason a
// forgotten Bolu is.
func TestArrangeKeepsTheLayoutOfAnEmptyWorkspace(t *testing.T) {
	h := newHarness(t)
	s := scope()
	bolu, hore := uuid.New(), uuid.New()

	h.repo.EXPECT().Teams(mock.Anything, s).Return([]domain.Team{
		{ID: bolu, Name: "Tim Bolu", Kind: "bolu"},
		{ID: hore, Name: "Tim Hore", Kind: "hore"},
	}, nil).Once()
	h.repo.EXPECT().SetPlacements(mock.Anything, s, []domain.Placement{}).Return(nil).Once()
	h.repo.EXPECT().Teams(mock.Anything, s).Return([]domain.Team{
		{ID: bolu, Name: "Tim Bolu", Kind: "bolu"},
		{ID: hore, Name: "Tim Hore", Kind: "hore"},
	}, nil).Once()
	h.repo.EXPECT().Activity(mock.Anything, s).Return(map[uuid.UUID]domain.Activity{}, nil).Once()

	teams, err := h.service.Arrange(context.Background(), s, domain.Arrangement{Teams: []domain.TeamArrangement{
		{TeamID: bolu},
		{TeamID: hore},
	}})
	require.NoError(t, err)
	require.Len(t, teams, 2)
}

// TestArrangeRequiresAWorkspaceScope is the same guard every other write has.
func TestArrangeRequiresAWorkspaceScope(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.Arrange(context.Background(), domain.Scope{}, domain.Arrangement{})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

type harness struct {
	service domain.Service
	repo    *mocks.Repository
}

// TestSetMemoryExtractionFlipsTheSwitch is the E10.5 switch at the service level:
// the column is written here, the memory module reads it, and the answer carries
// the value that landed.
func TestSetMemoryExtractionFlipsTheSwitch(t *testing.T) {
	h := newHarness(t)
	s := scope()
	agent := agentFixture(func(a *domain.Agent) { a.MemoryExtraction = false })

	h.repo.EXPECT().SetMemoryExtraction(mock.Anything, s, agent.ID, false).Return(agent, nil).Once()
	h.repo.EXPECT().ActivityFor(mock.Anything, s, agent.ID).Return(domain.Activity{}, nil).Once()

	updated, err := h.service.SetMemoryExtraction(context.Background(), s, agent.ID, false)
	require.NoError(t, err)
	require.False(t, updated.MemoryExtraction)
	require.Equal(t, domain.DisplayIdle, updated.Display, "the derived status is still computed")

	// A write with no workspace is refused rather than run against whatever
	// tenant the connection happens to carry.
	_, err = h.service.SetMemoryExtraction(context.Background(), domain.Scope{}, agent.ID, true)
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

func newHarness(t *testing.T, opts ...func(*Deps)) *harness {
	t.Helper()

	h := &harness{repo: mocks.NewRepository(t)}

	deps := Deps{Repository: h.repo}
	for _, opt := range opts {
		opt(&deps)
	}

	h.service = New(deps)
	return h
}
