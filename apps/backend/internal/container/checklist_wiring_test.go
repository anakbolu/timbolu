package container

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	checklistadapter "github.com/anakbolu/timbolu/apps/backend/internal/modules/checklist/adapter"
	checklistdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/checklist/domain"
	checklistrepo "github.com/anakbolu/timbolu/apps/backend/internal/modules/checklist/repository"
	integrationdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/integration/domain"
	integrationservice "github.com/anakbolu/timbolu/apps/backend/internal/modules/integration/service"
	taskdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/task/domain"
)

// The wiring tests for the checklist module cover the decisions only the container makes, and each of
// them spans two modules so no unit test can see it:
//
//   - **A plan reaches the tool executor with no connection at all.** The Bolu's own notebook is the
//     first application whose tools belong to nobody's account, so the executor has to run it without
//     looking for a grant — and the same fact has to be true of the catalogue row, or the tool would be
//     offered to a Bolu with nothing able to run it.
//   - **The adapter resolves the module when a call arrives.** The hub is assembled before the module
//     exists, so an adapter holding the service would hold a nil.
//   - **The schemas compile at startup.** A schema that will not compile is a bug in the adapter, and a
//     person waiting on a task is the worst time to find out.
//
// Everything runs on stubs, so nothing here needs Postgres, Redis, or Temporal.

// TestAChecklistToolRunsWithNoConnection is the connectionless path for a plan: the Bolu writes its own
// notebook, so no account is looked for and the call reaches the module with the Bolu and the task it
// was working on.
func TestAChecklistToolRunsWithNoConnection(t *testing.T) {
	plans := &stubChecklistService{created: checklistdomain.Checklist{ID: uuid.New(), Title: "Tagih pelanggan"}}
	hub := checklistHub(t, plans)
	container := &Container{Logger: zap.NewNop(), Services: &Services{Integration: hub, Checklist: plans}}
	executor := taskToolExecutor{container: container, hub: hub}

	agents := grantedTools(checklistadapter.ToolCreate, checklistadapter.App, taskdomain.LabelWriteInternal)
	executor.container.Services.Agent = agents

	arguments := json.RawMessage(`{"tasks":[{"title":"cari harga"}]}`)
	agentID, taskID := uuid.New(), uuid.New()

	result, err := executor.Execute(context.Background(), taskScope(), taskdomain.ToolCall{
		Name: checklistadapter.ToolCreate, Arguments: arguments, AgentID: agentID, TaskID: taskID,
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	require.Len(t, hub.executed, 1)
	require.Equal(t, uuid.Nil, hub.executed[0].integrationID, "the Bolu's own plan needs no account")
	require.Zero(t, hub.listCalls, "and no connection is ever looked for")
	require.Zero(t, hub.grantCalls)

	require.Len(t, plans.plans, 1)
	require.Equal(t, agentID, plans.plans[0].AgentID, "the plan belongs to the Bolu that wrote it")
	require.Equal(t, taskID, plans.plans[0].TaskID, "and is attributed to the task it was written during")
	require.Equal(t, "cari harga", plans.plans[0].Tasks[0].Title)
}

// TestAChecklistToolTheBoluWasNotOfferedIsRefused: a model can only call what it was given, and "the
// model asked for it" is not "the workspace allowed it" — the list a call is checked against is the
// registry's own, joined against the Bolu's tools.
func TestAChecklistToolTheBoluWasNotOfferedIsRefused(t *testing.T) {
	plans := &stubChecklistService{}
	hub := checklistHub(t, plans)
	container := &Container{Logger: zap.NewNop(), Services: &Services{
		Integration: hub,
		Checklist:   plans,
		Agent:       grantedTools(checklistadapter.ToolRead, checklistadapter.App, taskdomain.LabelRead),
	}}

	_, err := taskToolExecutor{container: container, hub: hub}.Execute(context.Background(), taskScope(), taskdomain.ToolCall{
		Name: checklistadapter.ToolClear, AgentID: uuid.New(),
	})

	require.ErrorIs(t, err, taskdomain.ErrToolNotAvailable)
	require.Empty(t, hub.executed, "a tool the Bolu was not offered never runs")
	require.Empty(t, plans.cleared)
}

// TestTheChecklistAdapterRefusesBeforeTheModuleIsAssembled is what the adapter answers when a call
// arrives before the module exists: a refusal naming the module rather than a panic. Every method has to
// give it, because any of them can be the first call a task makes.
func TestTheChecklistAdapterRefusesBeforeTheModuleIsAssembled(t *testing.T) {
	records := []struct {
		name    string
		adapter checklistPlans
	}{
		{"a container that was never built", checklistPlans{}},
		{"a container with no services", checklistPlans{container: &Container{Logger: zap.NewNop()}}},
		{"a container whose module is not wired", checklistPlans{container: &Container{Logger: zap.NewNop(), Services: &Services{}}}},
	}

	for _, test := range records {
		t.Run(test.name, func(t *testing.T) {
			scope := checklistdomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}
			agentID := uuid.New()

			_, err := test.adapter.Read(context.Background(), scope, agentID)
			require.ErrorIs(t, err, checklistdomain.ErrInvalidInput, "a read says so instead of panicking")
			require.ErrorContains(t, err, "modul rencana")

			_, err = test.adapter.Create(context.Background(), scope, checklistdomain.NewChecklist{
				AgentID: agentID,
				Tasks:   []checklistdomain.NewItem{{Title: "satu"}},
			})
			require.ErrorIs(t, err, checklistdomain.ErrInvalidInput)

			_, err = test.adapter.Check(context.Background(), scope, agentID, uuid.New())
			require.ErrorIs(t, err, checklistdomain.ErrInvalidInput)

			_, err = test.adapter.Remove(context.Background(), scope, agentID, uuid.New())
			require.ErrorIs(t, err, checklistdomain.ErrInvalidInput)

			require.ErrorIs(t, test.adapter.Clear(context.Background(), scope, agentID), checklistdomain.ErrInvalidInput)
		})
	}
}

// TestTheChecklistAdapterResolvesTheModuleWhenACallArrives is the claim the comment on the adapter
// makes: it is installed while the hub is assembled and the module it serves is assembled afterwards, so
// the service is looked up at call time rather than held. One adapter therefore refuses before assembly
// and answers after it, without anything being reinstalled.
func TestTheChecklistAdapterResolvesTheModuleWhenACallArrives(t *testing.T) {
	plans := &stubChecklistService{created: checklistdomain.Checklist{ID: uuid.New(), Title: "Rencana"}}
	container := &Container{Logger: zap.NewNop(), Services: &Services{}}
	adapter := checklistPlans{container: container}

	scope := checklistdomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}
	agentID := uuid.New()

	_, err := adapter.Read(context.Background(), scope, agentID)
	require.ErrorIs(t, err, checklistdomain.ErrInvalidInput, "installed before the module exists, it holds nothing")

	container.Services.Checklist = plans

	plan, err := adapter.Create(context.Background(), scope, checklistdomain.NewChecklist{
		AgentID: agentID,
		Tasks:   []checklistdomain.NewItem{{Title: "cari harga"}},
	})
	require.NoError(t, err)
	require.Equal(t, "Rencana", plan.Title)

	_, err = adapter.Check(context.Background(), scope, agentID, uuid.New())
	require.NoError(t, err)

	_, err = adapter.Remove(context.Background(), scope, agentID, uuid.New())
	require.NoError(t, err)

	require.NoError(t, adapter.Clear(context.Background(), scope, agentID))

	// The read that arrived before the module existed never reached the stub, so four calls did.
	require.Equal(t, []checklistdomain.Scope{scope, scope, scope, scope}, plans.scopes, "every call carried the caller's tenant")
	require.Equal(t, agentID, plans.plans[0].AgentID)
}

