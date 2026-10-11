package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/container"
	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/config"
)

// These tests drive the integration endpoints over the real router, so what is
// covered is the HTTP surface a client actually uses: the shapes it receives, the
// status codes it acts on, and the redirect a provider's callback produces.

// handlerAdapter is an application the registry offers, with one tool of each
// label so the screen has something to render.
type handlerAdapter struct{}

func (handlerAdapter) App() string { return "gmail" }

func (handlerAdapter) Tools() []integrationdomain.ToolSpec {
	return []integrationdomain.ToolSpec{
		{Name: "gmail.search", Label: integrationdomain.LabelRead, Description: "Cari email",
			Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "gmail.create_draft", Label: integrationdomain.LabelWriteInternal, Description: "Buat draf",
			Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "gmail.send", Label: integrationdomain.LabelWriteExternal, Description: "Kirim email",
			Schema: json.RawMessage(`{"type":"object"}`)},
	}
}

func (handlerAdapter) Triggers() []integrationdomain.TriggerSpec {
	return []integrationdomain.TriggerSpec{
		{App: "gmail", Kind: integrationdomain.TriggerPolling, Event: "gmail.email_received", Description: "Email masuk"},
	}
}

func (handlerAdapter) Execute(context.Context, integrationdomain.Call) (integrationdomain.Result, error) {
	return integrationdomain.Result{Content: json.RawMessage(`{}`)}, nil
}

func (handlerAdapter) AuthCodeURL(state, redirectURI string) string {
	return "https://accounts.example.com/o/oauth2/auth?state=" + state + "&redirect_uri=" + redirectURI
}

func (handlerAdapter) Exchange(context.Context, string, string) (integrationdomain.OAuthTokens, error) {
	return integrationdomain.OAuthTokens{AccessToken: "ya29.a-token", ExpiresAt: new(time.Now().Add(time.Hour))}, nil
}

func (handlerAdapter) Refresh(context.Context, string) (integrationdomain.OAuthTokens, error) {
	return integrationdomain.OAuthTokens{AccessToken: "ya29.refreshed"}, nil
}

func (handlerAdapter) DefaultScopes() []string { return []string{"gmail.readonly"} }

// newHubAPI builds the API with the integration hub wired, so the endpoints are
// driven over the real router.
func newHubAPI(t *testing.T, dsn string) (*fiber.App, *identityAPI) {
	t.Helper()

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
		Kms:          config.KmsConfig{MasterKey: kmsKey(70), MasterKeyID: "v1"},
		Integrations: config.IntegrationConfig{OAuthStateKey: "an-integration-state-signing-key-32-chars"},
	}

	mailer := &captureMailer{}

	app, err := container.New(t.Context(), cfg,
		container.WithLogger(zap.NewNop()),
		container.WithMailer(mailer),
		container.WithIntegrationAdapters(handlerAdapter{}),
	)
	require.NoError(t, err)
	t.Cleanup(app.Close)

	return app.App(), &identityAPI{app: app.App(), mailer: mailer}
}

// signedIn registers, verifies, onboards, and returns the headers a request needs.
func signedIn(t *testing.T, api *identityAPI) (token, workspace string) {
	t.Helper()

	const email = "hub@example.com"
	const password = "a-good-password"

	api.verifyEmail(t, api.register(t, email, password))
	token, _ = api.login(t, email, password)

	status, payload, body := api.call(t, fiber.MethodPost, "/api/workspaces/onboard",
		`{"name":"Toko Sinar","business_field":"Retail","timezone":"Asia/Jakarta"}`, bearer(token))
	require.Equal(t, http.StatusOK, status, body)

	workspace = data(t, payload)["workspace"].(map[string]any)["id"].(string)
	return token, workspace
}

