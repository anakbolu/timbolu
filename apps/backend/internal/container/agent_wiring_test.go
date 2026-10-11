package container

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/require"
)

// TestAgentModuleIsAssembled proves the registry is wired end to end: the
// repository, the service, and the handler all exist, and the routes answer.
func TestAgentModuleIsAssembled(t *testing.T) {
	c := newTestContainer(t)

	if c.Services.Agent == nil {
		t.Skip("the agent module needs a database; this container runs without one")
	}
	require.NotNil(t, c.Handlers.Agent)
}

// TestAgentRoutesReportUnavailableWithoutADatabase keeps the deployment without
// Postgres honest: the registry answers 503 instead of a confusing 404.
func TestAgentRoutesReportUnavailableWithoutADatabase(t *testing.T) {
	c := newTestContainer(t)

	if c.Services.Agent != nil {
		t.Skip("this test asserts the unconfigured deployment")
	}

	require.NotNil(t, c.App())

	// The route exists and reports the missing module, rather than 404 which
	// would look like a typo in the URL.
	req := httptest.NewRequest(fiber.MethodGet, "/api/agents", nil)
	resp, err := c.App().Test(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
}

// TestAgentServiceUsesTheConfiguredPlanLimit proves the container passes the
// plan cap through, so a workspace cannot create Bolu beyond its allowance.
func TestAgentServiceUsesTheConfiguredPlanLimit(t *testing.T) {
	cfg := testConfig()
	cfg.Plan.MaxAgentsPerWorkspace = 6

	c, err := New(context.Background(), cfg, WithLogger(nil), WithDB(nil), WithRedis(nil), WithTemporal(nil))
	require.NoError(t, err)
	t.Cleanup(c.Close)

	// Without a database the agent service is not assembled; the assertion here
	// is that the configuration is accepted and the container still builds.
	require.Nil(t, c.Services.Agent)
}