// TestTheChecklistSchemasCompile: the schemas are compiled once at startup and a schema that will not
// compile is a bug in the adapter. The create tool is the one that carries a shape, so it is the one
// that is validated both ways here.
func TestTheChecklistSchemasCompile(t *testing.T) {
	registry := integrationservice.NewRegistry()
	require.NoError(t, registry.Register(checklistadapter.New(&stubChecklistService{})))

	schemas, err := newToolSchemas(registry)
	require.NoError(t, err)

	create := registry.ToolsFor(checklistadapter.App)[0]
	require.Equal(t, checklistadapter.ToolCheck, create.Name, "tools are sorted by name")

	createSchema := schemaOf(t, registry, checklistadapter.ToolCreate)
	require.NoError(t, schemas.Validate(createSchema, json.RawMessage(`{"tasks":[{"title":"cari harga"}]}`)))

	// A plan with no tasks is the mistake the schema catches before the service is reached.
	err = schemas.Validate(createSchema, json.RawMessage(`{"title":"Tagih pelanggan"}`))
	require.ErrorIs(t, err, integrationdomain.ErrSchemaViolation)

	// And a tick with an id that is not a string never reaches the adapter either.
	checkSchema := schemaOf(t, registry, checklistadapter.ToolCheck)
	err = schemas.Validate(checkSchema, json.RawMessage(`{"id":7}`))
	require.ErrorIs(t, err, integrationdomain.ErrSchemaViolation)
}