// TestTheIntegrationEndpointsAnswer covers the reads a screen makes and the writes
// it offers, over the router rather than by calling the service.
func TestTheIntegrationEndpointsAnswer(t *testing.T) {
	dsn := migratedDatabase(t)
	app, api := newHubAPI(t, dsn)
	api.app = app

	token, workspace := signedIn(t, api)
	headers := withWorkspace(token, workspace)

	// The application list is what a person sees before connecting anything.
	status, payload, body := api.call(t, fiber.MethodGet, "/api/integrations/adapters", "", headers)
	require.Equal(t, http.StatusOK, status, body)

	adapters := payload["data"].([]any)
	require.Len(t, adapters, 1)
	entry := adapters[0].(map[string]any)
	require.Equal(t, "gmail", entry["app"])
	require.Equal(t, true, entry["oauth"])

	tools := entry["tools"].([]any)
	require.Len(t, tools, 3)

	// The label is restated for a screen, because "write_external" is not
	// something a person should have to translate.
	needsHuman := map[string]bool{}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		needsHuman[tool["name"].(string)] = tool["needs_human"].(bool)
	}
	require.False(t, needsHuman["gmail.search"])
	require.False(t, needsHuman["gmail.create_draft"])
	require.True(t, needsHuman["gmail.send"])

	triggers := entry["triggers"].([]any)
	require.Len(t, triggers, 1)
	require.Equal(t, "gmail.email_received", triggers[0].(map[string]any)["event"])

	// Nothing is connected yet.
	status, payload, body = api.call(t, fiber.MethodGet, "/api/integrations", "", headers)
	require.Equal(t, http.StatusOK, status, body)
	require.Empty(t, payload["data"].([]any))

	// Starting a flow redirects to the provider rather than answering JSON: a
	// browser has to follow the consent page.
	status, _, _ = api.call(t, fiber.MethodGet, "/api/integrations/gmail/connect", "", headers)
	require.Equal(t, http.StatusFound, status)

	// An application nobody registered is refused before any redirect.
	status, _, body = api.call(t, fiber.MethodGet, "/api/integrations/notion/connect", "", headers)
	require.Equal(t, http.StatusBadRequest, status, body)

	// The callback with a state this deployment never issued sends the browser
	// back to the app with the reason, rather than answering JSON to a person
	// staring at a blank page.
	status, _, _ = api.call(t, fiber.MethodGet, "/api/integrations/oauth/callback?code=x&state=forged", "", nil)
	require.Equal(t, http.StatusFound, status)

	// A provider's own refusal is reported as a decision rather than a failure.
	status, _, _ = api.call(t, fiber.MethodGet, "/api/integrations/oauth/callback?error=access_denied", "", nil)
	require.Equal(t, http.StatusFound, status)
}

// TestTheGrantEndpointsAnswer covers the governance surface.
func TestTheGrantEndpointsAnswer(t *testing.T) {
	dsn := migratedDatabase(t)
	app, api := newHubAPI(t, dsn)
	api.app = app

	token, workspace := signedIn(t, api)
	headers := withWorkspace(token, workspace)

	// The Bolu the workspace was created with.
	status, payload, body := api.call(t, fiber.MethodGet, "/api/agents", "", headers)
	require.Equal(t, http.StatusOK, status, body)

	agents := payload["data"].([]any)
	require.NotEmpty(t, agents)
	agentID := agents[0].(map[string]any)["id"].(string)

	// A Bolu with no grants yet.
	status, payload, body = api.call(t, fiber.MethodGet, "/api/agents/"+agentID+"/grants", "", headers)
	require.Equal(t, http.StatusOK, status, body)
	require.Empty(t, payload["data"].([]any))

	// A permission the schema does not have is refused.
	status, _, body = api.call(t, fiber.MethodPut, "/api/agents/"+agentID+"/grants/"+uuid.NewString(),
		`{"permission":"superuser"}`, headers)
	require.Equal(t, http.StatusBadRequest, status, body)

	// An id that is not a uuid is refused before anything else happens. The
	// permission routes are the ones that carry two ids, so they are where this
	// can be checked.
	status, _, body = api.call(t, fiber.MethodPut, "/api/agents/"+agentID+"/grants/not-a-uuid",
		`{"permission":"read"}`, headers)
	require.Equal(t, http.StatusBadRequest, status, body)

	status, _, body = api.call(t, fiber.MethodDelete, "/api/agents/not-a-uuid/grants/"+uuid.NewString(), "", headers)
	require.Equal(t, http.StatusBadRequest, status, body)

	// An agent id that is not a uuid is refused on the read too.
	status, _, body = api.call(t, fiber.MethodGet, "/api/agents/not-a-uuid/grants", "", headers)
	require.Equal(t, http.StatusBadRequest, status, body)
}

