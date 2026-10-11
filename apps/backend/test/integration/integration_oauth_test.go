package integration

import (
	"context"
	"encoding/json"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/container"
	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/config"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
)

// These tests cover the E8.3 gate: the state a flow carries is single-use and
// bound to the person who started it, and a provider that refuses a refresh
// leaves the connection recoverable rather than broken.
//
// The provider is a stub. What is under test is the flow this deployment owns —
// issuing the state, spending it exactly once, storing what came back, and
// marking a connection a provider no longer accepts.

// stubOAuth is a provider that speaks the shape the flow needs, with a scripted
// answer for the exchange and the refresh.
type stubOAuth struct {
	mu sync.Mutex
	// exchangeErr, when set, is what the provider answers an exchange with.
	exchangeErr error
	// refreshErr, when set, is what the provider answers a refresh with.
	refreshErr error
	// tokens is what a successful exchange returns.
	tokens integrationdomain.OAuthTokens
	// exchanges and refreshes count the calls, so a test can prove a replayed
	// callback never reached the provider.
	exchanges int
	refreshes int
	// lastState records the state the authorization URL was built with.
	lastState string
	// account is the address the provider says it connected. A second flow with
	// a different one is a second connection, not an update of the first.
	account string
}

func (s *stubOAuth) App() string { return "gmail" }

func (s *stubOAuth) Tools() []integrationdomain.ToolSpec {
	return []integrationdomain.ToolSpec{
		{Name: "gmail.search", Label: integrationdomain.LabelRead, Description: "Cari email",
			Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "gmail.send", Label: integrationdomain.LabelWriteExternal, Description: "Kirim email",
			Schema: json.RawMessage(`{"type":"object"}`)},
	}
}

func (s *stubOAuth) Triggers() []integrationdomain.TriggerSpec {
	return []integrationdomain.TriggerSpec{
		{App: "gmail", Kind: integrationdomain.TriggerWebhook, Event: "gmail.email_received", Description: "Email masuk"},
	}
}

func (s *stubOAuth) Execute(context.Context, integrationdomain.Call) (integrationdomain.Result, error) {
	return integrationdomain.Result{Content: json.RawMessage(`{}`)}, nil
}

func (s *stubOAuth) AuthCodeURL(state, redirectURI string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastState = state
	return "https://accounts.example.com/o/oauth2/auth?state=" + url.QueryEscape(state) +
		"&redirect_uri=" + url.QueryEscape(redirectURI)
}

func (s *stubOAuth) Exchange(_ context.Context, code, _ string) (integrationdomain.OAuthTokens, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.exchanges++
	if s.exchangeErr != nil {
		return integrationdomain.OAuthTokens{}, s.exchangeErr
	}
	if code != "a-good-code" {
		// What a provider says for a code that was already used or that expired.
		return integrationdomain.OAuthTokens{}, integrationdomain.ErrExchangeFailed
	}
	return s.tokens, nil
}

func (s *stubOAuth) Refresh(_ context.Context, refreshToken string) (integrationdomain.OAuthTokens, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.refreshes++
	if s.refreshErr != nil {
		return integrationdomain.OAuthTokens{}, s.refreshErr
	}
	if refreshToken == "" {
		return integrationdomain.OAuthTokens{}, integrationdomain.ErrRefreshFailed
	}
	return s.tokens, nil
}

func (s *stubOAuth) DefaultScopes() []string {
	return []string{"https://www.googleapis.com/auth/gmail.readonly"}
}

func (s *stubOAuth) AccountLabel(context.Context, integrationdomain.OAuthTokens) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.account != "" {
		return s.account, nil
	}
	return "toko@example.com", nil
}

func (s *stubOAuth) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exchanges, s.refreshes
}

// oauthFixture wires the hub over an onboarded workspace with a stub provider.
type oauthFixture struct {
	service     integrationdomain.Service
	provider    *stubOAuth
	pool        *database.Pool
	workspaceID uuid.UUID
	userID      uuid.UUID
	scope       integrationdomain.Scope
}