// TestOpenChecklistWithoutAStoreLeavesTheModuleUnwired keeps the container's "running without Postgres"
// contract: the module stays nil, and the route then answers 503 rather than a screen showing an empty
// plan that looks like a Bolu with nothing to do.
func TestOpenChecklistWithoutAStoreLeavesTheModuleUnwired(t *testing.T) {
	c := &Container{
		Config:       testConfig(),
		Logger:       zap.NewNop(),
		Repositories: &Repositories{},
		Services:     &Services{},
	}

	require.NoError(t, c.openChecklist(context.Background(), testConfig()))
	require.Nil(t, c.Services.Checklist, "the service stays nil, so its route reports not configured")
}

// TestOpenChecklistInstallsTheServiceTheAdapterResolves: the module is stored where the adapter looks it
// up, so the build that answers a screen is the same one that answers a tool.
//
// The repository here is one built over no pool: assembly only stores it, and no query runs in this test,
// so the module can be built without a database.
func TestOpenChecklistInstallsTheServiceTheAdapterResolves(t *testing.T) {
	c := &Container{
		Config:       testConfig(),
		Logger:       zap.NewNop(),
		Repositories: &Repositories{Checklist: checklistrepo.New(nil)},
		Services:     &Services{},
	}

	require.NoError(t, c.openChecklist(context.Background(), testConfig()))
	require.NotNil(t, c.Services.Checklist, "the module was built, so it answers")

	// The repository is built over no pool, so the call reaches it and is refused there — which is what
	// says the adapter found the module rather than answering for one it never had.
	_, err := checklistPlans{container: c}.Read(context.Background(),
		checklistdomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}, uuid.New())
	require.ErrorContains(t, err, "database is not configured")
	require.NotContains(t, err.Error(), "modul rencana")
}

// ------------------------------------------------------------------------------------ stubs

// schemaOf is the schema one tool of a registry declared.
func schemaOf(t *testing.T, registry integrationdomain.Registry, name string) json.RawMessage {
	t.Helper()

	spec, _, ok := registry.Lookup(name)
	require.True(t, ok, "%s must be declared", name)

	return spec.Schema
}

// checklistHub builds a hub holding the real checklist adapter — reading a stub module — and nothing
// else.
func checklistHub(t *testing.T, plans checklistadapter.Service) *stubOrderHub {
	t.Helper()

	registry := integrationservice.NewRegistry()
	require.NoError(t, registry.Register(checklistadapter.New(plans)))

	return &stubOrderHub{registry: registry, grants: map[uuid.UUID]string{}}
}

// stubChecklistService stands in for the checklist module. It answers with what a test canned and keeps
// what it was asked, so a test can see that the arguments which reached it were adapted rather than
// invented.
type stubChecklistService struct {
	checklistdomain.Service

	created checklistdomain.Checklist
	// fail is what every method answers with, so a test can drive the failure path of a tool.
	fail error

	scopes  []checklistdomain.Scope
	plans   []checklistdomain.NewChecklist
	cleared []uuid.UUID
}

func (s *stubChecklistService) Read(_ context.Context, scope checklistdomain.Scope, _ uuid.UUID) (checklistdomain.Checklist, error) {
	s.scopes = append(s.scopes, scope)

	if s.fail != nil {
		return checklistdomain.Checklist{}, s.fail
	}

	return s.created, nil
}

func (s *stubChecklistService) Create(_ context.Context, scope checklistdomain.Scope, plan checklistdomain.NewChecklist) (checklistdomain.Checklist, error) {
	s.scopes = append(s.scopes, scope)
	s.plans = append(s.plans, plan)

	if s.fail != nil {
		return checklistdomain.Checklist{}, s.fail
	}

	return s.created, nil
}

func (s *stubChecklistService) Check(_ context.Context, scope checklistdomain.Scope, _, _ uuid.UUID) (checklistdomain.Checklist, error) {
	s.scopes = append(s.scopes, scope)

	if s.fail != nil {
		return checklistdomain.Checklist{}, s.fail
	}

	return s.created, nil
}

func (s *stubChecklistService) Remove(_ context.Context, scope checklistdomain.Scope, _, _ uuid.UUID) (checklistdomain.Checklist, error) {
	s.scopes = append(s.scopes, scope)

	if s.fail != nil {
		return checklistdomain.Checklist{}, s.fail
	}

	return s.created, nil
}

func (s *stubChecklistService) Clear(_ context.Context, scope checklistdomain.Scope, agentID uuid.UUID) error {
	s.scopes = append(s.scopes, scope)
	s.cleared = append(s.cleared, agentID)

	return s.fail
}
