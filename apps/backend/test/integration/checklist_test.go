package integration

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	agentdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/domain"
	agentrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/repository"
	agentservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/service"
	authrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/repository"
	checklistadapter "github.com/robzlabz/employeebot/apps/backend/internal/modules/checklist/adapter"
	checklistdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/checklist/domain"
	checklistrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/checklist/repository"
	checklistservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/checklist/service"
	workspacedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	workspacerepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/repository"
	"github.com/robzlabz/employeebot/packages/platform/database"
)

// This file is EPIC 15's proof against a real Postgres. The claims are the ones a mock would only
// restate, because they are made by the schema rather than by the service:
//
//   - one Bolu holds one active plan, and the partial unique index is what says so — not the service's
//     carefulness, which a second writer would not have;
//   - a task with unfinished subtasks cannot be ticked off, and a subtask cannot have subtasks of its
//     own, both enforced by the trigger on the items table;
//   - a plan is settled by the same statement that ticks its last task, so a plan whose work is done
//     cannot be left open with the Bolu unable to start the next one;
//   - clearing removes the plan and everything in it, which is what frees the Bolu to write another;
//   - plans are tenant data, isolated by policy rather than by a careful query;
//   - the tool catalogue in the database and the adapter's own declaration are the same five tools with
//     the same labels, which is the agreement the adapter's comments promise — and the migration's
//     other half, that every Bolu and template carries them.

// checklistFixture is a workspace of its own with the checklist module over the real database.
//
// The workspace is onboarded through the module that owns it rather than inserted, so its rows live in a
// tenant the product itself could have created — which is what makes the policy assertions here claims
// about this deployment rather than about a test's hand-written insert.
type checklistFixture struct {
	repository *checklistrepo.Repository
	service    *checklistservice.Service
	pool       *database.Pool

	userID    uuid.UUID
	workspace uuid.UUID
	scope     checklistdomain.Scope
	// agent is the Bolu whose plan the tests move.
	agent uuid.UUID
}

// newChecklistFixture onboards a workspace of its own, with one Bolu in it, for one test.
func newChecklistFixture(t *testing.T, dsn string) *checklistFixture {
	t.Helper()

	return checklistFixtureOver(t, poolFor(t, dsn))
}

// checklistFixtureOver builds the fixture over a pool that is already open, so one test can hold two
// workspaces of the same database and compare them — which is what the isolation claim needs.
func checklistFixtureOver(t *testing.T, pool *database.Pool) *checklistFixture {
	t.Helper()

	ctx := t.Context()
	user, err := authrepo.New(pool.PgxPool()).CreateUser(ctx, uuid.NewString()+"@example.com", "$argon2id$hash")
	require.NoError(t, err)

	result, err := workspacerepo.New(pool).Onboard(ctx, user.ID, workspacedomain.OnboardRequest{
		Name:          "Toko Rencana",
		BusinessField: "Retail",
		Timezone:      "Asia/Jakarta",
	})
	require.NoError(t, err)

	repository := checklistrepo.New(pool)

	return &checklistFixture{
		repository: repository,
		service:    checklistservice.New(checklistservice.Deps{Repository: repository}),
		pool:       pool,
		userID:     user.ID,
		workspace:  result.Workspace.ID,
		scope:      checklistdomain.Scope{UserID: user.ID, WorkspaceID: result.Workspace.ID},
		agent:      seedAgent(t, pool, result.Workspace.ID),
	}
}

// write creates a plan through the service, which is the path the tool takes.
func (f *checklistFixture) write(t *testing.T, title string, tasks ...checklistdomain.NewItem) checklistdomain.Checklist {
	t.Helper()

	created, err := f.service.Create(t.Context(), f.scope, checklistdomain.NewChecklist{
		AgentID: f.agent,
		Title:   title,
		Tasks:   tasks,
	})
	require.NoError(t, err)

	return created
}

// active re-reads the Bolu's open plan from the database.
func (f *checklistFixture) active(t *testing.T) checklistdomain.Checklist {
	t.Helper()

	stored, err := f.repository.Active(t.Context(), f.scope, f.agent)
	require.NoError(t, err)

	return stored
}

