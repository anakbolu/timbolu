package container

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	integrationdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/integration/domain"
	orderadapter "github.com/anakbolu/timbolu/apps/backend/internal/modules/order/adapter"
	orderdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/order/domain"
	orderservice "github.com/anakbolu/timbolu/apps/backend/internal/modules/order/service"
	taskdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/task/domain"
	"github.com/anakbolu/timbolu/packages/platform/config"
)

// openOrder builds the module that holds the workspace's own commerce data: the catalogue a person
// maintains, the customers it talks to, and the orders a Bolu recorded.
//
// It is the first module whose tables are the workspace's business rather than a cache of something
// that lives at a provider, and that is visible in where it is built. Its *adapter* is registered with
// the integration hub much earlier, in `integrationAdapters`, because that is the one place an
// application is named — and the adapter resolves this service when a tool is called rather than
// holding it, which is what lets the two orders hold.
func (c *Container) openOrder(_ context.Context, _ *config.Config) error {
	if c.Repositories == nil || c.Repositories.Order == nil {
		c.Logger.Info("the order module is disabled: no database connection")
		return nil
	}

	service := orderservice.New(orderservice.Deps{
		Repository: c.Repositories.Order,
		Logger:     c.Logger,
	})
	c.Order = service
	c.Services.Order = service

	return nil
}

// adOrder is the integration adapter for the workspace's own data, registered with the hub beside the
// connected applications.
//
// It is registered when a database exists, because that is what decides whether the module can answer
// at all: a deployment without storage offers no catalogue tools rather than offering a tool that
// fails when a model calls it.
func adOrder(c *Container) integrationdomain.Adapter {
	return orderadapter.New(orderRecords{container: c})
}

// orderRecords resolves the order module when a tool is called.
//
// The adapter is installed while the hub is assembled, and the module it serves is assembled after it —
// the same shape as the WhatsApp adapter, and for the same reason. Holding the service here would hold
// a nil.
type orderRecords struct {
	container *Container
}

// live is the service this process has, or nil while it is still starting.
func (o orderRecords) live() orderdomain.Service {
	if o.container == nil || o.container.Services == nil {
		return nil
	}

	return o.container.Services.Order
}

// Lookup implements the adapter's port.
func (o orderRecords) Lookup(ctx context.Context, scope orderdomain.Scope, term string) (orderdomain.Lookup, error) {
	service := o.live()
	if service == nil {
		return orderdomain.Lookup{}, fmt.Errorf("%w: the order module is not configured", orderdomain.ErrInvalidInput)
	}

	return service.Lookup(ctx, scope, term)
}

// Record implements the adapter's port.
func (o orderRecords) Record(ctx context.Context, scope orderdomain.Scope, order orderdomain.NewOrder) (orderdomain.Order, error) {
	service := o.live()
	if service == nil {
		return orderdomain.Order{}, fmt.Errorf("%w: the order module is not configured", orderdomain.ErrInvalidInput)
	}

	return service.Record(ctx, scope, order)
}

// Orders implements the adapter's port.
func (o orderRecords) Orders(ctx context.Context, scope orderdomain.Scope, filter orderdomain.OrderFilter) ([]orderdomain.Order, error) {
	service := o.live()
	if service == nil {
		return nil, fmt.Errorf("%w: the order module is not configured", orderdomain.ErrInvalidInput)
	}

	return service.Orders(ctx, scope, filter)
}

// ---------------------------------------------------------------- tool execution

// taskToolExecutor runs the tool calls a task makes.
//
// It is the piece the runtime has been waiting for: until it existed every tool a Bolu was offered was
// answered with "not available", and the model learned that its tools do not work. Three decisions are
// made here and each is the product's own rule rather than defensive coding:
//
//   - **A `write_external` tool is refused.** The workflow routes those to the approval gate before an
//     executor sees them, so one arriving here means something went wrong upstream — and running it
//     would be a message leaving the building with nobody's approval.
//   - **A tool the Bolu was not offered is refused**, by asking the registry for the effective list.
//     A model can only call what it was given, but a call is what reaches here, and "the model asked
//     for it" is not the same as "the workspace allowed it".
//   - **A connection is the Bolu's grant, not the caller's word.** The integration id is looked up
//     from the grant, so a task cannot name a mailbox its Bolu was never handed.
type taskToolExecutor struct {
	container *Container
	hub       integrationdomain.Service
}

