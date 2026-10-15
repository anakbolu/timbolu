package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	agentdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/domain"
	agentrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/repository"
	agentservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/service"
	authrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/repository"
	workspacedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	workspacerepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/repository"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
)

// agentFixture is one onboarded workspace with its copied Bolu.
type agentFixture struct {
	repo      *agentrepo.Repository
	service   agentdomain.Service
	pool      *database.Pool
	workspace workspacedomain.Workspace
	userID    uuid.UUID
	scope     agentdomain.Scope
}

func newAgentFixture(t *testing.T, dsn string) *agentFixture {
	t.Helper()

	pool := poolFor(t, dsn)
	agents := agentrepo.New(pool)
	workspaces := workspacerepo.New(pool)

	user, err := authrepo.New(pool.PgxPool()).CreateUser(t.Context(), uuid.NewString()+"@example.com", "$argon2id$hash")
	require.NoError(t, err)

	result, err := workspaces.Onboard(t.Context(), user.ID, workspacedomain.OnboardRequest{
		Name:          "Toko Sinar",
		BusinessField: "Retail",
		Timezone:      "Asia/Jakarta",
	})
	require.NoError(t, err)

	scope := agentdomain.Scope{UserID: user.ID, WorkspaceID: result.Workspace.ID}

	return &agentFixture{
		repo:      agents,
		service:   agentservice.New(agentservice.Deps{Repository: agents}),
		pool:      pool,
		workspace: result.Workspace,
		userID:    user.ID,
		scope:     scope,
	}
}

// TestTemplatesAreSeeded is the E3.1 gate: a clean database has the six Bolu
// profiles.
func TestTemplatesAreSeeded(t *testing.T) {
	pool := poolFor(t, appDatabase(t))

	var templates int
	require.NoError(t, pool.PgxPool().QueryRow(t.Context(),
		"SELECT count(*) FROM agent_templates").Scan(&templates))
	require.Equal(t, 6, templates, "the six Bolu templates must be seeded")

	var tools int
	require.NoError(t, pool.PgxPool().QueryRow(t.Context(),
		"SELECT count(*) FROM tool_catalog").Scan(&tools))
	require.Positive(t, tools, "the tool catalogue must be seeded")

	// Every label in the catalogue is one of the three the policy engine knows.
	var unknown int
	require.NoError(t, pool.PgxPool().QueryRow(t.Context(),
		"SELECT count(*) FROM tool_catalog WHERE label NOT IN ('read','write_internal','write_external')").Scan(&unknown))
	require.Zero(t, unknown)

	// RLS is on for the tenant tables the registry owns.
	for _, table := range []string{"agents", "agent_grants"} {
		var enabled bool
		require.NoError(t, pool.PgxPool().QueryRow(t.Context(),
			"SELECT relrowsecurity FROM pg_class WHERE relname = $1", table).Scan(&enabled))
		require.True(t, enabled, "%s must have Row Level Security enabled", table)
	}
}

// TestANewWorkspaceStartsEmpty is the MVP's shape: onboarding creates the two
// teams and no Bolu. A team full of strangers somebody did not ask for is harder
// to understand than an empty one, and the screen that says it is empty is where
// the first one is made.
func TestANewWorkspaceStartsEmpty(t *testing.T) {
	fixture := newAgentFixture(t, appDatabase(t))
	ctx := t.Context()

	agents, err := fixture.service.List(ctx, fixture.scope)
	require.NoError(t, err)
	require.Empty(t, agents, "a new workspace starts with no Bolu")

	// The teams exist, because a Bolu has to belong to one and a person should
	// not have to invent a team before inventing a colleague.
	teams, err := fixture.service.Teams(ctx, fixture.scope)
	require.NoError(t, err)
	require.Len(t, teams, 2)

	kinds := map[string]string{}
	for _, team := range teams {
		kinds[team.Kind] = team.Name
	}
	require.Equal(t, "Tim Bolu", kinds["bolu"])
	require.Equal(t, "Tim Hore", kinds["hore"])

	// And a Bolu can be created immediately, which is what the empty screen
	// offers.
	created, err := fixture.service.Create(ctx, fixture.scope, agentdomain.CreateRequest{
		TeamID: teams[0].ID,
		Name:   "Oren",
	})
	require.NoError(t, err)
	require.Equal(t, "Oren", created.Name)
	require.Equal(t, "Tim Bolu", created.TeamName)
	require.Equal(t, agentdomain.StoredActive, created.Status)

	agents, err = fixture.service.List(ctx, fixture.scope)
	require.NoError(t, err)
	require.Len(t, agents, 1)
}

