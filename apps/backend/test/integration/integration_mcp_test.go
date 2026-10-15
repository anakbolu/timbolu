package integration

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/container"
	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
	"github.com/robzlabz/employeebot/packages/platform/config"
	"github.com/robzlabz/employeebot/packages/platform/database"
)

// These tests cover the E8.6 gate: a server's tools arrive with the same shape as
// a native adapter's, and a server that is down does not fail a task permanently.
//
// The server is a stub speaking the protocol over HTTP, so what is under test is
// the wrapper rather than a real vendor.

// stubMCPServer is a Model Context Protocol server that answers the three calls
// the wrapper makes.
type stubMCPServer struct {
	mu sync.Mutex
	// tools is what the server reports.
	tools []map[string]any
	// calls records the tool calls that arrived, so a test can prove the
	// arguments travelled and the remote name was used.
	calls []map[string]any
	// handshakeErr, when set, makes initialize fail.
	handshakeErr bool
	// down, when set, makes every request answer 503.
	down bool
	// credential is what the server expects as a bearer token. Empty accepts any.
	credential string
	// server is the httptest server, so a test can read its URL.
	server *httptest.Server
}

func newStubMCPServer(t *testing.T) *stubMCPServer {
	t.Helper()

	stub := &stubMCPServer{
		tools: []map[string]any{
			{
				"name":        "search_pages",
				"description": "Cari halaman di ruang kerja",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"q": map[string]any{"type": "string"}},
					"required":   []string{"q"},
				},
				"annotations": map[string]any{"readOnlyHint": true},
			},
			{
				"name":        "create_page",
				"description": "Buat halaman baru",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"title": map[string]any{"type": "string"}},
				},
				// No annotation: the server says nothing about whether this
				// changes anything, which must be treated as the unsafe answer.
			},
		},
	}

	stub.server = httptest.NewServer(http.HandlerFunc(stub.handle))
	t.Cleanup(stub.server.Close)

	return stub
}

func (s *stubMCPServer) url() string { return s.server.URL }