func newOAuthFixture(t *testing.T, dsn string, provider *stubOAuth) *oauthFixture {
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
		Kms:          config.KmsConfig{MasterKey: kmsKey(20), MasterKeyID: "v1"},
		Integrations: config.IntegrationConfig{OAuthStateKey: "an-integration-state-signing-key-32-chars"},
	}

	app, err := container.New(ctx, cfg,
		container.WithLogger(zap.NewNop()),
		container.WithIntegrationAdapters(provider),
	)
	require.NoError(t, err)
	t.Cleanup(app.Close)

	require.NotNil(t, app.Services.Integration)

	return &oauthFixture{
		service:     app.Services.Integration,
		provider:    provider,
		pool:        pool,
		workspaceID: workspace,
		userID:      user,
		scope:       integrationdomain.Scope{UserID: user, WorkspaceID: workspace},
	}
}

// oauthStubProvider returns a provider whose exchange succeeds.
func oauthStubProvider() *stubOAuth {
	return &stubOAuth{
		tokens: integrationdomain.OAuthTokens{
			AccessToken:  "ya29.a-real-access-token",
			RefreshToken: "1//a-real-refresh-token",
			ExpiresAt:    new(time.Now().Add(time.Hour)),
			Scopes:       []string{"https://www.googleapis.com/auth/gmail.readonly"},
		},
	}
}

// TestAFlowConnectsAnAccount is the happy path: the URL carries a state, the
// callback spends it, and the account is stored sealed.
func TestAFlowConnectsAnAccount(t *testing.T) {
	fixture := newOAuthFixture(t, migratedDatabase(t), oauthStubProvider())
	ctx := t.Context()

	auth, err := fixture.service.BeginAuth(ctx, fixture.scope, "gmail", "/dashboard/integrations")
	require.NoError(t, err)
	require.Contains(t, auth.URL, "accounts.example.com")
	require.NotEmpty(t, auth.State)

	// The consent URL the person is sent to carries the state, so the callback
	// can be matched to this flow.
	require.Contains(t, auth.URL, url.QueryEscape(auth.State))

	connection, err := fixture.service.CompleteAuth(ctx, integrationdomain.CompleteAuthRequest{
		Code:  "a-good-code",
		State: auth.State,
	})
	require.NoError(t, err)
	require.Equal(t, "gmail", connection.App)
	require.Equal(t, "toko@example.com", connection.AccountLabel)
	require.True(t, connection.Connected())

	// The stored token is sealed, and it opens where it is used.
	sealed := rawTokenColumn(t, fixture.pool, fixture.workspaceID, "gmail", "toko@example.com")
	require.NotEmpty(t, sealed)
	require.NotContains(t, string(sealed), "ya29.a-real-access-token")

	token, err := fixture.service.Token(ctx, fixture.scope, connection.ID)
	require.NoError(t, err)
	require.Equal(t, "ya29.a-real-access-token", token.AccessToken)
	require.Equal(t, "1//a-real-refresh-token", token.RefreshToken)
}

// TestAStateIsSpentExactlyOnce is the acceptance criterion: the callback stores a
// credential, so a replayed one must not.
func TestAStateIsSpentExactlyOnce(t *testing.T) {
	fixture := newOAuthFixture(t, migratedDatabase(t), oauthStubProvider())
	ctx := t.Context()

	auth, err := fixture.service.BeginAuth(ctx, fixture.scope, "gmail", "")
	require.NoError(t, err)

	_, err = fixture.service.CompleteAuth(ctx, integrationdomain.CompleteAuthRequest{
		Code:  "a-good-code",
		State: auth.State,
	})
	require.NoError(t, err)

	// The same value again is refused, and the provider is not asked twice.
	_, err = fixture.service.CompleteAuth(ctx, integrationdomain.CompleteAuthRequest{
		Code:  "a-good-code",
		State: auth.State,
	})
	require.ErrorIs(t, err, integrationdomain.ErrInvalidState)

	exchanges, _ := fixture.provider.counts()
	require.Equal(t, 1, exchanges, "a replayed callback must not reach the provider")
}