// TestTheTemplatesAreStillSeeded keeps the profiles a person can start from: the
// workspace is empty, but "create one like this" still has something to offer.
func TestTheTemplatesAreStillSeeded(t *testing.T) {
	fixture := newAgentFixture(t, appDatabase(t))
	ctx := t.Context()

	templates, err := fixture.service.Templates(ctx)
	require.NoError(t, err)
	require.Len(t, templates, 6)

	names := map[string]bool{}
	for _, template := range templates {
		names[template.Name] = true
	}
	for _, want := range []string{"Oren", "Biru", "Lila", "Ijo", "Pinky", "Kunyit"} {
		require.True(t, names[want], "%s must be offered as a starting point", want)
	}

	// Starting from one copies it, so editing the result leaves the template
	// alone.
	teams, err := fixture.service.Teams(ctx, fixture.scope)
	require.NoError(t, err)

	copied, err := fixture.service.Create(ctx, fixture.scope, agentdomain.CreateRequest{
		TeamID:      teams[0].ID,
		TemplateKey: "oren",
	})
	require.NoError(t, err)
	require.NotEmpty(t, copied.TemplateKey)

	updated, err := fixture.service.Update(ctx, fixture.scope, copied.ID, agentdomain.UpdateRequest{
		Name:    "Oren Punya Toko Sinar",
		Persona: "Persona khusus workspace ini.",
	})
	require.NoError(t, err)
	require.Equal(t, "Oren Punya Toko Sinar", updated.Name)

	template, err := fixture.repo.Template(ctx, copied.TemplateKey)
	require.NoError(t, err)
	require.NotEqual(t, "Oren Punya Toko Sinar", template.Name, "the template is a starting point, not a shared row")
	require.NotEqual(t, "Persona khusus workspace ini.", template.Persona)
}

// TestEditingOneWorkspaceDoesNotTouchAnother is the isolation half of the E3.2
// gate: templates are copied, not shared.
func TestEditingOneWorkspaceDoesNotTouchAnother(t *testing.T) {
	dsn := appDatabase(t)
	first := newAgentFixture(t, dsn)
	second := newAgentFixture(t, dsn)
	ctx := t.Context()

	// Each workspace makes its own Bolu, which is what a person does on the empty
	// screen. Two workspaces creating one is the isolation this checks.
	firstCreated := seedAgents(t, first.pool, first.workspace.ID, "Oren")
	seedAgents(t, second.pool, second.workspace.ID, "Oren")

	firstAgents, err := first.service.List(ctx, first.scope)
	require.NoError(t, err)
	require.Len(t, firstAgents, 1)

	target := firstAgents[0]
	require.Equal(t, firstCreated[0].ID, target.ID)
	updated, err := first.service.Update(ctx, first.scope, target.ID, agentdomain.UpdateRequest{
		Name:    "Oren Punya Toko Sinar",
		Persona: "Persona khusus workspace pertama.",
	})
	require.NoError(t, err)
	require.Equal(t, "Oren Punya Toko Sinar", updated.Name)

	// The other workspace is untouched.
	secondAgents, err := second.service.List(ctx, second.scope)
	require.NoError(t, err)
	require.Len(t, secondAgents, 1)
	for _, agent := range secondAgents {
		require.NotEqual(t, "Oren Punya Toko Sinar", agent.Name, "another workspace must not see the rename")
		require.NotEqual(t, "Persona khusus workspace pertama.", agent.Persona)
	}

	// That a Bolu made from a template leaves the template alone is checked where
	// the templates are, in TestTheTemplatesAreStillSeeded. What this test is
	// about is the boundary between two workspaces.
}