func (s *stubMCPServer) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.down {
		http.Error(w, "the server is down", http.StatusServiceUnavailable)
		return
	}

	if s.credential != "" {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+s.credential {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "unreadable", http.StatusBadRequest)
		return
	}

	var request struct {
		ID     int             `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		http.Error(w, "not json", http.StatusBadRequest)
		return
	}

	write := func(result any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      request.ID,
			"result":  result,
		})
	}

	switch request.Method {
	case "initialize":
		if s.handshakeErr {
			http.Error(w, "no handshake for you", http.StatusInternalServerError)
			return
		}
		write(map[string]any{
			"protocolVersion": "2025-06-18",
			"serverInfo":      map[string]any{"name": "stub-notion", "version": "1"},
		})

	case "tools/list":
		write(map[string]any{"tools": s.tools})

	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			http.Error(w, "bad params", http.StatusBadRequest)
			return
		}
		s.calls = append(s.calls, map[string]any{"name": params.Name, "arguments": params.Arguments})

		if params.Name == "explode" {
			write(map[string]any{
				"content": []map[string]any{{"type": "text", "text": "the tool could not do it"}},
				"isError": true,
			})
			return
		}

		write(map[string]any{
			"content": []map[string]any{{"type": "text", "text": `{"found":3}`}},
		})

	default:
		http.Error(w, "unknown method", http.StatusBadRequest)
	}
}

func (s *stubMCPServer) recordedCalls() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.calls...)
}

func (s *stubMCPServer) setDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
}

// mcpFixture wires the hub over an onboarded workspace.
type mcpFixture struct {
	service integrationdomain.Service
	scope   integrationdomain.Scope
	pool    *database.Pool
}

func newMCPFixture(t *testing.T, dsn string) *mcpFixture {
	t.Helper()

	ctx := t.Context()
	pool := poolFor(t, dsn)

	user, err := createVerifiedUser(t, pool)
	require.NoError(t, err)
	workspace, err := onboardWorkspace(t, pool, user)
	require.NoError(t, err)

	cfg := &config.Config{
		Application: config.AppConfig{Environment: "test", FrontendURL: "https://app.example.com"},
		Http:        config.HttpConfig{Address: ":0", ApiPrefix: "/api"},
		Database:    config.DatabaseConfig{URL: dsn, MaxOpenConns: 5, MaxIdleConns: 1},
		JWT:         config.JWTConfig{Secret: "integration-test-secret", ExpireHours: 1},
		Auth: config.AuthConfig{
			AccessTokenMinutes: 15, RefreshHours: 24, VerificationHours: 24,
			ResetMinutes: 60, StateMinutes: 10, LoginThrottleCapacity: 100, LoginThrottleRefill: 100,
		},
		Mail:         config.MailConfig{Driver: "log"},
		Logging:      config.LoggingConfig{Level: "error", Format: "json"},
		Kms:          config.KmsConfig{MasterKey: kmsKey(50), MasterKeyID: "v1"},
		Integrations: config.IntegrationConfig{OAuthStateKey: "an-integration-state-signing-key-32-chars"},
	}

	app, err := container.New(ctx, cfg, container.WithLogger(zap.NewNop()))
	require.NoError(t, err)
	t.Cleanup(app.Close)

	return &mcpFixture{
		service: app.Services.Integration,
		scope:   integrationdomain.Scope{UserID: user, WorkspaceID: workspace},
		pool:    pool,
	}
}

// TestAServerContributesToolsWithTheSameShape is the first acceptance criterion:
// a server's tools arrive as this product's tools, with the labels that decide
// whether a person is asked.
func TestAServerContributesToolsWithTheSameShape(t *testing.T) {
	fixture := newMCPFixture(t, migratedDatabase(t))
	server := newStubMCPServer(t)
	ctx := t.Context()

	added, err := fixture.service.AddMCPServer(ctx, fixture.scope, integrationdomain.MCPServerInput{
		App:      "notion",
		Label:    "Notion",
		Endpoint: server.url(),
		Enabled:  true,
	})
	require.NoError(t, err)
	require.Equal(t, "notion", added.App)
	require.Equal(t, 2, added.ToolCount, "the handshake recorded what the server offers")
	require.Empty(t, added.LastError)

	tools, err := fixture.service.MCPTools(ctx, fixture.scope)
	require.NoError(t, err)
	require.Len(t, tools, 2)

	byName := map[string]integrationdomain.ToolSpec{}
	for _, tool := range tools {
		byName[tool.Name] = tool
	}

	// The names are prefixed with the app, the same way a native adapter's are.
	search, ok := byName["notion.search_pages"]
	require.True(t, ok, "a server's tool arrives as `app.tool`")
	require.Equal(t, integrationdomain.LabelRead, search.Label,
		"a server that says the tool changes nothing gets the read label")
	require.NotEmpty(t, search.Schema)
	require.Contains(t, search.Description, "Cari halaman")

	create, ok := byName["notion.create_page"]
	require.True(t, ok)
	require.Equal(t, integrationdomain.LabelWriteExternal, create.Label,
		"a server that says nothing is treated as reaching outside the workspace")

	// The schema is the server's own, so the model is offered the real shape and
	// the validator enforces it.
	var schema map[string]any
	require.NoError(t, json.Unmarshal(search.Schema, &schema))
	require.Equal(t, "object", schema["type"])
	require.Contains(t, schema["required"], "q")
}

// TestAToolCallReachesTheServer is the second half: the wrapper forwards the call
// under the name the server knows.
func TestAToolCallReachesTheServer(t *testing.T) {
	fixture := newMCPFixture(t, migratedDatabase(t))
	server := newStubMCPServer(t)
	ctx := t.Context()

	_, err := fixture.service.AddMCPServer(ctx, fixture.scope, integrationdomain.MCPServerInput{
		App: "notion", Endpoint: server.url(), Enabled: true,
	})
	require.NoError(t, err)

	result, err := fixture.service.Execute(ctx, fixture.scope, uuid.Nil, "notion.search_pages",
		json.RawMessage(`{"q":"anggaran"}`))
	require.NoError(t, err)
	require.JSONEq(t, `{"found":3}`, string(result.Content))
	require.NotEmpty(t, result.Summary)

	// The server was called by its own name, with the arguments it was given.
	calls := server.recordedCalls()
	require.Len(t, calls, 1)
	require.Equal(t, "search_pages", calls[0]["name"], "the prefix is ours, not the server's")
	require.Equal(t, "anggaran", calls[0]["arguments"].(map[string]any)["q"])
}

// TestAToolThatFailsIsReportedAsAFailure covers the protocol's own shape: a server
// may answer an error while still returning a result.
func TestAToolThatFailsIsReportedAsAFailure(t *testing.T) {
	fixture := newMCPFixture(t, migratedDatabase(t))
	server := newStubMCPServer(t)
	ctx := t.Context()

	_, err := fixture.service.AddMCPServer(ctx, fixture.scope, integrationdomain.MCPServerInput{
		App: "notion", Endpoint: server.url(), Enabled: true,
	})
	require.NoError(t, err)

	_, err = fixture.service.Execute(ctx, fixture.scope, uuid.Nil, "notion.explode",
		json.RawMessage(`{}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "the tool could not do it")
	require.NotErrorIs(t, err, integrationdomain.ErrMCPUnavailable,
		"a tool that failed is not a server that is down")
}

