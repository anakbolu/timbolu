package container

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	integrationservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/service"
	"github.com/robzlabz/employeebot/packages/platform/config"
)

// Smoke: the env keys reach the config field, and the container offers the two
// connectors when they are set and withholds them when they are not.
func TestSmokeConnectors(t *testing.T) {
	for _, key := range []string{
		"SLACK_CLIENT_ID", "SLACK_CLIENT_SECRET", "SLACK_API_BASE", "SLACK_AUTH_ENDPOINT",
		"SLACK_TOKEN_ENDPOINT", "GITHUB_CLIENT_ID", "GITHUB_CLIENT_SECRET", "GITHUB_API_BASE",
		"GITHUB_AUTH_ENDPOINT", "GITHUB_TOKEN_ENDPOINT", "GMAIL_CLIENT_ID", "GMAIL_CLIENT_SECRET",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("ENVIRONMENT", "local")
	t.Setenv("SLACK_CLIENT_ID", "slack-id")
	t.Setenv("SLACK_CLIENT_SECRET", "slack-secret")
	t.Setenv("SLACK_API_BASE", "http://slack.test/api")
	t.Setenv("GITHUB_CLIENT_ID", "gh-id")
	t.Setenv("GITHUB_CLIENT_SECRET", "gh-secret")
	t.Setenv("GITHUB_TOKEN_ENDPOINT", "http://gh.test/token")

	wd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir("../.."))
	t.Cleanup(func() { _ = os.Chdir(wd) })

	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, "slack-id", cfg.Integrations.SlackClientID)
	require.Equal(t, "slack-secret", cfg.Integrations.SlackClientSecret)
	require.Equal(t, "http://slack.test/api", cfg.Integrations.SlackAPIBase)
	require.Equal(t, "gh-id", cfg.Integrations.GitHubClientID)
	require.Equal(t, "http://gh.test/token", cfg.Integrations.GitHubTokenEndpoint)

	c := &Container{Logger: zap.NewNop()}
	adapters := c.integrationAdapters(nil, cfg)

	apps := map[string]bool{}
	tools := map[string]bool{}
	for _, adapter := range adapters {
		apps[adapter.App()] = true
		for _, tool := range adapter.Tools() {
			tools[tool.Name] = true
			t.Logf("%s → %s (%s)", adapter.App(), tool.Name, tool.Label)
		}
		for _, trigger := range adapter.Triggers() {
			t.Logf("%s → trigger %s", adapter.App(), trigger.Event)
		}
	}
	require.True(t, apps["slack"], "slack is offered when its credentials are set: %v", apps)
	require.True(t, apps["github"], "github is offered when its credentials are set: %v", apps)
	require.True(t, tools["slack.post_message"])
	require.True(t, tools["slack.list_channels"])
	require.True(t, tools["github.list_repos"])
	require.True(t, tools["github.create_issue"])

	// And the registry accepts every spec it declares.
	registry := integrationservice.NewRegistry()
	for _, adapter := range adapters {
		require.NoError(t, registry.Register(adapter), adapter.App())
	}
	t.Logf("apps: %v", registry.Apps())

	// Empty credentials ⇒ the connector is absent rather than offered and broken.
	without := &config.Config{}
	require.Empty(t, c.integrationAdapters(nil, without))
}