// TestRegistryIsTenantScoped proves the registry cannot be read across
// workspaces, even with a valid agent id.
func TestRegistryIsTenantScoped(t *testing.T) {
	dsn := appDatabase(t)
	first := newAgentFixture(t, dsn)
	second := newAgentFixture(t, dsn)
	ctx := t.Context()

	seedTemplateAgents(t, first.pool, first.workspace.ID)
	seedTemplateAgents(t, second.pool, second.workspace.ID)

	firstAgents, err := first.service.List(ctx, first.scope)
	require.NoError(t, err)
	require.Len(t, firstAgents, 6)

	// Reading the other workspace's Bolu through the first scope finds nothing.
	_, err = first.service.Get(ctx, first.scope, second.scope.WorkspaceID)
	require.ErrorIs(t, err, agentdomain.ErrAgentNotFound)

	secondAgents, err := second.service.List(ctx, second.scope)
	require.NoError(t, err)
	_, err = first.service.Get(ctx, first.scope, secondAgents[0].ID)
	require.ErrorIs(t, err, agentdomain.ErrAgentNotFound, "another workspace's Bolu must be invisible")
}

// TestCreateAndDeleteBolu is the E3.6 gate: adding a Bolu needs no new code, and
// deleting one keeps the history.
func TestCreateAndDeleteBolu(t *testing.T) {
	fixture := newAgentFixture(t, appDatabase(t))
	ctx := t.Context()
	seedTemplateAgents(t, fixture.pool, fixture.workspace.ID)

	teams, err := fixture.service.Teams(ctx, fixture.scope)
	require.NoError(t, err)
	require.Len(t, teams, 2)

	var horeTeam uuid.UUID
	for _, team := range teams {
		if team.Kind == "hore" {
			horeTeam = team.ID
		}
	}
	require.NotEqual(t, uuid.Nil, horeTeam)

	// A new Bolu in Tim Hore, created from scratch.
	created, err := fixture.service.Create(ctx, fixture.scope, agentdomain.CreateRequest{
		TeamID:  horeTeam,
		Name:    "Kak Helper",
		Role:    "Serba bisa",
		Persona: "Membantu pekerjaan kantor harian.",
	})
	require.NoError(t, err)
	require.Equal(t, "Kak Helper", created.Name)
	require.Equal(t, "Tim Hore", created.TeamName)
	require.Equal(t, agentdomain.DisplayIdle, created.Display)

	// And one copied from an existing Bolu.
	copied, err := fixture.service.Create(ctx, fixture.scope, agentdomain.CreateRequest{
		TeamID:   horeTeam,
		CopyFrom: created.ID,
	})
	require.NoError(t, err)
	require.Contains(t, copied.Name, "salinan")
	require.Equal(t, created.Persona, copied.Persona)

	// The registry grew without a deploy.
	agents, err := fixture.service.List(ctx, fixture.scope)
	require.NoError(t, err)
	require.Len(t, agents, 8)

	// Deleting is a soft delete: the list shrinks, the row stays.
	require.NoError(t, fixture.service.Delete(ctx, fixture.scope, copied.ID))

	agents, err = fixture.service.List(ctx, fixture.scope)
	require.NoError(t, err)
	require.Len(t, agents, 7)

	// The row is still there, which is what keeps the task history readable.
	// The read runs in the tenant scope because RLS hides the row otherwise.
	var softDeleted bool
	require.NoError(t, fixture.pool.InScopeRead(ctx, database.Scope{
		UserID:      fixture.scope.UserID,
		WorkspaceID: fixture.scope.WorkspaceID,
	}, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT deleted_at IS NOT NULL FROM agents WHERE id = $1", copied.ID).Scan(&softDeleted)
	}))
	require.True(t, softDeleted, "deleting must mark the row, not remove it")
}

