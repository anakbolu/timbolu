package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
)

// AddMCPServer registers a server a workspace wants to use, then asks it what it
// offers.
//
// The handshake happens here rather than lazily so a person who mistyped an
// address finds out immediately, while they are still looking at the form.
func (s *service) AddMCPServer(ctx context.Context, scope domain.Scope, input domain.MCPServerInput) (domain.MCPServer, error) {
	if err := validateMCPServerInput(input); err != nil {
		return domain.MCPServer{}, err
	}
	if s.deps.MCP == nil || s.deps.MCPClients == nil {
		return domain.MCPServer{}, fmt.Errorf("%w: MCP servers are not configured", domain.ErrInvalidInput)
	}

	var sealed []byte
	if strings.TrimSpace(input.Credential) != "" {
		if s.deps.Cipher == nil {
			return domain.MCPServer{}, domain.ErrNoCipher
		}
		value, err := s.deps.Cipher.Seal(ctx, scope.WorkspaceID, input.Credential)
		if err != nil {
			return domain.MCPServer{}, fmt.Errorf("integration: seal MCP credential: %w", err)
		}
		sealed = value
	}

	server, err := s.deps.MCP.UpsertMCPServer(ctx, scope, input, sealed)
	if err != nil {
		return domain.MCPServer{}, err
	}

	// A server that does not answer is stored anyway, with the reason: a person
	// who typed the right address but is behind a firewall should see the server
	// listed as unreachable rather than absent.
	if _, err := s.refreshMCPTools(ctx, scope, server, input.Credential); err != nil {
		s.deps.Logger.Warn("an MCP server was added but did not answer",
			zap.String("app", server.App),
			zap.Error(err),
		)
		return s.deps.MCP.GetMCPServer(ctx, scope, server.ID)
	}

	return s.deps.MCP.GetMCPServer(ctx, scope, server.ID)
}

// ListMCPServers returns the workspace's servers.
func (s *service) ListMCPServers(ctx context.Context, scope domain.Scope) ([]domain.MCPServer, error) {
	if s.deps.MCP == nil {
		return nil, nil
	}
	return s.deps.MCP.ListMCPServers(ctx, scope)
}

// RemoveMCPServer removes one.
func (s *service) RemoveMCPServer(ctx context.Context, scope domain.Scope, id uuid.UUID) error {
	if s.deps.MCP == nil {
		return fmt.Errorf("%w: MCP servers are not configured", domain.ErrInvalidInput)
	}
	if id == uuid.Nil {
		return fmt.Errorf("%w: a server id is required", domain.ErrInvalidInput)
	}
	return s.deps.MCP.DeleteMCPServer(ctx, scope, id)
}

// RefreshMCPServer asks one server what it offers and records the answer.
func (s *service) RefreshMCPServer(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.MCPServer, error) {
	if s.deps.MCP == nil || s.deps.MCPClients == nil {
		return domain.MCPServer{}, fmt.Errorf("%w: MCP servers are not configured", domain.ErrInvalidInput)
	}

	server, err := s.deps.MCP.GetMCPServer(ctx, scope, id)
	if err != nil {
		return domain.MCPServer{}, err
	}

	credential, err := s.mcpCredential(ctx, scope, id)
	if err != nil {
		return domain.MCPServer{}, err
	}

	if _, err := s.refreshMCPTools(ctx, scope, server, credential); err != nil {
		return s.deps.MCP.GetMCPServer(ctx, scope, id)
	}
	return s.deps.MCP.GetMCPServer(ctx, scope, id)
}

// MCPTools lists the tools the workspace's servers contribute.
//
// It reads what each server last reported rather than reaching every server: a
// task is waiting, and a server that is briefly down must not cost a Bolu its
// tools. The list is refreshed when a server is added, when somebody asks for it,
// and when a call to it fails.
func (s *service) MCPTools(ctx context.Context, scope domain.Scope) ([]domain.ToolSpec, error) {
	if s.deps.MCP == nil {
		return nil, nil
	}

	servers, err := s.deps.MCP.ListMCPServers(ctx, scope)
	if err != nil {
		return nil, err
	}

	var tools []domain.ToolSpec
	for _, server := range servers {
		if !server.Enabled {
			continue
		}

		recorded, err := s.deps.MCP.MCPServerTools(ctx, scope, server.ID)
		if err != nil {
			return nil, err
		}
		for _, tool := range recorded {
			tools = append(tools, domain.ToolSpec{
				Name:        tool.Name,
				Label:       tool.Label,
				Description: tool.Description,
				Schema:      json.RawMessage(tool.Schema),
			})
		}
	}
	return tools, nil
}

