package container

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	agentdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/agent/domain"
	integrationdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/integration/domain"
	integrationservice "github.com/anakbolu/timbolu/apps/backend/internal/modules/integration/service"
	orderadapter "github.com/anakbolu/timbolu/apps/backend/internal/modules/order/adapter"
	orderdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/order/domain"
	orderrepo "github.com/anakbolu/timbolu/apps/backend/internal/modules/order/repository"
	taskdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/task/domain"
)

// The wiring tests for the order module cover the decisions only the container makes, and each of them
// spans two modules so no unit test can see it:
//
//   - **The tool executor is where the product's rules about tools are enforced.** A `write_external`
//     tool is refused there, a tool the Bolu was not offered is refused there, and a connection is the
//     Bolu's grant rather than the caller's word. The workflow holds one lock on the approval door and
//     the tool filter holds one on the grant door; what these tests are about is that the third lock
//     holds when a call arrives without either having been asked.
//   - **A tool is offered only where something can run it.** A catalogue row with no adapter behind it
//     is a migration that outran its code, and offering it would teach the model to call something that
//     cannot work.
//   - **The adapter resolves the module when a call arrives.** The hub is assembled before the module
//     exists, so an adapter holding the service would hold a nil.
//
// Everything runs on stubs. The hub and the Bolu registry are the two ports the executor reads, and the
// order module is a stub too, so nothing here needs Postgres, Redis, or Temporal.

// TestTheToolExecutorIsAbsentWithoutAHub is what the runtime reads as "this tool is not available": a
// deployment with no integration service gets no executor, so a task says so instead of failing at the
// first call — and a container that was never assembled answers the same way rather than dereferencing
// a nil aggregate. The other half is that the executor a wired container hands over really runs a call,
// which is what the connectionless test below drives end to end.
func TestTheToolExecutorIsAbsentWithoutAHub(t *testing.T) {
	var never *Container
	require.Nil(t, never.toolExecutor(), "a container that was never built has no executor")
	require.Nil(t, (&Container{Logger: zap.NewNop(), Services: &Services{}}).toolExecutor())

	// EPIC 14 (#112) supplies the executor, so a deployment with a hub must hand the runtime one.
	present := wiredContainer(nil, orderHub(t, &stubOrderService{}))
	require.NotNil(t, present.toolExecutor())
}

// TestAToolTheHubDoesNotKnowIsRefused: the hub's registry is the list of what this deployment can run,
// so a name outside it is a model calling something that does not exist here. It is refused before the
// tool is looked for in the Bolu's grants, because there is nothing to look for.
func TestAToolTheHubDoesNotKnowIsRefused(t *testing.T) {
	hub := orderHub(t, &stubOrderService{})
	executor := taskToolExecutor{container: wiredContainer(nil, hub), hub: hub}

	_, err := executor.Execute(context.Background(), taskScope(), taskdomain.ToolCall{Name: "gmail.send"})

	require.ErrorIs(t, err, taskdomain.ErrToolNotAvailable)
	require.Empty(t, hub.executed, "nothing ran")
}

// TestAToolTheBoluWasNotGrantedIsRefused: a model can only call what it was given, but a call is what
// arrives here, and "the model asked for it" is not "the workspace allowed it". The list a call is
// checked against is the registry's own — the workspace catalogue joined against the Bolu's grants —
// and it is asked about the Bolu the call names.
func TestAToolTheBoluWasNotGrantedIsRefused(t *testing.T) {
	agents := grantedTools(orderadapter.ToolReadOrders, orderadapter.App, taskdomain.LabelRead)
	hub := orderHub(t, &stubOrderService{})
	executor := taskToolExecutor{container: wiredContainer(agents, hub), hub: hub}

	scope := taskScope()
	agentID := uuid.New()

	_, err := executor.Execute(context.Background(), scope, taskdomain.ToolCall{
		Name: orderadapter.ToolRecordOrder, AgentID: agentID,
	})

	require.ErrorIs(t, err, taskdomain.ErrToolNotAvailable)
	require.Empty(t, hub.executed, "a tool the Bolu was not offered never runs")

	require.Equal(t, agentID, agents.askedAgentID, "the filter is asked about the Bolu the call names")
	require.Equal(t, scope.WorkspaceID, agents.askedScope.WorkspaceID, "and inside the workspace it belongs to")
}