// TestRestingBoluTakesNoNewWork is the E3.4 gate.
func TestRestingBoluTakesNoNewWork(t *testing.T) {
	fixture := newAgentFixture(t, appDatabase(t))
	ctx := t.Context()
	seedTemplateAgents(t, fixture.pool, fixture.workspace.ID)

	agents, err := fixture.service.List(ctx, fixture.scope)
	require.NoError(t, err)
	target := agents[0]

	ok, err := fixture.service.AcceptsNewTasks(ctx, fixture.scope, target.ID)
	require.NoError(t, err)
	require.True(t, ok)

	resting, err := fixture.service.SetStatus(ctx, fixture.scope, target.ID, agentdomain.StoredResting)
	require.NoError(t, err)
	require.Equal(t, agentdomain.StoredResting, resting.Status)
	require.Equal(t, agentdomain.DisplayResting, resting.Display)

	ok, err = fixture.service.AcceptsNewTasks(ctx, fixture.scope, target.ID)
	require.NoError(t, err)
	require.False(t, ok, "a resting Bolu must not accept new tasks")
}

// TestGrantsDecideTheToolList is the E3.5 gate: no grant, no *connected* tool; a
// read grant keeps only the read-labelled ones.
//
// The rule is about the tools that reach a provider. A tool that belongs to the
// workspace itself — the catalogue it sells from, the order it records — needs no
// account to be granted and is offered to any Bolu whose profile asks for it, which
// is what `needs_connection = false` says on the catalogue row. So the assertion is
// per kind rather than "the list is empty": a list containing a connected tool the
// Bolu was never granted is the failure this test exists for, and it would hide
// behind an emptiness check that the workspace tools now break.
func TestGrantsDecideTheToolList(t *testing.T) {
	fixture := newAgentFixture(t, appDatabase(t))
	ctx := t.Context()
	pool := fixture.pool

	seedTemplateAgents(t, pool, fixture.workspace.ID)

	agents, err := fixture.service.List(ctx, fixture.scope)
	require.NoError(t, err)

	var oren agentdomain.Agent
	for _, agent := range agents {
		if agent.Name == "Oren" {
			oren = agent
		}
	}
	require.NotEmpty(t, oren.TemplateKey)
	require.Contains(t, oren.Tools, "gmail.send", "the template asks for the send tool")

	// Without a connected integration there is nothing to grant, so no tool that
	// reaches a provider may appear — and the workspace's own tools still do, because
	// they need no account.
	tools, err := fixture.service.AllowedTools(ctx, fixture.scope, oren.ID)
	require.NoError(t, err)
	for _, tool := range tools {
		require.False(t, tool.NeedsConnection,
			"without a grant only a workspace tool may reach the model, not %s", tool.Name)
	}
	require.Contains(t, toolNames(tools), "order.record_order",
		"the workspace's own tools need no account and are offered anyway")

	// Connect a Gmail integration and grant read-only access.
	integrationID := uuid.New()
	require.NoError(t, pool.InScope(ctx, database.Scope{
		UserID:      fixture.scope.UserID,
		WorkspaceID: fixture.scope.WorkspaceID,
	}, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			"INSERT INTO integrations (id, workspace_id, app, account_label, status) VALUES ($1, $2, 'gmail', 'toko@example.com', 'connected')",
			integrationID, fixture.scope.WorkspaceID)
		return err
	}))

	grant, err := fixture.service.SetGrant(ctx, fixture.scope, oren.ID, integrationID, agentdomain.PermissionRead)
	require.NoError(t, err)
	require.Equal(t, agentdomain.PermissionRead, grant.Permission)

	tools, err = fixture.service.AllowedTools(ctx, fixture.scope, oren.ID)
	require.NoError(t, err)
	require.NotEmpty(t, tools)

	for _, tool := range tools {
		if !tool.NeedsConnection {
			continue
		}
		require.Equal(t, agentdomain.LabelRead, tool.Label,
			"a read-only grant must not expose %s", tool.Name)
	}
	names := toolNames(tools)
	require.True(t, names["gmail.search"], "the read tool must be available")
	require.False(t, names["gmail.send"], "the write_external tool must be withheld")
	require.False(t, names["gmail.create_draft"], "a write tool must be withheld")

	// Upgrading to read-write exposes the write tools.
	_, err = fixture.service.SetGrant(ctx, fixture.scope, oren.ID, integrationID, agentdomain.PermissionReadWrite)
	require.NoError(t, err)

	tools, err = fixture.service.AllowedTools(ctx, fixture.scope, oren.ID)
	require.NoError(t, err)
	names = toolNames(tools)
	require.True(t, names["gmail.send"])

	// Revoking takes them away again on the next read.
	require.NoError(t, fixture.service.DeleteGrant(ctx, fixture.scope, oren.ID, integrationID))

	tools, err = fixture.service.AllowedTools(ctx, fixture.scope, oren.ID)
	require.NoError(t, err)
	for _, tool := range tools {
		require.False(t, tool.NeedsConnection,
			"revoking the grant must remove every connected tool, including %s", tool.Name)
	}
	require.NotContains(t, toolNames(tools), "gmail.search")
}

