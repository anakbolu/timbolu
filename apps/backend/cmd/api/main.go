// Command api runs the HTTP and WebSocket surface: it accepts every user
// request, opens the realtime connection, and starts long-running work on
// Temporal instead of doing it in the request.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/container"
	"github.com/robzlabz/employeebot/packages/platform/config"
)

const shutdownTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "api: %v\n", err)
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
	cfg.Telemetry.DefaultServiceName("bolu-api")

	c, err := container.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer c.Close()

	address := cfg.Http.Address
	if address == "" {
		address = ":8080"
	}

	c.Logger.Info("starting api",
		zap.String("environment", cfg.Application.Environment),
		zap.String("address", address),
		zap.Bool("database", c.DB != nil),
		zap.Bool("redis", c.Redis != nil),
		zap.Bool("temporal", c.Temporal != nil),
	)

	// The billing sweep is a loop in this process rather than a durable workflow,
	// because nothing in it waits: each pass recomputes the allowance from the times
	// and the ledger, so a pass that never ran is corrected by the next one. It needs
	// the database and Redis and nothing else, both of which this process already has.
	go c.RunBillingSweep(ctx)
	// The retention pass shares the process for the reason the billing sweep does: it is a
	// loop with nothing to wait for. A deployment sets RETENTION_INTERVAL=0 to switch it off.
	go c.RunRetention(ctx)
	// The internal listener is the half of the WhatsApp seam the gateway calls:
	// it records what arrived, hands out what is due, and takes back what left. It
	// is off unless the deployment names an address, so an API with no gateway is
	// unchanged.
	go c.RunInternalServer(ctx)

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- c.App().Listen(address)
	}()

	select {
	case err := <-serverErr:
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		return nil
	case <-ctx.Done():
		c.Logger.Info("shutting down api")
		if err := c.App().ShutdownWithTimeout(shutdownTimeout); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}
