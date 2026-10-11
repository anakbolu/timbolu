package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/config"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/mcp"
)

// mcpClients builds a client for one MCP server.
//
// The credential is handed over already opened, so the client never touches the
// key ring and a token is never stored on a client that outlives the call.
type mcpClients struct {
	timeout time.Duration
}

// newMCPClients builds the factory.
//
// A deployment that has no MCP servers configured still gets a factory: whether a
// workspace added a server is a runtime fact, not a startup one, so the decision
// to reach a server belongs to the call rather than to the assembly.
func newMCPClients(cfg *config.Config) *mcpClients {
	timeout := cfg.Integrations.MCPTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &mcpClients{timeout: timeout}
}

// Client builds a client for a server.
func (f *mcpClients) Client(endpoint, credential string) (integrationdomain.MCPClient, error) {
	client, err := mcp.New(mcp.Config{
		Endpoint:   endpoint,
		Credential: credential,
		Timeout:    f.timeout,
	})
	if err != nil {
		return nil, err
	}
	return mcpClient{inner: client}, nil
}

// mcpClient adapts the protocol client to the port the integration domain
// declares.
//
// The translation is not busywork: the platform package speaks the protocol's own
// types, and the domain speaks the product's. Keeping them apart is what lets the
// protocol revision change without the module noticing.
type mcpClient struct {
	inner *mcp.Client
}

// Initialize performs the handshake.
func (c mcpClient) Initialize(ctx context.Context) error {
	if _, err := c.inner.Initialize(ctx); err != nil {
		return translateMCPError(err)
	}
	return nil
}

// Tools lists what the server offers.
func (c mcpClient) Tools(ctx context.Context) ([]integrationdomain.MCPRemoteTool, error) {
	remote, err := c.inner.Tools(ctx)
	if err != nil {
		return nil, translateMCPError(err)
	}

	out := make([]integrationdomain.MCPRemoteTool, 0, len(remote))
	for _, tool := range remote {
		// A tool the server does not name cannot be called, so it is skipped
		// rather than offered and then failing.
		if strings.TrimSpace(tool.Name) == "" {
			continue
		}

		schema := tool.InputSchema
		if len(schema) == 0 {
			// A server that declares no arguments still has a shape: an empty
			// object. Offering it with no schema would fail validation at the
			// first call.
			schema = []byte(`{"type":"object"}`)
		}

		out = append(out, integrationdomain.MCPRemoteTool{
			Name:        tool.Name,
			Description: tool.Description,
			Schema:      schema,
			ReadOnly:    tool.ReadOnly(),
		})
	}
	return out, nil
}

// Call runs one tool.
func (c mcpClient) Call(ctx context.Context, remoteName string, arguments []byte) (integrationdomain.MCPCallResult, error) {
	result, err := c.inner.Call(ctx, remoteName, arguments)
	if err != nil {
		return integrationdomain.MCPCallResult{}, translateMCPError(err)
	}

	text := result.Text()
	if text == "" {
		text = "{}"
	}

	// The answer is kept as JSON so a model reads the server's own words rather
	// than this adapter's prose about them.
	encoded, err := jsonObjectOrString(text)
	if err != nil {
		return integrationdomain.MCPCallResult{}, err
	}

	return integrationdomain.MCPCallResult{
		Content: encoded,
		Summary: fmt.Sprintf("%s menjawab", remoteName),
	}, nil
}

// translateMCPError renders a protocol failure as the module's own sentinel.
//
// The distinction that matters to a caller is whether the server could be reached
// at all: a server that is down is retried, and a tool that failed is not.
func translateMCPError(err error) error {
	switch {
	case errors.Is(err, mcp.ErrServerUnavailable):
		return fmt.Errorf("%w: %w", integrationdomain.ErrMCPUnavailable, err)
	case errors.Is(err, mcp.ErrUnauthorized):
		return fmt.Errorf("%w: %w", integrationdomain.ErrMCPUnavailable, err)
	default:
		return err
	}
}

// jsonObjectOrString keeps a server's answer as JSON.
//
// A server may answer with an object, which is passed through, or with prose,
// which is wrapped so what the model reads is always a JSON value.
func jsonObjectOrString(text string) ([]byte, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return []byte(`{}`), nil
	}

	if json.Valid([]byte(trimmed)) {
		return []byte(trimmed), nil
	}

	encoded, err := json.Marshal(map[string]any{"text": text})
	if err != nil {
		return nil, fmt.Errorf("integration: encode MCP answer: %w", err)
	}
	return encoded, nil
}

// compile-time check that the adapters satisfy the ports they are wired to.
var (
	_ integrationdomain.MCPClientFactory = (*mcpClients)(nil)
	_ integrationdomain.MCPClient        = mcpClient{}
)
