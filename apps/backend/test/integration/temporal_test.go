package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	sdkclient "go.temporal.io/sdk/client"
	sdkworker "go.temporal.io/sdk/worker"

	"github.com/anakbolu/timbolu/apps/backend/internal/container"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/temporal"
	"github.com/anakbolu/timbolu/packages/platform/config"
)

// TestWorkerRunsAgainstTemporal proves the whole worker path end to end: the
// container connects to Temporal, the worker registers the probe workflow, the
// workflow executes on the worker's task queue, and the worker stops when the
// context is cancelled. It is the same path the agent worker uses in
// docker-compose, so it covers EPIC 1's "both workers run" gate.
func TestWorkerRunsAgainstTemporal(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	temporalContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "temporalio/temporal:latest",
			ExposedPorts: []string{"7233/tcp"},
			Cmd:          []string{"server", "start-dev", "--ip", "0.0.0.0", "--port", "7233"},
			WaitingFor:   wait.ForListeningPort("7233/tcp").WithStartupTimeout(3 * time.Minute),
		},
		Started: true,
	})
	require.NoError(t, err, "start temporal dev server")

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cleanupCancel()
		require.NoError(t, temporalContainer.Terminate(cleanupCtx))
	})

	host, err := temporalContainer.Host(ctx)
	require.NoError(t, err)
	port, err := temporalContainer.MappedPort(ctx, "7233")
	require.NoError(t, err)

	cfg := &config.Config{
		Application: config.AppConfig{Environment: "test"},
		Http:        config.HttpConfig{Address: ":0", ApiPrefix: "/api"},
		Logging:     config.LoggingConfig{Level: "error", Format: "json"},
		Temporal: config.TemporalConfig{
			HostPort:       host + ":" + port.Port(),
			Namespace:      "default",
			TaskQueueAgent: "bolu-agent-test",
		},
	}

	c, err := container.New(ctx, cfg, container.WithoutHTTP())
	require.NoError(t, err)
	t.Cleanup(c.Close)

	require.NotNil(t, c.Temporal, "the container must connect to Temporal")

	workerCtx, stopWorker := context.WithCancel(ctx)
	workerErr := make(chan error, 1)
	go func() {
		workerErr <- container.RunWorker(workerCtx, c, container.WorkerConfig{
			Name:      "integration-agent-worker",
			TaskQueue: func(t *temporal.Client) string { return t.TaskQueueAgent() },
			Register: func(w sdkworker.Worker, _ *container.Container) {
				w.RegisterWorkflow(temporal.WorkerPingWorkflow)
				w.RegisterActivity(temporal.WorkerPingActivity)
			},
		})
	}()

	run, err := c.Temporal.Client().ExecuteWorkflow(ctx, sdkclient.StartWorkflowOptions{
		ID:        "worker-ping-" + uuid.NewString(),
		TaskQueue: c.Temporal.TaskQueueAgent(),
	}, temporal.WorkerPingWorkflow, "agent-worker")
	require.NoError(t, err, "start the probe workflow")

	var result string
	require.NoError(t, run.Get(ctx, &result), "the worker must execute the probe")
	require.Equal(t, "agent-worker pong", result)

	stopWorker()
	require.NoError(t, <-workerErr, "the worker must stop cleanly")
}