// TestAPlanRoundTripsInTheOrderItWasWritten: what a Bolu writes is what it reads back — the titles, the
// nesting, the ticks, and the order. The order is stored rather than derived, because two items written
// in one statement share a timestamp to the microsecond and their order would otherwise be whatever the
// database felt like.
func TestAPlanRoundTripsInTheOrderItWasWritten(t *testing.T) {
	fixture := newChecklistFixture(t, appDatabase(t))

	created := fixture.write(t, "Tagih pelanggan",
		checklistdomain.NewItem{Title: "cari harga", Subtasks: []string{"buka katalog", "catat harga"}},
		checklistdomain.NewItem{Title: "kirim tagihan"},
		checklistdomain.NewItem{Title: "catat pembayaran"},
	)
	require.Equal(t, checklistdomain.StatusActive, created.Status)
	require.Nil(t, created.CompletedAt)

	stored := fixture.active(t)
	require.Equal(t, created.ID, stored.ID)
	require.Equal(t, "Tagih pelanggan", stored.Title)
	require.Equal(t, fixture.agent, stored.AgentID)
	require.Equal(t, uuid.Nil, stored.TaskID, "a plan written outside a task carries no attribution")

	require.Len(t, stored.Items, 3)
	require.Equal(t, []string{"cari harga", "kirim tagihan", "catat pembayaran"}, titlesOf(stored))
	require.Equal(t, []string{"buka katalog", "catat harga"}, subtaskTitlesOf(stored.Items[0]))
	require.False(t, stored.Items[0].Done)

	done, total := stored.Counts()
	require.Zero(t, done)
	require.Equal(t, 5, total, "three tasks and two subtasks")

	// And the list a screen reads is the same plan.
	listed, err := fixture.repository.List(t.Context(), fixture.scope, checklistdomain.Filter{})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, stored.ID, listed[0].ID)
	require.Len(t, listed[0].Items, 3)
	require.Equal(t, []string{"buka katalog", "catat harga"}, subtaskTitlesOf(listed[0].Items[0]))
}

// TestOnePlanAtATimeIsTheDatabase: the rule is what makes the list a goal rather than a scratchpad, and
// it is a constraint rather than a check the service remembers to make — so a second row is refused even
// when it is written straight into the table.
func TestOnePlanAtATimeIsTheDatabase(t *testing.T) {
	fixture := newChecklistFixture(t, appDatabase(t))
	ctx := t.Context()

	fixture.write(t, "Rencana pertama", checklistdomain.NewItem{Title: "satu"})

	// Straight at the table, which is the path the service's own check would not cover.
	err := fixture.pool.InScope(ctx, database.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				"INSERT INTO checklists (workspace_id, agent_id, title) VALUES ($1, $2, 'Rencana kedua')",
				fixture.workspace, fixture.agent)

			return err
		})
	require.Error(t, err, "the partial unique index is what refuses a second open plan")
	require.Contains(t, err.Error(), "checklists_agent_active_key")

	// And through the service, where the refusal is a sentence the model can act on.
	_, err = fixture.service.Create(ctx, fixture.scope, checklistdomain.NewChecklist{
		AgentID: fixture.agent,
		Tasks:   []checklistdomain.NewItem{{Title: "dua"}},
	})
	require.ErrorIs(t, err, checklistdomain.ErrActiveExists)

	// The plan that was there is untouched.
	require.Equal(t, "Rencana pertama", fixture.active(t).Title)
}

// TestTickingTheLastTaskSettlesThePlanAndFreesTheNext is the rule the whole module exists for: the plan
// is over when nothing is open, and the Bolu may then write another.
func TestTickingTheLastTaskSettlesThePlanAndFreesTheNext(t *testing.T) {
	fixture := newChecklistFixture(t, appDatabase(t))

	created := fixture.write(t, "Rencana pertama",
		checklistdomain.NewItem{Title: "cari harga", Subtasks: []string{"buka katalog"}},
		checklistdomain.NewItem{Title: "kirim tagihan"},
	)

	// The subtask first, then its task, then the other task: the plan is settled by the last tick.
	for _, id := range []uuid.UUID{
		created.Items[0].Subtasks[0].ID,
		created.Items[0].ID,
		created.Items[1].ID,
	} {
		updated, err := fixture.service.Check(t.Context(), fixture.scope, fixture.agent, id)
		require.NoError(t, err)
		require.Equal(t, fixture.agent, updated.AgentID)
	}

	settled := fixture.activePlan(t)
	require.Equal(t, checklistdomain.StatusDone, settled.Status)
	require.NotNil(t, settled.CompletedAt, "a finished plan says when it finished")
	done, total := settled.Counts()
	require.Equal(t, total, done)

	// And the next plan may be written, which is what the rule was for.
	next := fixture.write(t, "Rencana kedua", checklistdomain.NewItem{Title: "hubungi pelanggan baru"})
	require.Equal(t, checklistdomain.StatusActive, next.Status)

	// The finished one stays readable as history, newest first.
	listed, err := fixture.repository.List(t.Context(), fixture.scope, checklistdomain.Filter{})
	require.NoError(t, err)
	require.Len(t, listed, 2)
	require.Equal(t, next.ID, listed[0].ID)
	require.Equal(t, settled.ID, listed[1].ID)
}