// TestAStateFromAnotherDeploymentIsRefused is why the state is signed: it travels
// through a browser, so anybody can hand one back.
func TestAStateFromAnotherDeploymentIsRefused(t *testing.T) {
	dsn := migratedDatabase(t)
	fixture := newOAuthFixture(t, dsn, oauthStubProvider())
	ctx := t.Context()

	// A second deployment with a different signing key issues a state for its
	// own workspace.
	other := newOAuthFixtureWithKey(t, dsn, oauthStubProvider(), "a-different-signing-key-of-32-chars")

	auth, err := other.service.BeginAuth(ctx, other.scope, "gmail", "")
	require.NoError(t, err)

	_, err = fixture.service.CompleteAuth(ctx, integrationdomain.CompleteAuthRequest{
		Code:  "a-good-code",
		State: auth.State,
	})
	require.ErrorIs(t, err, integrationdomain.ErrInvalidState)

	// A state that was never issued at all is refused the same way.
	_, err = fixture.service.CompleteAuth(ctx, integrationdomain.CompleteAuthRequest{
		Code:  "a-good-code",
		State: "not.a.real.state",
	})
	require.ErrorIs(t, err, integrationdomain.ErrInvalidState)

	// And a callback with no code is refused before anything else happens.
	auth2, err := fixture.service.BeginAuth(ctx, fixture.scope, "gmail", "")
	require.NoError(t, err)
	_, err = fixture.service.CompleteAuth(ctx, integrationdomain.CompleteAuthRequest{State: auth2.State})
	require.ErrorIs(t, err, integrationdomain.ErrInvalidState)
}

// TestAProviderThatRefusesTheCodeIsReported covers the code a person left too
// long, or that the browser replayed.
func TestAProviderThatRefusesTheCodeIsReported(t *testing.T) {
	fixture := newOAuthFixture(t, migratedDatabase(t), oauthStubProvider())
	ctx := t.Context()

	auth, err := fixture.service.BeginAuth(ctx, fixture.scope, "gmail", "")
	require.NoError(t, err)

	_, err = fixture.service.CompleteAuth(ctx, integrationdomain.CompleteAuthRequest{
		Code:  "a-code-that-was-already-used",
		State: auth.State,
	})
	require.ErrorIs(t, err, integrationdomain.ErrExchangeFailed)

	// Nothing was connected, so the screen has nothing to clean up.
	list, err := fixture.service.List(ctx, fixture.scope)
	require.NoError(t, err)
	require.Empty(t, list)
}

// TestAnAppWithNoAdapterCannotStartAFlow keeps a screen from sending somebody to
// a consent page this deployment cannot finish.
func TestAnAppWithNoAdapterCannotStartAFlow(t *testing.T) {
	fixture := newOAuthFixture(t, migratedDatabase(t), oauthStubProvider())
	ctx := t.Context()

	_, err := fixture.service.BeginAuth(ctx, fixture.scope, "notion", "")
	require.ErrorIs(t, err, integrationdomain.ErrUnknownAdapter)
}

// TestRefreshingReplacesTheStoredToken is the schedule's path.
func TestRefreshingReplacesTheStoredToken(t *testing.T) {
	provider := oauthStubProvider()
	fixture := newOAuthFixture(t, migratedDatabase(t), provider)
	ctx := t.Context()

	connected := connectThroughFlow(t, fixture)

	// The provider answers the refresh with a new access token.
	provider.tokens = integrationdomain.OAuthTokens{
		AccessToken: "ya29.a-refreshed-token",
		ExpiresAt:   new(time.Now().Add(time.Hour)),
	}

	refreshed, err := fixture.service.RefreshConnection(ctx, fixture.scope, connected.ID)
	require.NoError(t, err)
	require.True(t, refreshed.Connected())

	token, err := fixture.service.Token(ctx, fixture.scope, connected.ID)
	require.NoError(t, err)
	require.Equal(t, "ya29.a-refreshed-token", token.AccessToken)

	// A provider that rotates the refresh token sends a new one; one that does
	// not leaves the stored value alone. This provider sends none, so the
	// original is still there.
	require.Equal(t, "1//a-real-refresh-token", token.RefreshToken)
}