// TestAWriteExternalToolIsRefusedEvenWhenTheBoluHoldsTheGrant is the second lock on the approval gate's
// door. The workflow routes a `write_external` tool to the gate before an executor sees it, so one
// arriving here means something upstream went wrong — and what must not happen next is a message
// leaving the building with nobody's approval. The grant is held in this test on purpose: the lock is
// about the label, not about who was given what.
func TestAWriteExternalToolIsRefusedEvenWhenTheBoluHoldsTheGrant(t *testing.T) {
	agents := grantedTools("gmail.send", "gmail", taskdomain.LabelWriteExternal)
	hub := orderHub(t, &stubOrderService{}, &stubConnectedAdapter{})
	executor := taskToolExecutor{container: wiredContainer(agents, hub), hub: hub}

	_, err := executor.Execute(context.Background(), taskScope(), taskdomain.ToolCall{
		Name: "gmail.send", AgentID: uuid.New(),
	})

	require.ErrorIs(t, err, taskdomain.ErrToolNotAvailable)
	require.ErrorContains(t, err, "persetujuan", "the model and the log are told what the refusal is about")
	require.Empty(t, hub.executed, "no call left the executor")
	require.Zero(t, hub.grantCalls, "the refusal comes before any account is looked for")
	require.Zero(t, hub.listCalls)
}