// Execute implements taskdomain.ToolExecutor.
func (e taskToolExecutor) Execute(ctx context.Context, scope taskdomain.Scope, call taskdomain.ToolCall) (taskdomain.ToolResult, error) {
	if e.hub == nil {
		return taskdomain.ToolResult{}, fmt.Errorf("%w: %s", taskdomain.ErrToolNotAvailable, call.Name)
	}

	_, adapter, ok := e.hub.Registry().Lookup(call.Name)
	if !ok {
		return taskdomain.ToolResult{}, fmt.Errorf("%w: %s", taskdomain.ErrToolNotAvailable, call.Name)
	}

	// The registry's list is the effective one: the Bolu's profile, the workspace's catalogue, and the
	// grants, filtered together. It is asked rather than the tool names being trusted from the call, and
	// the app it reports is what the connection is looked up by — so the catalogue row, not the tool
	// name, decides which account a call may reach.
	granted, err := e.container.toolRegistry().Tools(ctx, scope, call.AgentID)
	if err != nil {
		return taskdomain.ToolResult{}, err
	}

	var permission taskdomain.Tool
	found := false
	for _, tool := range granted {
		if tool.Name == call.Name {
			permission, found = tool, true

			break
		}
	}
	if !found {
		return taskdomain.ToolResult{}, fmt.Errorf("%w: %s", taskdomain.ErrToolNotAvailable, call.Name)
	}

	// The gate's rule, enforced where the call actually happens. The workflow sends a `write_external`
	// tool to the approval gate before an executor sees it, so this is the second lock on the same
	// door — and the one that holds if a future path forgets the first.
	if permission.Label == taskdomain.LabelWriteExternal {
		return taskdomain.ToolResult{}, fmt.Errorf(
			"%w: %s perlu persetujuan manusia; jalankan lewat gerbang persetujuan", taskdomain.ErrToolNotAvailable, call.Name)
	}

	integrationID, err := e.connectionFor(ctx, permission.Integration, adapter, scope, call)
	if err != nil {
		return taskdomain.ToolResult{}, err
	}

	// The Bolu and the task travel on the scope so an adapter that records something can attribute it:
	// an order a Bolu took belongs to that Bolu and to the task it was working on. Reading them from
	// the call is right here in a way reading a connection id would not be — the caller is the workflow
	// running this very task, and who the task belongs to is what it is telling us.
	result, err := e.hub.Execute(ctx, integrationdomain.Scope{
		UserID:      scope.UserID,
		WorkspaceID: scope.WorkspaceID,
		AgentID:     call.AgentID,
		TaskID:      call.TaskID,
	}, integrationID, call.Name, call.Arguments)
	if err != nil {
		// A tool that failed is answered rather than returned as a Go error: the runtime hands a
		// failed result back to the model, which can try another tool or answer with what it knows.
		// Ending the task here would turn "the customer's product is not in the catalogue" into a
		// failed task, which is the opposite of what the model is there for.
		return taskdomain.ToolResult{
			Content: errorContent(err),
			IsError: true,
		}, nil
	}

	return taskdomain.ToolResult{Content: result.Content}, nil
}

// connectionFor decides which connection a call runs against, or none at all.
func (e taskToolExecutor) connectionFor(
	ctx context.Context,
	app string,
	adapter integrationdomain.Adapter,
	scope taskdomain.Scope,
	call taskdomain.ToolCall,
) (uuid.UUID, error) {
	// A tool that belongs to the workspace itself runs with no connection: there is no account to
	// authorise and nothing outside this deployment to reach. The adapter declares it, and the
	// catalogue says the same thing to the tool filter — one fact, read in the two places that need it.
	if connectionless, ok := adapter.(integrationdomain.Connectionless); ok && !connectionless.NeedsConnection() {
		return uuid.Nil, nil
	}

	// Otherwise the connection is the Bolu's grant, not the caller's word.
	connection, err := e.grantedConnection(ctx, scope, call.AgentID, app)
	if err != nil {
		return uuid.Nil, err
	}

	return connection, nil
}

// grantedConnection finds the connected account a call may run against.
func (e taskToolExecutor) grantedConnection(ctx context.Context, scope taskdomain.Scope, agentID uuid.UUID, app string) (uuid.UUID, error) {
	integrationScope := integrationdomain.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}

	grants, err := e.hub.AllowedPermissions(ctx, integrationScope, agentID)
	if err != nil {
		return uuid.Nil, err
	}

	connections, err := e.hub.List(ctx, integrationScope)
	if err != nil {
		return uuid.Nil, err
	}

	for _, connection := range connections {
		if connection.App != strings.TrimSpace(app) || !connection.Connected() {
			continue
		}
		if _, granted := grants[connection.ID]; granted {
			return connection.ID, nil
		}
	}

	return uuid.Nil, fmt.Errorf(
		"%w: tidak ada akun %s yang tersambung dan diberikan ke Bolu ini", integrationdomain.ErrNotFound, app)
}

// errorContent renders a tool failure as the JSON document the model reads.
//
// It is JSON rather than a sentence because of who reads it: the model gets a document it can reason
// about, and the shape is the same one a successful call answers with, so nothing has to be parsed
// differently in the two cases.
func errorContent(err error) json.RawMessage {
	content, marshalErr := json.Marshal(map[string]any{"error": err.Error()})
	if marshalErr != nil {
		return json.RawMessage(`{"error":"the tool call failed"}`)
	}

	return content
}

// toolExecutor builds the executor a task's activities use, or nil when nothing can run a tool.
//
// A nil executor is what the runtime reports as "this tool is not available", which is the honest
// answer for a deployment with no hub: the tools are listed either way, and a deployment that cannot
// run them says so instead of failing a task at the first call.
func (c *Container) toolExecutor() taskdomain.ToolExecutor {
	if c == nil || c.Services == nil || c.Services.Integration == nil {
		return nil
	}

	return taskToolExecutor{container: c, hub: c.Services.Integration}
}