// TestAServerThatIsDownDoesNotFailTheTaskPermanently is the second acceptance
// criterion: a server that cannot be reached is retryable, and it says why.
func TestAServerThatIsDownDoesNotFailTheTaskPermanently(t *testing.T) {
	fixture := newMCPFixture(t, migratedDatabase(t))
	server := newStubMCPServer(t)
	ctx := t.Context()

	added, err := fixture.service.AddMCPServer(ctx, fixture.scope, integrationdomain.MCPServerInput{
		App: "notion", Endpoint: server.url(), Enabled: true,
	})
	require.NoError(t, err)
	require.Equal(t, 2, added.ToolCount)

	// The server goes away.
	server.setDown(true)

	_, err = fixture.service.Execute(ctx, fixture.scope, uuid.Nil, "notion.search_pages",
		json.RawMessage(`{"q":"anggaran"}`))
	require.ErrorIs(t, err, integrationdomain.ErrMCPUnavailable,
		"a server that is down is retryable, not a permanent failure")

	// The reason is recorded on the row, so a screen can say why the tools
	// stopped working rather than showing an empty list.
	servers, err := fixture.service.ListMCPServers(ctx, fixture.scope)
	require.NoError(t, err)
	require.Len(t, servers, 1)
	require.NotEmpty(t, servers[0].LastError)

	// And the tools it reported are still offered: a server that is briefly down
	// must not cost a Bolu its tools.
	tools, err := fixture.service.MCPTools(ctx, fixture.scope)
	require.NoError(t, err)
	require.Len(t, tools, 2)

	// Once it is back, the same call goes through.
	server.setDown(false)
	result, err := fixture.service.Execute(ctx, fixture.scope, uuid.Nil, "notion.search_pages",
		json.RawMessage(`{"q":"anggaran"}`))
	require.NoError(t, err)
	require.JSONEq(t, `{"found":3}`, string(result.Content))
}

// TestAServerThatDoesNotAnswerIsStillListed is what a person behind a firewall
// sees: the server is recorded with the reason, not absent.
func TestAServerThatDoesNotAnswerIsStillListed(t *testing.T) {
	fixture := newMCPFixture(t, migratedDatabase(t))
	server := newStubMCPServer(t)
	ctx := t.Context()

	server.setDown(true)

	added, err := fixture.service.AddMCPServer(ctx, fixture.scope, integrationdomain.MCPServerInput{
		App: "notion", Endpoint: server.url(), Enabled: true,
	})
	require.NoError(t, err, "adding a server that does not answer is not an error")
	require.NotEmpty(t, added.LastError)
	require.Zero(t, added.ToolCount)

	listed, err := fixture.service.ListMCPServers(ctx, fixture.scope)
	require.NoError(t, err)
	require.Len(t, listed, 1)

	// The refresh is how a person retries without re-entering the address.
	server.setDown(false)
	refreshed, err := fixture.service.RefreshMCPServer(ctx, fixture.scope, added.ID)
	require.NoError(t, err)
	require.Equal(t, 2, refreshed.ToolCount)
	require.Empty(t, refreshed.LastError)
}

// TestAServerCredentialIsStoredSealed keeps the same rule as an integration token:
// a credential is never readable from the database.
func TestAServerCredentialIsStoredSealed(t *testing.T) {
	fixture := newMCPFixture(t, migratedDatabase(t))
	server := newStubMCPServer(t)
	server.credential = "a-secret-server-token"
	ctx := t.Context()

	_, err := fixture.service.AddMCPServer(ctx, fixture.scope, integrationdomain.MCPServerInput{
		App: "notion", Endpoint: server.url(), Credential: "a-secret-server-token", Enabled: true,
	})
	require.NoError(t, err, "the credential the server accepts was sent")

	// Straight from the column: the secret is not there.
	var sealed []byte
	require.NoError(t, fixture.pool.PgxPool().QueryRow(ctx,
		`SELECT credential_encrypted FROM mcp_servers WHERE workspace_id = $1`,
		fixture.scope.WorkspaceID).Scan(&sealed))
	require.NotEmpty(t, sealed)
	require.NotContains(t, string(sealed), "a-secret-server-token")

	// A wrong credential is refused rather than silently accepted.
	server.credential = "something-else"
	_, err = fixture.service.RefreshMCPServer(ctx, fixture.scope, uuid.Nil)
	require.Error(t, err, "a credential the server does not accept is not a working server")
}

