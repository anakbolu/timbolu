package integration

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	agentdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/domain"
	agentrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/repository"
	agentservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/service"
	"github.com/robzlabz/employeebot/packages/platform/database"
)

// A workspace is created empty, so a test that needs Bolu creates them the way a
// person does. This helper is that act, once, for the fixtures that need a team
// to work with rather than to test.

// seedAgents creates count Bolu in the workspace's first team and returns them.
//
// The names are stable so a test can refer to one by name rather than by position,
// which is what a person does on the screen.
func seedAgents(t *testing.T, pool *database.Pool, workspaceID uuid.UUID, names ...string) []agentdomain.Agent {
	t.Helper()

	if len(names) == 0 {
		names = []string{"Oren", "Biru"}
	}

	repo := agentrepo.New(pool)
	scope := agentdomain.Scope{UserID: uuid.New(), WorkspaceID: workspaceID}

	teams, err := repo.Teams(t.Context(), scope)
	require.NoError(t, err)
	require.NotEmpty(t, teams, "onboarding creates the workspace's teams")

	created := make([]agentdomain.Agent, 0, len(names))
	for _, name := range names {
		agent, err := repo.Create(t.Context(), scope, agentdomain.NewAgent{
			Name:   name,
			Role:   "Anggota tim",
			TeamID: teams[0].ID,
		})
		require.NoError(t, err)
		created = append(created, agent)
	}
	return created
}

// seedAgent creates one Bolu and returns its id, for the fixtures that need a
// single agent to point something at.
func seedAgent(t *testing.T, pool *database.Pool, workspaceID uuid.UUID) uuid.UUID {
	t.Helper()

	return seedAgents(t, pool, workspaceID, "Oren")[0].ID
}

// seedTemplateAgents creates one Bolu per template key, the way a person picks a
// profile on the screen that offers them.
//
// A fixture uses it when the test is about what a populated workspace does; the
// product itself starts empty.
func seedTemplateAgents(t *testing.T, pool *database.Pool, workspaceID uuid.UUID, keys ...string) []agentdomain.Agent {
	t.Helper()

	if len(keys) == 0 {
		keys = []string{"oren", "biru", "lila", "ijo", "pinky", "kunyit"}
	}

	repo := agentrepo.New(pool)
	service := agentservice.New(agentservice.Deps{Repository: repo})
	scope := agentdomain.Scope{UserID: uuid.New(), WorkspaceID: workspaceID}

	teams, err := service.Teams(t.Context(), scope)
	require.NoError(t, err)
	require.NotEmpty(t, teams)

	created := make([]agentdomain.Agent, 0, len(keys))
	for _, key := range keys {
		// Through the service, because resolving a template is the service's job:
		// a person picks a profile and gets a Bolu shaped like it.
		agent, err := service.Create(t.Context(), scope, agentdomain.CreateRequest{
			TeamID:      teams[0].ID,
			TemplateKey: key,
		})
		require.NoError(t, err)
		created = append(created, agent)
	}
	return created
}