// TestAConnectionlessToolRunsWithNoConnection is the workspace's own data: there is no account to
// authorise and nothing outside the deployment to reach, so the call runs with no integration id and
// the hub is never asked for a connection or a grant. What the adapter receives is what the model sent.
func TestAConnectionlessToolRunsWithNoConnection(t *testing.T) {
	orders := &stubOrderService{order: orderdomain.Order{
		ID: uuid.New(), Code: "ORD-7", Status: orderdomain.StatusRecorded,
		Customer: orderdomain.Customer{Name: "Ani", Phone: "0812"},
	}}
	hub := orderHub(t, orders)
	// The executor comes from the container rather than being built here, which is the end of the wire
	// the runtime holds: the container's own executor, its hub, the adapter the hub knows, and the
	// module behind it.
	executor := wiredContainer(
		grantedTools(orderadapter.ToolRecordOrder, orderadapter.App, taskdomain.LabelWriteInternal), hub,
	).toolExecutor()

	arguments := json.RawMessage(
		`{"customer_phone":"0812","customer_name":"Ani","items":[{"product":"Kopi","quantity":2}]}`)

	result, err := executor.Execute(context.Background(), taskScope(), taskdomain.ToolCall{
		Name: orderadapter.ToolRecordOrder, Arguments: arguments, AgentID: uuid.New(), TaskID: uuid.New(),
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	require.Len(t, hub.executed, 1)
	require.Equal(t, uuid.Nil, hub.executed[0].integrationID, "the workspace's own data needs no account")
	require.Equal(t, string(arguments), string(hub.executed[0].arguments), "the arguments travel verbatim")
	require.Zero(t, hub.listCalls, "no connection was looked for")
	require.Zero(t, hub.grantCalls, "no grant was read: there is no account to grant")

	// The adapter did receive them, which is the part the hub's record alone cannot show: the order
	// carries the line the arguments named.
	require.Len(t, orders.recorded, 1)
	require.Equal(t, "0812", orders.recorded[0].CustomerPhone)
	require.Len(t, orders.recorded[0].Items, 1)
	require.Equal(t, "Kopi", orders.recorded[0].Items[0].Product)
	require.Equal(t, 2.0, orders.recorded[0].Items[0].Quantity)
	require.Contains(t, string(result.Content), "ORD-7", "the model reads what was recorded")
}

// TestAConnectedToolRunsOnTheGrantedConnection is the third decision: the integration id is looked up
// from the Bolu's grant, so a task cannot name a mailbox its Bolu was never handed. Two mailboxes are
// connected in this workspace and only one was granted — the granted one is the one the call runs
// against, whichever order the hub happens to list them in.
func TestAConnectedToolRunsOnTheGrantedConnection(t *testing.T) {
	granted, stranger := uuid.New(), uuid.New()
	adapter := &stubConnectedAdapter{}
	hub := orderHub(t, &stubOrderService{}, adapter)
	hub.connections = []integrationdomain.Integration{
		{ID: stranger, App: "gmail", Status: integrationdomain.StatusConnected},
		{ID: granted, App: "gmail", Status: integrationdomain.StatusConnected},
	}
	hub.grants = map[uuid.UUID]string{granted: "read"}

	executor := taskToolExecutor{
		container: wiredContainer(grantedTools("gmail.search", "gmail", taskdomain.LabelRead), hub),
		hub:       hub,
	}

	result, err := executor.Execute(context.Background(), taskScope(), taskdomain.ToolCall{
		Name: "gmail.search", Arguments: json.RawMessage(`{}`), AgentID: uuid.New(),
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	require.Len(t, adapter.calls, 1)
	require.Equal(t, granted, adapter.calls[0].IntegrationID, "the grant decides which mailbox, not the first one found")
	require.Equal(t, "gmail.search", adapter.calls[0].Tool)
	require.Equal(t, granted, hub.executed[0].integrationID)
}

// TestAConnectionThatIsNotTheOneGrantedIsRefused is the other half of the same rule, in the three
// shapes an account can be unusable: its authorisation has lapsed, it belongs to another application,
// or the Bolu was never given it. None of them may be used, and each refusal names the application a
// person has to connect rather than looking like a bug in the task.
func TestAConnectionThatIsNotTheOneGrantedIsRefused(t *testing.T) {
	connected := uuid.New()

	tests := []struct {
		name        string
		connections []integrationdomain.Integration
		grants      map[uuid.UUID]string
	}{
		{
			name:        "the authorisation lapsed",
			connections: []integrationdomain.Integration{{ID: connected, App: "gmail", Status: integrationdomain.StatusNeedsRelogin}},
			grants:      map[uuid.UUID]string{connected: "read"},
		},
		{
			name:        "the connection belongs to another application",
			connections: []integrationdomain.Integration{{ID: connected, App: "jira", Status: integrationdomain.StatusConnected}},
			grants:      map[uuid.UUID]string{connected: "read"},
		},
		{
			name:        "the Bolu was never granted it",
			connections: []integrationdomain.Integration{{ID: connected, App: "gmail", Status: integrationdomain.StatusConnected}},
			grants:      map[uuid.UUID]string{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter := &stubConnectedAdapter{}
			hub := orderHub(t, &stubOrderService{}, adapter)
			hub.connections, hub.grants = test.connections, test.grants

			executor := taskToolExecutor{
				container: wiredContainer(grantedTools("gmail.search", "gmail", taskdomain.LabelRead), hub),
				hub:       hub,
			}

			_, err := executor.Execute(context.Background(), taskScope(), taskdomain.ToolCall{
				Name: "gmail.search", Arguments: json.RawMessage(`{}`), AgentID: uuid.New(),
			})

			require.ErrorIs(t, err, integrationdomain.ErrNotFound)
			require.ErrorContains(t, err, "gmail", "the refusal names what a person has to connect")
			require.Empty(t, adapter.calls, "the call does not run against a substitute account")
			require.Empty(t, hub.executed)
		})
	}
}

// TestTheHubIsToldWhichBoluAndTaskTheCallIsFor: the scope handed to a tool carries the Bolu and the
// task because an adapter that records something attributes it. An order a Bolu took belongs to that
// Bolu and to the task it was working on, and reading them off the scope is what lets the tool write
// them without the hub having to know what they are for.
func TestTheHubIsToldWhichBoluAndTaskTheCallIsFor(t *testing.T) {
	orders := &stubOrderService{order: orderdomain.Order{ID: uuid.New(), Code: "ORD-8"}}
	hub := orderHub(t, orders)
	executor := taskToolExecutor{
		container: wiredContainer(
			grantedTools(orderadapter.ToolRecordOrder, orderadapter.App, taskdomain.LabelWriteInternal), hub),
		hub: hub,
	}

	scope := taskScope()
	agentID, taskID := uuid.New(), uuid.New()

	_, err := executor.Execute(context.Background(), scope, taskdomain.ToolCall{
		Name:      orderadapter.ToolRecordOrder,
		Arguments: json.RawMessage(`{"items":[{"product":"Kopi","quantity":1}]}`),
		AgentID:   agentID,
		TaskID:    taskID,
	})
	require.NoError(t, err)

	require.Len(t, hub.executed, 1)
	require.Equal(t, scope.UserID, hub.executed[0].scope.UserID)
	require.Equal(t, scope.WorkspaceID, hub.executed[0].scope.WorkspaceID, "the tenancy still travels")
	require.Equal(t, agentID, hub.executed[0].scope.AgentID)
	require.Equal(t, taskID, hub.executed[0].scope.TaskID)

	// The recording tool writes them down, which is the whole point of carrying them.
	require.Len(t, orders.recorded, 1)
	require.Equal(t, agentID, orders.recorded[0].AgentID)
	require.Equal(t, taskID, orders.recorded[0].TaskID)
}

// TestAFailedToolIsAnsweredRatherThanFailingTheTask: the runtime hands a failed tool result back to the
// model, which can try another tool or answer with what it knows. Ending the task would turn "the
// customer's product is not in the catalogue" into a failed task, which is the opposite of what the
// model is there for — so the reason travels as a JSON document the model can read.
func TestAFailedToolIsAnsweredRatherThanFailingTheTask(t *testing.T) {
	orders := &stubOrderService{fail: &orderdomain.UnknownProductError{
		Name:       "Kopi Susu",
		Candidates: []orderdomain.Product{{Name: "Kopi Arabika", UnitPriceMicros: 15_000_000}},
	}}
	hub := orderHub(t, orders)
	executor := taskToolExecutor{
		container: wiredContainer(
			grantedTools(orderadapter.ToolRecordOrder, orderadapter.App, taskdomain.LabelWriteInternal), hub),
		hub: hub,
	}

	result, err := executor.Execute(context.Background(), taskScope(), taskdomain.ToolCall{
		Name:      orderadapter.ToolRecordOrder,
		Arguments: json.RawMessage(`{"items":[{"product":"Kopi Susu","quantity":1}]}`),
		AgentID:   uuid.New(),
	})

	require.NoError(t, err, "a tool that failed is answered, not returned as an error that ends the task")
	require.True(t, result.IsError)

	var body struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(result.Content, &body))
	require.Contains(t, body.Error, "Kopi Susu", "the model is told what it asked for")
	require.Contains(t, body.Error, "Kopi Arabika", "and what the catalogue has instead, so it can ask")
}

// TestAToolNoAdapterCanRunIsNotOffered is the migration case: the catalogue knows a row and this
// deployment has nothing that runs it, so the row is dropped rather than offered — offering it teaches
// the model to call something that cannot work. The row that is runnable is offered with the schema its
// own adapter declares, which is what makes the arguments a model sends the arguments the tool expects.
func TestAToolNoAdapterCanRunIsNotOffered(t *testing.T) {
	agents := &stubAgentTools{tools: []agentdomain.Tool{
		{
			Name: orderadapter.ToolFindProduct, Integration: orderadapter.App,
			Label: taskdomain.LabelRead, Description: "Cari harga satu produk di katalog workspace.",
		},
		{Name: "jira.create_issue", Integration: "jira", Label: taskdomain.LabelRead, Description: "Buat tiket."},
	}}
	hub := orderHub(t, &stubOrderService{})
	registry := toolRegistry{service: agents, container: wiredContainer(nil, hub)}

	tools, err := registry.Tools(context.Background(), taskScope(), uuid.New())
	require.NoError(t, err)

	require.Len(t, tools, 1, "the row nothing can run is not offered")
	require.Equal(t, orderadapter.ToolFindProduct, tools[0].Name)

	spec, _, ok := hub.registry.Lookup(orderadapter.ToolFindProduct)
	require.True(t, ok)
	require.JSONEq(t, string(spec.Schema), string(tools[0].Schema), "the schema comes from the adapter, not the row")

	// The row still carries what a screen shows and the filter decides on.
	require.Equal(t, orderadapter.App, tools[0].Integration)
	require.Equal(t, taskdomain.LabelRead, tools[0].Label)
	require.Equal(t, "Cari harga satu produk di katalog workspace.", tools[0].Description)
}

// TestWithoutAHubTheCatalogueRowIsOfferedWithTheOpenSchema: the API and the worker assemble separately,
// and a worker without a hub is a deployment that cannot run tools — which its own executor says when a
// call arrives. Hiding the rows instead would make a Bolu's profile look empty for a reason nobody can
// see, so they are offered with a schema that accepts anything; the hub validates against the real one
// when a call is made, so a wrong argument is refused either way.
func TestWithoutAHubTheCatalogueRowIsOfferedWithTheOpenSchema(t *testing.T) {
	agents := grantedTools(orderadapter.ToolFindProduct, orderadapter.App, taskdomain.LabelRead)

	registries := []toolRegistry{
		{service: agents},
		{service: agents, container: &Container{Logger: zap.NewNop(), Services: &Services{}}},
	}

	for _, registry := range registries {
		tools, err := registry.Tools(context.Background(), taskScope(), uuid.New())
		require.NoError(t, err)

		require.Len(t, tools, 1, "a deployment that cannot run tools still shows what the Bolu was given")
		require.JSONEq(t, string(openSchema), string(tools[0].Schema))
	}
}

// TestTheOrderAdapterRefusesBeforeTheModuleIsAssembled is what the adapter answers when a call arrives
// before the module exists: a refusal naming the module rather than a panic. Every method has to give
// it, because any of the three can be the first call a task makes.
func TestTheOrderAdapterRefusesBeforeTheModuleIsAssembled(t *testing.T) {
	records := []struct {
		name    string
		adapter orderRecords
	}{
		{"a container that was never built", orderRecords{}},
		{"a container with no services", orderRecords{container: &Container{Logger: zap.NewNop()}}},
		{"a container whose module is not wired", orderRecords{container: &Container{Logger: zap.NewNop(), Services: &Services{}}}},
	}

	for _, test := range records {
		t.Run(test.name, func(t *testing.T) {
			scope := orderdomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}

			_, err := test.adapter.Lookup(context.Background(), scope, "Kopi")
			require.ErrorIs(t, err, orderdomain.ErrInvalidInput, "a lookup says so instead of panicking")
			require.ErrorContains(t, err, "the order module is not configured")

			_, err = test.adapter.Record(context.Background(), scope, orderdomain.NewOrder{
				Items: []orderdomain.NewItem{{Product: "Kopi", Quantity: 1}},
			})
			require.ErrorIs(t, err, orderdomain.ErrInvalidInput)
			require.ErrorContains(t, err, "the order module is not configured")

			_, err = test.adapter.Orders(context.Background(), scope, orderdomain.OrderFilter{})
			require.ErrorIs(t, err, orderdomain.ErrInvalidInput)
			require.ErrorContains(t, err, "the order module is not configured")
		})
	}
}

// TestTheOrderAdapterResolvesTheModuleWhenACallArrives is the claim the comment on the adapter makes:
// it is installed while the hub is assembled and the module it serves is assembled afterwards, so the
// service is looked up at call time rather than held. One adapter therefore refuses before assembly and
// answers after it, without anything being reinstalled.
func TestTheOrderAdapterResolvesTheModuleWhenACallArrives(t *testing.T) {
	orders := &stubOrderService{
		lookup: orderdomain.Lookup{Found: &orderdomain.Product{Name: "Kopi", UnitPriceMicros: 15_000_000}},
		order:  orderdomain.Order{ID: uuid.New(), Code: "ORD-9"},
		orders: []orderdomain.Order{{Code: "ORD-9"}},
	}
	container := &Container{Logger: zap.NewNop(), Services: &Services{}}
	adapter := orderRecords{container: container}

	scope := orderdomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}

	_, err := adapter.Orders(context.Background(), scope, orderdomain.OrderFilter{})
	require.ErrorIs(t, err, orderdomain.ErrInvalidInput, "installed before the module exists, it holds nothing")

	container.Order = orders
	container.Services.Order = orders

	lookup, err := adapter.Lookup(context.Background(), scope, "Kopi")
	require.NoError(t, err)
	require.Equal(t, "Kopi", lookup.Found.Name)

	recorded, err := adapter.Record(context.Background(), scope, orderdomain.NewOrder{
		Items: []orderdomain.NewItem{{Product: "Kopi", Quantity: 1}},
	})
	require.NoError(t, err)
	require.Equal(t, "ORD-9", recorded.Code)

	listed, err := adapter.Orders(context.Background(), scope, orderdomain.OrderFilter{CustomerPhone: "0812", Limit: 5})
	require.NoError(t, err)
	require.Len(t, listed, 1)

	require.Equal(t, []string{"Kopi"}, orders.lookedUp, "the term reached the module")
	require.Len(t, orders.recorded, 1)
	require.Equal(t, "0812", orders.askedFilter.CustomerPhone, "the filter reached the module")
	require.Equal(t, scope.WorkspaceID, orders.scopes[0].WorkspaceID, "and so did the caller's scope")
}

// TestOpenOrderWithoutAStoreLeavesTheModuleUnwired keeps the container's "running without Postgres"
// contract: the module stays nil, and the routes then answer 503 rather than the screen showing an
// empty catalogue that looks like a workspace with nothing in it.
func TestOpenOrderWithoutAStoreLeavesTheModuleUnwired(t *testing.T) {
	c := &Container{
		Config:       testConfig(),
		Logger:       zap.NewNop(),
		Repositories: &Repositories{},
		Services:     &Services{},
	}

	require.NoError(t, c.openOrder(context.Background(), testConfig()))
	require.Nil(t, c.Services.Order, "the service stays nil, so its routes report not configured")
	require.Nil(t, c.Order)
}

// TestOpenOrderInstallsTheServiceWhereBothReadersLook is why the module is kept twice: a route reads it
// from `Services`, and the integration adapter resolves it from there when a tool is called. One build
// is installed in both places so the two cannot come to mean different things.
//
// The repository here is one built over no pool: assembly only stores it, and no query runs in this
// test, so the module can be built without a database.
func TestOpenOrderInstallsTheServiceWhereBothReadersLook(t *testing.T) {
	c := &Container{
		Config:       testConfig(),
		Logger:       zap.NewNop(),
		Repositories: &Repositories{Order: orderrepo.New(nil)},
		Services:     &Services{},
	}

	require.NoError(t, c.openOrder(context.Background(), testConfig()))
	require.NotNil(t, c.Services.Order, "the module was built, so it answers")
	require.Same(t, c.Services.Order, c.Order, "both readers hold the one service")
}

// ------------------------------------------------------------------------------------ stubs

// taskScope is the tenancy a test's call runs as.
func taskScope() taskdomain.Scope {
	return taskdomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}
}

