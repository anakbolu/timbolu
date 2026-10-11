package domain

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// This file is the MCP half of the adapter contract.
//
// An application that publishes an MCP server needs no adapter of its own: the
// server already says what it offers. What this deployment adds is the governance
// around it — which workspace added which server, which Bolu may use it, and how a
// tool the server describes becomes a tool this product can decide about.
//
// Servers belong to a workspace, while the adapters in the registry belong to the
// deployment. That is why the tools a server contributes are read per scope rather
// than registered globally.

// Errors the MCP path reports.
var (
	// ErrMCPServerNotFound means the workspace has no such server.
	ErrMCPServerNotFound = errors.New("integration: MCP server not found")
	// ErrMCPUnavailable means the server could not be reached. It is retryable:
	// a server that is down is usually down briefly.
	ErrMCPUnavailable = errors.New("integration: the MCP server is unavailable")
)

// MCPServer is one server a workspace added.
type MCPServer struct {
	ID       uuid.UUID
	App      string
	Label    string
	Endpoint string
	Enabled  bool
	// ToolCount is how many tools the server last reported, so a screen can show
	// what adding it would give without reaching it.
	ToolCount int
	// LastError is the last thing that went wrong talking to it. It is how a
	// screen says why a server's tools are missing instead of showing nothing.
	LastError string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// MCPServerInput is what a person supplies to add one.
type MCPServerInput struct {
	// App is the key the server is registered under, and the prefix of every tool
	// it contributes. It is what a grant names.
	App      string
	Label    string
	Endpoint string
	// Credential is sent as a bearer token. Empty means the server is open.
	Credential string
	Enabled    bool
}

// MCPTool is one tool a server reported, in this product's terms.
type MCPTool struct {
	// Name is the qualified name the model calls: `app.tool`.
	Name        string
	Label       string
	Description string
	Schema      []byte
	// RemoteName is what the server calls it, which is what a call must use.
	RemoteName string
}

// MCPRepository stores the servers a workspace added.
type MCPRepository interface {
	ListMCPServers(ctx context.Context, scope Scope) ([]MCPServer, error)
	GetMCPServer(ctx context.Context, scope Scope, id uuid.UUID) (MCPServer, error)
	// UpsertMCPServer stores a server, keyed by app, so re-adding one updates it
	// rather than creating a second.
	UpsertMCPServer(ctx context.Context, scope Scope, input MCPServerInput, sealedCredential []byte) (MCPServer, error)
	// SaveMCPTools records the tool list a server reported.
	SaveMCPTools(ctx context.Context, scope Scope, id uuid.UUID, tools []MCPTool) error
	// MCPServerTools returns the tool list a server last reported, which is what
	// a task is offered without reaching the server first.
	MCPServerTools(ctx context.Context, scope Scope, id uuid.UUID) ([]MCPTool, error)
	// SetMCPServerError records why a server could not be reached.
	SetMCPServerError(ctx context.Context, scope Scope, id uuid.UUID, reason string) error
	DeleteMCPServer(ctx context.Context, scope Scope, id uuid.UUID) error
	// MCPServerCredential returns the sealed credential of a server.
	MCPServerCredential(ctx context.Context, scope Scope, id uuid.UUID) ([]byte, error)
}

// MCPClient is one conversation with a server.
//
// It is a port so the domain does not carry an HTTP client, and so a test can
// point the product at a server it runs itself.
type MCPClient interface {
	// Initialize performs the protocol handshake.
	Initialize(ctx context.Context) error
	// Tools lists what the server offers.
	Tools(ctx context.Context) ([]MCPRemoteTool, error)
	// Call runs one tool by the name the server knows it by.
	Call(ctx context.Context, remoteName string, arguments []byte) (MCPCallResult, error)
}

// MCPRemoteTool is a tool as the server describes it.
type MCPRemoteTool struct {
	Name        string
	Description string
	Schema      []byte
	// ReadOnly is what the server says about whether the tool changes anything.
	// A server that says nothing is treated as though it changes something.
	ReadOnly bool
}

// MCPCallResult is what a call answered.
type MCPCallResult struct {
	// Content is the server's answer, already reduced to the text a model reads.
	Content []byte
	// Summary is one line for the step ledger.
	Summary string
}

// MCPClientFactory builds a client for one server, given the opened credential.
type MCPClientFactory interface {
	Client(endpoint, credential string) (MCPClient, error)
}