// TestARefusedRefreshAsksForALoginWithoutForgetting is the second acceptance
// criterion: a refresh that fails must not break the connection.
func TestARefusedRefreshAsksForALoginWithoutForgetting(t *testing.T) {
	provider := oauthStubProvider()
	fixture := newOAuthFixture(t, migratedDatabase(t), provider)
	ctx := t.Context()

	connected := connectThroughFlow(t, fixture)
	sealedBefore := rawTokenColumn(t, fixture.pool, fixture.workspaceID, "gmail", "toko@example.com")

	provider.refreshErr = integrationdomain.ErrRefreshFailed

	marked, err := fixture.service.RefreshConnection(ctx, fixture.scope, connected.ID)
	require.NoError(t, err, "a refused refresh is a state to show, not an error to raise")
	require.True(t, marked.NeedsRelogin())
	require.NotEmpty(t, marked.StatusReason)

	// What the connection already knew is still there, so re-authorising finds
	// it rather than starting over.
	sealedAfter := rawTokenColumn(t, fixture.pool, fixture.workspaceID, "gmail", "toko@example.com")
	require.Equal(t, sealedBefore, sealedAfter)

	// And the connection is honest about having no usable token right now.
	_, err = fixture.service.Token(ctx, fixture.scope, connected.ID)
	require.NoError(t, err, "the stored token is still readable; it is the provider that refuses it")

	// Re-authorising clears the status.
	reauthorised, err := fixture.service.CompleteAuth(ctx, integrationdomain.CompleteAuthRequest{
		Code:  "a-good-code",
		State: beginState(t, fixture),
	})
	require.NoError(t, err)
	require.True(t, reauthorised.Connected())
	require.Empty(t, reauthorised.StatusReason)
}

// TestRefreshExpiringOnlyTouchesWhatIsClose covers the schedule: a token with
// hours left is not refreshed, because every refresh is a provider call.
func TestRefreshExpiringOnlyTouchesWhatIsClose(t *testing.T) {
	provider := oauthStubProvider()
	fixture := newOAuthFixture(t, migratedDatabase(t), provider)
	ctx := t.Context()

	connectThroughFlow(t, fixture)

	// An hour of life is not close, so nothing is refreshed.
	refreshed, err := fixture.service.RefreshExpiring(ctx, fixture.scope)
	require.NoError(t, err)
	require.Empty(t, refreshed)

	_, refreshes := provider.counts()
	require.Zero(t, refreshes)

	// A connection whose token is about to expire is refreshed, with no margin
	// to spare.
	require.NoError(t, fixture.pool.PgxPool().QueryRow(ctx,
		`UPDATE integrations SET token_expires_at = now() + interval '30 seconds'
		 WHERE workspace_id = $1 AND app = 'gmail' RETURNING id`,
		fixture.workspaceID).Scan(new(uuid.UUID)))

	refreshed, err = fixture.service.RefreshExpiring(ctx, fixture.scope)
	require.NoError(t, err)
	require.Len(t, refreshed, 1)

	_, refreshes = provider.counts()
	require.Equal(t, 1, refreshes)
}