// wiredContainer is the container an executor reads its two ports from: the Bolu registry and the hub.
func wiredContainer(agents agentdomain.Service, hub integrationdomain.Service) *Container {
	return &Container{Logger: zap.NewNop(), Services: &Services{Agent: agents, Integration: hub}}
}

// stubOrderHub is the integration hub a container test holds: it answers with the registry this test
// assembled, records every call the executor asks it to run, and hands each one to the adapter that
// claims the tool — which is how a test reads what a tool received without reaching a provider.
//
// It is a stub rather than the real hub because the real one opens tokens and validates schemas: what
// is under test here is the executor's decision about which call to make, not the hub's own path.
type stubOrderHub struct {
	integrationdomain.Service

	registry    integrationdomain.Registry
	connections []integrationdomain.Integration
	grants      map[uuid.UUID]string

	listCalls  int
	grantCalls int

	executed []hubCall
}

// hubCall is one call the executor asked the hub to run, kept as it was handed over so a test can read
// the whole of it: the scope, the account, the tool, and the arguments.
type hubCall struct {
	scope         integrationdomain.Scope
	integrationID uuid.UUID
	tool          string
	arguments     json.RawMessage
}

func (h *stubOrderHub) Registry() integrationdomain.Registry { return h.registry }

func (h *stubOrderHub) List(context.Context, integrationdomain.Scope) ([]integrationdomain.Integration, error) {
	h.listCalls++
	return h.connections, nil
}

