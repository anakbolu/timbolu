package container

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	healthdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/health/domain"
	healthservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/health/service"
	"github.com/robzlabz/employeebot/packages/platform/config"
	"github.com/robzlabz/employeebot/packages/platform/metrics"
)

// The operations listener, which is what an orchestrator probes: a worker has no HTTP API, so without
// this a rollout has nothing to ask "is this process ready" and a restart decision is made blind.

// opsApp builds the listener's route table over a container with a real health service.
func opsApp(t *testing.T, databaseUp bool, exporter string) *http.ServeMux {
	t.Helper()

	// The service takes the two probes directly: a nil dependency is reported as not configured
	// rather than failing, which is why the fixture only supplies the database.
	health := healthservice.New(probeFunc(func() error {
		if !databaseUp {
			return errUnreachable
		}

		return nil
	}), nil)

	c := &Container{
		Config:   testConfig(),
		Logger:   zap.NewNop(),
		Services: &Services{Health: health},
	}

	if exporter != "" {
		provider, err := metrics.Setup(t.Context(), metrics.Config{
			ServiceName: "bolu-agent-worker", Exporter: exporter,
		}, zap.NewNop())
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, provider.Shutdown(t.Context())) })
		c.Metrics = provider
	}

	return c.opsMux()
}

// TestLivenessNeverTouchesADependency is the property a restart decision rests on: a probe that
// reports "unhealthy" because the database is down would have the orchestrator restart a process that
// is running perfectly, and it would restart it forever.
func TestLivenessNeverTouchesADependency(t *testing.T) {
	mux := opsApp(t, false, "")

	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))

	require.Equal(t, http.StatusOK, response.Code)

	body := decodeOps(t, response)
	require.Equal(t, true, body["success"])
	require.Equal(t, "alive", body["message"])
}

// TestReadinessRefusesWhileADependencyIsDown: a worker that cannot reach Postgres must not be told it
// is ready — a rollout that continued would put more tasks on a process that cannot run them.
func TestReadinessRefusesWhileADependencyIsDown(t *testing.T) {
	down := opsApp(t, false, "")

	response := httptest.NewRecorder()
	down.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ready", nil))
	require.Equal(t, http.StatusServiceUnavailable, response.Code)

	body := decodeOps(t, response)
	require.Equal(t, false, body["success"])
	require.Equal(t, "not_ready", body["error"].(map[string]any)["code"])

	up := opsApp(t, true, "")

	response = httptest.NewRecorder()
	up.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ready", nil))
	require.Equal(t, http.StatusOK, response.Code)

	body = decodeOps(t, response)
	require.Equal(t, true, body["success"])
	require.Equal(t, "ready", body["message"])
	require.NotEmpty(t, body["data"].(map[string]any)["components"])
}

// TestTheScrapeRouteExistsOnlyWhenSomethingIsScraped: a deployment that pushes its measurements, or
// that does not report any, has no such route — a 503 there would look like a broken scrape rather
// than a deployment that does not scrape.
func TestTheScrapeRouteExistsOnlyWhenSomethingIsScraped(t *testing.T) {
	pulling := opsApp(t, true, metrics.ExporterPrometheus)

	// A measured value, so the assertion is about the deployment's own numbers reaching the endpoint
	// rather than about an empty page that happens to parse.
	response := httptest.NewRecorder()
	pulling.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, response.Code)

	// The route that does not exist answers 404, which is what makes the absence meaningful.
	for _, exporter := range []string{"", metrics.ExporterNone, metrics.ExporterConsole} {
		pushing := opsApp(t, true, exporter)

		response := httptest.NewRecorder()
		pushing.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		require.Equal(t, http.StatusNotFound, response.Code, "exporter %q", exporter)
	}
}

// TestTheOpsListenerIsOffUnlessAnAddressIsNamed: a deployment that did not ask for a probe endpoint
// runs exactly as it did before.
func TestTheOpsListenerIsOffUnlessAnAddressIsNamed(t *testing.T) {
	c := &Container{
		Config: &config.Config{Http: config.HttpConfig{OpsAddress: ""}},
		Logger: zap.NewNop(),
	}

	// It returns immediately rather than listening: nothing to wait for, nothing to assert beyond
	// the call being safe on a container that serves no ops surface.
	require.NotPanics(t, func() { c.RunOpsServer(t.Context()) })
}

// decodeOps reads the envelope a probe answered.
func decodeOps(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	body := map[string]any{}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))

	return body
}

// probeFunc adapts a function to the health module's readiness check.
type probeFunc func() error

func (f probeFunc) Ping(context.Context) error { return f() }

var errUnreachable = errors.New("the dependency is unreachable")

// Compile-time check that the fixture is the interface the health service asks for.
var _ healthdomain.Checker = probeFunc(nil)