// executeMCP runs one tool on the server that offers it.
//
// A failure to reach the server is reported as retryable, because a server that is
// down is usually down briefly and a task should wait rather than fail. A server
// that answers with a failure is the tool's own failure, which is reported as what
// it is.
func (s *service) executeMCP(ctx context.Context, scope domain.Scope, tool string, arguments json.RawMessage) (domain.Result, error) {
	app, _, _ := strings.Cut(tool, ".")

	servers, err := s.deps.MCP.ListMCPServers(ctx, scope)
	if err != nil {
		return domain.Result{}, err
	}

	var server domain.MCPServer
	found := false
	for _, candidate := range servers {
		if candidate.App == app && candidate.Enabled {
			server, found = candidate, true
			break
		}
	}
	if !found {
		return domain.Result{}, fmt.Errorf("%w: %q", domain.ErrUnknownTool, tool)
	}

	// The tool's own name on the server is the part after the app.
	remote := strings.TrimPrefix(tool, app+".")
	if remote == tool || remote == "" {
		return domain.Result{}, fmt.Errorf("%w: %q", domain.ErrUnknownTool, tool)
	}

	credential, err := s.mcpCredential(ctx, scope, server.ID)
	if err != nil {
		return domain.Result{}, err
	}

	client, err := s.deps.MCPClients.Client(server.Endpoint, credential)
	if err != nil {
		return domain.Result{}, err
	}

	result, err := client.Call(ctx, remote, arguments)
	if err != nil {
		// The failure is recorded on the server row so the Integrations screen can
		// say why its tools stopped working, and reported as retryable when it was
		// a transport failure rather than the tool's own.
		s.recordMCPFailure(ctx, scope, server.ID, err)
		if errors.Is(err, domain.ErrMCPUnavailable) {
			return domain.Result{}, fmt.Errorf("%w: %s: %w", domain.ErrMCPUnavailable, server.App, err)
		}
		return domain.Result{}, err
	}

	return domain.Result{Content: result.Content, Summary: result.Summary}, nil
}

// refreshMCPTools asks a server what it offers and records the answer.
func (s *service) refreshMCPTools(ctx context.Context, scope domain.Scope, server domain.MCPServer, credential string) ([]domain.MCPTool, error) {
	client, err := s.deps.MCPClients.Client(server.Endpoint, credential)
	if err != nil {
		return nil, err
	}

	if err := client.Initialize(ctx); err != nil {
		s.recordMCPFailure(ctx, scope, server.ID, err)
		return nil, err
	}

	remote, err := client.Tools(ctx)
	if err != nil {
		s.recordMCPFailure(ctx, scope, server.ID, err)
		return nil, err
	}

	tools := make([]domain.MCPTool, 0, len(remote))
	for _, tool := range remote {
		tools = append(tools, domain.MCPTool{
			Name:        server.App + "." + tool.Name,
			Label:       labelForMCPTool(tool),
			Description: tool.Description,
			Schema:      tool.Schema,
			RemoteName:  tool.Name,
		})
	}

	if err := s.deps.MCP.SaveMCPTools(ctx, scope, server.ID, tools); err != nil {
		return nil, err
	}
	return tools, nil
}

// labelForMCPTool decides how a server's tool is governed.
//
// The protocol's own hint is honoured when a server states one, because that is
// what the hint is for. A server that says nothing is treated as reaching outside
// the workspace: the product's rule is that a person holds the door to anything
// that leaves, and reading "probably fine" out of silence is how that rule stops
// being true.
func labelForMCPTool(tool domain.MCPRemoteTool) string {
	if tool.ReadOnly {
		return domain.LabelRead
	}
	return domain.LabelWriteExternal
}

// mcpCredential opens a server's stored credential.
func (s *service) mcpCredential(ctx context.Context, scope domain.Scope, id uuid.UUID) (string, error) {
	sealed, err := s.deps.MCP.MCPServerCredential(ctx, scope, id)
	if err != nil {
		return "", err
	}
	if len(sealed) == 0 {
		return "", nil
	}
	if s.deps.Cipher == nil {
		return "", domain.ErrNoCipher
	}

	credential, err := s.deps.Cipher.Open(ctx, scope.WorkspaceID, sealed)
	if err != nil {
		return "", fmt.Errorf("integration: open MCP credential: %w", err)
	}
	return credential, nil
}

// recordMCPFailure notes why a server could not be reached, so a screen can say so.
func (s *service) recordMCPFailure(ctx context.Context, scope domain.Scope, id uuid.UUID, err error) {
	if s.deps.MCP == nil {
		return
	}
	if recordErr := s.deps.MCP.SetMCPServerError(ctx, scope, id, err.Error()); recordErr != nil {
		s.deps.Logger.Warn("an MCP failure could not be recorded",
			zap.String("server", id.String()),
			zap.Error(recordErr),
		)
	}
}

// validateMCPServerInput refuses what the schema or the protocol would.
func validateMCPServerInput(input domain.MCPServerInput) error {
	app := strings.TrimSpace(input.App)
	if app == "" {
		return fmt.Errorf("%w: an app key is required", domain.ErrInvalidInput)
	}
	if strings.Contains(app, ".") {
		return fmt.Errorf("%w: the app key must not contain a dot, because it prefixes tool names", domain.ErrInvalidInput)
	}
	if strings.TrimSpace(input.Endpoint) == "" {
		return fmt.Errorf("%w: an endpoint is required", domain.ErrInvalidInput)
	}
	return nil
}