func (h *stubOrderHub) AllowedPermissions(context.Context, integrationdomain.Scope, uuid.UUID) (map[uuid.UUID]string, error) {
	h.grantCalls++
	return h.grants, nil
}

func (h *stubOrderHub) Execute(ctx context.Context, scope integrationdomain.Scope, integrationID uuid.UUID, tool string, arguments json.RawMessage) (integrationdomain.Result, error) {
	h.executed = append(h.executed, hubCall{
		scope: scope, integrationID: integrationID, tool: tool, arguments: arguments,
	})

	_, adapter, ok := h.registry.Lookup(tool)
	if !ok {
		return integrationdomain.Result{}, fmt.Errorf("%w: %s", integrationdomain.ErrUnknownTool, tool)
	}

	return adapter.Execute(ctx, integrationdomain.Call{
		Scope: scope, IntegrationID: integrationID, Tool: tool, Arguments: arguments,
	})
}

// orderHub builds a hub holding the real order adapter — reading a stub module — and whatever other
// applications a test speaks.
func orderHub(t *testing.T, orders orderadapter.Service, others ...integrationdomain.Adapter) *stubOrderHub {
	t.Helper()

	registry := integrationservice.NewRegistry()
	require.NoError(t, registry.Register(orderadapter.New(orders)))
	for _, other := range others {
		require.NoError(t, registry.Register(other))
	}

	return &stubOrderHub{registry: registry, grants: map[uuid.UUID]string{}}
}

