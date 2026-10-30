package container

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/apps/backend/internal/platform/temporal"
)

// TestRunWorkerRequiresTemporal keeps a worker from starting without a workflow
// backend, where it would silently never receive work.
func TestRunWorkerRequiresTemporal(t *testing.T) {
	c := newTestContainer(t)

	err := RunWorker(context.Background(), c, WorkerConfig{
		Name:          "agent-worker",
		NeedsDatabase: false,
		TaskQueue:     func(*temporal.Client) string { return "bolu-agent" },
	})

	require.ErrorContains(t, err, "temporal is not configured")
}

func TestRunWorkerRequiresDatabase(t *testing.T) {
	c := newTestContainer(t, WithTemporal(&temporal.Client{}))

	err := RunWorker(context.Background(), c, WorkerConfig{
		Name:          "agent-worker",
		NeedsDatabase: true,
		TaskQueue:     func(*temporal.Client) string { return "bolu-agent" },
	})

	require.ErrorContains(t, err, "database is not configured")
}

func TestRunWorkerRequiresATaskQueue(t *testing.T) {
	c := newTestContainer(t, WithTemporal(&temporal.Client{}))

	err := RunWorker(context.Background(), c, WorkerConfig{
		Name:      "agent-worker",
		TaskQueue: func(*temporal.Client) string { return "" },
	})

	require.ErrorContains(t, err, "task queue is empty")
}
