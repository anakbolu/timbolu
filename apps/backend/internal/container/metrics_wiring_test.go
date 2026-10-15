package container

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	approvaldomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/approval/domain"
	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	taskdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	"github.com/robzlabz/employeebot/packages/platform/config"
	"github.com/robzlabz/employeebot/packages/platform/metrics"
)

// The wiring tests for metrics cover what only the container can get wrong: that a deployment which
// named no exporter has no provider at all, that the one adapter really is every port the modules
// declare, and that the scrape endpoint exists only when something is being scraped.

// withMetricsProvider assembles a container with a Prometheus deployment installed.
func withMetricsProvider(t *testing.T) *Container {
	t.Helper()

	c := newTestContainer(t, WithoutHTTP())
	c.Config.Telemetry = config.TelemetryConfig{
		ServiceName:     "bolu-api",
		MetricsExporter: metrics.ExporterPrometheus,
	}
	require.NoError(t, c.openMetrics(t.Context(), c.Config))
	require.NotNil(t, c.Metrics)

	return c
}

// TestNoExporterMeansTheContainerHasNoMeter: the deployment said nowhere to send measurements, so
// there is no provider — which is what makes every recording call a nil check rather than a branch
// at each call site.
func TestNoExporterMeansTheContainerHasNoMeter(t *testing.T) {
	for _, exporter := range []string{"", metrics.ExporterNone} {
		c := newTestContainer(t, WithoutHTTP())

		require.NoError(t, c.openMetrics(t.Context(), &config.Config{
			Telemetry: config.TelemetryConfig{MetricsExporter: exporter},
		}))
		require.Nil(t, c.Metrics, "exporter %q must leave the meter unbuilt", exporter)
	}
}

// TestAnUnknownMetricsExporterStopsTheProcess: where measurements go is configuration, and a typo is
// a startup failure rather than a deployment that silently reports nothing.
func TestAnUnknownMetricsExporterStopsTheProcess(t *testing.T) {
	c := newTestContainer(t, WithoutHTTP())

	err := c.openMetrics(t.Context(), &config.Config{
		Telemetry: config.TelemetryConfig{MetricsExporter: "influxdb"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown exporter")
	require.Nil(t, c.Metrics)
}

// TestTheScrapeRouteServesTheDeploymentsOwnMeasures: the route is what a self-hosted Prometheus
// walks, and it serves the deployment's measurements under Fiber rather than through a second server.
func TestTheScrapeRouteServesTheDeploymentsOwnMeasures(t *testing.T) {
	c := withMetricsProvider(t)
	require.NotNil(t, c.Metrics.Handler, "the Prometheus exporter serves what it gathered")

	// The route table is built by New, which this container skipped so that a test does not start
	// the whole module set: the scrape route is registered on the app it already has.
	c.app = fiber.New(fiber.Config{DisableStartupMessage: true})
	c.registerMetricsRoutes()

	c.Metrics.TaskDispatched(t.Context())

	response, err := c.app.Test(httptest.NewRequest(http.MethodGet, "/metrics", nil), -1)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()

	require.Equal(t, fiber.StatusOK, response.StatusCode)

	content, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Contains(t, string(content), "bolu_task_dispatched_total",
		"the deployment's own measures are served")
}

// TestAPushingDeploymentHasNoScrapeRoute: a deployment that sends to a collector has no endpoint to
// mount, because there is no handler to serve.
func TestAPushingDeploymentHasNoScrapeRoute(t *testing.T) {
	c := newTestContainer(t, WithoutHTTP())
	require.NoError(t, c.openMetrics(t.Context(), &config.Config{
		Telemetry: config.TelemetryConfig{
			ServiceName:     "bolu-agent-worker",
			MetricsExporter: metrics.ExporterConsole,
		},
	}))
	require.NotNil(t, c.Metrics)

	require.Nil(t, c.Metrics.Handler, "there is nothing to scrape")

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	c.app = app
	c.registerMetricsRoutes()

	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/metrics", nil), -1)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()

	require.Equal(t, fiber.StatusNotFound, response.StatusCode)
}

// TestTheOneAdapterIsEveryPortTheModulesDeclare is the compile-time check stated as a test, so a
// method added to one of the ports is a failure here rather than a module that silently reports
// nothing — and a nil provider tolerates every one of them.
func TestTheOneAdapterIsEveryPortTheModulesDeclare(t *testing.T) {
	adapter := (&Container{}).metricsFor()

	var taskPort taskdomain.Metrics = adapter
	var approvalPort approvaldomain.Metrics = adapter
	var llmPort llmdomain.Metrics = adapter

	ctx := t.Context()
	require.NotPanics(t, func() {
		taskPort.TaskDispatched(ctx)
		taskPort.TaskSettled(ctx)
		taskPort.TaskFinished(ctx, "chat", "succeeded", 1, 1, 1, 1)
		taskPort.ToolCall(ctx, "gmail", false, 1)
		taskPort.DraftReady(ctx, 1)
		taskPort.DraftDecided(ctx, "approved", false)
		taskPort.ActivityRetry(ctx, "RunTool", 2)

		approvalPort.DraftDecided(ctx, "revise", true)

		llmPort.ModelCall(ctx, "openai", "agent", true, 1)
	})
}

// TestTheAdapterPassesTheDeploymentsMeterOn: a container with a provider really records into it, so
// the adapter is a translation rather than a place a measurement can disappear.
func TestTheAdapterPassesTheDeploymentsMeterOn(t *testing.T) {
	provider, err := metrics.Setup(t.Context(), metrics.Config{
		ServiceName: "bolu-api", Exporter: metrics.ExporterPrometheus,
	}, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(t.Context())) })

	c := &Container{Metrics: provider}

	c.metricsFor().TaskFinished(t.Context(), "routine", "failed", 30, 800, 100, 900)

	response := httptest.NewRecorder()
	provider.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	require.Contains(t, response.Body.String(), `outcome="failed"`)
	require.Contains(t, response.Body.String(), `trigger="routine"`)
	require.False(t, strings.Contains(response.Body.String(), "workspace_id"),
		"no tenant is a label, because a label with an unbounded value is a series per value")
}