// toolNames is the set of tool names in a list, which is what the assertions above
// ask about: whether a tool is offered, not where it was in the order.
func toolNames(tools []agentdomain.Tool) map[string]bool {
	names := make(map[string]bool, len(tools))
	for _, tool := range tools {
		names[tool.Name] = true
	}

	return names
}

// TestAgentRepositoryWithoutAPool keeps the unconfigured state an error.
func TestAgentRepositoryWithoutAPool(t *testing.T) {
	ctx := context.Background()
	repo := agentrepo.New(nil)
	scope := agentdomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}

	_, err := repo.List(ctx, scope)
	require.ErrorContains(t, err, "not configured")

	_, err = repo.Get(ctx, scope, uuid.New())
	require.ErrorContains(t, err, "not configured")

	_, err = repo.Create(ctx, scope, agentdomain.NewAgent{Name: "X"})
	require.ErrorContains(t, err, "not configured")

	_, err = repo.Update(ctx, scope, uuid.New(), agentdomain.UpdateRequest{Name: "X"})
	require.ErrorContains(t, err, "not configured")

	_, err = repo.SetStatus(ctx, scope, uuid.New(), agentdomain.StoredActive)
	require.ErrorContains(t, err, "not configured")

	require.ErrorContains(t, repo.Delete(ctx, scope, uuid.New()), "not configured")

	_, err = repo.Activity(ctx, scope)
	require.ErrorContains(t, err, "not configured")

	_, err = repo.ActivityFor(ctx, scope, uuid.New())
	require.ErrorContains(t, err, "not configured")

	_, err = repo.ReasonFor(ctx, scope, uuid.New())
	require.ErrorContains(t, err, "not configured")

	_, err = repo.RunningTasks(ctx, scope, uuid.New())
	require.ErrorContains(t, err, "not configured")

	_, err = repo.CountLive(ctx, scope)
	require.ErrorContains(t, err, "not configured")

	_, err = repo.Teams(ctx, scope)
	require.ErrorContains(t, err, "not configured")

	_, err = repo.Templates(ctx)
	require.ErrorContains(t, err, "not configured")

	_, err = repo.Grants(ctx, scope, uuid.New())
	require.ErrorContains(t, err, "not configured")

	_, err = repo.SetGrant(ctx, scope, uuid.New(), uuid.New(), agentdomain.PermissionRead)
	require.ErrorContains(t, err, "not configured")

	require.ErrorContains(t, repo.DeleteGrant(ctx, scope, uuid.New(), uuid.New()), "not configured")

	_, err = repo.AllowedTools(ctx, scope, uuid.New())
	require.ErrorContains(t, err, "not configured")

	_, err = repo.ToolCatalog(ctx)
	require.ErrorContains(t, err, "not configured")

	_, err = repo.Integrations(ctx, scope)
	require.ErrorContains(t, err, "not configured")
}

