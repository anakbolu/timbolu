// Command integration-worker runs the Temporal worker that talks to the outside
// world: OAuth token refresh, provider API calls and inbound webhook payloads.
//
// EPIC 8 (#66) registers the adapter activities here; until then the worker
// registers the connectivity probe so a deployment can be verified end to end.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	sdkworker "go.temporal.io/sdk/worker"

	"github.com/robzlabz/employeebot/apps/backend/internal/container"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/config"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/temporal"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "integration-worker: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	env := flag.String("env", "", "environment: local, staging, production (defaults to $ENVIRONMENT, else local)")
	flag.Parse()
	if *env != "" {
		if err := os.Setenv("ENVIRONMENT", *env); err != nil {
			return fmt.Errorf("set environment: %w", err)
		}
	}

	if err := config.LoadDotEnv(); err != nil {
		return fmt.Errorf("load .env: %w", err)
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// This binary names itself in the traces, so the API and the workers are separate services
	// in a collector rather than one process that appears everywhere.
	cfg.Telemetry.DefaultServiceName("bolu-integration-worker")

	c, err := container.New(ctx, cfg, container.WithoutHTTP())
	if err != nil {
		return err
	}
	defer c.Close()

	// This process has no HTTP API, so the operations listener is the only surface it serves:
	// /health, /ready, and /metrics. It is off unless the deployment named an address for it, which
	// is what an orchestrator probes and a scraper reads.
	go c.RunOpsServer(ctx)

	return container.RunWorker(ctx, c, container.WorkerConfig{
		Name:          "integration-worker",
		NeedsDatabase: true,
		TaskQueue:     func(t *temporal.Client) string { return t.TaskQueueIntegration() },
		Register: func(w sdkworker.Worker, _ *container.Container) {
			w.RegisterWorkflow(temporal.WorkerPingWorkflow)
			w.RegisterActivity(temporal.WorkerPingActivity)
		},
	})
}
