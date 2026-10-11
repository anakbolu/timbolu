package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	approvaldomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/approval/domain"
	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
)

// integrationExecutor carries an approved action out.
//
// It is the piece the approval gate was built to wait for. Until this existed an
// approved draft stopped at the boundary with a reason, because there was nothing
// that could actually send anything; now the boundary is where the action happens.
//
// It sits in the container because it is the only package allowed to know both
// modules: the approval module says an action was approved, and the integration
// module knows how to perform it.
type integrationExecutor struct {
	integrations integrationdomain.Service
	logger       *zap.Logger
}

// Send performs one approved action.
//
// The connection is chosen from the grant the Bolu holds, not from the request: an
// action approved for one Bolu must not be sent from a mailbox that Bolu was never
// given. When a Bolu holds several connections to one application, the first is
// used and the choice is logged — the alternative, asking the approver which
// mailbox, is a question the product does not yet have a way to ask.
func (e integrationExecutor) Send(ctx context.Context, scope approvaldomain.Scope, draft approvaldomain.Draft) (json.RawMessage, error) {
	if e.integrations == nil {
		return nil, errors.New("integration: the integration hub is not configured")
	}

	action := strings.TrimSpace(draft.ActionKind)
	if action == "" {
		return nil, fmt.Errorf("integration: the draft names no action")
	}

	integrationScope := integrationdomain.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}

	connection, err := e.connectionFor(ctx, integrationScope, draft.AgentID, action)
	if err != nil {
		return nil, err
	}

	result, err := e.integrations.Execute(ctx, integrationScope, connection.ID, action, draft.Payload)
	if err != nil {
		return nil, err
	}

	e.logger.Info("an approved action was carried out",
		zap.String("action", action),
		zap.String("app", connection.App),
		zap.String("account", connection.AccountLabel),
		zap.String("draft", draft.ID.String()),
	)

	return result.Content, nil
}

// connectionFor picks the connection an action runs against.
func (e integrationExecutor) connectionFor(ctx context.Context, scope integrationdomain.Scope, agentID uuid.UUID, action string) (integrationdomain.Integration, error) {
	app, _, found := strings.Cut(action, ".")
	if !found {
		// A tool that is not named `app.action` belongs to a server the workspace
		// added, which is reached without a connection row.
		return integrationdomain.Integration{App: action}, nil
	}

	// A server the workspace added is named by its app, and has no connection row:
	// the server is the connection.
	if _, ok := e.mcpServerFor(ctx, scope, app); ok {
		return integrationdomain.Integration{App: app}, nil
	}

	grants, err := e.integrations.AllowedPermissions(ctx, scope, agentID)
	if err != nil {
		return integrationdomain.Integration{}, err
	}

	connections, err := e.integrations.List(ctx, scope)
	if err != nil {
		return integrationdomain.Integration{}, err
	}

	var candidates []integrationdomain.Integration
	for _, connection := range connections {
		if connection.App != app || !connection.Connected() {
			continue
		}
		// The grant is what decides, not the action's label: an approved action
		// still must not run on a mailbox the Bolu was never given.
		if _, granted := grants[connection.ID]; !granted {
			continue
		}
		candidates = append(candidates, connection)
	}

	switch len(candidates) {
	case 0:
		return integrationdomain.Integration{}, fmt.Errorf(
			"%w: no connected %s account is granted to this Bolu", integrationdomain.ErrNotFound, app)
	case 1:
		return candidates[0], nil
	default:
		// Said out loud rather than silently picked: an operator reading the log
		// can see which mailbox an action went out of.
		e.logger.Info("a Bolu holds several accounts for one app; the first is used",
			zap.String("app", app),
			zap.Int("accounts", len(candidates)),
			zap.String("chosen", candidates[0].AccountLabel),
		)
		return candidates[0], nil
	}
}

// mcpServerFor reports whether the workspace added a server under this app.
func (e integrationExecutor) mcpServerFor(ctx context.Context, scope integrationdomain.Scope, app string) (integrationdomain.MCPServer, bool) {
	servers, err := e.integrations.ListMCPServers(ctx, scope)
	if err != nil {
		return integrationdomain.MCPServer{}, false
	}
	for _, server := range servers {
		if server.App == app && server.Enabled {
			return server, true
		}
	}
	return integrationdomain.MCPServer{}, false
}

// approvalSender builds the executor the approval gate uses.
//
// It returns nil when there is no integration hub to run anything with, which is
// what a deployment without one looks like: an approved action is then held with a
// reason rather than recorded as sent.
func (c *Container) approvalSender() approvaldomain.Sender {
	if c == nil || c.Services == nil || c.Services.Integration == nil {
		return nil
	}
	return integrationExecutor{integrations: c.Services.Integration, logger: c.Logger}
}

// compile-time check that the executor satisfies the port it is wired to.
var _ approvaldomain.Sender = integrationExecutor{}