// TestArrangeMovesABoluUpAndAcrossTeams is the sidebar's drag against a real
// database: the positions are stored, they survive a reload, and a Bolu can change
// team in the same write.
func TestArrangeMovesABoluUpAndAcrossTeams(t *testing.T) {
	fixture := newAgentFixture(t, appDatabase(t))
	ctx := t.Context()

	created := seedAgents(t, fixture.pool, fixture.workspace.ID, "Oren", "Biru", "Lila")
	teams, err := fixture.service.Teams(ctx, fixture.scope)
	require.NoError(t, err)

	bolu, hore := teams[0], teams[1]
	require.Len(t, bolu.Agents, 3, "the seeded Bolu land in the first team")
	require.Equal(t, []string{"Oren", "Biru", "Lila"}, names(bolu.Agents), "a new Bolu lands at the end")

	// Lila moves to the top of Tim Bolu and Oren moves into Tim Hore, which is one
	// layout: the list as it should read.
	_, err = fixture.service.Arrange(ctx, fixture.scope, agentdomain.Arrangement{Teams: []agentdomain.TeamArrangement{
		{TeamID: bolu.ID, Agents: []uuid.UUID{created[2].ID, created[1].ID}},
		{TeamID: hore.ID, Agents: []uuid.UUID{created[0].ID}},
	}})
	require.NoError(t, err)

	// Read back through a fresh repository, which is what a reload does.
	reloaded, err := agentrepo.New(fixture.pool).Teams(ctx, fixture.scope)
	require.NoError(t, err)
	require.Equal(t, []string{"Lila", "Biru"}, names(reloaded[0].Agents))
	require.Equal(t, []string{"Oren"}, names(reloaded[1].Agents))

	// The team a Bolu moved to is the one it now reports, not the one it was made
	// in: the arrangement is a move, not a note beside it.
	moved, err := fixture.service.Get(ctx, fixture.scope, created[0].ID)
	require.NoError(t, err)
	require.Equal(t, hore.ID, moved.TeamID)
	require.Equal(t, "Tim Hore", moved.TeamName)
	require.Equal(t, int32(0), moved.Position)

	// A Bolu made afterwards lands at the end of its team, not in the gap the move
	// left behind.
	fresh, err := fixture.service.Create(ctx, fixture.scope, agentdomain.CreateRequest{TeamID: bolu.ID, Name: "Kunyit"})
	require.NoError(t, err)
	require.Equal(t, int32(2), fresh.Position)

	reloaded, err = fixture.service.Teams(ctx, fixture.scope)
	require.NoError(t, err)
	require.Equal(t, []string{"Lila", "Biru", "Kunyit"}, names(reloaded[0].Agents))
}

// TestArrangeRefusesAnIncompleteLayoutAgainstTheDatabase keeps the service's own
// check honest: the registry the database returns is what the layout is compared
// against, so a Bolu of the same workspace that the client forgot is a refusal
// rather than a row left at its old position.
func TestArrangeRefusesAnIncompleteLayoutAgainstTheDatabase(t *testing.T) {
	fixture := newAgentFixture(t, appDatabase(t))
	ctx := t.Context()

	created := seedAgents(t, fixture.pool, fixture.workspace.ID, "Oren", "Biru")
	teams, err := fixture.service.Teams(ctx, fixture.scope)
	require.NoError(t, err)

	_, err = fixture.service.Arrange(ctx, fixture.scope, agentdomain.Arrangement{Teams: []agentdomain.TeamArrangement{
		{TeamID: teams[0].ID, Agents: []uuid.UUID{created[0].ID}},
		{TeamID: teams[1].ID, Agents: []uuid.UUID{}},
	}})
	require.ErrorIs(t, err, agentdomain.ErrOrderIncomplete)

	// Nothing moved, which is the point of refusing the whole layout.
	reloaded, err := fixture.service.Teams(ctx, fixture.scope)
	require.NoError(t, err)
	require.Equal(t, []string{"Oren", "Biru"}, names(reloaded[0].Agents))
}

// names reads a team's Bolu in the order they are stored.
func names(agents []agentdomain.Agent) []string {
	list := make([]string, 0, len(agents))
	for _, agent := range agents {
		list = append(list, agent.Name)
	}
	return list
}

// TestProvisionerNeedsSeededTemplates keeps a missing seed an explicit failure
// rather than a workspace with no Bolu.