// stubAgentTools is the Bolu registry: it reports the tools a test says a Bolu was given, and keeps the
// question so a test can see the filter is asked about the right Bolu in the right workspace.
type stubAgentTools struct {
	agentdomain.Service

	tools        []agentdomain.Tool
	askedAgentID uuid.UUID
	askedScope   agentdomain.Scope
}

func (s *stubAgentTools) AllowedTools(_ context.Context, scope agentdomain.Scope, agentID uuid.UUID) ([]agentdomain.Tool, error) {
	s.askedScope, s.askedAgentID = scope, agentID
	return s.tools, nil
}

// grantedTools is a Bolu registry reporting exactly one tool, which is what every executor path starts
// from: the question is never "which of many" here but "was this one given".
func grantedTools(name, integration, label string) *stubAgentTools {
	return &stubAgentTools{tools: []agentdomain.Tool{{Name: name, Integration: integration, Label: label}}}
}

// stubOrderService stands in for the order module. It answers with what a test canned and keeps what it
// was asked, so a test can see that the arguments which reached it were adapted rather than invented.
type stubOrderService struct {
	orderdomain.Service

	lookup orderdomain.Lookup
	order  orderdomain.Order
	orders []orderdomain.Order
	// fail is what every method answers with, so a test can drive the failure path of a tool.
	fail error

	scopes      []orderdomain.Scope
	lookedUp    []string
	recorded    []orderdomain.NewOrder
	askedFilter orderdomain.OrderFilter
}

