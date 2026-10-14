package integration

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/container"
	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
	"github.com/robzlabz/employeebot/packages/platform/config"
	platformredis "github.com/robzlabz/employeebot/packages/platform/redis"
)

// These tests cover the E8.5 gate: a per-integration ceiling is respected even
// when many callers arrive together, and work that is held is held rather than
// failed, so it runs once the ceiling recovers.
//
// The bucket is a Redis script, so the test runs against a real Redis: what can
// only be wrong here is the atomicity, and a fake would not have any.

// limitedAdapter counts the calls that actually reached it, so a test can prove a
// held call never got there.
type limitedAdapter struct {
	mu    sync.Mutex
	calls int
}

func (a *limitedAdapter) App() string { return "gmail" }

func (a *limitedAdapter) Tools() []integrationdomain.ToolSpec {
	return []integrationdomain.ToolSpec{{
		Name: "gmail.search", Label: integrationdomain.LabelRead, Description: "Cari email",
		Schema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
	}}
}

func (a *limitedAdapter) Triggers() []integrationdomain.TriggerSpec { return nil }

func (a *limitedAdapter) Execute(context.Context, integrationdomain.Call) (integrationdomain.Result, error) {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()

	return integrationdomain.Result{Content: json.RawMessage(`{"ok":true}`), Summary: "cari"}, nil
}

func (a *limitedAdapter) reached() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// limitFixture wires the hub over one connected account with a paced app.
type limitFixture struct {
	service     integrationdomain.Service
	adapter     *limitedAdapter
	integration integrationdomain.Integration
	scope       integrationdomain.Scope
}

func newLimitFixture(t *testing.T, dsn, redisURL, rateLimits string, fallback int) *limitFixture {
	t.Helper()

	ctx := t.Context()
	pool := poolFor(t, dsn)

	user, err := createVerifiedUser(t, pool)
	require.NoError(t, err)
	workspace, err := onboardWorkspace(t, pool, user)
	require.NoError(t, err)

	adapter := &limitedAdapter{}

	client, err := platformredis.New(ctx, redisURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	cfg := &config.Config{
		Application: config.AppConfig{Environment: "test", FrontendURL: "https://app.example.com"},
		Http:        config.HttpConfig{Address: ":0", ApiPrefix: "/api"},
		Database:    config.DatabaseConfig{URL: dsn, MaxOpenConns: 5, MaxIdleConns: 1},
		JWT:         config.JWTConfig{Secret: "integration-test-secret", ExpireHours: 1},
		Auth: config.AuthConfig{
			AccessTokenMinutes: 15, RefreshHours: 24, VerificationHours: 24,
			ResetMinutes: 60, StateMinutes: 10, LoginThrottleCapacity: 100, LoginThrottleRefill: 100,
		},
		Mail:    config.MailConfig{Driver: "log"},
		Logging: config.LoggingConfig{Level: "error", Format: "json"},
		Kms:     config.KmsConfig{MasterKey: kmsKey(40), MasterKeyID: "v1"},
		Integrations: config.IntegrationConfig{
			OAuthStateKey:         "an-integration-state-signing-key-32-chars",
			RateLimits:            rateLimits,
			DefaultCallsPerMinute: fallback,
		},
	}

	app, err := container.New(ctx, cfg,
		container.WithLogger(zap.NewNop()),
		container.WithIntegrationAdapters(adapter),
		container.WithRedis(client),
	)
	require.NoError(t, err)
	t.Cleanup(app.Close)

	scope := integrationdomain.Scope{UserID: user, WorkspaceID: workspace}

	connection, err := app.Services.Integration.Connect(ctx, scope, integrationdomain.ConnectInput{
		App:          "gmail",
		AccountLabel: "toko@example.com",
		AccessToken:  "a-real-token",
	})
	require.NoError(t, err)

	return &limitFixture{
		service:     app.Services.Integration,
		adapter:     adapter,
		integration: connection,
		scope:       scope,
	}
}

// call runs one tool call through the service.
func (f *limitFixture) call(ctx context.Context) error {
	_, err := f.service.Execute(ctx, f.scope, f.integration.ID, "gmail.search",
		json.RawMessage(`{"q":"pesanan"}`))
	return err
}

// TestACeilingHoldsTheCallsBeyondIt is the acceptance criterion: a hundred calls
// against a ceiling of ten a minute leaves ten that went out and ninety held.
func TestACeilingHoldsTheCallsBeyondIt(t *testing.T) {
	fixture := newLimitFixture(t, migratedDatabase(t), redisURL(t), "gmail:10", 0)
	ctx := t.Context()

	held := 0
	for i := 0; i < 100; i++ {
		err := fixture.call(ctx)
		if err != nil {
			require.ErrorIs(t, err, integrationdomain.ErrRateLimited, "call %d", i)
			held++
		}
	}

	require.Equal(t, 90, held, "ninety of a hundred calls must be held")
	require.Equal(t, 10, fixture.adapter.reached(),
		"exactly the ten the ceiling allows reached the provider")

	// A held call says how long to wait and what the ceiling is, so the runtime
	// schedules its retry from it rather than guessing.
	err := fixture.call(ctx)
	require.ErrorIs(t, err, integrationdomain.ErrRateLimited)

	var limited *integrationdomain.RateLimitedError
	require.ErrorAs(t, err, &limited)
	require.Equal(t, "gmail", limited.App)
	require.Equal(t, 10, limited.PerMinute)
	require.Greater(t, limited.RetryAfter.Milliseconds(), int64(0))
	require.Contains(t, err.Error(), "10 calls a minute")
}

// TestTheCeilingHoldsUnderConcurrentCallers is the part a fake could not show: the
// bucket is one Redis script, so many callers cannot each see a token.
func TestTheCeilingHoldsUnderConcurrentCallers(t *testing.T) {
	fixture := newLimitFixture(t, migratedDatabase(t), redisURL(t), "gmail:10", 0)
	ctx := t.Context()

	const callers = 50

	var wg sync.WaitGroup
	results := make([]error, callers)

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index] = fixture.call(ctx)
		}(i)
	}
	wg.Wait()

	allowed, held := 0, 0
	for _, err := range results {
		if err == nil {
			allowed++
			continue
		}
		require.ErrorIs(t, err, integrationdomain.ErrRateLimited)
		held++
	}

	require.Equal(t, 10, allowed, "the ceiling holds no matter how many arrive together")
	require.Equal(t, callers-10, held)
	require.Equal(t, 10, fixture.adapter.reached(),
		"not one call beyond the ceiling reached the provider")
}