// TestTickingATaskWithOpenSubtasksIsRefusedByTheDatabase: the service refuses it with a sentence, and the
// trigger refuses it as well — which is the part that holds when a future writer forgets to ask.
func TestTickingATaskWithOpenSubtasksIsRefusedByTheDatabase(t *testing.T) {
	fixture := newChecklistFixture(t, appDatabase(t))
	ctx := t.Context()

	created := fixture.write(t, "Rencana",
		checklistdomain.NewItem{Title: "cari harga", Subtasks: []string{"buka katalog"}},
	)

	_, err := fixture.service.Check(ctx, fixture.scope, fixture.agent, created.Items[0].ID)
	require.ErrorIs(t, err, checklistdomain.ErrSubtasksOpen)
	require.Contains(t, err.Error(), "buka katalog")

	// The same tick, straight at the table.
	err = fixture.pool.InScope(ctx, database.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "UPDATE checklist_items SET done = true WHERE id = $1", created.Items[0].ID)

			return err
		})
	require.Error(t, err, "the trigger is the guarantee under the service's check")
	require.Contains(t, err.Error(), "unfinished subtasks")

	// And the subtask itself ticks fine, which is what unblocks the task.
	ticked, err := fixture.service.Check(ctx, fixture.scope, fixture.agent, created.Items[0].Subtasks[0].ID)
	require.NoError(t, err)
	require.True(t, ticked.Items[0].Subtasks[0].Done)
	require.False(t, ticked.Items[0].Done, "ticking the last subtask does not tick the task for you")
}

// TestASubtaskCannotHaveSubtasks: the plan is a list a person scans, not a project tree, so the depth is
// bounded where the rows are written.
func TestASubtaskCannotHaveSubtasks(t *testing.T) {
	fixture := newChecklistFixture(t, appDatabase(t))
	ctx := t.Context()

	created := fixture.write(t, "Rencana",
		checklistdomain.NewItem{Title: "cari harga", Subtasks: []string{"buka katalog"}},
	)

	err := fixture.pool.InScope(ctx, database.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				"INSERT INTO checklist_items (checklist_id, workspace_id, parent_id, title, position) VALUES ($1, $2, $3, 'terlalu dalam', 0)",
				created.ID, fixture.workspace, created.Items[0].Subtasks[0].ID)

			return err
		})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot have its own subtasks")
}

// TestRemovingTheLastOpenTaskSettlesThePlan: the alternative is a Bolu holding a plan with nothing in it
// and no way to start another, which is a worse answer than "the plan is over".
func TestRemovingTheLastOpenTaskSettlesThePlan(t *testing.T) {
	fixture := newChecklistFixture(t, appDatabase(t))
	ctx := t.Context()

	created := fixture.write(t, "Rencana",
		checklistdomain.NewItem{Title: "cari harga"},
		checklistdomain.NewItem{Title: "kirim tagihan"},
	)

	_, err := fixture.service.Check(ctx, fixture.scope, fixture.agent, created.Items[0].ID)
	require.NoError(t, err)

	updated, err := fixture.service.Remove(ctx, fixture.scope, fixture.agent, created.Items[1].ID)
	require.NoError(t, err)
	require.Equal(t, checklistdomain.StatusDone, updated.Status, "nothing is left open, so the plan is over")
	require.Len(t, updated.Items, 1)

	// Removing a task takes its subtasks with it.
	second := fixture.write(t, "Rencana kedua",
		checklistdomain.NewItem{Title: "cari harga", Subtasks: []string{"buka katalog", "catat harga"}},
	)
	_, err = fixture.service.Remove(ctx, fixture.scope, fixture.agent, second.Items[0].ID)
	require.NoError(t, err)

	var remaining int
	require.NoError(t, fixture.pool.InScope(ctx, database.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace},
		func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT count(*) FROM checklist_items WHERE checklist_id = $1", second.ID).Scan(&remaining)
		}))
	require.Zero(t, remaining, "the subtasks went with their task")
}

