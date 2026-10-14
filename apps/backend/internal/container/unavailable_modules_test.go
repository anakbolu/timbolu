package container

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/require"
)

// TestUnavailableModulesAnswer503RatherThan404 keeps the container's promise to
// a deployment with no database: every documented route still exists and says
// the module is not configured, rather than answering 404 and looking like a
// typo to the screen that asked for it.
//
// It is the guard for the drift that produced "Cannot GET /api/checklists": a
// module added to the tenant group but not to registerUnavailableModules leaves
// its route answering 404.
func TestUnavailableModulesAnswer503RatherThan404(t *testing.T) {
	c := newTestContainer(t)

	for _, route := range []struct {
		path string
		code string
	}{
		{"/api/workspaces", "workspace_not_configured"},
		{"/api/agents", "agent_not_configured"},
		{"/api/conversations", "chat_not_configured"},
		{"/api/tasks", "task_not_configured"},
		{"/api/memories", "memory_not_configured"},
		{"/api/routines", "routine_not_configured"},
		{"/api/whatsapp/numbers", "whatsapp_not_configured"},
		{"/api/billing/subscription", "billing_not_configured"},
		{"/api/drafts", "approval_not_configured"},
		{"/api/integrations", "integration_not_configured"},
		{"/api/orders", "order_not_configured"},
		{"/api/checklists", "checklist_not_configured"},
		{"/api/llm/providers", "llm_not_configured"},
	} {
		t.Run(route.path, func(t *testing.T) {
			resp, err := c.App().Test(httptest.NewRequest(http.MethodGet, route.path, nil), -1)
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())

			require.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode, route.path)
			require.Contains(t, string(body), route.code, route.path)
		})
	}
}