// TestTheCallbackReportsEveryOutcome is the redirect a person's browser lands on,
// which is why every branch is a redirect rather than an error body.
func TestTheCallbackReportsEveryOutcome(t *testing.T) {
	dsn := migratedDatabase(t)
	app, api := newHubAPI(t, dsn)
	api.app = app

	token, workspace := signedIn(t, api)
	headers := withWorkspace(token, workspace)

	// A flow is started, so a genuine state exists.
	status, _, _ := api.call(t, fiber.MethodGet, "/api/integrations/gmail/connect", "", headers)
	require.Equal(t, http.StatusFound, status)

	// The callback without a state at all is refused and sent back with a reason.
	status, _, _ = api.call(t, fiber.MethodGet, "/api/integrations/oauth/callback?code=abc", "", nil)
	require.Equal(t, http.StatusFound, status)

	// A callback with no code is the same: the flow cannot be finished.
	status, _, _ = api.call(t, fiber.MethodGet, "/api/integrations/oauth/callback?state=whatever", "", nil)
	require.Equal(t, http.StatusFound, status)
}

// TestTheWebhookEndpointRefusesWhatItCannotTrust covers the public endpoint's
// answer to a request that is not from a provider.
func TestTheWebhookEndpointRefusesWhatItCannotTrust(t *testing.T) {
	dsn := migratedDatabase(t)
	app, api := newHubAPI(t, dsn)
	api.app = app

	// No reference, so the request cannot name a tenant.
	status, _, body := api.call(t, fiber.MethodPost, "/api/webhooks/gmail", `{"a":1}`, nil)
	require.Equal(t, http.StatusUnauthorized, status, body)

	// A reference this deployment never issued is refused the same way.
	status, _, body = api.call(t, fiber.MethodPost, "/api/webhooks/gmail?ref=forged.value", `{"a":1}`, nil)
	require.Equal(t, http.StatusUnauthorized, status, body)
}

// TestTheConnectionEndpointsAnswer covers disconnect, refresh, and removal.
func TestTheConnectionEndpointsAnswer(t *testing.T) {
	dsn := migratedDatabase(t)
	app, api := newHubAPI(t, dsn)
	api.app = app

	token, workspace := signedIn(t, api)
	headers := withWorkspace(token, workspace)

	// A connection that does not exist is a 404 rather than an empty success.
	missing := uuid.NewString()
	status, _, body := api.call(t, fiber.MethodGet, "/api/integrations/"+missing, "", headers)
	require.Equal(t, http.StatusNotFound, status, body)

	status, _, body = api.call(t, fiber.MethodPost, "/api/integrations/"+missing+"/disconnect", "", headers)
	require.Equal(t, http.StatusNotFound, status, body)

	status, _, body = api.call(t, fiber.MethodPost, "/api/integrations/"+missing+"/refresh", "", headers)
	require.Equal(t, http.StatusNotFound, status, body)

	status, _, body = api.call(t, fiber.MethodDelete, "/api/integrations/"+missing, "", headers)
	require.Equal(t, http.StatusNotFound, status, body)

	// An id that is not a uuid is refused with the reason.
	status, _, body = api.call(t, fiber.MethodGet, "/api/integrations/not-a-uuid", "", headers)
	require.Equal(t, http.StatusBadRequest, status, body)

	// The MCP routes answer too, and an unknown server is a 404.
	status, payload, body := api.call(t, fiber.MethodGet, "/api/mcp/servers", "", headers)
	require.Equal(t, http.StatusOK, status, body)
	require.Empty(t, payload["data"].([]any))

	status, _, body = api.call(t, fiber.MethodPost, "/api/mcp/servers", `{"app":"","endpoint":""}`, headers)
	require.Equal(t, http.StatusBadRequest, status, body)

	status, _, body = api.call(t, fiber.MethodDelete, "/api/mcp/servers/"+uuid.NewString(), "", headers)
	require.Equal(t, http.StatusNotFound, status, body)
}

// TestTheIntegrationEndpointsRequireATenant keeps the surface from answering
// without an active workspace.
func TestTheIntegrationEndpointsRequireATenant(t *testing.T) {
	dsn := migratedDatabase(t)
	app, api := newHubAPI(t, dsn)
	api.app = app

	token, _ := signedIn(t, api)

	// A token with no workspace header cannot reach a tenant-scoped route.
	status, _, _ := api.call(t, fiber.MethodGet, "/api/integrations", "", bearer(token))
	require.Equal(t, http.StatusBadRequest, status)

	// And no token at all is refused before the workspace is even considered.
	status, _, _ = api.call(t, fiber.MethodGet, "/api/integrations", "", nil)
	require.Equal(t, http.StatusUnauthorized, status)
}