func (s *stubOrderService) Lookup(_ context.Context, scope orderdomain.Scope, term string) (orderdomain.Lookup, error) {
	s.scopes = append(s.scopes, scope)
	s.lookedUp = append(s.lookedUp, term)

	if s.fail != nil {
		return orderdomain.Lookup{}, s.fail
	}
	return s.lookup, nil
}

func (s *stubOrderService) Record(_ context.Context, scope orderdomain.Scope, order orderdomain.NewOrder) (orderdomain.Order, error) {
	s.scopes = append(s.scopes, scope)
	s.recorded = append(s.recorded, order)

	if s.fail != nil {
		return orderdomain.Order{}, s.fail
	}
	return s.order, nil
}

func (s *stubOrderService) Orders(_ context.Context, scope orderdomain.Scope, filter orderdomain.OrderFilter) ([]orderdomain.Order, error) {
	s.scopes = append(s.scopes, scope)
	s.askedFilter = filter

	if s.fail != nil {
		return nil, s.fail
	}
	return s.orders, nil
}

// stubConnectedAdapter is an application reached through a connected account. It deliberately does not
// declare `Connectionless`, which is the whole of what makes the executor look for a grant, and it
// keeps the calls it ran so a test can see which account was chosen.
type stubConnectedAdapter struct {
	calls []integrationdomain.Call
}

func (a *stubConnectedAdapter) App() string { return "gmail" }

func (a *stubConnectedAdapter) Tools() []integrationdomain.ToolSpec {
	return []integrationdomain.ToolSpec{
		{
			Name: "gmail.search", Label: integrationdomain.LabelRead, Description: "Cari email.",
			Schema: json.RawMessage(`{"type":"object"}`),
		},
		{
			Name: "gmail.send", Label: integrationdomain.LabelWriteExternal, Description: "Kirim email.",
			Schema: json.RawMessage(`{"type":"object"}`),
		},
	}
}

func (a *stubConnectedAdapter) Triggers() []integrationdomain.TriggerSpec { return nil }

func (a *stubConnectedAdapter) Execute(_ context.Context, call integrationdomain.Call) (integrationdomain.Result, error) {
	a.calls = append(a.calls, call)
	return integrationdomain.Result{Content: json.RawMessage(`{"ok":true}`)}, nil
}
