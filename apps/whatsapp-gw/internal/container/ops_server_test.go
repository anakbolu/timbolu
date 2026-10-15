package container

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/packages/platform/config"
	"github.com/robzlabz/employeebot/packages/platform/database"
	"github.com/robzlabz/employeebot/packages/platform/metrics"
	"github.com/robzlabz/employeebot/packages/platform/redis"
)

// The operations listener is what a process with no public API answers probes
// on. Three claims are under test: liveness never depends on a dependency, a
// gateway that cannot reach Postgres or Redis is not ready (two clients on one
// session is what gets a number banned), and the scrape endpoint exists only
// when a deployment named an exporter.

func opsContainer(t *testing.T) *Container {
	t.Helper()

	return &Container{Config: &config.Config{}, Logger: zap.NewNop()}
}

func TestOpsServerIsOffWithoutAnAddress(t *testing.T) {
	c := opsContainer(t)

	// A gateway run beside a local API is unchanged: no address, no listener.
	c.RunOpsServer(context.Background())

	var nilContainer *Container
	nilContainer.RunOpsServer(context.Background())
	(&Container{}).RunOpsServer(context.Background())
}

func TestHealthNeverTouchesADependency(t *testing.T) {
	c := opsContainer(t)

	recorder := httptest.NewRecorder()
	c.opsMux().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))

	var body struct {
		Success bool           `json:"success"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.True(t, body.Success)
	require.Equal(t, "alive", body.Message)
	require.Equal(t, "ok", body.Data["status"])
}

func TestReadinessIsNotReadyWithoutItsDependencies(t *testing.T) {
	c := opsContainer(t)

	recorder := httptest.NewRecorder()
	c.opsMux().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ready", nil))

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)

	var body struct {
		Success bool              `json:"success"`
		Message string            `json:"message"`
		Data    readinessReport   `json:"data"`
		Error   map[string]string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.False(t, body.Success)
	require.Equal(t, "ready", body.Message)
	require.Equal(t, "not_ready", body.Data.Status)
	require.Equal(t, "not_configured", body.Data.Checks["database"])
	require.Equal(t, "not_configured", body.Data.Checks["redis"])
	require.Equal(t, "not_ready", body.Error["code"])
}

func TestReadinessReportsWhatIsBroken(t *testing.T) {
	// A pool and a client that exist but cannot answer are the shape a real
	// outage takes, and the probe must say which one is the problem.
	c := &Container{
		Config: &config.Config{},
		Logger: zap.NewNop(),
		DB:     &database.Pool{},
		Redis:  &redis.Client{},
	}

	report := c.readiness(context.Background())
	require.Equal(t, "not_ready", report.Status)
	require.Contains(t, report.Checks["database"], "not configured")
	require.Contains(t, report.Checks["redis"], "not configured")
}

func TestMetricsIsServedOnlyWhenAnExporterIsNamed(t *testing.T) {
	c := opsContainer(t)

	recorder := httptest.NewRecorder()
	c.opsMux().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusNotFound, recorder.Code)

	c.Metrics = &metrics.Provider{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("bolu_metric 1\n"))
	})}

	recorder = httptest.NewRecorder()
	c.opsMux().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), "bolu_metric")
}

func TestRunOpsServerServesUntilTheContextIsCancelled(t *testing.T) {
	address := freeAddress(t)
	c := &Container{Config: &config.Config{Http: config.HttpConfig{OpsAddress: address}}, Logger: zap.NewNop()}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.RunOpsServer(ctx)
		close(done)
	}()

	// The listener comes up asynchronously; the probe is retried the way a
	// monitor would.
	client := &http.Client{Timeout: 500 * time.Millisecond}
	require.Eventually(t, func() bool {
		response, err := client.Get("http://" + address + "/health")
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		return response.StatusCode == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the operations endpoint did not stop when its context was cancelled")
	}
}

func TestRunOpsServerKeepsRunningWhenThePortIsBusy(t *testing.T) {
	// A busy monitoring port must not become an outage: the gateway's job is to
	// hold the connections, so it logs and runs without the endpoint.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	c := &Container{Config: &config.Config{Http: config.HttpConfig{OpsAddress: listener.Addr().String()}}, Logger: zap.NewNop()}
	c.RunOpsServer(context.Background())
}

func freeAddress(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	return address
}