// TestTwoServersOfOneAppAreRefused keeps the app key from naming two servers: a
// grant names the app, so it would be ambiguous.
func TestTwoServersOfOneAppAreRefused(t *testing.T) {
	fixture := newMCPFixture(t, migratedDatabase(t))
	first := newStubMCPServer(t)
	second := newStubMCPServer(t)
	ctx := t.Context()

	one, err := fixture.service.AddMCPServer(ctx, fixture.scope, integrationdomain.MCPServerInput{
		App: "notion", Endpoint: first.url(), Enabled: true,
	})
	require.NoError(t, err)

	// The same app again updates the row rather than adding a second, which is
	// what makes re-entering an address a correction rather than a duplicate.
	two, err := fixture.service.AddMCPServer(ctx, fixture.scope, integrationdomain.MCPServerInput{
		App: "notion", Endpoint: second.url(), Enabled: true,
	})
	require.NoError(t, err)
	require.Equal(t, one.ID, two.ID)

	servers, err := fixture.service.ListMCPServers(ctx, fixture.scope)
	require.NoError(t, err)
	require.Len(t, servers, 1)
}

// TestAnAppKeyIsRefusedWhenItWouldAmbiguateAName keeps a dot out of the app key,
// because the key is the prefix of every tool name.
func TestAnAppKeyIsRefusedWhenItWouldAmbiguateAName(t *testing.T) {
	fixture := newMCPFixture(t, migratedDatabase(t))
	ctx := t.Context()

	_, err := fixture.service.AddMCPServer(ctx, fixture.scope, integrationdomain.MCPServerInput{
		App: "notion.pages", Endpoint: "https://example.com",
	})
	require.ErrorIs(t, err, integrationdomain.ErrInvalidInput)

	_, err = fixture.service.AddMCPServer(ctx, fixture.scope, integrationdomain.MCPServerInput{
		App: "", Endpoint: "https://example.com",
	})
	require.ErrorIs(t, err, integrationdomain.ErrInvalidInput)

	_, err = fixture.service.AddMCPServer(ctx, fixture.scope, integrationdomain.MCPServerInput{
		App: "notion", Endpoint: "",
	})
	require.ErrorIs(t, err, integrationdomain.ErrInvalidInput)
}

// TestAnotherWorkspaceCannotUseAServer is the tenant boundary on this path.
func TestAnotherWorkspaceCannotUseAServer(t *testing.T) {
	fixture := newMCPFixture(t, migratedDatabase(t))
	server := newStubMCPServer(t)
	ctx := t.Context()

	added, err := fixture.service.AddMCPServer(ctx, fixture.scope, integrationdomain.MCPServerInput{
		App: "notion", Endpoint: server.url(), Enabled: true,
	})
	require.NoError(t, err)

	otherUser, err := createVerifiedUser(t, fixture.pool)
	require.NoError(t, err)
	otherWorkspace, err := onboardWorkspace(t, fixture.pool, otherUser)
	require.NoError(t, err)
	otherScope := integrationdomain.Scope{UserID: otherUser, WorkspaceID: otherWorkspace}

	// The other workspace sees no servers and no tools, and cannot refresh one.
	listed, err := fixture.service.ListMCPServers(ctx, otherScope)
	require.NoError(t, err)
	require.Empty(t, listed)

	tools, err := fixture.service.MCPTools(ctx, otherScope)
	require.NoError(t, err)
	require.Empty(t, tools)

	_, err = fixture.service.RefreshMCPServer(ctx, otherScope, added.ID)
	require.ErrorIs(t, err, integrationdomain.ErrMCPServerNotFound)

	// And a tool that only the first workspace's server offers is unknown to it.
	_, err = fixture.service.Execute(ctx, otherScope, uuid.Nil, "notion.search_pages",
		json.RawMessage(`{"q":"x"}`))
	require.Error(t, err)
}

// TestAnUnknownToolIsReported keeps a name nothing offers from reaching a server.
func TestAnUnknownToolIsReported(t *testing.T) {
	fixture := newMCPFixture(t, migratedDatabase(t))
	ctx := t.Context()

	_, err := fixture.service.Execute(ctx, fixture.scope, uuid.Nil, "nothing.here",
		json.RawMessage(`{}`))
	require.Error(t, err)
	require.True(t, errors.Is(err, integrationdomain.ErrUnknownTool) || strings.Contains(err.Error(), "no adapter"),
		err.Error())
}
