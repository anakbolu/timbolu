package container

import (
	"context"
	"fmt"

	sdkworker "go.temporal.io/sdk/worker"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/workflow"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/temporal"
)

// WorkerConfig describes one Temporal worker process. The container owns the
// bootstrap so the agent worker (EPIC 6, #52) and the integration worker
// (EPIC 8, #66) share the same connection, logging and shutdown behaviour.
type WorkerConfig struct {
	// Name is used in logs.
	Name string
	// TaskQueue picks the queue from the Temporal configuration.
	TaskQueue func(*temporal.Client) string
	// Register installs the workflows and activities of this worker. It runs
	// after the container is assembled, so it can reach the services.
	Register func(w sdkworker.Worker, c *Container)
	// NeedsDatabase makes a missing Postgres an explicit startup error instead
	// of a nil dereference on the first query.
	NeedsDatabase bool
}

// RegisterAgentTasks installs the workflows and activities the agent worker
// runs.
//
// It lives in the container because cmd must reach a module through it: the
// container is the only package allowed to know both the task module and the
// Temporal SDK.
func RegisterAgentTasks(w sdkworker.Worker, c *Container) {
	// The connectivity probe stays registered: deployment verification proves a
	// worker is reachable with it, which is cheaper than opening a task to find
	// out.
	w.RegisterWorkflow(temporal.WorkerPingWorkflow)
	w.RegisterActivity(temporal.WorkerPingActivity)

	if c == nil || c.Tasks == nil {
		// A worker without the runtime still starts, so a deployment whose
		// database is being migrated is reachable rather than crash-looping. It
		// answers the probe and nothing else.
		if c != nil && c.Logger != nil {
			c.Logger.Warn("agent tasks are disabled: the task runtime is not configured")
		}
		return
	}

	w.RegisterWorkflow(workflow.AgentTaskWorkflow)
	// The activity set is registered under the names the workflow calls, which
	// the method names mirror: a mismatch here would leave a scheduled activity
	// with no worker to run it, and the task would stall rather than fail.
	w.RegisterActivity(c.Tasks)

	// Memory extraction and routine runs ride on the same worker: each is one small
	// piece of work on a timer, and giving either its own queue and process would be a
	// deployment for a feature that shares every dependency the agent worker already
	// has.
	RegisterMemoryTasks(w, c)
	RegisterRoutineTasks(w, c)
}

// RunWorker starts the worker and blocks until ctx is cancelled. Every failure
// is returned instead of being logged and swallowed, so the process exits
// non-zero and the orchestrator restarts it.
func RunWorker(ctx context.Context, c *Container, cfg WorkerConfig) error {
	if c.Temporal == nil {
		return fmt.Errorf("%s: temporal is not configured (set TEMPORAL_HOST_PORT)", cfg.Name)
	}
	if cfg.NeedsDatabase && c.DB == nil {
		return fmt.Errorf("%s: database is not configured (set DATABASE_URL)", cfg.Name)
	}

	taskQueue := cfg.TaskQueue(c.Temporal)
	if taskQueue == "" {
		return fmt.Errorf("%s: task queue is empty", cfg.Name)
	}

	// The worker is run with the same tracing interceptor the client was dialled with: that is
	// what makes an activity's span a child of the workflow's, including when the activity is
	// retried, resumed after a restart, or run by a different process than the one that started
	// it.
	tracing, err := c.temporalInterceptors()
	if err != nil {
		return fmt.Errorf("%s: build tracing interceptor: %w", cfg.Name, err)
	}

	// The worker inherits its logger from the client it was built with, and that client was
	// dialled with the deployment's own logger — so a workflow's lines carry the same fields
	// and the same format as every other line, including the workflow id the SDK adds.
	w := sdkworker.New(c.Temporal.Client(), taskQueue, sdkworker.Options{
		Interceptors: temporalWorkerInterceptors(tracing),
	})
	if cfg.Register != nil {
		cfg.Register(w, c)
	}

	if err := w.Start(); err != nil {
		return fmt.Errorf("%s: start worker: %w", cfg.Name, err)
	}

	c.Logger.Info("worker started",
		zap.String("worker", cfg.Name),
		zap.String("task_queue", taskQueue),
		zap.String("namespace", c.Temporal.Namespace()),
	)

	<-ctx.Done()
	c.Logger.Info("worker stopping", zap.String("worker", cfg.Name))
	w.Stop()
	return nil
}