// TestAConnectionThatCannotBeRefreshedDoesNotStopTheOthers keeps one broken
// account from blocking a workspace's other connections.
func TestAConnectionThatCannotBeRefreshedDoesNotStopTheOthers(t *testing.T) {
	provider := oauthStubProvider()
	fixture := newOAuthFixture(t, migratedDatabase(t), provider)
	ctx := t.Context()

	first := connectThroughFlow(t, fixture)

	// A second connection, to the same app but a different account. The address
	// is what tells them apart: the same one would update the first row rather
	// than add a second.
	provider.tokens = integrationdomain.OAuthTokens{
		AccessToken:  "a-second-token",
		RefreshToken: "a-second-refresh",
		ExpiresAt:    new(time.Now().Add(time.Hour)),
	}
	provider.account = "pribadi@example.com"
	auth, err := fixture.service.BeginAuth(ctx, fixture.scope, "gmail", "")
	require.NoError(t, err)
	_, err = fixture.service.CompleteAuth(ctx, integrationdomain.CompleteAuthRequest{
		Code: "a-good-code", State: auth.State,
	})
	require.NoError(t, err)

	// Both are about to expire, and the provider refuses everything.
	_, err = fixture.pool.PgxPool().Exec(ctx,
		`UPDATE integrations SET token_expires_at = now() + interval '30 seconds' WHERE workspace_id = $1`,
		fixture.workspaceID)
	require.NoError(t, err)
	provider.refreshErr = integrationdomain.ErrRefreshFailed

	refreshed, err := fixture.service.RefreshExpiring(ctx, fixture.scope)
	require.NoError(t, err, "the sweep itself succeeds even when every refresh fails")

	// The sweep reports what it touched, which is both connections: a refusal is
	// a state to show rather than an error to raise, so the caller reads it off
	// the returned rows.
	require.Len(t, refreshed, 2)
	for _, connection := range refreshed {
		require.True(t, connection.NeedsRelogin(), connection.AccountLabel)
		require.NotEmpty(t, connection.StatusReason)
	}

	// Both are marked on the screen too, and both were attempted: one account
	// being refused does not stop the other from being tried.
	list, err := fixture.service.List(ctx, fixture.scope)
	require.NoError(t, err)
	require.Len(t, list, 2)

	_, refreshes := provider.counts()
	require.Equal(t, 2, refreshes)

	// And neither connection lost what it had stored, so re-authorising either
	// one finds it as it was.
	require.Equal(t, first.AccountLabel, "toko@example.com")
	for _, connection := range list {
		sealed := rawTokenColumn(t, fixture.pool, fixture.workspaceID, "gmail", connection.AccountLabel)
		require.NotEmpty(t, sealed, connection.AccountLabel)
	}
}

// TestAnAbsoluteReturnToIsRefused keeps the callback from being usable as an open
// redirect, which is the shape a phishing flow wants.
func TestAnAbsoluteReturnToIsRefused(t *testing.T) {
	fixture := newOAuthFixture(t, migratedDatabase(t), oauthStubProvider())
	ctx := t.Context()

	auth, err := fixture.service.BeginAuth(ctx, fixture.scope, "gmail", "https://evil.example.com/steal")
	require.NoError(t, err)

	// The state is signed, so the value it carries is readable: the return path
	// was replaced rather than trusted.
	require.NotContains(t, auth.State, "evil.example.com")

	auth2, err := fixture.service.BeginAuth(ctx, fixture.scope, "gmail", "//evil.example.com")
	require.NoError(t, err)
	require.NotContains(t, auth2.State, "evil.example.com")

	auth3, err := fixture.service.BeginAuth(ctx, fixture.scope, "gmail", "/dashboard/integrations")
	require.NoError(t, err)
	require.NotEmpty(t, auth3.State)
}

// connectThroughFlow runs the whole flow once and returns the connection.
func connectThroughFlow(t *testing.T, fixture *oauthFixture) integrationdomain.Integration {
	t.Helper()

	connection, err := fixture.service.CompleteAuth(t.Context(), integrationdomain.CompleteAuthRequest{
		Code:  "a-good-code",
		State: beginState(t, fixture),
	})
	require.NoError(t, err)
	return connection
}

// beginState starts a flow and returns the state it issued.
func beginState(t *testing.T, fixture *oauthFixture) string {
	t.Helper()

	auth, err := fixture.service.BeginAuth(t.Context(), fixture.scope, "gmail", "/dashboard/integrations")
	require.NoError(t, err)
	return auth.State
}

// newOAuthFixtureWithKey builds a fixture whose state signing key differs, which
// is how one deployment is told apart from another.
func newOAuthFixtureWithKey(t *testing.T, dsn string, provider *stubOAuth, key string) *oauthFixture {
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
		Kms:          config.KmsConfig{MasterKey: kmsKey(21), MasterKeyID: "v1"},
		Integrations: config.IntegrationConfig{OAuthStateKey: key},
	}

	app, err := container.New(ctx, cfg,
		container.WithLogger(zap.NewNop()),
		container.WithIntegrationAdapters(provider),
	)
	require.NoError(t, err)
	t.Cleanup(app.Close)

	return &oauthFixture{
		service:     app.Services.Integration,
		provider:    provider,
		pool:        pool,
		workspaceID: workspace,
		userID:      user,
		scope:       integrationdomain.Scope{UserID: user, WorkspaceID: workspace},
	}
}