// TestAnAppWithNoCeilingIsNotPaced is the honest reading of a deployment that
// configured none: pacing it at an invented number would hold work nobody asked
// to have held.
func TestAnAppWithNoCeilingIsNotPaced(t *testing.T) {
	fixture := newLimitFixture(t, migratedDatabase(t), redisURL(t), "", 0)
	ctx := t.Context()

	for i := 0; i < 30; i++ {
		require.NoError(t, fixture.call(ctx), "call %d", i)
	}
	require.Equal(t, 30, fixture.adapter.reached())
}

// TestTheFallbackPacesAnAppWithNoCeilingOfItsOwn covers the default.
func TestTheFallbackPacesAnAppWithNoCeilingOfItsOwn(t *testing.T) {
	fixture := newLimitFixture(t, migratedDatabase(t), redisURL(t), "jira:5", 4)
	ctx := t.Context()

	allowed := 0
	for i := 0; i < 20; i++ {
		if fixture.call(ctx) == nil {
			allowed++
		}
	}
	require.Equal(t, 4, allowed, "the fallback applies to gmail, which has no ceiling of its own")
	require.Equal(t, 4, fixture.adapter.reached())
}

// TestACeilingOfOneAppDoesNotPaceAnother keeps two providers' quotas apart.
func TestACeilingOfOneAppDoesNotPaceAnother(t *testing.T) {
	fixture := newLimitFixture(t, migratedDatabase(t), redisURL(t), "gmail:2,jira:50", 0)
	ctx := t.Context()

	// The gmail bucket is spent.
	require.NoError(t, fixture.call(ctx))
	require.NoError(t, fixture.call(ctx))
	require.ErrorIs(t, fixture.call(ctx), integrationdomain.ErrRateLimited)

	// The jira bucket is untouched, so a second connection is not punished for
	// the first one's spending. Its own ceiling is fifty, so it has room.
	second, err := fixture.service.Connect(ctx, fixture.scope, integrationdomain.ConnectInput{
		App:          "gmail",
		AccountLabel: "pribadi@example.com",
		AccessToken:  "another-token",
	})
	require.NoError(t, err)

	// The bucket is the connection's, so the second account has its own two.
	_, err = fixture.service.Execute(ctx, fixture.scope, second.ID, "gmail.search",
		json.RawMessage(`{"q":"pesanan"}`))
	require.NoError(t, err, "two accounts of one provider are two quotas")
}

// TestALimitDoesNotStopTheCallForever is the second acceptance criterion: the work
// held by a ceiling runs once the ceiling recovers.
func TestALimitDoesNotStopTheCallForever(t *testing.T) {
	// A ceiling of sixty a minute refills one token a second, so a wait of a
	// second is enough to prove recovery without making the test slow.
	fixture := newLimitFixture(t, migratedDatabase(t), redisURL(t), "gmail:60", 0)
	ctx := t.Context()

	spent := 0
	for i := 0; i < 60; i++ {
		if fixture.call(ctx) == nil {
			spent++
		}
	}
	require.Equal(t, 60, spent)

	// The bucket is empty, so the next call is held with a wait it states.
	err := fixture.call(ctx)
	require.ErrorIs(t, err, integrationdomain.ErrRateLimited)

	var limited *integrationdomain.RateLimitedError
	require.ErrorAs(t, err, &limited)
	require.LessOrEqual(t, limited.RetryAfter.Milliseconds(), int64(2000),
		"the wait must be short enough to retry soon, not a refusal in disguise")

	// Once the bucket has refilled, the same call goes through. Waiting the
	// reported time is what the runtime does with the error.
	waitFor := limited.RetryAfter
	require.Eventually(t, func() bool {
		return fixture.call(ctx) == nil
	}, waitFor+2e9, waitFor/4+1e8, "the held call must run once the ceiling recovers")

	require.Equal(t, 61, fixture.adapter.reached())
}