// TestClearingRemovesThePlanAndFreesTheBolu: clearing is the escape hatch the one-plan rule needs, and it
// is a delete rather than a status — what the Bolu *did* is in `task_steps`, and a plan it decided was
// wrong is not history worth keeping.
func TestClearingRemovesThePlanAndFreesTheBolu(t *testing.T) {
	fixture := newChecklistFixture(t, appDatabase(t))
	ctx := t.Context()

	created := fixture.write(t, "Rencana",
		checklistdomain.NewItem{Title: "cari harga", Subtasks: []string{"buka katalog"}},
	)

	require.NoError(t, fixture.service.Clear(ctx, fixture.scope, fixture.agent))

	_, err := fixture.repository.Active(ctx, fixture.scope, fixture.agent)
	require.ErrorIs(t, err, checklistdomain.ErrNotFound, "the plan is gone, not abandoned")

	var items int
	require.NoError(t, fixture.pool.InScope(ctx, database.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace},
		func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT count(*) FROM checklist_items WHERE checklist_id = $1", created.ID).Scan(&items)
		}))
	require.Zero(t, items, "the items went with the plan")

	// Nothing is in the way of the next one.
	require.Equal(t, checklistdomain.StatusActive,
		fixture.write(t, "Rencana baru", checklistdomain.NewItem{Title: "satu"}).Status)

	// And clearing a Bolu that has no plan is reported rather than silently accepted: a model that
	// believes it freed a plan that never existed would write the next one over nothing.
	require.NoError(t, fixture.service.Clear(ctx, fixture.scope, fixture.agent))
	require.ErrorIs(t, fixture.service.Clear(ctx, fixture.scope, fixture.agent), checklistdomain.ErrNotFound)
}

// TestReadingAPlanWithNoPlanIsNotFound: "you have not written one" is an answer, and it is the one the
// tool turns into an empty list rather than an error.
func TestReadingAPlanWithNoPlanIsNotFound(t *testing.T) {
	fixture := newChecklistFixture(t, appDatabase(t))

	_, err := fixture.service.Read(t.Context(), fixture.scope, fixture.agent)
	require.ErrorIs(t, err, checklistdomain.ErrNotFound)
}

// TestPlansAreTenantData: the policy is what keeps one workspace out of another's plan, and the read is
// the one that would quietly return another tenant's rows if it were wrong.
func TestPlansAreTenantData(t *testing.T) {
	pool := poolFor(t, appDatabase(t))
	mine := checklistFixtureOver(t, pool)
	theirs := checklistFixtureOver(t, pool)

	created := mine.write(t, "Rencana rahasia", checklistdomain.NewItem{Title: "cari harga"})

	// The other workspace cannot read it by id, and cannot see it in a list.
	_, err := theirs.repository.Active(t.Context(), theirs.scope, mine.agent)
	require.ErrorIs(t, err, checklistdomain.ErrNotFound)

	listed, err := theirs.repository.List(t.Context(), theirs.scope, checklistdomain.Filter{})
	require.NoError(t, err)
	require.Empty(t, listed)

	// And it cannot be cleared from outside either.
	require.ErrorIs(t, theirs.service.Clear(t.Context(), theirs.scope, mine.agent), checklistdomain.ErrNotFound)
	require.Equal(t, created.ID, mine.active(t).ID, "the plan is still there")
}

// TestTheChecklistToolsMatchTheDatabaseCatalogue reads the promise the adapter's comments make: the tools
// it declares are the rows the tool filter offers, with the same labels and with `needs_connection`
// false — because a plan belongs to the Bolu and nothing outside this deployment has to be connected for
// it to write one down. A disagreement would either offer a tool the hub has nothing to run or hide one
// that needs nothing.
func TestTheChecklistToolsMatchTheDatabaseCatalogue(t *testing.T) {
	fixture := newChecklistFixture(t, appDatabase(t))
	ctx := t.Context()

	adapter := checklistadapter.New(fixture.service)

	// Both directions are read: the rows that name this app, and any row whose name carries this app's
	// prefix — a row that says `checklist.` but belongs to another app would be a tool nothing can run.
	rows, err := fixture.pool.PgxPool().Query(ctx, `
		SELECT name, integration_app, label, needs_connection
		FROM tool_catalog
		WHERE integration_app = $1 OR name LIKE $2`, adapter.App(), checklistadapter.App+".%")
	require.NoError(t, err)
	defer rows.Close()

	labels := map[string]string{}
	connections := map[string]bool{}
	for rows.Next() {
		var name, app, label string
		var needsConnection bool
		require.NoError(t, rows.Scan(&name, &app, &label, &needsConnection))
		require.Equal(t, adapter.App(), app, "a tool named after this app belongs to this app")
		labels[name] = label
		connections[name] = needsConnection
	}
	require.NoError(t, rows.Err())

	require.Equal(t, map[string]string{
		"checklist.create": "write_internal",
		"checklist.check":  "write_internal",
		"checklist.remove": "write_internal",
		"checklist.clear":  "write_internal",
		"checklist.read":   "read",
	}, labels)

	for _, tool := range adapter.Tools() {
		label, declared := labels[tool.Name]
		require.True(t, declared, "the database must have a catalogue row for %s", tool.Name)
		require.Equal(t, tool.Label, label, "the label the adapter declares is the one the tool filter reads")
		require.False(t, connections[tool.Name], "%s belongs to the Bolu and needs no connected account", tool.Name)
	}

	// And the other way: a row with no declaration would be offered to a Bolu with nothing to run it.
	require.Len(t, labels, len(adapter.Tools()))
	require.False(t, adapter.NeedsConnection(), "the adapter and the rows agree that no connection is needed")
}

// TestEveryTemplateBoluIsBornWithTheNotebook: the five tools are on every template, so a Bolu a person
// makes from here on can write down what it is doing. (The migration's other half — the same five tools
// appended to every Bolu that already existed — is a fact about a database that had rows when it ran,
// and there is no way to observe it from a schema that was migrated empty.)
func TestEveryTemplateBoluIsBornWithTheNotebook(t *testing.T) {
	fixture := newChecklistFixture(t, appDatabase(t))

	templated := seedTemplateAgents(t, fixture.pool, fixture.workspace, "oren")[0]
	for _, tool := range []string{
		checklistadapter.ToolCreate, checklistadapter.ToolCheck, checklistadapter.ToolRemove,
		checklistadapter.ToolClear, checklistadapter.ToolRead,
	} {
		require.Contains(t, templated.Tools, tool, "the template asks for %s", tool)
	}

	// And the tools a templated Bolu is offered really are the ones the hub can run.
	tools, err := agentservice.New(agentservice.Deps{Repository: agentrepo.New(fixture.pool)}).
		AllowedTools(t.Context(), agentdomain.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace}, templated.ID)
	require.NoError(t, err)

	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	require.Contains(t, names, checklistadapter.ToolCreate)
	require.Contains(t, names, checklistadapter.ToolRead)
}

// activePlan re-reads the plan by id, which is how a settled one is read: it is no longer active.
func (f *checklistFixture) activePlan(t *testing.T) checklistdomain.Checklist {
	t.Helper()

	plans, err := f.repository.List(t.Context(), f.scope, checklistdomain.Filter{})
	require.NoError(t, err)
	require.NotEmpty(t, plans)

	return plans[0]
}

// titlesOf renders a plan's task titles.
func titlesOf(plan checklistdomain.Checklist) []string {
	titles := make([]string, 0, len(plan.Items))
	for _, item := range plan.Items {
		titles = append(titles, item.Title)
	}

	return titles
}

// subtaskTitlesOf renders one task's subtask titles.
func subtaskTitlesOf(item checklistdomain.Item) []string {
	titles := make([]string, 0, len(item.Subtasks))
	for _, subtask := range item.Subtasks {
		titles = append(titles, subtask.Title)
	}

	return titles
}
